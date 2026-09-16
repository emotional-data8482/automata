package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// --- encoding fixtures -------------------------------------------------------

// Golden fixtures lock the version 2 record encoding and the admission digest
// rule. Changing either changes every persisted record and requires a new
// encoding version, not a silent rewrite.
func TestRuntimeRecordEncodingIsStable(t *testing.T) {
	record := storedRuntimeRun{
		Version: 2, RunID: "run-1", DefinitionID: "agent", DefinitionRevision: "v1",
		Task: "work", Deadline: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
		State: RuntimeRunning, Generation: 7,
		Result:             RunResult{RunID: "run-1", Status: RunCompleted, Output: "done", Turns: 2},
		TranscriptChunks:   3,
		TranscriptMessages: 9,
		LastTransition:     "batch_committed",
		HookResults:        []RunHookResult{{Name: "audit"}},
	}
	data, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	// RunResult persists with Go field names (it has no JSON tags); this
	// fixture locks that encoding until T03's version decision is revisited.
	want := `{"version":2,"run_id":"run-1","definition_id":"agent","definition_revision":"v1",` +
		`"task":"work","deadline":"2026-01-02T03:04:05Z","state":"running","generation":7,` +
		`"result":{"RunID":"run-1","Status":"completed","Turns":2,"ProviderAttempts":0,` +
		`"ProviderStopReason":"","RawProviderStopReason":"","Diagnostics":null,"Output":"done",` +
		`"FinalMessage":{"role":""},"Messages":null,"Usage":{"InputTokens":0,"OutputTokens":0,` +
		`"CacheCreationTokens":0,"CacheReadTokens":0},"Steps":0,"StopReason":"","RawStopReason":""},` +
		`"transcript_chunks":3,"transcript_messages":9,"last_transition":"batch_committed",` +
		`"hook_results":[{"name":"audit"}]}`
	if string(data) != want {
		t.Fatalf("record encoding drifted:\n got %s\nwant %s", data, want)
	}
	var decoded storedRuntimeRun
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(record, decoded) {
		t.Fatalf("round trip = %#v, want %#v", decoded, record)
	}

	// A stored fixture decodes; an unsupported version is rejected, not migrated.
	var legacy storedRuntimeRun
	if err := json.Unmarshal([]byte(want), &legacy); err != nil {
		t.Fatal(err)
	}
	if legacy.TranscriptChunks != 3 {
		t.Fatalf("fixture decode = %#v", legacy)
	}
	bumped := strings.Replace(want, `"version":2`, `"version":99`, 1)
	if _, err := decodeRuntimeRun([]byte(bumped)); err == nil ||
		!strings.Contains(err.Error(), "unsupported runtime run version 99") {
		t.Fatalf("unsupported version = %v", err)
	}
}

// --- transcript facts --------------------------------------------------------

func TestRuntimeTranscriptIsStoredAsAppendOnlyFacts(t *testing.T) {
	runtime := newTestRuntime(t)
	agent := testAgent(&scriptedProvider{turns: []Message{
		asstTool("c1", "echo", `{}`),
		asstTool("c2", "echo", `{}`),
		asstText("done"),
	}})
	agent.RegisterTool(Func("echo", "echo", func(_ context.Context, input struct {
		Text string `json:"text"`
	}) (string, error) {
		return "pong", nil
	}))
	if err := runtime.Register("agent", "v1", agent); err != nil {
		t.Fatal(err)
	}
	result, err := runtime.Run(context.Background(), "agent", "v1", "work", SubmitOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Messages) != 6 {
		t.Fatalf("messages = %d, want task + 3 turns + 2 results", len(result.Messages))
	}

	store := runtime.store
	var record storedRuntimeRun
	if err := store.Transaction(context.Background(), false, func(tx StoreTransaction) error {
		record, err = loadRuntimeRun(tx, result.RunID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	var raw storedRuntimeRun
	if err := store.Transaction(context.Background(), false, func(tx StoreTransaction) error {
		rawBytes, err := tx.Get(runtimeRunsBucket, result.RunID)
		if err != nil {
			return err
		}
		return json.Unmarshal(rawBytes, &raw)
	}); err != nil {
		t.Fatal(err)
	}
	if len(raw.Result.Messages) != 0 {
		t.Fatal("run record persisted an inline transcript")
	}
	if raw.TranscriptChunks != 5 || raw.TranscriptMessages != 6 {
		t.Fatalf("transcript counts = %d chunks, %d messages; want 5 chunks over 6 messages", raw.TranscriptChunks, raw.TranscriptMessages)
	}
	if !reflect.DeepEqual(record.Result.Messages, result.Messages) {
		t.Fatal("reassembled transcript differs from the returned result")
	}
}

// countingBytesStore measures bytes written through Put to compare write
// amplification across run lengths.
type countingBytesStore struct {
	Store
	writes atomic.Int64
}

func (s *countingBytesStore) Transaction(ctx context.Context, writable bool, fn func(StoreTransaction) error) error {
	return s.Store.Transaction(ctx, writable, func(tx StoreTransaction) error {
		if !writable {
			return fn(tx)
		}
		return fn(&countingBytesTransaction{StoreTransaction: tx, store: s})
	})
}

type countingBytesTransaction struct {
	StoreTransaction
	store *countingBytesStore
}

func (tx *countingBytesTransaction) Put(bucket, key string, value []byte) error {
	tx.store.writes.Add(int64(len(bucket) + len(key) + len(value)))
	return tx.StoreTransaction.Put(bucket, key, value)
}

func TestRuntimeCompactChangesDoNotRewriteUnboundedHistories(t *testing.T) {
	newAgent := func(turns int) *Agent {
		script := make([]Message, 0, turns)
		for i := 0; i < turns-1; i++ {
			script = append(script, asstTool(fmt.Sprintf("c%d", i), "echo", `{}`))
		}
		script = append(script, asstText("done"))
		agent, err := New(&scriptedProvider{turns: script}, AgentConfig{MaxTurns: turns + 1})
		if err != nil {
			t.Fatal(err)
		}
		agent.RegisterTool(Func("echo", "echo", func(context.Context, struct{}) (string, error) {
			return strings.Repeat("x", 2048), nil
		}))
		return agent
	}
	measure := func(turns int) int64 {
		store := &countingBytesStore{Store: NewMemoryStore()}
		runtime, err := NewRuntime(context.Background(), RuntimeConfig{Store: store})
		if err != nil {
			t.Fatal(err)
		}
		defer runtime.Close()
		if err := runtime.Register("agent", "v1", newAgent(turns)); err != nil {
			t.Fatal(err)
		}
		if _, err := runtime.Run(context.Background(), "agent", "v1", "work", SubmitOptions{}); err != nil {
			t.Fatal(err)
		}
		return store.writes.Load()
	}
	const short, long = 3, 6
	small, large := measure(short), measure(long)
	if large == 0 || small == 0 {
		t.Fatalf("no writes measured: %d, %d", small, large)
	}
	// Linear growth in history length roughly doubles the bytes between the
	// two lengths; rewriting the full transcript per transition would quadruple
	// them.
	if float64(large) > 3*float64(small) {
		t.Fatalf("write amplification: %d bytes for %d turns vs %d bytes for %d turns", large, long, small, short)
	}
}

// --- fault injection at every transaction boundary ---------------------------

// Writable transactions for a plain run: initialize, admission, claim,
// provider transition, finishExecution, finishHooks. Injecting a failure at
// each boundary must preserve the run identity and produce a resolvable or
// explicitly classified outcome.
func TestRuntimeTransactionFaultsPreserveEvidenceAndRecover(t *testing.T) {
	type scenario struct {
		failAt         int32
		submitFails    bool
		recordState    RuntimeState
		recoveredState RuntimeState
	}
	for _, scenario := range []scenario{
		{failAt: 2, submitFails: true}, // admission commit outcome unknown
		{failAt: 3, recordState: RuntimeReady, recoveredState: RuntimeTerminal},
		{failAt: 4, recordState: RuntimeNeedsAttention, recoveredState: RuntimeNeedsAttention},
		{failAt: 5, recordState: RuntimeRunning, recoveredState: RuntimeNeedsAttention},
		{failAt: 6, recordState: RuntimeFinalizing, recoveredState: RuntimeNeedsAttention},
	} {
		t.Run(fmt.Sprintf("faultAt%d", scenario.failAt), func(t *testing.T) {
			base := &memoryStore{buckets: make(map[string]map[string][]byte)}
			store := &failWritableTransactionStore{Store: noCloseStore{Store: base}, failAt: scenario.failAt}
			runtime, err := NewRuntime(context.Background(), RuntimeConfig{Store: store})
			if err != nil {
				t.Fatal(err)
			}
			if err := runtime.Register("agent", "v1", testAgent(&countingRuntimeProvider{})); err != nil {
				t.Fatal(err)
			}
			var handle *RunHandle
			handle, err = runtime.Submit(context.Background(), "agent", "v1", "work", SubmitOptions{})
			if scenario.submitFails {
				if err == nil {
					t.Fatal("injected admission failure was invisible")
				}
				if _, getErr := getRuntimeRunFrom(base, handleID(handle)); getErr == nil {
					t.Fatal("failed admission left a committed run")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			awaitCtx, awaitCancel := context.WithTimeout(context.Background(), time.Second)
			defer awaitCancel()
			result, awaitErr := handle.Await(awaitCtx)
			if awaitErr == nil {
				t.Fatalf("fault at %d was invisible: %#v", scenario.failAt, result)
			}
			if result.RunID != handle.ID() {
				t.Fatalf("fault at %d lost run identity: %#v", scenario.failAt, result)
			}
			record := getRecord(t, base, handle.ID())
			if record.State != scenario.recordState {
				t.Fatalf("fault at %d produced state %q, want %q", scenario.failAt, record.State, scenario.recordState)
			}
			_ = runtime.Close()

			// A recovery pass must classify every interrupted outcome without
			// inventing an execution result. Ready work starts asynchronously, so
			// the terminal case waits for the resumed run to finish.
			recovered, err := NewRuntime(context.Background(), RuntimeConfig{Store: noCloseStore{Store: base}})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = recovered.Close() })
			if err := recovered.Register("agent", "v1", testAgent(&countingRuntimeProvider{})); err != nil {
				t.Fatal(err)
			}
			if err := recovered.Recover(context.Background()); err != nil {
				t.Fatal(err)
			}
			if scenario.recoveredState == RuntimeTerminal {
				awaitCtx, awaitCancel := context.WithTimeout(context.Background(), time.Second)
				defer awaitCancel()
				if result, err := recovered.Handle(handle.ID()).Await(awaitCtx); err != nil || result.Output != "done" {
					t.Fatalf("recovered ready run = %#v, %v", result, err)
				}
			}
			after := getRecord(t, base, handle.ID())
			if after.State != scenario.recoveredState {
				t.Fatalf("recovery classified fault at %d as %q, want %q", scenario.failAt, after.State, scenario.recoveredState)
			}
		})
	}
}

func getRuntimeRunFrom(store Store, runID string) (storedRuntimeRun, error) {
	var record storedRuntimeRun
	err := store.Transaction(context.Background(), false, func(tx StoreTransaction) error {
		var err error
		record, err = loadRuntimeRun(tx, runID)
		return err
	})
	return record, err
}

func getRecord(t *testing.T, store Store, runID string) *storedRuntimeRun {
	t.Helper()
	record, err := getRuntimeRunFrom(store, runID)
	if err != nil {
		t.Fatal(err)
	}
	return &record
}

func handleID(h *RunHandle) string {
	if h == nil {
		return ""
	}
	return h.ID()
}

// --- classification and preservation -----------------------------------------

func TestRuntimeDistinguishesStorageFailureFromMissingRun(t *testing.T) {
	runtime := newTestRuntime(t)
	if _, err := runtime.Handle("missing").Snapshot(context.Background()); !errors.Is(err, ErrRunNotFound) {
		t.Fatalf("missing run = %v", err)
	}
	failing := &failingRuntimeStore{}
	broken := &Runtime{store: failing}
	if _, err := (&RunHandle{runtime: broken, runID: "any"}).Snapshot(context.Background()); err == nil ||
		errors.Is(err, ErrRunNotFound) || errors.Is(err, ErrRunNeedsAttention) {
		t.Fatalf("storage failure classified as %v", err)
	}
}

func TestRuntimePreservesUnsupportedRecordsWithoutRewrite(t *testing.T) {
	base := &memoryStore{buckets: make(map[string]map[string][]byte)}
	runtime, err := NewRuntime(context.Background(), RuntimeConfig{Store: base})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = runtime.Close() })
	if err := runtime.Register("agent", "v1", testAgent(&countingRuntimeProvider{})); err != nil {
		t.Fatal(err)
	}
	future := storedRuntimeRun{
		Version: runtimeEncodingVersion + 1, RunID: "future", DefinitionID: "agent",
		DefinitionRevision: "v1", Task: "work", State: RuntimeReady, Generation: 1,
		Result: RunResult{RunID: "future"},
	}
	var rawFuture []byte
	if err := base.Transaction(context.Background(), true, func(tx StoreTransaction) error {
		record := future
		if err := appendTranscript(tx, future.RunID, &record, nil); err != nil {
			return err
		}
		data, err := json.Marshal(record)
		if err != nil {
			return err
		}
		rawFuture = data
		return tx.Put(runtimeRunsBucket, future.RunID, data)
	}); err != nil {
		t.Fatal(err)
	}
	if err := runtime.Recover(context.Background()); err == nil {
		t.Fatal("recovery accepted an unsupported record version")
	}
	if err := base.Transaction(context.Background(), false, func(tx StoreTransaction) error {
		raw, err := tx.Get(runtimeRunsBucket, future.RunID)
		if err != nil {
			return err
		}
		if string(raw) != string(rawFuture) {
			t.Fatal("unsupported record was rewritten during recovery")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestRuntimeUnsupportedStorageVersionIsRejectedWithoutRewrite(t *testing.T) {
	base := &memoryStore{buckets: make(map[string]map[string][]byte)}
	store := noCloseStore{Store: base}
	if err := base.Transaction(context.Background(), true, func(tx StoreTransaction) error {
		data, _ := json.Marshal(99)
		return tx.Put(runtimeMetaBucket, "encoding", data)
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := NewRuntime(context.Background(), RuntimeConfig{Store: store}); err == nil {
		t.Fatal("unsupported storage version opened")
	}
	if err := base.Transaction(context.Background(), false, func(tx StoreTransaction) error {
		raw, err := tx.Get(runtimeMetaBucket, "encoding")
		if err != nil || string(raw) != "99" {
			t.Fatalf("unsupported version record rewritten: %q, %v", raw, err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
