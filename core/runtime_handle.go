package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
)

// cancelReceipt is the durable receipt for one cancel command. It commits
// atomically with the state change it drove, so an exact retry resolves the
// original outcome even after a lost acknowledgement and any later commands.
type cancelReceipt struct {
	Version       int          `json:"version"`
	RunID         string       `json:"run_id"`
	Generation    uint64       `json:"generation"`
	ObservedState RuntimeState `json:"observed_state"`
}

func cancelReceiptKey(runID string) string { return "cancel\x00" + runID }

// Handle returns a handle for a run admitted earlier, by this process or a
// previous one. Operations on an unknown run return [ErrRunNotFound].
func (r *Runtime) Handle(runID string) *RunHandle {
	return &RunHandle{runtime: r, runID: runID}
}

// RunHandle addresses one durable run. It holds no resources: handles are
// cheap, and any number of them may address the same run, including from
// [Runtime.Handle] after a restart.
type RunHandle struct {
	runtime *Runtime
	runID   string
}

// ID returns the run's stable identity.
func (h *RunHandle) ID() string { return h.runID }

// Snapshot returns the authoritative committed view of the run, including
// its full transcript, tool batches, waits, and tree accounting. Its cost
// grows with the run's history; use [RunHandle.Events] for incremental
// observation and resynchronize from EventSequence.
func (h *RunHandle) Snapshot(ctx context.Context) (RunSnapshot, error) {
	record, err := h.runtime.loadSnapshotRecord(ctx, h.runID)
	if err != nil {
		return RunSnapshot{}, err
	}
	snapshot := snapshotFromRecord(record)
	if snapshot.State == RuntimeReady && !h.runtime.registered(record) {
		snapshot.State = RuntimeNeedsAttention
		snapshot.Attention = &RunAttention{Kind: AttentionExecution, Reason: ErrDefinitionNotRegistered.Error()}
	}
	return snapshot, nil
}

// loadSnapshotRecord reads everything a host-facing snapshot shows in one read
// transaction: the full record, tree accounting, and the event head. Execution
// paths use loadRuntimeRun, which skips the tree walk.
func (r *Runtime) loadSnapshotRecord(ctx context.Context, runID string) (storedRuntimeRun, error) {
	var record storedRuntimeRun
	err := r.transaction(ctx, false, func(tx StoreTransaction) error {
		var err error
		record, err = loadRuntimeRun(tx, runID)
		if err != nil {
			return err
		}
		if record.Tree, err = loadTreeAccounting(tx, record); err != nil {
			return err
		}
		head, err := getEventHead(tx, runID)
		record.EventSequence = head.Head
		return err
	})
	return record, err
}

// settledState reads only the compact run record and reports whether a view
// waiting for the run to finish should stop: the run is terminal, needs
// attention, or is admitted without a registered binding.
func (h *RunHandle) settledState(ctx context.Context) (storedRuntimeRun, bool, error) {
	var record storedRuntimeRun
	err := h.runtime.transaction(ctx, false, func(tx StoreTransaction) error {
		var err error
		record, err = getRuntimeRun(tx, h.runID)
		return err
	})
	if err != nil {
		return record, false, err
	}
	switch record.State {
	case RuntimeTerminal, RuntimeNeedsAttention:
		return record, true, nil
	case RuntimeReady:
		return record, !h.runtime.registered(record), nil
	}
	return record, false, nil
}

// follow delivers provisional live events until the run settles (see
// settledState) or ctx ends. It wakes on committed transitions of the run
// rather than polling, and reads only the compact record on each wake.
func (h *RunHandle) follow(ctx context.Context, events <-chan runtimeStreamItem, onEvent func(StreamEvent)) error {
	sub := h.runtime.hub.subscribe(h.runID)
	defer sub.close()
	read := true
	for {
		view := sub.next()
		if err := ctx.Err(); err != nil {
			return err
		}
		if read || view.gone || view.closed || mayHaveSettled(view.state) {
			_, settled, err := h.settledState(ctx)
			if err != nil || settled {
				return err
			}
			if view.closed {
				return ErrRuntimeClosed
			}
		}
		read = false
		changed := view.changed
	wait:
		for {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case item := <-events:
				if item.terminal {
					return nil
				}
				if onEvent != nil {
					onEvent(item.event)
				}
			case <-changed:
				break wait
			}
		}
	}
}

// Observe attaches a provisional live view to a run and returns when the run
// settles or ctx ends. Canceling ctx detaches only this view. Delivery is
// bounded and may omit events when the observer is slow; committed facts are
// replayable through [RunHandle.Events], and Snapshot remains authoritative.
func (h *RunHandle) Observe(ctx context.Context, onEvent func(StreamEvent)) error {
	events, unsubscribe := h.runtime.subscribe(h.runID)
	defer unsubscribe()
	return h.follow(ctx, events, onEvent)
}

// Await waits until the run is terminal or needs attention and returns its
// committed result. Waiting wakes on the run's committed transitions and
// reads only the compact run record per wake; the full result, transcript
// included, is read once when the run settles. If ctx ends first, Await
// returns the run identity with the last compact result observed, whose
// Messages are not loaded.
func (h *RunHandle) Await(ctx context.Context) (RunResult, error) {
	lastResult := RunResult{RunID: h.runID}
	sub := h.runtime.hub.subscribe(h.runID)
	defer sub.close()
	read := true
	for {
		view := sub.next()
		changed := view.changed
		if workerErr := h.runtime.failure(h.runID); workerErr != nil {
			if snapshot, err := h.Snapshot(ctx); err == nil {
				lastResult = resultWithRunID(snapshot.Result, h.runID)
			}
			return lastResult, workerErr
		}
		if err := ctx.Err(); err != nil {
			return lastResult, err
		}
		if !read && !view.gone && !view.closed && !mayHaveSettled(view.state) {
			// The committed transition that woke this wait cannot end it.
			select {
			case <-ctx.Done():
				return lastResult, ctx.Err()
			case <-changed:
			}
			continue
		}
		read = false
		record, settled, err := h.settledState(ctx)
		if err != nil {
			return lastResult, err
		}
		lastResult = resultWithRunID(cloneRunResult(record.Result), h.runID)
		if settled {
			snapshot, err := h.Snapshot(ctx)
			if err != nil {
				return lastResult, err
			}
			lastResult = resultWithRunID(snapshot.Result, h.runID)
			switch snapshot.State {
			case RuntimeTerminal:
				return lastResult, snapshotError(snapshot)
			case RuntimeNeedsAttention:
				return lastResult, fmt.Errorf("%w: %s", ErrRunNeedsAttention, snapshot.Attention.Reason)
			}
			// The run moved on between the compact read and the snapshot.
			read = true
			continue
		}
		if view.closed {
			return lastResult, ErrRuntimeClosed
		}
		select {
		case <-ctx.Done():
			return lastResult, ctx.Err()
		case <-changed:
		}
	}
}

func resultWithRunID(result RunResult, runID string) RunResult {
	if result.RunID == "" {
		result.RunID = runID
	}
	return result
}

// Cancel durably cancels the run and every non-terminal descendant in one
// commit. A suspended run becomes terminal at once; a running one stops
// dispatching and finishes as canceled. An exact retry returns the original
// outcome, even after a lost acknowledgement or later transitions.
func (h *RunHandle) Cancel(ctx context.Context) error {
	var cancels []context.CancelFunc
	var canceledChild, parentRunID string
	err := h.runtime.transaction(ctx, true, func(tx StoreTransaction) error {
		// Resolve an exact retry from the persisted receipt before touching
		// current state: a lost acknowledgement followed by any later command
		// still resolves the original outcome without re-deriving state.
		if _, err := tx.Get(runtimeReceiptsBucket, cancelReceiptKey(h.runID)); err == nil {
			return nil
		} else if !errors.Is(err, ErrStoreKeyNotFound) {
			return err
		}
		record, err := getRuntimeRun(tx, h.runID)
		if err != nil {
			return err
		}
		receipt := cancelReceipt{
			Version: runtimeEncodingVersion, RunID: h.runID,
			Generation: record.Generation, ObservedState: record.State,
		}
		if record.State == RuntimeReady || record.State == RuntimeWaiting || record.State == RuntimeNeedsAttention || record.State == RuntimeRunning {
			if err := resolveReservedInvocations(tx, record); err != nil {
				return err
			}
			if err := cancelRunWaits(tx, record.RunID); err != nil {
				return err
			}
		}
		switch record.State {
		case RuntimeFinalizing, RuntimeTerminal:
			return nil
		case RuntimeReady, RuntimeWaiting, RuntimeNeedsAttention:
			record.Result.Status = RunCancelled
			record.AttentionReason = ""
			record.AttentionKind = ""
			setRuntimeError(&record, context.Canceled)
			if record.ParentRunID != "" {
				canceledChild, parentRunID = h.runID, record.ParentRunID
			}
		case RuntimeRunning:
			record.State = RuntimeCancelRequested
		}
		record.Generation++
		if receipt.ObservedState == RuntimeReady || receipt.ObservedState == RuntimeWaiting || receipt.ObservedState == RuntimeNeedsAttention {
			err = commitTerminalTx(tx, &record)
		} else {
			err = putRuntimeRun(tx, record)
		}
		if err != nil {
			return err
		}
		// Required descendants are canceled in this same commit, before any
		// local worker is signaled.
		running, err := propagateCancellationTx(tx, h.runID, context.Canceled)
		if err != nil {
			return err
		}
		data, err := json.Marshal(receipt)
		if err != nil {
			return err
		}
		if err := tx.Put(runtimeReceiptsBucket, cancelReceiptKey(h.runID), data); err != nil {
			return err
		}
		cancels = cancels[:0]
		h.runtime.mu.Lock()
		for _, runID := range append([]string{h.runID}, running...) {
			if live, ok := h.runtime.live[runID]; ok {
				cancels = append(cancels, live.cancel)
			}
		}
		h.runtime.mu.Unlock()
		return nil
	})
	if err == nil {
		for _, cancel := range cancels {
			cancel()
		}
	}
	if err == nil && canceledChild != "" {
		// Cancellation durably terminalized a suspended child run; notify the
		// parent through the same central child-completion boundary so the
		// parent sees the model-visible cancellation outcome.
		_ = h.runtime.afterRunDisposition(context.Background(), storedRuntimeRun{RunID: canceledChild, ParentRunID: parentRunID, State: RuntimeTerminal})
	}
	return err
}
