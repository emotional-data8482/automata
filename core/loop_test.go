package core

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
)

// retryableToolErr is a tool error that reports itself retryable, so a test can
// prove the loop does NOT retry tool execution on its own.
type retryableToolErr struct{ msg string }

func (e *retryableToolErr) Error() string   { return e.msg }
func (e *retryableToolErr) Retryable() bool { return true }

// countingTool records how many times Execute is called and always fails with a
// retryable error.
type countingTool struct{ calls atomic.Int32 }

func (t *countingTool) Name() string            { return "counter" }
func (t *countingTool) Schema() json.RawMessage { return json.RawMessage(`{"name":"counter"}`) }
func (t *countingTool) Execute(context.Context, string) (string, error) {
	t.calls.Add(1)
	return "", &retryableToolErr{msg: "transient"}
}

// TestToolExecuteNotRetriedByLoop is the regression test for the double-retry
// bug: even when a tool returns a retryable error, the loop must call Execute
// exactly once. Retrying there would replay a whole sub-agent run, re-emitting
// its stream events and double-counting usage. The error is still fed back to
// the model as a recoverable tool result.
func TestToolExecuteNotRetriedByLoop(t *testing.T) {
	final := "recovered"
	provider := &capturingProvider{turns: []Message{
		asstTool("c1", "counter", "{}"),
		asstText(final),
	}}

	tool := &countingTool{}
	agent := New(provider)
	agent.RegisterTool(tool)

	out, err := agent.Run(context.Background(), "go")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if out.Output != final {
		t.Errorf("output = %q, want %q", out.Output, final)
	}
	if got := tool.calls.Load(); got != 1 {
		t.Errorf("Execute called %d times, want 1 (loop must not retry tools)", got)
	}
}

// TestUnknownToolIsRecoverable pins that a hallucinated tool name does not
// abort the run: the model gets a "tool not found" result naming the available
// tools, sibling calls in the same batch still execute, and the stream carries
// the failure as a StreamToolResult with ErrToolNotFound.
func TestUnknownToolIsRecoverable(t *testing.T) {
	final := "recovered"
	provider := &capturingProvider{turns: []Message{
		AssistantMessage(toolUse("g1", "ghost", "{}"), toolUse("e1", "echo", `{"msg":"hi"}`)),
		asstText(final),
	}}

	agent := New(provider)
	agent.RegisterTool(Func("echo", "echoes msg", func(_ context.Context, a echoArgs) (string, error) {
		return "echoed:" + a.Msg, nil
	}))

	var ghostEvent *StreamEvent
	out, err := agent.RunStream(context.Background(), "go", func(ev StreamEvent) {
		if ev.Kind == StreamToolResult && ev.ToolCall.ID == "g1" {
			e := ev
			ghostEvent = &e
		}
	})
	if err != nil {
		t.Fatalf("RunStream: %v", err)
	}
	if out.Output != final {
		t.Errorf("output = %q, want %q", out.Output, final)
	}

	if ghostEvent == nil {
		t.Fatal("no StreamToolResult event for the unknown tool")
	}
	if !errors.Is(ghostEvent.Err, ErrToolNotFound) {
		t.Errorf("event Err = %v, want ErrToolNotFound", ghostEvent.Err)
	}

	// The model should see both results on the next turn: the not-found error
	// (naming the real tools) and the sibling call's normal output.
	if len(provider.received) < 2 {
		t.Fatalf("provider saw %d turns, want 2", len(provider.received))
	}
	results := map[string]string{}
	for _, m := range provider.received[1] {
		if m.Role != "tool" {
			continue
		}
		for _, blk := range m.Blocks {
			if tr, ok := blk.(ToolResultBlock); ok {
				results[tr.ToolUseID] = Message{Blocks: tr.Content}.Text()
			}
		}
	}
	ghost := results["g1"]
	if !strings.Contains(ghost, "tool not found") || !strings.Contains(ghost, `"ghost"`) {
		t.Errorf("ghost result = %q, want a tool-not-found error naming the call", ghost)
	}
	if !strings.Contains(ghost, "echo") {
		t.Errorf("ghost result = %q, want it to list the available tools", ghost)
	}
	if results["e1"] != "echoed:hi" {
		t.Errorf("sibling tool result = %q, want %q", results["e1"], "echoed:hi")
	}
}

// TestParallelToolBatchRecordsEveryOutcome verifies that a fatal approver
// failure does not leave the assistant's parallel tool-call batch unmatched.
// Completed and recoverably failed siblings keep their actual results, the
// fatal call gets an aborted result, and the interrupted sibling gets a
// synthetic canceled result. Transcript order follows model order rather than
// completion order.
func TestParallelToolBatchRecordsEveryOutcome(t *testing.T) {
	approvalErr := errors.New("approval exploded")
	done := make(chan struct{})
	recovered := make(chan struct{})
	blockedStarted := make(chan struct{})
	var fatalExecuted atomic.Bool

	provider := &capturingProvider{turns: []Message{
		AssistantMessage(
			toolUse("b1", "blocked", `{}`),
			toolUse("f1", "fatal", `{}`),
			toolUse("d1", "done", `{}`),
			toolUse("r1", "recover", `{}`),
		),
	}}
	agent := New(provider).WithApprover(ApproverFunc(func(_ context.Context, call ToolUseBlock, _ []Message) (Decision, error) {
		if call.ID != "f1" {
			return Decision{Outcome: Allow}, nil
		}
		// Do not fail the batch until the other three calls have reached known
		// states: two completed and one is blocked awaiting cancellation.
		<-done
		<-recovered
		<-blockedStarted
		return Decision{}, approvalErr
	}))
	agent.RegisterTool(Func("blocked", "waits for cancellation", func(ctx context.Context, _ struct{}) (string, error) {
		close(blockedStarted)
		<-ctx.Done()
		return "", ctx.Err()
	}))
	agent.RegisterTool(Func("fatal", "approval fails", func(_ context.Context, _ struct{}) (string, error) {
		fatalExecuted.Store(true)
		return "", errors.New("fatal tool should not execute")
	}))
	agent.RegisterTool(Func("done", "completes normally", func(_ context.Context, _ struct{}) (string, error) {
		close(done)
		return "completed", nil
	}))
	agent.RegisterTool(Func("recover", "returns a recoverable error", func(_ context.Context, _ struct{}) (string, error) {
		close(recovered)
		return "", errors.New("recoverable failure")
	}))

	res, err := agent.Run(context.Background(), "go")
	if !errors.Is(err, approvalErr) {
		t.Fatalf("err = %v, want approval error", err)
	}
	if fatalExecuted.Load() {
		t.Error("fatal tool executed despite approver failure")
	}

	results := transcriptToolResults(res.Messages)
	if len(results) != 4 {
		t.Fatalf("got %d tool results, want 4: %+v", len(results), results)
	}
	wantIDs := []string{"b1", "f1", "d1", "r1"}
	for i, want := range wantIDs {
		if results[i].ToolUseID != want {
			t.Errorf("result %d ID = %q, want %q (model order)", i, results[i].ToolUseID, want)
		}
	}

	byID := make(map[string]ToolResultBlock, len(results))
	for _, result := range results {
		byID[result.ToolUseID] = result
	}
	if got := (Message{Blocks: byID["d1"].Content}).Text(); got != "completed" || byID["d1"].IsError {
		t.Errorf("completed result = %q, IsError=%v", got, byID["d1"].IsError)
	}
	if got := (Message{Blocks: byID["r1"].Content}).Text(); got != "recoverable failure" || !byID["r1"].IsError {
		t.Errorf("recoverable result = %q, IsError=%v", got, byID["r1"].IsError)
	}
	if got := (Message{Blocks: byID["f1"].Content}).Text(); got != "aborted: approval exploded" || !byID["f1"].IsError {
		t.Errorf("fatal result = %q, IsError=%v", got, byID["f1"].IsError)
	}
	if got := (Message{Blocks: byID["b1"].Content}).Text(); got != "canceled: tool batch aborted: approval exploded" || !byID["b1"].IsError {
		t.Errorf("canceled result = %q, IsError=%v", got, byID["b1"].IsError)
	}
}

func transcriptToolResults(messages []Message) []ToolResultBlock {
	var results []ToolResultBlock
	for _, message := range messages {
		if message.Role != "tool" {
			continue
		}
		for _, block := range message.Blocks {
			if result, ok := block.(ToolResultBlock); ok {
				results = append(results, result)
			}
		}
	}
	return results
}
