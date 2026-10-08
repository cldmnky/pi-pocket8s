package main

import (
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
)

// Defaults for the terminal daemon. The chart sets TERMINAL_PORT,
// TERMINAL_FRAME_ANCESTORS, and PI_POCKET_DIR on the pocket container.
const (
	defaultPort        = 8081
	defaultPocketDir   = "/workspace/home/.pi-pocket"
	defaultShell       = "bash"
	defaultWorkDir     = "/workspace/repos"
	maxSessions        = 16
	sessionTTLHours    = 12
	minCols, maxCols   = 2, 1000
	minRows, maxRows   = 2, 1000
	maxMessageBytes    = 1 << 20
	maxRequestBodyByte = 1 << 20
)

// Config is the validated terminal daemon configuration. Everything comes
// from the environment so the chart owns the wiring; see images/README.md.
type Config struct {
	ListenAddr      string
	PocketDir       string
	Shell           string
	ShellArgs       []string
	WorkDir         string
	FrameAncestors  string // extra frame-ancestors origin for portal embedding, "" to disable framing by others
	MaxSessions     int
	SessionTTLHours int
}

func loadConfig() (Config, error) {
	cfg := Config{
		PocketDir:       firstNonEmpty(os.Getenv("PI_POCKET_DIR"), defaultPocketDir),
		Shell:           firstNonEmpty(os.Getenv("TERMINAL_SHELL"), defaultShell),
		WorkDir:         firstNonEmpty(os.Getenv("TERMINAL_CWD"), defaultWorkDir),
		FrameAncestors:  strings.TrimSpace(os.Getenv("TERMINAL_FRAME_ANCESTORS")),
		MaxSessions:     maxSessions,
		SessionTTLHours: sessionTTLHours,
	}
	if args, ok := os.LookupEnv("TERMINAL_SHELL_ARGS"); ok {
		cfg.ShellArgs = strings.Fields(args)
	}
	port := strings.TrimSpace(os.Getenv("TERMINAL_PORT"))
	if port == "" {
		port = strconv.Itoa(defaultPort)
	}
	n, err := strconv.Atoi(port)
	if err != nil || n < 1 || n > 65535 {
		return Config{}, fmt.Errorf("TERMINAL_PORT %q is not a valid port", port)
	}
	cfg.ListenAddr = fmt.Sprintf(":%d", n)
	if cfg.FrameAncestors != "" {
		u, err := url.Parse(cfg.FrameAncestors)
		if err != nil || !strings.EqualFold(u.Scheme, "https") || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
			return Config{}, fmt.Errorf("TERMINAL_FRAME_ANCESTORS %q must be a bare https origin", cfg.FrameAncestors)
		}
		cfg.FrameAncestors = "https://" + strings.ToLower(u.Host)
	}
	if cfg.Shell == "" {
		return Config{}, fmt.Errorf("TERMINAL_SHELL must not be empty")
	}
	if cfg.WorkDir == "" || cfg.PocketDir == "" {
		return Config{}, fmt.Errorf("TERMINAL_CWD and PI_POCKET_DIR must not be empty")
	}
	return cfg, nil
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}
