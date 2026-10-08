package auth_test

import (
	"errors"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cldmnky/pi-pocket8s/portal/internal/auth"
)

const goodToken = "0123456789abcdef0123456789abcdef"

func writeToken(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatalf("write token file: %v", err)
	}
}

func TestCheckStrength(t *testing.T) {
	tests := []struct {
		name  string
		token string
		want  bool
	}{
		{"valid", goodToken, true},
		{"valid long", strings.Repeat("aB3!", 20), true},
		{"too short", "short-token", false},
		{"exactly minimum", strings.Repeat("x", auth.MinTokenLength-1) + "y", true},
		{"one below minimum", strings.Repeat("x", auth.MinTokenLength-1), false},
		{"empty", "", false},
		{"whitespace", goodToken + " tail", false},
		{"control", goodToken + "\x01", false},
		{"repeated character", strings.Repeat("a", 40), false},
		{"too long", strings.Repeat("aB", 3000), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := auth.CheckStrength(tt.token)
			if tt.want && err != nil {
				t.Errorf("CheckStrength(%q) = %v, want nil", tt.token, err)
			}
			if !tt.want && err == nil {
				t.Errorf("CheckStrength(%q) = nil, want error", tt.token)
			}
		})
	}
}

func TestLoadToken(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "token")

	if _, err := auth.LoadToken(path); !errors.Is(err, auth.ErrTokenUnavailable) {
		t.Fatalf("missing file: error = %v, want ErrTokenUnavailable", err)
	}

	writeToken(t, path, "")
	if _, err := auth.LoadToken(path); !errors.Is(err, auth.ErrTokenUnavailable) {
		t.Fatalf("empty file: error = %v, want ErrTokenUnavailable", err)
	}

	writeToken(t, path, "weak")
	if _, err := auth.LoadToken(path); !errors.Is(err, auth.ErrTokenUnavailable) {
		t.Fatalf("weak token: error = %v, want ErrTokenUnavailable", err)
	}

	writeToken(t, path, goodToken+"\n")
	token, err := auth.LoadToken(path)
	if err != nil {
		t.Fatalf("valid token: error = %v", err)
	}
	if token != goodToken {
		t.Errorf("token = %q, want %q", token, goodToken)
	}
}

func TestCheckBearer(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "token")
	writeToken(t, path, goodToken+"\n")

	tests := []struct {
		name   string
		header string
		want   error
	}{
		{"valid", "Bearer " + goodToken, nil},
		{"case-insensitive scheme", "bearer " + goodToken, nil},
		{"extra spaces trimmed", "Bearer   " + goodToken + "  ", nil},
		{"wrong token", "Bearer 0123456789abcdef0123456789abcdeF", auth.ErrUnauthorized},
		{"wrong length", "Bearer short", auth.ErrUnauthorized},
		{"empty header", "", auth.ErrUnauthorized},
		{"basic scheme", "Basic " + goodToken, auth.ErrUnauthorized},
		{"missing scheme", goodToken, auth.ErrUnauthorized},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := httptest.NewRequest("GET", "/api/config", nil)
			if tt.header != "" {
				r.Header.Set("Authorization", tt.header)
			}
			err := auth.Check(r, path)
			if tt.want == nil && err != nil {
				t.Fatalf("Check() = %v, want nil", err)
			}
			if tt.want != nil && !errors.Is(err, tt.want) {
				t.Fatalf("Check() = %v, want %v", err, tt.want)
			}
		})
	}
}

func TestCheckFailsClosedWhenTokenMissingOrWeak(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "token")
	r := httptest.NewRequest("GET", "/api/config", nil)
	r.Header.Set("Authorization", "Bearer "+goodToken)

	if err := auth.Check(r, path); !errors.Is(err, auth.ErrTokenUnavailable) {
		t.Fatalf("missing file: error = %v, want ErrTokenUnavailable", err)
	}
	writeToken(t, path, "weak")
	if err := auth.Check(r, path); !errors.Is(err, auth.ErrTokenUnavailable) {
		t.Fatalf("weak file: error = %v, want ErrTokenUnavailable", err)
	}
}

func TestCheckReloadsRotatedToken(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "token")
	writeToken(t, path, goodToken)

	first := httptest.NewRequest("GET", "/api/config", nil)
	first.Header.Set("Authorization", "Bearer "+goodToken)
	if err := auth.Check(first, path); err != nil {
		t.Fatalf("first token rejected: %v", err)
	}

	rotated := "fedcba9876543210fedcba9876543210"
	writeToken(t, path, rotated)

	stale := httptest.NewRequest("GET", "/api/config", nil)
	stale.Header.Set("Authorization", "Bearer "+goodToken)
	if err := auth.Check(stale, path); !errors.Is(err, auth.ErrUnauthorized) {
		t.Fatalf("stale token: error = %v, want ErrUnauthorized", err)
	}

	fresh := httptest.NewRequest("GET", "/api/config", nil)
	fresh.Header.Set("Authorization", "Bearer "+rotated)
	if err := auth.Check(fresh, path); err != nil {
		t.Fatalf("rotated token rejected: %v", err)
	}
}
