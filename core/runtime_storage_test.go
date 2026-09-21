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

// Golden fixtures lock the version 5 record encoding and the admission digest
// rule. Changing either changes every persisted record and requires a new
// encoding version, not a silent rewrite.
func TestRuntimeRecordEncodingIsStable(t *testing.T) {
	record := storedRuntimeRun{
		Version: 5, RunID: "run-1", DefinitionID: "agent", DefinitionRevision: "v1",
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
	want := `{"version":5,"run_id":"run-1","definition_id":"agent","definition_revision":"v1",` +
		`"task":"work","deadline":"2026-01-02T03:04:05Z","state":"running","generation":7,` +
		`"result":{"RunID":"run-1","Status":"completed","Turns":2,"ProviderAttempts":0,` +
		`"ProviderStopReason":"","RawProviderStopReason":"","Diagnostics":null,"Output":"done",` +
		`"FinalMessage":{"role":""},"Messages":null,"Usage":{"InputTokens":0,"OutputTokens":0,` +
		`"CacheCreationTokens":0,"CacheReadTokens":0},"Steps":0,"StopReason":"","RawStopReason":""},` +
		`"transcript_chunks":3,"transcript_messages":9,"last_transition":"batch_committed",` +
		`"hook_results":[{"name":"audit"}],"tool_budget":{}}`
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
	bumped := strings.Replace(want, `"version":5`, `"version":99`, 1)
	if _, err := decodeRuntimeRun([]byte(bumped)); err == nil ||
		!strings.Contains(err.Error(), "unsupported runtime run version 99") {
		t.Fatalf("unsupported version = %v", err)
	}
}

func TestAdmissionDigestRuleIsStable(t *testing.T) {
	got := admissionDigest(admissionPayload{DefinitionID: "agent", Revision: "v1", Task: "work"})
	// sha256 of {"definition_id":"agent","revision":"v1","task":"work","deadline":"0001-01-01T00:00:00Z"}:
	// a zero Deadline still marshals, keeping the rule field-complete.
	const want = "a9c041113609e2440d326b47a882fff370750a4c5355195a18ed92cc43ab2ccb"
	if got != want {
		t.Fatalf("digest rule drifted = %s, want %s", got, want)
	}
	if got == admissionDigest(admissionPayload{DefinitionID: "agent", Revision: "v1", Task: "changed"}) {
		t.Fatal("different payloads produced the same digest")
	}
	// The same deadline instant in a different location resolves the same
	// admission identity: Go marshals time.Time in the value's own zone, so the
	// rule normalizes to UTC before encoding.
	zoned := time.Date(2026, 1, 2, 5, 4, 5, 0, time.FixedZone("+02", 2*60*60))
	utcForm := admissionPayload{DefinitionID: "agent", Revision: "v1", Task: "work", Deadline: zoned}
	utcForm.Deadline = utcForm.Deadline.UTC()
	if admissionDigest(admissionPayload{DefinitionID: "agent", Revision: "v1", Task: "work", Deadline: zoned}) != admissionDigest(utcForm) {
		t.Fatal("same instant in different locations produced different admission digests")
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

// --- receipts and lost acknowledgements --------------------------------------

func TestRuntimeLostAdmissionAcknowledgementResolvesOnRetry(t *testing.T) {
	base := &memoryStore{buckets: make(map[string]map[string][]byte)}
	store := &unknownAdmissionStore{Store: base, err: errors.New("admission commit outcome unknown")}
	provider := &countingRuntimeProvider{}
	runtime, err := NewRuntime(context.Background(), RuntimeConfig{Store: store})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = runtime.Close() })
	if err := runtime.Register("agent", "v1", testAgent(provider)); err != nil {
		t.Fatal(err)
	}
	options := SubmitOptions{Scope: "tenant", Key: "lost-ack"}

	store.armed.Store(true)
	if _, err := runtime.Submit(context.Background(), "agent", "v1", "work", options); !errors.Is(err, store.err) {
		t.Fatalf("lost acknowledgement = %v", err)
	}
	runID := onlyStoredRunID(t, base)

	// A later cancel command commits before the lost acknowledgement resolves.
	if err := runtime.Handle(runID).Cancel(context.Background()); err != nil {
		t.Fatal(err)
	}
	retried, err := runtime.Submit(context.Background(), "agent", "v1", "work", options)
	if err != nil || retried.ID() != runID {
		t.Fatalf("resolved admission = %q, %v; want run %q", retried.ID(), err, runID)
	}
	result, err := retried.Await(context.Background())
	if !errors.Is(err, context.Canceled) || result.Status != RunCancelled || result.RunID != runID {
		t.Fatalf("resolved receipt = %#v, %v; want the cancelled run without dispatch", result, err)
	}
	if got := provider.calls.Load(); got != 0 {
		t.Fatalf("provider dispatched against stale pre-cancel state %d times", got)
	}

	// The same flow without cancel B resumes the run exactly once.
	base2 := &memoryStore{buckets: make(map[string]map[string][]byte)}
	store2 := &unknownAdmissionStore{Store: base2, err: errors.New("admission commit outcome unknown")}
	runtime2, err := NewRuntime(context.Background(), RuntimeConfig{Store: store2})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = runtime2.Close() })
	if err := runtime2.Register("agent", "v1", testAgent(provider)); err != nil {
		t.Fatal(err)
	}
	store2.armed.Store(true)
	if _, err := runtime2.Submit(context.Background(), "agent", "v1", "work", options); err == nil {
		t.Fatal("expected unknown admission outcome")
	}
	resumed, err := runtime2.Submit(context.Background(), "agent", "v1", "work", options)
	if err != nil {
		t.Fatal(err)
	}
	awaitCtx, awaitCancel := context.WithTimeout(context.Background(), time.Second)
	defer awaitCancel()
	result, err = resumed.Await(awaitCtx)
	if err != nil || result.Output != "done" {
		t.Fatalf("resumed run = %#v, %v", result, err)
	}
	if got := provider.calls.Load(); got != 1 {
		t.Fatalf("provider calls = %d, want 1", got)
	}
}

func TestRuntimeCancelReceiptSurvivesLaterCommits(t *testing.T) {
	base := &memoryStore{buckets: make(map[string]map[string][]byte)}
	store := &unknownAdmissionStore{Store: base, err: errors.New("admission commit outcome unknown")}
	runtime, err := NewRuntime(context.Background(), RuntimeConfig{Store: store})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = runtime.Close() })
	if err := runtime.Register("agent", "v1", testAgent(&countingRuntimeProvider{})); err != nil {
		t.Fatal(err)
	}
	store.armed.Store(true)
	if _, err := runtime.Submit(context.Background(), "agent", "v1", "work", SubmitOptions{Scope: "tenant", Key: "cancel-receipt"}); err == nil {
		t.Fatal("expected unknown admission outcome")
	}
	runID := onlyStoredRunID(t, base)
	handle := runtime.Handle(runID)
	if err := handle.Cancel(context.Background()); err != nil {
		t.Fatal(err)
	}

	// Corrupt the record state to prove the retry resolves from the receipt,
	// not from re-deriving current state: a receipt-blind retry would rewrite
	// the running record to terminal-cancelled again.
	if err := base.Transaction(context.Background(), true, func(tx StoreTransaction) error {
		record, err := getRuntimeRun(tx, runID)
		if err != nil {
			return err
		}
		record.State = RuntimeRunning
		return putRuntimeRun(tx, record)
	}); err != nil {
		t.Fatal(err)
	}
	if err := handle.Cancel(context.Background()); err != nil {
		t.Fatalf("cancel retry did not resolve its receipt: %v", err)
	}
	record := getRecord(t, base, runID)
	if record.State != RuntimeRunning || record.Result.Status != RunCancelled || record.Generation != 2 {
		t.Fatalf("retry re-derived state instead of resolving its receipt: %#v", record)
	}
}

// --- fault injection at every transaction boundary ---------------------------

// Writable transactions for a plain run: initialize, admission, claim,
// provider acceptance, response classification, finishExecution, finishHooks. Injecting a failure at
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
		{failAt: 5, recordState: RuntimeNeedsAttention, recoveredState: RuntimeNeedsAttention},
		// A classified final response is a safe continuation boundary in v3.
		{failAt: 6, recordState: RuntimeRunning, recoveredState: RuntimeTerminal},
		{failAt: 7, recordState: RuntimeFinalizing, recoveredState: RuntimeNeedsAttention},
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

// --- paged recovery ----------------------------------------------------------

type countingScanPageStore struct {
	Store
	scans atomic.Int32
}

func (s *countingScanPageStore) Transaction(ctx context.Context, writable bool, fn func(StoreTransaction) error) error {
	return s.Store.Transaction(ctx, writable, func(tx StoreTransaction) error {
		return fn(&countingScanPageTransaction{StoreTransaction: tx, store: s})
	})
}

type countingScanPageTransaction struct {
	StoreTransaction
	store *countingScanPageStore
}

func (tx *countingScanPageTransaction) ScanPage(bucket, prefix, after string, limit int, visit func(string, []byte) error) (string, error) {
	tx.store.scans.Add(1)
	return tx.StoreTransaction.ScanPage(bucket, prefix, after, limit, visit)
}

func TestRuntimeRecoverPagesThroughRuns(t *testing.T) {
	base := &memoryStore{buckets: make(map[string]map[string][]byte)}
	store := &countingScanPageStore{Store: base}
	runtime, err := NewRuntime(context.Background(), RuntimeConfig{Store: store})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = runtime.Close() })
	for i := range runtimeRecoverPageSize*3 + 5 {
		record := storedRuntimeRun{
			Version: runtimeEncodingVersion, RunID: fmt.Sprintf("%032x", i), DefinitionID: "agent",
			DefinitionRevision: "v1", Task: "work", State: RuntimeTerminal, Generation: 1,
			Result: RunResult{RunID: fmt.Sprintf("%032x", i), Status: RunCompleted, Output: "done"},
		}
		if err := base.Transaction(context.Background(), true, func(tx StoreTransaction) error {
			return putRuntimeRun(tx, record)
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := runtime.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := store.scans.Load(); got < 4 {
		t.Fatalf("recovery used %d scan pages, want at least 4", got)
	}
}
