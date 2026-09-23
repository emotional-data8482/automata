package core

import (
	"context"
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

// runScope owns one worker segment's accounting for a run. Runtime seeds it
// from the committed record, so turns, attempts, and usage continue across
// restarts. Child-run usage never enters these local totals.
type runScope struct {
	id                      string
	turns, providerAttempts int
	usage                   Usage
	result                  RunResult
}

func runOutcome(executionErr error) RunStatus {
	status := RunCompleted
	if executionErr != nil {
		switch {
		case errors.Is(executionErr, context.Canceled), errors.Is(executionErr, context.DeadlineExceeded):
			status = RunCancelled
		case errors.Is(executionErr, ErrMaxTurnsExceeded), errors.Is(executionErr, ErrTokenLimit):
			status = RunLimitReached
		default:
			status = RunFailed
		}
	}
	return status
}

// validateRun checks the effective configuration of a run before the loop
// starts, so a definition that cannot run fails before any provider work.
func (a *Agent) validateRun(cfg runConfig) error {
	if a.maxTurns <= 0 {
		return ErrInvalidMaxTurns
	}
	if err := validateCallOptions(cfg.options); err != nil {
		return err
	}
	_, _, err := registerTools(append(append([]Tool(nil), a.tools...), cfg.extraTools...), cfg.terminalTool)
	return err
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
