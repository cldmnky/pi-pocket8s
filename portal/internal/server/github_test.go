package server

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/cldmnky/pi-pocket8s/portal/internal/config"
	"github.com/cldmnky/pi-pocket8s/portal/internal/githubapp"
	"github.com/cldmnky/pi-pocket8s/portal/internal/kube"
)

type fakeGitHub struct{}

func (fakeGitHub) AuthorizationURL(callback, state, verifier string) string {
	return "https://github.com/login/oauth/authorize?state=" + url.QueryEscape(state)
}
func (fakeGitHub) Login(context.Context, string, string, string) (githubapp.User, error) {
	return githubapp.User{ID: 42, Login: "alice"}, nil
}
func (fakeGitHub) CheckMember(context.Context, string) error { return nil }
func (fakeGitHub) Organization() string                      { return "example" }
func (fakeGitHub) Team() string                              { return "builders" }
func (fakeGitHub) Repositories() []string                    { return []string{"example/repo"} }
func (fakeGitHub) RepositoryToken(ctx context.Context, repo string) (githubapp.Token, error) {
	if repo != "example/repo" {
		return githubapp.Token{}, errors.New("denied")
	}
	return githubapp.Token{Value: "only-scoped-bot-token", ExpiresAt: time.Now().Add(time.Hour)}, nil
}

func TestGitHubSessionsBrokerAudienceIdentityAndNoBearerBypass(t *testing.T) {
	api := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/apis/authentication.k8s.io/v1/tokenreviews" {
			t.Errorf("unexpected API path %s", r.URL.Path)
			w.WriteHeader(404)
			return
		}
		if r.Header.Get("Authorization") != "Bearer portal-api-token" {
			t.Error("wrong portal SA token")
		}
		var body struct {
			Spec struct {
				Token     string   `json:"token"`
				Audiences []string `json:"audiences"`
			} `json:"spec"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if len(body.Spec.Audiences) != 1 || body.Spec.Audiences[0] != "pi-pocket-github" {
			t.Error("broker audience not requested")
		}
		username := "system:serviceaccount:workspace:pocket"
		audience := "pi-pocket-github"
		if body.Spec.Token == "wrong-sa" {
			username = "system:serviceaccount:other:pocket"
		}
		if body.Spec.Token == "wrong-audience" {
			audience = "kubernetes"
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"status": map[string]any{"authenticated": body.Spec.Token != "invalid", "audiences": []string{audience}, "user": map[string]string{"username": username}}})
	}))
	defer api.Close()
	dir := t.TempDir()
	cert, _ := x509.ParseCertificate(api.TLS.Certificates[0].Certificate[0])
	ca := filepath.Join(dir, "ca")
	token := filepath.Join(dir, "sa")
	_ = os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Raw}), 0600)
	_ = os.WriteFile(token, []byte("portal-api-token"), 0600)
	kc, err := kube.NewClient(api.URL, ca, token, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{AuthMode: "github", Namespace: "workspace", WorkspaceServiceAccount: "pocket", PortalOrigin: "https://portal.example", PocketURL: "https://pocket.example", TokenFile: token}
	s := New(cfg, kc, nil, fstest.MapFS{})
	s.EnableGitHub(fakeGitHub{})
	handler := s.Handler()
	request := func(path, bearer, body, origin string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("POST", path, strings.NewReader(body))
		if bearer != "" {
			r.Header.Set("Authorization", "Bearer "+bearer)
		}
		if origin != "" {
			r.Header.Set("Origin", origin)
		}
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w
	}
	for _, sa := range []string{"", "invalid", "wrong-sa", "wrong-audience"} {
		if w := request("/api/github/credentials", sa, `{"repository":"example/repo"}`, ""); w.Code != 401 {
			t.Errorf("identity %s accepted: %d", sa, w.Code)
		}
	}
	if w := request("/api/github/credentials", "valid", `{"repository":"example/repo"}`, ""); w.Code != 200 || !strings.Contains(w.Body.String(), "only-scoped-bot-token") {
		t.Errorf("broker failed %d %s", w.Code, w.Body)
	}
	if w := request("/api/github/credentials", "valid", `{"repository":"other/repo"}`, ""); w.Code != 403 || strings.Contains(w.Body.String(), "only-scoped") {
		t.Error("repository allowlist bypass")
	}
	if w := request("/api/github/credentials", "valid", `{"repository":"example/repo","permissions":{"administration":"write"}}`, ""); w.Code != 400 {
		t.Error("caller controlled permissions")
	}
	r := httptest.NewRequest("GET", "/api/github/status", nil)
	r.Header.Set("Authorization", "Bearer portal-api-token")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	if w.Code != 401 {
		t.Error("bearer token bypassed SSO")
	}
	w = request("/api/deployment/start", "valid", "", "https://evil.example")
	if w.Code != 403 {
		t.Error("cross-origin mutation accepted")
	}
	w = request("/api/deployment/start", "valid", "", "")
	if w.Code != 403 {
		t.Error("missing-origin cookie mutation accepted")
	}
	start := httptest.NewRecorder()
	handler.ServeHTTP(start, httptest.NewRequest("GET", "/auth/github/start", nil))
	target, _ := url.Parse(start.Header().Get("Location"))
	callback := httptest.NewRequest("GET", "/auth/github/callback?state="+target.Query().Get("state")+"&code=code", nil)
	callback.AddCookie(start.Result().Cookies()[0])
	login := httptest.NewRecorder()
	handler.ServeHTTP(login, callback)
	if login.Code != 303 {
		t.Fatal("SSO callback failed")
	}
	for _, cookie := range login.Result().Cookies() {
		if cookie.Name == "__Host-pocket-session" {
			r = httptest.NewRequest("GET", "/api/github/status", nil)
			r.AddCookie(cookie)
			w = httptest.NewRecorder()
			handler.ServeHTTP(w, r)
			if w.Code != 200 || !strings.Contains(w.Body.String(), "example/repo") || strings.Contains(w.Body.String(), "only-scoped-bot-token") {
				t.Error("session/status failed or exposed credential")
			}
		}
	}
}
