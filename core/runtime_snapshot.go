package core

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

type RuntimeState string

const (
	RuntimeReady           RuntimeState = "ready"
	RuntimeRunning         RuntimeState = "running"
	RuntimeWaiting         RuntimeState = "waiting"
	RuntimeCancelRequested RuntimeState = "cancel_requested"
	RuntimeFinalizing      RuntimeState = "finalizing"
	RuntimeNeedsAttention  RuntimeState = "needs_attention"
	RuntimeTerminal        RuntimeState = "terminal"
)

// RunSnapshot is the host-facing view of a durable run. Optional groups are
// nil when the run has no parent, conversation, failure, or attention.
// Result.Usage is local; Accounting.Tree includes linked descendants.
type RunSnapshot struct {
	RunID        string
	Definition   DefinitionRef
	State        RuntimeState
	Parent       *ParentRef
	Conversation *ConversationRef
	Result       RunResult
	Failure      *RunFailure
	Attention    *RunAttention
	Accounting   RunAccounting
	Hooks        []RunHookResult
	ToolBatches  []ToolBatchSnapshot
	Waits        []WaitSnapshot
	// EventSequence is the run's committed event head read atomically with
	// this snapshot. Continue [RunHandle.Events] from it after resynchronizing.
	EventSequence uint64
	// HistoryPruned reports that retention removed this run's transcript and
	// tool result payloads: Result.Messages is empty and invocation results
	// are marked pruned, while state, accepted output, usage, and effect
	// reports remain.
	HistoryPruned bool
}

// ParentRef links a child to the durable invocation that admitted it.
type ParentRef struct {
	RunID       string
	OperationID string
}

// ConversationRef identifies the conversation containing a turn.
type ConversationRef struct {
	Scope string
	ID    string
}

// FailureKind classifies a persisted execution error independently of its message.
type FailureKind string

const (
	FailureDeadline                        FailureKind = "deadline"
	FailureCancelled                       FailureKind = "cancelled"
	FailureMaxTurns                        FailureKind = "max_turns"
	FailureMaxTurnsInvalidStructuredOutput FailureKind = "max_turns_invalid_structured_output"
	FailureInvalidMaxTurns                 FailureKind = "invalid_max_turns"
	FailureEmptyResponse                   FailureKind = "empty_response"
	FailureInvalidStructuredOutput         FailureKind = "invalid_structured_output"
	FailureCompletion                      FailureKind = "completion"
	FailureGeneric                         FailureKind = "generic"
)

// RunFailure preserves the error message and any provider completion details.
// The error an awaited run returns is rebuilt from it: errors.Is matches the
// sentinel for Kind, errors.As finds a [*CompletionError] for completion
// failures and an [*InvalidStructuredOutputError] carrying Violations for
// structured-output failures, and other errors keep their message only.
type RunFailure struct {
	Message    string
	Kind       FailureKind
	StopReason StopReason
	RawReason  string
	// Violations are the schema violations of the last rejected structured
	// payload, for the invalid-structured-output kinds.
	Violations []string
}

// AttentionKind identifies the reason a durable run cannot continue normally.
type AttentionKind string

const (
	AttentionExecution AttentionKind = "execution"
	AttentionHooks     AttentionKind = "hooks"
	AttentionChild     AttentionKind = "child"
	// AttentionProvider marks an interrupted provider attempt whose outcome
	// and usage are unknown (see [ProviderRecoveryPolicy]).
	AttentionProvider AttentionKind = "provider"
)

// RunAttention describes why a run cannot continue normally. Child attention
// may be advisory while a canceled subtree settles without host action.
// BlockingRunID names the immediate pending child blocking a parent, not
// necessarily the descendant that needs action; inspect that child's snapshot.
type RunAttention struct {
	Kind          AttentionKind
	Reason        string
	BlockingRunID string
}

// RunAccounting separates uncertain local attempts from known subtree totals.
type RunAccounting struct {
	// UnknownAttempts counts provider attempts this run started without a
	// recorded outcome: the provider may have received them. They are
	// included in Result.ProviderAttempts, but their usage is not in
	// Result.Usage. Retries inside one provider turn share one attempt
	// record, so an interrupted turn counts once.
	UnknownAttempts int
	// FreshAttempts counts provider attempts recovery started in place of
	// interrupted ones under RuntimeConfig.ProviderRecovery. They are new
	// requests, never replays of a lost response.
	FreshAttempts int
	// Tree aggregates recorded accounting over this run and its descendants.
	Tree TreeAccounting
}

func snapshotFromRecord(record storedRuntimeRun) RunSnapshot {
	snapshot := RunSnapshot{
		RunID: record.RunID, Definition: DefinitionRef{ID: record.DefinitionID, Revision: record.DefinitionRevision},
		State: record.State, Result: cloneRunResult(record.Result),
		Accounting:    RunAccounting{UnknownAttempts: record.UnknownAttempts, FreshAttempts: record.FreshAttempts, Tree: record.Tree},
		Hooks:         append([]RunHookResult(nil), record.HookResults...),
		ToolBatches:   cloneToolBatchSnapshots(record.ToolBatches),
		Waits:         append([]WaitSnapshot(nil), record.Waits...),
		EventSequence: record.EventSequence,
		HistoryPruned: record.HistoryPruned,
	}
	if record.ParentRunID != "" {
		snapshot.Parent = &ParentRef{RunID: record.ParentRunID, OperationID: record.ParentOperationID}
	}
	if record.ConversationID != "" {
		snapshot.Conversation = &ConversationRef{Scope: record.ConversationScope, ID: record.ConversationID}
	}
	snapshot.Failure = failureFromRecord(record)
	if record.State == RuntimeNeedsAttention {
		snapshot.Attention = &RunAttention{Kind: record.AttentionKind, Reason: record.AttentionReason}
		if snapshot.Attention.Kind == AttentionChild {
			// The persisted reason names the blocking child. Match it against a
			// pending child wait, rather than trusting an arbitrary string as an ID.
			for _, wait := range snapshot.Waits {
				if wait.Kind == WaitChild && wait.State == WaitPending && wait.ChildRunID != "" &&
					strings.HasPrefix(record.AttentionReason, "durable child "+wait.ChildRunID+" requires attention: ") {
					snapshot.Attention.BlockingRunID = wait.ChildRunID
					break
				}
			}
		}
	}
	return snapshot
}

func failureFromRecord(record storedRuntimeRun) *RunFailure {
	if record.Error == "" && record.ErrorKind == "" {
		return nil
	}
	return &RunFailure{Message: record.Error, Kind: record.ErrorKind, StopReason: record.ErrorStopReason,
		RawReason: record.ErrorRawReason, Violations: append([]string(nil), record.ErrorViolations...)}
}

func snapshotError(snapshot RunSnapshot) error {
	failure := snapshot.Failure
	if failure == nil || failure.Message == "" {
		return nil
	}
	switch failure.Kind {
	case FailureDeadline:
		return context.DeadlineExceeded
	case FailureCancelled:
		return context.Canceled
	case FailureMaxTurns:
		return fmt.Errorf("%s: %w", failure.Message, ErrMaxTurnsExceeded)
	case FailureMaxTurnsInvalidStructuredOutput:
		invalid := &InvalidStructuredOutputError{Violations: append([]string(nil), failure.Violations...)}
		return fmt.Errorf("%s: %w", failure.Message, errors.Join(ErrMaxTurnsExceeded, invalid))
	case FailureInvalidMaxTurns:
		return fmt.Errorf("%s: %w", failure.Message, ErrInvalidMaxTurns)
	case FailureEmptyResponse:
		return fmt.Errorf("%s: %w", failure.Message, ErrEmptyResponse)
	case FailureInvalidStructuredOutput:
		invalid := &InvalidStructuredOutputError{Violations: append([]string(nil), failure.Violations...)}
		return fmt.Errorf("%s: %w", failure.Message, invalid)
	case FailureCompletion:
		completion := &CompletionError{Reason: failure.StopReason, RawReason: failure.RawReason}
		// Restore the cause's text so the error reads as it did when the run
		// failed; only its message survives persistence.
		if cause, ok := strings.CutPrefix(failure.Message, completion.Error()+": "); ok {
			completion.Cause = errors.New(cause)
		}
		return completion
	}
	if snapshot.Result.Status == RunCancelled {
		return context.Canceled
	}
	return errors.New(failure.Message)
}
