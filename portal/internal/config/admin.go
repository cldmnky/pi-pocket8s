// Cluster-admin elevation configuration. Elevation is disabled unless every
// value below is supplied explicitly, and it is only available in GitHub
// authentication mode: in token mode the namespace-admin workspace can read
// the portal token Secret, and that credential must never become an elevation
// credential.
package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// AdminConfig is the validated cluster-admin elevation policy.
type AdminConfig struct {
	Enabled            bool
	Namespace          string
	Deployment         string
	ServiceAccount     string
	RuntimeSecret      string
	PocketURL          string
	TerminalURL        string
	SessionSecret      string
	ClusterRoleBinding string
	ClusterRole        string
	Operators          []int64
	DefaultDuration    time.Duration
	MaxDuration        time.Duration
	RecentLogin        time.Duration
	StartupTimeout     time.Duration
	// AllowTokenAuth permits elevation in token authentication mode. It is
	// false by default and must be set deliberately: with it, the shared portal
	// token becomes an elevation credential, and every holder of that token can
	// activate cluster-admin. GitHub mode remains the recommended configuration.
	AllowTokenAuth bool
}

// IsOperator reports whether the numeric GitHub user ID may administer the
// cluster. Numeric identity is used because logins can be renamed and reused.
func (a AdminConfig) IsOperator(userID int64) bool {
	for _, allowed := range a.Operators {
		if allowed == userID {
			return true
		}
	}
	return false
}

func adminFromEnv() (AdminConfig, error) {
	admin := AdminConfig{
		Namespace:          strings.TrimSpace(os.Getenv("ADMIN_NAMESPACE")),
		Deployment:         strings.TrimSpace(os.Getenv("ADMIN_DEPLOYMENT")),
		ServiceAccount:     strings.TrimSpace(os.Getenv("ADMIN_SERVICE_ACCOUNT")),
		RuntimeSecret:      strings.TrimSpace(os.Getenv("ADMIN_RUNTIME_SECRET")),
		PocketURL:          strings.TrimSpace(os.Getenv("ADMIN_POCKET_URL")),
		TerminalURL:        strings.TrimSpace(os.Getenv("ADMIN_TERMINAL_URL")),
		SessionSecret:      strings.TrimSpace(os.Getenv("ADMIN_SESSION_SECRET")),
		ClusterRoleBinding: strings.TrimSpace(os.Getenv("ADMIN_CLUSTER_ROLE_BINDING")),
		ClusterRole:        strings.TrimSpace(os.Getenv("ADMIN_CLUSTER_ROLE")),
	}
	raw := strings.TrimSpace(os.Getenv("ADMIN_ELEVATION_ENABLED"))
	enabled, err := parseBool("ADMIN_ELEVATION_ENABLED", raw)
	if err != nil {
		return AdminConfig{}, err
	}
	admin.Enabled = enabled
	if !enabled {
		return admin, nil
	}
	allowToken, err := parseBool("ADMIN_ALLOW_TOKEN_AUTH", os.Getenv("ADMIN_ALLOW_TOKEN_AUTH"))
	if err != nil {
		return AdminConfig{}, err
	}
	admin.AllowTokenAuth = allowToken
	if admin.ClusterRole == "" {
		admin.ClusterRole = "cluster-admin"
	}
	operators, err := parseOperators(os.Getenv("ADMIN_OPERATORS"))
	if err != nil {
		return AdminConfig{}, err
	}
	admin.Operators = operators
	seconds := []struct {
		env    string
		target *time.Duration
	}{
		{"ADMIN_DEFAULT_DURATION_SECONDS", &admin.DefaultDuration},
		{"ADMIN_MAX_DURATION_SECONDS", &admin.MaxDuration},
		{"ADMIN_RECENT_LOGIN_SECONDS", &admin.RecentLogin},
		{"ADMIN_STARTUP_TIMEOUT_SECONDS", &admin.StartupTimeout},
	}
	for _, item := range seconds {
		value, err := parsePositiveSeconds(item.env, os.Getenv(item.env))
		if err != nil {
			return AdminConfig{}, err
		}
		*item.target = value
	}
	return admin, nil
}

func parseBool(env, raw string) (bool, error) {
	switch strings.ToLower(raw) {
	case "", "false", "0", "no":
		return false, nil
	case "true", "1", "yes":
		return true, nil
	default:
		return false, fmt.Errorf("%s must be true or false", env)
	}
}

func parsePositiveSeconds(env, raw string) (time.Duration, error) {
	value, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
	if err != nil || value <= 0 {
		return 0, fmt.Errorf("%s must be a positive whole number of seconds", env)
	}
	return time.Duration(value) * time.Second, nil
}

func parseOperators(raw string) ([]int64, error) {
	var operators []int64
	seen := map[int64]bool{}
	for _, field := range strings.FieldsFunc(raw, func(r rune) bool { return r == ',' || r == ' ' }) {
		id, err := strconv.ParseInt(strings.TrimSpace(field), 10, 64)
		if err != nil || id <= 0 {
			return nil, fmt.Errorf("ADMIN_OPERATORS must be a comma-separated list of positive numeric GitHub user IDs")
		}
		if seen[id] {
			continue
		}
		seen[id] = true
		operators = append(operators, id)
	}
	if len(operators) == 0 {
		return nil, fmt.Errorf("ADMIN_OPERATORS must name at least one authorized operator")
	}
	return operators, nil
}

// validate enforces the elevation security contract against the rest of the
// portal configuration. Disabled elevation validates nothing.
func (a AdminConfig) validate(c *Config) error {
	if !a.Enabled {
		return nil
	}
	if c.AuthMode != "github" && !a.AllowTokenAuth {
		return fmt.Errorf("cluster-admin elevation requires GitHub authentication: a namespace-admin workspace can read a token-mode portal credential, and that credential must not become an elevation credential (set adminElevation.allowTokenAuth to accept that tradeoff deliberately)")
	}
	if !isDNSLabel(a.Namespace) || !isDNSSubdomain(a.Deployment) || !isDNSSubdomain(a.ServiceAccount) {
		return fmt.Errorf("ADMIN_NAMESPACE, ADMIN_DEPLOYMENT and ADMIN_SERVICE_ACCOUNT must be valid Kubernetes names")
	}
	if !isDNSSubdomain(a.RuntimeSecret) || !isDNSSubdomain(a.SessionSecret) || !isDNSSubdomain(a.ClusterRoleBinding) {
		return fmt.Errorf("ADMIN_RUNTIME_SECRET, ADMIN_SESSION_SECRET and ADMIN_CLUSTER_ROLE_BINDING must be valid Kubernetes names")
	}
	if !isDNSSubdomain(a.ClusterRole) {
		return fmt.Errorf("ADMIN_CLUSTER_ROLE must be a valid Kubernetes name")
	}
	// The admin workspace must be its own namespace: it can never share the
	// namespace-admin workspace's credentials, storage, or Secrets.
	if a.Namespace == c.Namespace || a.Namespace == c.PortalNamespace {
		return fmt.Errorf("ADMIN_NAMESPACE must differ from both the workspace and portal namespaces")
	}
	if _, err := parseHTTPSURL(a.PocketURL, "ADMIN_POCKET_URL", true); err != nil {
		return err
	}
	if a.TerminalURL != "" {
		if _, err := parseHTTPSURL(a.TerminalURL, "ADMIN_TERMINAL_URL", true); err != nil {
			return err
		}
	}
	if a.DefaultDuration <= 0 || a.MaxDuration <= 0 || a.DefaultDuration > a.MaxDuration {
		return fmt.Errorf("ADMIN_DEFAULT_DURATION_SECONDS must be positive and no greater than ADMIN_MAX_DURATION_SECONDS")
	}
	if a.MaxDuration > time.Hour {
		return fmt.Errorf("ADMIN_MAX_DURATION_SECONDS must not exceed 3600")
	}
	if a.RecentLogin <= 0 || a.StartupTimeout <= 0 {
		return fmt.Errorf("ADMIN_RECENT_LOGIN_SECONDS and ADMIN_STARTUP_TIMEOUT_SECONDS must be positive")
	}
	if len(a.Operators) == 0 {
		return fmt.Errorf("ADMIN_OPERATORS must name at least one authorized operator")
	}
	return nil
}

// AdminControllerConfig is the controller's own process configuration. It is
// the same elevation policy plus reconciliation and health settings, so one
// Deployment template can serve both processes.
type AdminControllerConfig struct {
	AdminConfig
	PortalNamespace string
	Bootstrap       bool
	Reconcile       time.Duration
	HealthAddr      string
}

// ControllerFromEnv reads the controller's environment contract.
func ControllerFromEnv() (AdminControllerConfig, error) {
	admin, err := adminFromEnv()
	if err != nil {
		return AdminControllerConfig{}, err
	}
	if !admin.Enabled {
		return AdminControllerConfig{}, fmt.Errorf("ADMIN_ELEVATION_ENABLED must be true for the admin controller")
	}
	controller := AdminControllerConfig{AdminConfig: admin, PortalNamespace: strings.TrimSpace(os.Getenv("POD_NAMESPACE"))}
	if !isDNSLabel(controller.PortalNamespace) {
		return AdminControllerConfig{}, fmt.Errorf("POD_NAMESPACE must name the management namespace")
	}
	if controller.PortalNamespace == admin.Namespace {
		return AdminControllerConfig{}, fmt.Errorf("the controller must not run in the admin workspace namespace")
	}
	bootstrap, err := parseBool("ADMIN_BOOTSTRAP", os.Getenv("ADMIN_BOOTSTRAP"))
	if err != nil {
		return AdminControllerConfig{}, err
	}
	controller.Bootstrap = bootstrap
	reconcile, err := parsePositiveSeconds("ADMIN_RECONCILE_SECONDS", os.Getenv("ADMIN_RECONCILE_SECONDS"))
	if err != nil {
		return AdminControllerConfig{}, err
	}
	controller.Reconcile = reconcile
	controller.HealthAddr = strings.TrimSpace(os.Getenv("ADMIN_HEALTH_ADDR"))
	if controller.HealthAddr == "" {
		controller.HealthAddr = ":8090"
	}
	return controller, nil
}
