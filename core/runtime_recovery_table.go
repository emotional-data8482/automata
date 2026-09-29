package core

// recoveryFacts contains observations made by the executor. The classifier
// does no I/O and never trusts these observations instead of a transaction's
// generation check. Batch facts are supplied only after inspecting invocations.
type recoveryFacts struct {
	liveWorker        bool
	deadlineChecked   bool
	deadlineFinalized bool
	childrenChecked   bool
	childrenReady     bool
	waitsChecked      bool
	waitsReady        bool
	bindingRegistered bool
	payloadFits       bool
	acceptedFinal     bool
	batchInspected    bool
	batchUncertain    bool
	maxFreshAttempts  int
}

type recoveryAction uint8

const (
	recoveryLeave recoveryAction = iota
	recoveryStart
	recoveryMaintainSuspended
	recoveryInspectChildren
	recoveryExpireWaits
	recoveryInspectBatch
	recoveryResume
	recoveryExecutionAttention
	recoveryProviderFresh
	recoveryProviderAttention
	recoveryCancel
	recoveryFinishHooks
	recoveryHookAttention
)

// classifyRecovery is the recovery transition table. Priority matters:
// cancellation wins over uncertain effects, provider attention is policy-gated,
// and execution attention may resume only from a known safe boundary. Unknown
// boundaries are conservative; neither tools nor provider calls are replayed.
func classifyRecovery(record storedRuntimeRun, facts recoveryFacts) recoveryAction {
	if facts.liveWorker {
		return recoveryLeave
	}
	if suspendedForDeadline(record) {
		switch {
		case !facts.deadlineChecked:
			return recoveryMaintainSuspended
		case facts.deadlineFinalized:
			return recoveryFinishHooks
		case !facts.childrenChecked:
			return recoveryInspectChildren
		case facts.childrenReady:
			return recoveryStart
		case record.State != RuntimeWaiting:
			return recoveryLeave
		case !facts.waitsChecked:
			return recoveryExpireWaits
		case facts.waitsReady:
			return recoveryStart
		default:
			return recoveryLeave
		}
	}
	switch record.State {
	case RuntimeReady:
		if facts.bindingRegistered {
			return recoveryStart
		}
		return recoveryLeave
	case RuntimeFinalizing:
		if len(record.HookDelivery) == 0 {
			return recoveryFinishHooks
		}
		return recoveryHookAttention
	case RuntimeNeedsAttention:
		switch record.AttentionKind {
		case AttentionProvider:
			if record.FreshAttempts < facts.maxFreshAttempts {
				return recoveryProviderFresh
			}
			return recoveryLeave
		case AttentionExecution:
			if !facts.payloadFits {
				return recoveryLeave
			}
			if record.PendingBatchID == "" && !recoverySafeBoundary(record.LastTransition) {
				return recoveryLeave
			}
		default:
			return recoveryLeave
		}
	case RuntimeRunning:
		if providerAttemptMayBeInFlight(record) {
			if record.FreshAttempts < facts.maxFreshAttempts {
				return recoveryProviderFresh
			}
			return recoveryProviderAttention
		}
	case RuntimeCancelRequested:
		// Inspect dispatched invocations before finalizing cancellation.
		if record.PendingBatchID != "" && !facts.batchInspected {
			return recoveryInspectBatch
		}
		return recoveryCancel
	default:
		return recoveryLeave
	}
	if record.PendingBatchID != "" {
		if !facts.batchInspected {
			return recoveryInspectBatch
		}
		if facts.batchUncertain {
			return recoveryExecutionAttention
		}
		return recoveryResume
	}
	if recoverySafeBoundary(record.LastTransition) || (record.LastTransition == transitionProviderAccepted && facts.acceptedFinal) {
		return recoveryResume
	}
	return recoveryExecutionAttention
}

func recoverySafeBoundary(kind transitionKind) bool {
	switch kind {
	case transitionNone, transitionProviderAttemptRecovered, transitionBatchReady, transitionResponseClassified, transitionBatchCommitted:
		return true
	}
	return false
}

func providerAttemptMayBeInFlight(record storedRuntimeRun) bool {
	return record.PendingBatchID == "" && record.LastTransition == transitionProviderAttemptStarted
}
