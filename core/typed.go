package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
)

// structuredOutputToolName is the name of the hidden tool [RunTyped] and
// [RunSessionTyped] inject to collect the typed result. It is namespaced under
// the automata_ prefix so a user tool called "structured_output" cannot collide
// with it; a user tool that deliberately occupies the namespaced name makes the
// typed run fail fast (see RunSessionTyped).
const structuredOutputToolName = "automata_structured_output"

// ErrInvalidStructuredOutput is matched (via errors.Is) by the
// [InvalidStructuredOutputError] returned when a typed run cannot produce a
// payload that satisfies the schema implied by T.
var ErrInvalidStructuredOutput = errors.New("invalid structured output")

// InvalidStructuredOutputError reports a structured-output payload that failed
// schema validation (or could not be decoded at all). It is returned by
// [RunTyped] and [RunSessionTyped] after correction attempts are exhausted —
// never a silently zero-filled T. Match it with errors.Is(err,
// ErrInvalidStructuredOutput); extract the per-field violations with
// errors.As. The accompanying [RunResult] is still populated as far as the run
// progressed.
type InvalidStructuredOutputError struct {
	// Violations lists the schema violations with path-qualified field paths,
	// e.g. "age: missing required field" or "questions[2].topic: expected
	// string, got number". Empty when the payload could not be decoded; see
	// Cause instead.
	Violations []string
	// Cause carries the underlying decode error for malformed JSON payloads.
	Cause error
}

func (e *InvalidStructuredOutputError) Error() string {
	msg := "invalid structured output"
	if len(e.Violations) > 0 {
		plural := "s"
		if len(e.Violations) == 1 {
			plural = ""
		}
		msg += fmt.Sprintf(" (%d violation%s): %s", len(e.Violations), plural, strings.Join(e.Violations, "; "))
	}
	if e.Cause != nil {
		msg += ": " + e.Cause.Error()
	}
	return msg
}

// Unwrap exposes the underlying decode error, if any.
func (e *InvalidStructuredOutputError) Unwrap() error { return e.Cause }

// Is matches the ErrInvalidStructuredOutput sentinel so callers can classify
// the failure with errors.Is without discarding the violation detail.
func (e *InvalidStructuredOutputError) Is(target error) bool {
	return target == ErrInvalidStructuredOutput
}

// WithMaxCorrectionTurns bounds how many correction turns [RunTyped] and
// [RunSessionTyped] spend feeding validation errors back to the model before
// returning [ErrInvalidStructuredOutput]. The default is 1; 0 disables
// correction entirely (an invalid payload errors immediately). Each correction
// is a full provider turn on the same session, so the worst case for one typed
// call is 1 (initial) + budget (corrections) + 1 (forced fallback) turns.
func WithMaxCorrectionTurns(n int) RunOption {
	if n < 0 {
		n = 0
	}
	return func(c *runConfig) { c.maxCorrectionTurns = &n }
}

// WithNativeStructuredOutput opts a typed run into provider-native,
// schema-enforced output: when the run's provider implements
// [StructuredOutputProvider] and reports support, the schema derived from T is
// sent via [CallOptions.OutputSchema] and the hidden structured-output tool is
// not injected. The response text is parsed and validated with the same
// validator as the tool path; if the provider returns an unusable payload, the
// run falls back to the hidden-tool path once (no further retries).
//
// Providers that do not implement the capability interface silently use the
// hidden-tool path, so the option is safe to set unconditionally.
func WithNativeStructuredOutput() RunOption {
	return func(c *runConfig) { c.nativeStructuredOutput = true }
}

// RunTyped runs the agent in a fresh [Session] and decodes its final answer into
// T. It is the one-shot counterpart to [RunSessionTyped].
func RunTyped[T any](ctx context.Context, a *Agent, task string, opts ...RunOption) (T, RunResult, error) {
	return RunSessionTyped[T](ctx, a.NewSession(), task, opts...)
}

// RunSessionTyped continues session and decodes the agent's final answer into
// T. It works by injecting a hidden "automata_structured_output" tool whose
// JSON schema is derived from T (via the same reflection as [Func]); when the
// model calls that tool, the run ends and its arguments are decoded into T.
//
// The returned value is guaranteed to satisfy the same schema that was
// advertised to the model: every payload is validated against T before it is
// returned, so a model that omits a required field or sends a wrong-typed value
// can never surface as a silently zero-filled T. Instead:
//
//  1. The initial run ends with a structured tool call → the payload is
//     validated and returned (one turn, no extra cost).
//  2. Validation fails → the violations are fed back to the model as a new
//     user turn on the same session and it is asked to call the tool correctly
//     (bounded by [WithMaxCorrectionTurns], default 1).
//  3. The model answers in prose → any JSON it embedded (a fenced ```json
//     block or a bare object) is extracted and validated first; a valid
//     payload is returned with no extra provider turn.
//  4. Nothing usable so far → one forced structured-output turn
//     (tool_choice, thinking disabled) runs as the final backstop. Its
//     payload is validated too; a forced-turn failure is terminal.
//
// When correction attempts are exhausted without a valid payload, an
// [InvalidStructuredOutputError] (matching [ErrInvalidStructuredOutput] via
// errors.Is) is returned with the per-field violations; malformed JSON wraps
// the syntax error as the error's Cause. The [RunResult] is returned alongside
// T (populated as far as the run got, even on error) so callers still see
// usage, steps, and the transcript. [PostRunHook]s fire after each underlying
// run — initial, every correction, and the forced fallback — so checkpoint
// consumers see each committed transcript.
func RunSessionTyped[T any](ctx context.Context, session *Session, task string, opts ...RunOption) (T, RunResult, error) {
	session.runMu.Lock()
	defer session.runMu.Unlock()

	var zero T

	// Read the typed-run knobs from the options. These have no agent-level
	// defaults, so applying them to a zero runConfig is sufficient.
	var knobs runConfig
	for _, opt := range opts {
		opt(&knobs)
	}
	correctionBudget := 1
	if knobs.maxCorrectionTurns != nil {
		correctionBudget = *knobs.maxCorrectionTurns
	}

	tool := Func(structuredOutputToolName,
		"Return the final answer as structured data. Call this exactly once, with the complete result.",
		func(_ context.Context, v T) (string, error) { return "ok", nil })

	// inject makes the tool visible to the model and marks it terminal.
	inject := func(c *runConfig) {
		c.extraTools = append(c.extraTools, tool)
		c.terminalTool = structuredOutputToolName
	}
	force := func(c *runConfig) {
		c.options.ToolChoice = &ToolChoice{Mode: ToolChoiceTool, Name: structuredOutputToolName}
		c.options.ThinkingBudget = 0
	}
	base := append([]RunOption{inject}, opts...)
	forced := append(append([]RunOption{inject}, opts...), force)
	forcedPrompt := fmt.Sprintf("Now return the final answer by calling the %s tool.", structuredOutputToolName)

	// Collision safety: the hidden tool is namespaced, but a user tool that
	// deliberately occupies the same name would be silently shadowed (the loop
	// indexes agent tools by name; the terminal tool is matched by name alone).
	// Fail fast instead of guessing which registration wins.
	for _, t := range session.agent.tools {
		if t.Name() == structuredOutputToolName {
			return zero, RunResult{}, fmt.Errorf(
				"typed run: registered tool %q collides with the hidden structured-output tool; rename it",
				structuredOutputToolName)
		}
	}

	// Native mode: when requested and supported, the provider enforces the
	// schema and the hidden tool is skipped entirely.
	if knobs.nativeStructuredOutput {
		if p, ok := session.agent.provider.(StructuredOutputProvider); ok && p.SupportsNativeStructuredOutput() {
			return runTypedNative[T](ctx, session, task, opts, forced, forcedPrompt)
		}
		session.agent.log.DebugContext(ctx, "native structured output requested but provider does not support it; using hidden tool")
	}

	res, err := session.run(ctx, task, base...)
	if err != nil {
		return zero, res, err
	}

	corrections := 0
	var invalid *InvalidStructuredOutputError
	for {
		if len(res.terminalToolInput) > 0 {
			val, derr := decodeAndValidate[T](res.terminalToolInput)
			if derr == nil {
				return val, res, nil
			}
			invalid = derr
		} else {
			// The model answered in prose. Try to extract a payload before
			// paying for a forced turn; extraction failure degrades to the
			// forced fallback exactly as before.
			invalid = nil
			for _, cand := range extractJSONCandidates(res.FinalMessage.Text()) {
				if !json.Valid(cand) {
					continue // broken JSON: treat as extraction failure
				}
				val, derr := decodeAndValidate[T](cand)
				if derr == nil {
					return val, res, nil
				}
				invalid = derr // valid JSON, wrong shape: correctable
			}
		}

		if invalid == nil || corrections >= correctionBudget {
			break
		}
		corrections++
		session.agent.log.InfoContext(ctx, "structured output failed validation; requesting correction",
			"violations", len(invalid.Violations), "correction", corrections, "budget", correctionBudget)
		res, err = session.run(ctx, correctionPrompt(invalid), base...)
		if err != nil {
			return zero, res, err // provider/hook failure: surface it, unmasked
		}
	}

	if invalid != nil {
		return zero, res, invalid
	}

	// The forced fallback: thinking disabled (forced tool choice + thinking is
	// rejected by Anthropic). Its output is the last resort and is not
	// corrected further.
	res, err = session.run(ctx, forcedPrompt, forced...)
	if err != nil {
		return zero, res, err
	}
	if len(res.terminalToolInput) == 0 {
		return zero, res, fmt.Errorf("model did not produce structured output")
	}
	val, derr := decodeAndValidate[T](res.terminalToolInput)
	if derr != nil {
		return zero, res, derr
	}
	return val, res, nil
}

// runTypedNative implements the provider-native structured-output path: the
// schema travels via CallOptions.OutputSchema, and the response text is parsed
// and validated like any other payload. An unusable native payload falls back
// to the hidden-tool path exactly once.
func runTypedNative[T any](ctx context.Context, session *Session, task string, opts, forced []RunOption, forcedPrompt string) (T, RunResult, error) {
	var zero T
	nativeInject := func(c *runConfig) {
		c.options.OutputSchema = typedRootSchema[T]()
	}
	session.agent.log.DebugContext(ctx, "using provider-native structured output")

	res, err := session.run(ctx, task, append([]RunOption{nativeInject}, opts...)...)
	if err != nil {
		return zero, res, err
	}

	if val, derr := decodeNativePayload[T](res.FinalMessage.Text()); derr == nil {
		return val, res, nil
	} else {
		session.agent.log.InfoContext(ctx, "native structured output payload unusable; falling back to hidden tool",
			"err", derr.Error())
	}

	// One retry on the hidden-tool path; do not loop.
	res, err = session.run(ctx, forcedPrompt, forced...)
	if err != nil {
		return zero, res, err
	}
	if len(res.terminalToolInput) == 0 {
		return zero, res, fmt.Errorf("model did not produce structured output")
	}
	val, derr := decodeAndValidate[T](res.terminalToolInput)
	if derr != nil {
		return zero, res, derr
	}
	return val, res, nil
}

// correctionPrompt builds the fixed user message presented to the model when
// its structured output fails validation. The template is deliberately stable:
// it is asserted in tests and models are prompted against it.
func correctionPrompt(err *InvalidStructuredOutputError) string {
	var b strings.Builder
	b.WriteString("Your previous structured output was invalid:\n")
	if len(err.Violations) > 0 {
		for _, v := range err.Violations {
			b.WriteString("- ")
			b.WriteString(v)
			b.WriteString("\n")
		}
	} else {
		b.WriteString("- ")
		b.WriteString(err.Error())
		b.WriteString("\n")
	}
	b.WriteString(fmt.Sprintf("Call the %s tool again with a corrected payload that fixes every listed violation. Do not omit any required field.", structuredOutputToolName))
	return b.String()
}

// typedRootSchema returns the JSON schema (the same object buildSchema
// advertises as the tool's parameters) used as the root schema for
// provider-native structured output.
func typedRootSchema[T any]() json.RawMessage {
	var zero T
	raw, _ := json.Marshal(objectSchema(reflect.TypeOf(zero), map[reflect.Type]bool{}))
	return raw
}

// decodeNativePayload parses the final message text of a native structured
// output response: the whole trimmed text first (a schema-enforced response is
// normally bare JSON), then the prose extraction candidates. Every candidate
// passes through the same validated decode as tool payloads.
func decodeNativePayload[T any](text string) (T, error) {
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		return *new(T), fmt.Errorf("empty structured output payload")
	}
	candidates := []json.RawMessage{json.RawMessage(trimmed)}
	candidates = append(candidates, extractJSONCandidates(trimmed)...)
	var lastErr error
	for _, cand := range candidates {
		val, verr := decodeAndValidate[T](cand)
		if verr == nil {
			return val, nil
		}
		lastErr = verr
	}
	return *new(T), lastErr
}

// decodeAndValidate decodes raw into T and validates it against the schema
// implied by T. Decode failures (malformed JSON, wrong root type) and schema
// violations both surface as [InvalidStructuredOutputError]; a successful
// return is guaranteed valid.
func decodeAndValidate[T any](raw json.RawMessage) (T, *InvalidStructuredOutputError) {
	val, err := decodeTyped[T](raw)
	if err != nil {
		return val, &InvalidStructuredOutputError{Cause: err}
	}
	if violations := validateTypedPayload[T](raw); len(violations) > 0 {
		return val, &InvalidStructuredOutputError{Violations: violations}
	}
	return val, nil
}

// extractJSONCandidates pulls structured-output candidates out of a prose
// final answer, in preference order: fenced code blocks (```json or bare
// ```), then the first balanced bare {...} object. The bare-object scan is
// string-aware, so braces inside JSON string literals do not count. Extraction
// is bounded to the given text — callers pass only the final message, never
// the whole transcript.
func extractJSONCandidates(text string) []json.RawMessage {
	var out []json.RawMessage
	var spans [][2]int // fenced code-block spans; excluded from the bare scan
	rest := text
	offset := 0
	for {
		start := strings.Index(rest, "```")
		if start < 0 {
			break
		}
		after := rest[start+3:]
		nl := strings.IndexByte(after, '\n')
		if nl < 0 {
			break
		}
		info := strings.TrimSpace(after[:nl])
		body := after[nl+1:]
		end := strings.Index(body, "```")
		if end < 0 {
			break
		}
		adv := start + 3 + nl + 1 + end + 3 // one past the closing fence
		spans = append(spans, [2]int{offset + start, offset + adv})
		if info == "" || strings.EqualFold(strings.TrimLeft(info, " \t"), "json") {
			if cand := strings.TrimSpace(body[:end]); cand != "" {
				out = append(out, json.RawMessage(cand))
			}
		}
		offset += adv
		rest = rest[adv:]
	}
	// Bare-object scan with fenced blocks masked out (length-preserving, so
	// found indices address the original text).
	masked := []byte(text)
	for _, s := range spans {
		for i := s[0]; i < s[1] && i < len(masked); i++ {
			masked[i] = ' '
		}
	}
	if start, end, ok := firstJSONObject(string(masked)); ok {
		out = append(out, json.RawMessage(text[start:end]))
	}
	return out
}

// firstJSONObject scans text for the first balanced {...} object, ignoring
// braces inside JSON string literals, and returns its [start, end) span.
func firstJSONObject(text string) (int, int, bool) {
	depth := 0
	inString := false
	escaped := false
	start := -1
	for i := 0; i < len(text); i++ {
		c := text[i]
		if inString {
			switch {
			case escaped:
				escaped = false
			case c == '\\':
				escaped = true
			case c == '"':
				inString = false
			}
			continue
		}
		switch c {
		case '"':
			inString = true
		case '{':
			if depth == 0 {
				start = i
			}
			depth++
		case '}':
			if depth > 0 {
				depth--
				if depth == 0 && start >= 0 {
					return start, i + 1, true
				}
			}
		}
	}
	return 0, 0, false
}

func decodeTyped[T any](raw json.RawMessage) (T, error) {
	var v T
	if err := json.Unmarshal(raw, &v); err != nil {
		return v, fmt.Errorf("decode structured output: %w", err)
	}
	return v, nil
}
