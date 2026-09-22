package core

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const summarySchema = `{"type":"object","properties":{"summary":{"type":"string"}},"required":["summary"],"additionalProperties":false}`

func summaryContract() *StructuredOutputConfig {
	return &StructuredOutputConfig{Schema: json.RawMessage(summarySchema), MaxCorrections: 1}
}

func newStructuredAgent(t *testing.T, p Provider, tools []Tool, cfg *StructuredOutputConfig, maxTurns int) *Agent {
	t.Helper()
	agent, err := New(p, AgentConfig{
		Tools: tools, MaxTurns: maxTurns, StructuredOutput: cfg,
	})
	if err != nil {
		t.Fatal(err)
	}
	return agent
}

// structuredRuntime builds a runtime over a shared memory store so a test can
// close one runtime and recover the same store with another.
type structuredFixture struct {
	store  Store
	writes atomic.Int64
}

func newStructuredFixture() *structuredFixture {
	return &structuredFixture{store: &memoryStore{buckets: make(map[string]map[string][]byte)}}
}

func (f *structuredFixture) writeTool() Tool {
	tool := FuncResult("write", "write a report", func(_ context.Context, input struct {
		Path string `json:"path"`
	}) (ToolResult, error) {
		f.writes.Add(1)
		result := TextResult("written")
		result.Effect = EffectReport{Status: EffectApplied, Receipt: "write-1"}
		return result, nil
	})
	return WithToolEffectPolicy(tool, ToolEffectPolicy{
		Kind: ToolEffectMutating, Scope: "docs",
		SemanticKey: func(raw json.RawMessage) (string, error) {
			var in struct {
				Path string `json:"path"`
			}
			if err := json.Unmarshal(raw, &in); err != nil {
				return "", err
			}
			return in.Path, nil
		},
	})
}

func structuredCall(id, raw string) ToolUseBlock {
	return toolUse(id, structuredOutputToolName, raw)
}

func asstCalls(calls ...Block) Message {
	return AssistantMessage(calls...)
}

func runtimeWithAgent(t *testing.T, f *structuredFixture, agent *Agent) *Runtime {
	t.Helper()
	runtime, err := NewRuntime(context.Background(), RuntimeConfig{Store: f.store})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = runtime.Close() })
	if err := runtime.Register("agent", "v1", agent); err != nil {
		t.Fatal(err)
	}
	return runtime
}

// TestRuntimeStructuredCorrectionKeepsReceiptsAndBlocksDuplicates drives the
// V06 acceptance path: a known write, an invalid final output corrected inside
// the same durable run, a newly proposed duplicate write rejected by the
// configured guard, and retained receipts and budgets.
func TestRuntimeStructuredCorrectionKeepsReceiptsAndBlocksDuplicates(t *testing.T) {
	f := newStructuredFixture()
	turns := []Message{
		asstCalls(toolUse("w1", "write", `{"path":"report.txt"}`)),
		asstCalls(structuredCall("s1", `{"summary":5}`)),
		asstCalls(toolUse("w2", "write", `{"path":"report.txt"}`)),
		asstCalls(structuredCall("s2", `{"summary":"done"}`)),
	}
	agent := newStructuredAgent(t, &scriptedProvider{turns: turns}, []Tool{f.writeTool()}, summaryContract(), 8)
	runtime := runtimeWithAgent(t, f, agent)
	handle, err := runtime.Submit(context.Background(), "agent", "v1", "produce summary", SubmitOptions{})
	if err != nil {
		t.Fatal(err)
	}
	result, err := handle.Await(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != RunCompleted || string(result.StructuredOutput) != `{"summary":"done"}` {
		t.Fatalf("result = %#v", result)
	}
	if f.writes.Load() != 1 {
		t.Fatalf("external writes = %d, want 1 (guard rejected the duplicate)", f.writes.Load())
	}
	if result.Turns != 4 {
		t.Fatalf("turns = %d, want 4 cumulative provider turns (budgets not reset)", result.Turns)
	}
	snapshot, err := handle.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.State != RuntimeTerminal {
		t.Fatalf("state = %s", snapshot.State)
	}
	// The accepted write receipt survives the invalid output and correction.
	var receipt string
	var denied bool
	for _, batch := range snapshot.ToolBatches {
		for _, invocation := range batch.Invocations {
			if invocation.Call.Name == "write" && invocation.Effect.Status == EffectApplied {
				receipt = invocation.Effect.Receipt
			}
			if strings.Contains(invocation.Result.Text(), "denied: semantic mutation already applied or unresolved") {
				denied = true
			}
		}
	}
	if receipt != "write-1" || !denied {
		t.Fatalf("receipt = %q, duplicate denied = %v; batches = %#v", receipt, denied, snapshot.ToolBatches)
	}
	// The invalid terminal payload is visible in the transcript as correction
	// evidence, never as accepted output.
	if result.Output != "" {
		t.Fatalf("model-facing output = %q, want empty for a terminal-tool completion", result.Output)
	}
}

// TestRuntimeStructuredCorrectionBudgetExhaustedFails pins the bounded budget:
// with zero remaining corrections an invalid payload fails the run with a
// durable invalid-output classification while retaining prior evidence.
func TestRuntimeStructuredCorrectionBudgetExhaustedFails(t *testing.T) {
	f := newStructuredFixture()
	turns := []Message{
		asstCalls(toolUse("w1", "write", `{"path":"report.txt"}`)),
		asstCalls(structuredCall("s1", `{"summary":5}`)),
		asstCalls(structuredCall("s2", `{"summary":7}`)),
	}
	cfg := &StructuredOutputConfig{Schema: json.RawMessage(summarySchema)} // MaxCorrections: 0
	agent := newStructuredAgent(t, &scriptedProvider{turns: turns}, []Tool{f.writeTool()}, cfg, 8)
	runtime := runtimeWithAgent(t, f, agent)
	handle, err := runtime.Submit(context.Background(), "agent", "v1", "produce summary", SubmitOptions{})
	if err != nil {
		t.Fatal(err)
	}
	result, err := handle.Await(context.Background())
	if err == nil || !errors.Is(err, ErrInvalidStructuredOutput) {
		t.Fatalf("await error = %v, want ErrInvalidStructuredOutput", err)
	}
	// Violation detail degrades to the recorded message across restart; the
	// sentinel classification survives.
	if !strings.Contains(err.Error(), "summary: expected string, got number") {
		t.Fatalf("error detail lost across restart: %v", err)
	}
	if result.Status != RunFailed || f.writes.Load() != 1 {
		t.Fatalf("result = %#v, writes = %d", result, f.writes.Load())
	}
	snapshot, err := handle.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.ErrorKind != "invalid_structured_output" || snapshot.State != RuntimeTerminal {
		t.Fatalf("snapshot = %#v", snapshot)
	}
	receipt := ""
	for _, batch := range snapshot.ToolBatches {
		for _, invocation := range batch.Invocations {
			if invocation.Effect.Status == EffectApplied {
				receipt = invocation.Effect.Receipt
			}
		}
	}
	if receipt != "write-1" {
		t.Fatalf("accepted write receipt = %q, want retained", receipt)
	}
}

// TestRuntimeStructuredFinalizeSurvivesWorkerStop pins the crash window: when
// a validated structured payload committed with its transcript and the worker
// stopped before finalization, recovery finalizes the same run without a new
// provider turn.
func TestRuntimeStructuredFinalizeSurvivesWorkerStop(t *testing.T) {
	f := newStructuredFixture()
	initializer, err := NewRuntime(context.Background(), RuntimeConfig{Store: noCloseStore{Store: f.store}})
	if err != nil {
		t.Fatal(err)
	}
	_ = initializer.Close()

	// Seed a run whose validated structured payload committed atomically with
	// its transcript; the worker stopped before finalization.
	messages := []Message{
		UserMessage("produce summary"),
		asstCalls(structuredCall("s1", `{"summary":"done"}`)),
		ToolResultBlockMessage("s1", Blocks{TextBlock{Text: "ok"}}, false),
	}
	call := structuredCall("s1", `{"summary":"done"}`)
	record := storedRuntimeRun{
		Version: runtimeEncodingVersion, RunID: "structured-finalize", DefinitionID: "agent", DefinitionRevision: "v1",
		Task: "produce summary", State: RuntimeRunning, Generation: 3,
		Result:         RunResult{RunID: "structured-finalize", Turns: 1, StructuredOutput: json.RawMessage(`{"summary":"done"}`)},
		LastTransition: "batch_committed",
		EffectiveTools: []string{structuredOutputToolName},
	}
	batch := storedToolBatch{Version: runtimeEncodingVersion, RunID: record.RunID, BatchID: "0000000000000000", Count: 1}
	if err := f.store.Transaction(context.Background(), true, func(tx StoreTransaction) error {
		if err := appendTranscript(tx, record.RunID, &record, messages); err != nil {
			return err
		}
		if err := putRuntimeRun(tx, record); err != nil {
			return err
		}
		if err := putStoredJSON(tx, runtimeBatchesBucket, batchStorageKey(record.RunID, batch.BatchID), batch); err != nil {
			return err
		}
		invocation := storedToolInvocation{
			Version: runtimeEncodingVersion, RunID: record.RunID, BatchID: batch.BatchID,
			OperationID: record.RunID + ":0:0", Ordinal: 0, Call: call,
			State: ToolInvocationCompleted, EffectKind: ToolEffectLegacy,
			Result: TextResult("ok"),
		}
		return putStoredJSON(tx, runtimeInvocationsBucket, invocationStorageKey(record.RunID, batch.BatchID, 0), invocation)
	}); err != nil {
		t.Fatal(err)
	}

	agent := newStructuredAgent(t, &scriptedProvider{turns: []Message{asstText("must not be invoked")}}, nil, summaryContract(), 8)
	runtime := runtimeWithAgent(t, f, agent)
	if err := runtime.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	result, err := runtime.Handle("structured-finalize").Await(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if string(result.StructuredOutput) != `{"summary":"done"}` || result.Status != RunCompleted {
		t.Fatalf("finalized result = %#v", result)
	}
}

// seedCorrectedRun commits the durable state of a run that already consumed
// one correction: write applied, invalid terminal payload corrected once.
func seedCorrectedRun(t *testing.T, f *structuredFixture, runID string) {
	t.Helper()
	writeCall := toolUse("w1", "write", `{"path":"report.txt"}`)
	invalidCall := structuredCall("s1", `{"summary":5}`)
	violations := correctionPrompt(&InvalidStructuredOutputError{
		Violations: []string{"summary: expected string, got number"},
	}, structuredOutputToolName)
	messages := []Message{
		UserMessage("produce summary"),
		asstCalls(writeCall),
		ToolResultBlockMessage("w1", Blocks{TextBlock{Text: "written"}}, false),
		asstCalls(invalidCall),
		ToolResultBlockMessage("s1", Blocks{TextBlock{Text: violations}}, true),
	}
	record := storedRuntimeRun{
		Version: runtimeEncodingVersion, RunID: runID, DefinitionID: "agent", DefinitionRevision: "v1",
		Task: "produce summary", State: RuntimeRunning, Generation: 4,
		Result:         RunResult{RunID: runID, Turns: 2, Steps: 2, FinalMessage: messages[3]},
		Corrections:    1,
		LastTransition: "batch_committed",
		EffectiveTools: []string{"write", structuredOutputToolName},
	}
	writeBatch := storedToolBatch{Version: runtimeEncodingVersion, RunID: runID, BatchID: "batch-write", Count: 1}
	terminalBatch := storedToolBatch{Version: runtimeEncodingVersion, RunID: runID, BatchID: "batch-terminal", Count: 1}
	guardKey := effectGuardStorageKey("docs", "write", "report.txt")
	if err := f.store.Transaction(context.Background(), true, func(tx StoreTransaction) error {
		if err := appendTranscript(tx, runID, &record, messages); err != nil {
			return err
		}
		if err := putRuntimeRun(tx, record); err != nil {
			return err
		}
		for _, batch := range []storedToolBatch{writeBatch, terminalBatch} {
			if err := putStoredJSON(tx, runtimeBatchesBucket, batchStorageKey(runID, batch.BatchID), batch); err != nil {
				return err
			}
		}
		writeInvocation := storedToolInvocation{
			Version: runtimeEncodingVersion, RunID: runID, BatchID: writeBatch.BatchID,
			OperationID: runID + ":batch-write:0", Ordinal: 0, Call: writeCall,
			State: ToolInvocationCompleted, EffectKind: ToolEffectMutating,
			Result:   TextResult("written"),
			Effect:   EffectReport{Status: EffectApplied, Receipt: "write-1"},
			GuardKey: guardKey, GuardDisplay: "docs:report.txt",
		}
		if err := putStoredJSON(tx, runtimeInvocationsBucket, invocationStorageKey(runID, writeBatch.BatchID, 0), writeInvocation); err != nil {
			return err
		}
		if err := putStoredJSON(tx, runtimeEffectGuardsBucket, guardKey, storedEffectGuard{
			OperationID: writeInvocation.OperationID, Status: EffectApplied,
		}); err != nil {
			return err
		}
		terminalInvocation := storedToolInvocation{
			Version: runtimeEncodingVersion, RunID: runID, BatchID: terminalBatch.BatchID,
			OperationID: runID + ":batch-terminal:0", Ordinal: 0, Call: invalidCall,
			State: ToolInvocationCompleted, EffectKind: ToolEffectLegacy,
			Result: TextResult(violations),
		}
		return putStoredJSON(tx, runtimeInvocationsBucket, invocationStorageKey(runID, terminalBatch.BatchID, 0), terminalInvocation)
	}); err != nil {
		t.Fatal(err)
	}
}

// TestRuntimeRestartRestoresCorrectionBudget pins that the persisted
// correction-turn count survives a worker stop: with the budget exhausted,
// another invalid payload fails the run instead of silently extending it.
func TestRuntimeRestartRestoresCorrectionBudget(t *testing.T) {
	f := newStructuredFixture()
	initializer, err := NewRuntime(context.Background(), RuntimeConfig{Store: noCloseStore{Store: f.store}})
	if err != nil {
		t.Fatal(err)
	}
	_ = initializer.Close()
	seedCorrectedRun(t, f, "restart-budget")

	turns := []Message{asstCalls(structuredCall("s2", `{"summary":7}`))}
	cfg := &StructuredOutputConfig{Schema: json.RawMessage(summarySchema), MaxCorrections: 1}
	agent := newStructuredAgent(t, &scriptedProvider{turns: turns}, []Tool{f.writeTool()}, cfg, 8)
	runtime := runtimeWithAgent(t, f, agent)
	if err := runtime.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	result, err := runtime.Handle("restart-budget").Await(context.Background())
	if err == nil || !errors.Is(err, ErrInvalidStructuredOutput) {
		t.Fatalf("await error = %v, want ErrInvalidStructuredOutput (budget restored, not reset)", err)
	}
	if result.Status != RunFailed || result.Turns != 3 {
		t.Fatalf("result = %#v", result)
	}
	if f.writes.Load() != 0 {
		t.Fatalf("external writes = %d, want 0 (the accepted write was never replayed)", f.writes.Load())
	}
}

// TestRuntimeRestartContinuesCorrectionWithinBudget pins the positive side of
// the same invariant: with budget remaining, a restarted run finishes inside
// the same lifecycle and the configured guard rejects the duplicate write the
// correction proposed.
func TestRuntimeRestartContinuesCorrectionWithinBudget(t *testing.T) {
	f := newStructuredFixture()
	initializer, err := NewRuntime(context.Background(), RuntimeConfig{Store: noCloseStore{Store: f.store}})
	if err != nil {
		t.Fatal(err)
	}
	_ = initializer.Close()
	seedCorrectedRun(t, f, "restart-continue")

	turns := []Message{
		asstCalls(toolUse("w2", "write", `{"path":"report.txt"}`)),
		asstCalls(structuredCall("s2", `{"summary":"done"}`)),
	}
	cfg := &StructuredOutputConfig{Schema: json.RawMessage(summarySchema), MaxCorrections: 2}
	agent := newStructuredAgent(t, &scriptedProvider{turns: turns}, []Tool{f.writeTool()}, cfg, 8)
	runtime := runtimeWithAgent(t, f, agent)
	if err := runtime.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	result, err := runtime.Handle("restart-continue").Await(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if string(result.StructuredOutput) != `{"summary":"done"}` || result.Status != RunCompleted {
		t.Fatalf("result = %#v", result)
	}
	// The duplicate write proposed during correction was guard-denied without
	// dispatching, and the original receipt is intact.
	if f.writes.Load() != 0 {
		t.Fatalf("external writes = %d, want 0", f.writes.Load())
	}
	snapshot, err := runtime.Handle("restart-continue").Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	receipt := ""
	denied := false
	for _, batch := range snapshot.ToolBatches {
		for _, invocation := range batch.Invocations {
			if invocation.Effect.Status == EffectApplied {
				receipt = invocation.Effect.Receipt
			}
			if strings.Contains(invocation.Result.Text(), "denied: semantic mutation already applied or unresolved") {
				denied = true
			}
		}
	}
	if receipt != "write-1" || !denied {
		t.Fatalf("receipt = %q, denied = %v; batches = %#v", receipt, denied, snapshot.ToolBatches)
	}
	if result.Turns != 4 {
		t.Fatalf("turns = %d, want 4 (2 committed + 2 after restart, cumulative)", result.Turns)
	}
}

// TestRuntimeNativeStructuredOutputFallsBackWithoutProviderSupport pins that
// a native declaration on an incapable provider still uses the hidden tool.
func TestRuntimeNativeStructuredOutputFallsBackWithoutProviderSupport(t *testing.T) {
	f := newStructuredFixture()
	turns := []Message{
		asstCalls(structuredCall("s1", `{"summary":"done"}`)),
	}
	cfg := &StructuredOutputConfig{Schema: json.RawMessage(summarySchema), Native: true, MaxCorrections: 1}
	agent := newStructuredAgent(t, &scriptedProvider{turns: turns}, nil, cfg, 8)
	runtime := runtimeWithAgent(t, f, agent)
	handle, err := runtime.Submit(context.Background(), "agent", "v1", "produce summary", SubmitOptions{})
	if err != nil {
		t.Fatal(err)
	}
	result, err := handle.Await(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if string(result.StructuredOutput) != `{"summary":"done"}` {
		t.Fatalf("result = %#v", result)
	}
}

// structuredNativeProvider implements StructuredOutputProvider and records the
// request the runtime sent, so tests can pin schema fidelity and that the
// hidden tool is not advertised.
type structuredNativeProvider struct {
	turns       []Message
	calls       int
	lastRequest Request
}

func (p *structuredNativeProvider) SupportsNativeStructuredOutput() bool { return true }

func (p *structuredNativeProvider) Invoke(_ context.Context, request Request) (Response, error) {
	if p.calls >= len(p.turns) {
		return Response{}, errors.New("no script for turn")
	}
	p.lastRequest = request
	p.calls++
	return fixtureResponse(p.turns[p.calls-1]), nil
}

// TestRuntimeNativeStructuredOutputKeepsSchemaFidelity pins that native mode
// sends the declared schema via CallOptions.OutputSchema, does not advertise
// the hidden tool, validates the response text, and preserves RawBlock
// provider content through the durable transcript.
func TestRuntimeNativeStructuredOutputKeepsSchemaFidelity(t *testing.T) {
	f := newStructuredFixture()
	rawThinking := json.RawMessage(`{"type":"thinking","thinking":"chain"}`)
	turns := []Message{
		AssistantMessage(RawBlock{Type: "thinking", Data: rawThinking}, TextBlock{Text: `{"summary":"native done"}`}),
	}
	cfg := &StructuredOutputConfig{Schema: json.RawMessage(summarySchema), Native: true, MaxCorrections: 1}
	provider := &structuredNativeProvider{turns: turns}
	agent := newStructuredAgent(t, provider, nil, cfg, 8)
	runtime := runtimeWithAgent(t, f, agent)
	handle, err := runtime.Submit(context.Background(), "agent", "v1", "produce summary", SubmitOptions{})
	if err != nil {
		t.Fatal(err)
	}
	result, err := handle.Await(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if string(result.StructuredOutput) != `{"summary":"native done"}` {
		t.Fatalf("result = %#v", result)
	}
	if string(provider.lastRequest.Options.OutputSchema) != summarySchema {
		t.Fatalf("native schema request = %q", provider.lastRequest.Options.OutputSchema)
	}
	for _, tool := range provider.lastRequest.Tools {
		if tool.Name == structuredOutputToolName {
			t.Fatal("native run advertised the hidden structured-output tool")
		}
	}
	snapshot, err := handle.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var sawRaw bool
	for _, message := range snapshot.Result.Messages {
		for _, block := range message.Blocks {
			if raw, ok := block.(RawBlock); ok && string(raw.Data) == string(rawThinking) {
				sawRaw = true
			}
		}
	}
	if !sawRaw {
		t.Fatal("provider-native RawBlock did not survive the durable transcript")
	}
}

// TestRuntimeNativeStructuredOutputCorrectsWithinOneLifecycle pins V10: an
// unusable native payload corrects inside the same run and caps, instead of a
// transparent replay.
func TestRuntimeNativeStructuredOutputCorrectsWithinOneLifecycle(t *testing.T) {
	f := newStructuredFixture()
	turns := []Message{
		asstText("partial attempt"),
		asstText(`{"summary":"corrected"}`),
	}
	cfg := &StructuredOutputConfig{Schema: json.RawMessage(summarySchema), Native: true, MaxCorrections: 1}
	agent := newStructuredAgent(t, &structuredNativeProvider{turns: turns}, nil, cfg, 8)
	runtime := runtimeWithAgent(t, f, agent)
	handle, err := runtime.Submit(context.Background(), "agent", "v1", "produce summary", SubmitOptions{})
	if err != nil {
		t.Fatal(err)
	}
	result, err := handle.Await(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if string(result.StructuredOutput) != `{"summary":"corrected"}` || result.Turns != 2 {
		t.Fatalf("result = %#v", result)
	}
	snapshot, err := handle.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.State != RuntimeTerminal || snapshot.Result.RunID != result.RunID {
		t.Fatalf("snapshot = %#v", snapshot)
	}
}

// TestRunTypedUsesDeclaredStructuredOutput pins that the typed facade adapts to
// an agent's pinned declared contract instead of installing a second hidden tool.
func TestRunTypedUsesDeclaredStructuredOutput(t *testing.T) {
	typedAgent, err := New(&scriptedProvider{turns: []Message{
		asstCalls(structuredCall("s1", `{"summary":"done"}`)),
	}}, AgentConfig{StructuredOutput: summaryContract()})
	if err != nil {
		t.Fatal(err)
	}
	got, result, err := RunTyped[struct {
		Summary string `json:"summary"`
	}](context.Background(), typedAgent, "work")
	if err != nil {
		t.Fatal(err)
	}
	if got.Summary != "done" || string(result.StructuredOutput) != `{"summary":"done"}` {
		t.Fatalf("typed declared result = %+v, %#v", got, result)
	}
}

// incompleteStructuredStreamProvider emits one partial JSON text block and closes
// with an incomplete stop reason. It proves partial stream text is evidence, not
// accepted structured output or a correctable payload.
type incompleteStructuredStreamProvider struct{ calls int }

func (p *incompleteStructuredStreamProvider) Invoke(context.Context, Request) (Response, error) {
	return Response{}, errors.New("non-streaming path should not be used")
}

func (p *incompleteStructuredStreamProvider) InvokeStream(context.Context, Request) (<-chan StreamChunk, error) {
	p.calls++
	ch := make(chan StreamChunk, 2)
	ch <- StreamChunk{Deltas: []BlockDelta{{Index: 0, Type: "text", Text: `{"summary":"partial`}}}
	ch <- StreamChunk{StopReason: StopIncomplete, RawStopReason: "stream_closed"}
	close(ch)
	return ch, nil
}

func TestDeclaredStructuredOutputRejectsIncompleteStreamPartial(t *testing.T) {
	provider := &incompleteStructuredStreamProvider{}
	agent := newStructuredAgent(t, provider, nil, summaryContract(), 8)
	result, err := agent.RunStream(context.Background(), "produce summary", nil)
	var completion *CompletionError
	if !errors.As(err, &completion) || completion.Reason != StopIncomplete {
		t.Fatalf("RunStream error = %v, want incomplete CompletionError", err)
	}
	if provider.calls != 1 {
		t.Fatalf("stream calls = %d, want 1 (no correction replay)", provider.calls)
	}
	if len(result.StructuredOutput) != 0 || result.Turns != 1 {
		t.Fatalf("result = %#v, want no accepted structured output from partial stream", result)
	}
	for _, msg := range result.Messages {
		if msg.Role == "user" && strings.Contains(msg.Text(), "previous structured output was invalid") {
			t.Fatalf("incomplete stream triggered a correction prompt: %#v", result.Messages)
		}
	}
}

func TestDeclaredStructuredOutputRunStreamAndSessionParity(t *testing.T) {
	streamAgent := newStructuredAgent(t, &scriptedProvider{turns: []Message{
		asstCalls(structuredCall("s1", `{"summary":5}`)),
		asstCalls(structuredCall("s2", `{"summary":"stream"}`)),
	}}, nil, summaryContract(), 8)
	streamResult, err := streamAgent.RunStream(context.Background(), "work", nil)
	if err != nil {
		t.Fatal(err)
	}
	if string(streamResult.StructuredOutput) != `{"summary":"stream"}` || streamResult.Turns != 2 {
		t.Fatalf("stream result = %#v", streamResult)
	}

	sessionAgent := newStructuredAgent(t, &scriptedProvider{turns: []Message{
		asstCalls(structuredCall("s1", `{"summary":5}`)),
		asstCalls(structuredCall("s2", `{"summary":"session"}`)),
	}}, nil, summaryContract(), 8)
	session := sessionAgent.NewSession()
	sessionResult, err := session.Run(context.Background(), "work")
	if err != nil {
		t.Fatal(err)
	}
	if string(sessionResult.StructuredOutput) != `{"summary":"session"}` || sessionResult.Turns != 2 {
		t.Fatalf("session result = %#v", sessionResult)
	}
	if len(session.Messages()) != len(sessionResult.Messages) {
		t.Fatalf("session transcript length = %d, result transcript length = %d", len(session.Messages()), len(sessionResult.Messages))
	}
}

func TestRuntimeStructuredDeadlineDuringCorrectionDoesNotResurrect(t *testing.T) {
	f := newStructuredFixture()
	deadline := time.Now().Add(10 * time.Millisecond)
	provider := &deadlineCorrectionProvider{
		first: asstCalls(structuredCall("s1", `{"summary":5}`)),
	}
	agent := newStructuredAgent(t, provider, nil, summaryContract(), 8)
	runtime, err := NewRuntime(context.Background(), RuntimeConfig{Store: noCloseStore{Store: f.store}})
	if err != nil {
		t.Fatal(err)
	}
	if err := runtime.Register("agent", "v1", agent); err != nil {
		t.Fatal(err)
	}
	handle, err := runtime.Submit(context.Background(), "agent", "v1", "work", SubmitOptions{Deadline: deadline})
	if err != nil {
		t.Fatal(err)
	}
	result, err := handle.Await(context.Background())
	if err == nil || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("await error = %v, want deadline", err)
	}
	if result.Status != RunCancelled || provider.calls.Load() != 2 {
		t.Fatalf("result = %#v, provider calls = %d", result, provider.calls.Load())
	}
	_ = runtime.Close()

	recovered, err := NewRuntime(context.Background(), RuntimeConfig{Store: f.store})
	if err != nil {
		t.Fatal(err)
	}
	defer recovered.Close()
	if err := recovered.Register("agent", "v1", newStructuredAgent(t, &scriptedProvider{turns: []Message{asstCalls(structuredCall("late", `{"summary":"late"}`))}}, nil, summaryContract(), 8)); err != nil {
		t.Fatal(err)
	}
	if err := recovered.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	snapshot, err := recovered.Handle(result.RunID).Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.State != RuntimeTerminal || snapshot.Result.Status != RunCancelled || len(snapshot.Result.StructuredOutput) != 0 {
		t.Fatalf("snapshot after recovery = %#v", snapshot)
	}
}

type deadlineCorrectionProvider struct {
	first Message
	calls atomic.Int64
}

func (p *deadlineCorrectionProvider) Invoke(ctx context.Context, _ Request) (Response, error) {
	call := p.calls.Add(1)
	if call == 1 {
		return fixtureResponse(p.first), nil
	}
	<-ctx.Done()
	return Response{}, ctx.Err()
}

func TestRuntimeNativeProviderAcceptedRestartFinalizesWithoutReplay(t *testing.T) {
	f := newStructuredFixture()
	initializer, err := NewRuntime(context.Background(), RuntimeConfig{Store: noCloseStore{Store: f.store}})
	if err != nil {
		t.Fatal(err)
	}
	_ = initializer.Close()

	messages := []Message{
		UserMessage("produce summary"),
		asstText("not json"),
		UserMessage(correctionPrompt(&InvalidStructuredOutputError{Cause: errors.New("model did not produce structured output")}, "")),
		asstText(`{"summary":"native corrected"}`),
	}
	record := storedRuntimeRun{
		Version: runtimeEncodingVersion, RunID: "native-provider-accepted", DefinitionID: "agent", DefinitionRevision: "v1",
		Task: "produce summary", State: RuntimeRunning, Generation: 4,
		Result:         RunResult{RunID: "native-provider-accepted", Turns: 2, Steps: 2, FinalMessage: messages[len(messages)-1]},
		Corrections:    1,
		LastTransition: "provider_accepted",
	}
	if err := f.store.Transaction(context.Background(), true, func(tx StoreTransaction) error {
		if err := appendTranscript(tx, record.RunID, &record, messages); err != nil {
			return err
		}
		return putRuntimeRun(tx, record)
	}); err != nil {
		t.Fatal(err)
	}

	provider := &structuredNativeProvider{turns: []Message{asstText("must not be invoked")}}
	cfg := &StructuredOutputConfig{Schema: json.RawMessage(summarySchema), Native: true, MaxCorrections: 1}
	runtime := runtimeWithAgent(t, f, newStructuredAgent(t, provider, nil, cfg, 8))
	if err := runtime.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	result, err := runtime.Handle("native-provider-accepted").Await(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if string(result.StructuredOutput) != `{"summary":"native corrected"}` || result.Status != RunCompleted {
		t.Fatalf("result = %#v", result)
	}
	if provider.calls != 0 {
		t.Fatalf("provider calls after recovery = %d, want 0", provider.calls)
	}
}

func TestDeclaredStructuredOutputMaxTurnsExhaustionKeepsInvalidCause(t *testing.T) {
	provider := &scriptedProvider{turns: []Message{asstCalls(structuredCall("s1", `{"summary":5}`))}}
	agent := newStructuredAgent(t, provider, nil, summaryContract(), 1)
	result, err := agent.Run(context.Background(), "work")
	if !errors.Is(err, ErrMaxStepsExceeded) || !errors.Is(err, ErrInvalidStructuredOutput) {
		t.Fatalf("err = %v, want max turns joined with invalid structured output", err)
	}
	if result.Status != RunLimitReached || provider.calls != 1 || len(result.StructuredOutput) != 0 {
		t.Fatalf("result = %#v, provider calls = %d", result, provider.calls)
	}
}

func TestRuntimeStructuredMaxTurnsSnapshotReopenKeepsInvalidCause(t *testing.T) {
	f := newStructuredFixture()
	provider := &scriptedProvider{turns: []Message{asstCalls(structuredCall("s1", `{"summary":5}`))}}
	agent := newStructuredAgent(t, provider, nil, summaryContract(), 1)
	runtime, err := NewRuntime(context.Background(), RuntimeConfig{Store: noCloseStore{Store: f.store}})
	if err != nil {
		t.Fatal(err)
	}
	if err := runtime.Register("agent", "v1", agent); err != nil {
		t.Fatal(err)
	}
	handle, err := runtime.Submit(context.Background(), "agent", "v1", "produce summary", SubmitOptions{})
	if err != nil {
		t.Fatal(err)
	}
	result, err := handle.Await(context.Background())
	if !errors.Is(err, ErrMaxStepsExceeded) || !errors.Is(err, ErrInvalidStructuredOutput) {
		t.Fatalf("await error = %v, want max turns joined with invalid structured output", err)
	}
	if result.Status != RunLimitReached || provider.calls != 1 || len(result.StructuredOutput) != 0 {
		t.Fatalf("result = %#v, provider calls = %d", result, provider.calls)
	}
	snapshot, err := handle.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.State != RuntimeTerminal || snapshot.ErrorKind != "max_steps_invalid_structured_output" {
		t.Fatalf("snapshot = %#v", snapshot)
	}
	runID := handle.ID()
	if err := runtime.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := NewRuntime(context.Background(), RuntimeConfig{Store: noCloseStore{Store: f.store}})
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	reopenedSnapshot, err := reopened.Handle(runID).Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if reopenedSnapshot.State != RuntimeTerminal || reopenedSnapshot.ErrorKind != "max_steps_invalid_structured_output" {
		t.Fatalf("reopened snapshot = %#v", reopenedSnapshot)
	}
	reopenedResult, err := reopened.Handle(runID).Await(context.Background())
	if !errors.Is(err, ErrMaxStepsExceeded) || !errors.Is(err, ErrInvalidStructuredOutput) {
		t.Fatalf("reopened await error = %v, want both sentinels", err)
	}
	if reopenedResult.Status != RunLimitReached || len(reopenedResult.StructuredOutput) != 0 {
		t.Fatalf("reopened result = %#v", reopenedResult)
	}
}

func TestDeclaredStructuredOutputCorrectionSiblingResultDoesNotClaimCompletion(t *testing.T) {
	f := newStructuredFixture()
	turns := []Message{
		asstCalls(structuredCall("s1", `{"summary":5}`), toolUse("w1", "write", `{"path":"report.txt"}`)),
		asstCalls(structuredCall("s2", `{"summary":"done"}`)),
	}
	agent := newStructuredAgent(t, &scriptedProvider{turns: turns}, []Tool{f.writeTool()}, summaryContract(), 8)
	result, err := agent.Run(context.Background(), "work")
	if err != nil {
		t.Fatal(err)
	}
	if f.writes.Load() != 0 {
		t.Fatalf("sibling write executed during terminal correction: %d", f.writes.Load())
	}
	var siblingText string
	for _, msg := range result.Messages {
		for _, block := range msg.Blocks {
			tr, ok := block.(ToolResultBlock)
			if ok && tr.ToolUseID == "w1" {
				siblingText = Message{Blocks: tr.Content}.Text()
			}
		}
	}
	if !strings.Contains(siblingText, "correction requested") || strings.Contains(siblingText, "run completed") {
		t.Fatalf("sibling correction result text = %q", siblingText)
	}
}

func TestNativeDeclaredSchemaCannotBeClearedOrReplacedByRunOptions(t *testing.T) {
	patches := []CallOptionsPatch{
		{OutputSchema: Setting[json.RawMessage]{Set: true}},
		{OutputSchema: Setting[json.RawMessage]{Set: true, Value: json.RawMessage(`{"type":"object","properties":{"other":{"type":"string"}}}`)}},
	}
	for _, patch := range patches {
		provider := &structuredNativeProvider{turns: []Message{asstText(`{"summary":"pinned"}`)}}
		cfg := &StructuredOutputConfig{Schema: json.RawMessage(summarySchema), Native: true, MaxCorrections: 1}
		agent := newStructuredAgent(t, provider, nil, cfg, 8)
		result, err := agent.Run(context.Background(), "work", WithCallOptions(patch))
		if err != nil {
			t.Fatal(err)
		}
		if string(result.StructuredOutput) != `{"summary":"pinned"}` {
			t.Fatalf("result = %#v", result)
		}
		if string(provider.lastRequest.Options.OutputSchema) != summarySchema {
			t.Fatalf("native schema = %q, want pinned %s", provider.lastRequest.Options.OutputSchema, summarySchema)
		}
	}
}

// TestDeclaredStructuredOutputRejectsUnsupportedSchema pins explicit
// declaration-time rejection of unsupported assertion keywords.
func TestDeclaredStructuredOutputRejectsUnsupportedSchema(t *testing.T) {
	_, err := New(&scriptedProvider{}, AgentConfig{
		StructuredOutput: &StructuredOutputConfig{Schema: json.RawMessage(`{"type":"object","properties":{"a":{}},"oneOf":[{"type":"string"}]}`)},
	})
	if err == nil || !strings.Contains(err.Error(), "unsupported schema keyword") {
		t.Fatalf("declared unsupported schema = %v, want explicit rejection", err)
	}
	if _, err := New(&scriptedProvider{}, AgentConfig{StructuredOutput: &StructuredOutputConfig{}}); err == nil {
		t.Fatal("empty structured output declaration accepted")
	}
	if _, err := New(&scriptedProvider{}, AgentConfig{StructuredOutput: &StructuredOutputConfig{
		Schema: json.RawMessage(summarySchema), MaxCorrections: -1,
	}}); err == nil {
		t.Fatal("negative correction budget accepted")
	}
}

// TestDirectDeclaredStructuredRunValidatesAndCorrects pins that direct
// process-local runs enforce the same declared contract through the same loop.
func TestDirectDeclaredStructuredRunValidatesAndCorrects(t *testing.T) {
	f := newStructuredFixture()
	turns := []Message{
		asstCalls(structuredCall("s1", `{"summary":5}`)),
		asstCalls(structuredCall("s2", `{"summary":"ok"}`)),
	}
	agent := newStructuredAgent(t, &scriptedProvider{turns: turns}, nil, summaryContract(), 8)
	result, err := agent.Run(context.Background(), "work")
	if err != nil {
		t.Fatal(err)
	}
	if string(result.StructuredOutput) != `{"summary":"ok"}` || result.Turns != 2 {
		t.Fatalf("result = %#v", result)
	}
	if f.writes.Load() != 0 {
		t.Fatalf("writes = %d", f.writes.Load())
	}
}
