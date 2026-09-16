package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

type personResult struct {
	Name string `json:"name"`
	Age  int    `json:"age"`
}

// TestRunTypedHappyPath pins the common case: the model calls the injected
// structured_output tool, and RunTyped decodes its arguments into T.
func TestRunTypedHappyPath(t *testing.T) {
	provider := &scriptedProvider{turns: []Message{
		asstTool("s1", structuredOutputToolName, `{"name":"Ada","age":36}`),
	}}
	agent := testAgent(provider)

	got, res, err := RunTyped[personResult](context.Background(), agent, "who?")
	if err != nil {
		t.Fatalf("RunTyped: %v", err)
	}
	if got.Name != "Ada" || got.Age != 36 {
		t.Errorf("decoded = %+v, want {Ada 36}", got)
	}
	if res.StopReason != StopEndTurn {
		t.Errorf("StopReason = %q, want %q", res.StopReason, StopEndTurn)
	}
}

// TestRunTypedInjectsSchemaTool pins that the injected tool's JSON schema is
// derived from T and advertised to the model.
func TestRunTypedInjectsSchemaTool(t *testing.T) {
	capt := &toolCapturingProvider{reply: asstTool("s1", structuredOutputToolName, `{"name":"x","age":1}`)}
	agent := testAgent(capt)

	if _, _, err := RunTyped[personResult](context.Background(), agent, "go"); err != nil {
		t.Fatalf("RunTyped: %v", err)
	}
	if capt.toolNames[structuredOutputToolName] == "" {
		t.Fatalf("structured_output tool not advertised; saw %v", capt.toolNames)
	}
	// The schema should mention the struct's fields.
	schema := capt.toolNames[structuredOutputToolName]
	for _, field := range []string{"name", "age"} {
		if !strings.Contains(schema, field) {
			t.Errorf("schema missing field %q: %s", field, schema)
		}
	}
}

// TestRunTypedForcesToolOnTextFallback pins the fallback: when the model answers
// with prose, RunTyped issues a second turn forcing the tool with thinking
// disabled, and decodes that.
func TestRunTypedForcesToolOnTextFallback(t *testing.T) {
	capt := &forcingProvider{
		replies: []Message{
			asstText("Ada is 36 years old."),                                    // turn 1: prose, no tool call
			asstTool("s2", structuredOutputToolName, `{"name":"Ada","age":36}`), // forced turn
		},
	}
	agent := testAgent(capt).WithDefaultCallOptions(CallOptions{ThinkingBudget: 2048})

	got, _, err := RunTyped[personResult](context.Background(), agent, "who?")
	if err != nil {
		t.Fatalf("RunTyped: %v", err)
	}
	if got.Name != "Ada" || got.Age != 36 {
		t.Errorf("decoded = %+v, want {Ada 36}", got)
	}
	// The second (forced) turn must set tool_choice=structured_output and
	// disable thinking, even though the agent default enabled it.
	if len(capt.seen) != 2 {
		t.Fatalf("provider saw %d turns, want 2", len(capt.seen))
	}
	forced := capt.seen[1]
	if forced.ToolChoice == nil || forced.ToolChoice.Mode != ToolChoiceTool || forced.ToolChoice.Name != structuredOutputToolName {
		t.Errorf("forced turn tool choice = %+v, want forced structured_output", forced.ToolChoice)
	}
	if forced.ThinkingBudget != 0 {
		t.Errorf("forced turn ThinkingBudget = %d, want 0 (thinking must be off)", forced.ThinkingBudget)
	}
}

// TestRunTypedDecodeErrorCorrects pins that malformed arguments surface as a
// typed error carrying the decode cause — and that a model which fixes its
// malformed JSON on the correction turn still produces a value.
func TestRunTypedDecodeErrorCorrects(t *testing.T) {
	provider := &scriptedProvider{turns: []Message{
		asstTool("s1", structuredOutputToolName, `{"name":"x","age":"not-a-number"}`),
		asstTool("s2", structuredOutputToolName, `{"name":"x","age":3}`),
	}}
	agent := testAgent(provider)

	got, _, err := RunTyped[personResult](context.Background(), agent, "go")
	if err != nil {
		t.Fatalf("RunTyped: %v", err)
	}
	if got.Age != 3 {
		t.Errorf("decoded = %+v, want age 3", got)
	}
}

// TestRunTypedDecodeErrorBudgetExhausted pins that malformed JSON keeps its
// syntax-error cause on the typed error once corrections run out.
func TestRunTypedDecodeErrorBudgetExhausted(t *testing.T) {
	provider := &scriptedProvider{turns: []Message{
		asstTool("s1", structuredOutputToolName, `{"name":"x","age":"not-a-number"}`),
		asstTool("s2", structuredOutputToolName, `{"name":"x","age":"still-not-a-number"}`), // still wrong type
	}}
	agent := testAgent(provider)

	_, res, err := RunTyped[personResult](context.Background(), agent, "go")
	if !errors.Is(err, ErrInvalidStructuredOutput) {
		t.Fatalf("err = %v, want ErrInvalidStructuredOutput", err)
	}
	var typed *InvalidStructuredOutputError
	if !errors.As(err, &typed) || typed.Cause == nil {
		t.Fatalf("errors.As = %v, want InvalidStructuredOutputError with decode cause", err)
	}
	if !strings.Contains(typed.Cause.Error(), "decode structured output") {
		t.Errorf("cause = %v, want decode error", typed.Cause)
	}
	if len(res.Messages) == 0 || res.Steps == 0 {
		t.Errorf("RunResult not populated alongside the error: %+v", res)
	}
}

// TestRunTypedMissingRequiredFieldRejected pins the headline fix: a payload
// missing a required field can never be returned as a zero-filled T.
func TestRunTypedMissingRequiredFieldRejected(t *testing.T) {
	provider := &scriptedProvider{turns: []Message{
		asstTool("s1", structuredOutputToolName, `{"name":"Ada"}`), // age missing
		asstTool("s2", structuredOutputToolName, `{"name":"Ada"}`), // still missing
	}}
	agent := testAgent(provider)

	got, res, err := RunTyped[personResult](context.Background(), agent, "go")
	if !errors.Is(err, ErrInvalidStructuredOutput) {
		t.Fatalf("err = %v, want ErrInvalidStructuredOutput", err)
	}
	if got != (personResult{}) {
		t.Errorf("returned zero-ish value %+v on error", got)
	}
	var typed *InvalidStructuredOutputError
	if !errors.As(err, &typed) {
		t.Fatalf("errors.As failed: %v", err)
	}
	if len(typed.Violations) == 0 {
		t.Fatal("no violations reported")
	}
	if !strings.Contains(typed.Violations[0], "age") {
		t.Errorf("violation %q does not name the missing field", typed.Violations[0])
	}
	if provider.calls != 2 {
		t.Errorf("provider calls = %d, want 2 (initial + correction)", provider.calls)
	}
	if len(res.Messages) == 0 {
		t.Errorf("RunResult not populated alongside the error")
	}
}

// TestRunTypedWrongTypeRejected pins wrong-typed fields are validation
// failures, not decode failures, and are corrected the same way.
func TestRunTypedWrongTypeRejected(t *testing.T) {
	provider := &scriptedProvider{turns: []Message{
		asstTool("s1", structuredOutputToolName, `{"name":42,"age":36}`),
		asstTool("s2", structuredOutputToolName, `{"name":"Ada","age":36}`),
	}}
	agent := testAgent(provider)

	got, _, err := RunTyped[personResult](context.Background(), agent, "go")
	if err != nil {
		t.Fatalf("RunTyped: %v", err)
	}
	if got.Name != "Ada" {
		t.Errorf("decoded = %+v, want Ada", got)
	}
}

// TestRunTypedCorrectionPromptListsViolations pins the correction turn's user
// message: the violation list plus the instruction to call the tool again.
func TestRunTypedCorrectionPromptListsViolations(t *testing.T) {
	provider := &capturingProvider{turns: []Message{
		asstTool("s1", structuredOutputToolName, `{"name":"Ada"}`),
		asstTool("s2", structuredOutputToolName, `{"name":"Ada","age":36}`),
	}}
	agent := testAgent(provider)

	if _, _, err := RunTyped[personResult](context.Background(), agent, "go"); err != nil {
		t.Fatalf("RunTyped: %v", err)
	}
	if len(provider.received) != 2 {
		t.Fatalf("provider saw %d requests, want 2", len(provider.received))
	}
	second := provider.received[1]
	lastUser := second[len(second)-1]
	if lastUser.Role != "user" {
		t.Fatalf("last message of correction turn = %q, want user", lastUser.Role)
	}
	text := lastUser.Text()
	if !strings.Contains(text, "age") || !strings.Contains(text, "missing required field") {
		t.Errorf("correction prompt does not name the violation: %q", text)
	}
	if !strings.Contains(text, structuredOutputToolName) {
		t.Errorf("correction prompt does not name the tool: %q", text)
	}
}

// TestRunTypedCorrectionBudgetZeroDisables pins WithMaxCorrectionTurns(0): the
// invalid payload errors immediately with no correction or forced turn.
func TestRunTypedCorrectionBudgetZeroDisables(t *testing.T) {
	provider := &scriptedProvider{turns: []Message{
		asstTool("s1", structuredOutputToolName, `{"name":"Ada"}`),
	}}
	agent := testAgent(provider)

	_, _, err := RunTyped[personResult](context.Background(), agent, "go", WithMaxCorrectionTurns(0))
	if !errors.Is(err, ErrInvalidStructuredOutput) {
		t.Fatalf("err = %v, want ErrInvalidStructuredOutput", err)
	}
	if provider.calls != 1 {
		t.Errorf("provider calls = %d, want 1 (no correction turn)", provider.calls)
	}
}

// TestRunTypedCorrectionBudgetTwo pins the budget is configurable and counted
// per RunTyped call.
func TestRunTypedCorrectionBudgetTwo(t *testing.T) {
	provider := &scriptedProvider{turns: []Message{
		asstTool("s1", structuredOutputToolName, `{"name":"Ada"}`),
		asstTool("s2", structuredOutputToolName, `{"age":36}`),
		asstTool("s3", structuredOutputToolName, `{"name":"Grace","age":37}`),
	}}
	agent := testAgent(provider)

	got, _, err := RunTyped[personResult](context.Background(), agent, "go", WithMaxCorrectionTurns(2))
	if err != nil {
		t.Fatalf("RunTyped: %v", err)
	}
	if got.Name != "Grace" {
		t.Errorf("decoded = %+v, want Grace", got)
	}
	if provider.calls != 3 {
		t.Errorf("provider calls = %d, want 3", provider.calls)
	}
}

// TestRunTypedCorrectionProviderErrorPropagates pins that a correction turn
// failing for unrelated reasons returns that error, not a validation error.
func TestRunTypedCorrectionProviderErrorPropagates(t *testing.T) {
	providerErr := errors.New("provider exploded")
	provider := &failOnSecondProvider{first: asstTool("s1", structuredOutputToolName, `{"name":"Ada"}`), err: providerErr}
	agent := testAgent(provider)

	_, _, err := RunTyped[personResult](context.Background(), agent, "go")
	if !errors.Is(err, providerErr) {
		t.Fatalf("err = %v, want provider error propagated", err)
	}
	if errors.Is(err, ErrInvalidStructuredOutput) {
		t.Errorf("provider error masked as validation error")
	}
}

func TestRunSessionTypedCorrectionCommitsConversation(t *testing.T) {
	provider := &scriptedProvider{turns: []Message{
		asstTool("s1", structuredOutputToolName, `{"name":"Ada"}`),
		asstTool("s2", structuredOutputToolName, `{"name":"Ada","age":36}`),
	}}
	session := testAgent(provider).NewSession()
	got, result, err := RunSessionTyped[personResult](context.Background(), session, "who?")
	if err != nil {
		t.Fatalf("RunSessionTyped: %v", err)
	}
	if got.Name != "Ada" || got.Age != 36 {
		t.Errorf("decoded = %+v, want Ada/36", got)
	}
	if !reflect.DeepEqual(session.Messages(), result.Messages) {
		t.Errorf("corrected result was not committed to the conversation")
	}
}

// TestRunSessionTypedCorrectionTranscriptResumable pins the correction
// exchange is committed to the transcript and survives a JSON round-trip.
func TestRunSessionTypedCorrectionTranscriptResumable(t *testing.T) {
	provider := &capturingProvider{turns: []Message{
		asstTool("s1", structuredOutputToolName, `{"name":"Ada"}`),
		asstTool("s2", structuredOutputToolName, `{"name":"Ada","age":36}`),
		asstTool("s3", structuredOutputToolName, `{"name":"Grace","age":37}`),
	}}
	agent := testAgent(provider)
	session := agent.NewSession()
	if _, _, err := RunSessionTyped[personResult](context.Background(), session, "first"); err != nil {
		t.Fatalf("first: %v", err)
	}

	blob, err := json.Marshal(session.Messages())
	if err != nil {
		t.Fatalf("marshal transcript: %v", err)
	}
	var transcript []Message
	if err := json.Unmarshal(blob, &transcript); err != nil {
		t.Fatalf("unmarshal transcript: %v", err)
	}
	resumed := agent.testResumeSession(transcript)
	got, _, err := RunSessionTyped[personResult](context.Background(), resumed, "second")
	if err != nil {
		t.Fatalf("resumed RunSessionTyped: %v", err)
	}
	if got.Name != "Grace" {
		t.Errorf("resumed typed result = %+v, want Grace", got)
	}
}

// --- prose extraction ------------------------------------------------------

func TestExtractJSONCandidates(t *testing.T) {
	tests := []struct {
		name string
		text string
		want []string
	}{
		{
			name: "fenced json block",
			text: "Here you go:\n```json\n{\"a\":1}\n```\nDone.",
			want: []string{"{\"a\":1}"},
		}, {
			name: "bare fence counts",
			text: "```\n{\"a\":1}\n```",
			want: []string{"{\"a\":1}"},
		}, {
			name: "non-json fence ignored but bare object found",
			text: "```python\nprint({\"a\":1})\n```\nAnswer: {\"a\":2}",
			want: []string{"{\"a\":2}"},
		}, {
			name: "bare object mid-sentence",
			text: "The answer is {\"a\": 1, \"b\": [2, 3]} as requested.",
			want: []string{"{\"a\": 1, \"b\": [2, 3]}"},
		}, {
			name: "braces and escapes inside string literals do not break the scan",
			text: `{"a": "curious { brace", "b": "escaped \" quote {", "c": 1}`,
			want: []string{`{"a": "curious { brace", "b": "escaped \" quote {", "c": 1}`},
		}, {
			name: "multiple fences returned in order",
			text: "```json\n{\"a\":1}\n```\n```json\n{\"b\":2}\n```",
			want: []string{"{\"a\":1}", "{\"b\":2}"},
		}, {
			name: "no json at all",
			text: "Ada is 36 years old.",
			want: nil,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := extractJSONCandidates(tt.text)
			if len(got) != len(tt.want) {
				t.Fatalf("candidates = %q, want %q", got, tt.want)
			}
			for i := range got {
				if string(got[i]) != tt.want[i] {
					t.Errorf("candidate[%d] = %q, want %q", i, got[i], tt.want[i])
				}
			}
		})
	}
}

// TestRunTypedProseFencedJSON pins the cheap prose path: a fenced ```json
// payload validates and returns with zero extra provider turns.
func TestRunTypedProseFencedJSON(t *testing.T) {
	provider := &scriptedProvider{turns: []Message{
		asstText("Here is the answer:\n```json\n{\"name\":\"Ada\",\"age\":36}\n```"),
	}}
	agent := testAgent(provider)

	got, _, err := RunTyped[personResult](context.Background(), agent, "who?")
	if err != nil {
		t.Fatalf("RunTyped: %v", err)
	}
	if got.Name != "Ada" || got.Age != 36 {
		t.Errorf("decoded = %+v", got)
	}
	if provider.calls != 1 {
		t.Errorf("provider calls = %d, want 1 (no forced turn)", provider.calls)
	}
}

// TestRunTypedProseBareObject pins that an unfenced JSON object is extracted.
func TestRunTypedProseBareObject(t *testing.T) {
	provider := &scriptedProvider{turns: []Message{
		asstText(`The answer is {"name": "Grace", "age": 37} — hope that helps.`),
	}}
	agent := testAgent(provider)

	got, _, err := RunTyped[personResult](context.Background(), agent, "who?")
	if err != nil {
		t.Fatalf("RunTyped: %v", err)
	}
	if got.Name != "Grace" || got.Age != 37 {
		t.Errorf("decoded = %+v", got)
	}
	if provider.calls != 1 {
		t.Errorf("provider calls = %d, want 1", provider.calls)
	}
}

// TestRunTypedProseWrongShapeCorrects pins that valid-JSON-wrong-shape prose
// routes to the correction turn before the forced fallback.
func TestRunTypedProseWrongShapeCorrects(t *testing.T) {
	provider := &scriptedProvider{turns: []Message{
		asstText(`{"name":"Ada"}`), // missing age
		asstTool("s2", structuredOutputToolName, `{"name":"Ada","age":36}`),
	}}
	agent := testAgent(provider)

	got, _, err := RunTyped[personResult](context.Background(), agent, "who?")
	if err != nil {
		t.Fatalf("RunTyped: %v", err)
	}
	if got.Age != 36 {
		t.Errorf("decoded = %+v", got)
	}
	if provider.calls != 2 {
		t.Errorf("provider calls = %d, want 2 (prose + correction)", provider.calls)
	}
}

// --- collision safety ------------------------------------------------------

// TestRunTypedToolNameCollisionFailsFast pins that a user tool occupying the
// hidden tool's (namespaced) name fails the run with an explicit error instead
// of silently shadowing.
func TestRunTypedToolNameCollisionFailsFast(t *testing.T) {
	provider := &scriptedProvider{turns: []Message{asstText("unused")}}
	agent := testAgent(provider)
	agent.RegisterTool(Func(structuredOutputToolName, "user tool with the same name",
		func(context.Context, struct{}) (string, error) { return "", nil }))

	_, _, err := RunTyped[personResult](context.Background(), agent, "go")
	if err == nil || !strings.Contains(err.Error(), "reserved") {
		t.Fatalf("err = %v, want explicit collision error", err)
	}
	if provider.calls != 0 {
		t.Errorf("provider calls = %d, want 0 (fail before any run)", provider.calls)
	}
}

// TestStructuredOutputNameIsNamespaced pins the hidden tool's name: it is
// stable (tool-call names persist in transcripts) and namespaced so a plain
// "structured_output" user tool cannot collide with it.
func TestStructuredOutputNameIsNamespaced(t *testing.T) {
	if structuredOutputToolName != "automata_structured_output" {
		t.Errorf("structuredOutputToolName = %q, want the namespaced automata_structured_output", structuredOutputToolName)
	}
	// A user tool named structured_output must not collide.
	provider := &scriptedProvider{turns: []Message{
		asstTool("s1", structuredOutputToolName, `{"name":"Ada","age":36}`),
	}}
	agent := testAgent(provider)
	agent.RegisterTool(Func("structured_output", "user tool",
		func(context.Context, struct{}) (string, error) { return "", nil }))
	if _, _, err := RunTyped[personResult](context.Background(), agent, "go"); err != nil {
		t.Fatalf("RunTyped with a structured_output user tool: %v", err)
	}
}

// --- native structured output ----------------------------------------------

// nativeCapturingProvider implements the capability interface and records the
// request options it saw.
type nativeCapturingProvider struct {
	turns      []Message
	calls      int
	options    []CallOptions
	toolCounts []int
}

func (p *nativeCapturingProvider) Invoke(_ context.Context, req Request) (Response, error) {
	if p.calls >= len(p.turns) {
		return Response{}, fmt.Errorf("no script for turn %d", p.calls)
	}
	p.options = append(p.options, req.Options)
	p.toolCounts = append(p.toolCounts, len(req.Tools))
	p.calls++
	return fixtureResponse(p.turns[p.calls-1]), nil
}

func (p *nativeCapturingProvider) SupportsNativeStructuredOutput() bool { return true }

// TestRunTypedNativeModeSupportedProvider pins the native path: no hidden tool
// is advertised, the schema travels via CallOptions.OutputSchema, and the
// response text is validated and decoded.
func TestRunTypedNativeModeSupportedProvider(t *testing.T) {
	provider := &nativeCapturingProvider{turns: []Message{
		asstText(`{"name":"Ada","age":36}`),
	}}
	agent := testAgent(provider)

	got, _, err := RunTyped[personResult](context.Background(), agent, "who?", WithNativeStructuredOutput())
	if err != nil {
		t.Fatalf("RunTyped: %v", err)
	}
	if got.Name != "Ada" || got.Age != 36 {
		t.Errorf("decoded = %+v", got)
	}
	if provider.calls != 1 {
		t.Errorf("provider calls = %d, want 1", provider.calls)
	}
	if provider.toolCounts[0] != 0 {
		t.Errorf("native turn advertised %d tools, want 0 (no hidden tool)", provider.toolCounts[0])
	}
	schema := string(provider.options[0].OutputSchema)
	if schema == "" || !strings.Contains(schema, "\"name\"") || !strings.Contains(schema, "age") {
		t.Errorf("OutputSchema not sent or missing fields: %q", schema)
	}
}

// TestRunTypedNativeModeInvalidPayloadFallsBack pins the one-retry rule: an
// unusable native payload falls back to the hidden-tool path exactly once.
func TestRunTypedNativeModeInvalidPayloadFallsBack(t *testing.T) {
	provider := &nativeCapturingProvider{turns: []Message{
		asstText("I cannot produce that."), // no JSON
		asstTool("s2", structuredOutputToolName, `{"name":"Ada","age":36}`),
	}}
	agent := testAgent(provider)

	got, _, err := RunTyped[personResult](context.Background(), agent, "who?", WithNativeStructuredOutput())
	if err != nil {
		t.Fatalf("RunTyped: %v", err)
	}
	if got.Name != "Ada" {
		t.Errorf("decoded = %+v", got)
	}
	if provider.calls != 2 {
		t.Errorf("provider calls = %d, want 2 (native + fallback)", provider.calls)
	}
}

// TestRunTypedNativeModeUnsupportedProviderUsesHiddenTool pins that the option
// is inert on providers that only implement the base interface.
func TestRunTypedNativeModeUnsupportedProviderUsesHiddenTool(t *testing.T) {
	provider := &scriptedProvider{turns: []Message{
		asstTool("s1", structuredOutputToolName, `{"name":"Ada","age":36}`),
	}}
	agent := testAgent(provider)

	got, _, err := RunTyped[personResult](context.Background(), agent, "go", WithNativeStructuredOutput())
	if err != nil {
		t.Fatalf("RunTyped: %v", err)
	}
	if got.Name != "Ada" {
		t.Errorf("decoded = %+v", got)
	}
	// The hidden tool was injected (scriptedProvider has no capability method).
	capt := &toolCapturingProvider{reply: asstTool("s1", structuredOutputToolName, `{"name":"x","age":1}`)}
	agent2 := testAgent(capt)
	if _, _, err := RunTyped[personResult](context.Background(), agent2, "go", WithNativeStructuredOutput()); err != nil {
		t.Fatalf("RunTyped: %v", err)
	}
	if capt.toolNames[structuredOutputToolName] == "" {
		t.Errorf("hidden tool not advertised on non-native provider")
	}
}

// failOnSecondProvider replies once, then fails with a fixed error.
type failOnSecondProvider struct {
	first Message
	err   error
	calls int
}

func (p *failOnSecondProvider) Invoke(_ context.Context, _ Request) (Response, error) {
	p.calls++
	if p.calls == 1 {
		return fixtureResponse(p.first), nil
	}
	return Response{}, p.err
}

// TestRunTypedCoexistsWithRealTools pins that RunTyped works when the agent also
// has ordinary tools: the model may call a real tool first, then the terminal.
func TestRunTypedCoexistsWithRealTools(t *testing.T) {
	provider := &scriptedProvider{turns: []Message{
		asstTool("c1", "lookup", `{"q":"Ada"}`),
		asstTool("s1", structuredOutputToolName, `{"name":"Ada","age":36}`),
	}}
	agent := testAgent(provider)
	agent.RegisterTool(Func("lookup", "look up a person", func(_ context.Context, _ struct {
		Q string `json:"q"`
	}) (string, error) {
		return "found", nil
	}))

	got, _, err := RunTyped[personResult](context.Background(), agent, "go")
	if err != nil {
		t.Fatalf("RunTyped: %v", err)
	}
	if got.Name != "Ada" {
		t.Errorf("decoded = %+v, want name Ada", got)
	}
}

// TestRunSessionTypedContinuesConversation verifies typed decisions can be
// produced repeatedly without discarding the coordinator's prior turns.
func TestRunSessionTypedContinuesConversation(t *testing.T) {
	provider := &capturingProvider{turns: []Message{
		asstTool("s1", structuredOutputToolName, `{"name":"Ada","age":36}`),
		asstTool("s2", structuredOutputToolName, `{"name":"Grace","age":37}`),
	}}
	agent := testAgent(provider).WithSystemPrompt("Remember prior decisions.")
	session := agent.NewSession()

	first, _, err := RunSessionTyped[personResult](context.Background(), session, "first")
	if err != nil {
		t.Fatalf("first RunSessionTyped: %v", err)
	}
	second, _, err := RunSessionTyped[personResult](context.Background(), session, "second")
	if err != nil {
		t.Fatalf("second RunSessionTyped: %v", err)
	}
	if first.Name != "Ada" || second.Name != "Grace" {
		t.Fatalf("typed results = %+v / %+v, want Ada / Grace", first, second)
	}

	if len(provider.received) != 2 {
		t.Fatalf("provider saw %d requests, want 2", len(provider.received))
	}
	wantRoles := []string{"system", "user", "assistant", "tool", "user"}
	if got := roles(provider.received[1]); strings.Join(got, ",") != strings.Join(wantRoles, ",") {
		t.Errorf("second request roles = %v, want %v", got, wantRoles)
	}
	priorCalls := provider.received[1][2].ToolUses()
	if len(priorCalls) != 1 || priorCalls[0].Name != structuredOutputToolName {
		t.Errorf("second request lost prior structured output call: %+v", priorCalls)
	}
	priorResults := transcriptToolResults(provider.received[1])
	if len(priorResults) != 1 || priorResults[0].ToolUseID != "s1" || priorResults[0].IsError {
		t.Errorf("second request prior tool results = %+v, want successful s1", priorResults)
	}
}

// TestRunSessionTypedResumeJSONRoundTrip proves a persisted typed conversation
// can be resumed and used for another typed decision without reseeding the
// system prompt.
func TestRunSessionTypedResumeJSONRoundTrip(t *testing.T) {
	provider := &capturingProvider{turns: []Message{
		asstTool("s1", structuredOutputToolName, `{"name":"Ada","age":36}`),
		asstTool("s2", structuredOutputToolName, `{"name":"Grace","age":37}`),
	}}
	agent := testAgent(provider).WithSystemPrompt("sys")
	session := agent.NewSession()
	if _, _, err := RunSessionTyped[personResult](context.Background(), session, "first"); err != nil {
		t.Fatalf("first RunSessionTyped: %v", err)
	}

	blob, err := json.Marshal(session.Messages())
	if err != nil {
		t.Fatalf("marshal transcript: %v", err)
	}
	var transcript []Message
	if err := json.Unmarshal(blob, &transcript); err != nil {
		t.Fatalf("unmarshal transcript: %v", err)
	}

	resumed := agent.testResumeSession(transcript)
	got, _, err := RunSessionTyped[personResult](context.Background(), resumed, "second")
	if err != nil {
		t.Fatalf("resumed RunSessionTyped: %v", err)
	}
	if got.Name != "Grace" || got.Age != 37 {
		t.Errorf("resumed typed result = %+v, want Grace/37", got)
	}

	systems := 0
	for _, message := range provider.received[1] {
		if message.Role == "system" {
			systems++
		}
	}
	if systems != 1 {
		t.Errorf("resumed typed request saw %d system messages, want 1", systems)
	}
}

func TestRunSessionTypedFallbackCommitsConversation(t *testing.T) {
	provider := &forcingProvider{replies: []Message{
		asstText("Ada is 36 years old."),
		asstTool("s2", structuredOutputToolName, `{"name":"Ada","age":36}`),
	}}
	session := testAgent(provider).NewSession()
	got, result, err := RunSessionTyped[personResult](context.Background(), session, "who?")
	if err != nil {
		t.Fatalf("RunSessionTyped: %v", err)
	}
	if got.Name != "Ada" || got.Age != 36 {
		t.Errorf("decoded = %+v, want Ada/36", got)
	}
	if len(result.Messages) != 5 || !reflect.DeepEqual(session.Messages(), result.Messages) {
		t.Errorf("fallback conversation = %+v", result.Messages)
	}
}

func TestRunSessionTypedMaxStepsReturnsPartialResult(t *testing.T) {
	provider := &optionsProvider{turns: []Message{
		withUsage(asstTool("c1", "lookup", `{}`), &Usage{InputTokens: 4, OutputTokens: 2}),
	}}
	agent := testAgent(provider).WithMaxSteps(1)
	agent.RegisterTool(Func("lookup", "looks up", func(context.Context, struct{}) (string, error) {
		return "found", nil
	}))
	session := agent.NewSession()

	_, result, err := RunSessionTyped[personResult](context.Background(), session, "go")
	if !errors.Is(err, ErrMaxStepsExceeded) {
		t.Fatalf("err = %v, want ErrMaxStepsExceeded", err)
	}
	if result.StopReason != StopMaxSteps || result.Steps != 1 {
		t.Errorf("partial result stop/steps = %q/%d, want max_steps/1", result.StopReason, result.Steps)
	}
	if result.Usage != (Usage{InputTokens: 4, OutputTokens: 2}) {
		t.Errorf("partial usage = %+v, want {4 2}", result.Usage)
	}
	if got := roles(result.Messages); len(got) != 3 {
		t.Errorf("partial transcript roles = %v, want user/assistant/tool", got)
	}
	if got := roles(session.Messages()); strings.Join(got, ",") != strings.Join(roles(result.Messages), ",") {
		t.Errorf("committed transcript roles = %v, want %v", got, roles(result.Messages))
	}
}

// --- test providers -------------------------------------------------------

// toolCapturingProvider records the tool schemas it was sent, then replies once.
type toolCapturingProvider struct {
	reply     Message
	toolNames map[string]string // name -> schema JSON
	calls     int
}

func (p *toolCapturingProvider) Invoke(_ context.Context, req Request) (Response, error) {
	if p.toolNames == nil {
		p.toolNames = map[string]string{}
	}
	for _, tl := range req.Tools {
		p.toolNames[tl.Name] = string(tl.InputSchema)
	}
	p.calls++
	return fixtureResponse(p.reply), nil
}

// forcingProvider replies with a scripted sequence and records the CallOptions
// of each turn, so a test can assert the forced-turn options.
type forcingProvider struct {
	replies []Message
	calls   int
	seen    []CallOptions
}

func (p *forcingProvider) Invoke(_ context.Context, req Request) (Response, error) {
	p.seen = append(p.seen, req.Options)
	m := p.replies[min(p.calls, len(p.replies)-1)]
	p.calls++
	return fixtureResponse(m), nil
}
