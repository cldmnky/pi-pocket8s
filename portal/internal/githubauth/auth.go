// Package githubauth keeps OAuth transactions and opaque portal sessions in
// memory. Restarting the single portal replica invalidates sessions; no GitHub
// user token or refresh token is persisted or sent to the coding agent.
package githubauth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/cldmnky/pi-pocket8s/portal/internal/githubapp"
)

const SessionCookie = "__Host-pocket-session"
const oauthCookie = "__Host-pocket-oauth"
const maxEntries = 4096

var ErrUnauthorized = errors.New("GitHub session required")

type Provider interface {
	AuthorizationURL(callback, state, verifier string) string
	Login(context.Context, string, string, string) (githubapp.User, error)
	CheckMember(context.Context, string) error
}

type pending struct {
	state, verifier string
	expires         time.Time
}
type session struct {
	user             githubapp.User
	expires, checked time.Time
}
type Auth struct {
	provider Provider
	origin   string
	mu       sync.Mutex
	pending  map[[32]byte]pending
	sessions map[[32]byte]session
	now      func() time.Time
	log      *slog.Logger
}

func New(provider Provider, origin string, loggers ...*slog.Logger) *Auth {
	logger := slog.New(slog.DiscardHandler)
	if len(loggers) > 0 && loggers[0] != nil {
		logger = loggers[0]
	}
	return &Auth{provider: provider, origin: origin, pending: map[[32]byte]pending{}, sessions: map[[32]byte]session{}, now: time.Now, log: logger}
}

// Log fixed stages/reasons only. Provider errors and request query/cookies may
// contain credentials and must never be attached to OAuth diagnostic events.
func (a *Auth) failure(stage, reason string) {
	a.log.Warn("github oauth", "stage", stage, "outcome", "failed", "reason", reason)
}
func randomValue() string {
	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		panic("secure random unavailable")
	}
	return base64.RawURLEncoding.EncodeToString(raw[:])
}
func key(raw string) [32]byte { return sha256.Sum256([]byte(raw)) }
func setCookie(w http.ResponseWriter, name, value string, age int) {
	http.SetCookie(w, &http.Cookie{Name: name, Value: value, Path: "/", MaxAge: age, HttpOnly: true, Secure: true, SameSite: http.SameSiteLaxMode})
}
func (a *Auth) prune() {
	now := a.now()
	for k, p := range a.pending {
		if !now.Before(p.expires) {
			delete(a.pending, k)
		}
	}
	for k, s := range a.sessions {
		if !now.Before(s.expires) {
			delete(a.sessions, k)
		}
	}
}

func (a *Auth) Start(w http.ResponseWriter, r *http.Request) {
	state, verifier, transaction := randomValue(), randomValue(), randomValue()
	a.mu.Lock()
	a.prune()
	if previous, err := r.Cookie(oauthCookie); err == nil {
		delete(a.pending, key(previous.Value))
	}
	if len(a.pending) >= maxEntries {
		a.mu.Unlock()
		a.failure("start", "capacity")
		http.Error(w, "Too many login attempts", http.StatusTooManyRequests)
		return
	}
	a.pending[key(transaction)] = pending{state, verifier, a.now().Add(10 * time.Minute)}
	a.mu.Unlock()
	a.log.Info("github oauth", "stage", "start", "outcome", "redirect")
	setCookie(w, oauthCookie, transaction, 600)
	http.Redirect(w, r, a.provider.AuthorizationURL(a.origin+"/auth/github/callback", state, verifier), http.StatusFound)
}

func (a *Auth) Callback(w http.ResponseWriter, r *http.Request) {
	cookie, err := r.Cookie(oauthCookie)
	if err != nil {
		a.failure("transaction", "missing_cookie")
		http.Error(w, "Invalid login transaction", http.StatusBadRequest)
		return
	}
	a.mu.Lock()
	p, ok := a.pending[key(cookie.Value)]
	delete(a.pending, key(cookie.Value))
	a.mu.Unlock()
	setCookie(w, oauthCookie, "", -1)
	if !ok || !a.now().Before(p.expires) || len(r.URL.Query()["state"]) != 1 || len(r.URL.Query()["code"]) != 1 || r.URL.Query().Get("state") != p.state || r.URL.Query().Get("code") == "" || len(r.URL.Query().Get("code")) > 4096 {
		a.failure("transaction", "invalid_or_expired")
		http.Error(w, "Invalid or expired login transaction", http.StatusBadRequest)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	user, err := a.provider.Login(ctx, r.URL.Query().Get("code"), a.origin+"/auth/github/callback", p.verifier)
	if err != nil {
		reason := "provider_unavailable"
		if errors.Is(err, githubapp.ErrDenied) {
			reason = "access_denied"
		}
		a.failure("identity_and_membership", reason)
		http.Error(w, "GitHub login denied or unavailable", http.StatusForbidden)
		return
	}
	token := randomValue()
	a.mu.Lock()
	a.prune()
	if len(a.sessions) >= maxEntries {
		a.mu.Unlock()
		a.failure("session", "capacity")
		http.Error(w, "Too many sessions", http.StatusTooManyRequests)
		return
	}
	if old, err := r.Cookie(SessionCookie); err == nil {
		delete(a.sessions, key(old.Value))
	}
	a.sessions[key(token)] = session{user, a.now().Add(8 * time.Hour), a.now()}
	a.mu.Unlock()
	a.log.Info("github oauth", "stage", "session", "outcome", "authenticated")
	setCookie(w, SessionCookie, token, 8*60*60)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// Authorize rechecks membership at least every five minutes, and fails closed
// if GitHub is unavailable at a required recheck (the cached check is not extended).
func (a *Auth) Authorize(r *http.Request) (githubapp.User, error) {
	cookie, err := r.Cookie(SessionCookie)
	if err != nil || len(cookie.Value) != 43 {
		return githubapp.User{}, ErrUnauthorized
	}
	k := key(cookie.Value)
	a.mu.Lock()
	s, ok := a.sessions[k]
	a.mu.Unlock()
	if !ok || !a.now().Before(s.expires) {
		a.mu.Lock()
		delete(a.sessions, k)
		a.mu.Unlock()
		return githubapp.User{}, ErrUnauthorized
	}
	if a.now().Sub(s.checked) >= 5*time.Minute {
		if err = a.provider.CheckMember(r.Context(), s.user.Login); err != nil {
			return githubapp.User{}, err
		}
		a.mu.Lock()
		if current, exists := a.sessions[k]; exists {
			current.checked = a.now()
			a.sessions[k] = current
		} else {
			a.mu.Unlock()
			return githubapp.User{}, ErrUnauthorized
		}
		a.mu.Unlock()
	}
	return s.user, nil
}

func (a *Auth) Logout(w http.ResponseWriter, r *http.Request) {
	// Strict Origin is required: cookie authentication must not allow cookie-free
	// bearer mode's "Origin omitted" exception on mutations.
	if r.Header.Get("Origin") != a.origin {
		http.Error(w, "Invalid origin", http.StatusForbidden)
		return
	}
	if cookie, err := r.Cookie(SessionCookie); err == nil {
		a.mu.Lock()
		delete(a.sessions, key(cookie.Value))
		a.mu.Unlock()
	}
	setCookie(w, SessionCookie, "", -1)
	w.WriteHeader(http.StatusNoContent)
}

func (a *Auth) Session(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	user, err := a.Authorize(r.WithContext(ctx))
	w.Header().Set("Content-Type", "application/json")
	if err != nil {
		_, _ = w.Write([]byte(`{"mode":"github","authenticated":false}`))
		return
	}
	// This response has no credentials of any kind.
	_ = json.NewEncoder(w).Encode(map[string]any{"mode": "github", "authenticated": true, "login": user.Login})
}
