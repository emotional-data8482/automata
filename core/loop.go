package core

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"golang.org/x/sync/errgroup"

	"github.com/emotional-data/automata/retry"
	"github.com/emotional-data/automata/tracing"
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

	// ErrToolNotFound is returned when the model requests a tool the agent
	// does not have registered.
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

// newLoop creates a run-scoped Loop for the agent, seeding the conversation with
// the agent's system prompt (if any) and indexing its tools for lookup.
func newLoop(a *Agent) *Loop {
	var messages []Message
	if a.systemPrompt != "" {
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
type invokeFn func(ctx context.Context, log *slog.Logger, messages []Message, tools []Tool) (Message, error)

func (l *Loop) run(ctx context.Context, task, mode string, invoke invokeFn) (string, error) {
	a := l.agent
	if a.maxSteps <= 0 {
		return "", fmt.Errorf("%w; use WithMaxSteps to configure", ErrInvalidMaxSteps)
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
				return "", fmt.Errorf("pre-send hook failed at step %d: %w", step, hookErr)
			}
			hookSpan.End()
		}

		invokeCtx, invokeSpan := a.tracer.Start(ctx, "provider.invoke",
			tracing.Int("step", step),
		)
		response, err := invoke(invokeCtx, log, sendMsgs, sendTools)
		if err != nil {
			invokeSpan.RecordError(err)
			invokeSpan.SetStatus(err)
			invokeSpan.End()
			log.ErrorContext(ctx, "provider invocation failed", "step", step, "err", err)
			return "", fmt.Errorf("api call failed at step %d: %w", step, err)
		}
		if response.Usage != nil {
			invokeSpan.SetAttributes(
				tracing.Int("input_tokens", response.Usage.InputTokens),
				tracing.Int("output_tokens", response.Usage.OutputTokens),
			)
			log.DebugContext(ctx, "provider response", "step", step,
				"input_tokens", response.Usage.InputTokens,
				"output_tokens", response.Usage.OutputTokens,
			)
			l.emit(StreamEvent{Kind: StreamUsage, Usage: response.Usage})
		}
		invokeSpan.End()

		l.messages = append(l.messages, response)

		if len(response.ToolCalls) == 0 {
			if response.Content == nil {
				return "", ErrEmptyResponse
			}
			span.SetAttributes(tracing.Int("steps", step+1))
			log.InfoContext(ctx, "run complete", "steps", step+1)
			return *response.Content, nil
		}

		log.DebugContext(ctx, "executing tools", "step", step, "count", len(response.ToolCalls))
		// Announce the batch before executing. Emitted serially here (not from
		// the goroutines below) so call events stay ordered.
		for _, call := range response.ToolCalls {
			l.emit(StreamEvent{Kind: StreamToolCall, ToolCall: call})
		}
		results := make([]Message, len(response.ToolCalls))
		// Snapshot messages for the approver — captures history up to and
		// including the assistant message that requested these tool calls.
		approverMessages := l.messages
		g, gctx := errgroup.WithContext(ctx)
		for i, call := range response.ToolCalls {
			g.Go(func() error {
				result, err := l.executeTool(gctx, call, approverMessages)
				if err != nil {
					return err
				}
				results[i] = ToolResultMessage(call.ID, result)
				return nil
			})
		}
		if err := g.Wait(); err != nil {
			span.RecordError(err)
			span.SetStatus(err)
			return "", err
		}
		l.messages = append(l.messages, results...)
	}

	err := fmt.Errorf("%w (%d)", ErrMaxStepsExceeded, a.maxSteps)
	span.SetStatus(err)
	log.WarnContext(ctx, "exceeded max steps", "max_steps", a.maxSteps)
	return "", err
}

func (l *Loop) executeTool(ctx context.Context, call ToolCall, messages []Message) (string, error) {
	a := l.agent
	tool, ok := l.toolsByName[call.Function.Name]
	if !ok {
		return "", fmt.Errorf("%w: %q", ErrToolNotFound, call.Function.Name)
	}

	// When this run is streaming, hand the sink to the tool via context so a
	// sub-agent tool (see AsTool) can forward its own stream events upward.
	if l.streaming {
		ctx = withEmitter(ctx, l.emit)
	}

	ctx, span := a.tracer.Start(ctx, "tool.execute",
		tracing.String("tool", call.Function.Name),
	)
	defer span.End()

	log := spanLogger(span, l.log)

	decision, err := a.approver.Approve(ctx, call, messages)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(err)
		return "", err
	}
	switch decision.Outcome {
	case Deny:
		reason := decision.Reason
		if reason == "" {
			reason = "denied"
		}
		log.DebugContext(ctx, "tool call denied", "tool", call.Function.Name, "reason", reason)
		denied := fmt.Sprintf("denied: %s", reason)
		l.emit(StreamEvent{Kind: StreamToolResult, ToolCall: call, Result: denied})
		return denied, nil
	case Modify:
		log.DebugContext(ctx, "tool call modified", "tool", call.Function.Name)
		call.Function.Arguments = string(decision.Args)
	}

	log.DebugContext(ctx, "executing tool", "tool", call.Function.Name, "args", call.Function.Arguments)
	result, err := retry.Do(ctx, a.retryCfg, func() (string, error) {
		return tool.Execute(ctx, call.Function.Arguments)
	})
	if err != nil {
		span.RecordError(err)
		span.SetStatus(err)
		log.WarnContext(ctx, "tool execution error", "tool", call.Function.Name, "err", err)
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return "", err
		}
		toolErr := fmt.Sprintf("error: %s", err.Error())
		l.emit(StreamEvent{Kind: StreamToolResult, ToolCall: call, Result: toolErr, Err: err})
		return toolErr, nil
	}
	log.DebugContext(ctx, "tool result", "tool", call.Function.Name, "result", result)
	l.emit(StreamEvent{Kind: StreamToolResult, ToolCall: call, Result: result})
	return result, nil
}

func spanLogger(span tracing.Span, log *slog.Logger) *slog.Logger {
	traceID, spanID := span.TraceIDs()
	if traceID == "" {
		return log
	}
	return log.With("trace_id", traceID, "span_id", spanID)
}
