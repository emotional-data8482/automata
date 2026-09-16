package core

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
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
	result                              RunResult
	ctx                                 context.Context
	commit                              func([]Message)
	checkpointMessages                  []Message
	sequence                            uint64
}

func newRunScopeWithID(parentRunID, parentInvocationID string, cfg runConfig, id string) *runScope {
	return &runScope{id: id, parentRunID: parentRunID, parentInvocationID: parentInvocationID, config: cfg, observers: append([]RunObserver(nil), cfg.observers...)}
}
func newRunIdentity() string {
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		panic(err)
	}
	return hex.EncodeToString(id[:])
}

func runOutcome(executionErr error) RunStatus {
	status := RunCompleted
	if executionErr != nil {
		switch {
		case errors.Is(executionErr, context.Canceled), errors.Is(executionErr, context.DeadlineExceeded):
			status = RunCancelled
		case errors.Is(executionErr, ErrMaxStepsExceeded), errors.Is(executionErr, ErrTokenLimit):
			status = RunLimitReached
		default:
			status = RunFailed
		}
	}
	return status
}

// beginRun admits an invocation before validating its effective options.
func (a *Agent) beginRun(ctx context.Context, cfg runConfig, history []Message, commit func([]Message), mode string) (*runScope, error) {
	return a.beginRunWithID(ctx, cfg, history, commit, mode, newRunIdentity())
}
func (a *Agent) beginRunWithID(ctx context.Context, cfg runConfig, history []Message, commit func([]Message), mode, runID string) (*runScope, error) {
	s := newRunScopeWithID("", "", cfg, runID)
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
func (s *runScope) checkpoint(messages []Message) {
	if reflect.DeepEqual(messages, s.checkpointMessages) {
		return
	}
	s.checkpointMessages = cloneMessages(messages)
	if s.commit != nil {
		s.commit(cloneMessages(messages))
	}
	cp := Checkpoint{RunID: s.id, Turn: s.turns, Messages: cloneMessages(messages), Usage: s.usage, Turns: s.turns, ProviderAttempts: s.providerAttempts}
	s.emit(s.ctx, s.turns, CheckpointCommittedPayload{Checkpoint: cp})
}
func (s *runScope) finalize(result RunResult, executionErr error) (RunResult, error) {
	var transitionErr *durableTransitionFailure
	if errors.As(executionErr, &transitionErr) {
		executionErr = transitionErr.executionErr
	}
	result.RunID = s.id
	result.Turns = s.turns
	result.ProviderAttempts = s.providerAttempts
	result.Usage = s.usage
	result.Status = runOutcome(executionErr)
	return cloneRunResult(result), executionErr
}

func (s *runScope) finish(result RunResult, executionErr error) (RunResult, error) {
	result, executionErr = s.finalize(result, executionErr)
	s.emit(s.ctx, 0, RunFinishedPayload{Result: result, Err: executionErr})
	return cloneRunResult(result), executionErr
}

type durableTransitionFailure struct {
	executionErr error
	cause        error
}

func (e *durableTransitionFailure) Error() string {
	return "durable transition failed: " + e.cause.Error()
}
func (e *durableTransitionFailure) Unwrap() error {
	return errors.Join(e.executionErr, e.cause)
}
