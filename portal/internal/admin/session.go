// Package admin implements the cluster-admin elevation session model: a
// durable approval record, the bounded lifecycle that grants and revokes it,
// and the reconciliation that keeps Kubernetes state matching that record.
//
// The package holds no HTTP surface and no credentials. The portal writes
// approvals; the controller reconciles them; the admin workspace's own service
// account is the only identity that ever receives cluster-admin.
package admin

import (
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"
)

// Phase is the observed lifecycle state of the elevation session. Desired and
// observed state are deliberately separate fields: a valid approval is not
// proof that the workspace is running or that permissions were granted.
type Phase string

const (
	PhaseIdle                Phase = "Idle"
	PhaseActivationRequested Phase = "ActivationRequested"
	PhaseActivating          Phase = "Activating"
	PhaseActive              Phase = "Active"
	PhaseRevocationRequested Phase = "RevocationRequested"
	PhaseExpired             Phase = "Expired"
	PhaseRevoking            Phase = "Revoking"
	PhaseCleanupRequired     Phase = "CleanupRequired"
)

// RequestedState is the durable intent written by the portal. It is the only
// field that can start or stop the lifecycle.
type RequestedState string

const (
	RequestedNone    RequestedState = "none"
	RequestedActive  RequestedState = "active"
	RequestedRevoked RequestedState = "revoked"
)

// Failure codes are fixed, credential-free reasons a session is not healthy.
const (
	FailureInvalidApproval    = "invalid_approval"
	FailureTargetUnavailable  = "target_unavailable"
	FailureBindingNotOwned    = "binding_not_owned"
	FailureBindingMissing     = "binding_missing"
	FailureGrantFailed        = "grant_failed"
	FailureScaleFailed        = "scale_failed"
	FailureWorkspaceTimeout   = "workspace_timeout"
	FailureWorkspaceNotReady  = "workspace_not_ready"
	FailureGrantDrift         = "grant_drift"
	FailurePodGone            = "pod_gone"
	FailureMetadataFailed     = "metadata_cleanup_failed"
	FailurePodShutdownPending = "pod_shutdown_pending"
	FailureStateInvalid       = "state_invalid"
)

// ReasonLimit bounds the justification stored with a session.
const ReasonLimit = 500

// Record is the durable session record. It never contains a Kubernetes token,
// an owner sign-in URL, or any other credential.
type Record struct {
	SessionID          string         `json:"sessionID,omitempty"`
	RequestedState     RequestedState `json:"requestedState,omitempty"`
	ApprovedByUserID   int64          `json:"approvedByUserID,omitempty"`
	ApprovedByLogin    string         `json:"approvedByLogin,omitempty"`
	Reason             string         `json:"reason,omitempty"`
	ApprovedAt         time.Time      `json:"approvedAt,omitempty"`
	ExpiresAt          time.Time      `json:"expiresAt,omitempty"`
	ObservedPhase      Phase          `json:"observedPhase,omitempty"`
	AdminDeploymentUID string         `json:"adminDeploymentUID,omitempty"`
	ActivePodUID       string         `json:"activePodUID,omitempty"`
	GrantObserved      bool           `json:"grantObserved,omitempty"`
	WorkspaceReady     bool           `json:"workspaceReady,omitempty"`
	LastReconciledAt   time.Time      `json:"lastReconciledAt,omitempty"`
	FailureCode        string         `json:"failureCode,omitempty"`
}

// Active reports whether the record currently represents a session that may
// have a live grant. It is intentionally not a claim that a grant exists.
func (r Record) Active() bool {
	switch r.ObservedPhase {
	case PhaseActivationRequested, PhaseActivating, PhaseActive, PhaseRevocationRequested, PhaseExpired, PhaseRevoking, PhaseCleanupRequired:
		return true
	}
	return false
}

// Expired reports whether the approval's deadline has passed.
func (r Record) Expired(now time.Time) bool {
	return !r.ExpiresAt.IsZero() && !now.Before(r.ExpiresAt)
}

// SecondsRemaining is the countdown shown to the operator, never negative.
func (r Record) SecondsRemaining(now time.Time) int64 {
	if r.ExpiresAt.IsZero() {
		return 0
	}
	remaining := r.ExpiresAt.Sub(now)
	if remaining <= 0 {
		return 0
	}
	return int64(remaining / time.Second)
}

// ActivationRequest is the portal-supplied part of an activation. The session
// ID, approver identity, approval time, and expiry are computed by the server;
// the client supplies only the justification and duration.
type ActivationRequest struct {
	Reason           string
	Duration         time.Duration
	ApprovedByUserID int64
	ApprovedByLogin  string
	SessionID        string
	// TokenOperator marks an approver identified by the portal token rather
	// than by a GitHub identity. It is only ever set when elevation was
	// configured with the explicit token-auth opt-in.
	TokenOperator bool
}

// Errors returned to the portal. They carry no Kubernetes detail.
var (
	ErrSessionActive    = errors.New("a cluster-admin session is already active or being cleaned up")
	ErrCleanupIncomplet = errors.New("a previous session's cleanup is incomplete")
	ErrInvalidReason    = errors.New("a justification of at most 500 characters is required")
	ErrInvalidDuration  = errors.New("duration must be positive and within the configured maximum")
	ErrInvalidApproval  = errors.New("the stored approval is not valid")
	ErrConflict         = errors.New("session state changed; reload before retrying")
	ErrStateMissing     = errors.New("admin session state secret is missing")
	ErrInvalidState     = errors.New("admin session state is malformed")
	ErrNoSession        = errors.New("no cluster-admin session exists")
)

// ValidateActivation builds the approval record for a new session. The caller
// must have already checked that the human is an authorized operator with a
// recent login.
func ValidateActivation(state State, request ActivationRequest, defaultDuration, maxDuration time.Duration, now time.Time) (Record, error) {
	reason := strings.TrimSpace(request.Reason)
	if reason == "" || utf8.RuneCountInString(reason) > ReasonLimit || hasControlCharacters(reason) {
		return Record{}, ErrInvalidReason
	}
	if request.ApprovedByUserID <= 0 && !request.TokenOperator {
		return Record{}, ErrInvalidApproval
	}
	if strings.TrimSpace(request.ApprovedByLogin) == "" || request.SessionID == "" {
		return Record{}, ErrInvalidApproval
	}
	if request.Duration == 0 {
		request.Duration = defaultDuration
	}
	if request.Duration <= 0 || request.Duration > maxDuration {
		return Record{}, ErrInvalidDuration
	}
	switch state.Record.ObservedPhase {
	case "", PhaseIdle:
	default:
		if state.Record.ObservedPhase == PhaseCleanupRequired {
			return Record{}, ErrCleanupIncomplet
		}
		return Record{}, ErrSessionActive
	}
	return Record{
		SessionID:        request.SessionID,
		RequestedState:   RequestedActive,
		ApprovedByUserID: request.ApprovedByUserID,
		ApprovedByLogin:  strings.TrimSpace(request.ApprovedByLogin),
		Reason:           reason,
		ApprovedAt:       now.UTC(),
		ExpiresAt:        now.Add(request.Duration).UTC(),
		ObservedPhase:    PhaseActivationRequested,
	}, nil
}

// RequestRevocation marks a session for revocation. It is deliberately usable
// without a recent login: emergency shutdown must not require a new sign-in.
func RequestRevocation(state State, now time.Time) (Record, error) {
	record := state.Record
	if record.ObservedPhase == "" || record.ObservedPhase == PhaseIdle {
		return Record{}, ErrNoSession
	}
	record.RequestedState = RequestedRevoked
	if record.ObservedPhase != PhaseCleanupRequired {
		record.ObservedPhase = PhaseRevoking
	}
	record.LastReconciledAt = now.UTC()
	return record, nil
}

// ApproveExpiry rewrites an expired approval to the revoked intent so that no
// reconciliation pass can restart it. The deadline itself is never extended.
func ApproveExpiry(record Record, now time.Time) (Record, bool) {
	if record.RequestedState != RequestedActive || !record.Expired(now) {
		return record, false
	}
	record.RequestedState = RequestedRevoked
	if record.ObservedPhase == PhaseActive || record.ObservedPhase == PhaseActivating || record.ObservedPhase == PhaseActivationRequested {
		record.ObservedPhase = PhaseExpired
	}
	return record, true
}

func hasControlCharacters(value string) bool {
	for _, r := range value {
		if r == '\n' || r == '\t' || r == '\r' {
			continue
		}
		if r < 0x20 || r == 0x7f {
			return true
		}
	}
	return false
}

// Describe returns a short, credential-free description of a record for logs.
func (r Record) Describe() string {
	return fmt.Sprintf("phase=%s requested=%s session=%s owner=%s failure=%s",
		r.ObservedPhase, r.RequestedState, r.SessionID, r.ApprovedByLogin, r.FailureCode)
}
