package claude

import (
	"context"
	"encoding/json"
	"github.com/anthropics/anthropic-sdk-go"
	"testing"

	"github.com/emotional-data8482/automata/core"
)

type replayFailureProvider struct{ message core.Message }

func (p replayFailureProvider) Invoke(context.Context, core.Request) (core.Response, error) {
	return core.Response{Message: p.message, StopReason: core.StopIncomplete}, nil
}

func TestConvertReconciledFailureHistory(t *testing.T) {
	for _, input := range []string{`{}`, `{"query":`} {
		t.Run(input, func(t *testing.T) {
			agent, err := core.New(replayFailureProvider{core.AssistantMessage(
				core.TextBlock{Text: "partial"},
				core.ToolUseBlock{ID: "call", Name: "work", Input: json.RawMessage(input)},
			)}, core.AgentConfig{})
			if err != nil {
				t.Fatal(err)
			}
			result, err := agent.Run(context.Background(), "go")
			if err == nil {
				t.Fatal("incomplete response succeeded")
			}
			data, err := json.Marshal(result.Messages)
			if err != nil {
				t.Fatal(err)
			}
			var restored []core.Message
			if err := json.Unmarshal(data, &restored); err != nil {
				t.Fatal(err)
			}
			if _, err := agent.ResumeSession(restored); err != nil {
				t.Fatal(err)
			}
			restored = append(restored, core.UserMessage("continue"))
			_, wire, err := convertMessages(restored)
			if err != nil {
				t.Fatal(err)
			}
			encoded, err := json.Marshal(wire)
			if err != nil || !json.Valid(encoded) {
				t.Fatalf("invalid provider replay: %s, %v", encoded, err)
			}
			if len(wire) < 3 {
				t.Fatalf("lost replay history: %s", encoded)
			}
		})
	}
}

type missingArgumentsProvider struct{ message core.Message }

func (p missingArgumentsProvider) Invoke(context.Context, core.Request) (core.Response, error) {
	return core.Response{Message: p.message, StopReason: core.StopToolUse}, nil
}

func TestMissingResponseArgumentsAreNotRepaired(t *testing.T) {
	var wire anthropic.Message
	if err := json.Unmarshal([]byte(`{"role":"assistant","content":[{"type":"tool_use","id":"call","name":"work"}]}`), &wire); err != nil {
		t.Fatal(err)
	}
	message := convertResponse(&wire)
	if calls := message.ToolUses(); len(calls) != 1 || len(calls[0].Input) != 0 {
		t.Fatalf("repaired missing input: %+v", calls)
	}
	agent, err := core.New(missingArgumentsProvider{message}, core.AgentConfig{Tools: []core.Tool{core.Func("work", "", func(context.Context, struct{}) (string, error) { t.Error("executed missing arguments"); return "", nil })}})
	if err != nil {
		t.Fatal(err)
	}
	result, err := agent.Run(context.Background(), "go")
	if err == nil || len(result.Diagnostics) == 0 {
		t.Fatalf("accepted malformed response: %+v, %v", result, err)
	}
	if _, err := json.Marshal(result); err != nil {
		t.Fatal(err)
	}
	if _, err := agent.ResumeSession(result.Messages); err != nil {
		t.Fatal(err)
	}
}
