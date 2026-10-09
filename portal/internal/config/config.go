// Package config loads and validates the portal's process configuration.
package config

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"

	"github.com/cldmnky/pi-pocket8s/portal/internal/githubapp"
)

const (
	// DefaultTokenFile is where the Helm chart mounts the portal token secret.
	DefaultTokenFile = "/run/portal/token"
	// DefaultListenAddr is the container port exposed by the portal.
	DefaultListenAddr = ":8080"
)

// Config is the validated portal configuration.
type Config struct {
	Namespace               string
	PortalNamespace         string
	Deployment              string
	ConfigSecret            string
	PocketURL               string
	TerminalURL             string
	PortalOrigin            string
	TokenFile               string
	AuthMode                string
	WorkspaceServiceAccount string
	GitHub                  githubapp.Options
}

var dnsLabelRE = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

// FromEnv reads and validates the environment contract of the portal.
func FromEnv() (Config, error) {
	cfg := Config{
		Namespace:    strings.TrimSpace(os.Getenv("POD_NAMESPACE")),
		Deployment:   strings.TrimSpace(os.Getenv("POCKET_DEPLOYMENT")),
		ConfigSecret: strings.TrimSpace(os.Getenv("CONFIG_SECRET")),
		PocketURL:    strings.TrimSpace(os.Getenv("POCKET_URL")),
		TerminalURL:  strings.TrimSpace(os.Getenv("TERMINAL_URL")),
		PortalOrigin: strings.TrimSpace(os.Getenv("PORTAL_ORIGIN")),
		TokenFile:    strings.TrimSpace(os.Getenv("PORTAL_TOKEN_FILE")),
	}
	cfg.PortalNamespace = cfg.Namespace
	if namespace := strings.TrimSpace(os.Getenv("POCKET_NAMESPACE")); namespace != "" {
		cfg.Namespace = namespace
	}
	cfg.AuthMode = strings.TrimSpace(os.Getenv("PORTAL_AUTH_MODE"))
	if cfg.AuthMode == "" || cfg.AuthMode == "auto" {
		cfg.AuthMode = "token"
		if os.Getenv("GITHUB_CLIENT_ID") != "" {
			cfg.AuthMode = "github"
		}
	}
	cfg.WorkspaceServiceAccount = strings.TrimSpace(os.Getenv("POCKET_SERVICE_ACCOUNT"))
	if cfg.AuthMode == "github" {
		appID, err := strconv.ParseInt(os.Getenv("GITHUB_APP_ID"), 10, 64)
		if err != nil {
			return Config{}, fmt.Errorf("invalid GITHUB_APP_ID")
		}
		installationID, err := strconv.ParseInt(os.Getenv("GITHUB_INSTALLATION_ID"), 10, 64)
		if err != nil {
			return Config{}, fmt.Errorf("invalid GITHUB_INSTALLATION_ID")
		}
		clientSecretFile := os.Getenv("GITHUB_CLIENT_SECRET_FILE")
		if clientSecretFile == "" {
			clientSecretFile = "/run/github-app/client-secret"
		}
		privateKeyFile := os.Getenv("GITHUB_APP_PRIVATE_KEY_FILE")
		if privateKeyFile == "" {
			privateKeyFile = "/run/github-app/private-key.pem"
		}
		secret := []byte(os.Getenv("GITHUB_CLIENT_SECRET"))
		if len(secret) == 0 {
			var err error
			secret, err = os.ReadFile(clientSecretFile)
			if err != nil {
				return Config{}, fmt.Errorf("cannot read GitHub client secret")
			}
		}
		cfg.GitHub = githubapp.Options{AppID: appID, InstallationID: installationID, ClientID: os.Getenv("GITHUB_CLIENT_ID"), ClientSecret: strings.TrimSpace(string(secret)), PrivateKeyFile: privateKeyFile, Organization: os.Getenv("GITHUB_ORGANIZATION"), Team: os.Getenv("GITHUB_TEAM"), Repositories: strings.FieldsFunc(os.Getenv("GITHUB_REPOSITORIES"), func(r rune) bool { return r == ',' })}
		if raw := os.Getenv("GITHUB_REPOSITORY_INSTALLATIONS"); raw != "" {
			if err := json.Unmarshal([]byte(raw), &cfg.GitHub.RepositoryInstallations); err != nil || cfg.GitHub.RepositoryInstallations == nil {
				return Config{}, fmt.Errorf("GITHUB_REPOSITORY_INSTALLATIONS must be a JSON object of account names to numeric installation IDs")
			}
		}
	}
	if cfg.TokenFile == "" {
		cfg.TokenFile = DefaultTokenFile
	}
	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// Validate reports whether the configuration is safe to serve with.
func (c *Config) Validate() error {
	if c.AuthMode != "" && c.AuthMode != "token" && c.AuthMode != "github" {
		return fmt.Errorf("PORTAL_AUTH_MODE must be token or github")
	}
	if c.AuthMode == "github" && (!isDNSSubdomain(c.WorkspaceServiceAccount) || c.GitHub.AppID <= 0 || c.GitHub.InstallationID <= 0 || c.GitHub.Organization == "" || len(c.GitHub.Repositories) == 0) {
		return fmt.Errorf("GitHub authentication requires app, installation, organization, repositories and workspace service account")
	}
	if c.AuthMode == "github" && (!isDNSLabel(c.PortalNamespace) || c.PortalNamespace == c.Namespace) {
		return fmt.Errorf("GitHub portal must run outside the workspace namespace")
	}
	if !isDNSLabel(c.Namespace) {
		return fmt.Errorf("POD_NAMESPACE %q is not a valid Kubernetes namespace", c.Namespace)
	}
	if !isDNSSubdomain(c.Deployment) {
		return fmt.Errorf("POCKET_DEPLOYMENT %q is not a valid Kubernetes object name", c.Deployment)
	}
	if !isDNSSubdomain(c.ConfigSecret) {
		return fmt.Errorf("CONFIG_SECRET %q is not a valid Kubernetes object name", c.ConfigSecret)
	}
	if c.TokenFile == "" {
		return fmt.Errorf("PORTAL_TOKEN_FILE must not be empty")
	}
	if _, err := parseHTTPSURL(c.PocketURL, "POCKET_URL", true); err != nil {
		return err
	}
	// The terminal is optional: older installs run a portal without it.
	if c.TerminalURL != "" {
		if _, err := parseHTTPSURL(c.TerminalURL, "TERMINAL_URL", true); err != nil {
			return err
		}
	}
	// Store the canonical origin so later comparisons are exact.
	origin, err := parseHTTPSURL(c.PortalOrigin, "PORTAL_ORIGIN", false)
	if err != nil {
		return err
	}
	c.PortalOrigin = canonicalOrigin(origin)
	return nil
}

// NormalizeOrigin canonicalizes an https origin for exact comparison with
// browser Origin headers: the host is lower-cased and a default :443 port is
// removed.
func NormalizeOrigin(raw string) (string, error) {
	u, err := parseHTTPSURL(raw, "origin", false)
	if err != nil {
		return "", err
	}
	return canonicalOrigin(u), nil
}

func canonicalOrigin(u *url.URL) string {
	return "https://" + strings.TrimSuffix(strings.ToLower(u.Host), ":443")
}

// parseHTTPSURL validates an absolute https URL without user information,
// query, or fragment. When allowPath is false the URL must be a bare origin
// such as https://portal.example.com; POCKET_URL may carry a path.
func parseHTTPSURL(raw, env string, allowPath bool) (*url.URL, error) {
	if raw == "" {
		return nil, fmt.Errorf("%s must be set", env)
	}
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("%s is not a valid URL", env)
	}
	if !strings.EqualFold(u.Scheme, "https") {
		return nil, fmt.Errorf("%s must be an https URL", env)
	}
	if u.Host == "" || u.User != nil {
		return nil, fmt.Errorf("%s must be an absolute https URL without user information", env)
	}
	if u.RawQuery != "" || u.Fragment != "" {
		return nil, fmt.Errorf("%s must not contain a query or fragment", env)
	}
	if !allowPath && u.Path != "" && u.Path != "/" {
		return nil, fmt.Errorf("%s must be an origin such as https://portal.example.com", env)
	}
	return u, nil
}

func isDNSLabel(s string) bool {
	return len(s) > 0 && len(s) <= 63 && dnsLabelRE.MatchString(s)
}

func isDNSSubdomain(s string) bool {
	if len(s) == 0 || len(s) > 253 {
		return false
	}
	for _, part := range strings.Split(s, ".") {
		if !isDNSLabel(part) {
			return false
		}
	}
	return true
}
