package admin

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cldmnky/pi-pocket8s/portal/internal/kube"
)

// fakeStore is an in-memory session store with an optimistic resource version.
type fakeStore struct {
	mu     sync.Mutex
	state  State
	saves  int
	failed error
}

func newFakeStore() *fakeStore {
	return &fakeStore{state: State{ResourceVersion: "1", Record: Record{ObservedPhase: PhaseIdle, RequestedState: RequestedNone}}}
}

func (f *fakeStore) Load(context.Context) (State, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.state, nil
}

func (f *fakeStore) Save(_ context.Context, state State) (State, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failed != nil {
		err := f.failed
		f.failed = nil
		return State{}, err
	}
	f.saves++
	f.state = State{ResourceVersion: fmt.Sprintf("rv-%d", f.saves), Record: state.Record}
	return f.state, nil
}

func (f *fakeStore) set(record Record) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.state = State{ResourceVersion: fmt.Sprintf("rv-%d", f.saves), Record: record}
}

func (f *fakeStore) record() Record {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.state.Record
}

// fakeOps is an in-memory Kubernetes API for the fixed elevation objects.
type fakeOps struct {
	deployment  kube.Deployment
	scale       kube.Scale
	binding     kube.ClusterRoleBinding
	pods        []kube.Pod
	runtimeData map[string]string

	// Pod simulation: scaling to one starts a Pod (ready or not) and publishes
	// its sign-in metadata; scaling to zero removes it unless stuckPods is set.
	nextPodUID      string
	podReady        bool
	publishMetadata bool
	stuckPods       bool

	failDeploymentGet bool
	failPodList       bool
	failScaleUpdate   bool
	failBindingPatch  bool
	failSecretPatch   bool
	scaleUpdates      int
	bindingPatches    int
	secretPatches     int
}

func newFakeOps() *fakeOps {
	selector := map[string]string{"app": "pi-pocket-admin"}
	ops := &fakeOps{}
	ops.deployment = kube.Deployment{
		Metadata: kube.ObjectMeta{Name: "pi-pocket-admin", Namespace: "pi-pocket-admin", UID: "deployment-uid"},
		Spec:     kube.DeploymentSpec{Selector: &kube.LabelSelector{MatchLabels: selector}},
	}
	ops.scale = kube.Scale{
		Metadata: kube.ObjectMeta{Name: "pi-pocket-admin", Namespace: "pi-pocket-admin", ResourceVersion: "1"},
		Spec:     kube.ScaleSpec{Replicas: 0},
	}
	ops.binding = kube.ClusterRoleBinding{
		Metadata: kube.ObjectMeta{Name: "pi-pocket-admin-cluster-admin", ResourceVersion: "1"},
		RoleRef:  kube.RoleRef{APIGroup: "rbac.authorization.k8s.io", Kind: "ClusterRole", Name: "cluster-admin"},
		Subjects: []kube.Subject{},
	}
	ops.runtimeData = map[string]string{}
	ops.nextPodUID = "pod-uid-1"
	ops.podReady = true
	ops.publishMetadata = true
	return ops
}

func (f *fakeOps) GetDeployment(context.Context, string, string) (*kube.Deployment, error) {
	if f.failDeploymentGet {
		return nil, errors.New("deployment unavailable")
	}
	copy := f.deployment
	return &copy, nil
}

func (f *fakeOps) GetDeploymentScale(context.Context, string, string) (*kube.Scale, error) {
	copy := f.scale
	return &copy, nil
}

func (f *fakeOps) UpdateDeploymentScale(_ context.Context, _, _ string, scale *kube.Scale) (*kube.Scale, error) {
	if f.failScaleUpdate {
		return nil, errors.New("scale update failed")
	}
	f.scaleUpdates++
	f.scale.Spec.Replicas = scale.Spec.Replicas
	switch {
	case scale.Spec.Replicas == 1 && len(f.pods) == 0:
		status := kube.PodStatus{Phase: "Running", Conditions: []kube.PodCondition{{Type: "Ready", Status: "False"}}}
		if f.podReady {
			status.Conditions[0].Status = "True"
		}
		f.pods = []kube.Pod{{
			Metadata: kube.ObjectMeta{Name: "pi-pocket-admin-" + f.nextPodUID, Namespace: "pi-pocket-admin", UID: f.nextPodUID, Labels: map[string]string{"app": "pi-pocket-admin"}},
			Status:   status,
		}}
		if f.publishMetadata {
			f.runtimeData[kube.OwnerLoginURLKey] = "https://pocket-admin.example.com/login?token=owner-secret"
			f.runtimeData[kube.OwnerLoginPodUIDKey] = f.nextPodUID
		}
	case scale.Spec.Replicas == 0 && !f.stuckPods:
		f.pods = nil
	}
	copy := f.scale
	return &copy, nil
}

func (f *fakeOps) GetClusterRoleBinding(context.Context, string) (*kube.ClusterRoleBinding, error) {
	copy := f.binding
	copy.Subjects = append([]kube.Subject{}, f.binding.Subjects...)
	return &copy, nil
}

func (f *fakeOps) PatchClusterRoleBinding(_ context.Context, _ string, patch []byte) (*kube.ClusterRoleBinding, error) {
	if f.failBindingPatch {
		return nil, errors.New("binding patch failed")
	}
	var payload struct {
		Subjects []kube.Subject `json:"subjects"`
	}
	if err := json.Unmarshal(patch, &payload); err != nil {
		return nil, err
	}
	f.bindingPatches++
	f.binding.Subjects = payload.Subjects
	copy := f.binding
	return &copy, nil
}

func (f *fakeOps) ListPods(context.Context, string, string) (*kube.PodList, error) {
	if f.failPodList {
		return nil, errors.New("pod list failed")
	}
	return &kube.PodList{Items: append([]kube.Pod{}, f.pods...)}, nil
}

func (f *fakeOps) GetSecret(context.Context, string, string) (*kube.Secret, error) {
	secret := &kube.Secret{Metadata: kube.ObjectMeta{Name: "pi-pocket-admin-runtime", ResourceVersion: "1"}, Data: map[string]string{}}
	for key, value := range f.runtimeData {
		secret.Data[key] = base64.StdEncoding.EncodeToString([]byte(value))
	}
	return secret, nil
}

func (f *fakeOps) PatchSecret(_ context.Context, _, _ string, patch []byte) (*kube.Secret, error) {
	if f.failSecretPatch {
		return nil, errors.New("secret patch failed")
	}
	var payload struct {
		Data map[string]*string `json:"data"`
	}
	if err := json.Unmarshal(patch, &payload); err != nil {
		return nil, err
	}
	f.secretPatches++
	for key, value := range payload.Data {
		if value == nil {
			delete(f.runtimeData, key)
			continue
		}
		decoded, err := base64.StdEncoding.DecodeString(*value)
		if err != nil {
			return nil, err
		}
		f.runtimeData[key] = string(decoded)
	}
	return f.GetSecret(context.Background(), "", "")
}

func (f *fakeOps) readyWorkspace(podUID string) {
	f.scale.Spec.Replicas = 1
	f.pods = []kube.Pod{{
		Metadata: kube.ObjectMeta{Name: "pi-pocket-admin-abc", Namespace: "pi-pocket-admin", UID: podUID, Labels: map[string]string{"app": "pi-pocket-admin"}},
		Status:   kube.PodStatus{Phase: "Running", Conditions: []kube.PodCondition{{Type: "Ready", Status: "True"}}},
	}}
	f.runtimeData[kube.OwnerLoginURLKey] = "https://pocket-admin.example.com/login?token=secret"
	f.runtimeData[kube.OwnerLoginPodUIDKey] = podUID
}

type fixture struct {
	reconciler *Reconciler
	store      *fakeStore
	ops        *fakeOps
}

func newFixture(t *testing.T, timeout time.Duration) *fixture {
	t.Helper()
	store := newFakeStore()
	ops := newFakeOps()
	reconciler := NewReconciler(Config{
		Namespace:          "pi-pocket-admin",
		Deployment:         "pi-pocket-admin",
		ServiceAccount:     "pi-pocket-admin",
		RuntimeSecret:      "pi-pocket-admin-runtime",
		ClusterRoleBinding: "pi-pocket-admin-cluster-admin",
		ClusterRole:        "cluster-admin",
		StartupTimeout:     timeout,
	}, store, ops, nil)
	reconciler.pollInterval = 2 * time.Millisecond
	return &fixture{reconciler: reconciler, store: store, ops: ops}
}

func (f *fixture) approve(duration time.Duration) {
	record := Record{
		SessionID:        "session-1",
		RequestedState:   RequestedActive,
		ApprovedByUserID: 42,
		ApprovedByLogin:  "operator",
		Reason:           "investigate a failed upgrade",
		ApprovedAt:       time.Now(),
		ExpiresAt:        time.Now().Add(duration),
		ObservedPhase:    PhaseActivationRequested,
	}
	f.store.set(record)
}

func mustReconcile(t *testing.T, f *fixture) Record {
	t.Helper()
	if err := f.reconciler.Reconcile(context.Background()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	return f.store.record()
}

func TestActivationHappyPathAndObservation(t *testing.T) {
	f := newFixture(t, time.Second)
	f.approve(time.Minute)
	record := mustReconcile(t, f)
	if record.ObservedPhase != PhaseActive || !record.GrantObserved || !record.WorkspaceReady {
		t.Fatalf("not active: %s", record.Describe())
	}
	if record.ActivePodUID != "pod-uid-1" || record.AdminDeploymentUID != "deployment-uid" {
		t.Fatalf("observations missing: %s", record.Describe())
	}
	if len(f.ops.binding.Subjects) != 1 || f.ops.binding.Subjects[0].Name != "pi-pocket-admin" {
		t.Fatalf("grant not applied: %+v", f.ops.binding.Subjects)
	}
	if f.ops.scale.Spec.Replicas != 1 {
		t.Fatalf("workspace not started: %d", f.ops.scale.Spec.Replicas)
	}
	// A second pass observes without changing the deadline.
	before := record.ExpiresAt
	record = mustReconcile(t, f)
	if !record.ExpiresAt.Equal(before) {
		t.Fatal("reconcile extended the deadline")
	}
	if record.ObservedPhase != PhaseActive {
		t.Fatalf("observation lost the session: %s", record.Describe())
	}
}

func TestGrantAppliedButScalingFailsRevokes(t *testing.T) {
	f := newFixture(t, 20*time.Millisecond)
	f.approve(time.Minute)
	f.ops.failScaleUpdate = true
	record := mustReconcile(t, f)
	// Revocation runs immediately after a failed activation; the failure stays
	// visible so the operator is not told the session simply ended.
	if record.ObservedPhase != PhaseIdle || record.FailureCode != FailureScaleFailed {
		t.Fatalf("expected idle with scale_failed: %s", record.Describe())
	}
	if len(f.ops.binding.Subjects) != 0 {
		t.Fatal("grant survived a failed activation")
	}
}

func TestWorkspaceNeverReadyTimesOutAndRevokes(t *testing.T) {
	f := newFixture(t, 30*time.Millisecond)
	f.approve(time.Minute)
	f.ops.podReady = false
	record := mustReconcile(t, f)
	if record.ObservedPhase != PhaseIdle || record.FailureCode != FailureWorkspaceTimeout {
		t.Fatalf("expected workspace_timeout: %s", record.Describe())
	}
	if len(f.ops.binding.Subjects) != 0 || f.ops.scale.Spec.Replicas != 0 {
		t.Fatal("failed activation left the grant or the workspace running")
	}
}

func TestExpiryDuringStartupRevokesWithoutExtension(t *testing.T) {
	f := newFixture(t, time.Second)
	f.approve(5 * time.Millisecond)
	time.Sleep(10 * time.Millisecond)
	record := mustReconcile(t, f)
	if record.RequestedState != RequestedNone || record.ObservedPhase != PhaseIdle {
		t.Fatalf("expired session was not revoked: %s", record.Describe())
	}
	if len(f.ops.binding.Subjects) != 0 {
		t.Fatal("expired session kept its grant")
	}
}

func TestControllerRestartResumesWithoutExtendingDeadline(t *testing.T) {
	f := newFixture(t, time.Second)
	// Durable state left behind by a controller that stopped mid-activation:
	// the grant is applied and the workspace is still starting.
	expires := time.Now().Add(time.Minute)
	f.store.set(Record{
		SessionID: "session-1", RequestedState: RequestedActive, ApprovedByUserID: 42, ApprovedByLogin: "operator",
		Reason: "restart during activation", ApprovedAt: time.Now(), ExpiresAt: expires,
		ObservedPhase: PhaseActivating, GrantObserved: true, AdminDeploymentUID: "deployment-uid",
	})
	f.ops.binding.Subjects = []kube.Subject{{Kind: "ServiceAccount", Name: "pi-pocket-admin", Namespace: "pi-pocket-admin"}}
	restarted := NewReconciler(f.reconciler.cfg, f.store, f.ops, nil)
	restarted.pollInterval = 2 * time.Millisecond
	if err := restarted.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	final := f.store.record()
	if final.ObservedPhase != PhaseActive {
		t.Fatalf("restart did not finish activation: %s", final.Describe())
	}
	if !final.ExpiresAt.Equal(expires) {
		t.Fatal("restart extended the deadline")
	}
}

func TestRestartAfterDeadlineRevokes(t *testing.T) {
	f := newFixture(t, time.Second)
	record := Record{
		SessionID: "session-1", RequestedState: RequestedActive, ApprovedByUserID: 42, ApprovedByLogin: "operator",
		Reason: "expired while the controller was down", ApprovedAt: time.Now().Add(-time.Hour),
		ExpiresAt: time.Now().Add(-time.Minute), ObservedPhase: PhaseActive, GrantObserved: true, ActivePodUID: "pod-uid-3",
	}
	f.store.set(record)
	f.ops.binding.Subjects = []kube.Subject{{Kind: "ServiceAccount", Name: "pi-pocket-admin", Namespace: "pi-pocket-admin"}}
	f.ops.readyWorkspace("pod-uid-3")
	final := mustReconcile(t, f)
	if final.ObservedPhase != PhaseIdle {
		t.Fatalf("expired session not cleaned up: %s", final.Describe())
	}
	if len(f.ops.binding.Subjects) != 0 || f.ops.scale.Spec.Replicas != 0 {
		t.Fatal("expired session left the grant or the workspace")
	}
}

func TestRevocationRequestDuringStartupWins(t *testing.T) {
	f := newFixture(t, time.Second)
	f.approve(time.Minute)
	if err := f.reconciler.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	// The operator revokes while the workspace is still starting.
	f.store.set(Record{SessionID: "session-1", RequestedState: RequestedRevoked, ApprovedByUserID: 42, ApprovedByLogin: "operator", Reason: "stop", ApprovedAt: time.Now(), ExpiresAt: time.Now().Add(time.Minute), ObservedPhase: PhaseActivating, GrantObserved: true})
	final := mustReconcile(t, f)
	if final.ObservedPhase != PhaseIdle {
		t.Fatalf("revocation lost: %s", final.Describe())
	}
	if len(f.ops.binding.Subjects) != 0 {
		t.Fatal("revocation left the grant")
	}
}

func TestMalformedStateNeverActivatesAndCleansUp(t *testing.T) {
	store := &failingLoadStore{err: fmt.Errorf("%w: trailing data", ErrInvalidState)}
	ops := newFakeOps()
	ops.binding.Subjects = []kube.Subject{{Kind: "ServiceAccount", Name: "pi-pocket-admin", Namespace: "pi-pocket-admin"}}
	ops.scale.Spec.Replicas = 1
	reconciler := NewReconciler(Config{Namespace: "pi-pocket-admin", Deployment: "pi-pocket-admin", ServiceAccount: "pi-pocket-admin", RuntimeSecret: "pi-pocket-admin-runtime", ClusterRoleBinding: "pi-pocket-admin-cluster-admin", ClusterRole: "cluster-admin", StartupTimeout: time.Second}, store, ops, nil)
	if err := reconciler.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(ops.binding.Subjects) != 0 || ops.scale.Spec.Replicas != 0 {
		t.Fatal("malformed state did not trigger cleanup")
	}
	if store.saved.Record.RequestedState == RequestedActive {
		t.Fatal("malformed state activated a session")
	}
}

type failingLoadStore struct {
	err   error
	saved State
}

func (f *failingLoadStore) Load(context.Context) (State, error) { return State{}, f.err }
func (f *failingLoadStore) Save(_ context.Context, state State) (State, error) {
	f.saved = state
	return state, nil
}

func TestBindingDriftIsNotAdoptedAndEndsTheSession(t *testing.T) {
	f := newFixture(t, time.Second)
	f.store.set(Record{SessionID: "session-1", RequestedState: RequestedActive, ApprovedByUserID: 42, ApprovedByLogin: "operator", Reason: "drift", ApprovedAt: time.Now(), ExpiresAt: time.Now().Add(time.Minute), ObservedPhase: PhaseActive, GrantObserved: true, ActivePodUID: "pod-uid-4"})
	f.ops.readyWorkspace("pod-uid-4")
	// Someone else added a subject: the binding is no longer ours to mutate.
	f.ops.binding.Subjects = []kube.Subject{
		{Kind: "ServiceAccount", Name: "pi-pocket-admin", Namespace: "pi-pocket-admin"},
		{Kind: "ServiceAccount", Name: "attacker", Namespace: "pi-pocket-admin"},
	}
	final := mustReconcile(t, f)
	if final.ObservedPhase != PhaseCleanupRequired || final.FailureCode != FailureBindingNotOwned {
		t.Fatalf("drift not reported: %s", final.Describe())
	}
	if len(f.ops.binding.Subjects) != 2 {
		t.Fatal("the controller mutated a binding it does not own")
	}
}

func TestGrantRemovalFailsButWorkloadStops(t *testing.T) {
	f := newFixture(t, time.Second)
	f.store.set(Record{SessionID: "session-1", RequestedState: RequestedRevoked, ApprovedByUserID: 42, ApprovedByLogin: "operator", Reason: "revoke", ApprovedAt: time.Now(), ExpiresAt: time.Now().Add(time.Minute), ObservedPhase: PhaseRevoking, GrantObserved: true, ActivePodUID: "pod-uid-5"})
	f.ops.binding.Subjects = []kube.Subject{{Kind: "ServiceAccount", Name: "pi-pocket-admin", Namespace: "pi-pocket-admin"}}
	f.ops.readyWorkspace("pod-uid-5")
	f.ops.failBindingPatch = true
	final := mustReconcile(t, f)
	if final.ObservedPhase != PhaseCleanupRequired || final.FailureCode != FailureGrantFailed {
		t.Fatalf("expected grant_failed: %s", final.Describe())
	}
	if f.ops.scale.Spec.Replicas != 0 {
		t.Fatal("workload was not stopped when the grant removal failed")
	}
	if _, present := f.ops.runtimeData[kube.OwnerLoginURLKey]; present {
		t.Fatal("published sign-in metadata survived cleanup")
	}
}

func TestPodShutdownFailureIsVisible(t *testing.T) {
	f := newFixture(t, time.Second)
	f.store.set(Record{SessionID: "session-1", RequestedState: RequestedRevoked, ApprovedByUserID: 42, ApprovedByLogin: "operator", Reason: "revoke", ApprovedAt: time.Now(), ExpiresAt: time.Now().Add(time.Minute), ObservedPhase: PhaseRevoking, GrantObserved: true, ActivePodUID: "pod-uid-6"})
	f.ops.binding.Subjects = []kube.Subject{{Kind: "ServiceAccount", Name: "pi-pocket-admin", Namespace: "pi-pocket-admin"}}
	f.ops.pods = []kube.Pod{{Metadata: kube.ObjectMeta{Name: "pi-pocket-admin-stuck", Namespace: "pi-pocket-admin", UID: "pod-uid-6", Labels: map[string]string{"app": "pi-pocket-admin"}}}}
	f.ops.stuckPods = true
	final := mustReconcile(t, f)
	if final.ObservedPhase != PhaseCleanupRequired || final.FailureCode != FailurePodShutdownPending {
		t.Fatalf("expected pod_shutdown_pending: %s", final.Describe())
	}
	if len(f.ops.binding.Subjects) != 0 {
		t.Fatal("grant was not removed before the Pod stopped")
	}
	// Once the Pod is gone, the next pass converges to Idle.
	f.ops.pods = nil
	f.ops.stuckPods = false
	final = mustReconcile(t, f)
	if final.ObservedPhase != PhaseIdle {
		t.Fatalf("cleanup did not converge: %s", final.Describe())
	}
}

func TestStalePodIsTerminatedBeforeActivation(t *testing.T) {
	f := newFixture(t, time.Second)
	f.approve(time.Minute)
	f.ops.pods = []kube.Pod{{Metadata: kube.ObjectMeta{Name: "pi-pocket-admin-old", Namespace: "pi-pocket-admin", UID: "old-pod", Labels: map[string]string{"app": "pi-pocket-admin"}}}}
	record := mustReconcile(t, f)
	if record.ObservedPhase != PhaseActivating || record.FailureCode != FailurePodShutdownPending {
		t.Fatalf("stale Pod not handled: %s", record.Describe())
	}
	if len(f.ops.binding.Subjects) != 0 || f.ops.scale.Spec.Replicas != 0 {
		t.Fatal("stale Pod kept the grant or the workspace")
	}
	if record.ActivePodUID != "" {
		t.Fatal("stale Pod UID was adopted")
	}
}

func TestEnsureIdleConvergesAndRefusesConcurrentActivation(t *testing.T) {
	f := newFixture(t, time.Second)
	// Nothing is requested but a previous session left a grant behind.
	f.ops.binding.Subjects = []kube.Subject{{Kind: "ServiceAccount", Name: "pi-pocket-admin", Namespace: "pi-pocket-admin"}}
	record := mustReconcile(t, f)
	if record.ObservedPhase != PhaseIdle || len(f.ops.binding.Subjects) != 0 {
		t.Fatalf("idle convergence failed: %s", record.Describe())
	}
	// An approval cannot start while cleanup is still required.
	f.store.set(Record{SessionID: "s", RequestedState: RequestedRevoked, ApprovedByUserID: 42, ApprovedByLogin: "operator", Reason: "x", ApprovedAt: time.Now(), ExpiresAt: time.Now().Add(time.Minute), ObservedPhase: PhaseCleanupRequired, FailureCode: FailurePodShutdownPending})
	f.ops.pods = []kube.Pod{{Metadata: kube.ObjectMeta{Name: "stuck", Namespace: "pi-pocket-admin", UID: "stuck", Labels: map[string]string{"app": "pi-pocket-admin"}}}}
	record = mustReconcile(t, f)
	if record.ObservedPhase != PhaseCleanupRequired {
		t.Fatalf("cleanup state lost: %s", record.Describe())
	}
	state, err := f.store.Load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ValidateActivation(state, ActivationRequest{Reason: "new", ApprovedByUserID: 42, ApprovedByLogin: "operator", SessionID: "s2"}, time.Minute, 2*time.Minute, time.Now()); !errors.Is(err, ErrCleanupIncomplet) {
		t.Fatalf("activation allowed during incomplete cleanup: %v", err)
	}
}

func TestValidateActivationBounds(t *testing.T) {
	idle := State{ResourceVersion: "1", Record: Record{ObservedPhase: PhaseIdle}}
	base := ActivationRequest{Reason: "why", ApprovedByUserID: 7, ApprovedByLogin: "op", SessionID: "sid"}
	if _, err := ValidateActivation(idle, base, time.Minute, 2*time.Minute, time.Now()); err != nil {
		t.Fatalf("valid activation rejected: %v", err)
	}
	for name, mutate := range map[string]func(*ActivationRequest){
		"empty reason":  func(r *ActivationRequest) { r.Reason = "   " },
		"long reason":   func(r *ActivationRequest) { r.Reason = strings.Repeat("x", ReasonLimit+1) },
		"control chars": func(r *ActivationRequest) { r.Reason = "ok\x00bad" },
		"no operator":   func(r *ActivationRequest) { r.ApprovedByUserID = 0 },
		"no approver":   func(r *ActivationRequest) { r.ApprovedByLogin = "" },
		"no session id": func(r *ActivationRequest) { r.SessionID = "" },
		"too long":      func(r *ActivationRequest) { r.Duration = 3 * time.Minute },
		"negative":      func(r *ActivationRequest) { r.Duration = -time.Minute },
	} {
		request := base
		mutate(&request)
		if _, err := ValidateActivation(idle, request, time.Minute, 2*time.Minute, time.Now()); err == nil {
			t.Fatalf("%s was accepted", name)
		}
	}
	active := State{Record: Record{ObservedPhase: PhaseActive}}
	if _, err := ValidateActivation(active, base, time.Minute, 2*time.Minute, time.Now()); !errors.Is(err, ErrSessionActive) {
		t.Fatal("a second concurrent session was allowed")
	}
	// Zero duration means the configured default.
	record, err := ValidateActivation(idle, base, 90*time.Second, 2*time.Minute, time.Now())
	if err != nil || record.ExpiresAt.Sub(record.ApprovedAt) != 90*time.Second {
		t.Fatalf("default duration not applied: %v %v", record.ExpiresAt, err)
	}
}

func TestTokenOperatorApprovalIsExplicit(t *testing.T) {
	idle := State{ResourceVersion: "1", Record: Record{ObservedPhase: PhaseIdle}}
	// A token-mode approval has no GitHub user ID; it is accepted only when the
	// caller says so explicitly, so a GitHub-mode caller cannot accidentally
	// record an identity-less approval.
	request := ActivationRequest{Reason: "why", ApprovedByLogin: "token-operator", SessionID: "sid", TokenOperator: true}
	record, err := ValidateActivation(idle, request, time.Minute, 2*time.Minute, time.Now())
	if err != nil {
		t.Fatalf("token approval rejected: %v", err)
	}
	if record.ApprovedByUserID != 0 || record.ApprovedByLogin != "token-operator" {
		t.Fatalf("unexpected approver: %s", record.Describe())
	}
	request.TokenOperator = false
	if _, err := ValidateActivation(idle, request, time.Minute, 2*time.Minute, time.Now()); !errors.Is(err, ErrInvalidApproval) {
		t.Fatal("identity-less approval accepted without the token flag")
	}
}

func TestTokenKindApprovalActivates(t *testing.T) {
	f := newFixture(t, time.Second)
	// A token-mode approval has no numeric GitHub ID but is not malformed: the
	// approver kind says how the identity was established.
	f.store.set(Record{
		SessionID: "token-session", RequestedState: RequestedActive, ApprovedByKind: ApprovedByToken,
		ApprovedByLogin: "token-operator", Reason: "lab activation",
		ApprovedAt: time.Now(), ExpiresAt: time.Now().Add(time.Minute), ObservedPhase: PhaseActivationRequested,
	})
	record := mustReconcile(t, f)
	if record.ObservedPhase != PhaseActive || record.FailureCode != "" {
		t.Fatalf("token approval did not activate: %s", record.Describe())
	}
	if record.ApprovedByKind != ApprovedByToken {
		t.Fatalf("approver kind lost: %s", record.Describe())
	}
	// An identity-less approval without the token kind is still refused.
	f2 := newFixture(t, time.Second)
	f2.store.set(Record{
		SessionID: "bad-session", RequestedState: RequestedActive, ApprovedByLogin: "nobody",
		Reason: "no kind", ApprovedAt: time.Now(), ExpiresAt: time.Now().Add(time.Minute),
		ObservedPhase: PhaseActivationRequested,
	})
	if final := mustReconcile(t, f2); final.FailureCode != FailureInvalidApproval {
		t.Fatalf("identity-less approval accepted: %s", final.Describe())
	}
}

func TestRequestRevocationNeedsNoRecentLogin(t *testing.T) {
	state := State{Record: Record{ObservedPhase: PhaseActive, SessionID: "s"}}
	record, err := RequestRevocation(state, time.Now())
	if err != nil || record.RequestedState != RequestedRevoked || record.ObservedPhase != PhaseRevoking {
		t.Fatalf("revocation request failed: %v %s", err, record.Describe())
	}
	if _, err := RequestRevocation(State{Record: Record{ObservedPhase: PhaseIdle}}, time.Now()); !errors.Is(err, ErrNoSession) {
		t.Fatal("idle state accepted a revocation")
	}
}
