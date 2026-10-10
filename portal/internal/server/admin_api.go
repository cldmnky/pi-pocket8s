package server

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/cldmnky/pi-pocket8s/portal/internal/admin"
	"github.com/cldmnky/pi-pocket8s/portal/internal/githubapp"
	"github.com/cldmnky/pi-pocket8s/portal/internal/githubauth"
	"github.com/cldmnky/pi-pocket8s/portal/internal/kube"
)

// adminConfirmation is the exact phrase an operator must type; it is a
// deliberate speed bump, not a security control.
const adminConfirmation = "cluster-admin"

// adminObservationStaleAfter bounds how old a controller observation may be
// before the portal treats the lifecycle state as unknown.
const adminObservationStaleAfter = 60 * time.Second

// adminSessionView is the session part of the status response. It carries no
// credentials: no token, no sign-in link.
type adminSessionView struct {
	SessionID        string    `json:"sessionID"`
	OwnerUserID      int64     `json:"ownerUserID"`
	Owner            string    `json:"owner"`
	Reason           string    `json:"reason"`
	ApprovedAt       time.Time `json:"approvedAt"`
	ExpiresAt        time.Time `json:"expiresAt"`
	SecondsRemaining int64     `json:"secondsRemaining"`
}

type adminWorkspaceView struct {
	Ready      bool      `json:"ready"`
	PodUID     string    `json:"podUID"`
	ObservedAt time.Time `json:"observedAt"`
}

type adminGrantView struct {
	Observed bool `json:"observed"`
}

type adminStatusResponse struct {
	Enabled          bool   `json:"enabled"`
	Namespace        string `json:"namespace,omitempty"`
	Deployment       string `json:"deployment,omitempty"`
	PocketURL        string `json:"pocketUrl,omitempty"`
	TerminalURL      string `json:"terminalUrl,omitempty"`
	IsOperator       bool   `json:"isOperator"`
	CanActivate      bool   `json:"canActivate"`
	CanRevoke        bool   `json:"canRevoke"`
	CanOpen          bool   `json:"canOpen"`
	NeedsRecentLogin bool   `json:"needsRecentLogin"`
	// Duration bounds are policy, not secrets: the SPA offers only durations
	// the server would accept.
	DefaultDurationSeconds int64              `json:"defaultDurationSeconds"`
	MaxDurationSeconds     int64              `json:"maxDurationSeconds"`
	ResourceVersion        string             `json:"resourceVersion,omitempty"`
	Phase                  admin.Phase        `json:"phase,omitempty"`
	ObservedAt             time.Time          `json:"observedAt,omitempty"`
	Fresh                  bool               `json:"fresh"`
	FailureCode            string             `json:"failureCode,omitempty"`
	Session                *adminSessionView  `json:"session"`
	Workspace              adminWorkspaceView `json:"workspace"`
	Grant                  adminGrantView     `json:"grant"`
	CleanupPending         bool               `json:"cleanupPending"`
}

func (s *Server) adminStore() admin.Store {
	return &admin.SecretStore{Client: s.kube, Namespace: s.cfg.PortalNamespace, Name: s.cfg.Admin.SessionSecret}
}

// adminCaller resolves the authenticated operator behind a request. It fails
// closed when elevation is disabled, GitHub authentication is off, or the
// numeric GitHub user ID is not on the allowlist.
func (s *Server) adminCaller(w http.ResponseWriter, r *http.Request) (githubapp.User, bool) {
	if !s.cfg.Admin.Enabled {
		writeError(w, http.StatusForbidden, "cluster-admin elevation is not enabled on this portal")
		return githubapp.User{}, false
	}
	if s.sessions == nil {
		writeError(w, http.StatusForbidden, "cluster-admin elevation requires GitHub authentication")
		return githubapp.User{}, false
	}
	user, err := s.sessions.Authorize(r)
	if err != nil {
		writeError(w, http.StatusUnauthorized, "GitHub session denied or unavailable")
		return githubapp.User{}, false
	}
	if !s.cfg.Admin.IsOperator(user.ID) {
		s.log.Warn("cluster-admin action denied", "action", r.URL.Path, "login", user.Login)
		writeError(w, http.StatusForbidden, "this account is not an authorized cluster-admin operator")
		return githubapp.User{}, false
	}
	return user, true
}

// adminCallerRecent additionally requires a recent sign-in. Activation and
// sign-in-link retrieval use it; revocation deliberately does not, so an
// emergency shutdown never depends on a fresh login.
func (s *Server) adminCallerRecent(w http.ResponseWriter, r *http.Request) (githubapp.User, bool) {
	user, ok := s.adminCaller(w, r)
	if !ok {
		return githubapp.User{}, false
	}
	if _, err := s.sessions.AuthorizeRecent(r, s.cfg.Admin.RecentLogin); err != nil {
		if errors.Is(err, githubauth.ErrStaleLogin) {
			writeError(w, http.StatusForbidden, "a recent GitHub sign-in is required for this action")
			return githubapp.User{}, false
		}
		writeError(w, http.StatusUnauthorized, "GitHub session denied or unavailable")
		return githubapp.User{}, false
	}
	return user, true
}

func (s *Server) handleAdminStatus(w http.ResponseWriter, r *http.Request) {
	if !s.cfg.Admin.Enabled {
		writeJSON(w, http.StatusOK, adminStatusResponse{Enabled: false})
		return
	}
	user, ok := s.adminCaller(w, r)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), requestTimeout)
	defer cancel()
	state, err := s.adminStore().Load(ctx)
	if err != nil {
		if errors.Is(err, admin.ErrInvalidState) {
			// Malformed state is a cleanup-required condition, not a server error.
			s.log.Error("admin session state is malformed")
			state.Record = admin.Record{ObservedPhase: admin.PhaseCleanupRequired, FailureCode: admin.FailureStateInvalid}
			writeJSON(w, http.StatusOK, s.adminView(user, user, state, admin.PhaseCleanupRequired, false))
			return
		}
		s.writeKubeError(w, "read admin session", err)
		return
	}
	record := state.Record
	now := time.Now()
	fresh := !record.LastReconciledAt.IsZero() && now.Sub(record.LastReconciledAt) <= adminObservationStaleAfter
	phase := record.ObservedPhase
	if record.Active() && !fresh {
		// Stale observations are unknown state, never a healthy session.
		phase = admin.PhaseCleanupRequired
	}
	_, recentErr := s.sessions.AuthorizeRecent(r, s.cfg.Admin.RecentLogin)
	view := s.adminView(user, user, state, phase, fresh)
	view.NeedsRecentLogin = errors.Is(recentErr, githubauth.ErrStaleLogin)
	writeJSON(w, http.StatusOK, view)
}

func (s *Server) adminView(_ githubapp.User, caller githubapp.User, state admin.State, phase admin.Phase, fresh bool) adminStatusResponse {
	record := state.Record
	now := time.Now()
	view := adminStatusResponse{
		Enabled:                true,
		Namespace:              s.cfg.Admin.Namespace,
		Deployment:             s.cfg.Admin.Deployment,
		PocketURL:              s.cfg.Admin.PocketURL,
		TerminalURL:            s.cfg.Admin.TerminalURL,
		IsOperator:             s.cfg.Admin.IsOperator(caller.ID),
		DefaultDurationSeconds: int64(s.cfg.Admin.DefaultDuration / time.Second),
		MaxDurationSeconds:     int64(s.cfg.Admin.MaxDuration / time.Second),
		ResourceVersion:        state.ResourceVersion,
		Phase:                  phase,
		ObservedAt:             record.LastReconciledAt,
		Fresh:                  fresh,
		FailureCode:            record.FailureCode,
		Workspace: adminWorkspaceView{
			Ready:      record.WorkspaceReady,
			PodUID:     record.ActivePodUID,
			ObservedAt: record.LastReconciledAt,
		},
		Grant:          adminGrantView{Observed: record.GrantObserved},
		CleanupPending: phase == admin.PhaseCleanupRequired,
	}
	if record.SessionID != "" {
		view.Session = &adminSessionView{
			SessionID:        record.SessionID,
			OwnerUserID:      record.ApprovedByUserID,
			Owner:            record.ApprovedByLogin,
			Reason:           record.Reason,
			ApprovedAt:       record.ApprovedAt,
			ExpiresAt:        record.ExpiresAt,
			SecondsRemaining: record.SecondsRemaining(now),
		}
	}
	idle := phase == admin.PhaseIdle || phase == ""
	view.CanActivate = view.IsOperator && idle
	view.CanRevoke = view.IsOperator && record.Active()
	owner := record.SessionID != "" && record.ApprovedByUserID == caller.ID && record.ApprovedByUserID != 0
	view.CanOpen = view.IsOperator && owner && phase == admin.PhaseActive && fresh && record.WorkspaceReady
	return view
}

func (s *Server) handleAdminActivate(w http.ResponseWriter, r *http.Request) {
	user, ok := s.adminCallerRecent(w, r)
	if !ok {
		return
	}
	var request struct {
		ResourceVersion string `json:"resourceVersion"`
		Reason          string `json:"reason"`
		DurationSeconds int64  `json:"durationSeconds"`
		Confirmation    string `json:"confirmation"`
	}
	if status, err := decodeJSON(w, r, &request); err != nil {
		writeError(w, status, err.Error())
		return
	}
	if request.Confirmation != adminConfirmation {
		writeError(w, http.StatusBadRequest, "confirmation must be exactly "+adminConfirmation)
		return
	}
	if request.ResourceVersion == "" {
		writeError(w, http.StatusBadRequest, "resourceVersion is required")
		return
	}
	sessionID, err := newSessionID()
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "cannot generate a session identifier")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), requestTimeout)
	defer cancel()
	store := s.adminStore()
	state, err := store.Load(ctx)
	if err != nil {
		if errors.Is(err, admin.ErrInvalidState) {
			writeError(w, http.StatusConflict, "cluster-admin state is malformed and requires cleanup")
			return
		}
		s.writeKubeError(w, "read admin session", err)
		return
	}
	if state.ResourceVersion != request.ResourceVersion {
		writeError(w, http.StatusConflict, "cluster-admin state changed; reload before activating")
		return
	}
	duration := time.Duration(request.DurationSeconds) * time.Second
	record, err := admin.ValidateActivation(state, admin.ActivationRequest{
		Reason:           request.Reason,
		Duration:         duration,
		ApprovedByUserID: user.ID,
		ApprovedByLogin:  user.Login,
		SessionID:        sessionID,
	}, s.cfg.Admin.DefaultDuration, s.cfg.Admin.MaxDuration, time.Now())
	if err != nil {
		switch {
		case errors.Is(err, admin.ErrSessionActive):
			writeError(w, http.StatusConflict, "a cluster-admin session is already active or being cleaned up")
		case errors.Is(err, admin.ErrCleanupIncomplet):
			writeError(w, http.StatusConflict, "a previous session's cleanup is incomplete; resolve it before activating")
		default:
			writeError(w, http.StatusBadRequest, err.Error())
		}
		return
	}
	saved, err := store.Save(ctx, admin.State{ResourceVersion: state.ResourceVersion, Record: record})
	if err != nil {
		if errors.Is(err, admin.ErrConflict) {
			writeError(w, http.StatusConflict, "cluster-admin state changed; reload before activating")
			return
		}
		s.writeKubeError(w, "write admin session", err)
		return
	}
	s.log.Info("cluster-admin session requested",
		"session", record.SessionID, "operator", user.Login, "seconds", int64(record.ExpiresAt.Sub(record.ApprovedAt)/time.Second))
	writeJSON(w, http.StatusAccepted, map[string]any{"phase": saved.Record.ObservedPhase, "sessionID": saved.Record.SessionID})
}

func (s *Server) handleAdminRevoke(w http.ResponseWriter, r *http.Request) {
	user, ok := s.adminCaller(w, r)
	if !ok {
		return
	}
	var request struct {
		ResourceVersion string `json:"resourceVersion"`
	}
	if r.ContentLength > 0 {
		if status, err := decodeJSON(w, r, &request); err != nil {
			writeError(w, status, err.Error())
			return
		}
	}
	ctx, cancel := context.WithTimeout(r.Context(), requestTimeout)
	defer cancel()
	store := s.adminStore()
	state, err := store.Load(ctx)
	if err != nil {
		s.writeKubeError(w, "read admin session", err)
		return
	}
	if request.ResourceVersion != "" && state.ResourceVersion != request.ResourceVersion {
		writeError(w, http.StatusConflict, "cluster-admin state changed; reload before revoking")
		return
	}
	record, err := admin.RequestRevocation(state, time.Now())
	if err != nil {
		writeError(w, http.StatusConflict, "no cluster-admin session is active")
		return
	}
	saved, err := store.Save(ctx, admin.State{ResourceVersion: state.ResourceVersion, Record: record})
	if err != nil {
		if errors.Is(err, admin.ErrConflict) {
			writeError(w, http.StatusConflict, "cluster-admin state changed; reload before revoking")
			return
		}
		s.writeKubeError(w, "write admin session", err)
		return
	}
	s.log.Info("cluster-admin session revocation requested", "session", saved.Record.SessionID, "operator", user.Login)
	writeJSON(w, http.StatusAccepted, map[string]any{"phase": saved.Record.ObservedPhase})
}

// handleAdminAccess returns the active session's owner sign-in link. The link
// is a credential: it is only ever returned to the operator who owns the
// session, only while the workspace is ready and freshly observed, and only
// when the stored Pod UID still matches the observed one.
func (s *Server) handleAdminAccess(w http.ResponseWriter, r *http.Request) {
	user, ok := s.adminCallerRecent(w, r)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), requestTimeout)
	defer cancel()
	state, err := s.adminStore().Load(ctx)
	if err != nil {
		s.writeKubeError(w, "read admin session", err)
		return
	}
	record := state.Record
	if record.SessionID == "" || record.ApprovedByUserID != user.ID {
		writeError(w, http.StatusForbidden, "only the operator who activated this session may retrieve its sign-in link")
		return
	}
	if record.ObservedPhase != admin.PhaseActive || record.RequestedState != admin.RequestedActive {
		writeError(w, http.StatusConflict, "the cluster-admin workspace is not active")
		return
	}
	if record.Expired(time.Now()) {
		writeError(w, http.StatusConflict, "the cluster-admin session has expired")
		return
	}
	if record.LastReconciledAt.IsZero() || time.Since(record.LastReconciledAt) > adminObservationStaleAfter {
		writeError(w, http.StatusConflict, "the cluster-admin state is not fresh; reload before retrieving the link")
		return
	}
	if !record.WorkspaceReady {
		writeError(w, http.StatusConflict, "the cluster-admin workspace is not ready")
		return
	}
	secret, err := s.kube.GetSecret(ctx, s.cfg.Admin.Namespace, s.cfg.Admin.RuntimeSecret)
	if err != nil {
		s.writeKubeError(w, "read admin sign-in link", err)
		return
	}
	link, present, err := kube.SecretString(secret, kube.OwnerLoginURLKey)
	if err != nil || !present || link == "" {
		writeError(w, http.StatusConflict, "the cluster-admin sign-in link is not published")
		return
	}
	podUID, uidPresent, err := kube.SecretString(secret, kube.OwnerLoginPodUIDKey)
	if err != nil || !uidPresent || podUID == "" || podUID != record.ActivePodUID {
		writeError(w, http.StatusConflict, "the cluster-admin sign-in link does not belong to the current workspace")
		return
	}
	if !sameOrigin(link, s.cfg.Admin.PocketURL) {
		s.log.Error("refusing an admin sign-in link with an unexpected origin")
		writeError(w, http.StatusConflict, "the cluster-admin sign-in link does not match the configured workspace origin")
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, map[string]any{"url": link})
}

// sameOrigin reports whether raw is an https URL on the same host as origin.
func sameOrigin(raw, origin string) bool {
	parsed, err := url.Parse(raw)
	if err != nil || !strings.EqualFold(parsed.Scheme, "https") || parsed.Host == "" {
		return false
	}
	expected, err := url.Parse(origin)
	if err != nil {
		return false
	}
	return strings.EqualFold(parsed.Host, expected.Host)
}

func newSessionID() (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("read random bytes: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(raw[:]), nil
}
