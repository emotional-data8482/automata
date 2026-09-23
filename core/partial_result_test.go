package core

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
)

// A provider failure on a later turn keeps the earlier turn's transcript,
// usage, and turn count.
func TestLaterTurnFailureKeepsEarlierProgress(t *testing.T) {
	p := &optionsProvider{turns: []Message{withUsage(asstTool("a", "echo", `{"msg":"hi"}`), &Usage{InputTokens: 10})}}
	a := testAgent(p).WithTools(Func("echo", "echo", func(context.Context, echoArgs) (string, error) { return "ok", nil }))
	res, err := runAgent(t, a, "go")
	if err == nil || res.Output != "" || len(res.FinalMessage.ToolUses()) != 1 || res.Turns != 2 || res.Usage.InputTokens != 10 || len(res.Messages) != 3 || res.StopReason != StopError {
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
	v, res, err := runTyped[answer](t, testAgent(p).WithMaxTurns(1), "go")
	if !errors.Is(err, ErrMaxTurnsExceeded) || !errors.Is(err, ErrInvalidStructuredOutput) || v.Value != "" || p.calls != 1 || res.Turns != 1 || res.Usage.InputTokens != 10 {
		t.Fatalf("value=%+v result=%+v err=%v", v, res, err)
	}
}

func TestPartialToolJSONStaysSerializable(t *testing.T) {
	p := fixedResponseProvider{Response{Message: asstTool("a", "echo", `{"msg":`), StopReason: StopIncomplete}}
	res, err := runAgent(t, testAgent(p), "go")
	if !errors.Is(err, ErrIncompleteResponse) || len(res.Messages) != 1 {
		t.Fatalf("result=%+v err=%v", res, err)
	}
	if _, err := json.Marshal(res.Messages); err != nil {
		t.Fatal(err)
	}
}
