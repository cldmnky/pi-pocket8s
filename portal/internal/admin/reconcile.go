package admin

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/cldmnky/pi-pocket8s/portal/internal/kube"
)

// Ops is the fixed set of Kubernetes operations the lifecycle needs. Every
// path is configured by name; there is no generic resource access.
type Ops interface {
	GetDeployment(ctx context.Context, namespace, name string) (*kube.Deployment, error)
	GetDeploymentScale(ctx context.Context, namespace, name string) (*kube.Scale, error)
	UpdateDeploymentScale(ctx context.Context, namespace, name string, scale *kube.Scale) (*kube.Scale, error)
	GetClusterRoleBinding(ctx context.Context, name string) (*kube.ClusterRoleBinding, error)
	PatchClusterRoleBinding(ctx context.Context, name string, patch []byte) (*kube.ClusterRoleBinding, error)
	ListPods(ctx context.Context, namespace, labelSelector string) (*kube.PodList, error)
	GetSecret(ctx context.Context, namespace, name string) (*kube.Secret, error)
	PatchSecret(ctx context.Context, namespace, name string, patch []byte) (*kube.Secret, error)
}

// Config is the fixed target of one elevation deployment. None of it comes
// from a browser request.
type Config struct {
	Namespace          string
	Deployment         string
	ServiceAccount     string
	RuntimeSecret      string
	ClusterRoleBinding string
	ClusterRole        string
	StartupTimeout     time.Duration
}

// Reconciler drives the lifecycle. Exactly one runs at a time.
type Reconciler struct {
	cfg   Config
	store Store
	ops   Ops
	log   *slog.Logger
	now   func() time.Time
	// pollInterval is how often long waits re-check for revocation requests.
	pollInterval time.Duration
}

// NewReconciler builds a reconciler. log may be nil.
func NewReconciler(cfg Config, store Store, ops Ops, log *slog.Logger) *Reconciler {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	if cfg.StartupTimeout <= 0 {
		cfg.StartupTimeout = 3 * time.Minute
	}
	return &Reconciler{cfg: cfg, store: store, ops: ops, log: log, now: time.Now, pollInterval: 2 * time.Second}
}

// VerifyTargets checks, once at startup, that the predefined binding and the
// admin Deployment exist and are the objects this controller is allowed to
// manage. It never mutates anything.
func (r *Reconciler) VerifyTargets(ctx context.Context) error {
	deployment, err := r.ops.GetDeployment(ctx, r.cfg.Namespace, r.cfg.Deployment)
	if err != nil {
		return fmt.Errorf("admin deployment %s/%s: %w", r.cfg.Namespace, r.cfg.Deployment, err)
	}
	if deployment.Metadata.UID == "" {
		return fmt.Errorf("admin deployment %s/%s has no UID", r.cfg.Namespace, r.cfg.Deployment)
	}
	binding, err := r.ops.GetClusterRoleBinding(ctx, r.cfg.ClusterRoleBinding)
	if err != nil {
		return fmt.Errorf("cluster role binding %s: %w", r.cfg.ClusterRoleBinding, err)
	}
	if !kube.BindingOwnedBy(binding, r.cfg.ClusterRole, r.cfg.Namespace, r.cfg.ServiceAccount) {
		return fmt.Errorf("cluster role binding %s is not the managed inert binding", r.cfg.ClusterRoleBinding)
	}
	return nil
}

// Reconcile advances the lifecycle by at most one meaningful step and returns
// only on unexpected failures. Lifecycle outcomes (grant, revoke, expiry,
// timeouts) are recorded in the durable state, not returned as errors.
func (r *Reconciler) Reconcile(ctx context.Context) error {
	state, err := r.store.Load(ctx)
	if err != nil {
		if errors.Is(err, ErrStateMissing) {
			// The chart always creates the state Secret; without it nothing can
			// be activated, which is the safe direction.
			return nil
		}
		if errors.Is(err, ErrInvalidState) {
			return r.recoverInvalidState(ctx, state)
		}
		return err
	}

	record := state.Record
	if record.ObservedPhase == "" {
		record.ObservedPhase = PhaseIdle
	}
	if record.RequestedState == "" {
		record.RequestedState = RequestedNone
	}
	// Expiry is evaluated on every pass, before anything else, and never
	// extends a deadline.
	if updated, expired := ApproveExpiry(record, r.now()); expired {
		r.log.Info("cluster-admin session expired", "session", updated.SessionID)
		record = updated
	}

	switch record.ObservedPhase {
	case PhaseIdle:
		if record.RequestedState == RequestedActive {
			return r.activate(ctx, state, record)
		}
		return r.ensureIdle(ctx, state, record)
	case PhaseActivationRequested, PhaseActivating:
		if record.RequestedState != RequestedActive {
			return r.revoke(ctx, state, record, "")
		}
		return r.activate(ctx, state, record)
	case PhaseActive:
		if record.RequestedState != RequestedActive {
			return r.revoke(ctx, state, record, "")
		}
		return r.observe(ctx, state, record)
	case PhaseRevocationRequested, PhaseExpired, PhaseRevoking, PhaseCleanupRequired:
		return r.revoke(ctx, state, record, record.FailureCode)
	default:
		// An unknown phase is never treated as idle or active.
		record.FailureCode = FailureStateInvalid
		return r.revoke(ctx, state, record, FailureStateInvalid)
	}
}

// ensureIdle makes sure nothing is left behind while no session exists: an
// inert binding, a stopped Deployment, no Pod, and no published sign-in link.
// It is what makes a crashed controller converge back to a safe state.
func (r *Reconciler) ensureIdle(ctx context.Context, state State, record Record) error {
	clean := true
	binding, err := r.ops.GetClusterRoleBinding(ctx, r.cfg.ClusterRoleBinding)
	switch {
	case err == nil:
		if kube.BindingHasSubject(binding, r.cfg.Namespace, r.cfg.ServiceAccount) {
			clean = false
		}
	case kube.IsNotFound(err):
		clean = false
	default:
		return err
	}
	scale, err := r.ops.GetDeploymentScale(ctx, r.cfg.Namespace, r.cfg.Deployment)
	if err != nil {
		return err
	}
	if scale.Spec.Replicas != 0 {
		clean = false
	}
	pods, err := r.listPods(ctx)
	if err != nil {
		return err
	}
	if len(pods) > 0 {
		clean = false
	}
	if clean {
		return nil
	}
	r.log.Warn("idle admin workspace was not clean; converging to stopped state", "session", record.SessionID)
	return r.revoke(ctx, state, record, record.FailureCode)
}

// recoverInvalidState attempts the managed cleanup and records the outcome.
// Malformed state can never cause an activation.
func (r *Reconciler) recoverInvalidState(ctx context.Context, state State) error {
	r.log.Error("admin session state is malformed; revoking and cleaning up")
	record := Record{
		RequestedState: RequestedRevoked,
		ObservedPhase:  PhaseCleanupRequired,
		FailureCode:    FailureStateInvalid,
	}
	state.Record = record
	return r.revoke(ctx, state, record, FailureStateInvalid)
}

func (r *Reconciler) activate(ctx context.Context, state State, record Record) error {
	now := r.now()
	// A token-mode approval is identified by its kind, not by a numeric GitHub
	// ID; anything else without an ID is malformed.
	approverKnown := record.ApprovedByUserID > 0 || record.ApprovedByKind == ApprovedByToken
	if record.SessionID == "" || !approverKnown || strings.TrimSpace(record.Reason) == "" || record.ExpiresAt.IsZero() {
		record.FailureCode = FailureInvalidApproval
		return r.revoke(ctx, state, record, FailureInvalidApproval)
	}
	if record.Expired(now) {
		record.RequestedState = RequestedRevoked
		return r.revoke(ctx, state, record, "")
	}

	// 1. The fixed target must exist and be the same object every pass.
	deployment, err := r.ops.GetDeployment(ctx, r.cfg.Namespace, r.cfg.Deployment)
	if err != nil {
		return r.fail(ctx, state, record, FailureTargetUnavailable)
	}
	if deployment.Metadata.UID == "" {
		return r.fail(ctx, state, record, FailureTargetUnavailable)
	}
	if record.AdminDeploymentUID != "" && record.AdminDeploymentUID != deployment.Metadata.UID {
		// The Deployment was replaced; the previous session's object is gone.
		return r.fail(ctx, state, record, FailureTargetUnavailable)
	}
	record.AdminDeploymentUID = deployment.Metadata.UID

	// 2. The predefined binding must exist and be inert or ours.
	binding, err := r.ops.GetClusterRoleBinding(ctx, r.cfg.ClusterRoleBinding)
	if err != nil {
		if kube.IsNotFound(err) {
			return r.fail(ctx, state, record, FailureBindingMissing)
		}
		return r.fail(ctx, state, record, FailureTargetUnavailable)
	}
	if !kube.BindingOwnedBy(binding, r.cfg.ClusterRole, r.cfg.Namespace, r.cfg.ServiceAccount) {
		return r.fail(ctx, state, record, FailureBindingNotOwned)
	}

	// 3. A Pod from an earlier session must lose its grant and terminate
	// before a new one starts: two elevated Pods must never overlap.
	pods, err := r.listPods(ctx)
	if err != nil {
		return err
	}
	if stale := stalePods(pods, record.ActivePodUID); len(stale) > 0 {
		r.log.Warn("terminating a stale admin workspace before activation", "pods", len(stale))
		problems := r.cleanupGrantAndWorkload(ctx)
		record.ObservedPhase = PhaseActivating
		record.GrantObserved = false
		record.WorkspaceReady = false
		record.ActivePodUID = ""
		record.LastReconciledAt = now.UTC()
		record.FailureCode = firstProblem(problems, FailurePodShutdownPending)
		_, err := r.store.Save(ctx, State{ResourceVersion: state.ResourceVersion, Record: record})
		return err
	}

	// 4. Persist the activating intent before touching Kubernetes.
	record.ObservedPhase = PhaseActivating
	record.LastReconciledAt = now.UTC()
	if !record.GrantObserved {
		record.FailureCode = ""
	}
	state, err = r.store.Save(ctx, State{ResourceVersion: state.ResourceVersion, Record: record})
	if err != nil {
		return err
	}

	// 5. Grant: exactly the configured service account.
	if !record.GrantObserved {
		if !kube.BindingHasSubject(binding, r.cfg.Namespace, r.cfg.ServiceAccount) {
			patch, err := kube.BindingSubjectsPatch(r.cfg.Namespace, r.cfg.ServiceAccount)
			if err != nil {
				return r.fail(ctx, state, record, FailureGrantFailed)
			}
			if _, err := r.ops.PatchClusterRoleBinding(ctx, r.cfg.ClusterRoleBinding, patch); err != nil {
				return r.fail(ctx, state, record, FailureGrantFailed)
			}
		}
		binding, err = r.ops.GetClusterRoleBinding(ctx, r.cfg.ClusterRoleBinding)
		if err != nil || !kube.BindingHasSubject(binding, r.cfg.Namespace, r.cfg.ServiceAccount) {
			return r.fail(ctx, state, record, FailureGrantFailed)
		}
		record.GrantObserved = true
		record.LastReconciledAt = r.now().UTC()
		if state, err = r.store.Save(ctx, State{ResourceVersion: state.ResourceVersion, Record: record}); err != nil {
			return err
		}
		r.log.Info("cluster-admin grant applied", "session", record.SessionID, "operator", record.ApprovedByLogin)
	}

	// 6. Start the predefined workspace.
	scale, err := r.ops.GetDeploymentScale(ctx, r.cfg.Namespace, r.cfg.Deployment)
	if err != nil {
		return r.fail(ctx, state, record, FailureScaleFailed)
	}
	if scale.Spec.Replicas != 1 {
		scale.Spec.Replicas = 1
		if _, err := r.ops.UpdateDeploymentScale(ctx, r.cfg.Namespace, r.cfg.Deployment, scale); err != nil {
			return r.fail(ctx, state, record, FailureScaleFailed)
		}
	}

	// 7. Wait for the workspace and its published sign-in metadata.
	return r.awaitWorkspace(ctx, state, record)
}

// awaitWorkspace waits for a ready Pod with matching owner-login metadata. It
// re-reads the durable state while waiting so an operator's revocation, or the
// deadline, cancels the startup instead of being ignored.
func (r *Reconciler) awaitWorkspace(ctx context.Context, state State, record Record) error {
	deadline := r.now().Add(r.cfg.StartupTimeout)
	if record.ExpiresAt.Before(deadline) {
		deadline = record.ExpiresAt
	}
	for {
		if r.now().After(deadline) {
			r.log.Warn("admin workspace did not become ready in time", "session", record.SessionID)
			record.FailureCode = FailureWorkspaceTimeout
			return r.revoke(ctx, state, record, FailureWorkspaceTimeout)
		}
		// A revocation or a newer approval always wins over startup.
		current, err := r.store.Load(ctx)
		if err != nil {
			return err
		}
		if current.Record.RequestedState != RequestedActive || current.Record.SessionID != record.SessionID {
			return r.revoke(ctx, current, current.Record, "")
		}
		state = current
		record = current.Record

		pods, err := r.listPods(ctx)
		if err != nil {
			return err
		}
		pod := activePod(pods, record.ActivePodUID)
		if pod != nil && record.ActivePodUID == "" {
			record.ActivePodUID = pod.Metadata.UID
			record.LastReconciledAt = r.now().UTC()
			if state, err = r.store.Save(ctx, State{ResourceVersion: state.ResourceVersion, Record: record}); err != nil {
				return err
			}
		}
		url, podUID, present := r.ownerLogin(ctx)
		ready := pod != nil && podReady(pod) && present && podUID == record.ActivePodUID && record.ActivePodUID != ""
		record.WorkspaceReady = ready
		record.LastReconciledAt = r.now().UTC()
		if ready {
			record.ObservedPhase = PhaseActive
			record.FailureCode = ""
			if _, err := r.store.Save(ctx, State{ResourceVersion: state.ResourceVersion, Record: record}); err != nil {
				return err
			}
			r.log.Info("cluster-admin workspace ready", "session", record.SessionID, "pod", record.ActivePodUID, "url", redactURL(url))
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(r.pollInterval):
		}
	}
}

// observe refreshes an active session's observations and detects drift: a
// missing grant or a replaced Pod ends the session instead of being ignored.
func (r *Reconciler) observe(ctx context.Context, state State, record Record) error {
	binding, err := r.ops.GetClusterRoleBinding(ctx, r.cfg.ClusterRoleBinding)
	if err != nil {
		if kube.IsNotFound(err) {
			return r.revoke(ctx, state, record, FailureBindingMissing)
		}
		return err
	}
	if !kube.BindingOwnedBy(binding, r.cfg.ClusterRole, r.cfg.Namespace, r.cfg.ServiceAccount) {
		return r.revoke(ctx, state, record, FailureBindingNotOwned)
	}
	if !kube.BindingHasSubject(binding, r.cfg.Namespace, r.cfg.ServiceAccount) {
		r.log.Warn("cluster-admin grant drifted; ending the session", "session", record.SessionID)
		return r.revoke(ctx, state, record, FailureGrantDrift)
	}
	pods, err := r.listPods(ctx)
	if err != nil {
		return err
	}
	pod := activePod(pods, record.ActivePodUID)
	if pod == nil {
		r.log.Warn("admin workspace Pod is gone; ending the session", "session", record.SessionID)
		return r.revoke(ctx, state, record, FailurePodGone)
	}
	if record.ActivePodUID == "" {
		record.ActivePodUID = pod.Metadata.UID
	}
	_, podUID, present := r.ownerLogin(ctx)
	record.WorkspaceReady = podReady(pod) && present && podUID == record.ActivePodUID
	if record.WorkspaceReady {
		record.FailureCode = ""
	} else if record.FailureCode == "" {
		record.FailureCode = FailureWorkspaceNotReady
	}
	record.LastReconciledAt = r.now().UTC()
	_, err = r.store.Save(ctx, State{ResourceVersion: state.ResourceVersion, Record: record})
	return err
}

// revoke removes the grant, stops the workspace, and clears published metadata.
// Each step is attempted even when another fails, and the phase only becomes
// Idle once cleanup is confirmed. The revoked intent is persisted first so no
// reconciliation pass can restart the session.
func (r *Reconciler) revoke(ctx context.Context, state State, record Record, failure string) error {
	record.RequestedState = RequestedRevoked
	if record.ObservedPhase != PhaseCleanupRequired {
		record.ObservedPhase = PhaseRevoking
	}
	record.LastReconciledAt = r.now().UTC()
	if failure != "" {
		record.FailureCode = failure
	}
	saved, err := r.store.Save(ctx, State{ResourceVersion: state.ResourceVersion, Record: record})
	if err != nil {
		return err
	}
	state, record = saved, saved.Record

	problems := r.cleanupGrantAndWorkload(ctx)

	if len(problems) > 0 {
		record.ObservedPhase = PhaseCleanupRequired
		record.GrantObserved = false
		record.WorkspaceReady = false
		if failure == "" {
			record.FailureCode = problems[0]
		}
		record.LastReconciledAt = r.now().UTC()
		if _, err := r.store.Save(ctx, State{ResourceVersion: state.ResourceVersion, Record: record}); err != nil {
			return err
		}
		r.log.Error("cluster-admin cleanup is incomplete", "session", record.SessionID, "problem", record.FailureCode)
		return nil
	}

	wasFailure := record.FailureCode != ""
	record = Record{
		ObservedPhase:    PhaseIdle,
		RequestedState:   RequestedNone,
		LastReconciledAt: r.now().UTC(),
	}
	if wasFailure && failure != "" {
		record.FailureCode = failure
	}
	if _, err := r.store.Save(ctx, State{ResourceVersion: state.ResourceVersion, Record: record}); err != nil {
		return err
	}
	r.log.Info("cluster-admin session revoked and cleaned up", "session", saved.Record.SessionID, "failure", failure)
	return nil
}

// cleanupGrantAndWorkload performs the three independent cleanup attempts and
// returns the failure codes that remain.
func (r *Reconciler) cleanupGrantAndWorkload(ctx context.Context) []string {
	var problems []string
	binding, err := r.ops.GetClusterRoleBinding(ctx, r.cfg.ClusterRoleBinding)
	switch {
	case err == nil:
		if !kube.BindingOwnedBy(binding, r.cfg.ClusterRole, r.cfg.Namespace, r.cfg.ServiceAccount) {
			problems = append(problems, FailureBindingNotOwned)
		} else if kube.BindingHasSubject(binding, r.cfg.Namespace, r.cfg.ServiceAccount) {
			patch, err := kube.BindingSubjectsPatch("", "")
			if err != nil {
				problems = append(problems, FailureGrantFailed)
			} else if _, err := r.ops.PatchClusterRoleBinding(ctx, r.cfg.ClusterRoleBinding, patch); err != nil {
				problems = append(problems, FailureGrantFailed)
			} else {
				r.log.Info("cluster-admin grant removed")
			}
		}
	case kube.IsNotFound(err):
		problems = append(problems, FailureBindingMissing)
	default:
		problems = append(problems, FailureTargetUnavailable)
	}

	scale, err := r.ops.GetDeploymentScale(ctx, r.cfg.Namespace, r.cfg.Deployment)
	switch {
	case err != nil:
		problems = append(problems, FailureScaleFailed)
	case scale.Spec.Replicas != 0:
		scale.Spec.Replicas = 0
		if _, err := r.ops.UpdateDeploymentScale(ctx, r.cfg.Namespace, r.cfg.Deployment, scale); err != nil {
			problems = append(problems, FailureScaleFailed)
		}
	}

	if err := r.clearOwnerLogin(ctx); err != nil {
		problems = append(problems, FailureMetadataFailed)
	}

	pods, err := r.listPods(ctx)
	if err != nil {
		problems = append(problems, FailureTargetUnavailable)
	} else if len(pods) > 0 {
		problems = append(problems, FailurePodShutdownPending)
	}
	return problems
}

// clearOwnerLogin removes the published sign-in metadata so a stale link can
// never be shown for a later session.
func (r *Reconciler) clearOwnerLogin(ctx context.Context) error {
	patch, err := kube.ClearOwnerLoginPatch()
	if err != nil {
		return err
	}
	_, err = r.ops.PatchSecret(ctx, r.cfg.Namespace, r.cfg.RuntimeSecret, patch)
	return err
}

// ownerLogin reads the admin workspace's published sign-in metadata.
func (r *Reconciler) ownerLogin(ctx context.Context) (url, podUID string, present bool) {
	secret, err := r.ops.GetSecret(ctx, r.cfg.Namespace, r.cfg.RuntimeSecret)
	if err != nil {
		return "", "", false
	}
	url, urlPresent, err := kube.SecretString(secret, kube.OwnerLoginURLKey)
	if err != nil || !urlPresent || url == "" {
		return "", "", false
	}
	podUID, uidPresent, err := kube.SecretString(secret, kube.OwnerLoginPodUIDKey)
	if err != nil || !uidPresent || podUID == "" {
		return url, "", false
	}
	return url, podUID, true
}

// fail records a failure and immediately attempts the cleanup.
func (r *Reconciler) fail(ctx context.Context, state State, record Record, code string) error {
	r.log.Error("cluster-admin lifecycle failed", "session", record.SessionID, "failure", code)
	return r.revoke(ctx, state, record, code)
}

func (r *Reconciler) listPods(ctx context.Context) ([]kube.Pod, error) {
	deployment, err := r.ops.GetDeployment(ctx, r.cfg.Namespace, r.cfg.Deployment)
	if err != nil {
		return nil, err
	}
	pods, err := r.ops.ListPods(ctx, r.cfg.Namespace, selectorString(deployment))
	if err != nil {
		return nil, err
	}
	return pods.Items, nil
}

func selectorString(deployment *kube.Deployment) string {
	if deployment == nil || deployment.Spec.Selector == nil {
		return ""
	}
	keys := make([]string, 0, len(deployment.Spec.Selector.MatchLabels))
	for key := range deployment.Spec.Selector.MatchLabels {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, key := range keys {
		parts = append(parts, key+"="+deployment.Spec.Selector.MatchLabels[key])
	}
	return strings.Join(parts, ",")
}

// activePod returns the non-terminating Pod that belongs to the session, and
// nil when none does.
func activePod(pods []kube.Pod, uid string) *kube.Pod {
	for i := range pods {
		pod := pods[i]
		if pod.Metadata.DeletionTimestamp != nil {
			continue
		}
		if uid == "" || pod.Metadata.UID == uid {
			return &pod
		}
	}
	return nil
}

func stalePods(pods []kube.Pod, uid string) []kube.Pod {
	var stale []kube.Pod
	for _, pod := range pods {
		if pod.Metadata.UID != uid {
			stale = append(stale, pod)
		}
	}
	return stale
}

func podReady(pod *kube.Pod) bool {
	if pod == nil || pod.Metadata.DeletionTimestamp != nil {
		return false
	}
	if pod.Status.Phase != "Running" {
		return false
	}
	for _, condition := range pod.Status.Conditions {
		if condition.Type == "Ready" {
			return condition.Status == "True"
		}
	}
	return false
}

func firstProblem(problems []string, fallback string) string {
	if len(problems) > 0 {
		return problems[0]
	}
	return fallback
}

// redactURL keeps a URL's origin for logs and drops the query, which carries
// the owner sign-in token.
func redactURL(raw string) string {
	if index := strings.IndexByte(raw, '?'); index >= 0 {
		return raw[:index]
	}
	return raw
}
