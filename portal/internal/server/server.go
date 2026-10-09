// Package server implements the portal's HTTP API and its embedded SPA.
package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"time"

	"github.com/cldmnky/pi-pocket8s/portal/internal/auth"
	"github.com/cldmnky/pi-pocket8s/portal/internal/config"
	"github.com/cldmnky/pi-pocket8s/portal/internal/githubauth"
	"github.com/cldmnky/pi-pocket8s/portal/internal/kube"
)

// requestTimeout bounds how long a single API request may take.
const requestTimeout = 20 * time.Second

// Server serves the portal API and the static SPA.
type Server struct {
	cfg      config.Config
	kube     *kube.Client
	log      *slog.Logger
	static   fs.FS
	github   GitHubProvider
	sessions *githubauth.Auth
}

// New builds a portal server. static must expose index.html, app.js, and
// app.css at its root.
func New(cfg config.Config, kubeClient *kube.Client, logger *slog.Logger, static fs.FS) *Server {
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	return &Server{cfg: cfg, kube: kubeClient, log: logger, static: static}
}

// Handler returns the complete HTTP handler of the portal. Routes are fixed;
// there is no generic Kubernetes resource path.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.handleHealthz)
	mux.HandleFunc("GET /auth/session", s.handleSession)
	mux.Handle("GET /api/github/status", s.api(s.handleGitHubStatus))
	mux.Handle("GET /api/github/repositories", s.api(s.handleRepositoryCatalog))
	mux.Handle("POST /api/github/repositories", s.api(s.handleSaveRepositories))
	if s.sessions != nil {
		mux.HandleFunc("GET /auth/github/start", s.sessions.Start)
		mux.HandleFunc("GET /auth/github/callback", s.sessions.Callback)
		mux.HandleFunc("POST /auth/logout", s.sessions.Logout)
		mux.HandleFunc("POST /api/github/credentials", s.handleGitHubCredentials)
	}
	mux.Handle("GET /api/config", s.api(s.handleGetConfig))
	mux.Handle("POST /api/config", s.api(s.handlePostConfig))
	mux.Handle("GET /api/status", s.api(s.handleStatus))
	mux.Handle("POST /api/deployment/start", s.api(s.handleStart))
	mux.Handle("POST /api/deployment/stop", s.api(s.handleStop))
	mux.Handle("POST /api/deployment/restart", s.api(s.handleRestart))
	mux.HandleFunc("GET /", s.handleStatic)
	return s.withSecurityHeaders(s.withRequestLogging(s.withRecovery(mux)))
}

// handleHealthz is the unauthenticated liveness endpoint.
func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, "ok\n")
}

// api guards an API handler with origin and bearer-token checks and applies
// the per-request timeout.
func (s *Server) api(next http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && (!s.originAllowed(r.Header.Get("Origin")) || (s.sessions != nil && r.Header.Get("Origin") == "")) {
			writeError(w, http.StatusForbidden, "request origin is not allowed")
			return
		}
		if s.cfg.AuthMode == "github" {
			if s.sessions == nil {
				writeError(w, http.StatusServiceUnavailable, "GitHub authentication unavailable")
				return
			}
			ctx, cancel := context.WithTimeout(r.Context(), requestTimeout)
			defer cancel()
			if _, err := s.sessions.Authorize(r.WithContext(ctx)); err != nil {
				writeError(w, http.StatusUnauthorized, "GitHub session denied or unavailable")
				return
			}
			next(w, r.WithContext(ctx))
			return
		}
		switch err := auth.Check(r, s.cfg.TokenFile); {
		case errors.Is(err, auth.ErrTokenUnavailable):
			s.log.Error("portal token unavailable", "error", err)
			writeError(w, http.StatusServiceUnavailable, "portal token is unavailable")
			return
		case err != nil:
			w.Header().Set("WWW-Authenticate", `Bearer realm="pi-pocket-portal"`)
			writeError(w, http.StatusUnauthorized, "missing or invalid bearer token")
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), requestTimeout)
		defer cancel()
		next(w, r.WithContext(ctx))
	})
}

// originAllowed accepts browser mutations that match PORTAL_ORIGIN. Requests
// without an Origin header (curl, tests) are allowed because the portal never
// authenticates with cookies.
func (s *Server) originAllowed(origin string) bool {
	if origin == "" {
		return true
	}
	normalized, err := config.NormalizeOrigin(origin)
	if err != nil {
		return false
	}
	return normalized == s.cfg.PortalOrigin
}

var staticFiles = map[string]string{
	"/":             "index.html",
	"/index.html":   "index.html",
	"/app.js":       "app.js",
	"/app.css":      "app.css",
	"/qrcodegen.js": "qrcodegen.js",
}

// handleStatic serves only the embedded SPA files.
func (s *Server) handleStatic(w http.ResponseWriter, r *http.Request) {
	name, ok := staticFiles[r.URL.Path]
	if !ok {
		http.NotFound(w, r)
		return
	}
	data, err := fs.ReadFile(s.static, name)
	if err != nil {
		s.log.Error("read static asset", "asset", name, "error", err)
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", contentTypeFor(name))
	http.ServeContent(w, r, name, time.Time{}, bytes.NewReader(data))
}

func contentTypeFor(name string) string {
	switch filepath.Ext(name) {
	case ".html":
		return "text/html; charset=utf-8"
	case ".js":
		return "text/javascript; charset=utf-8"
	case ".css":
		return "text/css; charset=utf-8"
	default:
		return "application/octet-stream"
	}
}

const contentSecurityPolicy = "default-src 'none'; script-src 'self'; style-src 'self'; " +
	"img-src 'self'; connect-src 'self'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'"

// frameSources is the frame-src allowlist for the embedded agent iframe. It is
// derived from POCKET_URL at startup; empty when no agent URL is configured.
func frameSources(pocketURL string) string {
	u, err := url.Parse(pocketURL)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return ""
	}
	return u.Scheme + "://" + u.Host
}

// withSecurityHeaders applies the portal's response hardening headers.
func (s *Server) withSecurityHeaders(next http.Handler) http.Handler {
	policy := contentSecurityPolicy
	if src := frameSources(s.cfg.PocketURL); src != "" {
		policy += "; frame-src " + src
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", policy)
		h.Set("Cache-Control", "no-store")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		next.ServeHTTP(w, r)
	})
}

// withRecovery converts handler panics into a sanitized 500 response.
func (s *Server) withRecovery(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if recovered := recover(); recovered != nil {
				s.log.Error("panic serving request",
					"method", r.Method,
					"path", r.URL.Path,
					"panic", fmt.Sprint(recovered),
				)
				writeError(w, http.StatusInternalServerError, "internal server error")
			}
		}()
		next.ServeHTTP(w, r)
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	if r.status == 0 {
		r.status = code
		r.ResponseWriter.WriteHeader(code)
	}
}

func (r *statusRecorder) Write(data []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	return r.ResponseWriter.Write(data)
}

// withRequestLogging logs method, path, status, and duration. Headers, bodies,
// and tokens are never logged.
func (s *Server) withRequestLogging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		recorder := &statusRecorder{ResponseWriter: w}
		next.ServeHTTP(recorder, r)
		if recorder.status == 0 {
			recorder.status = http.StatusOK
		}
		s.log.Info("request",
			"method", r.Method,
			"path", r.URL.Path,
			"status", recorder.status,
			"duration_ms", time.Since(start).Milliseconds(),
		)
	})
}

type errorResponse struct {
	Error string `json:"error"`
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, errorResponse{Error: sanitizeMessage(message)})
}

// sanitizeMessage strips control characters and caps the length so error
// responses stay safe and small.
func sanitizeMessage(message string) string {
	var b strings.Builder
	count := 0
	for _, r := range message {
		if count >= 512 {
			break
		}
		if r == '\n' || r == '\t' {
			r = ' '
		}
		if r < 0x20 || r == 0x7f {
			continue
		}
		b.WriteRune(r)
		count++
	}
	return strings.TrimSpace(b.String())
}
