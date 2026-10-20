package admin

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"

	"github.com/cldmnky/pi-pocket8s/portal/internal/kube"
)

// StateKey is the single key the elevation session is stored under.
const StateKey = "session.json"

// State is the stored record plus the resource version needed for optimistic
// concurrency. The resource version is never serialized into the record.
type State struct {
	ResourceVersion string
	Record          Record
}

// Store persists the one elevation session. Implementations must treat a
// missing record as Idle, a malformed record as ErrInvalidState, and a
// concurrent write as ErrConflict.
type Store interface {
	Load(ctx context.Context) (State, error)
	Save(ctx context.Context, state State) (State, error)
}

// SecretStore keeps the session in one named Secret in the management
// namespace. It stores no credentials: the record holds identities, times,
// phases, and failure codes only.
type SecretStore struct {
	Client    *kube.Client
	Namespace string
	Name      string
}

// Load reads the session record. An absent key is Idle; malformed JSON is an
// error the reconciler must resolve by cleanup, never by activating.
func (s *SecretStore) Load(ctx context.Context) (State, error) {
	secret, err := s.Client.GetSecret(ctx, s.Namespace, s.Name)
	if err != nil {
		if kube.IsNotFound(err) {
			return State{}, ErrStateMissing
		}
		return State{}, err
	}
	raw, present, err := secret.Bytes(StateKey)
	if err != nil {
		return State{ResourceVersion: secret.Metadata.ResourceVersion}, fmt.Errorf("%w: %v", ErrInvalidState, err)
	}
	if !present || len(bytes.TrimSpace(raw)) == 0 {
		return State{ResourceVersion: secret.Metadata.ResourceVersion, Record: Record{ObservedPhase: PhaseIdle, RequestedState: RequestedNone}}, nil
	}
	var record Record
	if err := json.Unmarshal(raw, &record); err != nil {
		return State{ResourceVersion: secret.Metadata.ResourceVersion}, fmt.Errorf("%w: %v", ErrInvalidState, err)
	}
	if record.ObservedPhase == "" {
		record.ObservedPhase = PhaseIdle
	}
	if record.RequestedState == "" {
		record.RequestedState = RequestedNone
	}
	return State{ResourceVersion: secret.Metadata.ResourceVersion, Record: record}, nil
}

// Save writes the record with the resource version it was loaded from, so a
// concurrent portal write is rejected instead of being overwritten.
func (s *SecretStore) Save(ctx context.Context, state State) (State, error) {
	payload, err := json.Marshal(state.Record)
	if err != nil {
		return State{}, fmt.Errorf("encode session record: %w", err)
	}
	patch, err := json.Marshal(map[string]any{
		"metadata": map[string]string{"resourceVersion": state.ResourceVersion},
		"data":     map[string]string{StateKey: base64.StdEncoding.EncodeToString(payload)},
	})
	if err != nil {
		return State{}, fmt.Errorf("encode session patch: %w", err)
	}
	secret, err := s.Client.PatchSecret(ctx, s.Namespace, s.Name, patch)
	if err != nil {
		if kube.IsConflict(err) {
			return State{}, ErrConflict
		}
		return State{}, err
	}
	return State{ResourceVersion: secret.Metadata.ResourceVersion, Record: state.Record}, nil
}
