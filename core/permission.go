package core

import (
	"context"
	"encoding/json"
)

// Outcome is the result of an approval decision.
type Outcome int

const (
	// Allow proceeds with the tool call as-is.
	Allow Outcome = iota
	// Modify proceeds with the tool call but replaces the arguments with
	// Decision.Args. Reason is optional and not surfaced to the model.
	Modify
	// Deny refuses the tool call. The loop returns "denied: <Reason>"
	// to the model as the tool result, letting the model adapt.
	Deny
)

// Decision is returned by Approver.Approve.
//
// Invariants:
//   - Args is ignored unless Outcome == Modify.
//   - Reason is surfaced to the model on Deny; it is optional on other outcomes.
type Decision struct {
	Outcome Outcome
	// Args replaces the call's Input when Outcome == Modify.
	Args json.RawMessage
	// Reason is returned to the model as "denied: <Reason>" when Outcome == Deny.
	Reason string
}

// Approver gates tool calls before execution. Approve is called with the
// pending tool call and the full message history at that point. It may block
// arbitrarily (e.g. waiting for a human to respond); ctx cancellation is the
// caller's mechanism for aborting a long-running approval.
//
// Returning an error aborts the run entirely, the same way a context error
// from a tool does. Prefer returning Deny with a Reason for recoverable cases.
type Approver interface {
	Approve(ctx context.Context, call ToolUseBlock, messages []Message) (Decision, error)
}

// AllowAll is the default Approver. It permits every call unconditionally.
var AllowAll Approver = allowAll{}

type allowAll struct{}

func (allowAll) Approve(_ context.Context, _ ToolUseBlock, _ []Message) (Decision, error) {
	return Decision{Outcome: Allow}, nil
}

// ApproverFunc is a function that implements [Approver]. It lets callers pass
// an inline function to [WithApprover] without defining a named type.
type ApproverFunc func(ctx context.Context, call ToolUseBlock, messages []Message) (Decision, error)

func (f ApproverFunc) Approve(ctx context.Context, call ToolUseBlock, messages []Message) (Decision, error) {
	return f(ctx, call, messages)
}
