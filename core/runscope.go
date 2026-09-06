package core

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
)

// RunStatus describes the public invocation, independently of provider completion.
type RunStatus string

const (
	RunCompleted    RunStatus = "completed"
	RunFailed       RunStatus = "failed"
	RunCancelled    RunStatus = "cancelled"
	RunLimitReached RunStatus = "limit_reached"
)

// RunDiagnostic retains original assembled evidence outside canonical history.
// Bytes are ordinary byte slices (base64 in JSON), including invalid JSON fragments.
// No executable or raw JSON value can make a diagnostic impossible to serialize.
type RunDiagnostic struct {
	Turn         int
	InvocationID string
	ToolCallID   string
	Kind         string
	Message      string
	Data         []byte
}

// Checkpoint is an in-memory canonical history boundary with cumulative local
// accounting. It does not assert successful durable storage or typed validation.
type Checkpoint struct {
	RunID            string
	Turn             int
	Messages         []Message
	Usage            Usage
	Turns            int
	ProviderAttempts int
}

// CheckpointHook persists committed history. Finalization supplies a
// value-preserving, cancellation-detached context; implementations bound their
// own storage work. Returning an error stops continuation after all hooks run.
type CheckpointHook func(context.Context, Checkpoint, error) error

// runScope is the public invocation owner to be wired through entry points in
// Task 5. Internal machines/typed phases borrow it; background delivery does not
// create another owner. Children own distinct scopes and inherit only total tool
// caps through policy context. Descendant usage never enters these local totals.
type runScope struct {
	id, parentRunID, parentInvocationID string
	config                              runConfig
	turns, providerAttempts             int
	usage                               Usage
	policy                              *toolPolicyState
	observers                           []RunObserver
	checkpointHooks                     []CheckpointHook
	postRunHooks                        []PostRunHook
}

func newRunScope(parentRunID, parentInvocationID string, cfg runConfig) *runScope {
	return &runScope{id: newRunIdentity(), parentRunID: parentRunID, parentInvocationID: parentInvocationID, config: cfg, postRunHooks: append([]PostRunHook(nil), cfg.postRunHooks...)}
}
func newRunIdentity() string {
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		panic(err)
	}
	return hex.EncodeToString(id[:])
}
func newInvocationIdentity() string { return newRunIdentity() }

// runFinalization keeps the execution outcome distinct from persistence failures.
// An execution cancellation/limit wins over hook failures; errors.Join retains
// all causes for errors.Is/errors.As. Hook-only context errors mean failed storage,
// not cancellation of the public execution.
type runFinalization struct {
	executionErr     error
	checkpointErrors []error
	postRunErrors    []error
}

func (f runFinalization) outcome() (RunStatus, error) {
	status := RunCompleted
	if f.executionErr != nil {
		switch {
		case errors.Is(f.executionErr, context.Canceled), errors.Is(f.executionErr, context.DeadlineExceeded):
			status = RunCancelled
		case errors.Is(f.executionErr, ErrMaxStepsExceeded), errors.Is(f.executionErr, ErrTokenLimit):
			status = RunLimitReached
		default:
			status = RunFailed
		}
	}
	errs := []error{f.executionErr}
	errs = append(errs, f.checkpointErrors...)
	errs = append(errs, f.postRunErrors...)
	err := errors.Join(errs...)
	if err != nil && status == RunCompleted {
		status = RunFailed
	}
	return status, err
}
