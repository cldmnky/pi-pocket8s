package server

import (
	"context"
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
	"strconv"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"github.com/cldmnky/pi-pocket8s/portal/internal/config"
	"github.com/cldmnky/pi-pocket8s/portal/internal/kube"
)

type policyGitHub struct {
	fakeGitHub
	owner bool
}

func (p *policyGitHub) CheckOwner(context.Context, string) error {
	if !p.owner {
		return errors.New("not owner")
	}
	return nil
}

func TestRepositoryPolicyOwnerBoundaryAndImmediateRevocation(t *testing.T) {
	var mu sync.Mutex
	raw := `[]`
	rv := 1
	unavailable := false
	patches := 0
	api := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if r.URL.Path == "/apis/authentication.k8s.io/v1/tokenreviews" {
			json.NewEncoder(w).Encode(map[string]any{"status": map[string]any{"authenticated": true, "audiences": []string{"pi-pocket-github"}, "user": map[string]string{"username": "system:serviceaccount:workspace:pocket"}}})
			return
		}
		if r.URL.Path != "/api/v1/namespaces/management/secrets/policy" {
			t.Errorf("wrong policy boundary: %s", r.URL.Path)
			w.WriteHeader(404)
			return
		}
		if unavailable {
			w.WriteHeader(503)
			return
		}
		if r.Method == "PATCH" {
			var patch struct {
				Metadata struct {
					ResourceVersion string `json:"resourceVersion"`
				} `json:"metadata"`
				Data map[string]string `json:"data"`
			}
			json.NewDecoder(r.Body).Decode(&patch)
			if patch.Metadata.ResourceVersion != strconv.Itoa(rv) {
				w.WriteHeader(409)
				return
			}
			decoded, _ := base64.StdEncoding.DecodeString(patch.Data[repositoryPolicyKey])
			raw = string(decoded)
			rv++
			patches++
		}
		json.NewEncoder(w).Encode(map[string]any{"metadata": map[string]string{"resourceVersion": strconv.Itoa(rv)}, "data": map[string]string{repositoryPolicyKey: base64.StdEncoding.EncodeToString([]byte(raw))}})
	}))
	defer api.Close()
	dir := t.TempDir()
	cert, _ := x509.ParseCertificate(api.TLS.Certificates[0].Certificate[0])
	ca := filepath.Join(dir, "ca")
	token := filepath.Join(dir, "token")
	os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Raw}), 0600)
	os.WriteFile(token, []byte("sa"), 0600)
	kc, err := kube.NewClient(api.URL, ca, token, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	s := New(config.Config{AuthMode: "github", Namespace: "workspace", PortalNamespace: "management", RepositoryPolicySecret: "policy", WorkspaceServiceAccount: "pocket", PortalOrigin: "https://portal.example", PocketURL: "https://pocket.example"}, kc, nil, fstest.MapFS{})
	provider := &policyGitHub{owner: true}
	s.EnableGitHub(provider)
	handler := s.Handler()
	start := httptest.NewRecorder()
	handler.ServeHTTP(start, httptest.NewRequest("GET", "/auth/github/start", nil))
	target, _ := url.Parse(start.Header().Get("Location"))
	cb := httptest.NewRequest("GET", "/auth/github/callback?state="+target.Query().Get("state")+"&code=code", nil)
	cb.AddCookie(start.Result().Cookies()[0])
	login := httptest.NewRecorder()
	handler.ServeHTTP(login, cb)
	var cookie *http.Cookie
	for _, c := range login.Result().Cookies() {
		if c.Name == "__Host-pocket-session" {
			cookie = c
		}
	}
	if cookie == nil {
		t.Fatal("no session")
	}
	request := func(method, path, body, origin string, loggedIn bool) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		if loggedIn {
			r.AddCookie(cookie)
		}
		if origin != "" {
			r.Header.Set("Origin", origin)
		}
		if path == "/api/github/credentials" {
			r.Header.Set("Authorization", "Bearer workspace")
		}
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w
	}
	save := func(body string) *httptest.ResponseRecorder {
		return request("POST", "/api/github/repositories", body, "https://portal.example", true)
	}
	broker := func() *httptest.ResponseRecorder {
		return request("POST", "/api/github/credentials", `{"repository":"example/repo"}`, "", false)
	}
	if w := broker(); w.Code != 403 {
		t.Fatalf("empty initial policy: %d", w.Code)
	}
	if w := request("GET", "/api/github/repositories", "", "", false); w.Code != 401 {
		t.Fatal("anonymous catalog allowed")
	}
	provider.owner = false
	if w := request("GET", "/api/github/repositories", "", "", true); w.Code != 403 {
		t.Fatal("member can list owner catalog")
	}
	if w := save(`{"resourceVersion":"1","repositories":["example/repo"]}`); w.Code != 403 {
		t.Fatal("member can grant repos")
	}
	provider.owner = true
	if w := request("POST", "/api/github/repositories", `{"resourceVersion":"1","repositories":[]}`, "https://evil.example", true); w.Code != 403 {
		t.Fatal("cross-origin save allowed")
	}
	if w := request("POST", "/api/github/repositories", `{"resourceVersion":"1","repositories":[]}`, "", true); w.Code != 403 {
		t.Fatal("missing origin allowed")
	}
	if w := save(`{"resourceVersion":"1","repositories":["evil/repo"]}`); w.Code != 400 {
		t.Fatal("invalid repo allowed")
	}
	if w := save(`{"resourceVersion":"1","repositories":["example/repo"]}`); w.Code != 200 {
		t.Fatalf("save %d %s", w.Code, w.Body)
	}
	if w := broker(); w.Code != 200 {
		t.Fatalf("live grant %d %s", w.Code, w.Body)
	}
	if w := save(`{"resourceVersion":"1","repositories":[]}`); w.Code != 409 {
		t.Fatal("stale save overwrote policy")
	}
	if w := save(`{"resourceVersion":"2","repositories":[]}`); w.Code != 200 {
		t.Fatal("clear failed")
	}
	if w := broker(); w.Code != 403 {
		t.Fatal("revocation did not override cached token")
	}
	mu.Lock()
	raw = "null"
	mu.Unlock()
	if w := broker(); w.Code != 503 {
		t.Fatal("malformed policy authorized")
	}
	mu.Lock()
	unavailable = true
	mu.Unlock()
	if w := broker(); w.Code != 503 {
		t.Fatal("unreadable policy authorized")
	}
	mu.Lock()
	defer mu.Unlock()
	if patches != 2 {
		t.Fatalf("unexpected writes %d", patches)
	}
}
