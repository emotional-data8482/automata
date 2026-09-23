package core

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
)

type personResult struct {
	Name string `json:"name"`
	Age  int    `json:"age"`
}

// TestTypedHappyPath pins the common case: the model calls the injected
// structured_output tool, and Decode reads its arguments into T.
func TestTypedHappyPath(t *testing.T) {
	provider := &scriptedProvider{turns: []Message{
		asstTool("s1", structuredOutputToolName, `{"name":"Ada","age":36}`),
	}}
	agent := testAgent(provider)

	got, res, err := runTyped[personResult](t, agent, "who?")
	if err != nil {
		t.Fatalf("typed run: %v", err)
	}
	if got.Name != "Ada" || got.Age != 36 {
		t.Errorf("decoded = %+v, want {Ada 36}", got)
	}
	if res.StopReason != StopEndTurn {
		t.Errorf("StopReason = %q, want %q", res.StopReason, StopEndTurn)
	}
}

// TestTypedInjectsSchemaTool pins that the injected tool's JSON schema is
// derived from T and advertised to the model.
func TestTypedInjectsSchemaTool(t *testing.T) {
	capt := &toolCapturingProvider{reply: asstTool("s1", structuredOutputToolName, `{"name":"x","age":1}`)}
	agent := testAgent(capt)

	if _, _, err := runTyped[personResult](t, agent, "go"); err != nil {
		t.Fatalf("typed run: %v", err)
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

// TestTypedForcesToolOnTextFallback pins the fallback: when the model answers
// with prose, the run issues a second turn forcing the tool with thinking
// disabled, and decodes that.
func TestTypedForcesToolOnTextFallback(t *testing.T) {
	capt := &forcingProvider{
		replies: []Message{
			asstText("Ada is 36 years old."),                                    // turn 1: prose, no tool call
			asstTool("s2", structuredOutputToolName, `{"name":"Ada","age":36}`), // forced turn
		},
	}
	agent := testAgent(capt).WithCallOptions(CallOptions{ThinkingBudget: 2048})

	got, _, err := runTyped[personResult](t, agent, "who?")
	if err != nil {
		t.Fatalf("typed run: %v", err)
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

// TestTypedDecodeErrorCorrects pins that malformed arguments surface as a
// typed error carrying the decode cause — and that a model which fixes its
// malformed JSON on the correction turn still produces a value.
func TestTypedDecodeErrorCorrects(t *testing.T) {
	provider := &scriptedProvider{turns: []Message{
		asstTool("s1", structuredOutputToolName, `{"name":"x","age":"not-a-number"}`),
		asstTool("s2", structuredOutputToolName, `{"name":"x","age":3}`),
	}}
	agent := testAgent(provider)

	got, _, err := runTyped[personResult](t, agent, "go")
	if err != nil {
		t.Fatalf("typed run: %v", err)
	}
	if got.Age != 3 {
		t.Errorf("decoded = %+v, want age 3", got)
	}
}

// TestTypedDecodeErrorBudgetExhausted pins that wrong-shaped payloads keep
// shared schema violation detail on the typed error once corrections run out.
func TestTypedDecodeErrorBudgetExhausted(t *testing.T) {
	provider := &scriptedProvider{turns: []Message{
		asstTool("s1", structuredOutputToolName, `{"name":"x","age":"not-a-number"}`),
		asstTool("s2", structuredOutputToolName, `{"name":"x","age":"still-not-a-number"}`), // still wrong type
	}}
	agent := testAgent(provider)

	_, res, err := runTyped[personResult](t, agent, "go")
	if !errors.Is(err, ErrInvalidStructuredOutput) {
		t.Fatalf("err = %v, want ErrInvalidStructuredOutput", err)
	}
	var typed *InvalidStructuredOutputError
	if !errors.As(err, &typed) || len(typed.Violations) == 0 {
		t.Fatalf("errors.As = %v, want InvalidStructuredOutputError with violations", err)
	}
	if !strings.Contains(typed.Violations[0], "age") || !strings.Contains(typed.Violations[0], "expected integer") {
		t.Errorf("violations = %v, want age integer violation", typed.Violations)
	}
	if len(res.Messages) == 0 || res.Turns == 0 {
		t.Errorf("RunResult not populated alongside the error: %+v", res)
	}
}

// TestTypedMissingRequiredFieldRejected pins the headline fix: a payload
// missing a required field can never be returned as a zero-filled T.
func TestTypedMissingRequiredFieldRejected(t *testing.T) {
	provider := &scriptedProvider{turns: []Message{
		asstTool("s1", structuredOutputToolName, `{"name":"Ada"}`), // age missing
		asstTool("s2", structuredOutputToolName, `{"name":"Ada"}`), // still missing
	}}
	agent := testAgent(provider)

	got, res, err := runTyped[personResult](t, agent, "go")
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

// TestTypedWrongTypeRejected pins wrong-typed fields are validation
// failures, not decode failures, and are corrected the same way.
func TestTypedWrongTypeRejected(t *testing.T) {
	provider := &scriptedProvider{turns: []Message{
		asstTool("s1", structuredOutputToolName, `{"name":42,"age":36}`),
		asstTool("s2", structuredOutputToolName, `{"name":"Ada","age":36}`),
	}}
	agent := testAgent(provider)

	got, _, err := runTyped[personResult](t, agent, "go")
	if err != nil {
		t.Fatalf("typed run: %v", err)
	}
	if got.Name != "Ada" {
		t.Errorf("decoded = %+v, want Ada", got)
	}
}

// TestTypedCorrectionPromptListsViolations pins the correction evidence fed
// back to the model: the violation list plus the instruction to call the tool again.
func TestTypedCorrectionPromptListsViolations(t *testing.T) {
	provider := &capturingProvider{turns: []Message{
		asstTool("s1", structuredOutputToolName, `{"name":"Ada"}`),
		asstTool("s2", structuredOutputToolName, `{"name":"Ada","age":36}`),
	}}
	agent := testAgent(provider)

	if _, _, err := runTyped[personResult](t, agent, "go"); err != nil {
		t.Fatalf("typed run: %v", err)
	}
	if len(provider.received) != 2 {
		t.Fatalf("provider saw %d requests, want 2", len(provider.received))
	}
	second := provider.received[1]
	last := second[len(second)-1]
	if last.Role != "tool" {
		t.Fatalf("last message of correction turn = %q, want tool result evidence", last.Role)
	}
	resultBlock, ok := last.Blocks[0].(ToolResultBlock)
	if !ok {
		t.Fatalf("last correction evidence block = %T, want ToolResultBlock", last.Blocks[0])
	}
	text := Message{Blocks: resultBlock.Content}.Text()
	if !strings.Contains(text, "age") || !strings.Contains(text, "missing required field") {
		t.Errorf("correction prompt does not name the violation: %q", text)
	}
	if !strings.Contains(text, structuredOutputToolName) {
		t.Errorf("correction prompt does not name the tool: %q", text)
	}
}

// TestTypedCorrectionBudgetZeroDisables pins withCorrections(0): the
// invalid payload errors immediately with no correction or forced turn.
func TestTypedCorrectionBudgetZeroDisables(t *testing.T) {
	provider := &scriptedProvider{turns: []Message{
		asstTool("s1", structuredOutputToolName, `{"name":"Ada"}`),
	}}
	agent := testAgent(provider)

	_, _, err := runTyped[personResult](t, agent, "go", withCorrections(0))
	if !errors.Is(err, ErrInvalidStructuredOutput) {
		t.Fatalf("err = %v, want ErrInvalidStructuredOutput", err)
	}
	if provider.calls != 1 {
		t.Errorf("provider calls = %d, want 1 (no correction turn)", provider.calls)
	}
}

// TestTypedCorrectionBudgetTwo pins the budget is configurable and counted
// per typed run.
func TestTypedCorrectionBudgetTwo(t *testing.T) {
	provider := &scriptedProvider{turns: []Message{
		asstTool("s1", structuredOutputToolName, `{"name":"Ada"}`),
		asstTool("s2", structuredOutputToolName, `{"age":36}`),
		asstTool("s3", structuredOutputToolName, `{"name":"Grace","age":37}`),
	}}
	agent := testAgent(provider)

	got, _, err := runTyped[personResult](t, agent, "go", withCorrections(2))
	if err != nil {
		t.Fatalf("typed run: %v", err)
	}
	if got.Name != "Grace" {
		t.Errorf("decoded = %+v, want Grace", got)
	}
	if provider.calls != 3 {
		t.Errorf("provider calls = %d, want 3", provider.calls)
	}
}

// TestTypedCorrectionProviderErrorPropagates pins that a correction turn
// failing for unrelated reasons returns that error, not a validation error.
func TestTypedCorrectionProviderErrorPropagates(t *testing.T) {
	providerErr := errors.New("provider exploded")
	provider := &failOnSecondProvider{first: asstTool("s1", structuredOutputToolName, `{"name":"Ada"}`), err: providerErr}
	agent := testAgent(provider)

	_, _, err := runTyped[personResult](t, agent, "go")
	if err == nil || !strings.Contains(err.Error(), providerErr.Error()) {
		t.Fatalf("err = %v, want provider error propagated", err)
	}
	if errors.Is(err, ErrInvalidStructuredOutput) {
		t.Errorf("provider error masked as validation error")
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

// TestTypedProseFencedJSON pins the cheap prose path: a fenced ```json
// payload validates and returns with zero extra provider turns.
func TestTypedProseFencedJSON(t *testing.T) {
	provider := &scriptedProvider{turns: []Message{
		asstText("Here is the answer:\n```json\n{\"name\":\"Ada\",\"age\":36}\n```"),
	}}
	agent := testAgent(provider)

	got, _, err := runTyped[personResult](t, agent, "who?")
	if err != nil {
		t.Fatalf("typed run: %v", err)
	}
	if got.Name != "Ada" || got.Age != 36 {
		t.Errorf("decoded = %+v", got)
	}
	if provider.calls != 1 {
		t.Errorf("provider calls = %d, want 1 (no forced turn)", provider.calls)
	}
}

// TestTypedProseBareObject pins that an unfenced JSON object is extracted.
func TestTypedProseBareObject(t *testing.T) {
	provider := &scriptedProvider{turns: []Message{
		asstText(`The answer is {"name": "Grace", "age": 37} — hope that helps.`),
	}}
	agent := testAgent(provider)

	got, _, err := runTyped[personResult](t, agent, "who?")
	if err != nil {
		t.Fatalf("typed run: %v", err)
	}
	if got.Name != "Grace" || got.Age != 37 {
		t.Errorf("decoded = %+v", got)
	}
	if provider.calls != 1 {
		t.Errorf("provider calls = %d, want 1", provider.calls)
	}
}

// TestTypedProseWrongShapeCorrects pins that valid-JSON-wrong-shape prose
// routes to the correction turn before the forced fallback.
func TestTypedProseWrongShapeCorrects(t *testing.T) {
	provider := &scriptedProvider{turns: []Message{
		asstText(`{"name":"Ada"}`), // missing age
		asstTool("s2", structuredOutputToolName, `{"name":"Ada","age":36}`),
	}}
	agent := testAgent(provider)

	got, _, err := runTyped[personResult](t, agent, "who?")
	if err != nil {
		t.Fatalf("typed run: %v", err)
	}
	if got.Age != 36 {
		t.Errorf("decoded = %+v", got)
	}
	if provider.calls != 2 {
		t.Errorf("provider calls = %d, want 2 (prose + correction)", provider.calls)
	}
}

// --- collision safety ------------------------------------------------------

// TestTypedToolNameCollisionFailsFast pins that a user tool occupying the
// hidden tool's (namespaced) name fails the run with an explicit error instead
// of silently shadowing.
func TestTypedToolNameCollisionFailsFast(t *testing.T) {
	provider := &scriptedProvider{turns: []Message{asstText("unused")}}
	agent := testAgent(provider)
	agent.RegisterTool(Func(structuredOutputToolName, "user tool with the same name",
		func(context.Context, struct{}) (string, error) { return "", nil }))

	_, _, err := runTyped[personResult](t, agent, "go")
	if err == nil || !strings.Contains(err.Error(), `duplicate tool "automata_structured_output"`) {
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
	if _, _, err := runTyped[personResult](t, agent, "go"); err != nil {
		t.Fatalf("typed run with a structured_output user tool: %v", err)
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

// TestTypedNativeModeSupportedProvider pins the native path: no hidden tool
// is advertised, the schema travels via CallOptions.OutputSchema, and the
// response text is validated and decoded.
func TestTypedNativeModeSupportedProvider(t *testing.T) {
	provider := &nativeCapturingProvider{turns: []Message{
		asstText(`{"name":"Ada","age":36}`),
	}}
	agent := testAgent(provider)

	got, _, err := runTyped[personResult](t, agent, "who?", withNative())
	if err != nil {
		t.Fatalf("typed run: %v", err)
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

// TestTypedNativeModeInvalidPayloadFallsBack pins the shared native correction
// path: an unusable native payload is corrected in prose inside the same run.
func TestTypedNativeModeInvalidPayloadFallsBack(t *testing.T) {
	provider := &nativeCapturingProvider{turns: []Message{
		asstText("I cannot produce that."), // no JSON
		asstText(`{"name":"Ada","age":36}`),
	}}
	agent := testAgent(provider)

	got, _, err := runTyped[personResult](t, agent, "who?", withNative())
	if err != nil {
		t.Fatalf("typed run: %v", err)
	}
	if got.Name != "Ada" {
		t.Errorf("decoded = %+v", got)
	}
	if provider.calls != 2 {
		t.Errorf("provider calls = %d, want 2 (native + correction)", provider.calls)
	}
}

// TestTypedNativeModeUnsupportedProviderUsesHiddenTool pins that the option
// is inert on providers that only implement the base interface.
func TestTypedNativeModeUnsupportedProviderUsesHiddenTool(t *testing.T) {
	provider := &scriptedProvider{turns: []Message{
		asstTool("s1", structuredOutputToolName, `{"name":"Ada","age":36}`),
	}}
	agent := testAgent(provider)

	got, _, err := runTyped[personResult](t, agent, "go", withNative())
	if err != nil {
		t.Fatalf("typed run: %v", err)
	}
	if got.Name != "Ada" {
		t.Errorf("decoded = %+v", got)
	}
	// The hidden tool was injected (scriptedProvider has no capability method).
	capt := &toolCapturingProvider{reply: asstTool("s1", structuredOutputToolName, `{"name":"x","age":1}`)}
	agent2 := testAgent(capt)
	if _, _, err := runTyped[personResult](t, agent2, "go", withNative()); err != nil {
		t.Fatalf("typed run: %v", err)
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

// TestTypedCoexistsWithRealTools pins that typed output works when the agent also
// has ordinary tools: the model may call a real tool first, then the terminal.
func TestTypedCoexistsWithRealTools(t *testing.T) {
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

	got, _, err := runTyped[personResult](t, agent, "go")
	if err != nil {
		t.Fatalf("typed run: %v", err)
	}
	if got.Name != "Ada" {
		t.Errorf("decoded = %+v, want name Ada", got)
	}
}

// TestTypedConversationContinuesPriorTurns verifies typed decisions can be
// produced by successive conversation turns without discarding the
// coordinator's prior turns.
func TestTypedConversationContinuesPriorTurns(t *testing.T) {
	provider := &capturingProvider{turns: []Message{
		asstTool("s1", structuredOutputToolName, `{"name":"Ada","age":36}`),
		asstTool("s2", structuredOutputToolName, `{"name":"Grace","age":37}`),
	}}
	agent, err := New(provider, AgentConfig{
		SystemPrompt:     "Remember prior decisions.",
		StructuredOutput: &StructuredOutputConfig{Schema: OutputSchema[personResult]()},
	})
	if err != nil {
		t.Fatal(err)
	}
	runtime := newTestRuntime(t)
	ref, err := runtime.Register("coordinator", "v1", agent)
	if err != nil {
		t.Fatal(err)
	}
	thread := ConversationRef{ID: "decisions"}
	firstResult, err := runtime.Run(context.Background(), ref, "first", WithConversation(thread, ""))
	if err != nil {
		t.Fatalf("first turn: %v", err)
	}
	secondResult, err := runtime.Run(context.Background(), ref, "second", WithConversation(thread, firstResult.RunID))
	if err != nil {
		t.Fatalf("second turn: %v", err)
	}
	first, err := Decode[personResult](firstResult)
	if err != nil {
		t.Fatal(err)
	}
	second, err := Decode[personResult](secondResult)
	if err != nil {
		t.Fatal(err)
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

func TestTypedFallbackCommitsTranscript(t *testing.T) {
	provider := &forcingProvider{replies: []Message{
		asstText("Ada is 36 years old."),
		asstTool("s2", structuredOutputToolName, `{"name":"Ada","age":36}`),
	}}
	got, result, err := runTyped[personResult](t, testAgent(provider), "who?")
	if err != nil {
		t.Fatalf("runTyped: %v", err)
	}
	if got.Name != "Ada" || got.Age != 36 {
		t.Errorf("decoded = %+v, want Ada/36", got)
	}
	if len(result.Messages) != 5 {
		t.Errorf("fallback transcript = %+v", result.Messages)
	}
}

func TestTypedMaxTurnsReturnsPartialResult(t *testing.T) {
	provider := &optionsProvider{turns: []Message{
		withUsage(asstTool("c1", "lookup", `{}`), &Usage{InputTokens: 4, OutputTokens: 2}),
	}}
	agent := testAgent(provider).WithMaxTurns(1)
	agent.RegisterTool(Func("lookup", "looks up", func(context.Context, struct{}) (string, error) {
		return "found", nil
	}))

	_, result, err := runTyped[personResult](t, agent, "go")
	if !errors.Is(err, ErrMaxTurnsExceeded) {
		t.Fatalf("err = %v, want ErrMaxTurnsExceeded", err)
	}
	if result.StopReason != StopMaxTurns || result.Turns != 1 {
		t.Errorf("partial result stop/turns = %q/%d, want max_turns/1", result.StopReason, result.Turns)
	}
	if result.Usage != (Usage{InputTokens: 4, OutputTokens: 2}) {
		t.Errorf("partial usage = %+v, want {4 2}", result.Usage)
	}
	if got := roles(result.Messages); len(got) != 3 {
		t.Errorf("partial transcript roles = %v, want user/assistant/tool", got)
	}
}

// --- helpers --------------------------------------------------------------

type typedRun struct {
	corrections int
	native      bool
}

type typedOption func(*typedRun)

func withCorrections(n int) typedOption { return func(r *typedRun) { r.corrections = n } }
func withNative() typedOption           { return func(r *typedRun) { r.native = true } }

// runTyped declares T's schema on the fixture agent, with one correction
// turn unless overridden, runs it through a Runtime, and decodes the
// accepted output with Decode.
func runTyped[T any](t testing.TB, agent *Agent, task string, opts ...typedOption) (T, RunResult, error) {
	t.Helper()
	cfg := typedRun{corrections: 1}
	for _, opt := range opts {
		opt(&cfg)
	}
	contract, err := validateStructuredOutputDeclaration(&StructuredOutputConfig{
		Schema: OutputSchema[T](), MaxCorrections: cfg.corrections, Native: cfg.native,
	})
	if err != nil {
		t.Fatal(err)
	}
	agent.structuredOutput = contract
	var zero T
	result, err := runAgent(t, agent, task)
	if err != nil {
		return zero, result, err
	}
	value, err := Decode[T](result)
	return value, result, err
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
