// Package githubapp implements GitHub App login and repository-scoped bot tokens.
// User tokens are used only to identify the browser user and are never delegated.
package githubapp

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

var ErrDenied = errors.New("GitHub access denied")
var namePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]*$`)

type Options struct {
	AppID          int64
	InstallationID int64
	ClientID       string
	ClientSecret   string
	PrivateKeyFile string
	Organization   string
	Team           string
	Repositories   []string // owner/name, explicitly selected; no wildcard
}

type User struct {
	ID    int64  `json:"id"`
	Login string `json:"login"`
}
type Token struct {
	Value     string    `json:"token"`
	ExpiresAt time.Time `json:"expires_at"`
}

type Client struct {
	options             Options
	key                 *rsa.PrivateKey
	http                *http.Client
	apiURL              string
	exchangeURL         string
	mu                  sync.Mutex
	tokens              map[string]Token
	installationChecked bool
}

func New(options Options) (*Client, error) {
	if options.AppID <= 0 || options.InstallationID <= 0 || options.ClientID == "" || options.ClientSecret == "" || !namePattern.MatchString(options.Organization) {
		return nil, errors.New("GitHub App IDs, client credentials and organization are required")
	}
	if options.Team != "" && !namePattern.MatchString(options.Team) {
		return nil, errors.New("invalid GitHub team slug")
	}
	if len(options.Repositories) == 0 || len(options.Repositories) > 500 {
		return nil, errors.New("select 1–500 GitHub repositories")
	}
	seen := map[string]bool{}
	for i, repo := range options.Repositories {
		parts := strings.Split(repo, "/")
		if len(parts) != 2 || !strings.EqualFold(parts[0], options.Organization) || !namePattern.MatchString(parts[1]) || strings.HasSuffix(parts[1], ".git") {
			return nil, errors.New("repositories must be organization/name without .git")
		}
		options.Repositories[i] = strings.ToLower(repo)
		if seen[options.Repositories[i]] {
			return nil, errors.New("duplicate GitHub repository")
		}
		seen[options.Repositories[i]] = true
	}
	raw, err := os.ReadFile(options.PrivateKeyFile)
	if err != nil {
		return nil, errors.New("cannot read GitHub App private key")
	}
	block, _ := pem.Decode(raw)
	if block == nil {
		return nil, errors.New("invalid GitHub App PEM key")
	}
	key, err := x509.ParsePKCS1PrivateKey(block.Bytes)
	if err != nil {
		parsed, e := x509.ParsePKCS8PrivateKey(block.Bytes)
		if e != nil {
			return nil, errors.New("invalid GitHub App private key")
		}
		var ok bool
		key, ok = parsed.(*rsa.PrivateKey)
		if !ok {
			return nil, errors.New("GitHub App key must be RSA")
		}
	}
	if key.N.BitLen() < 2048 {
		return nil, errors.New("GitHub App RSA key must be at least 2048 bits")
	}
	return &Client{options: options, key: key, http: &http.Client{Timeout: 10 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}, apiURL: "https://api.github.com", exchangeURL: "https://github.com/login/oauth/access_token", tokens: map[string]Token{}}, nil
}

func (c *Client) Repositories() []string { return append([]string(nil), c.options.Repositories...) }
func (c *Client) Organization() string   { return c.options.Organization }
func (c *Client) Team() string           { return c.options.Team }

func (c *Client) AuthorizationURL(callback, state, verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	q := url.Values{"client_id": {c.options.ClientID}, "redirect_uri": {callback}, "state": {state}, "code_challenge": {base64.RawURLEncoding.EncodeToString(sum[:])}, "code_challenge_method": {"S256"}}
	return "https://github.com/login/oauth/authorize?" + q.Encode()
}

func (c *Client) Login(ctx context.Context, code, callback, verifier string) (User, error) {
	form := url.Values{"client_id": {c.options.ClientID}, "client_secret": {c.options.ClientSecret}, "code": {code}, "redirect_uri": {callback}, "code_verifier": {verifier}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.exchangeURL, strings.NewReader(form.Encode()))
	if err != nil {
		return User{}, errors.New("invalid GitHub exchange request")
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	var result struct {
		AccessToken string `json:"access_token"`
		Error       string `json:"error"`
	}
	if err = c.send(req, &result); err != nil {
		return User{}, err
	}
	if result.Error != "" || result.AccessToken == "" {
		return User{}, ErrDenied
	}
	var user User
	if err = c.api(ctx, http.MethodGet, "/user", result.AccessToken, nil, &user); err != nil {
		return User{}, err
	}
	if user.ID <= 0 || !namePattern.MatchString(user.Login) {
		return User{}, ErrDenied
	}
	// Membership is checked with a separate, organization-read installation token.
	if err = c.CheckMember(ctx, user.Login); err != nil {
		return User{}, err
	}
	return user, nil // deliberately discard the user access/refresh tokens
}

func (c *Client) CheckMember(ctx context.Context, login string) error {
	if !namePattern.MatchString(login) {
		return ErrDenied
	}
	token, err := c.installationToken(ctx, "membership")
	if err != nil {
		return err
	}
	path := "/orgs/" + url.PathEscape(c.options.Organization) + "/memberships/" + url.PathEscape(login)
	if c.options.Team != "" {
		path = "/orgs/" + url.PathEscape(c.options.Organization) + "/teams/" + url.PathEscape(c.options.Team) + "/memberships/" + url.PathEscape(login)
	}
	var member struct {
		State string `json:"state"`
	}
	if err = c.api(ctx, http.MethodGet, path, token.Value, nil, &member); err != nil {
		return err
	}
	if member.State != "active" {
		return ErrDenied
	}
	return nil
}

func (c *Client) RepositoryToken(ctx context.Context, repo string) (Token, error) {
	repo = strings.ToLower(repo)
	for _, allowed := range c.options.Repositories {
		if repo == allowed {
			return c.installationToken(ctx, repo)
		}
	}
	return Token{}, ErrDenied
}

// Serialize minting so concurrent helpers do not race token renewal. Renew five
// minutes early; callers still reject an expired token if GitHub is unavailable.
func (c *Client) installationToken(ctx context.Context, scope string) (Token, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if t, ok := c.tokens[scope]; ok && time.Until(t.ExpiresAt) > 5*time.Minute {
		return t, nil
	}
	jwt, err := c.jwt()
	if err != nil {
		return Token{}, err
	}
	if !c.installationChecked {
		var installation struct {
			Account struct {
				Login string `json:"login"`
			} `json:"account"`
			SuspendedAt *string `json:"suspended_at"`
		}
		if err = c.api(ctx, http.MethodGet, "/app/installations/"+strconv.FormatInt(c.options.InstallationID, 10), jwt, nil, &installation); err != nil {
			return Token{}, err
		}
		if !strings.EqualFold(installation.Account.Login, c.options.Organization) || installation.SuspendedAt != nil {
			return Token{}, ErrDenied
		}
		c.installationChecked = true
	}
	var payload any
	if scope == "membership" {
		payload = map[string]any{"permissions": map[string]string{"members": "read"}}
	} else {
		payload = map[string]any{"repositories": []string{strings.Split(scope, "/")[1]}, "permissions": map[string]string{"contents": "write", "pull_requests": "write", "actions": "write"}}
	}
	var token Token
	if err = c.api(ctx, http.MethodPost, "/app/installations/"+strconv.FormatInt(c.options.InstallationID, 10)+"/access_tokens", jwt, payload, &token); err != nil {
		return Token{}, err
	}
	if token.Value == "" || time.Until(token.ExpiresAt) < time.Minute {
		return Token{}, errors.New("GitHub returned an invalid installation token")
	}
	c.tokens[scope] = token
	return token, nil
}

func (c *Client) jwt() (string, error) {
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"RS256","typ":"JWT"}`))
	claims, _ := json.Marshal(map[string]any{"iat": time.Now().Add(-time.Minute).Unix(), "exp": time.Now().Add(9 * time.Minute).Unix(), "iss": strconv.FormatInt(c.options.AppID, 10)})
	input := header + "." + base64.RawURLEncoding.EncodeToString(claims)
	digest := sha256.Sum256([]byte(input))
	signature, err := rsa.SignPKCS1v15(rand.Reader, c.key, crypto.SHA256, digest[:])
	if err != nil {
		return "", errors.New("cannot sign GitHub App JWT")
	}
	return input + "." + base64.RawURLEncoding.EncodeToString(signature), nil
}

func (c *Client) api(ctx context.Context, method, path, token string, body, out any) error {
	var data io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return errors.New("cannot encode GitHub request")
		}
		data = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.apiURL+path, data)
	if err != nil {
		return errors.New("invalid GitHub request")
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", "pi-pocket8s-portal")
	return c.send(req, out)
}
func (c *Client) send(req *http.Request, out any) error {
	resp, err := c.http.Do(req)
	if err != nil {
		return errors.New("GitHub request unavailable")
	}
	defer resp.Body.Close()
	if resp.StatusCode == 401 || resp.StatusCode == 403 || resp.StatusCode == 404 {
		return ErrDenied
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("GitHub request failed (status %d)", resp.StatusCode)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, (1<<20)+1))
	if err != nil || len(raw) > 1<<20 {
		return errors.New("invalid GitHub response")
	}
	if err = json.Unmarshal(raw, out); err != nil {
		return errors.New("invalid GitHub response")
	}
	return nil
}
