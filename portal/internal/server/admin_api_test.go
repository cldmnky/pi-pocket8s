package server

import (
	"context"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
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

	"github.com/cldmnky/pi-pocket8s/portal/internal/admin"
	"github.com/cldmnky/pi-pocket8s/portal/internal/config"
	"github.com/cldmnky/pi-pocket8s/portal/internal/githubapp"
	"github.com/cldmnky/pi-pocket8s/portal/internal/kube"
)

// adminProvider is a GitHub provider whose identity is selected by the OAuth
// code, so one test server can represent several distinct humans.
type adminProvider struct {
	fakeGitHub
	users map[string]githubapp.User
}

func (p adminProvider) Login(_ context.Context, code, _, _ string) (githubapp.User, error) {
	user, ok := p.users[code]
	if !ok {
		return githubapp.User{}, githubapp.ErrDenied
	}
	return user, nil
}

// adminAPI is an in-memory Kubernetes API for the elevation objects.
type adminAPI struct {
	mu           sync.Mutex
	sessionRV    int
	session      map[string]string
	runtime      map[string]string
	binding      []kube.Subject
	scale        int32
	requests     []string
	failSession  bool
	conflictOnce bool
}

func newAdminAPI() *adminAPI {
	return &adminAPI{sessionRV: 1, session: map[string]string{}, runtime: map[string]string{}, scale: 0}
}

func (a *adminAPI) writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func (a *adminAPI) secret(namespace, name string, data map[string]string) map[string]any {
	encoded := map[string]string{}
	for key, value := range data {
		encoded[key] = base64.StdEncoding.EncodeToString([]byte(value))
	}
	return map[string]any{
		"apiVersion": "v1", "kind": "Secret",
		"metadata": map[string]any{"name": name, "namespace": namespace, "resourceVersion": strconv.Itoa(a.sessionRV)},
		"data":     encoded,
	}
}

func (a *adminAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.requests = append(a.requests, r.Method+" "+r.URL.Path)
	switch {
	case r.URL.Path == "/api/v1/namespaces/management/secrets/admin-session":
		if a.failSession {
			a.writeJSON(w, 500, map[string]any{"message": "boom"})
			return
		}
		if r.Method == http.MethodPatch {
			if a.conflictOnce {
				a.conflictOnce = false
				a.writeJSON(w, 409, map[string]any{"message": "conflict"})
				return
			}
			var patch struct {
				Metadata struct {
					ResourceVersion string `json:"resourceVersion"`
				} `json:"metadata"`
				Data map[string]*string `json:"data"`
			}
			_ = json.NewDecoder(r.Body).Decode(&patch)
			if patch.Metadata.ResourceVersion != strconv.Itoa(a.sessionRV) {
				a.writeJSON(w, 409, map[string]any{"message": "conflict"})
				return
			}
			for key, value := range patch.Data {
				if value == nil {
					delete(a.session, key)
					continue
				}
				decoded, _ := base64.StdEncoding.DecodeString(*value)
				a.session[key] = string(decoded)
			}
			a.sessionRV++
		}
		a.writeJSON(w, 200, a.secret("management", "admin-session", a.session))
	case r.URL.Path == "/api/v1/namespaces/pi-pocket-admin/secrets/pi-pocket-admin-runtime":
		a.writeJSON(w, 200, a.secret("pi-pocket-admin", "pi-pocket-admin-runtime", a.runtime))
	case r.URL.Path == "/apis/apps/v1/namespaces/pi-pocket-admin/deployments/pi-pocket-admin":
		a.writeJSON(w, 200, map[string]any{
			"apiVersion": "apps/v1", "kind": "Deployment",
			"metadata": map[string]any{"name": "pi-pocket-admin", "namespace": "pi-pocket-admin", "uid": "deployment-uid", "resourceVersion": "1"},
			"spec":     map[string]any{"replicas": a.scale, "selector": map[string]any{"matchLabels": map[string]string{"app": "pi-pocket-admin"}}},
			"status":   map[string]any{"replicas": a.scale},
		})
	case r.URL.Path == "/apis/rbac.authorization.k8s.io/v1/clusterrolebindings/pi-pocket-admin-cluster-admin":
		a.writeJSON(w, 200, map[string]any{
			"apiVersion": "rbac.authorization.k8s.io/v1", "kind": "ClusterRoleBinding",
			"metadata": map[string]any{"name": "pi-pocket-admin-cluster-admin", "resourceVersion": "1"},
			"roleRef":  map[string]any{"apiGroup": "rbac.authorization.k8s.io", "kind": "ClusterRole", "name": "cluster-admin"},
			"subjects": a.binding,
		})
	case r.URL.Path == "/apis/authentication.k8s.io/v1/tokenreviews":
		a.writeJSON(w, 200, map[string]any{"status": map[string]any{"authenticated": true, "audiences": []string{"pi-pocket-github"}, "user": map[string]string{"username": "system:serviceaccount:workspace:pocket"}}})
	default:
		a.writeJSON(w, 404, map[string]any{"message": "not found: " + r.URL.Path})
	}
}

func (a *adminAPI) setSession(record admin.Record) {
	a.mu.Lock()
	defer a.mu.Unlock()
	payload, _ := json.Marshal(record)
	a.session[admin.StateKey] = string(payload)
}

func (a *adminAPI) record() admin.Record {
	a.mu.Lock()
	defer a.mu.Unlock()
	var record admin.Record
	_ = json.Unmarshal([]byte(a.session[admin.StateKey]), &record)
	return record
}

func (a *adminAPI) setRuntime(values map[string]string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.runtime = values
}

type adminEnv struct {
	t       *testing.T
	handler http.Handler
	api     *adminAPI
	cookies map[string]*http.Cookie
}

// newAdminEnv builds a GitHub-mode portal with elevation enabled. Each user
// gets their own session cookie, so operator identity is explicit per request.
func newAdminEnv(t *testing.T) *adminEnv {
	t.Helper()
	api := newAdminAPI()
	server := httptest.NewTLSServer(api)
	t.Cleanup(server.Close)
	dir := t.TempDir()
	cert, err := x509.ParseCertificate(server.TLS.Certificates[0].Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	caPath := filepath.Join(dir, "ca.crt")
	tokenPath := filepath.Join(dir, "sa-token")
	if err := os.WriteFile(caPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Raw}), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tokenPath, []byte("portal-sa-token"), 0o600); err != nil {
		t.Fatal(err)
	}
	client, err := kube.NewClient(server.URL, caPath, tokenPath, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{
		Namespace:               "pi-pocket",
		PortalNamespace:         "management",
		Deployment:              "pi-pocket",
		ConfigSecret:            "pi-pocket-runtime",
		PocketURL:               "https://pocket.example.com",
		PortalOrigin:            "https://portal.example",
		TokenFile:               tokenPath,
		AuthMode:                "github",
		WorkspaceServiceAccount: "pocket",
		Admin: config.AdminConfig{
			Enabled:            true,
			Namespace:          "pi-pocket-admin",
			Deployment:         "pi-pocket-admin",
			ServiceAccount:     "pi-pocket-admin",
			RuntimeSecret:      "pi-pocket-admin-runtime",
			PocketURL:          "https://pocket-admin.example.com",
			TerminalURL:        "https://pocket-admin-terminal.example.com",
			SessionSecret:      "admin-session",
			ClusterRoleBinding: "pi-pocket-admin-cluster-admin",
			ClusterRole:        "cluster-admin",
			Operators:          []int64{42, 77},
			DefaultDuration:    15 * time.Minute,
			MaxDuration:        30 * time.Minute,
			RecentLogin:        15 * time.Minute,
			StartupTimeout:     3 * time.Minute,
		},
	}
	s := New(cfg, client, nil, fstest.MapFS{})
	s.EnableGitHub(adminProvider{users: map[string]githubapp.User{
		"operator":  {ID: 42, Login: "operator"},
		"operator2": {ID: 77, Login: "operator2"},
		"member":    {ID: 99, Login: "member"},
	}})
	handler := s.Handler()

	env := &adminEnv{t: t, handler: handler, api: api, cookies: map[string]*http.Cookie{}}
	env.login("operator")
	env.login("operator2")
	env.login("member")
	return env
}

// login runs the OAuth flow for one identity and remembers its session cookie.
func (e *adminEnv) login(name string) {
	e.t.Helper()
	start := httptest.NewRecorder()
	e.handler.ServeHTTP(start, httptest.NewRequest("GET", "/auth/github/start", nil))
	target, _ := url.Parse(start.Header().Get("Location"))
	callback := httptest.NewRequest("GET", "/auth/github/callback?state="+target.Query().Get("state")+"&code="+name, nil)
	callback.AddCookie(start.Result().Cookies()[0])
	response := httptest.NewRecorder()
	e.handler.ServeHTTP(response, callback)
	for _, cookie := range response.Result().Cookies() {
		if cookie.Name == "__Host-pocket-session" {
			e.cookies[name] = cookie
			return
		}
	}
	e.t.Fatalf("no session cookie for %s", name)
}

func (e *adminEnv) request(user, method, path, body string, origin bool) *httptest.ResponseRecorder {
	e.t.Helper()
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	if cookie := e.cookies[user]; cookie != nil {
		r.AddCookie(cookie)
	}
	if origin {
		r.Header.Set("Origin", "https://portal.example")
	}
	w := httptest.NewRecorder()
	e.handler.ServeHTTP(w, r)
	return w
}

func TestAdminAPIRequiresOperatorIdentity(t *testing.T) {
	env := newAdminEnv(t)
	for _, path := range []string{"/api/admin/status", "/api/admin/activate", "/api/admin/revoke", "/api/admin/access"} {
		method := "GET"
		if path != "/api/admin/status" {
			method = "POST"
		}
		// Anonymous.
		if w := env.request("anonymous", method, path, `{}`, true); w.Code != 401 {
			t.Errorf("%s anonymous: %d", path, w.Code)
		}
		// A workspace service-account bearer token is not a portal session.
		r := httptest.NewRequest(method, path, strings.NewReader(`{}`))
		r.Header.Set("Authorization", "Bearer workspace-token")
		r.Header.Set("Origin", "https://portal.example")
		w := httptest.NewRecorder()
		env.handler.ServeHTTP(w, r)
		if w.Code != 401 {
			t.Errorf("%s workspace token: %d", path, w.Code)
		}
	}
}

func TestAdminStatusExposesCapabilitiesWithoutCredentials(t *testing.T) {
	env := newAdminEnv(t)
	w := env.request("operator", "GET", "/api/admin/status", "", false)
	if w.Code != 200 {
		t.Fatalf("status: %d %s", w.Code, w.Body)
	}
	body := w.Body.String()
	for _, leaked := range []string{"owner-secret", "token=", "ownerLoginUrl", "owner-login-url"} {
		if strings.Contains(body, leaked) {
			t.Fatalf("status leaked %q: %s", leaked, body)
		}
	}
	var view map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &view); err != nil {
		t.Fatal(err)
	}
	if view["enabled"] != true || view["isOperator"] != true || view["canActivate"] != true {
		t.Fatalf("unexpected status: %v", view)
	}
	if view["phase"] != "Idle" {
		t.Fatalf("phase: %v", view["phase"])
	}
	if view["session"] != nil {
		t.Fatalf("idle status carries a session: %v", view["session"])
	}
}

func TestAdminActivationValidationAndConcurrency(t *testing.T) {
	env := newAdminEnv(t)
	status := decodeInto[struct {
		ResourceVersion string `json:"resourceVersion"`
	}](t, env.request("operator", "GET", "/api/admin/status", "", false))

	// Wrong confirmation phrase.
	w := env.request("operator", "POST", "/api/admin/activate", `{"resourceVersion":"`+status.ResourceVersion+`","reason":"why","durationSeconds":300,"confirmation":"yes"}`, true)
	if w.Code != 400 {
		t.Fatalf("bad confirmation: %d %s", w.Code, w.Body)
	}
	// Missing reason.
	w = env.request("operator", "POST", "/api/admin/activate", `{"resourceVersion":"`+status.ResourceVersion+`","reason":"","durationSeconds":300,"confirmation":"cluster-admin"}`, true)
	if w.Code != 400 {
		t.Fatalf("empty reason: %d %s", w.Code, w.Body)
	}
	// Duration above the configured maximum.
	w = env.request("operator", "POST", "/api/admin/activate", `{"resourceVersion":"`+status.ResourceVersion+`","reason":"why","durationSeconds":7200,"confirmation":"cluster-admin"}`, true)
	if w.Code != 400 {
		t.Fatalf("over-long duration: %d %s", w.Code, w.Body)
	}
	// Unknown fields are rejected.
	w = env.request("operator", "POST", "/api/admin/activate", `{"resourceVersion":"`+status.ResourceVersion+`","reason":"why","confirmation":"cluster-admin","sessionID":"attacker"}`, true)
	if w.Code != 400 {
		t.Fatalf("client-supplied session id accepted: %d %s", w.Code, w.Body)
	}
	// Stale resource version.
	w = env.request("operator", "POST", "/api/admin/activate", `{"resourceVersion":"999","reason":"why","durationSeconds":300,"confirmation":"cluster-admin"}`, true)
	if w.Code != 409 {
		t.Fatalf("stale activation: %d %s", w.Code, w.Body)
	}
	// Cross-origin and missing-origin mutations are refused.
	if w := env.request("operator", "POST", "/api/admin/activate", `{}`, false); w.Code != 403 {
		t.Fatalf("missing origin: %d", w.Code)
	}
	// A valid activation is accepted and the server owns the identity fields.
	w = env.request("operator", "POST", "/api/admin/activate", `{"resourceVersion":"`+status.ResourceVersion+`","reason":"investigate a failed upgrade","durationSeconds":300,"confirmation":"cluster-admin"}`, true)
	if w.Code != 202 {
		t.Fatalf("activation: %d %s", w.Code, w.Body)
	}
	record := env.api.record()
	if record.RequestedState != admin.RequestedActive || record.ApprovedByUserID != 42 || record.SessionID == "" || record.SessionID == "attacker" {
		t.Fatalf("unexpected record: %s", record.Describe())
	}
	if record.ObservedPhase != admin.PhaseActivationRequested {
		t.Fatalf("phase: %s", record.ObservedPhase)
	}
	// A second session cannot start while the first is live.
	fresh := decodeInto[struct {
		ResourceVersion string `json:"resourceVersion"`
	}](t, env.request("operator", "GET", "/api/admin/status", "", false))
	w = env.request("operator", "POST", "/api/admin/activate", `{"resourceVersion":"`+fresh.ResourceVersion+`","reason":"second","durationSeconds":300,"confirmation":"cluster-admin"}`, true)
	if w.Code != 409 {
		t.Fatalf("concurrent activation: %d %s", w.Code, w.Body)
	}
}

func TestAdminRevocationIsAvailableToAnyOperator(t *testing.T) {
	env := newAdminEnv(t)
	env.api.setSession(admin.Record{
		SessionID: "session-1", RequestedState: admin.RequestedActive, ApprovedByUserID: 42, ApprovedByLogin: "operator",
		Reason: "live", ApprovedAt: time.Now(), ExpiresAt: time.Now().Add(10 * time.Minute), ObservedPhase: admin.PhaseActive, GrantObserved: true,
	})
	// An ordinary member who is not an authorized operator is refused.
	if w := env.request("member", "POST", "/api/admin/revoke", `{}`, true); w.Code != 403 {
		t.Fatalf("non-operator revocation: %d %s", w.Code, w.Body)
	}
	// Another authorized operator may revoke even though they do not own the session.
	w := env.request("operator2", "POST", "/api/admin/revoke", `{}`, true)
	if w.Code != 202 {
		t.Fatalf("revocation by another operator: %d %s", w.Code, w.Body)
	}
	if record := env.api.record(); record.RequestedState != admin.RequestedRevoked {
		t.Fatalf("not revoked: %s", record.Describe())
	}
	// Revoking again is a conflict, not a success.
	if w := env.request("operator2", "POST", "/api/admin/revoke", `{}`, true); w.Code != 202 && w.Code != 409 {
		t.Fatalf("unexpected repeat revocation: %d", w.Code)
	}
}

func TestAdminAccessIsOwnerOnlyFreshAndPodBound(t *testing.T) {
	env := newAdminEnv(t)
	now := time.Now()
	env.api.setSession(admin.Record{
		SessionID: "session-1", RequestedState: admin.RequestedActive, ApprovedByUserID: 42, ApprovedByLogin: "operator",
		Reason: "live", ApprovedAt: now, ExpiresAt: now.Add(10 * time.Minute),
		ObservedPhase: admin.PhaseActive, GrantObserved: true, WorkspaceReady: true,
		ActivePodUID: "pod-1", LastReconciledAt: now,
	})
	env.api.setRuntime(map[string]string{
		kube.OwnerLoginURLKey:    "https://pocket-admin.example.com/login?token=owner-secret",
		kube.OwnerLoginPodUIDKey: "pod-1",
	})

	// A different authorized operator must not receive the link.
	if w := env.request("operator2", "POST", "/api/admin/access", `{}`, true); w.Code != 403 {
		t.Fatalf("non-owner access: %d %s", w.Code, w.Body)
	}
	// The owner receives it, and only through this endpoint.
	w := env.request("operator", "POST", "/api/admin/access", `{}`, true)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "owner-secret") {
		t.Fatalf("owner access: %d %s", w.Code, w.Body)
	}
	if w.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("admin sign-in link must not be cacheable")
	}
	// A stale Pod UID must not be presented as the current workspace.
	env.api.setRuntime(map[string]string{
		kube.OwnerLoginURLKey:    "https://pocket-admin.example.com/login?token=owner-secret",
		kube.OwnerLoginPodUIDKey: "pod-old",
	})
	if w := env.request("operator", "POST", "/api/admin/access", `{}`, true); w.Code != 409 {
		t.Fatalf("stale pod uid accepted: %d %s", w.Code, w.Body)
	}
	// An unexpected origin is refused even when everything else matches.
	env.api.setRuntime(map[string]string{
		kube.OwnerLoginURLKey:    "https://evil.example.com/login?token=owner-secret",
		kube.OwnerLoginPodUIDKey: "pod-1",
	})
	if w := env.request("operator", "POST", "/api/admin/access", `{}`, true); w.Code != 409 {
		t.Fatalf("foreign origin accepted: %d %s", w.Code, w.Body)
	}
	// An expired session has no link.
	env.api.setSession(admin.Record{
		SessionID: "session-1", RequestedState: admin.RequestedActive, ApprovedByUserID: 42, ApprovedByLogin: "operator",
		Reason: "live", ApprovedAt: now.Add(-time.Hour), ExpiresAt: now.Add(-time.Minute),
		ObservedPhase: admin.PhaseActive, GrantObserved: true, WorkspaceReady: true,
		ActivePodUID: "pod-1", LastReconciledAt: now,
	})
	if w := env.request("operator", "POST", "/api/admin/access", `{}`, true); w.Code != 409 {
		t.Fatalf("expired session access: %d %s", w.Code, w.Body)
	}
}

func TestAdminStatusTreatsStaleAndMalformedStateAsUncertain(t *testing.T) {
	env := newAdminEnv(t)
	now := time.Now()
	env.api.setSession(admin.Record{
		SessionID: "session-1", RequestedState: admin.RequestedActive, ApprovedByUserID: 42, ApprovedByLogin: "operator",
		Reason: "live", ApprovedAt: now, ExpiresAt: now.Add(10 * time.Minute),
		ObservedPhase: admin.PhaseActive, GrantObserved: true,
		LastReconciledAt: now.Add(-10 * time.Minute),
	})
	type statusView struct {
		Phase       string `json:"phase"`
		Fresh       bool   `json:"fresh"`
		CanOpen     bool   `json:"canOpen"`
		CanRevoke   bool   `json:"canRevoke"`
		CanActivate bool   `json:"canActivate"`
		Failure     string `json:"failureCode"`
		CleanupPnd  bool   `json:"cleanupPending"`
	}
	view := decodeInto[statusView](t, env.request("operator", "GET", "/api/admin/status", "", false))
	if view.Fresh || view.Phase != string(admin.PhaseCleanupRequired) || view.CanOpen {
		t.Fatalf("stale observation presented as healthy: %+v", view)
	}
	if !view.CanRevoke {
		t.Fatal("revocation must remain available while state is uncertain")
	}

	env.api.mu.Lock()
	env.api.session[admin.StateKey] = "{not json"
	env.api.mu.Unlock()
	view = decodeInto[statusView](t, env.request("operator", "GET", "/api/admin/status", "", false))
	if view.Phase != string(admin.PhaseCleanupRequired) || view.Failure != admin.FailureStateInvalid || view.CanActivate {
		t.Fatalf("malformed state presented as usable: %+v", view)
	}
	if w := env.request("operator", "POST", "/api/admin/activate", `{"resourceVersion":"1","reason":"x","confirmation":"cluster-admin"}`, true); w.Code != 409 {
		t.Fatalf("activation against malformed state: %d %s", w.Code, w.Body)
	}
}

// TestAdminTokenModeElevation exercises the explicit token-auth opt-in: the
// bearer token is the operator, activation records a token approver, and the
// sign-in link is available to any holder of that token.
func TestAdminTokenModeElevation(t *testing.T) {
	api := newAdminAPI()
	server := httptest.NewTLSServer(api)
	defer server.Close()
	dir := t.TempDir()
	cert, err := x509.ParseCertificate(server.TLS.Certificates[0].Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	caPath := filepath.Join(dir, "ca.crt")
	tokenPath := filepath.Join(dir, "portal-token")
	if err := os.WriteFile(caPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Raw}), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tokenPath, []byte("0123456789abcdef0123456789abcdef"), 0o600); err != nil {
		t.Fatal(err)
	}
	client, err := kube.NewClient(server.URL, caPath, tokenPath, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{
		Namespace: "pi-pocket", PortalNamespace: "management", Deployment: "pi-pocket",
		ConfigSecret: "pi-pocket-runtime", PocketURL: "https://pocket.example.com",
		PortalOrigin: "https://portal.example", TokenFile: tokenPath, AuthMode: "token",
		Admin: config.AdminConfig{
			Enabled: true, AllowTokenAuth: true,
			Namespace: "pi-pocket-admin", Deployment: "pi-pocket-admin", ServiceAccount: "pi-pocket-admin",
			RuntimeSecret: "pi-pocket-admin-runtime", PocketURL: "https://pocket-admin.example.com",
			SessionSecret: "admin-session", ClusterRoleBinding: "pi-pocket-admin-cluster-admin",
			ClusterRole: "cluster-admin", Operators: []int64{42},
			DefaultDuration: 15 * time.Minute, MaxDuration: 30 * time.Minute,
			RecentLogin: 15 * time.Minute, StartupTimeout: 3 * time.Minute,
		},
	}
	handler := New(cfg, client, nil, fstest.MapFS{}).Handler()
	call := func(method, path, body string, withToken bool) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		if withToken {
			r.Header.Set("Authorization", "Bearer 0123456789abcdef0123456789abcdef")
		}
		if method == http.MethodPost {
			r.Header.Set("Origin", "https://portal.example")
		}
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w
	}
	// Without the token there is no operator, and the token-mode operator is not
	// required to appear on the GitHub allowlist.
	if w := call("GET", "/api/admin/status", "", false); w.Code != 401 {
		t.Fatalf("anonymous status: %d", w.Code)
	}
	status := decodeInto[struct {
		Enabled         bool   `json:"enabled"`
		IsOperator      bool   `json:"isOperator"`
		CanActivate     bool   `json:"canActivate"`
		ResourceVersion string `json:"resourceVersion"`
	}](t, call("GET", "/api/admin/status", "", true))
	if !status.Enabled || !status.IsOperator || !status.CanActivate {
		t.Fatalf("token operator not authorized: %+v", status)
	}
	if w := call("POST", "/api/admin/activate", `{"resourceVersion":"`+status.ResourceVersion+`","reason":"token-mode validation","durationSeconds":300,"confirmation":"cluster-admin"}`, true); w.Code != 202 {
		t.Fatalf("token-mode activation: %d %s", w.Code, w.Body)
	}
	record := api.record()
	if record.ApprovedByLogin != "token-operator" || record.ApprovedByUserID != 0 || record.RequestedState != admin.RequestedActive {
		t.Fatalf("unexpected approver: %s", record.Describe())
	}
	// The link is ownerless in token mode: any token holder may retrieve it.
	now := time.Now()
	api.setSession(admin.Record{
		SessionID: record.SessionID, RequestedState: admin.RequestedActive, ApprovedByLogin: "token-operator",
		Reason: "token-mode validation", ApprovedAt: now, ExpiresAt: now.Add(5 * time.Minute),
		ObservedPhase: admin.PhaseActive, GrantObserved: true, WorkspaceReady: true,
		ActivePodUID: "pod-1", LastReconciledAt: now,
	})
	api.setRuntime(map[string]string{
		kube.OwnerLoginURLKey:    "https://pocket-admin.example.com/login?token=owner-secret",
		kube.OwnerLoginPodUIDKey: "pod-1",
	})
	if w := call("POST", "/api/admin/access", `{}`, true); w.Code != 200 || !strings.Contains(w.Body.String(), "owner-secret") {
		t.Fatalf("token-mode access: %d %s", w.Code, w.Body)
	}
	// And revocation works the same way.
	if w := call("POST", "/api/admin/revoke", `{}`, true); w.Code != 202 {
		t.Fatalf("token-mode revocation: %d %s", w.Code, w.Body)
	}
	if record := api.record(); record.RequestedState != admin.RequestedRevoked {
		t.Fatalf("not revoked: %s", record.Describe())
	}
	// Without the explicit opt-in the same configuration refuses elevation.
	cfg.Admin.AllowTokenAuth = false
	handler = New(cfg, client, nil, fstest.MapFS{}).Handler()
	r := httptest.NewRequest("GET", "/api/admin/status", nil)
	r.Header.Set("Authorization", "Bearer 0123456789abcdef0123456789abcdef")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	if w.Code != 403 {
		t.Fatalf("token mode without the opt-in: %d %s", w.Code, w.Body)
	}
}

func decodeInto[T any](t *testing.T, w *httptest.ResponseRecorder) T {
	t.Helper()
	var value T
	if err := json.Unmarshal(w.Body.Bytes(), &value); err != nil {
		t.Fatalf("decode %q: %v", w.Body, err)
	}
	return value
}
