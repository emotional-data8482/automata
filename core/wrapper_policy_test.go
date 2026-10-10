package core

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/emotional-data8482/automata/retry"
)

// An invalid approval policy must be rejected whichever wrapper surrounds it;
// a wrapper must never turn it into "no approval configured" (bug review T1).
func TestWrappedInvalidApprovalPolicyIsRejected(t *testing.T) {
	invalid := func() Tool {
		return WithDurableWait(Func("pay", "pay", func(context.Context, struct{}) (string, error) {
			return "paid", nil
		}), DurableWaitPolicy{Kind: WaitApproval}) // no Target
	}
	wrappers := map[string]func(Tool) Tool{
		"retry":  func(tool Tool) Tool { return WithToolRetry(tool, retry.Config{MaxAttempts: 2}) },
		"legacy": WithLegacyToolErrors,
		"effect": func(tool Tool) Tool {
			return WithToolEffectPolicy(tool, ToolEffectPolicy{Kind: ToolEffectMutating})
		},
		"nested": func(tool Tool) Tool {
			return WithLegacyToolErrors(WithToolRetry(tool, retry.Config{MaxAttempts: 2}))
		},
	}
	for name, wrap := range wrappers {
		t.Run(name, func(t *testing.T) {
			tool := wrap(invalid())
			if _, _, err := waitPolicyFor(tool); err == nil {
				t.Fatal("waitPolicyFor accepted an approval without a target")
			}
			if _, err := New(&scriptedProvider{}, AgentConfig{Tools: []Tool{tool}}); err == nil {
				t.Fatal("New registered a tool whose approval has no target")
			}
		})
	}
}

// Describe reports policies declared under wrappers; a misconfigured one is
// rejected by New rather than shown as unclassified (bug review D2).
func TestDescribeReportsWrappedPolicies(t *testing.T) {
	base := WithDurableWait(Func("pay", "pay", func(context.Context, struct{}) (string, error) {
		return "paid", nil
	}), DurableWaitPolicy{Kind: WaitApproval, Target: func(json.RawMessage) (string, error) { return "card", nil }})
	tool := WithToolRetry(WithToolEffectPolicy(base, ToolEffectPolicy{Kind: ToolEffectMutating}), retry.Config{MaxAttempts: 2})
	agent, err := New(&scriptedProvider{}, AgentConfig{Tools: []Tool{tool}})
	if err != nil {
		t.Fatal(err)
	}
	manifest := agent.Describe()
	entry := manifest.Tools[0]
	if entry.Effect != ToolEffectMutating || entry.Wait == nil || entry.Wait.Kind != WaitApproval || !entry.Retry {
		t.Fatalf("manifest entry = %#v", entry)
	}
}
