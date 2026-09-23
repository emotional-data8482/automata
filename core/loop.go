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
	// ErrInvalidMaxTurns is returned when an agent is configured with a
	// negative turn limit.
	ErrInvalidMaxTurns = errors.New("max turns must be greater than 0")

	// ErrMaxTurnsExceeded is returned when the model keeps requesting tools
	// and the turn budget is exhausted before a final response.
	ErrMaxTurnsExceeded = errors.New("exceeded max turns")

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

// loop holds the per-run conversation state that loopMachine drives through
// the turn cycle for an [Agent]. Runtime creates one per worker segment.
type loop struct {
	agent       *Agent
	messages    []Message
	toolsByName map[string]registeredTool
	log         *slog.Logger
	// emit receives provisional observations (text deltas, tool calls, tool
	// results). It must be safe for concurrent use: tool-result events fire
	// from parallel tool workers.
	emit        func(StreamEvent)
	diagnostics []RunDiagnostic
}

// newLoop creates a run-scoped loop for the agent. With no history the
// conversation is seeded with the agent's system prompt (if any); committed
// history, which already carries its system message, is copied verbatim.
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

// RunResult is the outcome of a run. It is always populated as far as the run
// progressed — including when the run returns an error — so callers can inspect
// partial output, the transcript, usage, and turns even on failure.
type RunResult struct {
	RunID  string
	Status RunStatus
	// Turns counts provider turns, including structured-output corrections
	// and recovery's fresh attempts.
	Turns int
	// ProviderAttempts counts provider calls, including retries within a
	// turn and attempts whose outcome is unknown after a crash.
	ProviderAttempts int
	// ProviderStopReason is the neutral stop reason of the last provider
	// turn; StopReason below explains why the run ended.
	ProviderStopReason StopReason
	Diagnostics        []RunDiagnostic
	// Output is the final assistant text (FinalMessage.Text()); "" if the run
	// failed before producing a final message.
	Output string
	// FinalMessage is the last assistant message, blocks included.
	FinalMessage Message
	// Messages is the run's complete transcript (system prompt through the last
	// turn).
	Messages []Message
	// Usage is this run's provider-turn usage, summed. Child-run usage is
	// not included; see [RunAccounting].Tree.
	Usage Usage
	// StopReason explains why the run ended.
	StopReason StopReason
	// RawStopReason is the provider's verbatim reason for the terminal provider
	// turn. It is useful when StopReason is StopUnknown and for distinguishing
	// provider spellings such as OpenAI "length" and Anthropic "max_tokens".
	RawStopReason string

	// StructuredOutput is the validated final structured payload when the
	// run's definition declares a required structured output (see
	// [StructuredOutputConfig]). It stays separate from the model-facing
	// Output text and FinalMessage blocks, and from durable effect receipts.
	// It is empty for ordinary runs. omitempty keeps absent payloads out of
	// persisted records so a decoded empty value stays truly absent.
	StructuredOutput json.RawMessage `json:",omitempty"`
}

// runConfig carries the settings of one worker segment, resolved from the
// registered Agent by Runtime before the loop starts.
type runConfig struct {
	// options is the CallOptions sent on every provider turn.
	options CallOptions
	scope   *runScope
	// extraTools are tools the definition's contract adds for its runs: the
	// hidden structured-output tool.
	extraTools []Tool
	// terminalTool, when non-empty, names a tool whose call ends the run
	// without executing it. It is the structured-output tool.
	terminalTool string
	// durableTransition persists a loop transition and durableBatch executes
	// a tool batch as durable invocations. Runtime installs both.
	durableTransition func(context.Context, durableLoopTransition) error
	durableBatch      func(context.Context, *loop, []ToolUseBlock) ([]Message, error)
	// resume tells the machine that its loop already contains the canonical
	// transcript of this run. It must not append the admitted task again.
	resume bool
	// resumeTools is the exact effective tool selection for the already
	// accepted provider turn. Request transforms must not be rerun on recovery.
	resumeTools []string
	// structuredOutput carries the declared final-output contract installed
	// from AgentConfig.StructuredOutput (see installStructuredOutput). The
	// validated payload lands in RunResult.StructuredOutput, and correction
	// turns run inside the same run within the persisted budgets.
	structuredOutput *structuredOutputState
}

type durableLoopTransition struct {
	Kind           string
	Result         RunResult
	EffectiveTools []string
	// StructuredCorrections is the run's cumulative correction-turn count for
	// declared structured-output contracts. Runtime persists it atomically
	// with the transition's transcript so correction state survives restart.
	StructuredCorrections int
}

func (l *loop) run(ctx context.Context, task, mode string, cfg runConfig, invoke invokeFn) (RunResult, error) {
	m := &loopMachine{loop: l, ctx: ctx, task: task, mode: mode, cfg: cfg, invoke: invoke, result: cloneRunResult(cfg.scope.result)}
	m.result.StopReason = StopError
	return m.drive()
}

// canceledToolResult is the model-visible result of a sibling that never ran
// because its batch was aborted.
const canceledToolResult = "canceled: tool batch aborted"

// normalizeResponseStop validates the provider-neutral reason and supplies a
// compatibility inference only when an older custom provider supplied no
// reason at all. A nonempty unrecognized value is preserved as raw diagnostic
// data and becomes StopUnknown.
func normalizeResponseStop(response Response) (StopReason, string) {
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

// executeTool runs one reserved tool call and returns (result, fatalErr).
// result is the [ToolResult] fed back to the model — the text view of its
// blocks is what string consumers and text-only providers see; IsError marks
// a recoverable tool failure (unknown tool, invalid arguments, policy
// timeout, limiter failure) that the model can adapt to. Budget denials are
// planned when the durable batch is created, before this function is called.
// fatalErr is non-nil for run-aborting conditions (a tool's Go error, parent
// cancellation, or a failed dispatch commit) and stops the whole run after
// the batch records an outcome for every sibling.
//
// dispatch commits the invocation's dispatch boundary. It runs after the
// timeout and rate-limit wait and immediately before the executor is called.
//
// Cancellation is cooperative. A tool may finish a side effect while batch
// cancellation races its return. If it returns success, that actual result is
// recorded; if it returns a context error, the transcript records cancellation.
// Neither outcome implies that an external side effect was rolled back.
func (l *loop) safelyExecuteTool(
	ctx context.Context,
	call ToolUseBlock,
	policy ToolPolicy,
	budget toolBudgetUsage,
	dispatch func() error,
) (result ToolResult, err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("tool %q panicked: %v", call.Name, recovered)
		}
	}()
	return l.executeTool(ctx, call, policy, budget, dispatch)
}

func (l *loop) executeTool(
	ctx context.Context,
	call ToolUseBlock,
	policy ToolPolicy,
	budget toolBudgetUsage,
	dispatch func() error,
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

	ctx, span := a.tracer.Start(ctx, "tool.execute",
		tracing.String("tool", call.Name),
	)
	defer span.End()

	log := spanLogger(span, l.log)
	limits := policy.limitsFor(call.Name)
	span.SetAttributes(
		tracing.String("policy.timeout", limits.Timeout.String()),
		tracing.Int("policy.budget_used", budget.used),
		tracing.Int("policy.budget_max", budget.max),
		tracing.Int("policy.tool_budget_used", budget.toolUsed),
		tracing.Int("policy.tool_budget_max", budget.toolMax),
		tracing.Bool("policy.rate_limited", limits.RateLimiter != nil),
	)

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
	// Tools own their retry policy: the loop never re-executes a tool, which
	// could repeat an external effect. [WithToolRetry] opts a tool in, and one
	// budget reservation covers every retry inside that wrapper.
	if err := dispatch(); err != nil {
		return ToolResult{}, fmt.Errorf("persist tool dispatch: %w", err)
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
