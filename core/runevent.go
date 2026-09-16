package core

import "context"

// RunEventKind is a stable wire spelling. Observers must tolerate future kinds.
type RunEventKind string

const (
	// RunStarted identifies run_started.
	RunStarted RunEventKind = "run_started"
	// CheckpointCommitted identifies checkpoint_committed.
	CheckpointCommitted RunEventKind = "checkpoint_committed"
	// RunFinished identifies run_finished.
	RunFinished RunEventKind = "run_finished"
)

// RunObserver receives synchronous, detached observations from direct Agent,
// Session, and typed runs. Observation does not select streaming transport, and
// Runtime does not invoke Agent observers. Independent roots may call an
// observer concurrently.
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

// RunStartedPayload is the payload for RunStarted.
// Before validation or task append; run-level (Turn 0).
type RunStartedPayload struct{ Mode string }

func (RunStartedPayload) runEventPayload() {}

// CheckpointCommittedPayload is the payload for CheckpointCommitted.
// After an in-memory conversation commit; turn-scoped (0 if no turn started).
type CheckpointCommittedPayload struct{ Checkpoint Checkpoint }

func (CheckpointCommittedPayload) runEventPayload() {}

// RunFinishedPayload is the payload for RunFinished.
// After result finalization; run-level (Turn 0).
type RunFinishedPayload struct {
	Result RunResult
	Err    error
}

func (RunFinishedPayload) runEventPayload() {}

// newRunEvent derives the kind from the payload so production cannot mismatch it.
func newRunEvent(scope *runScope, turn int, sequence uint64, payload RunEventPayload) RunEvent {
	var kind RunEventKind
	switch payload.(type) {
	case RunStartedPayload:
		kind = RunStarted
	case CheckpointCommittedPayload:
		kind = CheckpointCommitted
	case RunFinishedPayload:
		kind = RunFinished
	default:
		panic("unsupported run event payload")
	}
	return RunEvent{Kind: kind, RunID: scope.id, ParentRunID: scope.parentRunID, ParentInvocationID: scope.parentInvocationID, Turn: turn, Sequence: sequence, Payload: payload}
}
