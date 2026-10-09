package server_test

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"io"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cldmnky/pi-pocket8s/portal/internal/config"
	"github.com/cldmnky/pi-pocket8s/portal/internal/kube"
	"github.com/cldmnky/pi-pocket8s/portal/internal/server"
	"golang.org/x/crypto/ssh"
)

const (
	testNamespace      = "pi-pocket"
	testDeploymentName = "pi-pocket"
	testSecretName     = "pi-pocket-runtime"
	testOrigin         = "https://portal.example.com"
	testPortalToken    = "0123456789abcdef0123456789abcdef"
	testSAToken        = "service-account-token"
	testAPIKeyValue    = "sk-ant-secret-value-should-never-leak"

	testStoredAPIKeys = `{"ANTHROPIC_API_KEY":"` + testAPIKeyValue + `","OPENAI_API_KEY":"sk-openai-secret-value"}`
	testKnownHosts    = "pocket.example.com ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIPLACEHOLDER portal-test\n"
)

// portalTestEnv wires a portal server to a fake Kubernetes API server.
type portalTestEnv struct {
	t           *testing.T
	portal      *httptest.Server
	fake        *fakeKube
	tokenPath   string
	saTokenPath string
}

func writeFile(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// writeFileAtomic replaces a file via rename so readers never see it partial.
func writeFileAtomic(t *testing.T, path, contents string) {
	t.Helper()
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(contents), 0o600); err != nil {
		t.Fatalf("write %s: %v", tmp, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		t.Fatalf("rename %s: %v", tmp, err)
	}
}

// newTLSTestServer starts a TLS server with its own self-signed certificate so
// certificate verification can be tested.
func newTLSTestServer(t *testing.T, handler http.Handler) (*httptest.Server, string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	template := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "kube-api-test"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
		DNSNames:              []string{"localhost"},
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("::1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create certificate: %v", err)
	}
	kubeServer := httptest.NewUnstartedServer(handler)
	kubeServer.TLS = &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}}}
	kubeServer.StartTLS()
	return kubeServer, string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
}

// testAuthorizedKeys generates a fresh valid OpenSSH public key line.
func testAuthorizedKeys(t *testing.T) string {
	t.Helper()
	_, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate ssh key: %v", err)
	}
	public, err := ssh.NewPublicKey(private.Public())
	if err != nil {
		t.Fatalf("marshal ssh public key: %v", err)
	}
	return strings.TrimSpace(string(ssh.MarshalAuthorizedKey(public))) + " portal-test@example.com"
}

func newPortalTestEnv(t *testing.T) *portalTestEnv {
	t.Helper()
	dir := t.TempDir()
	tokenPath := filepath.Join(dir, "portal-token")
	writeFile(t, tokenPath, testPortalToken+"\n")
	saTokenPath := filepath.Join(dir, "sa-token")
	writeFile(t, saTokenPath, testSAToken)
	caPath := filepath.Join(dir, "ca.crt")

	fake := newFakeKube(testNamespace, testSecretName, testDeploymentName, testStoredAPIKeys, testAuthorizedKeys(t), testKnownHosts)
	kubeServer, caPEM := newTLSTestServer(t, fake)
	writeFile(t, caPath, caPEM)

	kubeClient, err := kube.NewClient(kubeServer.URL, caPath, saTokenPath, 5*time.Second)
	if err != nil {
		t.Fatalf("kube client: %v", err)
	}

	cfg := config.Config{
		Namespace:    testNamespace,
		Deployment:   testDeploymentName,
		ConfigSecret: testSecretName,
		PocketURL:    "https://pi.example.com",
		TerminalURL:  "https://terminal.example.com",
		PortalOrigin: testOrigin,
		TokenFile:    tokenPath,
	}
	handler := server.New(cfg, kubeClient, slog.New(slog.NewTextHandler(io.Discard, nil)), os.DirFS("../../web")).Handler()
	portal := httptest.NewServer(handler)
	t.Cleanup(func() {
		portal.Close()
		kubeServer.Close()
	})
	return &portalTestEnv{t: t, portal: portal, fake: fake, tokenPath: tokenPath, saTokenPath: saTokenPath}
}

type apiResult struct {
	Status int
	Header http.Header
	Body   []byte
}

func (e *portalTestEnv) request(method, path, token, origin string, body any) apiResult {
	e.t.Helper()
	var reader io.Reader
	if body != nil {
		switch value := body.(type) {
		case string:
			reader = strings.NewReader(value)
		case []byte:
			reader = bytes.NewReader(value)
		default:
			encoded, err := json.Marshal(value)
			if err != nil {
				e.t.Fatalf("marshal request body: %v", err)
			}
			reader = bytes.NewReader(encoded)
		}
	}
	req, err := http.NewRequest(method, e.portal.URL+path, reader)
	if err != nil {
		e.t.Fatalf("build request: %v", err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if origin != "" {
		req.Header.Set("Origin", origin)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := e.portal.Client().Do(req)
	if err != nil {
		e.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		e.t.Fatalf("%s %s: read body: %v", method, path, err)
	}
	return apiResult{Status: resp.StatusCode, Header: resp.Header.Clone(), Body: data}
}

func (e *portalTestEnv) getConfig() apiResult {
	e.t.Helper()
	return e.request(http.MethodGet, "/api/config", testPortalToken, "", nil)
}

func (e *portalTestEnv) postConfig(body any) apiResult {
	e.t.Helper()
	return e.request(http.MethodPost, "/api/config", testPortalToken, testOrigin, body)
}

func decodeJSON[T any](t *testing.T, result apiResult) T {
	t.Helper()
	var value T
	if err := json.Unmarshal(result.Body, &value); err != nil {
		t.Fatalf("decode response %q: %v", result.Body, err)
	}
	return value
}

type configViewResponse struct {
	ResourceVersion string          `json:"resourceVersion"`
	AllowedAPIKeys  []string        `json:"allowedApiKeys"`
	APIKeys         map[string]bool `json:"apiKeys"`
	AuthorizedKeys  string          `json:"authorizedKeys"`
	KnownHosts      string          `json:"knownHosts"`
	PocketURL       string          `json:"pocketUrl"`
	TerminalURL     string          `json:"terminalUrl"`
	OwnerLoginURL   string          `json:"ownerLoginUrl"`
	WebSearch       *struct {
		Provider string `json:"provider"`
		Model    string `json:"model"`
	} `json:"webSearch"`
}

type statusResponse struct {
	Namespace         string `json:"namespace"`
	Deployment        string `json:"deployment"`
	DesiredReplicas   int32  `json:"desiredReplicas"`
	ReadyReplicas     int32  `json:"readyReplicas"`
	AvailableReplicas int32  `json:"availableReplicas"`
	UpdatedReplicas   int32  `json:"updatedReplicas"`
	Running           bool   `json:"running"`
	RestartedAt       string `json:"restartedAt"`
	PocketURL         string `json:"pocketUrl"`
	TerminalURL       string `json:"terminalUrl"`
}

func TestHealthzIsUnauthenticated(t *testing.T) {
	env := newPortalTestEnv(t)
	result := env.request(http.MethodGet, "/healthz", "", "", nil)
	if result.Status != http.StatusOK {
		t.Fatalf("GET /healthz status = %d, want 200", result.Status)
	}
	if !strings.Contains(string(result.Body), "ok") {
		t.Errorf("GET /healthz body = %q, want ok", result.Body)
	}
	if result.Header.Get("Cache-Control") != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", result.Header.Get("Cache-Control"))
	}
	if !strings.Contains(result.Header.Get("Content-Security-Policy"), "default-src 'none'") {
		t.Errorf("Content-Security-Policy = %q", result.Header.Get("Content-Security-Policy"))
	}
}

func TestAPIRequiresBearerToken(t *testing.T) {
	env := newPortalTestEnv(t)
	tests := []struct {
		name  string
		token string
		want  int
	}{
		{"missing token", "", http.StatusUnauthorized},
		{"wrong token", "wrong-token-wrong-token-wrong-token", http.StatusUnauthorized},
		{"with cookie only", "", http.StatusUnauthorized},
		{"valid token", testPortalToken, http.StatusOK},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := env.request(http.MethodGet, "/api/config", tt.token, "", nil)
			if result.Status != tt.want {
				t.Fatalf("status = %d, want %d (body %s)", result.Status, tt.want, result.Body)
			}
			if tt.want == http.StatusUnauthorized && result.Header.Get("WWW-Authenticate") == "" {
				t.Error("401 response is missing WWW-Authenticate")
			}
		})
	}
}

func TestPortalTokenRotation(t *testing.T) {
	env := newPortalTestEnv(t)
	rotated := "fedcba9876543210fedcba9876543210"
	writeFileAtomic(t, env.tokenPath, rotated+"\n")

	if result := env.request(http.MethodGet, "/api/config", testPortalToken, "", nil); result.Status != http.StatusUnauthorized {
		t.Fatalf("stale token status = %d, want 401", result.Status)
	}
	if result := env.request(http.MethodGet, "/api/config", rotated, "", nil); result.Status != http.StatusOK {
		t.Fatalf("rotated token status = %d, want 200 (body %s)", result.Status, result.Body)
	}
}

func TestAPIFailsClosedWithoutStrongToken(t *testing.T) {
	env := newPortalTestEnv(t)
	if err := os.Remove(env.tokenPath); err != nil {
		t.Fatalf("remove token file: %v", err)
	}
	if result := env.request(http.MethodGet, "/api/config", testPortalToken, "", nil); result.Status != http.StatusServiceUnavailable {
		t.Fatalf("missing token file status = %d, want 503", result.Status)
	}
	writeFileAtomic(t, env.tokenPath, "weak")
	if result := env.request(http.MethodGet, "/api/config", testPortalToken, "", nil); result.Status != http.StatusServiceUnavailable {
		t.Fatalf("weak token status = %d, want 503", result.Status)
	}
}

func TestMutationsEnforceOrigin(t *testing.T) {
	env := newPortalTestEnv(t)
	env.fake.forgetRequests()

	if result := env.request(http.MethodPost, "/api/deployment/restart", testPortalToken, "https://evil.example", nil); result.Status != http.StatusForbidden {
		t.Fatalf("foreign origin status = %d, want 403", result.Status)
	}
	if result := env.request(http.MethodPost, "/api/deployment/restart", testPortalToken, "null", nil); result.Status != http.StatusForbidden {
		t.Fatalf("null origin status = %d, want 403", result.Status)
	}
	if result := env.request(http.MethodPost, "/api/deployment/restart", testPortalToken, "https://portal.example.com.evil.example", nil); result.Status != http.StatusForbidden {
		t.Fatalf("suffix origin status = %d, want 403", result.Status)
	}
	if count := env.fake.countRequests(http.MethodPatch, env.fake.deploymentPath()); count != 0 {
		t.Fatalf("rejected origins caused %d deployment patches, want 0", count)
	}
	if result := env.request(http.MethodPost, "/api/deployment/restart", testPortalToken, testOrigin, nil); result.Status != http.StatusOK {
		t.Fatalf("matching origin status = %d, want 200 (body %s)", result.Status, result.Body)
	}
	// A browser that spells out the default https port must also be accepted.
	if result := env.request(http.MethodPost, "/api/deployment/restart", testPortalToken, "https://portal.example.com:443", nil); result.Status != http.StatusOK {
		t.Fatalf("default-port origin status = %d, want 200 (body %s)", result.Status, result.Body)
	}
	// Reads are not mutations and must not be blocked by a foreign origin.
	if result := env.request(http.MethodGet, "/api/status", testPortalToken, "https://evil.example", nil); result.Status != http.StatusOK {
		t.Fatalf("GET with foreign origin status = %d, want 200", result.Status)
	}
}

func TestMethodStrictness(t *testing.T) {
	env := newPortalTestEnv(t)
	result := env.request(http.MethodDelete, "/api/config", testPortalToken, "", nil)
	if result.Status != http.StatusMethodNotAllowed {
		t.Fatalf("DELETE /api/config status = %d, want 405", result.Status)
	}
	allow := result.Header.Get("Allow")
	if !strings.Contains(allow, http.MethodGet) || !strings.Contains(allow, http.MethodPost) {
		t.Errorf("Allow = %q, want GET and POST", allow)
	}
	if result := env.request(http.MethodPut, "/api/status", testPortalToken, "", nil); result.Status != http.StatusMethodNotAllowed {
		t.Fatalf("PUT /api/status status = %d, want 405", result.Status)
	}
	if result := env.request(http.MethodGet, "/api/unknown", testPortalToken, "", nil); result.Status != http.StatusNotFound {
		t.Fatalf("GET /api/unknown status = %d, want 404", result.Status)
	}
}

func TestStaticSPA(t *testing.T) {
	env := newPortalTestEnv(t)

	index := env.request(http.MethodGet, "/", "", "", nil)
	if index.Status != http.StatusOK {
		t.Fatalf("GET / status = %d, want 200", index.Status)
	}
	if contentType := index.Header.Get("Content-Type"); !strings.Contains(contentType, "text/html") {
		t.Errorf("GET / Content-Type = %q, want text/html", contentType)
	}
	if index.Header.Get("Cache-Control") != "no-store" {
		t.Errorf("GET / Cache-Control = %q, want no-store", index.Header.Get("Cache-Control"))
	}
	if index.Header.Get("X-Content-Type-Options") != "nosniff" {
		t.Errorf("GET / X-Content-Type-Options = %q, want nosniff", index.Header.Get("X-Content-Type-Options"))
	}
	html := string(index.Body)
	for _, want := range []string{"<title>", `type="password"`, "authorized_keys", "known_hosts"} {
		if !strings.Contains(html, want) {
			t.Errorf("index.html does not contain %q", want)
		}
	}

	script := env.request(http.MethodGet, "/app.js", "", "", nil)
	if script.Status != http.StatusOK {
		t.Fatalf("GET /app.js status = %d, want 200", script.Status)
	}
	if contentType := script.Header.Get("Content-Type"); !strings.Contains(contentType, "text/javascript") {
		t.Errorf("GET /app.js Content-Type = %q, want text/javascript", contentType)
	}
	js := string(script.Body)
	for _, forbidden := range []string{"localStorage", "sessionStorage", "document.cookie", "innerHTML", "eval(", "new Function"} {
		if strings.Contains(js, forbidden) {
			t.Errorf("app.js must not use %s", forbidden)
		}
	}
	if !strings.Contains(js, "textContent") {
		t.Error("app.js does not use textContent for rendering")
	}

	style := env.request(http.MethodGet, "/app.css", "", "", nil)
	if style.Status != http.StatusOK {
		t.Fatalf("GET /app.css status = %d, want 200", style.Status)
	}
	if contentType := style.Header.Get("Content-Type"); !strings.Contains(contentType, "text/css") {
		t.Errorf("GET /app.css Content-Type = %q, want text/css", contentType)
	}

	qr := env.request(http.MethodGet, "/qrcodegen.js", "", "", nil)
	if qr.Status != http.StatusOK {
		t.Fatalf("GET /qrcodegen.js status = %d, want 200", qr.Status)
	}
	if contentType := qr.Header.Get("Content-Type"); !strings.Contains(contentType, "text/javascript") {
		t.Errorf("GET /qrcodegen.js Content-Type = %q, want text/javascript", contentType)
	}
	if !strings.Contains(string(qr.Body), "QrCode") {
		t.Error("qrcodegen.js does not define QrCode")
	}

	policy := index.Header.Get("Content-Security-Policy")
	if !strings.Contains(policy, "frame-src https://pi.example.com") {
		t.Errorf("Content-Security-Policy = %q, want frame-src for the agent origin", policy)
	}
	if !strings.Contains(html, "agent-frame") || !strings.Contains(html, "owner-qr") {
		t.Error("index.html does not contain the embedded workspace and owner QR elements")
	}

	for _, path := range []string{"/nope", "/../portal", "/api/unknown"} {
		if result := env.request(http.MethodGet, path, testPortalToken, "", nil); result.Status != http.StatusNotFound {
			t.Errorf("GET %s status = %d, want 404", path, result.Status)
		}
	}
}

func TestKubernetesErrorsAreSanitized(t *testing.T) {
	env := newPortalTestEnv(t)
	env.fake.failNextSecretPatch(http.StatusInternalServerError, "internal detail: "+testAPIKeyValue)
	body := map[string]any{
		"resourceVersion": env.fake.secretResourceVersion(),
		"apiKeys":         map[string]any{"set": map[string]string{"GROQ_API_KEY": "gsk-new-secret"}},
	}
	result := env.postConfig(body)
	if result.Status != http.StatusBadGateway {
		t.Fatalf("failed save status = %d, want 502 (body %s)", result.Status, result.Body)
	}
	if strings.Contains(string(result.Body), testAPIKeyValue) || strings.Contains(string(result.Body), "internal detail") {
		t.Fatalf("error response leaked upstream detail: %s", result.Body)
	}

	env.fake.failNextDeploymentGet(http.StatusNotFound, "deployment is named pi-pocket")
	result = env.request(http.MethodGet, "/api/status", testPortalToken, "", nil)
	if result.Status != http.StatusNotFound {
		t.Fatalf("missing deployment status = %d, want 404", result.Status)
	}
	if strings.Contains(string(result.Body), "deployment is named") {
		t.Fatalf("error response leaked upstream detail: %s", result.Body)
	}
}

func TestServiceAccountTokenRotatedPerRequest(t *testing.T) {
	env := newPortalTestEnv(t)
	if result := env.request(http.MethodGet, "/api/status", testPortalToken, "", nil); result.Status != http.StatusOK {
		t.Fatalf("status request = %d, want 200", result.Status)
	}
	if got := env.fake.lastSAToken(); got != "Bearer "+testSAToken {
		t.Fatalf("service account token = %q, want %q", got, "Bearer "+testSAToken)
	}

	writeFileAtomic(t, env.saTokenPath, "second-service-account-token")
	env.fake.forgetRequests()
	if result := env.request(http.MethodGet, "/api/status", testPortalToken, "", nil); result.Status != http.StatusOK {
		t.Fatalf("status request after rotation = %d, want 200", result.Status)
	}
	if got := env.fake.lastSAToken(); got != "Bearer second-service-account-token" {
		t.Fatalf("rotated service account token = %q, want %q", got, "Bearer second-service-account-token")
	}
}
