package core

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
)

// These characterize behavior retained until the corresponding later migration
// task changes it (accounting in Task 5, replay in Task 6, options in Task 4).
func TestMigrationBaselineLaterFailure(t *testing.T) {
	p := &optionsProvider{turns: []Message{withUsage(asstTool("a", "echo", `{"msg":"hi"}`), &Usage{InputTokens: 10})}}
	a := testAgent(p).WithTools(Func("echo", "echo", func(context.Context, echoArgs) (string, error) { return "ok", nil }))
	res, err := a.Run(context.Background(), "go")
	if err == nil || res.Output != "" || len(res.FinalMessage.ToolUses()) != 1 || res.Steps != 1 || res.Usage.InputTokens != 10 || len(res.Messages) != 3 || res.StopReason != StopError {
		t.Fatalf("result=%+v err=%v", res, err)
	}
}

func TestTypedAccountingSharesTurnBudget(t *testing.T) {
	type answer struct {
		Value string `json:"value"`
	}
	p := &optionsProvider{turns: []Message{
		withUsage(asstTool("a", structuredOutputToolName, `{}`), &Usage{InputTokens: 10}),
		withUsage(asstTool("b", structuredOutputToolName, `{"value":"ok"}`), &Usage{InputTokens: 8}),
	}}
	v, res, err := RunTyped[answer](context.Background(), testAgent(p).WithMaxSteps(1), "go")
	if !errors.Is(err, ErrMaxStepsExceeded) || !errors.Is(err, ErrInvalidStructuredOutput) || v.Value != "" || p.calls != 1 || res.Turns != 1 || res.Usage.InputTokens != 10 {
		t.Fatalf("value=%+v result=%+v err=%v", v, res, err)
	}
}

func TestMigrationBaselinePartialToolJSON(t *testing.T) {
	p := fixedResponseProvider{Response{Message: asstTool("a", "echo", `{"msg":`), StopReason: StopIncomplete}}
	res, err := testAgent(p).Run(context.Background(), "go")
	if !errors.Is(err, ErrIncompleteResponse) || len(res.Messages) != 1 {
		t.Fatalf("result=%+v err=%v", res, err)
	}
	if _, err := json.Marshal(res.Messages); err != nil {
		t.Fatal(err)
	}
}

func TestMigrationBaselineZeroOptionsInherit(t *testing.T) {
	p := &optionsProvider{turns: []Message{asstText("ok")}}
	_, err := testAgent(p).WithDefaultCallOptions(CallOptions{MaxTokens: 32, ThinkingBudget: 16}).Run(context.Background(), "go", legacyCallOptions(CallOptions{}))
	if err != nil || p.seen[0].MaxTokens != 32 || p.seen[0].ThinkingBudget != 16 {
		t.Fatalf("options=%+v err=%v", p.seen, err)
	}
}
