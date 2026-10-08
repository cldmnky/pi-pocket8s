package server_test

import (
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"sync"
)

// fakeKube is a minimal in-memory Kubernetes API server implementing exactly
// the endpoints the portal uses: one Secret, one Deployment, and the scale
// subresource. It enforces resourceVersion checks on every write.
type fakeKube struct {
	mu sync.Mutex

	namespace      string
	secretName     string
	deploymentName string

	secret     map[string]any
	deployment map[string]any
	scale      map[string]any

	nextRV int64

	seenSATokens []string
	requests     []string

	// Test hooks. All are cleared after use.
	failNextScaleUpdate     bool
	secretPatchFailStatus   int
	secretPatchFailBody     string
	deploymentGetFailStatus int
	deploymentGetFailBody   string
	beforeSecretPatch       func(*fakeKube)
}

func newFakeKube(namespace, secretName, deploymentName, apiKeysJSON, authorizedKeys, knownHosts string) *fakeKube {
	f := &fakeKube{
		namespace:      namespace,
		secretName:     secretName,
		deploymentName: deploymentName,
		nextRV:         1,
	}
	data := map[string]any{}
	if apiKeysJSON != "" {
		data["api-keys.json"] = base64.StdEncoding.EncodeToString([]byte(apiKeysJSON))
	}
	if authorizedKeys != "" {
		data["authorized_keys"] = base64.StdEncoding.EncodeToString([]byte(authorizedKeys))
	}
	if knownHosts != "" {
		data["known_hosts"] = base64.StdEncoding.EncodeToString([]byte(knownHosts))
	}
	f.secret = map[string]any{
		"apiVersion": "v1",
		"kind":       "Secret",
		"metadata":   map[string]any{"name": secretName, "namespace": namespace, "resourceVersion": "1"},
		"type":       "Opaque",
		"data":       data,
	}
	f.deployment = map[string]any{
		"apiVersion": "apps/v1",
		"kind":       "Deployment",
		"metadata":   map[string]any{"name": deploymentName, "namespace": namespace, "resourceVersion": "1"},
		"spec": map[string]any{
			"replicas": float64(1),
			"template": map[string]any{"metadata": map[string]any{"annotations": map[string]any{}}},
		},
		"status": map[string]any{
			"replicas":          float64(1),
			"readyReplicas":     float64(1),
			"availableReplicas": float64(1),
			"updatedReplicas":   float64(1),
		},
	}
	f.scale = map[string]any{
		"apiVersion": "autoscaling/v1",
		"kind":       "Scale",
		"metadata":   map[string]any{"name": deploymentName, "namespace": namespace, "resourceVersion": "1"},
		"spec":       map[string]any{"replicas": float64(1)},
		"status":     map[string]any{"replicas": float64(1)},
	}
	return f
}

// failNextScaleUpdateConflict makes the next scale write fail with 409.
func (f *fakeKube) failNextScaleUpdateConflict() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failNextScaleUpdate = true
}

// failNextSecretPatch makes the next secret patch fail with the given status.
func (f *fakeKube) failNextSecretPatch(status int, body string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.secretPatchFailStatus = status
	f.secretPatchFailBody = body
}

// failNextDeploymentGet makes the next deployment read fail.
func (f *fakeKube) failNextDeploymentGet(status int, body string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deploymentGetFailStatus = status
	f.deploymentGetFailBody = body
}

// beforeNextSecretPatch runs fn while handling the next secret patch, with the
// fake's lock held. Tests use it to simulate a concurrent writer.
func (f *fakeKube) beforeNextSecretPatch(fn func(*fakeKube)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.beforeSecretPatch = fn
}

func (f *fakeKube) secretPath() string {
	return "/api/v1/namespaces/" + f.namespace + "/secrets/" + f.secretName
}

func (f *fakeKube) deploymentPath() string {
	return "/apis/apps/v1/namespaces/" + f.namespace + "/deployments/" + f.deploymentName
}

func (f *fakeKube) scalePath() string {
	return f.deploymentPath() + "/scale"
}

func (f *fakeKube) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.seenSATokens = append(f.seenSATokens, r.Header.Get("Authorization"))
	f.requests = append(f.requests, r.Method+" "+r.URL.Path)

	switch {
	case r.Method == http.MethodGet && r.URL.Path == f.secretPath():
		f.writeJSON(w, http.StatusOK, f.secret)
	case r.Method == http.MethodPatch && r.URL.Path == f.secretPath():
		if f.beforeSecretPatch != nil {
			hook := f.beforeSecretPatch
			f.beforeSecretPatch = nil
			hook(f)
		}
		if f.secretPatchFailStatus != 0 {
			status, body := f.secretPatchFailStatus, f.secretPatchFailBody
			f.secretPatchFailStatus, f.secretPatchFailBody = 0, ""
			f.writeStatus(w, status, body)
			return
		}
		if merged := f.applyPatch(w, r, f.secret); merged != nil {
			f.secret = merged
		}
	case r.Method == http.MethodGet && r.URL.Path == f.deploymentPath():
		if f.deploymentGetFailStatus != 0 {
			status, body := f.deploymentGetFailStatus, f.deploymentGetFailBody
			f.deploymentGetFailStatus, f.deploymentGetFailBody = 0, ""
			f.writeStatus(w, status, body)
			return
		}
		f.writeJSON(w, http.StatusOK, f.deployment)
	case r.Method == http.MethodPatch && r.URL.Path == f.deploymentPath():
		if merged := f.applyPatch(w, r, f.deployment); merged != nil {
			f.deployment = merged
		}
	case r.Method == http.MethodGet && r.URL.Path == f.scalePath():
		f.writeJSON(w, http.StatusOK, f.scale)
	case r.Method == http.MethodPut && r.URL.Path == f.scalePath():
		f.putScale(w, r)
	default:
		f.writeStatus(w, http.StatusNotFound, "the server could not find the requested resource")
	}
}

func (f *fakeKube) applyPatch(w http.ResponseWriter, r *http.Request, target map[string]any) map[string]any {
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil || len(body) == 0 {
		f.writeStatus(w, http.StatusBadRequest, "empty patch")
		return nil
	}
	var patch map[string]any
	if err := json.Unmarshal(body, &patch); err != nil {
		f.writeStatus(w, http.StatusBadRequest, "invalid patch")
		return nil
	}
	current := childString(childMap(target, "metadata"), "resourceVersion")
	patchRV := childString(childMap(patch, "metadata"), "resourceVersion")
	// resourceVersion is optional in a merge patch; when present it must match.
	if patchRV != "" && patchRV != current {
		f.writeStatus(w, http.StatusConflict, "Operation cannot be fulfilled: the object has been modified")
		return nil
	}
	merged := mergeMaps(target, patch)
	f.bumpRV(merged)
	f.writeJSON(w, http.StatusOK, merged)
	return merged
}

func (f *fakeKube) putScale(w http.ResponseWriter, r *http.Request) {
	var incoming map[string]any
	if err := json.NewDecoder(r.Body).Decode(&incoming); err != nil {
		f.writeStatus(w, http.StatusBadRequest, "invalid scale object")
		return
	}
	if f.failNextScaleUpdate {
		f.failNextScaleUpdate = false
		f.writeStatus(w, http.StatusConflict, "Operation cannot be fulfilled: the object has been modified")
		return
	}
	if childString(childMap(incoming, "metadata"), "resourceVersion") != childString(childMap(f.scale, "metadata"), "resourceVersion") {
		f.writeStatus(w, http.StatusConflict, "Operation cannot be fulfilled: the object has been modified")
		return
	}
	replicas := childNumber(childMap(incoming, "spec"), "replicas")
	f.scale["spec"] = map[string]any{"replicas": replicas}
	f.scale["status"] = map[string]any{"replicas": replicas}
	f.bumpRV(f.scale)

	spec := childMap(f.deployment, "spec")
	spec["replicas"] = replicas
	status := childMap(f.deployment, "status")
	status["replicas"] = replicas
	status["readyReplicas"] = replicas
	status["availableReplicas"] = replicas
	status["updatedReplicas"] = replicas
	f.bumpRV(f.deployment)

	f.writeJSON(w, http.StatusOK, f.scale)
}

func (f *fakeKube) bumpRV(object map[string]any) {
	f.nextRV++
	childMap(object, "metadata")["resourceVersion"] = strconv.FormatInt(f.nextRV, 10)
}

func (f *fakeKube) writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func (f *fakeKube) writeStatus(w http.ResponseWriter, status int, message string) {
	f.writeJSON(w, status, map[string]any{
		"kind":       "Status",
		"apiVersion": "v1",
		"status":     "Failure",
		"message":    message,
		"code":       status,
	})
}

// setSecretValue stores a plaintext value under key, bumping the secret's
// resourceVersion so portal writes against an older version conflict.
func (f *fakeKube) setSecretValue(key, value string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	data := childMap(f.secret, "data")
	data[key] = base64.StdEncoding.EncodeToString([]byte(value))
	f.bumpRV(f.secret)
}

// secretValue returns the decoded plaintext stored under key.
func (f *fakeKube) secretValue(key string) (string, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	data, _ := f.secret["data"].(map[string]any)
	encoded, ok := data[key].(string)
	if !ok {
		return "", false
	}
	decoded, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return "", false
	}
	return string(decoded), true
}

// secretResourceVersion returns the current resourceVersion of the Secret.
func (f *fakeKube) secretResourceVersion() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return childString(childMap(f.secret, "metadata"), "resourceVersion")
}

// scaleReplicas returns the replica count currently stored in the scale object.
func (f *fakeKube) scaleReplicas() int32 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return int32(childNumber(childMap(f.scale, "spec"), "replicas"))
}

// deploymentReplicas returns the replica count currently stored in the deployment.
func (f *fakeKube) deploymentReplicas() int32 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return int32(childNumber(childMap(f.deployment, "spec"), "replicas"))
}

// restartAnnotation returns the stored template annotation value, if any.
func (f *fakeKube) restartAnnotation() (string, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	annotations := childMap(childMap(childMap(childMap(f.deployment, "spec"), "template"), "metadata"), "annotations")
	value, ok := annotations["kubectl.kubernetes.io/restartedAt"].(string)
	return value, ok
}

// countRequests counts recorded requests with the given method and path.
func (f *fakeKube) countRequests(method, path string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	count := 0
	for _, request := range f.requests {
		if request == method+" "+path {
			count++
		}
	}
	return count
}

// lastSAToken returns the Authorization header of the most recent request.
func (f *fakeKube) lastSAToken() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.seenSATokens) == 0 {
		return ""
	}
	return f.seenSATokens[len(f.seenSATokens)-1]
}

// forgetRequests clears the request log and seen tokens.
func (f *fakeKube) forgetRequests() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests = nil
	f.seenSATokens = nil
}

func childMap(m map[string]any, key string) map[string]any {
	if child, ok := m[key].(map[string]any); ok {
		return child
	}
	child := map[string]any{}
	m[key] = child
	return child
}

func childString(m map[string]any, key string) string {
	value, _ := m[key].(string)
	return value
}

func childNumber(m map[string]any, key string) float64 {
	switch value := m[key].(type) {
	case float64:
		return value
	case int:
		return float64(value)
	case json.Number:
		number, _ := value.Float64()
		return number
	}
	return 0
}

// mergeMaps applies a JSON merge patch (RFC 7386) to dst.
func mergeMaps(dst, patch map[string]any) map[string]any {
	merged := make(map[string]any, len(dst)+len(patch))
	for key, value := range dst {
		merged[key] = value
	}
	for key, value := range patch {
		if value == nil {
			delete(merged, key)
			continue
		}
		if patchChild, ok := value.(map[string]any); ok {
			if dstChild, ok := merged[key].(map[string]any); ok {
				merged[key] = mergeMaps(dstChild, patchChild)
				continue
			}
		}
		merged[key] = value
	}
	return merged
}
