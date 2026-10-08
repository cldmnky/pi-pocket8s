package server

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/cldmnky/pi-pocket8s/portal/internal/kube"
)

const (
	apiKeysSecretKey        = "api-keys.json"
	authorizedKeysSecretKey = "authorized_keys"
	knownHostsSecretKey     = "known_hosts"
)

// configView is the redacted runtime configuration shown to the browser.
// API key values are never included; only names and set/unset status.
type configView struct {
	ResourceVersion string          `json:"resourceVersion"`
	AllowedAPIKeys  []string        `json:"allowedApiKeys"`
	APIKeys         map[string]bool `json:"apiKeys"`
	AuthorizedKeys  string          `json:"authorizedKeys"`
	KnownHosts      string          `json:"knownHosts"`
	PocketURL       string          `json:"pocketUrl"`
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
		writeError(w, http.StatusInternalServerError, "runtime secret contains invalid api-keys.json")
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
			writeError(w, http.StatusInternalServerError, "runtime secret contains invalid api-keys.json")
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
		writeError(w, http.StatusInternalServerError, "runtime secret contains invalid api-keys.json")
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
	return configView{
		ResourceVersion: secret.Metadata.ResourceVersion,
		AllowedAPIKeys:  append([]string(nil), allowedAPIKeyNames...),
		APIKeys:         apiKeys,
		AuthorizedKeys:  string(authorizedKeys),
		KnownHosts:      string(knownHosts),
		PocketURL:       s.cfg.PocketURL,
	}, nil
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
