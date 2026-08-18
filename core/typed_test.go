package core

import (
	"context"
	"encoding/json"
	"errors"
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
	agent := New(provider)

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
	agent := New(capt)

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
	agent := New(capt).WithDefaultCallOptions(CallOptions{ThinkingBudget: 2048})

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

// TestRunTypedDecodeError pins that malformed arguments surface as a decode
// error rather than a silent zero value.
func TestRunTypedDecodeError(t *testing.T) {
	provider := &scriptedProvider{turns: []Message{
		asstTool("s1", structuredOutputToolName, `{"name":"x","age":"not-a-number"}`),
	}}
	agent := New(provider)

	_, _, err := RunTyped[personResult](context.Background(), agent, "go")
	if err == nil || !strings.Contains(err.Error(), "decode structured output") {
		t.Errorf("err = %v, want a decode error", err)
	}
}

// TestRunTypedCoexistsWithRealTools pins that RunTyped works when the agent also
// has ordinary tools: the model may call a real tool first, then the terminal.
func TestRunTypedCoexistsWithRealTools(t *testing.T) {
	provider := &scriptedProvider{turns: []Message{
		asstTool("c1", "lookup", `{"q":"Ada"}`),
		asstTool("s1", structuredOutputToolName, `{"name":"Ada","age":36}`),
	}}
	agent := New(provider)
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
	agent := New(provider).WithSystemPrompt("Remember prior decisions.")
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
	agent := New(provider).WithSystemPrompt("sys")
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

	resumed := agent.ResumeSession(transcript)
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

// TestRunSessionTypedFallbackCheckpointsEachRun pins the checkpoint cadence:
// the prose attempt is committed first, followed by the forced typed run.
func TestRunSessionTypedFallbackCheckpointsEachRun(t *testing.T) {
	provider := &forcingProvider{replies: []Message{
		asstText("Ada is 36 years old."),
		asstTool("s2", structuredOutputToolName, `{"name":"Ada","age":36}`),
	}}
	session := New(provider).NewSession()
	var transcriptLengths []int
	hook := WithPostRunHook(func(_ context.Context, result RunResult, runErr error) error {
		if runErr != nil {
			t.Errorf("hook runErr = %v, want nil", runErr)
		}
		transcriptLengths = append(transcriptLengths, len(result.Messages))
		return nil
	})

	got, _, err := RunSessionTyped[personResult](context.Background(), session, "who?", hook)
	if err != nil {
		t.Fatalf("RunSessionTyped: %v", err)
	}
	if got.Name != "Ada" || got.Age != 36 {
		t.Errorf("decoded = %+v, want Ada/36", got)
	}
	if len(transcriptLengths) != 2 || transcriptLengths[0] != 2 || transcriptLengths[1] != 5 {
		t.Errorf("checkpoint transcript lengths = %v, want [2 5]", transcriptLengths)
	}
}

func TestRunSessionTypedStopsWhenFirstCheckpointFails(t *testing.T) {
	checkpointErr := errors.New("checkpoint failed")
	provider := &forcingProvider{replies: []Message{
		asstText("Ada is 36 years old."),
		asstTool("s2", structuredOutputToolName, `{"name":"Ada","age":36}`),
	}}
	session := New(provider).NewSession()

	_, result, err := RunSessionTyped[personResult](context.Background(), session, "who?",
		WithPostRunHook(func(context.Context, RunResult, error) error {
			return checkpointErr
		}),
	)
	if !errors.Is(err, checkpointErr) {
		t.Fatalf("err = %v, want checkpoint failure", err)
	}
	if provider.calls != 1 {
		t.Errorf("provider calls = %d, want 1 (forced fallback must not start)", provider.calls)
	}
	if result.Output != "Ada is 36 years old." || len(session.Messages()) != 2 {
		t.Errorf("first run result/session = %+v / %+v", result, session.Messages())
	}
}

func TestRunSessionTypedMaxStepsReturnsPartialResult(t *testing.T) {
	provider := &optionsProvider{turns: []Message{
		withUsage(asstTool("c1", "lookup", `{}`), &Usage{InputTokens: 4, OutputTokens: 2}),
	}}
	agent := New(provider).WithMaxSteps(1)
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
		p.toolNames[tl.Name()] = string(tl.Schema())
	}
	p.calls++
	return Response{Message: p.reply}, nil
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
	return Response{Message: m}, nil
}
