package config_test

import (
	"strings"
	"testing"

	"github.com/cldmnky/pi-pocket8s/portal/internal/config"
)

func setEnv(t *testing.T, values map[string]string) {
	t.Helper()
	for _, key := range []string{
		"POD_NAMESPACE", "POCKET_DEPLOYMENT", "CONFIG_SECRET",
		"POCKET_URL", "PORTAL_ORIGIN", "PORTAL_TOKEN_FILE",
	} {
		value := values[key]
		t.Setenv(key, value)
	}
}

func validEnv() map[string]string {
	return map[string]string{
		"POD_NAMESPACE":     "pi-pocket",
		"POCKET_DEPLOYMENT": "pi-pocket",
		"CONFIG_SECRET":     "pi-pocket-runtime",
		"POCKET_URL":        "https://pi.example.com",
		"PORTAL_ORIGIN":     "https://portal.example.com",
	}
}

func TestFromEnvValid(t *testing.T) {
	setEnv(t, validEnv())
	cfg, err := config.FromEnv()
	if err != nil {
		t.Fatalf("FromEnv() error = %v", err)
	}
	if cfg.TokenFile != config.DefaultTokenFile {
		t.Errorf("TokenFile = %q, want default %q", cfg.TokenFile, config.DefaultTokenFile)
	}
	if cfg.Namespace != "pi-pocket" || cfg.Deployment != "pi-pocket" || cfg.ConfigSecret != "pi-pocket-runtime" {
		t.Errorf("unexpected identity fields: %+v", cfg)
	}
	if cfg.PortalOrigin != "https://portal.example.com" {
		t.Errorf("PortalOrigin = %q", cfg.PortalOrigin)
	}
}

func TestFromEnvTokenFileOverride(t *testing.T) {
	env := validEnv()
	env["PORTAL_TOKEN_FILE"] = "/custom/token"
	setEnv(t, env)
	cfg, err := config.FromEnv()
	if err != nil {
		t.Fatalf("FromEnv() error = %v", err)
	}
	if cfg.TokenFile != "/custom/token" {
		t.Errorf("TokenFile = %q, want /custom/token", cfg.TokenFile)
	}
}

func TestFromEnvRejectsInvalid(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(map[string]string)
		wantErr string
	}{
		{"missing namespace", func(env map[string]string) { env["POD_NAMESPACE"] = "" }, "POD_NAMESPACE"},
		{"uppercase namespace", func(env map[string]string) { env["POD_NAMESPACE"] = "Pi-Pocket" }, "POD_NAMESPACE"},
		{"missing deployment", func(env map[string]string) { env["POCKET_DEPLOYMENT"] = "" }, "POCKET_DEPLOYMENT"},
		{"invalid deployment", func(env map[string]string) { env["POCKET_DEPLOYMENT"] = "pi_pocket" }, "POCKET_DEPLOYMENT"},
		{"missing secret", func(env map[string]string) { env["CONFIG_SECRET"] = "" }, "CONFIG_SECRET"},
		{"missing pocket url", func(env map[string]string) { env["POCKET_URL"] = "" }, "POCKET_URL"},
		{"pocket url http", func(env map[string]string) { env["POCKET_URL"] = "http://pi.example.com" }, "POCKET_URL"},
		{"pocket url userinfo", func(env map[string]string) { env["POCKET_URL"] = "https://user@pi.example.com" }, "POCKET_URL"},
		{"pocket url query", func(env map[string]string) { env["POCKET_URL"] = "https://pi.example.com/?debug=1" }, "POCKET_URL"},
		{"missing origin", func(env map[string]string) { env["PORTAL_ORIGIN"] = "" }, "PORTAL_ORIGIN"},
		{"origin http", func(env map[string]string) { env["PORTAL_ORIGIN"] = "http://portal.example.com" }, "PORTAL_ORIGIN"},
		{"origin with path", func(env map[string]string) { env["PORTAL_ORIGIN"] = "https://portal.example.com/portal" }, "PORTAL_ORIGIN"},
		{"origin with fragment", func(env map[string]string) { env["PORTAL_ORIGIN"] = "https://portal.example.com/#x" }, "PORTAL_ORIGIN"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			env := validEnv()
			tt.mutate(env)
			setEnv(t, env)
			_, err := config.FromEnv()
			if err == nil {
				t.Fatalf("FromEnv() = nil error, want error mentioning %s", tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("error = %q, want it to mention %s", err, tt.wantErr)
			}
		})
	}
}

func TestValidateCanonicalizesOrigin(t *testing.T) {
	cfg := config.Config{
		Namespace:    "pi-pocket",
		Deployment:   "pi-pocket",
		ConfigSecret: "pi-pocket-runtime",
		PocketURL:    "https://pi.example.com",
		PortalOrigin: "https://PORTAL.example.com/",
		TokenFile:    config.DefaultTokenFile,
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate() error = %v", err)
	}
	if cfg.PortalOrigin != "https://portal.example.com" {
		t.Errorf("PortalOrigin = %q, want canonical https://portal.example.com", cfg.PortalOrigin)
	}
}

func TestNormalizeOrigin(t *testing.T) {
	tests := []struct {
		in      string
		want    string
		wantErr bool
	}{
		{"https://portal.example.com", "https://portal.example.com", false},
		{"https://PORTAL.example.com:443", "https://portal.example.com", false},
		{"https://portal.example.com:443/", "https://portal.example.com", false},
		{"https://portal.example.com:8443", "https://portal.example.com:8443", false},
		{"http://portal.example.com", "", true},
		{"https://portal.example.com/portal", "", true},
		{"https://portal.example.com?debug=1", "", true},
		{"https://user@portal.example.com", "", true},
		{"null", "", true},
		{"", "", true},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			got, err := config.NormalizeOrigin(tt.in)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("NormalizeOrigin(%q) = %q, want error", tt.in, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("NormalizeOrigin(%q) error = %v", tt.in, err)
			}
			if got != tt.want {
				t.Errorf("NormalizeOrigin(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}
