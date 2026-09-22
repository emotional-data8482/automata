package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/emotional-data8482/automata/tracing"
	"log/slog"
)

type loopState uint8

const (
	loopStateStart loopState = iota
	loopStatePrepareTurn
	loopStateInvokeProvider
	loopStateClassifyResponse
	loopStateExecuteTools
	loopStateFinish
	loopStateDone
)

func (s loopState) String() string {
	names := [...]string{"start", "prepare_turn", "invoke_provider", "classify_response", "execute_tools", "finish", "done"}
	if int(s) >= len(names) {
		return "invalid"
	}
	return names[s]
}
func validLoopTransition(from, to loopState) bool {
	switch from {
	case loopStateStart:
		return to == loopStatePrepareTurn || to == loopStateExecuteTools || to == loopStateFinish
	case loopStatePrepareTurn:
		return to == loopStateInvokeProvider || to == loopStateFinish
	case loopStateInvokeProvider:
		return to == loopStateClassifyResponse || to == loopStateFinish
	case loopStateClassifyResponse:
		// Structured-output correction turns re-enter the turn cycle directly
		// after classification: the violations result or correction prompt is
		// committed, then the run asks the model again within its budgets.
		return to == loopStateExecuteTools || to == loopStatePrepareTurn || to == loopStateFinish
	case loopStateExecuteTools:
		return to == loopStatePrepareTurn || to == loopStateFinish
	case loopStateFinish:
		return to == loopStateDone
	}
	return false
}

// loopMachine is the current internal turn driver. Runtime may replace or
// reshape it as durable transition semantics become richer.
type loopMachine struct {
	loop       *loop
	ctx        context.Context
	task, mode string
	cfg        runConfig
	invoke     invokeFn
	state      loopState
	result     RunResult
	err        error
	step       int
	policy     *toolPolicyState
	tools      []ToolDefinition
	registry   map[string]registeredTool
	hooks      []PreSendHook
	request    Request
	response   Response
	calls      []ToolUseBlock
	log        *slog.Logger
	span       tracing.Span
}

func (m *loopMachine) transition(next loopState) {
	if !validLoopTransition(m.state, next) {
		panic(fmt.Sprintf("invalid loop transition %s -> %s", m.state, next))
	}
	if m.state == loopStatePrepareTurn && next == loopStateInvokeProvider && len(m.request.Messages) == 0 {
		panic("provider request not prepared")
	}

	if m.state == loopStateExecuteTools && !(m.cfg.durableBatch != nil && m.err != nil) {
		msgs := m.loop.messages
		if len(msgs) < len(m.calls) {
			panic("tool results not committed")
		}
		for i, call := range m.calls {
			blocks := msgs[len(msgs)-len(m.calls)+i].Blocks
			if len(blocks) != 1 {
				panic("tool result missing")
			}
			result, ok := blocks[0].(ToolResultBlock)
			if !ok || result.ToolUseID != call.ID {
				panic("tool results not committed in model order")
			}
		}
	}
	m.state = next
}
func (m *loopMachine) drive() (RunResult, error) {
	defer func() {
		if m.span != nil {
			m.span.End()
		}
	}()
	for m.state != loopStateDone {
		var next loopState
		switch m.state {
		case loopStateStart:
			next = m.start()
		case loopStatePrepareTurn:
			next = m.prepareTurn()
		case loopStateInvokeProvider:
			next = m.invokeProvider()
		case loopStateClassifyResponse:
			next = m.classifyResponse()
		case loopStateExecuteTools:
			next = m.executeTools()
		case loopStateFinish:
			m.result.Messages = m.loop.snapshot()
			m.result.Diagnostics = append(m.result.Diagnostics, m.loop.diagnostics...)
			m.result.Output = m.result.FinalMessage.Text()
			m.result.Turns = m.cfg.scope.turns
			m.result.ProviderAttempts = m.cfg.scope.providerAttempts
			m.result.Usage = m.cfg.scope.usage
			m.cfg.scope.checkpoint(m.loop.messages)
			m.cfg.scope.result = cloneRunResult(m.result)
			next = loopStateDone
		}
		m.transition(next)
	}
	return m.result, m.err
}

func (m *loopMachine) persistTransition(kind string) error {
	if m.cfg.durableTransition == nil {
		return nil
	}
	result := cloneRunResult(m.result)
	result.RunID = m.cfg.scope.id
	result.Messages = m.loop.snapshot()
	result.Diagnostics = append(result.Diagnostics, m.loop.diagnostics...)
	result.Output = result.FinalMessage.Text()
	result.Turns = m.cfg.scope.turns
	result.ProviderAttempts = m.cfg.scope.providerAttempts
	result.Usage = m.cfg.scope.usage
	transition := durableLoopTransition{Kind: kind, Result: result}
	transition.StructuredCorrections = m.structuredCorrections()
	if kind == "provider_accepted" {
		transition.EffectiveTools = make([]string, 0, len(m.loop.toolsByName))
		for _, definition := range m.request.Tools {
			if _, ok := m.loop.toolsByName[definition.Name]; ok {
				transition.EffectiveTools = append(transition.EffectiveTools, definition.Name)
			}
		}
	}
	return m.cfg.durableTransition(context.WithoutCancel(m.ctx), transition)
}
func (m *loopMachine) fail(err error) loopState { m.err = err; return loopStateFinish }

// structuredFinalReady reports that a validated structured payload already
// committed for this run and only finalization remains.
func (m *loopMachine) structuredFinalReady() bool {
	return m.cfg.structuredOutput != nil && len(m.result.StructuredOutput) > 0
}

// structuredCorrections returns the run's cumulative correction-turn count
// for durable persistence.
func (m *loopMachine) structuredCorrections() int {
	if m.cfg.structuredOutput == nil {
		return 0
	}
	return m.cfg.structuredOutput.correctionsUsed
}
func (m *loopMachine) start() loopState {
	l, ctx, task, mode, cfg := m.loop, m.ctx, m.task, m.mode, m.cfg
	a := l.agent
	policy := cfg.scope.policy

	ctx, span := a.tracer.Start(ctx, "agent.run",
		tracing.String("task", task),
		tracing.Int("max_steps", a.maxSteps),
		tracing.String("mode", mode),
		tracing.String("tool_policy.timeout", policy.policy.Timeout.String()),
		tracing.Int("tool_policy.max_calls", policy.policy.MaxCalls),
		tracing.Int("tool_policy.max_parallel", policy.policy.MaxParallel),
	)
	m.ctx, m.span = ctx, span

	log := spanLogger(span, l.log)
	m.log, m.policy = log, policy
	log.InfoContext(ctx, "starting run", "task", task, "max_steps", a.maxSteps, "mode", mode)

	if !cfg.resume {
		l.messages = append(l.messages, UserMessage(task))
	}

	tools := append([]Tool(nil), a.tools...)
	tools = append(tools, cfg.extraTools...)

	// Snapshot hooks at run start so concurrent reconfiguration cannot mutate
	// the slice mid-run. Hooks run in registration order.
	m.hooks = append([]PreSendHook(nil), a.preSendHooks...)
	var err error
	m.registry, m.tools, err = registerTools(tools, cfg.terminalTool)
	if err != nil {
		return m.fail(err)
	}
	if !cfg.resume {
		return loopStatePrepareTurn
	}
	// The accepted provider call was already validated against the effective
	// registry before it was persisted. Restore that exact selection without
	// invoking request transforms again.
	l.toolsByName = make(map[string]registeredTool, len(cfg.resumeTools))
	for _, name := range cfg.resumeTools {
		tool, ok := m.registry[name]
		if !ok {
			return m.fail(fmt.Errorf("durable continuation references unregistered tool %q", name))
		}
		l.toolsByName[name] = tool
	}
	if len(l.messages) == 0 {
		return m.fail(errors.New("durable continuation has no transcript"))
	}
	if m.structuredFinalReady() {
		// The validated structured output committed atomically with its
		// transcript; only finalization remains.
		m.result.StopReason = StopEndTurn
		return loopStateFinish
	}
	last := l.messages[len(l.messages)-1]
	if last.Role != "assistant" {
		return loopStatePrepareTurn
	}
	m.calls = last.ToolUses()
	m.result.FinalMessage = cloneMessages([]Message{last})[0]
	if len(m.calls) > 0 {
		return loopStateExecuteTools
	}
	if m.cfg.structuredOutput != nil {
		// Declared structured output: validate the final text (native
		// enforcement or prose extraction) and correct within the same run.
		return m.structuredProseOutcome(last.Text())
	}
	if last.Text() == "" {
		return m.fail(ErrEmptyResponse)
	}
	m.result.Output = last.Text()
	m.result.StopReason = StopEndTurn
	return loopStateFinish

}

// structuredProseOutcome validates a final assistant text (provider-native
// enforcement or prose extraction) for a declared structured-output contract
// and either finishes the run, schedules a bounded correction turn inside the
// same run, or fails with [InvalidStructuredOutputError]. Incomplete provider
// outcomes never reach here: classifyResponse rejects them first.
func (m *loopMachine) structuredProseOutcome(text string) loopState {
	l, ctx, log, span := m.loop, m.ctx, m.log, m.span
	state := m.cfg.structuredOutput
	toolName := ""
	if !state.native {
		toolName = structuredOutputToolName
	}
	var invalid *InvalidStructuredOutputError
	for _, cand := range extractJSONCandidates(text) {
		if !json.Valid(cand) {
			continue
		}
		_, violations := state.validate(cand)
		if len(violations) == 0 {
			m.result.StructuredOutput = append(json.RawMessage(nil), cand...)
			m.result.Output = text
			m.result.StopReason = StopEndTurn
			if err := m.persistTransition("response_classified"); err != nil {
				return m.fail(&durableTransitionFailure{cause: err})
			}
			span.SetAttributes(tracing.Int("steps", m.result.Steps))
			log.InfoContext(ctx, "run complete via structured output", "steps", m.result.Steps)
			return loopStateFinish
		}
		invalid = &InvalidStructuredOutputError{Violations: violations}
	}
	if state.correctionsUsed < state.maxCorrections {
		state.correctionsUsed++
		state.lastInvalid = invalid
		if state.lastInvalid == nil {
			state.lastInvalid = &InvalidStructuredOutputError{Cause: errors.New("model did not produce structured output")}
		}
		if toolName != "" {
			state.forceToolChoice = true
		}
		prompt := structuredMissingPrompt(toolName)
		if invalid != nil {
			prompt = correctionPrompt(invalid, toolName)
		}
		l.messages = append(l.messages, UserMessage(prompt))
		// The correction state and its transcript evidence commit atomically
		// before the correction turn is dispatched.
		if err := m.persistTransition("response_classified"); err != nil {
			return m.fail(&durableTransitionFailure{cause: err})
		}
		log.InfoContext(ctx, "structured output failed validation; requesting correction",
			"correction", state.correctionsUsed, "budget", state.maxCorrections)
		m.step++
		return loopStatePrepareTurn
	}
	if invalid == nil {
		invalid = &InvalidStructuredOutputError{Cause: errors.New("model did not produce structured output")}
	}
	m.result.Output = text
	m.result.StopReason = StopEndTurn
	if err := m.persistTransition("response_classified"); err != nil {
		return m.fail(&durableTransitionFailure{cause: err})
	}
	span.RecordError(invalid)
	span.SetStatus(invalid)
	log.WarnContext(ctx, "structured output correction budget exhausted", "steps", m.result.Steps)
	return m.fail(invalid)
}

func (m *loopMachine) prepareTurn() loopState {
	l, ctx, a, result, step, log, span := m.loop, m.ctx, m.loop.agent, &m.result, m.step, m.log, m.span
	_, _, _, _, _, _, _ = l, ctx, a, result, step, log, span

	if err := ctx.Err(); err != nil {
		m.result.StopReason = StopCancelled
		return m.fail(err)
	}
	if m.cfg.scope.turns >= m.cfg.scope.config.maxTurns {
		err := fmt.Errorf("%w (%d)", ErrMaxStepsExceeded, a.maxSteps)
		if state := m.cfg.structuredOutput; state != nil && state.lastInvalid != nil {
			err = errors.Join(err, state.lastInvalid)
		}
		span.SetStatus(err)
		log.WarnContext(ctx, "exceeded max steps", "max_steps", a.maxSteps)
		result.StopReason = StopMaxSteps
		return m.fail(err)
	}
	m.cfg.scope.turns++
	m.result.Turns = m.cfg.scope.turns
	log.DebugContext(ctx, "invoking provider", "step", step)

	req := cloneRequest(Request{Messages: l.messages, Tools: m.tools, Options: m.cfg.options})
	if len(m.hooks) > 0 {
		// Hand hooks a copy so an in-capacity append from a hook can't
		// scribble into the canonical messages backing array.
		hookCtx, hookSpan := a.tracer.Start(ctx, "agent.preSend",
			tracing.Int("step", step),
			tracing.Int("hook_count", len(m.hooks)),
		)
		var hookErr error
		for i, hook := range m.hooks {
			inMsgs, inTools := len(req.Messages), len(req.Tools)
			_, perHookSpan := a.tracer.Start(hookCtx, "agent.preSend.hook",
				tracing.Int("index", i),
				tracing.Int("in_msgs", inMsgs),
				tracing.Int("in_tools", inTools),
			)
			req, hookErr = hook(hookCtx, cloneRequest(req))
			if hookErr != nil {
				perHookSpan.RecordError(hookErr)
				perHookSpan.SetStatus(hookErr)
				perHookSpan.End()
				break
			}
			outMsgs, outTools := len(req.Messages), len(req.Tools)
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
			return m.fail(fmt.Errorf("pre-send hook failed at step %d: %w", step, hookErr))
		}
		hookSpan.End()
	}

	if state := m.cfg.structuredOutput; state != nil && state.forceToolChoice && m.cfg.terminalTool != "" {
		req.Options.ToolChoice = &ToolChoice{Mode: ToolChoiceTool, Name: m.cfg.terminalTool}
		req.Options.ThinkingBudget = 0
	}
	if err := validateCallOptions(req.Options); err != nil {
		return m.fail(err)
	}
	if err := validateHistory(req.Messages); err != nil {
		return m.fail(fmt.Errorf("request history: %w", err))
	}
	selected, err := effectiveTools(req, m.registry, m.cfg.terminalTool)
	if err != nil {
		return m.fail(fmt.Errorf("prepare request at step %d: %w", step, err))
	}
	l.toolsByName = selected
	m.request = cloneRequest(req)
	return loopStateInvokeProvider
}
func (m *loopMachine) invokeProvider() loopState {
	l, ctx, a, result, step, log, span := m.loop, m.ctx, m.loop.agent, &m.result, m.step, m.log, m.span
	_, _, _, _, _, _, _ = l, ctx, a, result, step, log, span

	invokeCtx, invokeSpan := a.tracer.Start(ctx, "provider.invoke",
		tracing.Int("step", step),
	)
	response, err := m.invoke(invokeCtx, log, m.request)
	if err != nil {
		invokeSpan.RecordError(err)
		invokeSpan.SetStatus(err)
		invokeSpan.End()
		log.ErrorContext(ctx, "provider invocation failed", "step", step, "err", err)
		result.Steps = step
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			result.StopReason = StopCancelled
		}
		return m.fail(fmt.Errorf("api call failed at step %d: %w", step, err))
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
		m.cfg.scope.usage.Add(msg.Usage)
		result.Usage = m.cfg.scope.usage
		l.emit(StreamEvent{Kind: StreamUsage, Usage: msg.Usage})
	}
	invokeSpan.End()

	result.Steps = step + 1

	m.response = response
	return loopStateClassifyResponse
}
func (m *loopMachine) classifyResponse() loopState {
	l, ctx, a, result, step, log, span := m.loop, m.ctx, m.loop.agent, &m.result, m.step, m.log, m.span
	_, _, _, _, _, _, _ = l, ctx, a, result, step, log, span

	response := m.response
	msg, rejection := reconcileAssistant(response.Message)
	toolUses := msg.ToolUses()
	m.calls = toolUses
	stopReason, rawStopReason := normalizeResponseStop(response, toolUses)
	result.ProviderStopReason = stopReason
	result.RawProviderStopReason = rawStopReason
	result.RawStopReason = rawStopReason
	if response.completionErr != nil && (stopReason == StopEndTurn || stopReason == StopToolUse) {
		stopReason = StopIncomplete
	}
	if rejection != nil {
		l.diagnostics = append(l.diagnostics, responseDiagnostics(response.Message, m.cfg.scope.turns, rejection)...)
		if stopReason == StopEndTurn || stopReason == StopToolUse {
			stopReason = StopIncomplete
		}
		response.completionErr = errors.Join(response.completionErr, rejection)
	}
	if len(msg.Blocks) > 0 {
		l.messages = append(l.messages, msg)
		result.FinalMessage = cloneMessages([]Message{msg})[0]
	}
	m.response.Message = msg
	// Runtime persists every accepted provider turn before the loop can
	// dispatch its requested tools. This is an internal transition of the
	// existing machine, not a competing execution loop.
	if err := m.persistTransition("provider_accepted"); err != nil {
		return m.fail(&durableTransitionFailure{cause: err})
	}
	if err := ctx.Err(); err != nil {
		return m.failCompletion(StopCancelled, rawStopReason, errors.Join(err, response.completionErr))
	}

	// These provider outcomes are never successful final answers, even when
	// the provider returned nonempty text. Preserve that partial text and the
	// full transcript, then return a typed error that carries both neutral and
	// raw reasons.
	switch stopReason {
	case StopTokenLimit, StopContentFilter, StopCancelled, StopIncomplete, StopUnknown:
		return m.failCompletion(stopReason, rawStopReason, response.completionErr)
	}

	// A provider reason and its message shape must agree. Executing calls from
	// a response marked complete, or accepting a tool-use stop with no call,
	// risks committing an incomplete provider turn.
	if stopReason == StopEndTurn && len(toolUses) > 0 {
		cause := errors.New("provider reported normal completion with pending tool calls")
		return m.failCompletion(StopIncomplete, rawStopReason, cause)
	}
	if stopReason == StopToolUse && len(toolUses) == 0 {
		cause := errors.New("provider reported tool use without a tool call")
		return m.failCompletion(StopIncomplete, rawStopReason, cause)
	}

	if len(toolUses) == 0 {
		// No tool calls: the model is done. Return its text. A message with
		// neither text nor tool calls (e.g. thinking only) is an empty
		// response — usually a provider bug or safety filter.
		text := msg.Text()
		if m.cfg.structuredOutput != nil {
			// Declared structured output: validate the final text and correct
			// within the same run. Incomplete provider outcomes never reach
			// here; the switch above rejects them first.
			return m.structuredProseOutcome(text)
		}
		if text == "" {
			return m.fail(ErrEmptyResponse)
		}
		result.Output = text
		result.StopReason = StopEndTurn
		if err := m.persistTransition("response_classified"); err != nil {
			return m.fail(&durableTransitionFailure{cause: err})
		}
		span.SetAttributes(tracing.Int("steps", result.Steps))
		log.InfoContext(ctx, "run complete", "steps", result.Steps)
		return loopStateFinish
	}

	// This second boundary records that stop reason and message shape were
	// validated. Recovery never dispatches tools from provider_accepted alone.
	if err := m.persistTransition("batch_ready"); err != nil {
		return m.fail(&durableTransitionFailure{cause: err})
	}
	return loopStateExecuteTools
}
func (m *loopMachine) executeTools() loopState {
	l, ctx, a, result, step, log, span := m.loop, m.ctx, m.loop.agent, &m.result, m.step, m.log, m.span
	_, _, _, _, _, _, _ = l, ctx, a, result, step, log, span

	cfg, toolUses := m.cfg, m.calls
	log.DebugContext(ctx, "executing tools", "step", step, "count", len(toolUses))
	// Announce the batch before executing. Emitted serially here (not from
	// the goroutines below) so call events stay ordered.
	for _, call := range toolUses {
		l.emit(StreamEvent{Kind: StreamToolCall, ToolCall: call})
	}

	// Terminal tool (RunTyped): if the model invoked it, capture its raw
	// arguments and end the run without executing anything. Every sibling call
	// still receives a synthetic result so the transcript stays well-formed.
	if cfg.terminalTool != "" {
		terminalFound := false
		var input json.RawMessage
		for _, call := range toolUses {
			if call.Name != cfg.terminalTool || terminalFound {
				continue
			}
			input = call.Input
			if len(input) == 0 {
				input = json.RawMessage("{}")
			}
			result.terminalToolInput = input
			terminalFound = true
		}
		if terminalFound {
			state := m.cfg.structuredOutput
			var violations []string
			corrected := false
			terminalContent := "ok"
			if state != nil {
				_, violations = state.validate(input)
				switch {
				case len(violations) == 0:
					m.result.StructuredOutput = append(json.RawMessage(nil), input...)
				case state.correctionsUsed < state.maxCorrections:
					state.correctionsUsed++
					corrected = true
					state.lastInvalid = &InvalidStructuredOutputError{Violations: violations}
					state.forceToolChoice = true
					terminalContent = correctionPrompt(state.lastInvalid, structuredOutputToolName)
				default:
					terminalContent = (&InvalidStructuredOutputError{Violations: violations}).Error()
				}
			}
			results := make([]Message, len(toolUses))
			for i, call := range toolUses {
				content := terminalContent
				isError := false
				if call.Name != cfg.terminalTool {
					if corrected {
						content = fmt.Sprintf("not executed: structured output from terminal tool %q failed validation; correction requested", cfg.terminalTool)
					} else {
						content = fmt.Sprintf("not executed: run completed by terminal tool %q", cfg.terminalTool)
					}
					isError = true
				} else if len(violations) > 0 {
					isError = true
				}
				blocks := Blocks{TextBlock{Text: content}}
				results[i] = ToolResultBlockMessage(call.ID, blocks, isError)
				l.emit(StreamEvent{
					Kind: StreamToolResult, ToolCall: call, Result: content,
					ResultBlocks: blocks, IsError: isError,
				})
			}
			l.messages = append(l.messages, results...)
			if err := m.persistTransition("batch_committed"); err != nil {
				return m.fail(&durableTransitionFailure{cause: err})
			}
			if len(violations) == 0 {
				m.result.StopReason = StopEndTurn
				log.InfoContext(ctx, "run complete via terminal tool", "tool", cfg.terminalTool, "steps", result.Steps)
				return loopStateFinish
			}
			invalid := &InvalidStructuredOutputError{Violations: violations}
			if corrected {
				// Correction state and violation evidence committed atomically
				// above; the next turn is bounded by the persisted budgets.
				m.step++
				return loopStatePrepareTurn
			}
			span.RecordError(invalid)
			span.SetStatus(invalid)
			log.WarnContext(ctx, "structured output correction budget exhausted", "steps", result.Steps)
			m.result.StopReason = StopEndTurn
			return m.fail(invalid)
		}
	}

	// Snapshot messages for the approver — captures history up to and
	// including the assistant message that requested these tool calls.
	approverMessages := l.messages
	var results []Message
	var fatalErr error
	if cfg.durableBatch != nil {
		results, fatalErr = cfg.durableBatch(ctx, l, toolUses, approverMessages, m.policy)
	} else {
		results, fatalErr = l.executeToolBatch(ctx, toolUses, approverMessages, m.policy)
	}
	if len(results) != len(toolUses) {
		if fatalErr == nil {
			fatalErr = errors.New("tool batch returned incomplete results")
		}
		return m.fail(fatalErr)
	}
	l.messages = append(l.messages, results...)
	if err := m.persistTransition("batch_committed"); err != nil {
		return m.fail(&durableTransitionFailure{executionErr: fatalErr, cause: err})
	}
	if fatalErr != nil {
		span.RecordError(fatalErr)
		span.SetStatus(fatalErr)
		return m.fail(fatalErr)
	}
	m.cfg.scope.checkpoint(l.messages)
	m.step++
	return loopStatePrepareTurn
}
func (m *loopMachine) failCompletion(reason StopReason, raw string, cause error) loopState {
	l, ctx, a, result, step, log, span := m.loop, m.ctx, m.loop.agent, &m.result, m.step, m.log, m.span
	_, _, _, _, _, _, _ = l, ctx, a, result, step, log, span

	for _, call := range m.calls {
		l.emit(StreamEvent{Kind: StreamToolCall, ToolCall: call})
	}
	for _, call := range m.calls {
		r := ErrorResult("not executed: provider response incomplete")
		l.messages = append(l.messages, ToolResultBlockMessage(call.ID, r.Blocks, true))
		l.emit(StreamEvent{Kind: StreamToolResult, ToolCall: call, Result: r.Text(), ResultBlocks: r.Blocks, IsError: true})
	}
	if cause != nil {
		l.diagnostics = append(l.diagnostics, RunDiagnostic{Turn: m.cfg.scope.turns, Kind: "completion", Message: cause.Error()})
	}
	result.Output = result.FinalMessage.Text()
	result.StopReason = reason
	result.RawStopReason = raw
	err := &CompletionError{Reason: reason, RawReason: raw, Cause: cause}
	span.RecordError(err)
	span.SetStatus(err)
	span.SetAttributes(tracing.Int("steps", result.Steps))
	log.WarnContext(ctx, "provider completion was not final",
		"reason", reason, "raw_reason", raw, "steps", result.Steps)
	return m.fail(err)
}
