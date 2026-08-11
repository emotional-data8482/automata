package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"golang.org/x/sync/errgroup"

	"github.com/emotional-data8482/automata/tracing"
)

// Sentinel errors returned by a run. Callers can use errors.Is to distinguish
// them from provider or tool errors.
var (
	// ErrInvalidMaxSteps is returned when a run starts with maxSteps <= 0.
	ErrInvalidMaxSteps = errors.New("maxSteps must be greater than 0")

	// ErrMaxStepsExceeded is returned when the model keeps requesting tools
	// and the step budget is exhausted before a final response.
	ErrMaxStepsExceeded = errors.New("exceeded max steps")

	// ErrEmptyResponse is returned when the assistant returns a message with
	// neither content nor tool calls — usually a provider bug or an
	// over-aggressive safety filter.
	ErrEmptyResponse = errors.New("assistant returned no content and no tool calls")

	// ErrToolNotFound is embedded in the tool result fed back to the model
	// when it requests a tool the agent does not have registered. Models
	// occasionally hallucinate tool names, so an unknown tool is recoverable —
	// the result names the available tools and the run continues — rather
	// than a hard failure. It also appears as the Err on the corresponding
	// StreamToolResult event.
	ErrToolNotFound = errors.New("tool not found")
)

// Loop is the execution engine: it holds the per-run conversation state and
// drives the turn-by-turn cycle for an [Agent]. A Loop is created fresh per run
// (see newLoop) and is not reused across runs.
type Loop struct {
	agent       *Agent
	messages    []Message
	toolsByName map[string]Tool
	log         *slog.Logger
	// emit receives run-level observations (text deltas, tool calls, tool
	// results). It defaults to a no-op; RunStream installs a callback-backed
	// sink. Loop code calls it unconditionally. The sink must be safe for
	// concurrent use — tool-result events fire from the parallel executeTool
	// goroutines (RunStream's sink serializes with a mutex).
	emit func(StreamEvent)
	// streaming is set by RunStream. When true, executeTool installs emit into
	// the tool's context (see withEmitter) so sub-agent tools can stream their
	// own events up into this run. A plain Run leaves it false, so sub-agents
	// run non-streaming.
	streaming bool
}

// newLoop creates a run-scoped Loop for the agent and indexes its tools for
// lookup. With no history the conversation is seeded with the agent's system
// prompt (if any); a non-empty history (a [Session] transcript, which already
// carries its system message) is copied in verbatim instead.
func newLoop(a *Agent, history []Message) *Loop {
	var messages []Message
	if len(history) > 0 {
		messages = append([]Message(nil), history...)
	} else if a.systemPrompt != "" {
		messages = []Message{SystemMessage(a.systemPrompt)}
	}
	toolsByName := make(map[string]Tool, len(a.tools))
	for _, t := range a.tools {
		toolsByName[t.Name()] = t
	}
	return &Loop{
		agent:       a,
		messages:    messages,
		toolsByName: toolsByName,
		log:         a.log,
		emit:        func(StreamEvent) {},
	}
}

// invokeFn performs one provider turn: send messages/tools, return the assistant
// reply. Implementations own their own retry policy because streaming and
// non-streaming retry differently (see terminalStreamError).
type invokeFn func(ctx context.Context, log *slog.Logger, req Request) (Response, error)

// StopReason explains why a run ended.
type StopReason string

const (
	// StopEndTurn: the model returned a final answer with no tool calls.
	StopEndTurn StopReason = "end_turn"
	// StopMaxSteps: the step budget was exhausted with tools still pending.
	StopMaxSteps StopReason = "max_steps"
	// StopError: the run aborted on a provider, tool, or hook error.
	StopError StopReason = "error"
)

// RunResult is the outcome of a run. It is always populated as far as the run
// progressed — including when the run returns an error — so callers can inspect
// partial output, the transcript, usage, and steps even on failure.
type RunResult struct {
	// Output is the final assistant text (FinalMessage.Text()); "" if the run
	// failed before producing a final message.
	Output string
	// FinalMessage is the last assistant message, blocks included.
	FinalMessage Message
	// Messages is the run's complete transcript (system prompt through the last
	// turn).
	Messages []Message
	// Usage is this run's provider-turn usage, summed. Sub-agent usage is not
	// included here — observe it via tagged StreamUsage events / the accumulator.
	Usage Usage
	// Steps is the number of provider turns taken.
	Steps int
	// StopReason explains why the run ended.
	StopReason StopReason

	// terminalToolInput holds the raw JSON arguments of a terminal-tool call
	// (see runConfig.terminalTool). Unexported: only [RunTyped] reads it.
	terminalToolInput json.RawMessage
}

// runConfig carries per-run settings resolved from agent defaults plus
// [RunOption]s before the loop starts.
type runConfig struct {
	// options is the merged CallOptions sent on every provider turn.
	options CallOptions
	// extraTools are tools added for this run only (not on the Agent). Used by
	// [RunTyped] to inject its structured-output tool.
	extraTools []Tool
	// terminalTool, when non-empty, names a tool whose call ends the run without
	// executing it; the call's Input is recorded in the result. Used by
	// [RunTyped]. Empty for ordinary runs.
	terminalTool string
}

// RunOption customizes a single run. See [WithCallOptions].
type RunOption func(*runConfig)

// WithCallOptions overrides the agent's default [CallOptions] for one run. The
// override is merged over the agent defaults field-by-field.
func WithCallOptions(o CallOptions) RunOption {
	return func(c *runConfig) { c.options = c.options.merge(o) }
}

func (l *Loop) run(ctx context.Context, task, mode string, cfg runConfig, invoke invokeFn) (RunResult, error) {
	a := l.agent
	result := RunResult{StopReason: StopError}
	if a.maxSteps <= 0 {
		return result, fmt.Errorf("%w; use WithMaxSteps to configure", ErrInvalidMaxSteps)
	}

	ctx, span := a.tracer.Start(ctx, "agent.run",
		tracing.String("task", task),
		tracing.Int("max_steps", a.maxSteps),
		tracing.String("mode", mode),
	)
	defer span.End()

	log := spanLogger(span, l.log)
	log.InfoContext(ctx, "starting run", "task", task, "max_steps", a.maxSteps, "mode", mode)

	l.messages = append(l.messages, UserMessage(task))

	tools := append([]Tool(nil), a.tools...)
	tools = append(tools, cfg.extraTools...)

	// Snapshot hooks at run start so concurrent reconfiguration cannot mutate
	// the slice mid-run. Hooks run in registration order.
	hooks := append([]PreSendHook(nil), a.preSendHooks...)

	for step := 0; step < a.maxSteps; step++ {
		log.DebugContext(ctx, "invoking provider", "step", step)

		sendMsgs := l.messages
		sendTools := tools
		if len(hooks) > 0 {
			// Hand hooks a copy so an in-capacity append from a hook can't
			// scribble into the canonical messages backing array.
			sendMsgs = append([]Message(nil), l.messages...)
			hookCtx, hookSpan := a.tracer.Start(ctx, "agent.preSend",
				tracing.Int("step", step),
				tracing.Int("hook_count", len(hooks)),
			)
			var hookErr error
			for i, hook := range hooks {
				inMsgs, inTools := len(sendMsgs), len(sendTools)
				_, perHookSpan := a.tracer.Start(hookCtx, "agent.preSend.hook",
					tracing.Int("index", i),
					tracing.Int("in_msgs", inMsgs),
					tracing.Int("in_tools", inTools),
				)
				sendMsgs, sendTools, hookErr = hook(hookCtx, sendMsgs, sendTools)
				if hookErr != nil {
					perHookSpan.RecordError(hookErr)
					perHookSpan.SetStatus(hookErr)
					perHookSpan.End()
					break
				}
				outMsgs, outTools := len(sendMsgs), len(sendTools)
				perHookSpan.SetAttributes(
					tracing.Int("out_msgs", outMsgs),
					tracing.Int("out_tools", outTools),
					tracing.Int("delta_msgs", outMsgs-inMsgs),
					tracing.Int("delta_tools", outTools-inTools),
				)
				log.DebugContext(ctx, "pre-send hook applied",
					"step", step, "index", i,
					"in_msgs", inMsgs, "out_msgs", outMsgs,
					"in_tools", inTools, "out_tools", outTools,
				)
				perHookSpan.End()
			}
			if hookErr != nil {
				hookSpan.RecordError(hookErr)
				hookSpan.SetStatus(hookErr)
				hookSpan.End()
				span.RecordError(hookErr)
				span.SetStatus(hookErr)
				log.ErrorContext(ctx, "pre-send hook failed", "step", step, "err", hookErr)
				result.Steps = step
				result.Messages = l.snapshot()
				return result, fmt.Errorf("pre-send hook failed at step %d: %w", step, hookErr)
			}
			hookSpan.End()
		}

		invokeCtx, invokeSpan := a.tracer.Start(ctx, "provider.invoke",
			tracing.Int("step", step),
		)
		response, err := invoke(invokeCtx, log, Request{Messages: sendMsgs, Tools: sendTools, Options: cfg.options})
		if err != nil {
			invokeSpan.RecordError(err)
			invokeSpan.SetStatus(err)
			invokeSpan.End()
			log.ErrorContext(ctx, "provider invocation failed", "step", step, "err", err)
			result.Steps = step
			result.Messages = l.snapshot()
			return result, fmt.Errorf("api call failed at step %d: %w", step, err)
		}
		msg := response.Message
		if msg.Usage != nil {
			invokeSpan.SetAttributes(
				tracing.Int("input_tokens", msg.Usage.InputTokens),
				tracing.Int("output_tokens", msg.Usage.OutputTokens),
			)
			log.DebugContext(ctx, "provider response", "step", step,
				"input_tokens", msg.Usage.InputTokens,
				"output_tokens", msg.Usage.OutputTokens,
			)
			result.Usage.Add(msg.Usage)
			l.emit(StreamEvent{Kind: StreamUsage, Usage: msg.Usage})
		}
		invokeSpan.End()

		l.messages = append(l.messages, msg)
		result.Steps = step + 1
		result.FinalMessage = msg

		toolUses := msg.ToolUses()
		if len(toolUses) == 0 {
			// No tool calls: the model is done. Return its text. A message with
			// neither text nor tool calls (e.g. thinking only) is an empty
			// response — usually a provider bug or safety filter.
			result.Messages = l.snapshot()
			text := msg.Text()
			if text == "" {
				return result, ErrEmptyResponse
			}
			result.Output = text
			result.StopReason = StopEndTurn
			span.SetAttributes(tracing.Int("steps", result.Steps))
			log.InfoContext(ctx, "run complete", "steps", result.Steps)
			return result, nil
		}

		log.DebugContext(ctx, "executing tools", "step", step, "count", len(toolUses))
		// Announce the batch before executing. Emitted serially here (not from
		// the goroutines below) so call events stay ordered.
		for _, call := range toolUses {
			l.emit(StreamEvent{Kind: StreamToolCall, ToolCall: call})
		}

		// Terminal tool (RunTyped): if the model invoked it, capture its raw
		// arguments and end the run without executing anything. A synthetic
		// success result keeps the transcript well-formed.
		if cfg.terminalTool != "" {
			for _, call := range toolUses {
				if call.Name != cfg.terminalTool {
					continue
				}
				input := call.Input
				if len(input) == 0 {
					input = json.RawMessage("{}")
				}
				l.messages = append(l.messages, ToolResultMessage(call.ID, "ok", false))
				l.emit(StreamEvent{Kind: StreamToolResult, ToolCall: call, Result: "ok"})
				result.terminalToolInput = input
				result.StopReason = StopEndTurn
				result.Messages = l.snapshot()
				log.InfoContext(ctx, "run complete via terminal tool", "tool", cfg.terminalTool, "steps", result.Steps)
				return result, nil
			}
		}

		results := make([]Message, len(toolUses))
		// Snapshot messages for the approver — captures history up to and
		// including the assistant message that requested these tool calls.
		approverMessages := l.messages
		g, gctx := errgroup.WithContext(ctx)
		for i, call := range toolUses {
			g.Go(func() error {
				out, isErr, err := l.executeTool(gctx, call, approverMessages)
				if err != nil {
					return err
				}
				results[i] = ToolResultMessage(call.ID, out, isErr)
				return nil
			})
		}
		if err := g.Wait(); err != nil {
			span.RecordError(err)
			span.SetStatus(err)
			result.Messages = l.snapshot()
			return result, err
		}
		l.messages = append(l.messages, results...)
	}

	err := fmt.Errorf("%w (%d)", ErrMaxStepsExceeded, a.maxSteps)
	span.SetStatus(err)
	log.WarnContext(ctx, "exceeded max steps", "max_steps", a.maxSteps)
	result.StopReason = StopMaxSteps
	result.Messages = l.snapshot()
	return result, err
}

// snapshot returns a copy of the loop's current transcript for a RunResult.
func (l *Loop) snapshot() []Message {
	return append([]Message(nil), l.messages...)
}

// executeTool runs one tool call and returns (result, isError, fatalErr).
// result is the string fed back to the model; isError marks a recoverable tool
// failure (unknown tool, denial, or a tool error) that the model can adapt to;
// fatalErr is non-nil only for run-aborting conditions (approver error, context
// cancellation) and stops the whole run.
func (l *Loop) executeTool(ctx context.Context, call ToolUseBlock, messages []Message) (string, bool, error) {
	a := l.agent
	tool, ok := l.toolsByName[call.Name]
	if !ok {
		// A hallucinated tool name is model error, not program error: feed it
		// back like any other tool failure so the model can pick a real tool.
		names := make([]string, len(a.tools))
		for i, t := range a.tools {
			names[i] = t.Name()
		}
		err := fmt.Errorf("%w: %q (available tools: %s)", ErrToolNotFound, call.Name, strings.Join(names, ", "))
		l.log.WarnContext(ctx, "tool not found", "tool", call.Name)
		notFound := err.Error()
		l.emit(StreamEvent{Kind: StreamToolResult, ToolCall: call, Result: notFound, IsError: true, Err: err})
		return notFound, true, nil
	}

	// When this run is streaming, hand the sink to the tool via context so a
	// sub-agent tool (see AsTool) can forward its own stream events upward, and
	// stash this call's ID so the sub-agent can tag those events with it (see
	// StreamEvent.InvocationID).
	if l.streaming {
		ctx = withEmitter(ctx, l.emit)
		ctx = withToolCallID(ctx, call.ID)
	}

	ctx, span := a.tracer.Start(ctx, "tool.execute",
		tracing.String("tool", call.Name),
	)
	defer span.End()

	log := spanLogger(span, l.log)

	decision, err := a.approver.Approve(ctx, call, messages)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(err)
		return "", false, err
	}
	switch decision.Outcome {
	case Deny:
		reason := decision.Reason
		if reason == "" {
			reason = "denied"
		}
		log.DebugContext(ctx, "tool call denied", "tool", call.Name, "reason", reason)
		denied := "denied: " + reason
		l.emit(StreamEvent{Kind: StreamToolResult, ToolCall: call, Result: denied, IsError: true})
		return denied, true, nil
	case Modify:
		log.DebugContext(ctx, "tool call modified", "tool", call.Name)
		call.Input = decision.Args
	}

	args := string(call.Input)
	log.DebugContext(ctx, "executing tool", "tool", call.Name, "args", args)
	// Tools own their own retry policy. The loop deliberately does NOT wrap
	// Execute in retry.Do: an [AsTool] sub-agent already retries at its provider
	// layer, and re-running its Execute would replay a whole sub-run — including
	// re-emitting every stream event and double-counting usage it already
	// forwarded. Plain tools that want retries can opt in with [WithToolRetry].
	result, err := tool.Execute(ctx, args)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(err)
		log.WarnContext(ctx, "tool execution error", "tool", call.Name, "err", err)
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return "", false, err
		}
		// Recoverable tool error: the message carries the raw error text and the
		// IsError flag. The provider decides how to present it (Anthropic sets a
		// native is_error; OpenAI prefixes "error:"), so core does not prefix.
		toolErr := err.Error()
		l.emit(StreamEvent{Kind: StreamToolResult, ToolCall: call, Result: toolErr, IsError: true, Err: err})
		return toolErr, true, nil
	}
	log.DebugContext(ctx, "tool result", "tool", call.Name, "result", result)
	l.emit(StreamEvent{Kind: StreamToolResult, ToolCall: call, Result: result})
	return result, false, nil
}

func spanLogger(span tracing.Span, log *slog.Logger) *slog.Logger {
	traceID, spanID := span.TraceIDs()
	if traceID == "" {
		return log
	}
	return log.With("trace_id", traceID, "span_id", spanID)
}
