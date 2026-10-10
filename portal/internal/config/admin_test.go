package config_test

import (
	"strings"
	"testing"

	"github.com/cldmnky/pi-pocket8s/portal/internal/config"
)

// githubEnv is a valid GitHub-mode portal configuration; adminEnv adds the
// elevation variables on top of it.
func githubEnv() map[string]string {
	env := validEnv()
	env["POD_NAMESPACE"] = "management"
	env["POCKET_NAMESPACE"] = "workspace"
	env["POCKET_SERVICE_ACCOUNT"] = "pocket"
	env["GITHUB_CLIENT_ID"] = "client"
	env["GITHUB_APP_ID"] = "123"
	env["GITHUB_INSTALLATION_ID"] = "456"
	env["GITHUB_ORGANIZATION"] = "example"
	env["GITHUB_REPOSITORY_POLICY_SECRET"] = "policy"
	env["GITHUB_CLIENT_SECRET"] = "secret"
	return env
}

func adminEnv() map[string]string {
	env := githubEnv()
	env["ADMIN_ELEVATION_ENABLED"] = "true"
	env["ADMIN_NAMESPACE"] = "admin-workspace"
	env["ADMIN_DEPLOYMENT"] = "pi-pocket-admin"
	env["ADMIN_SERVICE_ACCOUNT"] = "pi-pocket-admin"
	env["ADMIN_RUNTIME_SECRET"] = "pi-pocket-admin-runtime"
	env["ADMIN_POCKET_URL"] = "https://pocket-admin.example.com"
	env["ADMIN_TERMINAL_URL"] = "https://pocket-admin-terminal.example.com"
	env["ADMIN_SESSION_SECRET"] = "admin-session"
	env["ADMIN_CLUSTER_ROLE_BINDING"] = "pi-pocket-admin-cluster-admin"
	env["ADMIN_OPERATORS"] = "42,77"
	env["ADMIN_DEFAULT_DURATION_SECONDS"] = "900"
	env["ADMIN_MAX_DURATION_SECONDS"] = "1800"
	env["ADMIN_RECENT_LOGIN_SECONDS"] = "900"
	env["ADMIN_STARTUP_TIMEOUT_SECONDS"] = "180"
	return env
}

func TestElevationDisabledByDefault(t *testing.T) {
	setEnv(t, githubEnv())
	cfg, err := config.FromEnv()
	if err != nil {
		t.Fatalf("FromEnv: %v", err)
	}
	if cfg.Admin.Enabled {
		t.Fatal("elevation enabled without configuration")
	}
	// A default installation must not acquire any elevation behaviour.
	if cfg.Admin.Namespace != "" || len(cfg.Admin.Operators) != 0 {
		t.Fatalf("unexpected elevation defaults: %+v", cfg.Admin)
	}
}

func TestElevationValidConfiguration(t *testing.T) {
	setEnv(t, adminEnv())
	cfg, err := config.FromEnv()
	if err != nil {
		t.Fatalf("FromEnv: %v", err)
	}
	if !cfg.Admin.Enabled || cfg.Admin.Namespace != "admin-workspace" || cfg.Admin.DefaultDuration.String() != "15m0s" {
		t.Fatalf("unexpected elevation config: %+v", cfg.Admin)
	}
	if !cfg.Admin.IsOperator(42) || !cfg.Admin.IsOperator(77) || cfg.Admin.IsOperator(99) {
		t.Fatal("operator allowlist mismatch")
	}
}

func TestElevationRejectsUnsafeConfigurations(t *testing.T) {
	cases := map[string]func(map[string]string){
		"token auth mode":              func(env map[string]string) { env["GITHUB_CLIENT_ID"] = ""; env["PORTAL_AUTH_MODE"] = "token" },
		"admin namespace is workspace": func(env map[string]string) { env["ADMIN_NAMESPACE"] = "workspace" },
		"admin namespace is portal":    func(env map[string]string) { env["ADMIN_NAMESPACE"] = "management" },
		"no operators":                 func(env map[string]string) { env["ADMIN_OPERATORS"] = "" },
		"non-numeric operator":         func(env map[string]string) { env["ADMIN_OPERATORS"] = "alice" },
		"negative operator":            func(env map[string]string) { env["ADMIN_OPERATORS"] = "-4" },
		"http workspace url":           func(env map[string]string) { env["ADMIN_POCKET_URL"] = "http://pocket-admin.example.com" },
		"http terminal url":            func(env map[string]string) { env["ADMIN_TERMINAL_URL"] = "http://pocket-admin-terminal.example.com" },
		"missing session secret":       func(env map[string]string) { env["ADMIN_SESSION_SECRET"] = "" },
		"missing binding name":         func(env map[string]string) { env["ADMIN_CLUSTER_ROLE_BINDING"] = "" },
		"missing deployment":           func(env map[string]string) { env["ADMIN_DEPLOYMENT"] = "" },
		"missing runtime secret":       func(env map[string]string) { env["ADMIN_RUNTIME_SECRET"] = "" },
		"default above maximum":        func(env map[string]string) { env["ADMIN_DEFAULT_DURATION_SECONDS"] = "3600" },
		"maximum above one hour":       func(env map[string]string) { env["ADMIN_MAX_DURATION_SECONDS"] = "7200" },
		"zero recent login":            func(env map[string]string) { env["ADMIN_RECENT_LOGIN_SECONDS"] = "0" },
		"negative startup timeout":     func(env map[string]string) { env["ADMIN_STARTUP_TIMEOUT_SECONDS"] = "-5" },
		"non-numeric duration":         func(env map[string]string) { env["ADMIN_MAX_DURATION_SECONDS"] = "soon" },
		"garbage enable flag":          func(env map[string]string) { env["ADMIN_ELEVATION_ENABLED"] = "maybe" },
	}
	for name, mutate := range cases {
		env := adminEnv()
		mutate(env)
		setEnv(t, env)
		if _, err := config.FromEnv(); err == nil {
			t.Errorf("%s was accepted", name)
		} else if !strings.Contains(err.Error(), "ADMIN") && name != "token auth mode" {
			t.Errorf("%s produced an unrelated error: %v", name, err)
		}
	}
}

func TestControllerConfigRequiresItsOwnNamespaceContract(t *testing.T) {
	env := adminEnv()
	env["POD_NAMESPACE"] = "management"
	env["ADMIN_BOOTSTRAP"] = "true"
	env["ADMIN_RECONCILE_SECONDS"] = "5"
	setEnv(t, env)
	controller, err := config.ControllerFromEnv()
	if err != nil {
		t.Fatalf("ControllerFromEnv: %v", err)
	}
	if !controller.Bootstrap || controller.Reconcile.String() != "5s" || controller.HealthAddr != ":8090" {
		t.Fatalf("unexpected controller config: %+v", controller)
	}
	if controller.PortalNamespace != "management" || controller.Namespace != "admin-workspace" {
		t.Fatalf("unexpected namespaces: %+v", controller)
	}
	// The controller must never run inside the elevated namespace.
	env["POD_NAMESPACE"] = "admin-workspace"
	setEnv(t, env)
	if _, err := config.ControllerFromEnv(); err == nil {
		t.Fatal("controller accepted running in the admin namespace")
	}
	// Elevation must be on: the controller has nothing to do otherwise.
	env["POD_NAMESPACE"] = "management"
	env["ADMIN_ELEVATION_ENABLED"] = "false"
	setEnv(t, env)
	if _, err := config.ControllerFromEnv(); err == nil {
		t.Fatal("controller accepted disabled elevation")
	}
}
