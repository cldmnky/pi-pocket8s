package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"

	"github.com/creack/pty"
	"nhooyr.io/websocket"
)

//go:embed web/static web/index.html web/terminal.js web/terminal.css
var webFiles embed.FS

const (
	sessionCookie = "__Host-pi-terminal"
	tokenQuery    = "token"
)

// server is the terminal HTTP server. Sessions live in memory only; a restart
// signs every browser out and drops every shell.
type server struct {
	cfg    Config
	log    *slog.Logger
	static fs.FS

	mu       sync.Mutex
	sessions map[string]time.Time
	sem      chan struct{}
}

func newServer(cfg Config, logger *slog.Logger) *server {
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	static, err := fs.Sub(webFiles, "web")
	if err != nil {
		panic(fmt.Sprintf("embedded terminal assets: %v", err))
	}
	return &server{cfg: cfg, log: logger, static: static, sessions: map[string]time.Time{}, sem: make(chan struct{}, cfg.MaxSessions)}
}

func (s *server) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.handleHealthz)
	mux.HandleFunc("GET /", s.handleIndex)
	mux.HandleFunc("GET /ws", s.handleWS)
	mux.HandleFunc("GET /ghostty-web.js", s.handleStatic)
	mux.HandleFunc("GET /ghostty-vt.wasm", s.handleStatic)
	mux.HandleFunc("GET /terminal.js", s.handleStatic)
	mux.HandleFunc("GET /terminal.css", s.handleStatic)
	return s.withSecurityHeaders(mux)
}

// ownerToken re-reads the owner token on every check so a rotation takes
// effect without restarting the daemon.
func (s *server) ownerToken() (string, error) {
	raw, err := os.ReadFile(filepath.Join(s.cfg.PocketDir, "config.json"))
	if err != nil {
		return "", err
	}
	var config struct {
		OwnerToken string `json:"ownerToken"`
	}
	if err := json.Unmarshal(raw, &config); err != nil || config.OwnerToken == "" {
		return "", errors.New("pi-pocket config has no owner token")
	}
	return config.OwnerToken, nil
}

// verifyToken compares hashes in constant time so neither length nor content
// leaks through timing.
func (s *server) verifyToken(presented string) bool {
	if presented == "" {
		return false
	}
	stored, err := s.ownerToken()
	if err != nil {
		return false
	}
	a := sha256.Sum256([]byte(presented))
	b := sha256.Sum256([]byte(stored))
	return subtle.ConstantTimeCompare(a[:], b[:]) == 1
}

// issueSession mints a random session id with a bounded store.
func (s *server) issueSession() (string, bool) {
	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", false
	}
	id := hex.EncodeToString(raw[:])
	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	for k, exp := range s.sessions {
		if !now.Before(exp) {
			delete(s.sessions, k)
		}
	}
	if len(s.sessions) >= s.cfg.MaxSessions*4 {
		return "", false
	}
	s.sessions[id] = now.Add(time.Duration(s.cfg.SessionTTLHours) * time.Hour)
	return id, true
}

func (s *server) validSession(r *http.Request) bool {
	cookie, err := r.Cookie(sessionCookie)
	if err != nil || cookie.Value == "" {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	exp, ok := s.sessions[cookie.Value]
	if !ok || !time.Now().Before(exp) {
		delete(s.sessions, cookie.Value)
		return false
	}
	return true
}

func (s *server) setSessionCookie(w http.ResponseWriter, id string) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    id,
		Path:     "/",
		MaxAge:   s.cfg.SessionTTLHours * 3600,
		HttpOnly: true,
		Secure:   true,
		SameSite: http.SameSiteLaxMode,
	})
}

// handleIndex serves the terminal page. A valid ?token= sets the session
// cookie and redirects to the clean URL (same pattern as upstream /login);
// otherwise a valid session cookie is required.
func (s *server) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	if token := r.URL.Query().Get(tokenQuery); token != "" {
		if !s.verifyToken(token) {
			http.Error(w, "terminal authentication required", http.StatusUnauthorized)
			return
		}
		id, ok := s.issueSession()
		if !ok {
			http.Error(w, "terminal unavailable", http.StatusServiceUnavailable)
			return
		}
		s.setSessionCookie(w, id)
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	if !s.validSession(r) {
		http.Error(w, "terminal authentication required", http.StatusUnauthorized)
		return
	}
	s.serveFile(w, r, "index.html", "text/html; charset=utf-8")
}

func (s *server) handleStatic(w http.ResponseWriter, r *http.Request) {
	name, ok := staticFiles[r.URL.Path]
	if !ok {
		http.NotFound(w, r)
		return
	}
	s.serveFile(w, r, name, contentType(name))
}

var staticFiles = map[string]string{
	"/ghostty-web.js":  "static/ghostty-web.js",
	"/ghostty-vt.wasm": "static/ghostty-vt.wasm",
	"/terminal.js":     "terminal.js",
	"/terminal.css":    "terminal.css",
}

func contentType(name string) string {
	switch filepath.Ext(name) {
	case ".js":
		return "text/javascript; charset=utf-8"
	case ".wasm":
		return "application/wasm"
	case ".css":
		return "text/css; charset=utf-8"
	default:
		return "application/octet-stream"
	}
}

func (s *server) serveFile(w http.ResponseWriter, r *http.Request, name, contentType string) {
	data, err := fs.ReadFile(s.static, name)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", contentType)
	http.ServeContent(w, r, name, time.Time{}, bytes.NewReader(data))
}

func (s *server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = io.WriteString(w, "ok\n")
}

// framePolicy allows the portal to embed the terminal while denying everyone
// else. Empty FrameAncestors keeps the default deny.
func (s *server) framePolicy() string {
	if s.cfg.FrameAncestors == "" {
		return "frame-ancestors 'none'"
	}
	return "frame-ancestors 'self' " + s.cfg.FrameAncestors
}

func (s *server) withSecurityHeaders(next http.Handler) http.Handler {
	policy := "default-src 'none'; script-src 'self' 'wasm-unsafe-eval'; " +
		"style-src 'self'; connect-src 'self'; img-src 'none'; font-src 'none'; " +
		"base-uri 'none'; form-action 'none'; " + s.framePolicy()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", policy)
		h.Set("Cache-Control", "no-store")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		next.ServeHTTP(w, r)
	})
}

// resizeMessage mirrors the ghostty-web demo framing: raw text is PTY input,
// JSON carries control messages the page never forwards as input.
type controlMessage struct {
	Type string `json:"type"`
	Cols int    `json:"cols"`
	Rows int    `json:"rows"`
}

func clampSize(n, min, max int) int {
	if n < min {
		return min
	}
	if n > max {
		return max
	}
	return n
}

// handleWS upgrades an authenticated caller and bridges the socket to one PTY
// shell. Authentication is the session cookie or a ?token= on the upgrade.
func (s *server) handleWS(w http.ResponseWriter, r *http.Request) {
	if !s.validSession(r) && !s.verifyToken(r.URL.Query().Get(tokenQuery)) {
		http.Error(w, "terminal authentication required", http.StatusUnauthorized)
		return
	}
	select {
	case s.sem <- struct{}{}:
		defer func() { <-s.sem }()
	default:
		http.Error(w, "too many terminal sessions", http.StatusServiceUnavailable)
		return
	}
	conn, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	defer conn.Close(websocket.StatusInternalError, "terminal error")

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	shell := exec.CommandContext(ctx, s.cfg.Shell, s.cfg.ShellArgs...)
	shell.Dir = s.cfg.WorkDir
	if info, err := os.Stat(shell.Dir); err != nil || !info.IsDir() {
		shell.Dir, _ = os.UserHomeDir()
	}
	shell.Env = append(os.Environ(), "TERM=xterm-256color", "COLORTERM=truecolor")
	ptmx, err := pty.StartWithSize(shell, &pty.Winsize{Rows: 24, Cols: 80})
	if err != nil {
		s.log.Error("spawn shell", "error", err)
		return
	}
	defer ptmx.Close()
	s.log.Info("terminal session started", "remote", r.RemoteAddr)

	conn.SetReadLimit(maxMessageBytes)
	errCh := make(chan error, 2)

	// PTY -> WebSocket.
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := ptmx.Read(buf)
			if n > 0 {
				if err := conn.Write(ctx, websocket.MessageBinary, buf[:n]); err != nil {
					errCh <- err
					return
				}
			}
			if err != nil {
				errCh <- err
				return
			}
		}
	}()

	// WebSocket -> PTY.
	go func() {
		for {
			typ, data, err := conn.Read(ctx)
			if err != nil {
				errCh <- err
				return
			}
			if typ != websocket.MessageText && typ != websocket.MessageBinary {
				continue
			}
			if typ == websocket.MessageText {
				var control controlMessage
				if json.Unmarshal(data, &control) == nil {
					switch control.Type {
					case "resize":
						_ = pty.Setsize(ptmx, &pty.Winsize{
							Rows: uint16(clampSize(control.Rows, minRows, maxRows)),
							Cols: uint16(clampSize(control.Cols, minCols, maxCols)),
						})
						continue
					case "ping":
						continue
					}
				}
			}
			if _, err := ptmx.Write(data); err != nil {
				errCh <- err
				return
			}
		}
	}()

	<-errCh
	_ = shell.Wait()
	code := 0
	if shell.ProcessState != nil {
		code = shell.ProcessState.ExitCode()
	}
	s.log.Info("terminal session ended", "remote", r.RemoteAddr, "code", code)
	bye, _ := json.Marshal(map[string]any{"type": "exit", "code": code})
	_ = conn.Write(context.Background(), websocket.MessageText, bye)
	conn.Close(websocket.StatusNormalClosure, "")
}
