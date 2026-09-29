package core

import (
	"context"
	"errors"
	"fmt"
	"time"
)

func (r *Runtime) completeRunHooks(runID string) error {
	// Committed-run hooks observe the record as committed, including its
	// reassembled transcript.
	full, err := r.loadSnapshotRecord(context.Background(), runID)
	if err != nil {
		return err
	}
	if len(r.hooks) > 0 {
		claimed, err := r.startHookDelivery(runID)
		if err != nil || !claimed {
			return err
		}
	}
	return r.finishHooks(runID, r.invokeCommittedRunHooks(full))
}

// startHookDelivery commits HookDelivery before any hook runs. Exactly one
// caller claims delivery for a finalizing run; others leave it alone.
func (r *Runtime) startHookDelivery(runID string) (bool, error) {
	claimed := false
	err := r.transaction(context.Background(), true, func(tx StoreTransaction) error {
		record, err := getRuntimeRun(tx, runID)
		if err != nil {
			return err
		}
		if record.State != RuntimeFinalizing || len(record.HookDelivery) > 0 {
			return nil
		}
		for _, hook := range r.hooks {
			record.HookDelivery = append(record.HookDelivery, hook.Name)
		}
		claimed = true
		return putRuntimeRun(tx, record)
	})
	return claimed, err
}

func (r *Runtime) invokeCommittedRunHooks(record storedRuntimeRun) []RunHookResult {
	results := make([]RunHookResult, 0, len(r.hooks))
	for _, hook := range r.hooks {
		timeout := hook.Timeout
		if timeout == 0 {
			timeout = 30 * time.Second
		}
		ctx, cancel := context.WithTimeout(context.WithoutCancel(r.ctx), timeout)
		err := invokeCommittedRunHook(ctx, hook, snapshotFromRecord(record))
		cancel()
		results = append(results, RunHookResult{Name: hook.Name, Error: errorString(err)})
	}
	return results
}

func invokeCommittedRunHook(ctx context.Context, hook CommittedRunHook, snapshot RunSnapshot) (err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("panic: %v", recovered)
		}
	}()
	return hook.Handle(ctx, snapshot)
}

func (r *Runtime) finishHooks(runID string, results []RunHookResult) error {
	var childOf string
	err := r.transaction(context.Background(), true, func(tx StoreTransaction) error {
		record, err := getRuntimeRun(tx, runID)
		if err != nil {
			return err
		}
		if record.State == RuntimeTerminal {
			// Another finisher already committed this run's terminal state.
			return nil
		}
		if record.State != RuntimeFinalizing {
			return fmt.Errorf("run %s cannot finish hooks from %s", runID, record.State)
		}
		record.HookResults = append([]RunHookResult(nil), results...)
		record.Generation++
		childOf = record.ParentRunID
		return commitTerminalTx(tx, &record)
	})
	if err != nil {
		return err
	}
	if childOf != "" {
		// Central terminal boundary: every path that durably terminalizes a
		// child (execution, cancellation, waiting-deadline finalization,
		// recovery) notifies the parent here. If this wake transaction fails,
		// the parent stays inspectable as Waiting with its pending child wait
		// and Recover consumes the terminal child on the next pass.
		_ = r.afterRunDisposition(context.Background(), storedRuntimeRun{RunID: runID, ParentRunID: childOf, State: RuntimeTerminal})
	}
	return nil
}

func errorString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// hooksAckReceiptKey keys the receipt of a hook acknowledgement, so an exact
// retry after a lost response resolves the original outcome.
func hooksAckReceiptKey(runID string) string { return "hooks\x00" + runID }

// AcknowledgeHooks resolves the attention left when committed-run hook
// delivery was interrupted. The host asserts it has handled whatever the
// interrupted hooks may or may not have done; Runtime never invokes them
// again. The run becomes terminal with its committed execution result
// unchanged, each hook whose delivery had started is recorded as Unknown in
// RunSnapshot.Hooks, and the terminal commit wakes a waiting durable
// parent or advances the run's conversation like any other. An exact retry
// after success returns nil; a run not awaiting hook acknowledgement is an
// error.
func (h *RunHandle) AcknowledgeHooks(ctx context.Context) error {
	var childOf string
	err := h.runtime.transaction(ctx, true, func(tx StoreTransaction) error {
		childOf = ""
		if _, err := tx.Get(runtimeReceiptsBucket, hooksAckReceiptKey(h.runID)); err == nil {
			return nil
		} else if !errors.Is(err, ErrStoreKeyNotFound) {
			return err
		}
		record, err := getRuntimeRun(tx, h.runID)
		if err != nil {
			return err
		}
		if record.State != RuntimeNeedsAttention || record.AttentionKind != AttentionHooks {
			return fmt.Errorf("run %s is not awaiting hook acknowledgement (state %s)", h.runID, record.State)
		}
		record.HookResults = record.HookResults[:0]
		for _, name := range record.HookDelivery {
			record.HookResults = append(record.HookResults, RunHookResult{
				Name: name, Error: "delivery outcome unknown: interrupted and acknowledged", Unknown: true,
			})
		}
		record.Generation++
		childOf = record.ParentRunID
		if err := commitTerminalTx(tx, &record); err != nil {
			return err
		}
		return putStoredJSON(tx, runtimeReceiptsBucket, hooksAckReceiptKey(h.runID), cancelReceipt{
			Version: runtimeEncodingVersion, RunID: h.runID, Generation: record.Generation, ObservedState: RuntimeNeedsAttention,
		})
	})
	if err == nil && childOf != "" {
		err = h.runtime.afterRunDisposition(context.WithoutCancel(ctx), storedRuntimeRun{RunID: h.runID, ParentRunID: childOf, State: RuntimeTerminal})
	}
	return err
}
