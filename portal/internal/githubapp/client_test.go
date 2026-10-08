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
