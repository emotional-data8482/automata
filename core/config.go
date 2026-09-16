package core

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
)

// Setting distinguishes inheritance (Set false) from replacement, including zero
// and nil. A nil replacement clears a nullable setting.
type Setting[T any] struct {
	Set   bool
	Value T
}

// CallOptionsPatch overrides resolved provider settings. Empty and nil stop
// sequences both clear the list when Set is true. Zero MaxTokens selects the
// provider default, zero ThinkingBudget disables thinking, and a non-nil zero
// Temperature requests temperature zero.
type CallOptionsPatch struct {
	Temperature    Setting[*float64]
	MaxTokens      Setting[int]
	StopSequences  Setting[[]string]
	ToolChoice     Setting[*ToolChoice]
	ThinkingBudget Setting[int]
	OutputSchema   Setting[json.RawMessage]
}

func (p CallOptionsPatch) apply(o CallOptions) CallOptions {
	if p.Temperature.Set {
		o.Temperature = p.Temperature.Value
	}
	if p.MaxTokens.Set {
		o.MaxTokens = p.MaxTokens.Value
	}
	if p.StopSequences.Set {
		o.StopSequences = p.StopSequences.Value
	}
	if p.ToolChoice.Set {
		o.ToolChoice = p.ToolChoice.Value
	}
	if p.ThinkingBudget.Set {
		o.ThinkingBudget = p.ThinkingBudget.Value
	}
	if p.OutputSchema.Set {
		o.OutputSchema = p.OutputSchema.Value
	}
	return cloneCallOptions(o)
}
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
		if err := json.Unmarshal(o.OutputSchema, &schema); err != nil {
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

// WithCallOptions captures an explicit patch for reuse across independent runs.
func WithCallOptions(p CallOptionsPatch) RunOption {
	o := p.apply(CallOptions{})
	p.Temperature.Value = o.Temperature
	p.StopSequences.Value = o.StopSequences
	p.ToolChoice.Value = o.ToolChoice
	p.OutputSchema.Value = o.OutputSchema
	return func(c *runConfig) { c.options = p.apply(c.options) }
}

// WithMaxTurns replaces the total public-run turn allowance; n must be positive.
func WithMaxTurns(n int) RunOption { return func(c *runConfig) { c.maxTurns = n } }

// WithObserver appends an optional observer. Nil observers are ignored.
func WithObserver(o RunObserver) RunOption {
	return func(c *runConfig) {
		if o != nil {
			c.observers = append(c.observers, o)
		}
	}
}

type frozenTool struct{ registeredTool }

func (t frozenTool) Definition() ToolDefinition { return cloneToolDefinition(t.definition) }
func (t frozenTool) Execute(ctx context.Context, args json.RawMessage) (ToolResult, error) {
	return t.executor.Execute(ctx, args)
}
func freezeTools(ts []Tool, terminal string) ([]Tool, error) {
	registry, defs, err := registerTools(ts, terminal)
	if err != nil {
		return nil, err
	}
	out := make([]Tool, len(defs))
	for i, d := range defs {
		out[i] = frozenTool{registry[d.Name]}
	}
	return out, nil
}
