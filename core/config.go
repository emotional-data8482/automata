package core

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
)

func cloneCallOptions(o CallOptions) CallOptions { return cloneRequest(Request{Options: o}).Options }
func validateCallOptions(o CallOptions) error {
	if o.MaxTokens < 0 || o.ThinkingBudget < 0 {
		return fmt.Errorf("token limits must not be negative")
	}
	if t := o.Temperature; t != nil && (*t < 0 || math.IsNaN(*t) || math.IsInf(*t, 0)) {
		return fmt.Errorf("invalid temperature")
	}
	if c := o.ToolChoice; c != nil {
		if c.Mode < ToolChoiceAuto || c.Mode > ToolChoiceTool || (c.Mode == ToolChoiceTool && c.Name == "") || (c.Mode != ToolChoiceTool && c.Name != "") {
			return fmt.Errorf("invalid tool choice")
		}
	}
	if o.OutputSchema != nil {
		var schema map[string]any
		if err := decodeSchemaRaw(o.OutputSchema, &schema); err != nil {
			return fmt.Errorf("output schema: %w", err)
		}
		if schema == nil {
			return fmt.Errorf("output schema must be an object")
		}
		if err := validateSchema(schema); err != nil {
			return fmt.Errorf("output schema: %w", err)
		}
	}
	return nil
}

type frozenTool struct{ registeredTool }

func (t frozenTool) Definition() ToolDefinition { return cloneToolDefinition(t.definition) }
func (t frozenTool) Execute(ctx context.Context, args json.RawMessage) (ToolResult, error) {
	return t.executor.Execute(ctx, args)
}
func (t frozenTool) toolEffectPolicy() ToolEffectPolicy {
	policy, _ := effectPolicyFor(t.executor)
	return policy
}
func (t frozenTool) durableWaitPolicy() DurableWaitPolicy {
	policy, _, _ := waitPolicyFor(t.executor)
	return policy
}
func freezeTools(ts []Tool, terminal string) ([]Tool, error) {
	registry, defs, err := registerTools(ts, terminal)
	if err != nil {
		return nil, err
	}
	out := make([]Tool, len(defs))
	for i, d := range defs {
		registered := registry[d.Name]
		if _, _, err := waitPolicyFor(registered.executor); err != nil {
			return nil, fmt.Errorf("tool %q: %w", d.Name, err)
		}
		out[i] = frozenTool{registered}
	}
	return out, nil
}
