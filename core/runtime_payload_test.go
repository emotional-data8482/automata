package core

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
)

// alterTranscriptChunk rewrites the messages of one stored chunk in place,
// keeping its recorded digest, as bit rot or a bad restore would.
func alterTranscriptChunk(t *testing.T, store Store, runID string, index int, from, to string) {
	t.Helper()
	if err := store.Transaction(context.Background(), true, func(tx StoreTransaction) error {
		key := transcriptFactKey(runID, index)
		raw, err := tx.Get(runtimeFactsBucket, key)
		if err != nil {
			return err
		}
		var chunk storedTranscriptChunk
		if err := json.Unmarshal(raw, &chunk); err != nil {
			return err
		}
		chunk.Messages = json.RawMessage(strings.Replace(string(chunk.Messages), from, to, 1))
		data, err := json.Marshal(chunk)
		if err != nil {
			return err
		}
		return tx.Put(runtimeFactsBucket, key, data)
	}); err != nil {
		t.Fatal(err)
	}
}

// Altered or missing transcript facts are reported as unavailable payloads
// by every read that would otherwise return them, never as different history.
func TestRuntimeTranscriptIntegrityFailuresAreReported(t *testing.T) {
	base := NewMemoryStore().(*memoryStore)
	runtime, err := NewRuntime(context.Background(), RuntimeConfig{Store: noCloseStore{Store: base}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = runtime.Close() })
	if _, err := runtime.Register("agent", "v1", testAgent(&scriptedProvider{turns: []Message{asstText("the answer")}})); err != nil {
		t.Fatal(err)
	}
	result, err := runtime.Run(context.Background(), DefinitionRef{ID: "agent", Revision: "v1"}, "question")
	if err != nil {
		t.Fatal(err)
	}
	handle := runtime.Handle(result.RunID)
	alterTranscriptChunk(t, base, result.RunID, 1, "the answer", "a forged answer")
	if _, err := handle.Snapshot(context.Background()); !errors.Is(err, ErrPayloadUnavailable) {
		t.Fatalf("snapshot of altered transcript = %v, want ErrPayloadUnavailable", err)
	}
	if _, err := handle.Events(context.Background(), 0, 100); !errors.Is(err, ErrPayloadUnavailable) {
		t.Fatalf("events over altered transcript = %v, want ErrPayloadUnavailable", err)
	}
	if err := base.Transaction(context.Background(), true, func(tx StoreTransaction) error {
		return tx.Delete(runtimeFactsBucket, transcriptFactKey(result.RunID, 1))
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := handle.Snapshot(context.Background()); !errors.Is(err, ErrPayloadUnavailable) {
		t.Fatalf("snapshot of missing transcript chunk = %v, want ErrPayloadUnavailable", err)
	}
}

// Recovery reports a run whose committed history is unavailable as needing
// attention, with the reason, and keeps recovering the other runs: one bad
// payload never blocks the whole pass or becomes a guessed continuation.
func TestRuntimeRecoveryReportsUnavailablePayloadAndContinues(t *testing.T) {
	base := NewMemoryStore().(*memoryStore)
	initializer, err := NewRuntime(context.Background(), RuntimeConfig{Store: noCloseStore{Store: base}})
	if err != nil {
		t.Fatal(err)
	}
	_ = initializer.Close()
	seedInterruptedRun(t, base, "damaged", "provider_accepted", []Message{UserMessage("go"), asstText("final")})
	seedInterruptedRun(t, base, "healthy", "provider_accepted", []Message{UserMessage("go"), asstText("final")})
	alterTranscriptChunk(t, base, "damaged", 0, "final", "forged")

	runtime, err := NewRuntime(context.Background(), RuntimeConfig{Store: noCloseStore{Store: base}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = runtime.Close() })
	agent := testAgent(&repeatingChildProvider{})
	agent.RegisterTool(Func("extra", "ordinary work", func(context.Context, struct{}) (string, error) { return "ok", nil }))
	if _, err := runtime.Register("agent", "v1", agent); err != nil {
		t.Fatal(err)
	}
	if err := runtime.Recover(context.Background()); err != nil {
		t.Fatalf("recovery aborted on one damaged run: %v", err)
	}
	if result, err := runtime.Handle("healthy").Await(context.Background()); err != nil || result.Output != "final" {
		t.Fatalf("healthy run = %#v, %v", result, err)
	}
	record, err := runtimeRecord(runtime, "damaged")
	if err != nil {
		t.Fatal(err)
	}
	if record.State != RuntimeNeedsAttention || !strings.Contains(record.AttentionReason, ErrPayloadUnavailable.Error()) {
		t.Fatalf("damaged run = %s %q, want payload attention", record.State, record.AttentionReason)
	}
}

// A provider turn too large to store needs attention; nothing is truncated,
// and the committed transcript stays exactly what committed before it.
func TestRuntimeOversizedProviderTurnNeedsAttentionWithoutTruncation(t *testing.T) {
	base := NewMemoryStore().(*memoryStore)
	runtime, err := NewRuntime(context.Background(), RuntimeConfig{Store: noCloseStore{Store: base}, MaxPayloadBytes: 4096})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = runtime.Close() })
	provider := &repeatingTextProvider{text: strings.Repeat("x", 10_000)}
	if _, err := runtime.Register("agent", "v1", testAgent(provider)); err != nil {
		t.Fatal(err)
	}
	result, err := runtime.Run(context.Background(), DefinitionRef{ID: "agent", Revision: "v1"}, "write a lot")
	if !errors.Is(err, ErrRunNeedsAttention) || !strings.Contains(err.Error(), ErrPayloadTooLarge.Error()) {
		t.Fatalf("oversized turn = %v, want payload attention", err)
	}
	if len(result.Messages) != 1 || result.Messages[0].Text() != "write a lot" || provider.calls.Load() != 1 {
		t.Fatalf("committed transcript = %#v after %d calls, want only the task", result.Messages, provider.calls.Load())
	}
	// The attempt happened and is accounted for even though its turn could
	// not be stored.
	if result.ProviderAttempts != 1 || result.Turns != 1 || result.FinalMessage.Text() != "" {
		t.Fatalf("accounting = attempts %d turns %d final %q", result.ProviderAttempts, result.Turns, result.FinalMessage.Text())
	}
	for _, event := range allEvents(t, runtime.Handle(result.RunID)) {
		for _, message := range event.Messages {
			if strings.Contains(message.Text(), "xxx") {
				t.Fatal("a truncated or oversized assistant turn was committed")
			}
		}
	}
	if _, err := NewRuntime(context.Background(), RuntimeConfig{Store: NewMemoryStore(), MaxPayloadBytes: -1}); err == nil {
		t.Fatal("negative MaxPayloadBytes accepted")
	}
}

type repeatingTextProvider struct {
	text  string
	calls atomic.Int32
}

func (p *repeatingTextProvider) Invoke(context.Context, Request) (Response, error) {
	p.calls.Add(1)
	return fixtureResponse(asstText(p.text)), nil
}

// A tool result too large to store never becomes truncated history: the
// invocation keeps its effect report and awaits an authoritative resolution,
// the run needs attention, and Reconcile continues it without running the
// tool again.
func TestRuntimeOversizedToolResultAwaitsReconciliation(t *testing.T) {
	var executions atomic.Int32
	write := FuncResult("write", "write", func(context.Context, struct{}) (ToolResult, error) {
		executions.Add(1)
		result := TextResult(strings.Repeat("y", 10_000))
		result.Effect = EffectReport{Status: EffectApplied, Receipt: "receipt-1"}
		return result, nil
	})
	write = WithToolEffectPolicy(write, ToolEffectPolicy{Kind: ToolEffectMutating})
	agent, err := New(&scriptedProvider{turns: []Message{asstTool("w1", "write", `{}`), asstText("done")}}, AgentConfig{Tools: []Tool{write}})
	if err != nil {
		t.Fatal(err)
	}
	runtime, err := NewRuntime(context.Background(), RuntimeConfig{Store: NewMemoryStore(), MaxPayloadBytes: 4096})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = runtime.Close() })
	if _, err := runtime.Register("agent", "v1", agent); err != nil {
		t.Fatal(err)
	}
	result, err := runtime.Run(context.Background(), DefinitionRef{ID: "agent", Revision: "v1"}, "go")
	if !errors.Is(err, ErrRunNeedsAttention) || !strings.Contains(err.Error(), ErrPayloadTooLarge.Error()) {
		t.Fatalf("oversized tool result = %v, want payload attention", err)
	}
	handle := runtime.Handle(result.RunID)
	snapshot, err := handle.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	invocation := snapshot.ToolBatches[0].Invocations[0]
	if invocation.State != ToolInvocationUncertain || invocation.Effect.Receipt != "receipt-1" ||
		len(invocation.Result.Blocks) != 0 || !strings.Contains(invocation.Error, ErrPayloadTooLarge.Error()) {
		t.Fatalf("oversized invocation = %#v", invocation)
	}
	if err := handle.Reconcile(context.Background(), invocation.OperationID, EffectResolution{
		Result: TextResult("wrote 10000 bytes (summarized)"),
		Effect: EffectReport{Status: EffectApplied, Receipt: "receipt-1"},
	}); err != nil {
		t.Fatal(err)
	}
	final, err := handle.Await(context.Background())
	if err != nil || final.Output != "done" || executions.Load() != 1 {
		t.Fatalf("reconciled run = %#v, %v, executions %d", final, err, executions.Load())
	}
}

// Invocation results carry a digest that survives an encode/decode round
// trip for rich blocks and rejects an altered result.
func TestStoredToolInvocationResultDigest(t *testing.T) {
	invocation := storedToolInvocation{
		Version: runtimeEncodingVersion, RunID: "run", OperationID: "op", State: ToolInvocationCompleted,
		Result: ToolResult{Blocks: Blocks{
			TextBlock{Text: "<tag> &   unicode"},
			ImageBlock{MediaType: "image/png", Data: []byte{0x89, 0x50, 0x4e, 0x47, 0x00, 0xff}},
			RawBlock{Provider: "fake", Type: "citation", Data: json.RawMessage(`{ "n" : 1.50, "k":[1,2] }`)},
		}},
		Effect: EffectReport{Status: EffectApplied, Receipt: "r"},
	}
	data, err := json.Marshal(invocation)
	if err != nil {
		t.Fatal(err)
	}
	var decoded storedToolInvocation
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("round trip rejected: %v", err)
	}
	if decoded.ResultDigest == "" {
		t.Fatal("no result digest was stamped")
	}
	again, err := json.Marshal(decoded)
	if err != nil || string(again) != string(data) {
		t.Fatalf("re-encoding changed the record:\n%s\n%s", data, again)
	}
	altered := strings.Replace(string(data), "unicode", "forged", 1)
	if err := json.Unmarshal([]byte(altered), &decoded); !errors.Is(err, ErrPayloadUnavailable) {
		t.Fatalf("altered result = %v, want ErrPayloadUnavailable", err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		t.Fatal(err)
	}
	delete(fields, "result_digest")
	stripped, _ := json.Marshal(fields)
	if err := json.Unmarshal(stripped, &decoded); !errors.Is(err, ErrPayloadUnavailable) {
		t.Fatalf("result without its digest = %v, want ErrPayloadUnavailable", err)
	}

	// A block type this version does not know still verifies: the digest
	// covers the stored bytes, not a re-encoding of what was decoded.
	future := []byte(`{"blocks":[{"type":"future_block","payload":{"x":1}}]}`)
	sum := sha256.Sum256(future)
	fields["result"] = future
	fields["result_digest"], _ = json.Marshal(hex.EncodeToString(sum[:]))
	forward, _ := json.Marshal(fields)
	if err := json.Unmarshal(forward, &decoded); err != nil {
		t.Fatalf("unknown block type rejected: %v", err)
	}
}
