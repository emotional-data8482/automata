package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

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

// loop is the execution engine: it holds the per-run conversation state and
// drives the turn-by-turn cycle for an [Agent]. A loop is created fresh per run
// (see newLoop) and is not reused across runs.
type loop struct {
	agent       *Agent
	messages    []Message
	toolsByName map[string]registeredTool
	log         *slog.Logger
	// emit receives run-level observations (text deltas, tool calls, tool
	// results). It defaults to a no-op; RunStream installs a callback-backed
	// sink. The loop calls it unconditionally. The sink must be safe for
	// concurrent use — tool-result events fire from the parallel executeTool
	// goroutines (RunStream's sink serializes with a mutex).
	emit func(StreamEvent)
	// streaming is set by RunStream. When true, executeTool installs emit into
	// the tool's context (see withEmitter) so sub-agent tools can stream their
	// own events up into this run. A plain Run leaves it false, so sub-agents
	// run non-streaming.
	streaming   bool
	diagnostics []RunDiagnostic
}

// newLoop creates a run-scoped loop for the agent and indexes its tools for
// lookup. With no history the conversation is seeded with the agent's system
// prompt (if any); a non-empty history (a [Session] transcript, which already
// carries its system message) is copied in verbatim instead.
func newLoop(a *Agent, history []Message) *loop {
	var messages []Message
	if len(history) > 0 {
		messages = cloneMessages(history)
	} else if a.systemPrompt != "" {
		messages = []Message{SystemMessage(a.systemPrompt)}
	}
	toolsByName := make(map[string]registeredTool)
	return &loop{
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

type durableToolDispatchContextKey struct{}

func withDurableToolDispatch(ctx context.Context, dispatch func() error) context.Context {
	return context.WithValue(ctx, durableToolDispatchContextKey{}, dispatch)
}

// RunResult is the outcome of a run. It is always populated as far as the run
// progressed — including when the run returns an error — so callers can inspect
// partial output, the transcript, usage, and steps even on failure.
type RunResult struct {
	// The consolidated fields are defined here for migration. Public scope
	// accounting and finalization are integrated in project Task 5.
	RunID                 string
	Status                RunStatus
	Turns                 int
	ProviderAttempts      int
	ProviderStopReason    StopReason
	RawProviderStopReason string
	Diagnostics           []RunDiagnostic
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
	// RawStopReason is the provider's verbatim reason for the terminal provider
	// turn. It is useful when StopReason is StopUnknown and for distinguishing
	// provider spellings such as OpenAI "length" and Anthropic "max_tokens".
	RawStopReason string

	// terminalToolInput holds the raw JSON arguments of a terminal-tool call
	// (see runConfig.terminalTool). Unexported: only [RunTyped] and
	// [RunSessionTyped] read it.
	terminalToolInput json.RawMessage
}

// runConfig carries per-run settings resolved from agent defaults plus
// [RunOption]s before the loop starts.
type runConfig struct {
	// options is the merged CallOptions sent on every provider turn.
	options   CallOptions
	maxTurns  int
	observers []RunObserver
	optionErr error
	scope     *runScope
	// toolPolicy contains the run-scoped local execution controls. A RunOption
	// replaces the Agent default before execution state is allocated.
	toolPolicy ToolPolicy
	// extraTools are tools added for this run only (not on the Agent). Used by
	// typed runs to inject their structured-output tool.
	extraTools []Tool
	// terminalTool, when non-empty, names a tool whose call ends the run without
	// executing it; the call's Input is recorded in the result. Used by typed
	// runs; empty for ordinary runs.
	terminalTool string
	// durableTransition and durableBatch are installed only by Runtime. Direct
	// compatibility entry points leave them nil.
	durableTransition func(context.Context, durableLoopTransition) error
	durableBatch      func(context.Context, *loop, []ToolUseBlock, []Message, *toolPolicyState) ([]Message, error)
	// resume tells the machine that its loop already contains the canonical
	// transcript of this run. It must not append the admitted task again.
	resume bool
	// resumeTools is the exact effective tool selection for the already
	// accepted provider turn. Request transforms must not be rerun on recovery.
	resumeTools []string
	// maxCorrectionTurns bounds model-mediated correction turns in typed runs
	// (see [WithMaxCorrectionTurns]). nil means the default of 1.
	maxCorrectionTurns *int
	// nativeStructuredOutput opts a typed run into provider-native schema
	// enforcement when the provider supports it (see
	// [WithNativeStructuredOutput]).
	nativeStructuredOutput bool
}

type durableLoopTransition struct {
	Kind           string
	Result         RunResult
	EffectiveTools []string
}

// RunOption customizes a single run. See [WithCallOptions] and [WithToolPolicy].
type RunOption func(*runConfig)

// WithToolPolicy replaces the Agent's default [ToolPolicy] for one run. Unlike
// CallOptions, execution policies are replaced as a unit rather than merged;
// this makes clearing an agent default possible with ToolPolicy{}.
func WithToolPolicy(policy ToolPolicy) RunOption {
	snapshot := policy.clone()
	return func(c *runConfig) { c.toolPolicy = snapshot.clone() }
}

func (l *loop) run(ctx context.Context, task, mode string, cfg runConfig, invoke invokeFn) (RunResult, error) {
	m := &loopMachine{loop: l, ctx: ctx, task: task, mode: mode, cfg: cfg, invoke: invoke, result: cloneRunResult(cfg.scope.result)}
	m.result.terminalToolInput = nil
	m.result.StopReason = StopError
	return m.drive()
}

func canceledToolResult(cause error) string {
	return "canceled: tool batch aborted"
}

// normalizeResponseStop validates the provider-neutral reason and supplies a
// compatibility inference only when an older custom provider supplied no
// reason at all. A nonempty unrecognized value is preserved as raw diagnostic
// data and becomes StopUnknown.
func normalizeResponseStop(response Response, toolUses []ToolUseBlock) (StopReason, string) {
	reason := response.StopReason
	raw := response.RawStopReason

	if reason == "" {
		return StopIncomplete, raw
	}

	switch reason {
	case StopEndTurn, StopToolUse, StopTokenLimit, StopContentFilter,
		StopCancelled, StopIncomplete, StopUnknown:
		return reason, raw
	default:
		if raw == "" {
			raw = string(reason)
		}
		return StopUnknown, raw
	}
}

// snapshot returns a copy of the loop's current transcript for a RunResult.
func (l *loop) snapshot() []Message {
	return cloneMessages(l.messages)
}

// executeTool runs one tool call and returns (result, fatalErr). result is the
// [ToolResult] fed back to the model — the text view of its blocks is what
// string consumers and text-only providers see; IsError marks a recoverable
// tool failure (unknown tool, approval denial, policy timeout, limiter failure,
// or a tool error) that the model can adapt to. Budget denials are planned by
// executeToolBatch before this function is called. fatalErr is non-nil only for
// run-aborting conditions (approver error or parent context cancellation) and
// stops the whole run after the batch records an outcome for every sibling.
//
// Cancellation is cooperative. A tool may finish a side effect while batch
// cancellation races its return. If it returns success, that actual result is
// recorded; if it returns a context error, the transcript records cancellation.
// Neither outcome implies that an external side effect was rolled back.
//
// This is also the pre-append choke point: a future per-result truncation or
// summarization limiter (see planning/roadmap.md) runs here, after executeTool
// returns and before the result becomes a transcript message.
func (l *loop) safelyExecuteTool(
	ctx context.Context,
	call ToolUseBlock,
	messages []Message,
	policy *toolPolicyState,
	budget toolBudgetUsage,
) (result ToolResult, err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("tool %q panicked: %v", call.Name, recovered)
		}
	}()
	return l.executeTool(ctx, call, messages, policy, budget)
}

func (l *loop) executeTool(
	ctx context.Context,
	call ToolUseBlock,
	messages []Message,
	policy *toolPolicyState,
	budget toolBudgetUsage,
) (ToolResult, error) {
	a := l.agent
	tool, ok := l.toolsByName[call.Name]
	if !ok {
		// A hallucinated tool name is model error, not program error: feed it
		// back like any other tool failure so the model can pick a real tool.
		names := make([]string, 0, len(l.toolsByName))
		for name := range l.toolsByName {
			names = append(names, name)
		}
		err := fmt.Errorf("%w: %q (available tools: %s)", ErrToolNotFound, call.Name, strings.Join(names, ", "))
		l.log.WarnContext(ctx, "tool not found", "tool", call.Name)
		notFound := err.Error()
		blocks := Blocks{TextBlock{Text: notFound}}
		l.emit(StreamEvent{Kind: StreamToolResult, ToolCall: call, Result: notFound, ResultBlocks: blocks, IsError: true, Err: err})
		return ErrorResult(notFound), nil
	}

	if err := validateToolArguments(tool.definition.InputSchema, call.Input); err != nil {
		return l.invalidArguments(call, err), nil
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
	limits := policy.policy.limitsFor(call.Name)
	span.SetAttributes(
		tracing.String("policy.timeout", limits.Timeout.String()),
		tracing.Int("policy.budget_used", budget.used),
		tracing.Int("policy.budget_max", budget.max),
		tracing.Int("policy.tool_budget_used", budget.toolUsed),
		tracing.Int("policy.tool_budget_max", budget.toolMax),
		tracing.Bool("policy.rate_limited", limits.RateLimiter != nil),
	)

	decision, err := a.approver.Approve(ctx, cloneBlock(call).(ToolUseBlock), cloneMessages(messages))
	if err != nil {
		span.RecordError(err)
		span.SetStatus(err)
		return ToolResult{}, err
	}
	switch decision.Outcome {
	case Deny:
		span.SetAttributes(tracing.String("policy.outcome", "approval_denied"))
		reason := decision.Reason
		if reason == "" {
			reason = "denied"
		}
		log.DebugContext(ctx, "tool call denied", "tool", call.Name, "reason", reason)
		denied := "denied: " + reason
		blocks := Blocks{TextBlock{Text: denied}}
		l.emit(StreamEvent{Kind: StreamToolResult, ToolCall: call, Result: denied, ResultBlocks: blocks, IsError: true})
		return ErrorResult(denied), nil
	case Modify:
		log.DebugContext(ctx, "tool call modified", "tool", call.Name)
		call.Input = append(json.RawMessage(nil), decision.Args...)
		if err := validateToolArguments(tool.definition.InputSchema, call.Input); err != nil {
			return l.invalidArguments(call, err), nil
		}
	}

	execCtx := ctx
	cancel := func() {}
	if limits.Timeout > 0 {
		execCtx, cancel = context.WithTimeout(ctx, limits.Timeout)
	}
	defer cancel()

	if limits.RateLimiter != nil {
		waitStarted := time.Now()
		waitErr := limits.RateLimiter.Wait(execCtx)
		span.SetAttributes(tracing.Float64("policy.rate_limit_wait_ms", float64(time.Since(waitStarted))/float64(time.Millisecond)))
		if policyResult, fatal, handled := l.handleToolExecutionFailure(
			ctx, execCtx, call, limits.Timeout, "rate_limit", waitErr, span, log,
		); handled {
			return policyResult, fatal
		}
		// A limiter that returned nil after parent cancellation must not allow the
		// external operation to begin.
		if err := ctx.Err(); err != nil {
			return ToolResult{}, err
		}
	}

	args := string(call.Input)
	log.DebugContext(execCtx, "executing tool", "tool", call.Name, "args", args)
	// Tools own their own retry policy. The loop deliberately does NOT wrap
	// Execute in retry.Do: an [AsTool] sub-agent already retries at its provider
	// layer, and re-running its Execute would replay a whole sub-run — including
	// re-emitting every stream event and double-counting usage it already
	// forwarded. Plain tools that want retries can opt in with [WithToolRetry].
	// One policy budget reservation covers all retries inside that wrapper.
	//
	// Rich-result tools (see [ResultTool]) are preferred: their block content is
	// carried through verbatim; string tools are wrapped with [TextResult].
	if dispatch, ok := execCtx.Value(durableToolDispatchContextKey{}).(func() error); ok {
		if err := dispatch(); err != nil {
			return ToolResult{}, fmt.Errorf("persist tool dispatch: %w", err)
		}
	}
	result, executeErr := tool.executor.Execute(execCtx, append(json.RawMessage(nil), call.Input...))
	result = normalizeResult(result)
	result.Blocks = cloneBlocks(result.Blocks)
	if executeErr == nil {
		if err := validateBlock(ToolResultBlock{ToolUseID: call.ID, Content: result.Blocks}); err != nil {
			executeErr = fmt.Errorf("invalid tool result: %w", err)
		}
	}
	if policyResult, fatal, handled := l.handleToolExecutionFailure(
		ctx, execCtx, call, limits.Timeout, "tool", executeErr, span, log,
	); handled {
		// Effect evidence is independent of failure policy. In particular, a
		// recoverable tool timeout must not erase an authoritative receipt.
		if fatal != nil && result.Effect.Status != EffectUnreported {
			return result, fatal
		}
		policyResult.Effect = result.Effect
		return policyResult, fatal
	}

	span.SetAttributes(tracing.String("policy.outcome", "executed"))
	text := result.Text()
	log.DebugContext(execCtx, "tool result", "tool", call.Name, "result", text)
	l.emit(StreamEvent{Kind: StreamToolResult, ToolCall: call, Result: text, ResultBlocks: result.Blocks, IsError: result.IsError})
	return result, nil
}

// handleToolExecutionFailure distinguishes an Automata-created per-tool
// deadline (recoverable) from cancellation of the parent run (fatal). It also
// normalizes limiter and ordinary tool errors into auditable result events.
func (l *loop) handleToolExecutionFailure(
	parentCtx context.Context,
	execCtx context.Context,
	call ToolUseBlock,
	timeout time.Duration,
	stage string,
	execErr error,
	span tracing.Span,
	log *slog.Logger,
) (ToolResult, error, bool) {
	if timeout > 0 && parentCtx.Err() == nil && errors.Is(execCtx.Err(), context.DeadlineExceeded) {
		err := fmt.Errorf("%w: tool %q exceeded %s deadline", ErrToolTimeout, call.Name, timeout)
		content := fmt.Sprintf("timeout: tool %q exceeded %s deadline", call.Name, timeout)
		span.SetAttributes(tracing.String("policy.outcome", "timeout"))
		span.RecordError(err)
		span.SetStatus(err)
		log.WarnContext(parentCtx, "tool execution timed out", "tool", call.Name, "timeout", timeout)
		blocks := Blocks{TextBlock{Text: content}}
		l.emit(StreamEvent{
			Kind: StreamToolResult, ToolCall: call, Result: content,
			ResultBlocks: blocks, IsError: true, Err: err,
		})
		return ErrorResult(content), nil, true
	}
	if execErr == nil {
		return ToolResult{}, nil, false
	}

	span.RecordError(execErr)
	span.SetStatus(execErr)
	log.WarnContext(parentCtx, "tool execution error", "tool", call.Name, "stage", stage, "err", execErr)
	if stage == "tool" || errors.Is(execErr, context.Canceled) || errors.Is(execErr, context.DeadlineExceeded) {
		return ToolResult{}, execErr, true
	}

	content := execErr.Error()
	eventErr := execErr
	outcome := "tool_error"
	if stage == "rate_limit" {
		content = "rate limit: " + content
		eventErr = fmt.Errorf("rate limit wait: %w", execErr)
		outcome = "rate_limit_error"
	}
	span.SetAttributes(tracing.String("policy.outcome", outcome))
	blocks := Blocks{TextBlock{Text: content}}
	l.emit(StreamEvent{
		Kind: StreamToolResult, ToolCall: call, Result: content,
		ResultBlocks: blocks, IsError: true, Err: eventErr,
	})
	return ErrorResult(content), nil, true
}

func spanLogger(span tracing.Span, log *slog.Logger) *slog.Logger {
	traceID, spanID := span.TraceIDs()
	if traceID == "" {
		return log
	}
	return log.With("trace_id", traceID, "span_id", spanID)
}

func (l *loop) invalidArguments(call ToolUseBlock, err error) ToolResult {
	result := ErrorResult("invalid args: " + err.Error())
	l.emit(StreamEvent{Kind: StreamToolResult, ToolCall: call, Result: result.Text(), ResultBlocks: result.Blocks, IsError: true, Err: err})
	return result
}
