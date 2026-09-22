package core

import (
	"context"
	"errors"
	"testing"
)

func TestRunSnapshotGroupsDurableEvidence(t *testing.T) {
	cases := []struct {
		name             string
		record           storedRuntimeRun
		wantBlockingRun  string
		wantFailure      bool
		wantAttention    bool
		wantParent       bool
		wantConversation bool
	}{
		{name: "ordinary success", record: storedRuntimeRun{RunID: "root", State: RuntimeTerminal, DefinitionID: "agent", DefinitionRevision: "v1"}},
		{name: "child blocked on descendant", record: storedRuntimeRun{
			RunID: "parent", State: RuntimeNeedsAttention, DefinitionID: "agent", DefinitionRevision: "v2",
			AttentionKind: "child", AttentionReason: "durable child child-2 requires attention: pending",
			ConversationScope: "tenant", ConversationID: "thread",
			Waits: []WaitSnapshot{
				{Kind: WaitChild, State: WaitPending, ChildRunID: "child-1"},
				{Kind: WaitChild, State: WaitPending, ChildRunID: "child-2"},
			},
		}, wantAttention: true, wantConversation: true, wantBlockingRun: "child-2"},
		{name: "failed child", record: storedRuntimeRun{
			RunID: "child", State: RuntimeTerminal, ParentRunID: "parent", ParentOperationID: "op",
			Error: "deadline exceeded", ErrorKind: "deadline", UnknownAttempts: 1,
		}, wantFailure: true, wantParent: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := snapshotFromRecord(tc.record)
			if s.Definition.ID != tc.record.DefinitionID || s.Definition.Revision != tc.record.DefinitionRevision || s.State != tc.record.State {
				t.Fatalf("identity/state = %#v", s)
			}
			if (s.Parent != nil) != tc.wantParent || (s.Conversation != nil) != tc.wantConversation ||
				(s.Failure != nil) != tc.wantFailure || (s.Attention != nil) != tc.wantAttention {
				t.Fatalf("optional groups = %#v", s)
			}
			if tc.wantParent && (s.Parent.RunID != "parent" || s.Parent.OperationID != "op") {
				t.Fatalf("parent = %#v", s.Parent)
			}
			if tc.wantConversation && (s.Conversation.Scope != "tenant" || s.Conversation.ID != "thread") {
				t.Fatalf("conversation = %#v", s.Conversation)
			}
			if tc.wantAttention && (s.Attention.Kind != AttentionChild || s.Attention.BlockingRunID != tc.wantBlockingRun) {
				t.Fatalf("attention = %#v", s.Attention)
			}
			if tc.wantFailure && (s.Failure.Kind != FailureDeadline || !errors.Is(snapshotError(s), context.DeadlineExceeded)) {
				t.Fatalf("failure = %#v, error = %v", s.Failure, snapshotError(s))
			}
			if s.Accounting.UnknownAttempts != tc.record.UnknownAttempts || s.Accounting.Tree != tc.record.Tree {
				t.Fatalf("accounting = %#v", s.Accounting)
			}
		})
	}
}

func TestRunSnapshotFailureRoundTrip(t *testing.T) {
	cases := []struct {
		name string
		err  error
		kind FailureKind
		is   error
	}{
		{"deadline", context.DeadlineExceeded, FailureDeadline, context.DeadlineExceeded},
		{"cancel", context.Canceled, FailureCancelled, context.Canceled},
		{"max steps", ErrMaxStepsExceeded, FailureMaxSteps, ErrMaxStepsExceeded},
		{"invalid structured", ErrInvalidStructuredOutput, FailureInvalidStructuredOutput, ErrInvalidStructuredOutput},
		{"completion", &CompletionError{Reason: StopReason("length"), RawReason: "limit"}, FailureCompletion, nil},
		{"generic", errors.New("failure"), FailureGeneric, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			record := storedRuntimeRun{Result: RunResult{}}
			setRuntimeError(&record, tc.err)
			s := snapshotFromRecord(record)
			if s.Failure == nil || s.Failure.Kind != tc.kind || s.Failure.Message != record.Error {
				t.Fatalf("failure = %#v, stored = %#v", s.Failure, record)
			}
			got := snapshotError(s)
			if got == nil || tc.is != nil && !errors.Is(got, tc.is) {
				t.Fatalf("roundtrip = %v, want %v", got, tc.is)
			}
			if tc.kind == FailureCompletion && (s.Failure.StopReason != StopReason("length") || s.Failure.RawReason != "limit") {
				t.Fatalf("completion details = %#v", s.Failure)
			}
		})
	}
}
