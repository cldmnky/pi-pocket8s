package server

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/cldmnky/pi-pocket8s/portal/internal/githubapp"
	"github.com/cldmnky/pi-pocket8s/portal/internal/githubauth"
)

// GitHubProvider combines human identity checks with scoped bot credentials.
type GitHubProvider interface {
	githubauth.Provider
	Organization() string
	Team() string
	CheckOwner(context.Context, string) error
	AvailableRepositories(context.Context) ([]string, error)
	ValidateRepositories([]string) ([]string, error)
	MintRepositoryToken(context.Context, string) (githubapp.Token, error)
}

// EnableGitHub switches the server to GitHub-only sessions. The bearer-token
// fallback is deliberately disabled in this mode, rather than being an SSO bypass.
func (s *Server) EnableGitHub(client GitHubProvider) {
	s.github = client
	s.sessions = githubauth.New(client, s.cfg.PortalOrigin, s.log)
}

func (s *Server) handleSession(w http.ResponseWriter, r *http.Request) {
	if s.sessions != nil {
		s.sessions.Session(w, r)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"mode": "token", "authenticated": false})
}

func (s *Server) handleGitHubStatus(w http.ResponseWriter, r *http.Request) {
	if s.github == nil {
		writeJSON(w, http.StatusOK, map[string]any{"enabled": false})
		return
	}
	policy, err := s.repositoryPolicy(r.Context())
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "repository policy unavailable")
		return
	}
	user, err := s.sessions.Authorize(r)
	canManage := err == nil && s.github.CheckOwner(r.Context(), user.Login) == nil
	writeJSON(w, http.StatusOK, map[string]any{"enabled": true, "identity": "GitHub App bot", "organization": s.github.Organization(), "team": s.github.Team(), "repositories": policy.Repositories, "resourceVersion": policy.ResourceVersion, "canManage": canManage, "permissions": map[string]string{"contents": "write", "pull_requests": "write", "actions": "write"}, "tokenLifetimeSeconds": 3600})
}

// This endpoint uses an audience-bound Kubernetes identity, not a browser
// session, so unattended jobs can renew credentials even after the operator
// logs out. It only issues bot credentials for the server-side repository list.
func (s *Server) handleGitHubCredentials(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	scheme, token, ok := strings.Cut(r.Header.Get("Authorization"), " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") || s.kube.CheckWorkspaceToken(ctx, token, s.cfg.Namespace, s.cfg.WorkspaceServiceAccount) != nil {
		writeError(w, http.StatusUnauthorized, "workspace identity required")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	var request struct {
		Repository string `json:"repository"`
	}
	if status, err := decodeJSON(w, r, &request); err != nil {
		writeError(w, status, "invalid credential request")
		return
	}
	policy, err := s.repositoryPolicy(ctx)
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "repository policy unavailable")
		return
	}
	repo := strings.ToLower(strings.TrimSpace(request.Repository))
	allowed := false
	for _, selected := range policy.Repositories {
		if selected == repo {
			allowed = true
			break
		}
	}
	if !allowed {
		writeError(w, http.StatusForbidden, "repository credentials denied or unavailable")
		return
	}
	credential, err := s.github.MintRepositoryToken(ctx, repo)
	if err != nil {
		writeError(w, http.StatusForbidden, "repository credentials denied or unavailable")
		return
	}
	writeJSON(w, http.StatusOK, credential)
}
