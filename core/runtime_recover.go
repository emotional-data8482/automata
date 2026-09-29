package core

import (
	"context"
	"errors"
	"fmt"
)

// runtimeRecoverPageSize bounds how many run records one recovery scan page
// decodes. Records are compact; transcripts load only when a run is inspected.
const runtimeRecoverPageSize = 128

// Recover adopts persisted work that no worker in this process owns, paging
// through the non-terminal runs only. Ready work with a registered binding
// starts. A run whose previous owner stopped resumes from its last committed
// transition when no external work was in flight; an open provider attempt
// follows RuntimeConfig.ProviderRecovery, and a dispatched tool call becomes
// uncertain and needs reconciliation. Suspended runs have their deadlines,
// child outcomes, and wait expiries applied and their timers armed for the
// background driver. Recover may return before background work, including
// committed-run hook delivery, makes a run terminal; use Await or observe its
// snapshot when terminal completion is required. A run whose payload is
// unavailable or too large needs attention while recovery continues with the
// others.
func (r *Runtime) Recover(ctx context.Context) error {
	var records []storedRuntimeRun
	if err := r.transaction(ctx, false, func(tx StoreTransaction) error {
		// Page through the active index, which holds exactly the non-terminal
		// runs, and decode their compact records only: terminal history never
		// adds recovery cost.
		after := ""
		for {
			var page []string
			last, err := tx.ScanPage(runtimeActiveBucket, "", after, runtimeRecoverPageSize, func(runID string, _ []byte) error {
				page = append(page, runID)
				return nil
			})
			if err != nil {
				return err
			}
			// Records are read after the page's scan closes, so no adapter has
			// to serve a read inside an open scan.
			for _, runID := range page {
				record, err := getRuntimeRun(tx, runID)
				if err != nil {
					return fmt.Errorf("active run %s: %w", runID, err)
				}
				records = append(records, record)
			}
			if last == "" {
				return nil
			}
			after = last
		}
	}); err != nil {
		return err
	}
	for _, record := range records {
		err := r.recoverRecord(ctx, record)
		if errors.Is(err, ErrPayloadUnavailable) || errors.Is(err, ErrPayloadTooLarge) {
			// Report the unreadable or unstorable payload on its run and keep
			// recovering the others; never invent or truncate history.
			err = r.markPayloadAttention(ctx, record.RunID, err)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// recoverRecord applies the recovery decision for one scanned run.
func (r *Runtime) recoverRecord(ctx context.Context, record storedRuntimeRun) error {
	r.mu.Lock()
	_, live := r.live[record.RunID]
	r.mu.Unlock()
	_, bindingErr := r.binding(record.definition())
	facts := recoveryFacts{liveWorker: live, bindingRegistered: bindingErr == nil, payloadFits: r.payloadFits(record), maxFreshAttempts: r.providers.MaxFreshAttempts}
	action := classifyRecovery(record, facts)
	if action == recoveryLeave {
		return nil
	}
	if record.State == RuntimeRunning || record.State == RuntimeCancelRequested {
		if err := r.recordInterruptedAttempt(ctx, record); err != nil {
			return err
		}
	}
	switch action {
	case recoveryStart:
		r.start(record.RunID)
	case recoveryMaintainSuspended:
		// Enforce the deadline, consume children that settled while this
		// owner was away, expire elapsed waits, and arm the driver for
		// whatever is still pending.
		if err := r.maintainSuspended(ctx, record, r.completeRunHooks); err != nil {
			return err
		}
		return r.armSuspended(ctx, record.RunID)
	case recoveryProviderFresh, recoveryProviderAttention:
		return r.recoverProviderAttempt(ctx, record)
	case recoveryInspectBatch, recoveryResume, recoveryExecutionAttention, recoveryCancel:
		resumable, err := r.recoverToolBatch(ctx, record)
		if err != nil {
			return err
		}
		if resumable {
			r.start(record.RunID)
		} else if record.State == RuntimeRunning {
			return r.markAttention(ctx, record.RunID, record.Generation, "previous owner stopped during execution")
		}
	case recoveryFinishHooks:
		// No hook was invoked for this committed result, so finishing
		// it cannot repeat a delivery.
		return r.completeRunHooks(record.RunID)
	case recoveryHookAttention:
		return r.markAttentionKind(ctx, record.RunID, record.Generation, AttentionHooks, "previous owner stopped while delivering committed-run hooks; delivery outcome is unknown")
	}
	return nil
}

// recordInterruptedAttempt persists unknown provider-attempt evidence for a
// run whose previous owner stopped while its next step was a provider call:
// that attempt may have been dispatched, and its usage, if any, was never
// recorded. It changes no lifecycle state, so the recovery transition that
// follows still matches the scanned generation, and the generation marker
// makes a repeated recovery pass count the same interruption once.
func (r *Runtime) recordInterruptedAttempt(ctx context.Context, scanned storedRuntimeRun) error {
	return r.transaction(ctx, true, func(tx StoreTransaction) error {
		record, err := getRuntimeRun(tx, scanned.RunID)
		if err != nil {
			return err
		}
		if record.Generation != scanned.Generation || record.UnknownAttemptGeneration == record.Generation {
			return nil
		}
		if record.State != RuntimeRunning && record.State != RuntimeCancelRequested {
			return nil
		}
		if !providerAttemptMayBeInFlight(record) {
			return nil
		}
		// The attempt started, so it counts like any attempt; its outcome and
		// usage are unknown. A graceful stop records the same pair.
		record.Result.ProviderAttempts++
		record.UnknownAttempts++
		record.UnknownAttemptGeneration = record.Generation
		return putRuntimeRun(tx, record)
	})
}

// recoverProviderAttempt resolves a run whose provider attempt was
// interrupted. Within the ProviderRecovery bound it starts a fresh attempt,
// recorded as such; otherwise the run needs attention. The scanned generation
// guards against acting on a run that moved on.
func (r *Runtime) recoverProviderAttempt(ctx context.Context, scanned storedRuntimeRun) error {
	var committed storedRuntimeRun
	ready := false
	err := r.transaction(ctx, true, func(tx StoreTransaction) error {
		record, err := getRuntimeRun(tx, scanned.RunID)
		if err != nil {
			return err
		}
		if record.Generation != scanned.Generation {
			return nil
		}
		switch classifyRecovery(record, recoveryFacts{maxFreshAttempts: r.providers.MaxFreshAttempts}) {
		case recoveryProviderFresh:
			record.FreshAttempts++
			record.State = RuntimeReady
			record.AttentionReason, record.AttentionKind = "", ""
			record.LastTransition = transitionProviderAttemptRecovered
			ready = true
		case recoveryProviderAttention:
			setRunAttention(&record, AttentionProvider, "previous owner stopped during execution: a provider attempt may have been sent and its outcome and usage are unknown")
		default:
			return nil
		}
		record.Generation++
		committed = record
		if err := putRuntimeRun(tx, record); err != nil {
			return err
		}
		if ready && record.ParentRunID != "" {
			// A parent blocked on this child's provider attention returns to
			// waiting once the child can run again.
			return refreshParentChildAttentionTx(tx, record.ParentRunID)
		}
		return nil
	})
	if err != nil || committed.RunID == "" {
		return err
	}
	if ready {
		r.start(committed.RunID)
		return nil
	}
	return r.notifyParentOfAttention(ctx, committed)
}

func (r *Runtime) recoverToolBatch(ctx context.Context, record storedRuntimeRun) (bool, error) {
	if record.PendingBatchID == "" {
		if classifyRecovery(record, recoveryFacts{}) == recoveryCancel {
			full, err := r.load(ctx, record.RunID)
			if err != nil {
				return false, err
			}
			return false, r.completeExecution(record.RunID, full.Result, context.Canceled, nil)
		}
		facts := recoveryFacts{payloadFits: r.payloadFits(record)}
		if record.State == RuntimeRunning && record.LastTransition == transitionProviderAccepted {
			full, err := r.load(ctx, record.RunID)
			if err != nil {
				return false, err
			}
			messages := full.Result.Messages
			if len(messages) > 0 {
				last := messages[len(messages)-1]
				facts.acceptedFinal = last.Role == "assistant" && len(last.ToolUses()) == 0
			}
		}
		if classifyRecovery(record, facts) == recoveryResume {
			// No provider attempt is in flight: every attempt commits its record
			// before sending, and none is open. A run with no transition yet
			// stopped before its first attempt record.
			return true, r.makeRunReady(ctx, record.RunID, record.Generation)
		}
		return false, nil
	}
	var hasUncertain, cancelled bool
	var committed storedRuntimeRun
	err := r.transaction(ctx, true, func(tx StoreTransaction) error {
		hasUncertain = false
		current, err := getRuntimeRun(tx, record.RunID)
		if err != nil {
			return err
		}
		if current.Generation != record.Generation {
			return nil
		}
		if current.State == RuntimeCancelRequested {
			if err := resolveReservedInvocations(tx, current); err != nil {
				return err
			}
		}
		invocations, err := loadStoredInvocations(tx, record.RunID, record.PendingBatchID)
		if err != nil {
			return err
		}
		for i := range invocations {
			if invocations[i].State == ToolInvocationDispatched {
				invocations[i].State = ToolInvocationUncertain
				invocations[i].Effect = EffectReport{Status: EffectUnknown}
				if err := putStoredJSON(tx, runtimeInvocationsBucket, invocationStorageKey(record.RunID, record.PendingBatchID, invocations[i].Ordinal), invocations[i]); err != nil {
					return err
				}
			}
			if invocations[i].State == ToolInvocationUncertain {
				hasUncertain = true
			}
		}
		action := classifyRecovery(current, recoveryFacts{payloadFits: r.payloadFits(current), batchInspected: true, batchUncertain: hasUncertain})
		cancelled = action == recoveryCancel
		switch action {
		case recoveryCancel:
			current.State = RuntimeFinalizing
			current.Result.Status = RunCancelled
			current.AttentionReason = ""
			current.AttentionKind = ""
			setRuntimeError(&current, context.Canceled)
		case recoveryExecutionAttention:
			setRunAttention(&current, AttentionExecution, ErrToolEffectUncertain.Error())
		case recoveryResume:
			current.State = RuntimeReady
			current.AttentionReason = ""
			current.AttentionKind = ""
		default:
			return nil
		}
		current.Generation++
		committed = current
		return putRuntimeRun(tx, current)
	})
	if err == nil && cancelled {
		return false, r.completeRunHooks(record.RunID)
	}
	if err == nil && hasUncertain {
		err = r.notifyParentOfAttention(ctx, committed)
	}
	return committed.RunID != "" && committed.State == RuntimeReady, err
}
