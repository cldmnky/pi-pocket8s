package server

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/cldmnky/pi-pocket8s/portal/internal/kube"
)

const repositoryPolicyKey = "repositories.json"

type repositoryPolicyView struct {
	ResourceVersion string   `json:"resourceVersion"`
	Repositories    []string `json:"repositories"`
}

// Read on EVERY request: no in-memory allowlist fallback, and no authorization
// via the workspace runtime Secret. An unreadable/malformed policy fails closed.
func (s *Server) repositoryPolicy(ctx context.Context) (repositoryPolicyView, error) {
	secret, err := s.kube.GetSecret(ctx, s.cfg.PortalNamespace, s.cfg.RepositoryPolicySecret)
	if err != nil {
		return repositoryPolicyView{}, err
	}
	raw, present, err := secret.Bytes(repositoryPolicyKey)
	if err != nil || !present {
		return repositoryPolicyView{}, errors.New("repository policy missing")
	}
	var repos []string
	if len(raw) > 128*1024 || json.Unmarshal(raw, &repos) != nil || repos == nil {
		return repositoryPolicyView{}, errors.New("invalid repository policy")
	}
	repos, err = s.github.ValidateRepositories(repos)
	if err != nil {
		return repositoryPolicyView{}, err
	}
	return repositoryPolicyView{ResourceVersion: secret.Metadata.ResourceVersion, Repositories: repos}, nil
}

func (s *Server) repositoryOwner(w http.ResponseWriter, r *http.Request) bool {
	if s.sessions == nil || s.github == nil {
		writeError(w, http.StatusForbidden, "GitHub organization owner required")
		return false
	}
	user, err := s.sessions.Authorize(r)
	if err != nil || s.github.CheckOwner(r.Context(), user.Login) != nil {
		writeError(w, http.StatusForbidden, "GitHub organization owner required")
		return false
	}
	return true
}

func (s *Server) handleRepositoryCatalog(w http.ResponseWriter, r *http.Request) {
	if !s.repositoryOwner(w, r) {
		return
	}
	repos, err := s.github.AvailableRepositories(r.Context())
	if err != nil {
		writeError(w, http.StatusBadGateway, "App repository catalog unavailable")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"repositories": repos})
}

func (s *Server) handleSaveRepositories(w http.ResponseWriter, r *http.Request) {
	if !s.repositoryOwner(w, r) {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 128*1024)
	var request repositoryPolicyView
	if status, err := decodeJSON(w, r, &request); err != nil {
		writeError(w, status, "invalid repository policy")
		return
	}
	if request.ResourceVersion == "" || request.Repositories == nil {
		writeError(w, http.StatusBadRequest, "resourceVersion and repositories array required")
		return
	}
	repos, err := s.github.ValidateRepositories(request.Repositories)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	// Only grant names actually exposed by a configured installation. Revoking all
	// repositories must remain possible even during a GitHub outage.
	if len(repos) > 0 {
		available, err := s.github.AvailableRepositories(r.Context())
		if err != nil {
			writeError(w, http.StatusBadGateway, "App repository catalog unavailable")
			return
		}
		catalog := map[string]bool{}
		for _, repo := range available {
			catalog[repo] = true
		}
		for _, repo := range repos {
			if !catalog[repo] {
				writeError(w, http.StatusBadRequest, "repository is not accessible to the App")
				return
			}
		}
	}
	raw, _ := json.Marshal(repos)
	patch, _ := json.Marshal(map[string]any{"metadata": map[string]string{"resourceVersion": request.ResourceVersion}, "data": map[string]string{repositoryPolicyKey: base64.StdEncoding.EncodeToString(raw)}})
	_, err = s.kube.PatchSecret(r.Context(), s.cfg.PortalNamespace, s.cfg.RepositoryPolicySecret, patch)
	if err != nil {
		if kube.IsConflict(err) {
			writeError(w, http.StatusConflict, "repository policy changed; reload before saving")
			return
		}
		s.log.Error("repository policy write failed")
		writeError(w, http.StatusServiceUnavailable, "repository policy save failed")
		return
	}
	s.log.Info("repository policy updated", "repository_count", len(repos))
	writeJSON(w, http.StatusOK, map[string]any{"repositories": repos, "message": fmt.Sprintf("Saved %d repositories; applies to new credentials immediately", len(repos))})
}
