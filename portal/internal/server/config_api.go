package server

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/cldmnky/pi-pocket8s/portal/internal/kube"
)

const (
	apiKeysSecretKey        = "api-keys.json"
	authorizedKeysSecretKey = "authorized_keys"
	knownHostsSecretKey     = "known_hosts"
	ownerLoginSecretKey     = "owner-login-url"
	// webSearchSecretKey is mounted into the agent at /run/pocket-config/web-search.json;
	// the entrypoint points the agent's web-search config at it, so the agent picks up
	// this setting on its next start.
	webSearchSecretKey = "web-search.json"
)

// configView is the redacted runtime configuration shown to the browser.
// API key values are never included; only names and set/unset status. The
// owner sign-in URL is shown because this portal already requires its own
// administrative bearer token; it is synced into the secret by the agent
// entrypoint at container start.
type configView struct {
	ResourceVersion string          `json:"resourceVersion"`
	AllowedAPIKeys  []string        `json:"allowedApiKeys"`
	APIKeys         map[string]bool `json:"apiKeys"`
	AuthorizedKeys  string          `json:"authorizedKeys"`
	KnownHosts      string          `json:"knownHosts"`
	PocketURL       string          `json:"pocketUrl"`
	TerminalURL     string          `json:"terminalUrl"`
	OwnerLoginURL   string          `json:"ownerLoginUrl"`
	// WebSearch is nil until an operator chooses a search model.
	WebSearch *webSearchView `json:"webSearch"`
}

// webSearchView is the stored choice of which model performs web searches.
type webSearchView struct {
	Provider string `json:"provider"`
	Model    string `json:"model"`
}

func (s *Server) handleGetConfig(w http.ResponseWriter, r *http.Request) {
	secret, err := s.kube.GetSecret(r.Context(), s.cfg.Namespace, s.cfg.ConfigSecret)
	if err != nil {
		s.writeKubeError(w, "read runtime secret", err)
		return
	}
	view, err := s.configView(secret)
	if err != nil {
		s.log.Error("runtime secret is invalid", "error", err)
		writeError(w, http.StatusInternalServerError, "runtime secret is invalid: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, view)
}

func (s *Server) handlePostConfig(w http.ResponseWriter, r *http.Request) {
	var req configUpdateRequest
	if status, err := decodeJSON(w, r, &req); err != nil {
		writeError(w, status, err.Error())
		return
	}
	if err := validateConfigUpdate(&req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	secret, err := s.kube.GetSecret(r.Context(), s.cfg.Namespace, s.cfg.ConfigSecret)
	if err != nil {
		s.writeKubeError(w, "read runtime secret", err)
		return
	}
	if secret.Metadata.ResourceVersion != req.ResourceVersion {
		writeError(w, http.StatusConflict, "runtime configuration changed concurrently; reload and try again")
		return
	}

	dataPatch := map[string]any{}
	if req.APIKeys != nil {
		stored, err := readAPIKeys(secret)
		if err != nil {
			s.log.Error("runtime secret is invalid", "error", err)
			writeError(w, http.StatusInternalServerError, "runtime secret is invalid: "+err.Error())
			return
		}
		for name, value := range req.APIKeys.Set {
			stored[name] = value
		}
		for _, name := range req.APIKeys.Remove {
			delete(stored, name)
		}
		encoded, err := json.Marshal(stored)
		if err != nil {
			s.log.Error("encode api keys", "error", err)
			writeError(w, http.StatusInternalServerError, "internal server error")
			return
		}
		if len(encoded) > maxAPIKeysJSONBytes {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("api-keys.json must be at most %d bytes after saving", maxAPIKeysJSONBytes))
			return
		}
		dataPatch[apiKeysSecretKey] = base64.StdEncoding.EncodeToString(encoded)
	}
	if req.AuthorizedKeys != nil {
		dataPatch[authorizedKeysSecretKey] = base64.StdEncoding.EncodeToString([]byte(*req.AuthorizedKeys))
	}
	if req.KnownHosts != nil {
		dataPatch[knownHostsSecretKey] = base64.StdEncoding.EncodeToString([]byte(*req.KnownHosts))
	}
	if req.WebSearch != nil {
		if strings.TrimSpace(req.WebSearch.Provider) == "" && strings.TrimSpace(req.WebSearch.Model) == "" {
			// Clearing removes the key: the mount then holds no file and the agent goes
			// back to choosing for itself.
			dataPatch[webSearchSecretKey] = nil
		} else {
			encoded, err := json.Marshal(map[string]string{
				"provider": strings.TrimSpace(req.WebSearch.Provider),
				"model":    strings.TrimSpace(req.WebSearch.Model),
			})
			if err != nil {
				s.log.Error("encode web search setting", "error", err)
				writeError(w, http.StatusInternalServerError, "internal server error")
				return
			}
			dataPatch[webSearchSecretKey] = base64.StdEncoding.EncodeToString(encoded)
		}
	}
	if len(dataPatch) == 0 {
		writeError(w, http.StatusBadRequest, "no configuration changes requested")
		return
	}

	patch, err := json.Marshal(map[string]any{
		"metadata": map[string]any{"resourceVersion": req.ResourceVersion},
		"data":     dataPatch,
	})
	if err != nil {
		s.log.Error("encode secret patch", "error", err)
		writeError(w, http.StatusInternalServerError, "internal server error")
		return
	}

	updated, err := s.kube.PatchSecret(r.Context(), s.cfg.Namespace, s.cfg.ConfigSecret, patch)
	if err != nil {
		if kube.IsConflict(err) {
			writeError(w, http.StatusConflict, "runtime configuration changed concurrently; reload and try again")
			return
		}
		s.writeKubeError(w, "save runtime secret", err)
		return
	}
	view, err := s.configView(updated)
	if err != nil {
		s.log.Error("saved runtime secret is invalid", "error", err)
		writeError(w, http.StatusInternalServerError, "runtime secret is invalid: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, view)
}

// configView builds the redacted view from a Secret.
func (s *Server) configView(secret *kube.Secret) (configView, error) {
	stored, err := readAPIKeys(secret)
	if err != nil {
		return configView{}, err
	}
	apiKeys := make(map[string]bool, len(allowedAPIKeyNames))
	for _, name := range allowedAPIKeyNames {
		_, set := stored[name]
		apiKeys[name] = set
	}
	authorizedKeys, _, err := secret.Bytes(authorizedKeysSecretKey)
	if err != nil {
		return configView{}, err
	}
	knownHosts, _, err := secret.Bytes(knownHostsSecretKey)
	if err != nil {
		return configView{}, err
	}
	ownerLogin, _, err := secret.Bytes(ownerLoginSecretKey)
	if err != nil {
		return configView{}, err
	}
	webSearch, err := readWebSearch(secret)
	if err != nil {
		return configView{}, err
	}
	return configView{
		ResourceVersion: secret.Metadata.ResourceVersion,
		AllowedAPIKeys:  append([]string(nil), allowedAPIKeyNames...),
		APIKeys:         apiKeys,
		AuthorizedKeys:  string(authorizedKeys),
		KnownHosts:      string(knownHosts),
		PocketURL:       s.cfg.PocketURL,
		TerminalURL:     s.cfg.TerminalURL,
		OwnerLoginURL:   string(ownerLogin),
		WebSearch:       webSearch,
	}, nil
}

// readWebSearch parses the stored search-model choice. An absent or empty entry is
// not an error; a present one must name both a provider and a model, because half a
// choice would be mounted into the agent as a file it cannot use.
func readWebSearch(secret *kube.Secret) (*webSearchView, error) {
	raw, present, err := secret.Bytes(webSearchSecretKey)
	if err != nil {
		return nil, err
	}
	if !present || len(bytes.TrimSpace(raw)) == 0 {
		return nil, nil
	}
	var stored webSearchView
	decoder := json.NewDecoder(bytes.NewReader(raw))
	if err := decoder.Decode(&stored); err != nil {
		return nil, fmt.Errorf("%s: %w", webSearchSecretKey, err)
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("%s: trailing data", webSearchSecretKey)
	}
	stored.Provider = strings.TrimSpace(stored.Provider)
	stored.Model = strings.TrimSpace(stored.Model)
	if stored.Provider == "" || stored.Model == "" {
		return nil, fmt.Errorf("%s: needs a provider and a model", webSearchSecretKey)
	}
	return &stored, nil
}

// readAPIKeys parses the api-keys.json secret entry. Unknown names already
// stored are preserved so the portal never destroys data it does not manage.
func readAPIKeys(secret *kube.Secret) (map[string]string, error) {
	raw, present, err := secret.Bytes(apiKeysSecretKey)
	if err != nil {
		return nil, err
	}
	if !present || len(bytes.TrimSpace(raw)) == 0 {
		return map[string]string{}, nil
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	keys := map[string]string{}
	if err := decoder.Decode(&keys); err != nil {
		return nil, fmt.Errorf("api-keys.json: %w", err)
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return nil, errors.New("api-keys.json: trailing data")
	}
	return keys, nil
}
