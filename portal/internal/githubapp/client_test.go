package githubapp

import (
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
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func testOptions(t *testing.T) Options {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(t.TempDir(), "app-key.pem")
	if err = os.WriteFile(file, pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}), 0600); err != nil {
		t.Fatal(err)
	}
	return Options{AppID: 123, InstallationID: 456, ClientID: "test-client", ClientSecret: "test-secret", PrivateKeyFile: file, Organization: "example", Team: "builders", Repositories: []string{"example/repo"}}
}

func TestLoginMembershipScopedTokensAndJWT(t *testing.T) {
	client, err := New(testOptions(t))
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	minted := 0
	denied := false
	api := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/exchange":
			_ = r.ParseForm()
			if r.Form.Get("code_verifier") != "verifier" || r.Form.Get("redirect_uri") != "https://portal.example/auth/github/callback" || r.Form.Get("client_secret") != "test-secret" {
				t.Error("missing PKCE/callback/client secret")
			}
			_, _ = w.Write([]byte(`{"access_token":"personal-user-token","refresh_token":"never-delegated"}`))
		case "/user":
			if r.Header.Get("Authorization") != "Bearer personal-user-token" {
				t.Error("incorrect identity token")
			}
			_, _ = w.Write([]byte(`{"id":42,"login":"alice"}`))
		case "/app/installations/456":
			jwt := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
			parts := strings.Split(jwt, ".")
			if len(parts) != 3 {
				t.Error("missing app JWT")
				w.WriteHeader(403)
				return
			}
			signature, _ := base64.RawURLEncoding.DecodeString(parts[2])
			digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
			if rsa.VerifyPKCS1v15(&client.key.PublicKey, crypto.SHA256, digest[:], signature) != nil {
				t.Error("invalid JWT signature")
			}
			_, _ = w.Write([]byte(`{"account":{"login":"example"},"suspended_at":null}`))
		case "/app/installations/456/access_tokens":
			var body struct {
				Repositories []string          `json:"repositories"`
				Permissions  map[string]string `json:"permissions"`
			}
			if json.NewDecoder(r.Body).Decode(&body) != nil {
				t.Error("bad mint payload")
			}
			mu.Lock()
			minted++
			mu.Unlock()
			value := "membership-token"
			if body.Permissions["contents"] != "" {
				value = "repo-bot-token"
				if len(body.Repositories) != 1 || body.Repositories[0] != "repo" || len(body.Permissions) != 3 || body.Permissions["contents"] != "write" || body.Permissions["pull_requests"] != "write" || body.Permissions["actions"] != "write" {
					t.Errorf("bad repository scope: %+v", body)
				}
			} else if len(body.Permissions) != 1 || body.Permissions["members"] != "read" {
				t.Errorf("bad membership scope: %+v", body)
			}
			if body.Permissions["workflows"] != "" || body.Permissions["administration"] != "" {
				t.Error("excess permissions")
			}
			_ = json.NewEncoder(w).Encode(Token{value, time.Now().Add(time.Hour)})
		case "/orgs/example/teams/builders/memberships/alice":
			if r.Header.Get("Authorization") != "Bearer membership-token" {
				t.Error("wrong membership token")
			}
			mu.Lock()
			pending := denied
			mu.Unlock()
			state := "active"
			if pending {
				state = "pending"
			}
			_ = json.NewEncoder(w).Encode(map[string]string{"state": state})
		default:
			t.Errorf("unexpected request %s", r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	defer api.Close()
	client.http = api.Client()
	client.apiURL = api.URL
	client.exchangeURL = api.URL + "/exchange"
	authorize, _ := url.Parse(client.AuthorizationURL("https://portal.example/auth/github/callback", "state", "verifier"))
	if authorize.Query().Get("code_challenge_method") != "S256" || len(authorize.Query().Get("code_challenge")) != 43 {
		t.Error("missing PKCE")
	}
	user, err := client.Login(context.Background(), "code", "https://portal.example/auth/github/callback", "verifier")
	if err != nil || user.ID != 42 {
		t.Fatalf("login: %+v %v", user, err)
	}
	token, err := client.RepositoryToken(context.Background(), "example/repo")
	if err != nil || token.Value != "repo-bot-token" {
		t.Fatalf("bot credentials: %+v %v", token, err)
	}
	if _, err = client.RepositoryToken(context.Background(), "other/repo"); !errors.Is(err, ErrDenied) {
		t.Error("out-of-scope repository allowed")
	}
	if _, err = client.RepositoryToken(context.Background(), "example/other"); !errors.Is(err, ErrDenied) {
		t.Error("unselected repository allowed")
	}
	_, _ = client.RepositoryToken(context.Background(), "example/repo")
	mu.Lock()
	if minted != 2 {
		t.Errorf("cache: minted %d tokens", minted)
	}
	mu.Unlock()
	client.mu.Lock()
	client.tokens["example/repo"] = Token{"expiring", time.Now().Add(time.Minute)}
	client.mu.Unlock()
	_, _ = client.RepositoryToken(context.Background(), "example/repo")
	mu.Lock()
	if minted != 3 {
		t.Error("expiring token was not renewed")
	}
	denied = true
	mu.Unlock()
	if err = client.CheckMember(context.Background(), "alice"); !errors.Is(err, ErrDenied) {
		t.Error("pending membership accepted")
	}
}

func TestInvalidConfigurationAndInstallation(t *testing.T) {
	options := testOptions(t)
	for _, repo := range []string{"*", "other/repo", "example/repo.git", "example/repo/extra"} {
		bad := options
		bad.Repositories = []string{repo}
		if _, err := New(bad); err == nil {
			t.Errorf("invalid repo accepted: %s", repo)
		}
	}
	c, err := New(options)
	if err != nil {
		t.Fatal(err)
	}
	api := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(`{"account":{"login":"other"}}`)) }))
	defer api.Close()
	c.http = api.Client()
	c.apiURL = api.URL
	if _, err = c.RepositoryToken(context.Background(), "example/repo"); !errors.Is(err, ErrDenied) {
		t.Error("installation for another org accepted")
	}
}

func TestUpstreamErrorsNeverExposeCredentials(t *testing.T) {
	c, err := New(testOptions(t))
	if err != nil {
		t.Fatal(err)
	}
	api := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
		_, _ = w.Write([]byte("personal-user-token app-private-key secret response"))
	}))
	defer api.Close()
	c.http = api.Client()
	c.exchangeURL = api.URL
	_, err = c.Login(context.Background(), "code", "https://portal.example/callback", "verifier")
	if err == nil || strings.Contains(err.Error(), "personal-user-token") || strings.Contains(err.Error(), "secret response") {
		t.Fatalf("unsanitized error %v", err)
	}
}

func TestSeparateRepositoryInstallationsPreserveLoginAndTokenScopes(t *testing.T) {
	options := testOptions(t)
	options.RepositoryInstallations = map[string]int64{"CLDMNKY": 789}
	options.Repositories = []string{"example/repo", "CLDMNKY/repo", "cldmnky/second"}
	c, err := New(options)
	if err != nil {
		t.Fatal(err)
	}
	// The client owns copies: caller mutations must not change authorization.
	options.RepositoryInstallations["CLDMNKY"] = 999
	options.Repositories[1] = "other/repo"
	var mu sync.Mutex
	checks, mints := map[string]int{}, map[string]int{}
	membershipActive := true
	api := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/exchange":
			_, _ = w.Write([]byte(`{"access_token":"user-login-token"}`))
		case "/user":
			_, _ = w.Write([]byte(`{"id":42,"login":"cldmnky"}`))
		case "/app/installations/456", "/app/installations/789":
			mu.Lock()
			checks[r.URL.Path]++
			mu.Unlock()
			owner := "example"
			if strings.HasSuffix(r.URL.Path, "/789") {
				owner = "cldmnky"
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"account": map[string]string{"login": owner}, "suspended_at": nil})
		case "/app/installations/456/access_tokens", "/app/installations/789/access_tokens":
			var body struct {
				Repositories []string          `json:"repositories"`
				Permissions  map[string]string `json:"permissions"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Error(err)
			}
			value := "membership-token"
			if body.Permissions["members"] == "read" {
				if r.URL.Path != "/app/installations/456/access_tokens" || len(body.Permissions) != 1 || len(body.Repositories) != 0 {
					t.Errorf("membership token used repository installation: %s %+v", r.URL.Path, body)
				}
			} else {
				if len(body.Repositories) != 1 || len(body.Permissions) != 3 || body.Permissions["contents"] != "write" || body.Permissions["pull_requests"] != "write" || body.Permissions["actions"] != "write" {
					t.Errorf("incorrect repository scope: %+v", body)
				}
				value = fmt.Sprintf("%s/%s", r.URL.Path, strings.Join(body.Repositories, ","))
			}
			mu.Lock()
			mints[value]++
			mu.Unlock()
			_ = json.NewEncoder(w).Encode(Token{value, time.Now().Add(time.Hour)})
		case "/orgs/example/teams/builders/memberships/cldmnky":
			if r.Header.Get("Authorization") != "Bearer membership-token" {
				t.Error("membership checked with repository or user token")
			}
			mu.Lock()
			state := "pending"
			if membershipActive {
				state = "active"
			}
			mu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]string{"state": state})
		default:
			t.Errorf("unexpected request: %s", r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	defer api.Close()
	c.http, c.apiURL, c.exchangeURL = api.Client(), api.URL, api.URL+"/exchange"
	if user, err := c.Login(context.Background(), "code", "https://portal.example/callback", "verifier"); err != nil || user.Login != "cldmnky" {
		t.Fatalf("organization-gated login: %+v %v", user, err)
	}
	for _, tt := range []struct{ repo, token string }{
		{"example/repo", "/app/installations/456/access_tokens/repo"},
		{"CLDMNKY/repo", "/app/installations/789/access_tokens/repo"},
		{"cldmnky/second", "/app/installations/789/access_tokens/second"},
	} {
		for range 2 {
			if token, err := c.RepositoryToken(context.Background(), tt.repo); err != nil || token.Value != tt.token {
				t.Fatalf("repository %s: %+v %v", tt.repo, token, err)
			}
		}
	}
	for _, repo := range []string{"cldmnky/unselected", "other/repo"} {
		if _, err := c.RepositoryToken(context.Background(), repo); !errors.Is(err, ErrDenied) {
			t.Errorf("unselected repository accepted: %s", repo)
		}
	}
	if err := c.CheckMember(context.Background(), "cldmnky"); err != nil {
		t.Fatal(err)
	}
	c.mu.Lock()
	c.tokens["cldmnky/repo"] = Token{"expiring", time.Now().Add(time.Minute)}
	c.mu.Unlock()
	if _, err := c.RepositoryToken(context.Background(), "cldmnky/repo"); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	membershipActive = false
	mu.Unlock()
	if _, err := c.Login(context.Background(), "code", "https://portal.example/callback", "verifier"); !errors.Is(err, ErrDenied) {
		t.Fatalf("personal repository access bypassed organization membership: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(checks) != 2 || checks["/app/installations/456"] != 1 || checks["/app/installations/789"] != 1 {
		t.Errorf("installation validation must be per installation: %v", checks)
	}
	if len(mints) != 4 || mints["membership-token"] != 1 || mints["/app/installations/456/access_tokens/repo"] != 1 || mints["/app/installations/789/access_tokens/repo"] != 2 || mints["/app/installations/789/access_tokens/second"] != 1 {
		t.Errorf("tokens must cache and renew per full repository name: %v", mints)
	}
}

func TestInvalidRepositoryInstallationConfiguration(t *testing.T) {
	options := testOptions(t)
	for _, tt := range []struct {
		name          string
		installations map[string]int64
	}{
		{"zero ID", map[string]int64{"cldmnky": 0}},
		{"negative ID", map[string]int64{"cldmnky": -1}},
		{"invalid owner", map[string]int64{"cldmnky/repo": 789}},
		{"wildcard owner", map[string]int64{"*": 789}},
		{"duplicate owner", map[string]int64{"cldmnky": 789, "CLDMNKY": 789}},
		{"membership installation reused", map[string]int64{"cldmnky": 456}},
		{"installation reused", map[string]int64{"cldmnky": 789, "other": 789}},
		{"membership installation overridden", map[string]int64{"example": 789}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			bad := options
			bad.RepositoryInstallations = tt.installations
			if _, err := New(bad); err == nil {
				t.Fatal("invalid installation configuration accepted")
			}
		})
	}
}

func TestRepositoryInstallationOwnerAndSuspensionCheckedIndependently(t *testing.T) {
	options := testOptions(t)
	options.RepositoryInstallations = map[string]int64{"cldmnky": 789}
	options.Repositories = []string{"example/repo", "cldmnky/repo"}
	for _, tt := range []struct {
		name     string
		status   int
		response string
	}{
		{"wrong owner", 200, `{"account":{"login":"other"},"suspended_at":null}`},
		{"suspended", 200, `{"account":{"login":"cldmnky"},"suspended_at":"2026-10-09T00:00:00Z"}`},
		{"removed installation", 404, `{"message":"Not Found"}`},
		{"unavailable", 500, `{"message":"Unavailable"}`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			c, err := New(options)
			if err != nil {
				t.Fatal(err)
			}
			api := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/app/installations/456":
					_, _ = w.Write([]byte(`{"account":{"login":"example"}}`))
				case "/app/installations/456/access_tokens":
					_ = json.NewEncoder(w).Encode(Token{"org-token", time.Now().Add(time.Hour)})
				case "/app/installations/789":
					w.WriteHeader(tt.status)
					_, _ = w.Write([]byte(tt.response))
				default:
					t.Errorf("must not mint for invalid repository installation: %s", r.URL.Path)
					w.WriteHeader(403)
				}
			}))
			defer api.Close()
			c.http, c.apiURL = api.Client(), api.URL
			if _, err := c.RepositoryToken(context.Background(), "example/repo"); err != nil {
				t.Fatal(err)
			}
			if _, err := c.RepositoryToken(context.Background(), "cldmnky/repo"); err == nil || (tt.status != 500 && !errors.Is(err, ErrDenied)) {
				t.Fatalf("invalid repository installation accepted: %v", err)
			}
		})
	}
}
