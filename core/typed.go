package core

import (
	"context"
	"encoding/json"
	"fmt"
)

// structuredOutputToolName is the name of the hidden tool [RunTyped] and
// [RunSessionTyped] inject to collect the typed result.
const structuredOutputToolName = "structured_output"

// RunTyped runs the agent in a fresh [Session] and decodes its final answer into
// T. It is the one-shot counterpart to [RunSessionTyped].
func RunTyped[T any](ctx context.Context, a *Agent, task string, opts ...RunOption) (T, RunResult, error) {
	return RunSessionTyped[T](ctx, a.NewSession(), task, opts...)
}

// RunSessionTyped continues session and decodes the agent's final answer into
// T. It works by injecting a hidden "structured_output" tool whose JSON schema
// is derived from T (via the same reflection as [Func]); when the model calls
// that tool, the run ends and its arguments are decoded into T.
//
// If the model instead ends with a plain-text answer, RunSessionTyped issues one
// more run on the same session that forces the structured_output tool
// (tool_choice), so a value is always produced or a decode error is returned.
// Extended thinking is disabled on that forced run because providers
// (Anthropic) forbid combining forced tool choice with thinking.
//
// The [RunResult] is returned alongside T (populated as far as the run got,
// even on error) so callers still see usage, steps, and the transcript.
// [PostRunHook]s fire after each underlying run, so the forced fallback path
// creates one checkpoint for the prose run and another for the forced run.
func RunSessionTyped[T any](ctx context.Context, session *Session, task string, opts ...RunOption) (T, RunResult, error) {
	session.runMu.Lock()
	defer session.runMu.Unlock()

	var zero T

	tool := Func(structuredOutputToolName,
		"Return the final answer as structured data. Call this exactly once, with the complete result.",
		func(_ context.Context, v T) (string, error) { return "ok", nil })

	// inject makes the tool visible to the model and marks it terminal.
	inject := func(c *runConfig) {
		c.extraTools = append(c.extraTools, tool)
		c.terminalTool = structuredOutputToolName
	}

	res, err := session.run(ctx, task, append([]RunOption{inject}, opts...)...)
	if err != nil {
		return zero, res, err
	}

	if len(res.terminalToolInput) > 0 {
		val, derr := decodeTyped[T](res.terminalToolInput)
		return val, res, derr
	}

	// The model answered in prose. Force the tool on one more turn, with
	// thinking disabled (forced tool choice + thinking is rejected by Anthropic).
	force := func(c *runConfig) {
		c.options.ToolChoice = &ToolChoice{Mode: ToolChoiceTool, Name: structuredOutputToolName}
		c.options.ThinkingBudget = 0
	}
	forced := append([]RunOption{inject}, opts...)
	forced = append(forced, force)

	res, err = session.run(ctx,
		"Now return the final answer by calling the structured_output tool.",
		forced...)
	if err != nil {
		return zero, res, err
	}
	if len(res.terminalToolInput) == 0 {
		return zero, res, fmt.Errorf("model did not produce structured output")
	}
	val, derr := decodeTyped[T](res.terminalToolInput)
	return val, res, derr
}

func decodeTyped[T any](raw json.RawMessage) (T, error) {
	var v T
	if err := json.Unmarshal(raw, &v); err != nil {
		return v, fmt.Errorf("decode structured output: %w", err)
	}
	return v, nil
}
