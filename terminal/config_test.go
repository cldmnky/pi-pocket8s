package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadConfigDefaults(t *testing.T) {
	t.Setenv("TERMINAL_PORT", "")
	t.Setenv("TERMINAL_FRAME_ANCESTORS", "")
	t.Setenv("PI_POCKET_DIR", "")
	t.Setenv("TERMINAL_SHELL", "")
	t.Setenv("TERMINAL_CWD", "")
	t.Setenv("TERMINAL_SHELL_ARGS", "")

	cfg, err := loadConfig()
	if err != nil {
		t.Fatalf("loadConfig() error = %v", err)
	}
	if cfg.ListenAddr != ":8081" {
		t.Errorf("ListenAddr = %q, want :8081", cfg.ListenAddr)
	}
	if cfg.PocketDir != "/workspace/home/.pi-pocket" {
		t.Errorf("PocketDir = %q", cfg.PocketDir)
	}
	if cfg.Shell != "bash" || len(cfg.ShellArgs) != 0 {
		t.Errorf("Shell = %q args %q", cfg.Shell, cfg.ShellArgs)
	}
	if cfg.WorkDir != "/workspace/repos" {
		t.Errorf("WorkDir = %q", cfg.WorkDir)
	}
	if cfg.FrameAncestors != "" {
		t.Errorf("FrameAncestors = %q, want empty", cfg.FrameAncestors)
	}
}

func TestLoadConfigTerminalOrigin(t *testing.T) {
	t.Setenv("TERMINAL_FRAME_ANCESTORS", "https://portal.example.com/")
	cfg, err := loadConfig()
	if err != nil {
		t.Fatalf("loadConfig() error = %v", err)
	}
	if cfg.FrameAncestors != "https://portal.example.com" {
		t.Errorf("FrameAncestors = %q", cfg.FrameAncestors)
	}
}

func TestLoadConfigShellFallback(t *testing.T) {
	t.Setenv("TERMINAL_SHELL", "  ")
	cfg, err := loadConfig()
	if err != nil {
		t.Fatalf("loadConfig() error = %v", err)
	}
	if cfg.Shell != "bash" {
		t.Errorf("Shell = %q, want fallback bash", cfg.Shell)
	}
}

func TestLoadConfigRejects(t *testing.T) {
	cases := map[string]map[string]string{
		"bad port":      {"TERMINAL_PORT": "http"},
		"port zero":     {"TERMINAL_PORT": "0"},
		"port huge":     {"TERMINAL_PORT": "99999"},
		"http ancestor": {"TERMINAL_FRAME_ANCESTORS": "http://portal.example.com"},
		"ancestor path": {"TERMINAL_FRAME_ANCESTORS": "https://portal.example.com/portal"},
	}
	for name, env := range cases {
		t.Run(name, func(t *testing.T) {
			for k, v := range env {
				t.Setenv(k, v)
			}
			if _, err := loadConfig(); err == nil {
				t.Errorf("loadConfig() accepted %v", env)
			}
		})
	}
}

func TestClampSize(t *testing.T) {
	if got := clampSize(80, minCols, maxCols); got != 80 {
		t.Errorf("clampSize(80) = %d", got)
	}
	if got := clampSize(0, minRows, maxRows); got != minRows {
		t.Errorf("clampSize(0) = %d", got)
	}
	if got := clampSize(5000, minCols, maxCols); got != maxCols {
		t.Errorf("clampSize(5000) = %d", got)
	}
}

func writePocketConfig(t *testing.T, token string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(`{"version":1,"ownerToken":`+quoteJSON(token)+`}`), 0600); err != nil {
		t.Fatal(err)
	}
	return dir
}

func quoteJSON(s string) string {
	out := make([]byte, 0, len(s)+2)
	out = append(out, '"')
	for i := 0; i < len(s); i++ {
		if s[i] == '"' || s[i] == '\\' {
			out = append(out, '\\')
		}
		out = append(out, s[i])
	}
	return string(append(out, '"'))
}
