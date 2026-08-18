package core

import (
	"context"
	"errors"
	"fmt"
)

// PreSendHook fires once per turn, immediately before the provider invocation.
// It receives the messages and tools to be sent and returns (possibly modified)
// versions. Hooks transform a snapshot of the canonical message history into
// a rendered view sent to the provider — they do NOT mutate the canonical
// history maintained by the run loop.
//
// Returning a non-nil error aborts the run.
type PreSendHook func(
	ctx context.Context,
	messages []Message,
	tools []Tool,
) ([]Message, []Tool, error)

// PostRunHook fires after a run has finished and its result is fully populated.
// For a [Session], the canonical transcript is committed before the hook runs,
// and the session's run lock remains held until every hook has returned.
//
// result and its messages are read-only snapshots. runErr is the original run
// error, if any; it does not include errors returned by earlier post-run hooks.
// Hooks run even after failures so callers can persist partial transcripts and
// usage. The supplied context preserves values from the run context but has no
// cancellation signal or deadline; persistence implementations should impose
// their own timeout.
//
// Returning an error reports a checkpoint failure to the caller without
// discarding result. If the run also failed, the errors are joined so both
// remain discoverable with errors.Is and errors.As.
type PostRunHook func(
	ctx context.Context,
	result RunResult,
	runErr error,
) error

// WithPostRunHook registers a hook for one run. Repeated options append hooks,
// which are invoked in option order after the run boundary. Every hook runs
// even if an earlier hook returns an error.
func WithPostRunHook(hook PostRunHook) RunOption {
	return func(c *runConfig) {
		c.postRunHooks = append(c.postRunHooks, hook)
	}
}

// finishRun invokes post-run hooks after the caller has finalized any owning
// state (notably Session commits). Hooks see the original run error rather than
// failures from earlier hooks, while the returned error joins every failure.
func finishRun(ctx context.Context, cfg runConfig, result RunResult, runErr error) (RunResult, error) {
	if len(cfg.postRunHooks) == 0 {
		return result, runErr
	}

	hookCtx := context.WithoutCancel(ctx)
	errs := make([]error, 0, len(cfg.postRunHooks)+1)
	if runErr != nil {
		errs = append(errs, runErr)
	}
	for i, hook := range cfg.postRunHooks {
		if err := hook(hookCtx, result, runErr); err != nil {
			errs = append(errs, fmt.Errorf("post-run hook %d failed: %w", i, err))
		}
	}
	if len(errs) == 0 {
		return result, nil
	}
	if len(errs) == 1 {
		return result, errs[0]
	}
	return result, errors.Join(errs...)
}
