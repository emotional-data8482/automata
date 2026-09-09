package core

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"reflect"
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

// runScope owns a public invocation. Internal machines and typed phases
// borrow it; background delivery does not
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
	result                              RunResult
	ctx                                 context.Context
	commit                              func([]Message)
	checkpointMessages                  []Message
	finalization                        runFinalization
	sequence                            uint64
}

func newRunScope(parentRunID, parentInvocationID string, cfg runConfig) *runScope {
	return &runScope{id: newRunIdentity(), parentRunID: parentRunID, parentInvocationID: parentInvocationID, config: cfg, postRunHooks: append([]PostRunHook(nil), cfg.postRunHooks...), observers: append([]RunObserver(nil), cfg.observers...), checkpointHooks: append([]CheckpointHook(nil), cfg.checkpointHooks...)}
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

// beginRun admits an invocation before validating its effective options.
func (a *Agent) beginRun(ctx context.Context, cfg runConfig, history []Message, commit func([]Message), mode string) (*runScope, error) {
	s := newRunScope("", "", cfg)
	s.ctx = ctx
	s.commit = commit
	s.checkpointMessages = cloneMessages(history)
	s.result = RunResult{RunID: s.id, Messages: cloneMessages(history)}
	s.emit(ctx, 0, RunStartedPayload{Mode: mode})
	var err error
	switch {
	case cfg.optionErr != nil:
		err = cfg.optionErr
	case cfg.maxTurns <= 0:
		err = ErrInvalidMaxSteps
	default:
		err = validateCallOptions(cfg.options)
	}
	if err == nil {
		_, _, err = registerTools(append(append([]Tool(nil), a.tools...), cfg.extraTools...), cfg.terminalTool)
	}
	if err == nil {
		s.ctx, s.policy, err = newToolPolicyState(ctx, cfg.toolPolicy)
	}
	return s, err
}
func (s *runScope) emit(ctx context.Context, turn int, p RunEventPayload) {
	if len(s.observers) == 0 {
		return
	}
	s.sequence++
	for i, o := range s.observers {
		if o == nil {
			continue
		}
		func() {
			defer func() {
				if recover() != nil {
					s.observers[i] = nil
				}
			}()
			o(ctx, newRunEvent(s, turn, s.sequence, cloneEventPayload(p)))
		}()
	}
}
func (s *runScope) checkpoint(messages []Message, executionErr error) error {
	if reflect.DeepEqual(messages, s.checkpointMessages) {
		return nil
	}
	s.checkpointMessages = cloneMessages(messages)
	if s.commit != nil {
		s.commit(cloneMessages(messages))
	}
	cp := Checkpoint{RunID: s.id, Turn: s.turns, Messages: cloneMessages(messages), Usage: s.usage, Turns: s.turns, ProviderAttempts: s.providerAttempts}
	s.emit(s.ctx, s.turns, CheckpointCommittedPayload{Checkpoint: cp})
	var errs []error
	for i, h := range s.checkpointHooks {
		if h != nil {
			c := cp
			c.Messages = cloneMessages(cp.Messages)
			if e := h(context.WithoutCancel(s.ctx), c, executionErr); e != nil {
				errs = append(errs, fmt.Errorf("checkpoint hook %d failed: %w", i, e))
			}
		}
	}
	s.finalization.checkpointErrors = append(s.finalization.checkpointErrors, errs...)
	return errors.Join(errs...)
}
func (s *runScope) finish(result RunResult, executionErr error) (RunResult, error) {
	var checkpointErr *checkpointFailure
	if errors.As(executionErr, &checkpointErr) {
		executionErr = checkpointErr.executionErr
	}
	s.finalization.executionErr = executionErr
	result.RunID = s.id
	result.Turns = s.turns
	result.ProviderAttempts = s.providerAttempts
	result.Usage = s.usage
	result.Status, executionErr = s.finalization.outcome()
	for i, h := range s.postRunHooks {
		if h != nil {
			if err := h(context.WithoutCancel(s.ctx), cloneRunResult(result), executionErr); err != nil {
				s.finalization.postRunErrors = append(s.finalization.postRunErrors, fmt.Errorf("post-run hook %d failed: %w", i, err))
			}
		}
	}
	var err error
	result.Status, err = s.finalization.outcome()
	s.emit(s.ctx, 0, RunFinishedPayload{Result: result, Err: err})
	return cloneRunResult(result), err
}

type checkpointFailure struct{ executionErr error }

func (e *checkpointFailure) Error() string { return "checkpoint failed" }
func (e *checkpointFailure) Unwrap() error { return e.executionErr }
