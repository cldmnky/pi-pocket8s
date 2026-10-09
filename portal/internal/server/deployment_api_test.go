package server_test

import (
	"net/http"
	"testing"
	"time"
)

func TestStatusEndpoint(t *testing.T) {
	env := newPortalTestEnv(t)
	result := env.request(http.MethodGet, "/api/status", testPortalToken, "", nil)
	if result.Status != http.StatusOK {
		t.Fatalf("GET /api/status status = %d, want 200 (body %s)", result.Status, result.Body)
	}
	status := decodeJSON[statusResponse](t, result)
	if status.Namespace != testNamespace || status.Deployment != testDeploymentName {
		t.Errorf("identity = %s/%s, want %s/%s", status.Namespace, status.Deployment, testNamespace, testDeploymentName)
	}
	if !status.Running {
		t.Error("running = false, want true")
	}
	if status.DesiredReplicas != 1 || status.ReadyReplicas != 1 || status.AvailableReplicas != 1 || status.UpdatedReplicas != 1 {
		t.Errorf("replica counts = %+v, want all 1", status)
	}
	if status.PocketURL != "https://pi.example.com" {
		t.Errorf("pocketUrl = %q", status.PocketURL)
	}
	if status.TerminalURL != "https://terminal.example.com" {
		t.Errorf("terminalUrl = %q", status.TerminalURL)
	}
	if status.RestartedAt != "" {
		t.Errorf("restartedAt = %q, want empty before any restart", status.RestartedAt)
	}
}

func TestStopAndStartScaleDeployment(t *testing.T) {
	env := newPortalTestEnv(t)

	stop := env.request(http.MethodPost, "/api/deployment/stop", testPortalToken, testOrigin, nil)
	if stop.Status != http.StatusOK {
		t.Fatalf("stop status = %d, want 200 (body %s)", stop.Status, stop.Body)
	}
	stopped := decodeJSON[statusResponse](t, stop)
	if stopped.Running || stopped.DesiredReplicas != 0 {
		t.Errorf("stopped status = %+v, want running=false desired=0", stopped)
	}
	if replicas := env.fake.scaleReplicas(); replicas != 0 {
		t.Errorf("scale replicas = %d, want 0", replicas)
	}
	if replicas := env.fake.deploymentReplicas(); replicas != 0 {
		t.Errorf("deployment replicas = %d, want 0", replicas)
	}

	start := env.request(http.MethodPost, "/api/deployment/start", testPortalToken, testOrigin, nil)
	if start.Status != http.StatusOK {
		t.Fatalf("start status = %d, want 200 (body %s)", start.Status, start.Body)
	}
	started := decodeJSON[statusResponse](t, start)
	if !started.Running || started.DesiredReplicas != 1 {
		t.Errorf("started status = %+v, want running=true desired=1", started)
	}
	if replicas := env.fake.scaleReplicas(); replicas != 1 {
		t.Errorf("scale replicas = %d, want 1", replicas)
	}
}

func TestStartWhenAlreadyRunningSkipsScaleWrite(t *testing.T) {
	env := newPortalTestEnv(t)
	env.fake.forgetRequests()
	result := env.request(http.MethodPost, "/api/deployment/start", testPortalToken, testOrigin, nil)
	if result.Status != http.StatusOK {
		t.Fatalf("start status = %d, want 200 (body %s)", result.Status, result.Body)
	}
	if count := env.fake.countRequests(http.MethodPut, env.fake.scalePath()); count != 0 {
		t.Fatalf("scale writes = %d, want 0 for an already-running deployment", count)
	}
}

func TestRestartPatchesTemplateAnnotation(t *testing.T) {
	env := newPortalTestEnv(t)
	result := env.request(http.MethodPost, "/api/deployment/restart", testPortalToken, testOrigin, nil)
	if result.Status != http.StatusOK {
		t.Fatalf("restart status = %d, want 200 (body %s)", result.Status, result.Body)
	}
	status := decodeJSON[statusResponse](t, result)
	if status.RestartedAt == "" {
		t.Fatal("restartedAt is empty after restart")
	}
	parsed, err := time.Parse(time.RFC3339, status.RestartedAt)
	if err != nil {
		t.Fatalf("restartedAt = %q is not RFC3339: %v", status.RestartedAt, err)
	}
	if since := time.Since(parsed); since < 0 || since > time.Minute {
		t.Errorf("restartedAt = %s, want a recent timestamp", parsed)
	}
	stored, ok := env.fake.restartAnnotation()
	if !ok || stored != status.RestartedAt {
		t.Errorf("stored template annotation = %q (present %v), want %q", stored, ok, status.RestartedAt)
	}
	if count := env.fake.countRequests(http.MethodPatch, env.fake.deploymentPath()); count != 1 {
		t.Errorf("deployment patches = %d, want 1", count)
	}
	if stored := env.fake.scaleReplicas(); stored != 1 {
		t.Errorf("restart changed replicas to %d, want 1", stored)
	}
}

func TestScaleConflictIsRetried(t *testing.T) {
	env := newPortalTestEnv(t)
	env.fake.forgetRequests()
	env.fake.failNextScaleUpdateConflict()

	result := env.request(http.MethodPost, "/api/deployment/stop", testPortalToken, testOrigin, nil)
	if result.Status != http.StatusOK {
		t.Fatalf("stop after conflict status = %d, want 200 (body %s)", result.Status, result.Body)
	}
	if replicas := env.fake.scaleReplicas(); replicas != 0 {
		t.Errorf("scale replicas = %d, want 0", replicas)
	}
	if count := env.fake.countRequests(http.MethodPut, env.fake.scalePath()); count != 2 {
		t.Errorf("scale writes = %d, want 2 (initial conflict plus retry)", count)
	}
}
