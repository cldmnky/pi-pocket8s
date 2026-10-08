package server_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func TestGetConfigRedactsAPIKeyValues(t *testing.T) {
	env := newPortalTestEnv(t)
	result := env.getConfig()
	if result.Status != http.StatusOK {
		t.Fatalf("GET /api/config status = %d, want 200 (body %s)", result.Status, result.Body)
	}
	for _, secret := range []string{testAPIKeyValue, "sk-openai-secret-value"} {
		if strings.Contains(string(result.Body), secret) {
			t.Fatalf("GET /api/config leaked API key value %q", secret)
		}
	}

	view := decodeJSON[configViewResponse](t, result)
	if view.ResourceVersion != "1" {
		t.Errorf("resourceVersion = %q, want 1", view.ResourceVersion)
	}
	if !view.APIKeys["ANTHROPIC_API_KEY"] || !view.APIKeys["OPENAI_API_KEY"] {
		t.Errorf("stored keys not reported as set: %+v", view.APIKeys)
	}
	if view.APIKeys["GROQ_API_KEY"] || view.APIKeys["AWS_SECRET_ACCESS_KEY"] {
		t.Errorf("unset keys reported as set: %+v", view.APIKeys)
	}
	if len(view.AllowedAPIKeys) != 13 {
		t.Errorf("allowedApiKeys has %d entries, want 13", len(view.AllowedAPIKeys))
	}
	if !contains(view.AllowedAPIKeys, "AWS_REGION") {
		t.Errorf("allowedApiKeys is missing AWS_REGION: %v", view.AllowedAPIKeys)
	}
	if view.AuthorizedKeys == "" || view.KnownHosts == "" {
		t.Errorf("SSH text missing: authorizedKeys=%q knownHosts=%q", view.AuthorizedKeys, view.KnownHosts)
	}
	if view.PocketURL != "https://pi.example.com" {
		t.Errorf("pocketUrl = %q", view.PocketURL)
	}
	if view.TerminalURL != "https://terminal.example.com" {
		t.Errorf("terminalUrl = %q", view.TerminalURL)
	}
	if view.OwnerLoginURL != "" {
		t.Errorf("ownerLoginUrl = %q, want empty before the agent syncs it", view.OwnerLoginURL)
	}
}

func TestGetConfigExposesSyncedOwnerLogin(t *testing.T) {
	env := newPortalTestEnv(t)
	env.fake.setSecretValue("owner-login-url", "https://pi.example.com/login?token=owner-secret")
	result := env.getConfig()
	if result.Status != http.StatusOK {
		t.Fatalf("GET /api/config status = %d, want 200 (body %s)", result.Status, result.Body)
	}
	view := decodeJSON[configViewResponse](t, result)
	if view.OwnerLoginURL != "https://pi.example.com/login?token=owner-secret" {
		t.Errorf("ownerLoginUrl = %q", view.OwnerLoginURL)
	}
}

func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func TestGetConfigRejectsMalformedAPIKeys(t *testing.T) {
	env := newPortalTestEnv(t)
	env.fake.setSecretValue("api-keys.json", "this is not json")
	result := env.getConfig()
	if result.Status != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 (body %s)", result.Status, result.Body)
	}
	if !strings.Contains(string(result.Body), "api-keys.json") {
		t.Errorf("error body = %s, want mention of api-keys.json", result.Body)
	}
}

func TestPostConfigSetsAndRemovesAPIKeys(t *testing.T) {
	env := newPortalTestEnv(t)
	body := map[string]any{
		"resourceVersion": "1",
		"apiKeys": map[string]any{
			"set":    map[string]string{"GROQ_API_KEY": "gsk-new-secret"},
			"remove": []string{"OPENAI_API_KEY"},
		},
	}
	result := env.postConfig(body)
	if result.Status != http.StatusOK {
		t.Fatalf("POST /api/config status = %d, want 200 (body %s)", result.Status, result.Body)
	}
	if strings.Contains(string(result.Body), "gsk-new-secret") {
		t.Fatal("POST /api/config response leaked the new API key value")
	}

	view := decodeJSON[configViewResponse](t, result)
	if !view.APIKeys["GROQ_API_KEY"] {
		t.Error("GROQ_API_KEY not reported as set")
	}
	if view.APIKeys["OPENAI_API_KEY"] {
		t.Error("OPENAI_API_KEY still reported as set after removal")
	}
	if view.ResourceVersion == "1" {
		t.Error("resourceVersion was not advanced after the write")
	}

	stored := storedAPIKeys(t, env)
	if stored["GROQ_API_KEY"] != "gsk-new-secret" {
		t.Errorf("stored GROQ_API_KEY = %q", stored["GROQ_API_KEY"])
	}
	if stored["ANTHROPIC_API_KEY"] != testAPIKeyValue {
		t.Errorf("stored ANTHROPIC_API_KEY = %q, want untouched original", stored["ANTHROPIC_API_KEY"])
	}
	if _, present := stored["OPENAI_API_KEY"]; present {
		t.Error("OPENAI_API_KEY still stored after removal")
	}
	if authorized, ok := env.fake.secretValue("authorized_keys"); !ok || authorized == "" {
		t.Error("authorized_keys was modified by an API key update")
	}
}

func TestPostConfigPreservesUnknownStoredKeys(t *testing.T) {
	env := newPortalTestEnv(t)
	env.fake.setSecretValue("api-keys.json", `{"ANTHROPIC_API_KEY":"keep","LEGACY_CUSTOM_KEY":"keepme"}`)
	body := map[string]any{
		"resourceVersion": env.fake.secretResourceVersion(),
		"apiKeys":         map[string]any{"set": map[string]string{"GROQ_API_KEY": "gsk"}},
	}
	if result := env.postConfig(body); result.Status != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", result.Status, result.Body)
	}
	stored := storedAPIKeys(t, env)
	if stored["LEGACY_CUSTOM_KEY"] != "keepme" {
		t.Errorf("unknown stored key was dropped: %+v", stored)
	}
	if stored["GROQ_API_KEY"] != "gsk" {
		t.Errorf("new key not stored: %+v", stored)
	}
}

func TestPostConfigRejectsUnknownAPIKeys(t *testing.T) {
	env := newPortalTestEnv(t)
	env.fake.forgetRequests()

	cases := map[string]map[string]any{
		"set": {
			"resourceVersion": "1",
			"apiKeys":         map[string]any{"set": map[string]string{"PATH": "/usr/bin"}},
		},
		"remove": {
			"resourceVersion": "1",
			"apiKeys":         map[string]any{"remove": []string{"LD_PRELOAD"}},
		},
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			result := env.postConfig(body)
			if result.Status != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400 (body %s)", result.Status, result.Body)
			}
			if !strings.Contains(string(result.Body), "not allowed") {
				t.Errorf("error body = %s, want not allowed", result.Body)
			}
		})
	}
	if count := env.fake.countRequests(http.MethodPatch, env.fake.secretPath()); count != 0 {
		t.Fatalf("rejected keys caused %d secret patches, want 0", count)
	}
}

func TestPostConfigRequiresResourceVersion(t *testing.T) {
	env := newPortalTestEnv(t)
	body := map[string]any{
		"apiKeys": map[string]any{"set": map[string]string{"GROQ_API_KEY": "gsk"}},
	}
	result := env.postConfig(body)
	if result.Status != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (body %s)", result.Status, result.Body)
	}
	if !strings.Contains(string(result.Body), "resourceVersion") {
		t.Errorf("error body = %s, want resourceVersion mention", result.Body)
	}
}

func TestPostConfigRejectsEmptyUpdate(t *testing.T) {
	env := newPortalTestEnv(t)
	if result := env.postConfig(map[string]any{"resourceVersion": "1"}); result.Status != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (body %s)", result.Status, result.Body)
	}
	if result := env.postConfig(map[string]any{
		"resourceVersion": "1",
		"apiKeys":         map[string]any{},
	}); result.Status != http.StatusBadRequest {
		t.Fatalf("empty apiKeys status = %d, want 400 (body %s)", result.Status, result.Body)
	}
	body := map[string]any{
		"resourceVersion": "1",
		"apiKeys":         map[string]any{"set": map[string]string{"GROQ_API_KEY": "gsk"}, "remove": []string{"GROQ_API_KEY"}},
	}
	if result := env.postConfig(body); result.Status != http.StatusBadRequest {
		t.Fatalf("set and remove same key status = %d, want 400 (body %s)", result.Status, result.Body)
	}
}

func TestPostConfigConflictsOnStaleResourceVersion(t *testing.T) {
	env := newPortalTestEnv(t)
	before := storedAPIKeys(t, env)
	body := map[string]any{
		"resourceVersion": "999",
		"apiKeys":         map[string]any{"set": map[string]string{"GROQ_API_KEY": "gsk"}},
	}
	result := env.postConfig(body)
	if result.Status != http.StatusConflict {
		t.Fatalf("status = %d, want 409 (body %s)", result.Status, result.Body)
	}
	if !strings.Contains(string(result.Body), "reload") {
		t.Errorf("error body = %s, want reload hint", result.Body)
	}
	after := storedAPIKeys(t, env)
	if len(before) != len(after) {
		t.Fatalf("stale write changed stored keys: before %v after %v", before, after)
	}
}

func TestPostConfigConflictsWhenSecretChangesMidWrite(t *testing.T) {
	env := newPortalTestEnv(t)
	before := storedAPIKeys(t, env)
	env.fake.beforeNextSecretPatch(func(f *fakeKube) {
		// Simulate another writer winning between the portal's read and write.
		f.bumpRV(f.secret)
	})
	body := map[string]any{
		"resourceVersion": "1",
		"apiKeys":         map[string]any{"set": map[string]string{"GROQ_API_KEY": "gsk"}},
	}
	result := env.postConfig(body)
	if result.Status != http.StatusConflict {
		t.Fatalf("status = %d, want 409 (body %s)", result.Status, result.Body)
	}
	after := storedAPIKeys(t, env)
	if len(before) != len(after) {
		t.Fatalf("conflicting write changed stored keys: before %v after %v", before, after)
	}
}

func TestPostConfigValidatesAPIKeyValues(t *testing.T) {
	env := newPortalTestEnv(t)
	cases := map[string]string{
		"empty":      "",
		"whitespace": "   ",
		"padded":     " gsk",
		"non-ascii":  "caf\u00e9-key",
		"control":    "gsk\x01",
		"very long":  strings.Repeat("a", 4097),
	}
	for name, value := range cases {
		t.Run(name, func(t *testing.T) {
			body := map[string]any{
				"resourceVersion": env.fake.secretResourceVersion(),
				"apiKeys":         map[string]any{"set": map[string]string{"GROQ_API_KEY": value}},
			}
			result := env.postConfig(body)
			if result.Status != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400 (body %s)", result.Status, result.Body)
			}
		})
	}
}

func TestPostConfigAuthorizedKeys(t *testing.T) {
	env := newPortalTestEnv(t)
	valid := testAuthorizedKeys(t) + "\n" + testAuthorizedKeys(t) + " second@example.com\n"
	body := map[string]any{"resourceVersion": env.fake.secretResourceVersion(), "authorizedKeys": valid}
	result := env.postConfig(body)
	if result.Status != http.StatusOK {
		t.Fatalf("valid keys status = %d, want 200 (body %s)", result.Status, result.Body)
	}
	stored, ok := env.fake.secretValue("authorized_keys")
	if !ok || stored != valid {
		t.Errorf("stored authorized_keys = %q, want %q", stored, valid)
	}

	// Built by concatenation so secret scanners do not flag this test fixture.
	private := "-----BEGIN " + "OPENSSH PRIVATE KEY-----\nb3BlbnNzaC1rZXktdjEAAAAABG5vbmUAAAAEbm9uZQ==\n-----END " + "OPENSSH PRIVATE KEY-----\n"
	body = map[string]any{"resourceVersion": env.fake.secretResourceVersion(), "authorizedKeys": private}
	result = env.postConfig(body)
	if result.Status != http.StatusBadRequest {
		t.Fatalf("private key status = %d, want 400 (body %s)", result.Status, result.Body)
	}
	if !strings.Contains(string(result.Body), "private keys") {
		t.Errorf("error body = %s, want private key rejection", result.Body)
	}

	body = map[string]any{"resourceVersion": env.fake.secretResourceVersion(), "authorizedKeys": "not a key at all\n"}
	result = env.postConfig(body)
	if result.Status != http.StatusBadRequest {
		t.Fatalf("garbage key status = %d, want 400 (body %s)", result.Status, result.Body)
	}
	if !strings.Contains(string(result.Body), "line 1") {
		t.Errorf("error body = %s, want line number", result.Body)
	}

	stored, _ = env.fake.secretValue("authorized_keys")
	if stored != valid {
		t.Errorf("rejected inputs changed stored authorized_keys: %q", stored)
	}

	body = map[string]any{
		"resourceVersion": env.fake.secretResourceVersion(),
		"authorizedKeys":  strings.Repeat(strings.Repeat("k", 100)+"\n", 400),
	}
	if result := env.postConfig(body); result.Status != http.StatusBadRequest {
		t.Fatalf("oversized keys status = %d, want 400 (body %s)", result.Status, result.Body)
	}
}

func TestPostConfigKnownHosts(t *testing.T) {
	env := newPortalTestEnv(t)
	valid := "pocket.example.com ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIPLACEHOLDER\n"
	body := map[string]any{"resourceVersion": env.fake.secretResourceVersion(), "knownHosts": valid}
	if result := env.postConfig(body); result.Status != http.StatusOK {
		t.Fatalf("valid known_hosts status = %d, want 200 (body %s)", result.Status, result.Body)
	}
	stored, ok := env.fake.secretValue("known_hosts")
	if !ok || stored != valid {
		t.Errorf("stored known_hosts = %q, want %q", stored, valid)
	}

	body = map[string]any{"resourceVersion": env.fake.secretResourceVersion(), "knownHosts": "host\x00name"}
	if result := env.postConfig(body); result.Status != http.StatusBadRequest {
		t.Fatalf("NUL known_hosts status = %d, want 400 (body %s)", result.Status, result.Body)
	}

	body = map[string]any{
		"resourceVersion": env.fake.secretResourceVersion(),
		"knownHosts":      strings.Repeat("host ", 20000),
	}
	if result := env.postConfig(body); result.Status != http.StatusBadRequest {
		t.Fatalf("oversized known_hosts status = %d, want 400 (body %s)", result.Status, result.Body)
	}

	// Built by concatenation so secret scanners do not flag this test fixture.
	private := "-----BEGIN " + "RSA PRIVATE KEY-----"
	body = map[string]any{"resourceVersion": env.fake.secretResourceVersion(), "knownHosts": private}
	if result := env.postConfig(body); result.Status != http.StatusBadRequest {
		t.Fatalf("private key in known_hosts status = %d, want 400 (body %s)", result.Status, result.Body)
	}
}

func TestPostConfigBodyValidation(t *testing.T) {
	env := newPortalTestEnv(t)
	huge := `{"resourceVersion":"1","knownHosts":"` + strings.Repeat("a", maxTestBodyBytes) + `"}`
	if result := env.postConfig(huge); result.Status != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized body status = %d, want 413 (body %s)", result.Status, result.Body)
	}
	for name, body := range map[string]string{
		"unknown field": `{"resourceVersion":"1","bogus":true}`,
		"malformed":     `{"resourceVersion":`,
		"array":         `[]`,
		"trailing data": `{"resourceVersion":"1","knownHosts":""} {}`,
	} {
		t.Run(name, func(t *testing.T) {
			result := env.postConfig(body)
			if result.Status != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400 (body %s)", result.Status, result.Body)
			}
		})
	}
}

const maxTestBodyBytes = 1 << 20

func TestPostConfigReportsFailureWhenSaveFails(t *testing.T) {
	env := newPortalTestEnv(t)
	env.fake.failNextSecretPatch(http.StatusInternalServerError, "boom")
	body := map[string]any{
		"resourceVersion": "1",
		"apiKeys":         map[string]any{"set": map[string]string{"GROQ_API_KEY": "gsk"}},
	}
	result := env.postConfig(body)
	if result.Status != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502 (body %s)", result.Status, result.Body)
	}
	if stored := storedAPIKeys(t, env); stored["GROQ_API_KEY"] != "" {
		t.Error("failed save reported changes that were never stored")
	}
}

func storedAPIKeys(t *testing.T, env *portalTestEnv) map[string]string {
	t.Helper()
	raw, ok := env.fake.secretValue("api-keys.json")
	if !ok {
		t.Fatal("api-keys.json is not present in the fake secret")
	}
	keys := map[string]string{}
	if err := json.Unmarshal([]byte(raw), &keys); err != nil {
		t.Fatalf("decode stored api-keys.json %q: %v", raw, err)
	}
	return keys
}
