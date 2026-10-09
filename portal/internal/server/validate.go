package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"

	"golang.org/x/crypto/ssh"
)

const (
	// maxRequestBodyBytes bounds every API request body.
	maxRequestBodyBytes = 1 << 20
	// maxAPIKeyValueBytes bounds a single API key or AWS credential value.
	maxAPIKeyValueBytes = 4096
	// maxAPIKeysJSONBytes bounds the composed api-keys.json secret entry.
	maxAPIKeysJSONBytes = 32 << 10
	// maxAuthorizedKeysBytes bounds the authorized_keys secret entry.
	maxAuthorizedKeysBytes = 32 << 10
	// maxSSHKeyLineBytes bounds one public key line.
	maxSSHKeyLineBytes = 8192
	// maxAuthorizedKeyLines bounds how many public keys may be stored.
	maxAuthorizedKeyLines = 64
	// maxKnownHostsBytes bounds the known_hosts secret entry.
	maxKnownHostsBytes = 64 << 10
	// maxKnownHostsLineBytes bounds one known_hosts line.
	maxKnownHostsLineBytes = 8192
	// maxKnownHostLines bounds how many known_hosts lines may be stored.
	maxKnownHostLines = 1024
)

// allowedAPIKeyNames is the complete set of environment variable names the
// portal accepts for the agent's runtime secret. Names outside this list are
// rejected so the portal cannot become a generic environment injector.
var allowedAPIKeyNames = []string{
	"ANTHROPIC_API_KEY",
	"OPENAI_API_KEY",
	"GEMINI_API_KEY",
	"GOOGLE_API_KEY",
	"GROQ_API_KEY",
	"OPENROUTER_API_KEY",
	"MISTRAL_API_KEY",
	"DEEPSEEK_API_KEY",
	"XAI_API_KEY",
	"AWS_ACCESS_KEY_ID",
	"AWS_SECRET_ACCESS_KEY",
	"AWS_SESSION_TOKEN",
	"AWS_REGION",
}

var allowedAPIKeySet = func() map[string]struct{} {
	set := make(map[string]struct{}, len(allowedAPIKeyNames))
	for _, name := range allowedAPIKeyNames {
		set[name] = struct{}{}
	}
	return set
}()

type configUpdateRequest struct {
	ResourceVersion string          `json:"resourceVersion"`
	APIKeys         *apiKeysPatch   `json:"apiKeys"`
	AuthorizedKeys  *string         `json:"authorizedKeys"`
	KnownHosts      *string         `json:"knownHosts"`
	WebSearch       *webSearchPatch `json:"webSearch"`
}

// webSearchPatch chooses the model that performs web searches. Both fields empty
// clears the setting; one of them alone is rejected, because half a choice would
// leave the agent searching with a provider the operator did not choose.
type webSearchPatch struct {
	Provider string `json:"provider"`
	Model    string `json:"model"`
}

type apiKeysPatch struct {
	Set    map[string]string `json:"set"`
	Remove []string          `json:"remove"`
}

// validateConfigUpdate rejects malformed or out-of-policy updates before any
// cluster state is read or written.
func validateConfigUpdate(req *configUpdateRequest) error {
	if req.ResourceVersion == "" {
		return errors.New("resourceVersion is required")
	}
	if len(req.ResourceVersion) > 128 || !isPrintableASCII(req.ResourceVersion) {
		return errors.New("resourceVersion is invalid")
	}
	if req.APIKeys == nil && req.AuthorizedKeys == nil && req.KnownHosts == nil && req.WebSearch == nil {
		return errors.New("no configuration changes requested")
	}
	if req.APIKeys != nil {
		if len(req.APIKeys.Set) == 0 && len(req.APIKeys.Remove) == 0 {
			return errors.New("apiKeys must set or remove at least one key")
		}
		for name, value := range req.APIKeys.Set {
			if !allowedAPIKeyName(name) {
				return fmt.Errorf("api key %q is not allowed", name)
			}
			if err := validateAPIKeyValue(value); err != nil {
				return fmt.Errorf("api key %s: %w", name, err)
			}
		}
		seen := make(map[string]bool, len(req.APIKeys.Remove))
		for _, name := range req.APIKeys.Remove {
			if !allowedAPIKeyName(name) {
				return fmt.Errorf("api key %q is not allowed", name)
			}
			if _, both := req.APIKeys.Set[name]; both {
				return fmt.Errorf("api key %s cannot be set and removed at once", name)
			}
			if seen[name] {
				return fmt.Errorf("api key %s is listed twice", name)
			}
			seen[name] = true
		}
	}
	if req.AuthorizedKeys != nil {
		if err := validateAuthorizedKeys(*req.AuthorizedKeys); err != nil {
			return err
		}
	}
	if req.KnownHosts != nil {
		if err := validateKnownHosts(*req.KnownHosts); err != nil {
			return err
		}
	}
	if req.WebSearch != nil {
		if err := validateWebSearch(req.WebSearch); err != nil {
			return err
		}
	}
	return nil
}

// Provider ids and model ids as the agent's model registry spells them; the model
// may carry a namespace ("anthropic/claude-haiku-4.5") or a version suffix. Kept
// narrow on purpose: these two strings become the agent's web-search.json.
var (
	webSearchProviderRE = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)
	webSearchModelRE    = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/@+-]{0,127}$`)
)

func validateWebSearch(patch *webSearchPatch) error {
	provider := strings.TrimSpace(patch.Provider)
	model := strings.TrimSpace(patch.Model)

	if provider == "" && model == "" {
		return nil
	}
	if provider == "" || model == "" {
		return errors.New("webSearch needs a provider and a model, or neither to clear it")
	}
	if !webSearchProviderRE.MatchString(provider) {
		return fmt.Errorf("invalid webSearch provider %q", provider)
	}
	if !webSearchModelRE.MatchString(model) {
		return fmt.Errorf("invalid webSearch model %q", model)
	}
	return nil
}

func allowedAPIKeyName(name string) bool {
	_, ok := allowedAPIKeySet[name]
	return ok
}

// validateAPIKeyValue accepts a single printable-ASCII credential without
// surrounding whitespace.
func validateAPIKeyValue(value string) error {
	if value == "" {
		return errors.New("value must not be empty")
	}
	if value != strings.TrimSpace(value) {
		return errors.New("value must not start or end with whitespace")
	}
	if len(value) > maxAPIKeyValueBytes {
		return fmt.Errorf("value must be at most %d bytes", maxAPIKeyValueBytes)
	}
	if !isPrintableASCII(value) {
		return errors.New("value must contain only printable ASCII characters")
	}
	return nil
}

// validateAuthorizedKeys parses every non-comment line as an OpenSSH public
// key and rejects private key material.
func validateAuthorizedKeys(text string) error {
	if len(text) > maxAuthorizedKeysBytes {
		return fmt.Errorf("authorized_keys must be at most %d bytes", maxAuthorizedKeysBytes)
	}
	if containsPrivateKeyMarker(text) {
		return errors.New("private keys are not accepted; provide SSH public keys only")
	}
	count := 0
	for i, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		count++
		if count > maxAuthorizedKeyLines {
			return fmt.Errorf("authorized_keys must contain at most %d keys", maxAuthorizedKeyLines)
		}
		if len(line) > maxSSHKeyLineBytes {
			return fmt.Errorf("authorized_keys line %d is too long", i+1)
		}
		if _, _, _, _, err := ssh.ParseAuthorizedKey([]byte(line)); err != nil {
			return fmt.Errorf("authorized_keys line %d is not a valid SSH public key", i+1)
		}
	}
	return nil
}

// validateKnownHosts treats the file as opaque text: bounded size, no control
// characters, and no private key material. The portal never executes it.
func validateKnownHosts(text string) error {
	if len(text) > maxKnownHostsBytes {
		return fmt.Errorf("known_hosts must be at most %d bytes", maxKnownHostsBytes)
	}
	if containsPrivateKeyMarker(text) {
		return errors.New("private keys are not accepted in known_hosts")
	}
	lines := strings.Split(text, "\n")
	if len(lines) > maxKnownHostLines {
		return fmt.Errorf("known_hosts must contain at most %d lines", maxKnownHostLines)
	}
	for i, line := range lines {
		if len(line) > maxKnownHostsLineBytes {
			return fmt.Errorf("known_hosts line %d is too long", i+1)
		}
		for _, r := range line {
			if (r < 0x20 && r != '\t') || r == 0x7f {
				return fmt.Errorf("known_hosts line %d contains control characters", i+1)
			}
		}
	}
	return nil
}

func containsPrivateKeyMarker(text string) bool {
	return strings.Contains(strings.ToUpper(text), "PRIVATE KEY")
}

func isPrintableASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < 0x21 || s[i] > 0x7e {
			return false
		}
	}
	return true
}

// decodeJSON decodes exactly one JSON object, bounded by maxRequestBodyBytes.
// It returns the HTTP status to use when decoding fails.
func decodeJSON(w http.ResponseWriter, r *http.Request, dst any) (int, error) {
	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBodyBytes)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(dst); err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			return http.StatusRequestEntityTooLarge, fmt.Errorf("request body must be at most %d bytes", maxRequestBodyBytes)
		}
		return http.StatusBadRequest, errors.New("request body must be a single JSON object with known fields")
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return http.StatusBadRequest, errors.New("request body must contain a single JSON object")
	}
	return 0, nil
}
