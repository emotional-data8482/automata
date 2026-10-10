package core

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/emotional-data8482/automata/retry"
)

type limiterFunc func(context.Context) error

func (f limiterFunc) Wait(ctx context.Context) error { return f(ctx) }

func TestAgentDescribe(t *testing.T) {
	noop := func(context.Context, struct{}) (string, error) { return "", nil }
	child := DefinitionRef{ID: "researcher", Revision: "v1"}
	agent, err := New(&scriptedProvider{}, AgentConfig{
		SystemPrompt: "be brief",
		MaxTurns:     4,
		Tools: []Tool{
			Func("plain", "legacy tool", noop),
			WithToolEffectPolicy(Func("lookup", "read", noop), ToolEffectPolicy{Kind: ToolEffectReadOnly}),
			WithToolRetry(WithToolEffectPolicy(Func("charge", "mutate", noop), ToolEffectPolicy{
				Kind: ToolEffectMutating, Scope: "billing",
				SemanticKey: func(json.RawMessage) (string, error) { return "k", nil },
			}), retry.Config{MaxAttempts: 2}),
			WithDurableWait(Func("deploy", "needs approval", noop), DurableWaitPolicy{
				Kind: WaitApproval, PolicyContext: "policy-7", ExpiresAfter: time.Hour,
				Target: func(json.RawMessage) (string, error) { return "prod", nil },
			}),
			NewChildTool(childTestDefinition("research"), child),
			NewChildTool(childTestDefinition("research_again"), child),
		},
		ToolPolicy: ToolPolicy{
			Timeout: time.Minute, MaxCalls: 9, MaxParallel: 2,
			PerTool: map[string]ToolLimits{"lookup": {MaxCalls: 3, RateLimiter: limiterFunc(func(context.Context) error { return nil })}},
		},
		StructuredOutput: &StructuredOutputConfig{
			Schema:         json.RawMessage(`{"type":"object","properties":{"answer":{"type":"string"}},"required":["answer"]}`),
			MaxCorrections: 1,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	manifest := agent.Describe()
	if manifest.SystemPrompt != "be brief" || manifest.MaxTurns != 4 || len(manifest.Tools) != 6 {
		t.Fatalf("manifest = %+v", manifest)
	}
	byName := map[string]ToolManifest{}
	for _, tool := range manifest.Tools {
		byName[tool.Name] = tool
	}
	if got := byName["plain"]; got.Effect != ToolEffectLegacy || got.Retry || got.Wait != nil || got.Child != nil || len(got.InputSchema) == 0 {
		t.Fatalf("plain = %+v", got)
	}
	if got := byName["lookup"]; got.Effect != ToolEffectReadOnly {
		t.Fatalf("lookup = %+v", got)
	}
	if got := byName["charge"]; got.Effect != ToolEffectMutating || got.EffectScope != "billing" || !got.SemanticGuard || !got.Retry {
		t.Fatalf("charge = %+v", got)
	}
	if got := byName["deploy"]; got.Wait == nil || got.Wait.Kind != WaitApproval || got.Wait.PolicyContext != "policy-7" || got.Wait.ExpiresAfter != time.Hour {
		t.Fatalf("deploy = %+v", got)
	}
	if got := byName["research"]; got.Child == nil || *got.Child != child || got.Effect != "" {
		t.Fatalf("research = %+v", got)
	}
	if !reflect.DeepEqual(manifest.Children, []DefinitionRef{child}) {
		t.Fatalf("children = %v", manifest.Children)
	}
	policy := manifest.ToolPolicy
	if policy.Timeout != time.Minute || policy.MaxCalls != 9 || policy.MaxParallel != 2 || policy.RateLimited ||
		policy.PerTool["lookup"] != (ToolLimitsManifest{MaxCalls: 3, RateLimited: true}) {
		t.Fatalf("tool policy = %+v", policy)
	}
	if manifest.StructuredOutput == nil || manifest.StructuredOutput.MaxCorrections != 1 || len(manifest.StructuredOutput.Schema) == 0 {
		t.Fatalf("structured output = %+v", manifest.StructuredOutput)
	}

	// The manifest is a copy and round-trips through JSON.
	manifest.Tools[0].InputSchema[0] = 'X'
	if agent.Describe().Tools[0].InputSchema[0] == 'X' {
		t.Fatal("Describe exposed the agent's schema bytes")
	}
	data, err := json.Marshal(agent.Describe())
	if err != nil {
		t.Fatal(err)
	}
	var decoded AgentManifest
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(decoded.Children, []DefinitionRef{child}) || decoded.Tools[2].EffectScope != "billing" {
		t.Fatalf("decoded manifest = %+v", decoded)
	}
}

func TestRuntimeDescribe(t *testing.T) {
	runtime := newTestRuntime(t)
	agent := testAgent(nil)
	ref, err := runtime.Register("agent", "v1", agent)
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := runtime.Describe(ref)
	if err != nil || manifest.MaxTurns != DefaultMaxTurns {
		t.Fatalf("Describe = %+v, %v", manifest, err)
	}
	if _, err := runtime.Describe(DefinitionRef{ID: "agent", Revision: "v2"}); !errors.Is(err, ErrDefinitionNotRegistered) {
		t.Fatalf("unregistered Describe = %v", err)
	}
}
