package kube_test

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cldmnky/pi-pocket8s/portal/internal/kube"
)

func writeFile(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// newTLSTestServer starts a TLS server with its own self-signed certificate so
// certificate verification can be tested (httptest.NewTLSServer would reuse a
// shared certificate).
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
	server := httptest.NewUnstartedServer(handler)
	server.TLS = &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}}}
	server.StartTLS()
	return server, string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
}

func TestNewClientValidation(t *testing.T) {
	dir := t.TempDir()
	server, caPEM := newTLSTestServer(t, http.NotFoundHandler())
	defer server.Close()
	caPath := filepath.Join(dir, "ca.crt")
	writeFile(t, caPath, caPEM)
	tokenPath := filepath.Join(dir, "token")
	writeFile(t, tokenPath, "sa-token")

	if _, err := kube.NewClient("http://kubernetes.default.svc", caPath, tokenPath, time.Second); err == nil {
		t.Error("http URL: want error, got nil")
	}
	if _, err := kube.NewClient("https://kubernetes.default.svc", filepath.Join(dir, "missing.crt"), tokenPath, time.Second); err == nil {
		t.Error("missing CA: want error, got nil")
	}
	garbageCA := filepath.Join(dir, "garbage.crt")
	writeFile(t, garbageCA, "not a certificate")
	if _, err := kube.NewClient("https://kubernetes.default.svc", garbageCA, tokenPath, time.Second); err == nil {
		t.Error("garbage CA: want error, got nil")
	}
	if _, err := kube.NewClient("https://kubernetes.default.svc", caPath, "", time.Second); err == nil {
		t.Error("empty token file: want error, got nil")
	}
}

func TestClientVerifiesTLSAndReadsTokenPerRequest(t *testing.T) {
	var seenTokens []string
	server, serverCA := newTLSTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenTokens = append(seenTokens, r.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"apiVersion":"v1","kind":"Secret","metadata":{"name":"s","namespace":"n","resourceVersion":"7"},"data":{}}`))
	}))
	defer server.Close()
	other, otherCA := newTLSTestServer(t, http.NotFoundHandler())
	defer other.Close()

	dir := t.TempDir()
	tokenPath := filepath.Join(dir, "token")
	writeFile(t, tokenPath, "sa-token-1")

	// A CA from a different server must not verify this server.
	otherCAPath := filepath.Join(dir, "other-ca.crt")
	writeFile(t, otherCAPath, otherCA)
	untrusted, err := kube.NewClient(server.URL, otherCAPath, tokenPath, 2*time.Second)
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}
	if _, err := untrusted.GetSecret(context.Background(), "n", "s"); err == nil {
		t.Fatal("untrusted CA: want TLS error, got nil")
	}

	caPath := filepath.Join(dir, "ca.crt")
	writeFile(t, caPath, serverCA)
	client, err := kube.NewClient(server.URL, caPath, tokenPath, 2*time.Second)
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}

	secret, err := client.GetSecret(context.Background(), "n", "s")
	if err != nil {
		t.Fatalf("GetSecret() error = %v", err)
	}
	if secret.Metadata.ResourceVersion != "7" {
		t.Errorf("ResourceVersion = %q, want 7", secret.Metadata.ResourceVersion)
	}

	writeFile(t, tokenPath, "sa-token-2\n")
	if _, err := client.GetSecret(context.Background(), "n", "s"); err != nil {
		t.Fatalf("GetSecret() after rotation error = %v", err)
	}
	want := []string{"Bearer sa-token-1", "Bearer sa-token-2"}
	if len(seenTokens) != len(want) {
		t.Fatalf("seen %d requests, want %d", len(seenTokens), len(want))
	}
	for i := range want {
		if seenTokens[i] != want[i] {
			t.Errorf("request %d Authorization = %q, want %q", i, seenTokens[i], want[i])
		}
	}
}

func TestClientFailsWithoutServiceAccountToken(t *testing.T) {
	server, caPEM := newTLSTestServer(t, http.NotFoundHandler())
	defer server.Close()
	dir := t.TempDir()
	caPath := filepath.Join(dir, "ca.crt")
	writeFile(t, caPath, caPEM)
	tokenPath := filepath.Join(dir, "token")

	client, err := kube.NewClient(server.URL, caPath, tokenPath, 2*time.Second)
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}
	if _, err := client.GetSecret(context.Background(), "n", "s"); err == nil || !strings.Contains(err.Error(), "service account token") {
		t.Fatalf("missing token file: error = %v, want service account token error", err)
	}

	writeFile(t, tokenPath, "   \n")
	if _, err := client.GetSecret(context.Background(), "n", "s"); err == nil || !strings.Contains(err.Error(), "empty") {
		t.Fatalf("empty token: error = %v, want empty token error", err)
	}
}

func TestClientMapsAPIStatusErrors(t *testing.T) {
	statusFor := func(code int) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(code)
			_, _ = w.Write([]byte(`{"kind":"Status","message":"resourceVersion conflict","code":409}`))
		}
	}
	conflictServer, conflictCA := newTLSTestServer(t, statusFor(http.StatusConflict))
	defer conflictServer.Close()

	dir := t.TempDir()
	caPath := filepath.Join(dir, "ca.crt")
	writeFile(t, caPath, conflictCA)
	tokenPath := filepath.Join(dir, "token")
	writeFile(t, tokenPath, "sa-token")

	client, err := kube.NewClient(conflictServer.URL, caPath, tokenPath, 2*time.Second)
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}
	_, err = client.PatchSecret(context.Background(), "n", "s", []byte(`{}`))
	if !kube.IsConflict(err) {
		t.Fatalf("PatchSecret() error = %v, want conflict", err)
	}
	if kube.IsNotFound(err) {
		t.Error("IsNotFound(conflict) = true, want false")
	}
	var apiErr *kube.APIError
	if !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusConflict {
		t.Fatalf("error = %v, want *kube.APIError with 409", err)
	}

	notFoundServer, notFoundCA := newTLSTestServer(t, statusFor(http.StatusNotFound))
	defer notFoundServer.Close()
	writeFile(t, caPath, notFoundCA)
	notFoundClient, err := kube.NewClient(notFoundServer.URL, caPath, tokenPath, 2*time.Second)
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}
	if _, err := notFoundClient.GetDeployment(context.Background(), "n", "d"); !kube.IsNotFound(err) {
		t.Fatalf("GetDeployment() error = %v, want not found", err)
	}
}

func TestInClusterClientRequiresEnvironment(t *testing.T) {
	t.Setenv("KUBERNETES_SERVICE_HOST", "")
	t.Setenv("KUBERNETES_SERVICE_PORT", "")
	if _, err := kube.InClusterClient(time.Second); err == nil {
		t.Fatal("InClusterClient() = nil error, want error")
	}

	t.Setenv("KUBERNETES_SERVICE_HOST", "10.0.0.1")
	t.Setenv("KUBERNETES_SERVICE_PORT", "443")
	if _, err := kube.InClusterClient(time.Second); err == nil {
		t.Fatal("InClusterClient() with no service account files = nil error, want error")
	}
}

func TestSecretBytes(t *testing.T) {
	secret := &kube.Secret{Data: map[string]string{
		"good": "aGVsbG8=",
		"bad":  "!!!",
	}}
	value, present, err := secret.Bytes("good")
	if err != nil || !present || string(value) != "hello" {
		t.Fatalf("Bytes(good) = %q, %v, %v", value, present, err)
	}
	if _, _, err := secret.Bytes("bad"); err == nil {
		t.Fatal("Bytes(bad) = nil error, want error")
	}
	if _, present, err := secret.Bytes("missing"); err != nil || present {
		t.Fatalf("Bytes(missing) = present %v, err %v", present, err)
	}
}
