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
		"POCKET_URL", "TERMINAL_URL", "PORTAL_ORIGIN", "PORTAL_TOKEN_FILE", "POCKET_NAMESPACE", "PORTAL_AUTH_MODE", "POCKET_SERVICE_ACCOUNT", "GITHUB_CLIENT_ID", "GITHUB_APP_ID", "GITHUB_INSTALLATION_ID", "GITHUB_ORGANIZATION", "GITHUB_TEAM", "GITHUB_REPOSITORIES", "GITHUB_REPOSITORY_POLICY_SECRET", "GITHUB_REPOSITORY_INSTALLATIONS", "GITHUB_CLIENT_SECRET", "GITHUB_CLIENT_SECRET_FILE", "GITHUB_APP_PRIVATE_KEY_FILE",
		"ADMIN_ELEVATION_ENABLED", "ADMIN_NAMESPACE", "ADMIN_DEPLOYMENT", "ADMIN_SERVICE_ACCOUNT", "ADMIN_RUNTIME_SECRET", "ADMIN_POCKET_URL", "ADMIN_TERMINAL_URL", "ADMIN_SESSION_SECRET", "ADMIN_CLUSTER_ROLE_BINDING", "ADMIN_CLUSTER_ROLE", "ADMIN_OPERATORS", "ADMIN_BOOTSTRAP", "ADMIN_DEFAULT_DURATION_SECONDS", "ADMIN_MAX_DURATION_SECONDS", "ADMIN_RECENT_LOGIN_SECONDS", "ADMIN_STARTUP_TIMEOUT_SECONDS", "ADMIN_RECONCILE_SECONDS", "ADMIN_HEALTH_ADDR",
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

func TestGitHubAutoDetectionAndFailClosed(t *testing.T) {
	env := validEnv()
	setEnv(t, env)
	cfg, err := config.FromEnv()
	if err != nil || cfg.AuthMode != "token" {
		t.Fatalf("legacy mode: %s %v", cfg.AuthMode, err)
	}
	t.Setenv("GITHUB_CLIENT_ID", "client")
	if _, err = config.FromEnv(); err == nil {
		t.Error("partial GitHub configuration fell back to token")
	}
	env["POD_NAMESPACE"] = "management"
	env["POCKET_NAMESPACE"] = "workspace"
	env["POCKET_SERVICE_ACCOUNT"] = "pi-pocket"
	env["GITHUB_CLIENT_ID"] = "client"
	env["GITHUB_APP_ID"] = "123"
	env["GITHUB_INSTALLATION_ID"] = "456"
	env["GITHUB_ORGANIZATION"] = "example"
	env["GITHUB_REPOSITORY_POLICY_SECRET"] = "pocket-github-repositories"
	env["GITHUB_CLIENT_SECRET"] = "test-client-secret"
	setEnv(t, env)
	cfg, err = config.FromEnv()
	if err != nil || cfg.AuthMode != "github" || cfg.Namespace != "workspace" || cfg.PortalNamespace != "management" {
		t.Fatalf("auto GitHub mode: %+v %v", cfg, err)
	}
	if len(cfg.GitHub.RepositoryInstallations) != 0 {
		t.Error("legacy configuration gained repository installations")
	}
	t.Setenv("GITHUB_REPOSITORY_POLICY_SECRET", "")
	if _, err := config.FromEnv(); err == nil {
		t.Error("missing management policy Secret accepted")
	}
	t.Setenv("GITHUB_REPOSITORY_POLICY_SECRET", "pocket-github-repositories")
	t.Setenv("GITHUB_REPOSITORIES", "cldmnky/repo")
	t.Setenv("GITHUB_REPOSITORY_INSTALLATIONS", `{"cldmnky":789}`)
	cfg, err = config.FromEnv()
	if len(cfg.GitHub.Repositories) != 0 {
		t.Error("legacy environment list still grants repositories")
	}
	if err != nil || cfg.GitHub.RepositoryInstallations["cldmnky"] != 789 || cfg.GitHub.Organization != "example" || cfg.GitHub.InstallationID != 456 {
		t.Fatalf("separate repository installation: %+v %v", cfg, err)
	}
	for _, raw := range []string{`null`, `[]`, `{"cldmnky":"789"}`, `{"cldmnky":1.5}`, `{"cldmnky":9223372036854775808}`, `not-json`} {
		t.Setenv("GITHUB_REPOSITORY_INSTALLATIONS", raw)
		if _, err := config.FromEnv(); err == nil || !strings.Contains(err.Error(), "GITHUB_REPOSITORY_INSTALLATIONS") {
			t.Errorf("invalid repository installation JSON accepted: %s, %v", raw, err)
		}
	}
	t.Setenv("GITHUB_REPOSITORY_INSTALLATIONS", `{"cldmnky":789}`)
	t.Setenv("POD_NAMESPACE", "workspace")
	if _, err = config.FromEnv(); err == nil {
		t.Error("GitHub private key allowed in admin-enabled workspace namespace")
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
		{"terminal url http", func(env map[string]string) { env["TERMINAL_URL"] = "http://terminal.example.com" }, "TERMINAL_URL"},
		{"terminal url query", func(env map[string]string) { env["TERMINAL_URL"] = "https://terminal.example.com/?debug=1" }, "TERMINAL_URL"},
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
