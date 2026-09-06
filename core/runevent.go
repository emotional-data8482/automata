package core

import "context"

// RunEventKind is a stable wire spelling. Observers must tolerate future kinds.
// These contracts are staged for dispatcher integration in Task 7.
type RunEventKind string

const (
	// RunStarted identifies run_started.
	RunStarted RunEventKind = "run_started"
	// TurnStarted identifies turn_started.
	TurnStarted RunEventKind = "turn_started"
	// ProviderRequestPrepared identifies provider_request_prepared.
	ProviderRequestPrepared RunEventKind = "provider_request_prepared"
	// ProviderResponseAccepted identifies provider_response_accepted.
	ProviderResponseAccepted RunEventKind = "provider_response_accepted"
	// TextDelta identifies text_delta.
	TextDelta RunEventKind = "text_delta"
	// ThinkingDelta identifies thinking_delta.
	ThinkingDelta RunEventKind = "thinking_delta"
	// UsageReported identifies usage_reported.
	UsageReported RunEventKind = "usage_reported"
	// ToolBatchStarted identifies tool_batch_started.
	ToolBatchStarted RunEventKind = "tool_batch_started"
	// ToolCallRequested identifies tool_call_requested.
	ToolCallRequested RunEventKind = "tool_call_requested"
	// ToolCallFinished identifies tool_call_finished.
	ToolCallFinished RunEventKind = "tool_call_finished"
	// ToolBatchFinished identifies tool_batch_finished.
	ToolBatchFinished RunEventKind = "tool_batch_finished"
	// TurnFinished identifies turn_finished.
	TurnFinished RunEventKind = "turn_finished"
	// CheckpointCommitted identifies checkpoint_committed.
	CheckpointCommitted RunEventKind = "checkpoint_committed"
	// RunFinished identifies run_finished.
	RunFinished RunEventKind = "run_finished"
)

// RunObserver receives synchronous, detached observations. Observation does not
// select streaming transport. Independent roots may call an observer concurrently.
type RunObserver func(context.Context, RunEvent)

// RunEvent identifies the source run. Forwarding preserves every envelope field.
// Sequence is one-based per source; Turn is one-based or zero for run boundaries.
type RunEvent struct {
	Kind               RunEventKind
	RunID              string
	ParentRunID        string
	ParentInvocationID string
	Turn               int
	Sequence           uint64
	Payload            RunEventPayload
}

// RunEventPayload is the closed set of typed core observation payloads.
type RunEventPayload interface{ runEventPayload() }

// ToolCallData separates library identity from provider correlation and retains
// the requested input even when an approver supplies different effective input.
type ToolCallData struct {
	InvocationID string
	ToolCallID   string
	Requested    ToolUseBlock
	Effective    ToolUseBlock
}

// RunStartedPayload is the payload for RunStarted.
// Before validation or task append; run-level (Turn 0).
type RunStartedPayload struct{ Mode string }

func (RunStartedPayload) runEventPayload() {}

// TurnStartedPayload is the payload for TurnStarted.
// After turn reservation, before request preparation; turn-scoped.
type TurnStartedPayload struct{}

func (TurnStartedPayload) runEventPayload() {}

// ProviderRequestPreparedPayload is the payload for ProviderRequestPrepared.
// After request validation, once per logical turn; turn-scoped.
type ProviderRequestPreparedPayload struct{ Request Request }

func (ProviderRequestPreparedPayload) runEventPayload() {}

// ProviderResponseAcceptedPayload is the payload for ProviderResponseAccepted.
// After usage_reported, including partial outcomes; turn-scoped.
type ProviderResponseAcceptedPayload struct {
	Response    Response
	Diagnostics []RunDiagnostic
}

func (ProviderResponseAcceptedPayload) runEventPayload() {}

// TextDeltaPayload is the payload for TextDelta.
// Before accepted usage/response; turn-scoped.
type TextDeltaPayload struct{ Text string }

func (TextDeltaPayload) runEventPayload() {}

// ThinkingDeltaPayload is the payload for ThinkingDelta.
// Before accepted usage/response; turn-scoped.
type ThinkingDeltaPayload struct{ Text string }

func (ThinkingDeltaPayload) runEventPayload() {}

// UsageReportedPayload is the payload for UsageReported.
// Once per accepted response, before response acceptance; local turn usage.
type UsageReportedPayload struct{ Usage Usage }

func (UsageReportedPayload) runEventPayload() {}

// ToolBatchStartedPayload is the payload for ToolBatchStarted.
// Before all call-requested events; calls in model order; turn-scoped.
type ToolBatchStartedPayload struct{ Calls []ToolCallData }

func (ToolBatchStartedPayload) runEventPayload() {}

// ToolCallRequestedPayload is the payload for ToolCallRequested.
// Before execution; all requests precede any finished calls; turn-scoped.
type ToolCallRequestedPayload struct{ Call ToolCallData }

func (ToolCallRequestedPayload) runEventPayload() {}

// ToolCallFinishedPayload is the payload for ToolCallFinished.
// Actual/synthetic result in completion order; turn-scoped.
type ToolCallFinishedPayload struct {
	Call      ToolCallData
	Result    ToolResult
	Synthetic bool
	Err       error
}

func (ToolCallFinishedPayload) runEventPayload() {}

// ToolBatchFinishedPayload is the payload for ToolBatchFinished.
// After every call is reconciled; results in model order; turn-scoped.
type ToolBatchFinishedPayload struct {
	Results []ToolCallFinishedPayload
	Err     error
}

func (ToolBatchFinishedPayload) runEventPayload() {}

// TurnFinishedPayload is the payload for TurnFinished.
// Exactly once per started turn, including preparation failure.
type TurnFinishedPayload struct {
	Response *Response
	Results  []ToolCallFinishedPayload
	Err      error
}

func (TurnFinishedPayload) runEventPayload() {}

// CheckpointCommittedPayload is the payload for CheckpointCommitted.
// After in-memory commit, before checkpoint hooks; turn-scoped (0 if no turn started).
type CheckpointCommittedPayload struct{ Checkpoint Checkpoint }

func (CheckpointCommittedPayload) runEventPayload() {}

// RunFinishedPayload is the payload for RunFinished.
// After post-run hooks and error aggregation; run-level (Turn 0).
type RunFinishedPayload struct {
	Result RunResult
	Err    error
}

func (RunFinishedPayload) runEventPayload() {}

// newRunEvent derives the kind from the payload so production cannot mismatch it.
// The dispatcher will own sequencing and detached payload construction.
func newRunEvent(scope *runScope, turn int, sequence uint64, payload RunEventPayload) RunEvent {
	var kind RunEventKind
	switch payload.(type) {
	case RunStartedPayload:
		kind = RunStarted
	case TurnStartedPayload:
		kind = TurnStarted
	case ProviderRequestPreparedPayload:
		kind = ProviderRequestPrepared
	case ProviderResponseAcceptedPayload:
		kind = ProviderResponseAccepted
	case TextDeltaPayload:
		kind = TextDelta
	case ThinkingDeltaPayload:
		kind = ThinkingDelta
	case UsageReportedPayload:
		kind = UsageReported
	case ToolBatchStartedPayload:
		kind = ToolBatchStarted
	case ToolCallRequestedPayload:
		kind = ToolCallRequested
	case ToolCallFinishedPayload:
		kind = ToolCallFinished
	case ToolBatchFinishedPayload:
		kind = ToolBatchFinished
	case TurnFinishedPayload:
		kind = TurnFinished
	case CheckpointCommittedPayload:
		kind = CheckpointCommitted
	case RunFinishedPayload:
		kind = RunFinished
	default:
		panic("unsupported run event payload")
	}
	return RunEvent{Kind: kind, RunID: scope.id, ParentRunID: scope.parentRunID, ParentInvocationID: scope.parentInvocationID, Turn: turn, Sequence: sequence, Payload: payload}
}
