package core

import "context"

// commitTerminalTx is the only terminal state writer. Callers retain ownership
// of the result, error, generation, and receipts; conversation release always
// commits with the terminal record. Cancellation does not acquire new hooks.
func commitTerminalTx(tx StoreTransaction, record *storedRuntimeRun) error {
	record.State = RuntimeTerminal
	record.AttentionReason, record.AttentionKind = "", ""
	if err := releaseConversationTx(tx, *record); err != nil {
		return err
	}
	return putRuntimeRun(tx, *record)
}

// setRunAttention is the only attention state writer. It does not increment
// Generation: callers do that once for their enclosing lifecycle transition.
func setRunAttention(record *storedRuntimeRun, kind AttentionKind, reason string) {
	record.State = RuntimeNeedsAttention
	record.AttentionKind, record.AttentionReason = kind, reason
}

// afterRunDisposition is the common parent-notification path. The committed
// event capture already publishes the view and schedules the driver's repair
// backstop, including descendants terminalized within another run's commit.
// Callers decide whether a notice error is returned or left for that backstop.
func (r *Runtime) afterRunDisposition(ctx context.Context, record storedRuntimeRun) error {
	if record.ParentRunID == "" {
		return nil
	}
	switch record.State {
	case RuntimeTerminal:
		return r.wakeParentFromChild(ctx, record.RunID)
	case RuntimeNeedsAttention:
		return r.notifyParentChildAttention(ctx, record.ParentRunID, record.RunID, record.AttentionReason)
	}
	return nil
}
