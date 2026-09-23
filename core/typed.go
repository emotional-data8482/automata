package core

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strings"
)

// structuredOutputToolName is the name of the hidden tool a declared
// structured-output contract injects to collect the final payload. It is
// namespaced under the automata_ prefix so a user tool called
// "structured_output" cannot collide with it; a user tool that occupies the
// namespaced name is rejected by [New].
const structuredOutputToolName = "automata_structured_output"

// ErrInvalidStructuredOutput is matched (via errors.Is) by the
// [InvalidStructuredOutputError] returned when a run cannot produce a payload
// that satisfies its declared schema, or when [Decode] rejects a payload.
var ErrInvalidStructuredOutput = errors.New("invalid structured output")

// InvalidStructuredOutputError reports a structured-output payload that failed
// schema validation (or could not be decoded at all). A run returns it after
// its correction budget is exhausted, and [Decode] returns it instead of a
// silently zero-filled value. Match it with errors.Is(err,
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

// StructuredOutputConfig declares a required validated final output for an
// Agent definition. The declaration is frozen with the agent, so a Runtime
// definition revision pins both the executable binding and its output
// contract. Every payload is validated against Schema by the shared supported
// schema contract before it becomes the run's accepted output; unsupported
// assertion keywords are rejected explicitly at construction.
type StructuredOutputConfig struct {
	// Schema is a JSON-schema object describing the accepted final output. It
	// must be in the supported subset shared with tool input schemas; see the
	// contract documentation in schemacontract.go.
	Schema json.RawMessage
	// Native requests provider-native enforcement: when the run's provider
	// implements [StructuredOutputProvider] and reports support, Schema is
	// sent via [CallOptions.OutputSchema] and the hidden structured-output
	// tool is not injected. The final response text is validated the same way,
	// and supported native invalid payloads correct within the run's own
	// budgets. Providers without support use the hidden-tool path.
	Native bool
	// MaxCorrections bounds how many correction turns the run may spend
	// feeding violations back to the model. Each correction is a full provider
	// turn inside the same run; zero disables correction (an invalid payload
	// fails the run immediately). Correction state is durable for [Runtime]
	// runs and survives restart.
	MaxCorrections int
}

// structuredOutputContract is the frozen compiled declaration owned by an
// Agent. The compiled contract is read-only and safe for concurrent runs.
type structuredOutputContract struct {
	config   StructuredOutputConfig
	contract schemaContract
}

// structuredOutputState is the per-run working state of a declared contract.
type structuredOutputState struct {
	contract        schemaContract
	maxCorrections  int
	correctionsUsed int
	native          bool
	lastInvalid     *InvalidStructuredOutputError
	forceToolChoice bool
}

// validate checks one raw payload against the contract and returns the decoded
// value plus path-qualified violations. Exactly one top-level JSON value is
// accepted: a second Decode must reach io.EOF, so the terminal payload cannot
// carry trailing garbage or concatenated values.
func (s *structuredOutputState) validate(raw json.RawMessage) (any, []string) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var value any
	if err := dec.Decode(&value); err != nil {
		return nil, []string{"payload is not valid JSON: " + err.Error()}
	}
	var extra any
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		return nil, []string{"payload must contain exactly one JSON value"}
	}
	return value, s.contract.validate(value, "")
}

// validateStructuredOutputDeclaration validates and freezes a declared
// structured-output contract. The schema is deep-copied so later caller
// mutation cannot change frozen definitions.
func validateStructuredOutputDeclaration(config *StructuredOutputConfig) (*structuredOutputContract, error) {
	if config == nil {
		return nil, nil
	}
	if len(config.Schema) == 0 {
		return nil, errors.New("structured output requires a schema")
	}
	var schema map[string]any
	if err := decodeSchemaRaw(config.Schema, &schema); err != nil {
		return nil, fmt.Errorf("structured output schema: %w", err)
	}
	if schema == nil || schema["type"] != "object" {
		return nil, errors.New("structured output schema must be an object schema")
	}
	contract, err := compileSchemaContract(schema)
	if err != nil {
		return nil, fmt.Errorf("structured output schema: %w", err)
	}
	if config.MaxCorrections < 0 {
		return nil, errors.New("structured output correction budget cannot be negative")
	}
	return &structuredOutputContract{
		config:   StructuredOutputConfig{Schema: append(json.RawMessage(nil), config.Schema...), Native: config.Native, MaxCorrections: config.MaxCorrections},
		contract: contract,
	}, nil
}

// structuredOutputTool is the hidden terminal tool injected for declared
// structured-output runs. It is never executed: its call is validated and
// either ends the run or feeds violations back to the model.
type structuredOutputTool struct{ schema json.RawMessage }

func (t structuredOutputTool) Definition() ToolDefinition {
	return ToolDefinition{
		Name:        structuredOutputToolName,
		Description: "Return the final answer as structured data. Call this exactly once, with the complete result.",
		InputSchema: t.schema,
	}
}

func (structuredOutputTool) Execute(context.Context, json.RawMessage) (ToolResult, error) {
	return TextResult("ok"), nil
}

// installStructuredOutput resolves the declared contract into per-run state:
// either provider-native enforcement via CallOptions.OutputSchema, or the
// hidden structured-output terminal tool.
func (a *Agent) installStructuredOutput(cfg *runConfig) {
	declared := a.structuredOutput
	if declared == nil {
		return
	}
	state := &structuredOutputState{contract: declared.contract, maxCorrections: declared.config.MaxCorrections}
	native := false
	if declared.config.Native {
		if p, ok := a.provider.(StructuredOutputProvider); ok && p.SupportsNativeStructuredOutput() {
			cfg.options.OutputSchema = append(json.RawMessage(nil), declared.config.Schema...)
			native = true
		} else {
			a.log.DebugContext(context.Background(), "native structured output requested but provider does not support it; using hidden tool")
		}
	}
	state.native = native
	if !native {
		cfg.extraTools = append(cfg.extraTools, structuredOutputTool{schema: declared.config.Schema})
		cfg.terminalTool = structuredOutputToolName
	}
	cfg.structuredOutput = state
}

// correctionPrompt builds the fixed user message presented to the model when
// its structured output fails validation. The template is deliberately stable:
// it is asserted in tests and models are prompted against it. An empty tool
// name (provider-native runs) asks for the corrected payload as the final
// message instead of a tool call.
func correctionPrompt(err *InvalidStructuredOutputError, toolName string) string {
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
	if toolName != "" {
		b.WriteString(fmt.Sprintf("Call the %s tool again with a corrected payload that fixes every listed violation. Do not omit any required field.", toolName))
	} else {
		b.WriteString("Return a corrected payload that fixes every listed violation as your entire final message. Do not omit any required field.")
	}
	return b.String()
}

// structuredMissingPrompt asks the model to produce the structured payload
// when its final answer contained no usable JSON candidate.
func structuredMissingPrompt(toolName string) string {
	if toolName != "" {
		return fmt.Sprintf("Return the final answer by calling the %s tool with the complete structured result.", toolName)
	}
	return "Return the final answer as structured data: a single JSON object matching the required schema, as your entire final message."
}

// OutputSchema returns the JSON schema derived from T, for
// [StructuredOutputConfig].Schema. T must be a struct: its exported fields
// become properties, fields without omitempty are required, and a desc tag
// becomes the property description, exactly as for [Func] arguments.
//
//	agent, err := core.New(provider, core.AgentConfig{
//		StructuredOutput: &core.StructuredOutputConfig{
//			Schema:         core.OutputSchema[Verdict](),
//			MaxCorrections: 1,
//		},
//	})
func OutputSchema[T any]() json.RawMessage {
	raw, _ := json.Marshal(objectSchema(reflect.TypeFor[T](), map[reflect.Type]bool{}))
	return raw
}

// Decode returns a run's accepted structured output as T. The payload is
// validated against the schema implied by T before it is decoded, so a
// missing required field or a mistyped value returns an
// [InvalidStructuredOutputError] rather than a zero-filled T. A run without
// structured output, including one that failed before accepting a payload,
// returns an error matching [ErrInvalidStructuredOutput].
//
// Decode works on any [RunResult]: the one returned by [Runtime.Run] or
// [RunHandle.Await], or RunSnapshot.Result of a child run.
func Decode[T any](result RunResult) (T, error) {
	if len(result.StructuredOutput) == 0 {
		var zero T
		return zero, &InvalidStructuredOutputError{Cause: errors.New("run has no structured output")}
	}
	value, err := decodeAndValidate[T](result.StructuredOutput)
	if err != nil {
		return value, err
	}
	return value, nil
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
