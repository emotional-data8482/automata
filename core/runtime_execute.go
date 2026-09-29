package core

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// payloadFits reports whether a run blocked on a payload may resume: it has
// no payload block, or the configured limit now covers the rejected size.
// An unavailable payload never resumes on its own.
func (r *Runtime) payloadFits(record storedRuntimeRun) bool {
	return record.PayloadError == "" || (record.PayloadNeeded > 0 && record.PayloadNeeded <= r.maxPayload)
}

// recordPayloadBlock stores a payload failure on the record.
func recordPayloadBlock(record *storedRuntimeRun, cause error) {
	record.PayloadError, record.PayloadNeeded = cause.Error(), 0
	var tooLarge *payloadTooLargeError
	if errors.As(cause, &tooLarge) {
		record.PayloadNeeded = tooLarge.size
	}
}

// markPayloadAttention records that a run's durable payload cannot be read or
// stored. The run needs attention and recovery leaves it there (see
// payloadFits). An acknowledged cancellation still ends the run: it finishes
// from the compact record, without committed-run hooks, since the history they
// would observe is unreadable. A terminal run keeps its state; its reads
// report the error.
func (r *Runtime) markPayloadAttention(ctx context.Context, runID string, cause error) error {
	var committed storedRuntimeRun
	terminal := false
	err := r.transaction(ctx, true, func(tx StoreTransaction) error {
		committed, terminal = storedRuntimeRun{}, false
		record, err := getRuntimeRun(tx, runID)
		if err != nil || record.State == RuntimeTerminal {
			return err
		}
		recordPayloadBlock(&record, cause)
		if record.State == RuntimeCancelRequested {
			record.State = RuntimeTerminal
			record.Result.Status = RunCancelled
			record.AttentionReason, record.AttentionKind = "", ""
			setRuntimeError(&record, context.Canceled)
			record.Result.Diagnostics = append(record.Result.Diagnostics, RunDiagnostic{Kind: "payload_error", Message: cause.Error()})
			if err := releaseConversationTx(tx, record); err != nil {
				return err
			}
			terminal = true
		} else {
			record.State = RuntimeNeedsAttention
			record.AttentionKind = AttentionExecution
			record.AttentionReason = cause.Error()
		}
		record.Generation++
		committed = record
		return putRuntimeRun(tx, record)
	})
	if err != nil || committed.RunID == "" {
		return err
	}
	if terminal {
		if committed.ParentRunID != "" {
			return r.wakeParentFromChild(context.WithoutCancel(ctx), runID)
		}
		return nil
	}
	return r.notifyParentOfAttention(ctx, committed)
}

func (r *Runtime) start(runID string) {
	r.mu.Lock()
	if r.closing || r.closed {
		r.mu.Unlock()
		return
	}
	if live, ok := r.live[runID]; ok {
		live.restart = true
		r.live[runID] = live
		r.mu.Unlock()
		return
	}
	ctx, cancel := context.WithCancel(r.ctx)
	r.live[runID] = liveRuntimeRun{cancel: cancel}
	r.wg.Add(1)
	r.mu.Unlock()
	go r.execute(ctx, runID)
}

func (r *Runtime) execute(workerCtx context.Context, runID string) {
	logicalComplete := true
	stale := false
	defer func() {
		r.mu.Lock()
		live := r.live[runID]
		delete(r.live, runID)
		restart := live.restart && !r.closing && !r.closed
		var nextCtx context.Context
		if restart {
			var cancel context.CancelFunc
			nextCtx, cancel = context.WithCancel(r.ctx)
			r.live[runID] = liveRuntimeRun{cancel: cancel}
			r.wg.Add(1)
		}
		r.mu.Unlock()
		if logicalComplete {
			r.publishTerminal(runID)
		}
		if restart {
			go r.execute(nextCtx, runID)
		} else if !logicalComplete || stale {
			// The run suspended, or this was a stale start. A due time that
			// fired while this worker was live was dropped; rearm now that no
			// worker owns the run.
			_ = r.armSuspended(r.ctx, runID)
		}
		r.wg.Done()
	}()
	record, claimed, err := r.claim(workerCtx, runID)
	if err != nil {
		r.setFailure(runID, err)
		return
	}
	if !claimed {
		stale = true
		return
	}
	r.mu.Lock()
	delete(r.failures, runID)
	r.mu.Unlock()
	binding, err := r.binding(record.definition())
	if err != nil {
		if markErr := r.markAttention(context.Background(), runID, record.Generation, err.Error()); markErr != nil {
			r.setFailure(runID, errors.Join(err, markErr))
		}
		return
	}
	execCtx := workerCtx
	var cancel context.CancelFunc
	if !record.Deadline.IsZero() {
		execCtx, cancel = context.WithDeadline(workerCtx, record.Deadline)
		defer cancel()
	}
	// Load with the worker context: an expired logical deadline must finalize
	// the claimed run below, not strand it running behind a failed read.
	record, err = r.load(workerCtx, runID)
	if errors.Is(err, ErrPayloadUnavailable) {
		// The committed history cannot be read truthfully; the run needs
		// attention rather than a guessed continuation.
		if markErr := r.markPayloadAttention(context.Background(), runID, err); markErr != nil {
			r.setFailure(runID, errors.Join(err, markErr))
		}
		return
	}
	if err != nil {
		r.setFailure(runID, err)
		return
	}
	cfg := binding.agent.newRunConfig()
	// Restore the persisted correction-turn count so a restarted correction
	// continues the original budget rather than resetting it.
	if cfg.structuredOutput != nil {
		cfg.structuredOutput.correctionsUsed = record.Corrections
	}
	cfg.resume = record.TranscriptMessages > 0
	cfg.resumeTools = append([]string(nil), record.EffectiveTools...)
	var transitionErr error
	cfg.durableTransition = func(ctx context.Context, transition durableLoopTransition) error {
		err := r.persistTransition(ctx, runID, transition)
		if err != nil {
			transitionErr = err
		}
		return err
	}
	cfg.durableBatch = func(ctx context.Context, l *loop, calls []ToolUseBlock) ([]Message, error) {
		return r.executeDurableToolBatch(ctx, runID, l, calls, binding.agent.toolPolicy)
	}
	history := record.Result.Messages
	scope := &runScope{id: runID, result: cloneRunResult(record.Result),
		turns: record.Result.Turns, providerAttempts: record.Result.ProviderAttempts, usage: record.Result.Usage}
	scope.result.Messages = cloneMessages(history)
	if err := binding.agent.validateRun(cfg); err != nil {
		result, finishErr := scope.finalize(scope.result, err)
		if commitErr := r.completeExecution(runID, result, finishErr, workerCtx.Err()); commitErr != nil {
			r.setFailure(runID, commitErr)
		}
		return
	}
	cfg.scope = scope
	r.attachAncestors(workerCtx, record)
	result := cloneRunResult(record.Result)
	var runErr error
	if record.LastTransition == "batch_committed" && record.Error != "" {
		// The batch and its fatal outcome committed before the previous worker
		// stopped. Only finalization remains; do not ask the provider to continue.
		runErr = snapshotError(snapshotFromRecord(record))
	} else {
		result, runErr = binding.agent.runStream(execCtx, newLoop(binding.agent, history), record.Task, func(event StreamEvent) {
			r.publish(runID, event)
		}, cfg)
	}
	result, runErr = scope.finalize(result, runErr)
	if transitionErr != nil {
		if markErr := r.markAttentionWithResult(context.Background(), runID, result, "durable transition failed: "+transitionErr.Error()); markErr != nil {
			r.setFailure(runID, errors.Join(transitionErr, markErr))
		}
		return
	}
	if errors.Is(runErr, errDurableWaiting) {
		logicalComplete = false
		return
	}
	if errors.Is(runErr, errDurableBatchIncomplete) {
		if markErr := r.markAttentionWithResult(context.Background(), runID, result, runErr.Error()); markErr != nil {
			r.setFailure(runID, errors.Join(runErr, markErr))
		}
		return
	}
	if commitErr := r.completeExecution(runID, result, runErr, workerCtx.Err()); commitErr != nil {
		r.setFailure(runID, commitErr)
	}
}

func (r *Runtime) completeExecution(runID string, result RunResult, runErr, workerErr error) error {
	record, err := r.finishExecution(runID, result, runErr, workerErr)
	if err != nil {
		return err
	}
	if record.State == RuntimeNeedsAttention {
		// Best effort, like the terminal wake: the child's attention already
		// committed, and Recover rechecks a waiting parent's children.
		_ = r.notifyParentOfAttention(context.Background(), record)
		return nil
	}
	return r.completeRunHooks(runID)
}

// notifyParentOfAttention surfaces a child's committed attention on its
// waiting parent so required-child attention blocks ordinary parent
// continuation visibly, not just through a later recovery pass.
func (r *Runtime) notifyParentOfAttention(ctx context.Context, record storedRuntimeRun) error {
	if record.ParentRunID == "" {
		return nil
	}
	return r.notifyParentChildAttention(ctx, record.ParentRunID, record.RunID, record.AttentionReason)
}

func (r *Runtime) persistTransition(ctx context.Context, runID string, transition durableLoopTransition) error {
	return r.transaction(ctx, true, func(tx StoreTransaction) error {
		record, err := getRuntimeRun(tx, runID)
		if err != nil {
			return err
		}
		if record.State != RuntimeRunning && record.State != RuntimeCancelRequested {
			return fmt.Errorf("run %s cannot commit transition from %s", runID, record.State)
		}
		if err := appendTranscript(tx, runID, &record, transition.Result.Messages); err != nil {
			return err
		}
		if transition.Kind == "batch_committed" {
			if err := commitPendingToolBatch(tx, &record); err != nil {
				return err
			}
		}
		record.Result = cloneRunResult(transition.Result)
		record.Result.Messages = nil
		record.Corrections = transition.StructuredCorrections
		record.LastTransition = transition.Kind
		if transition.Kind == "provider_accepted" {
			record.EffectiveTools = append([]string(nil), transition.EffectiveTools...)
		}
		record.Generation++
		return putRuntimeRun(tx, record)
	})
}

func (r *Runtime) claim(ctx context.Context, runID string) (storedRuntimeRun, bool, error) {
	var claimed storedRuntimeRun
	var didClaim bool
	err := r.transaction(ctx, true, func(tx StoreTransaction) error {
		record, err := getRuntimeRun(tx, runID)
		if err != nil {
			return err
		}
		switch record.State {
		case RuntimeRunning, RuntimeWaiting, RuntimeCancelRequested, RuntimeFinalizing, RuntimeNeedsAttention, RuntimeTerminal:
			// A stale scheduling attempt, such as a restart requested while the
			// previous worker was suspending the run, is benign: the transition
			// that makes the run ready again schedules it.
			return nil
		case RuntimeReady:
			// Claim below.
		default:
			return fmt.Errorf("run %s has invalid state %q", runID, record.State)
		}
		record.State = RuntimeRunning
		record.Generation++
		if err := putRuntimeRun(tx, record); err != nil {
			return err
		}
		claimed = record
		didClaim = true
		return nil
	})
	return claimed, didClaim, err
}

func (r *Runtime) finishExecution(runID string, result RunResult, runErr, workerErr error) (storedRuntimeRun, error) {
	var committed storedRuntimeRun
	err := r.transaction(context.Background(), true, func(tx StoreTransaction) error {
		record, err := getRuntimeRun(tx, runID)
		if err != nil {
			return err
		}
		if err := appendTranscript(tx, runID, &record, result.Messages); err != nil {
			return err
		}
		record.Result = cloneRunResult(result)
		record.Result.Messages = nil
		record.Generation++
		switch {
		case record.State == RuntimeCancelRequested:
			record.State = RuntimeFinalizing
			record.Result.Status = RunCancelled
			setRuntimeError(&record, context.Canceled)
		case workerErr != nil:
			record.State = RuntimeNeedsAttention
			record.AttentionReason = "worker stopped during execution"
			record.AttentionKind = AttentionExecution
			if record.LastTransition == transitionProviderAttemptStarted {
				// The stopped attempt may have reached the provider: its outcome
				// and usage are unknown, exactly as after a crash, and the same
				// recovery policy decides whether a fresh attempt may follow.
				record.AttentionReason = "worker stopped during execution: a provider attempt may have been sent and its outcome and usage are unknown"
				record.AttentionKind = AttentionProvider
				record.UnknownAttempts++
			}
			setRuntimeError(&record, runErr)
		default:
			record.State = RuntimeFinalizing
			setRuntimeError(&record, runErr)
		}
		if err := putRuntimeRun(tx, record); err != nil {
			return err
		}
		committed = record
		return nil
	})
	return committed, err
}

func (r *Runtime) markAttentionWithResult(ctx context.Context, runID string, result RunResult, reason string) error {
	var cancelled bool
	var committed storedRuntimeRun
	err := r.transaction(ctx, true, func(tx StoreTransaction) error {
		record, err := getRuntimeRun(tx, runID)
		if err != nil {
			return err
		}
		batchResults := record.PendingBatchID != "" && len(result.Messages) > record.TranscriptMessages
		if err := appendTranscript(tx, runID, &record, result.Messages); errors.Is(err, ErrPayloadTooLarge) {
			// The in-memory progress cannot be stored whole. Keep the committed
			// transcript and final message, never a truncated suffix, and say
			// why; the accounting the worker knows stays truthful. Recovery
			// resumes it only once the payload limit covers it.
			recordPayloadBlock(&record, err)
			if !strings.Contains(reason, err.Error()) {
				reason += ": " + err.Error()
			}
			record.Result.Turns = result.Turns
			record.Result.ProviderAttempts, record.Result.Usage = result.ProviderAttempts, result.Usage
		} else if err != nil {
			return err
		} else {
			if batchResults {
				// A failed batch transition may leave complete results only in
				// memory. Retain them atomically with the batch marker and fatal
				// outcome, just as the original transition would, never as
				// transcript-only progress.
				if err := commitPendingToolBatch(tx, &record); err != nil {
					return err
				}
				record.LastTransition = "batch_committed"
			}
			record.Result = cloneRunResult(result)
			record.Result.Messages = nil
		}
		cancelled = record.State == RuntimeCancelRequested
		if cancelled {
			// Uncertainty is retained on the invocation, but cannot undo the
			// durable cancellation or allow reconciliation to restart the run.
			record.State = RuntimeFinalizing
			record.Result.Status = RunCancelled
			record.AttentionReason = ""
			setRuntimeError(&record, context.Canceled)
		} else {
			record.State = RuntimeNeedsAttention
			record.AttentionReason = reason
			record.AttentionKind = AttentionExecution
		}
		record.Generation++
		committed = record
		return putRuntimeRun(tx, record)
	})
	if err != nil {
		return err
	}
	if !cancelled {
		_ = r.notifyParentOfAttention(ctx, committed)
		return nil
	}
	return r.completeRunHooks(runID)
}

func setRuntimeError(record *storedRuntimeRun, err error) {
	failure := failureFromError(err)
	record.Error, record.ErrorKind, record.ErrorStopReason, record.ErrorRawReason = "", "", "", ""
	record.ErrorViolations = nil
	if failure != nil {
		record.Error, record.ErrorKind = failure.Message, failure.Kind
		record.ErrorStopReason, record.ErrorRawReason = failure.StopReason, failure.RawReason
		record.ErrorViolations = failure.Violations
	}
}

func failureFromError(err error) *RunFailure {
	if err == nil {
		return nil
	}
	failure := &RunFailure{Message: err.Error()}
	if invalid, ok := errors.AsType[*InvalidStructuredOutputError](err); ok {
		failure.Violations = append([]string(nil), invalid.Violations...)
	}
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		failure.Kind = FailureDeadline
	case errors.Is(err, context.Canceled):
		failure.Kind = FailureCancelled
	case errors.Is(err, ErrMaxTurnsExceeded) && errors.Is(err, ErrInvalidStructuredOutput):
		failure.Kind = FailureMaxTurnsInvalidStructuredOutput
	case errors.Is(err, ErrMaxTurnsExceeded):
		failure.Kind = FailureMaxTurns
	case errors.Is(err, ErrInvalidMaxTurns):
		failure.Kind = FailureInvalidMaxTurns
	case errors.Is(err, ErrEmptyResponse):
		failure.Kind = FailureEmptyResponse
	case errors.Is(err, ErrInvalidStructuredOutput):
		failure.Kind = FailureInvalidStructuredOutput
	default:
		var completion *CompletionError
		if errors.As(err, &completion) {
			failure.Kind = FailureCompletion
			failure.StopReason, failure.RawReason = completion.Reason, completion.RawReason
		} else {
			failure.Kind = FailureGeneric
		}
	}
	return failure
}

func (r *Runtime) setFailure(runID string, err error) {
	if err == nil {
		return
	}
	r.mu.Lock()
	r.failures[runID] = err
	r.mu.Unlock()
	r.hub.wake(runID)
}

func (r *Runtime) failure(runID string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, live := r.live[runID]; live {
		return nil
	}
	return r.failures[runID]
}

func (r *Runtime) markAttention(ctx context.Context, runID string, generation uint64, reason string) error {
	return r.markAttentionKind(ctx, runID, generation, AttentionExecution, reason)
}

func (r *Runtime) markAttentionKind(ctx context.Context, runID string, generation uint64, kind AttentionKind, reason string) error {
	var childOf string
	err := r.transaction(ctx, true, func(tx StoreTransaction) error {
		record, err := getRuntimeRun(tx, runID)
		if err != nil {
			return err
		}
		if record.Generation != generation {
			return nil
		}
		record.State = RuntimeNeedsAttention
		record.AttentionReason = reason
		record.AttentionKind = kind
		record.Generation++
		childOf = record.ParentRunID
		return putRuntimeRun(tx, record)
	})
	if err == nil && childOf != "" {
		// A required child that needs attention blocks ordinary parent
		// continuation. Surface that on the parent while keeping the child's
		// own evidence authoritative on the child run.
		if err := r.notifyParentChildAttention(ctx, childOf, runID, reason); err != nil {
			return err
		}
	}
	return err
}
