package core

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestWithLabelsValidation(t *testing.T) {
	runtime := newTestRuntime(t)
	ref, err := runtime.Register("agent", "v1", testAgent(&scriptedProvider{turns: []Message{asstText("ok")}}))
	if err != nil {
		t.Fatal(err)
	}
	tooMany := map[string]string{}
	for i := range maxRunLabels + 1 {
		tooMany[fmt.Sprint(i)] = "v"
	}
	for name, labels := range map[string]map[string]string{
		"too many":    tooMany,
		"empty key":   {"": "v"},
		"long key":    {strings.Repeat("k", maxRunLabelKeyBytes+1): "v"},
		"long value":  {"k": strings.Repeat("v", maxRunLabelValueBytes+1)},
		"NUL in key":  {"a\x00b": "v"},
		"invalid utf": {"k": "\xff"},
	} {
		if _, err := runtime.Submit(context.Background(), ref, "go", WithLabels(labels)); err == nil {
			t.Errorf("%s: admission accepted invalid labels", name)
		}
	}
}

// Labels persist with the run, stay outside admission identity, and are
// inherited by durable child runs.
func TestRuntimeLabelsPersistOutsideAdmissionIdentity(t *testing.T) {
	runtime := newTestRuntime(t)
	parent := testAgent(&scriptedProvider{turns: []Message{
		asstTool("c1", "delegate", `{"topic":"tea"}`),
		asstText("parent done"),
	}})
	newSharedChildDefinition(parent)
	for id, agent := range map[string]*Agent{"child": questionChild(), "parent": parent} {
		if _, err := runtime.Register(id, "v1", agent); err != nil {
			t.Fatal(err)
		}
	}
	ref := DefinitionRef{ID: "parent", Revision: "v1"}
	labels := map[string]string{"schedule": "nightly", "actor": "ops"}
	handle, err := runtime.Submit(context.Background(), ref, "go", WithIdempotencyKey("tenant", "k1"), WithLabels(labels))
	if err != nil {
		t.Fatal(err)
	}
	labels["actor"] = "mutated after submit"
	retry, err := runtime.Submit(context.Background(), ref, "go", WithIdempotencyKey("tenant", "k1"), WithLabels(map[string]string{"actor": "other"}))
	if err != nil {
		t.Fatalf("labels changed admission identity: %v", err)
	}
	if retry.ID() != handle.ID() {
		t.Fatalf("retry admitted %s, want %s", retry.ID(), handle.ID())
	}
	snapshot := waitForRunState(t, runtime, handle.ID(), RuntimeWaiting)
	if snapshot.Labels["actor"] != "ops" || snapshot.Labels["schedule"] != "nightly" || snapshot.CreatedAt.IsZero() || snapshot.UpdatedAt.Before(snapshot.CreatedAt) {
		t.Fatalf("snapshot labels %v created %v updated %v", snapshot.Labels, snapshot.CreatedAt, snapshot.UpdatedAt)
	}
	childID := snapshot.ToolBatches[0].Invocations[0].ChildRunID
	child, err := runtime.Handle(childID).Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if child.Labels["schedule"] != "nightly" {
		t.Fatalf("child labels = %v", child.Labels)
	}
}

func TestRuntimeRunsFiltersAndPages(t *testing.T) {
	runtime := newTestRuntime(t)
	done, err := runtime.Register("done", "v1", testAgent(&scriptedProvider{turns: []Message{asstText("ok"), asstText("ok"), asstText("ok")}}))
	if err != nil {
		t.Fatal(err)
	}
	parent := testAgent(&scriptedProvider{turns: []Message{
		asstTool("c1", "delegate", `{"topic":"tea"}`),
		asstText("parent done"),
	}})
	newSharedChildDefinition(parent)
	if _, err := runtime.Register("child", "v1", questionChild()); err != nil {
		t.Fatal(err)
	}
	waiting, err := runtime.Register("parent", "v1", parent)
	if err != nil {
		t.Fatal(err)
	}
	var doneIDs []string
	for i := range 3 {
		result, err := runtime.Run(context.Background(), done, "go", WithLabels(map[string]string{"n": fmt.Sprint(i)}), WithIdempotencyKey("tenant-a", fmt.Sprint(i)))
		if err != nil {
			t.Fatal(err)
		}
		doneIDs = append(doneIDs, result.RunID)
	}
	mid := time.Now()
	handle, err := runtime.Submit(context.Background(), waiting, "go", WithLabels(map[string]string{"n": "w"}))
	if err != nil {
		t.Fatal(err)
	}
	snapshot := waitForRunState(t, runtime, handle.ID(), RuntimeWaiting)
	childID := snapshot.ToolBatches[0].Invocations[0].ChildRunID
	waitForRunState(t, runtime, childID, RuntimeWaiting)

	list := func(filter RunFilter) []string {
		t.Helper()
		page, err := runtime.Runs(context.Background(), filter)
		if err != nil {
			t.Fatal(err)
		}
		var ids []string
		for _, run := range page.Runs {
			ids = append(ids, run.RunID)
		}
		return ids
	}
	same := func(name string, got, want []string) {
		t.Helper()
		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Fatalf("%s = %v, want %v", name, got, want)
		}
	}
	same("all", list(RunFilter{}), append(append([]string(nil), doneIDs...), handle.ID(), childID))
	same("definition", list(RunFilter{Definition: DefinitionRef{ID: "done"}}), doneIDs)
	same("revision mismatch", list(RunFilter{Definition: DefinitionRef{ID: "done", Revision: "v2"}}), nil)
	same("scope", list(RunFilter{Scope: "tenant-a"}), doneIDs)
	same("label", list(RunFilter{Labels: map[string]string{"n": "1"}}), doneIDs[1:2])
	same("inherited label", list(RunFilter{Labels: map[string]string{"n": "w"}}), []string{handle.ID(), childID})
	same("roots", list(RunFilter{RootsOnly: true, States: []RuntimeState{RuntimeWaiting}}), []string{handle.ID()})
	same("terminal", list(RunFilter{States: []RuntimeState{RuntimeTerminal}}), doneIDs)
	same("created after", list(RunFilter{CreatedAfter: mid}), []string{handle.ID(), childID})
	same("created before", list(RunFilter{CreatedBefore: mid}), doneIDs)

	// Page one at a time through the whole listing.
	var paged []string
	filter := RunFilter{Limit: 1}
	for range 10 {
		page, err := runtime.Runs(context.Background(), filter)
		if err != nil {
			t.Fatal(err)
		}
		for _, run := range page.Runs {
			paged = append(paged, run.RunID)
		}
		if !page.More {
			break
		}
		filter.After = page.Next
	}
	same("paged", paged, list(RunFilter{}))

	page, err := runtime.Runs(context.Background(), RunFilter{Scope: "tenant-a", Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	run := page.Runs[0]
	if run.Key != "0" || run.Status != RunCompleted || run.Labels["n"] != "0" || run.CreatedAt.IsZero() || !page.More {
		t.Fatalf("summary = %+v more %v", run, page.More)
	}

	// An unregistered definition's ready run lists as needing attention.
	runtime.mu.Lock()
	delete(runtime.bindings, bindingKey(done.ID, done.Revision))
	runtime.mu.Unlock()
	if err := runtime.transaction(context.Background(), true, func(tx StoreTransaction) error {
		record, err := getRuntimeRun(tx, doneIDs[0])
		if err != nil {
			return err
		}
		record.State = RuntimeReady
		return putRuntimeRun(tx, record)
	}); err != nil {
		t.Fatal(err)
	}
	same("attention", list(RunFilter{AttentionKinds: []AttentionKind{AttentionExecution}}), doneIDs[:1])
}

// A store written before the run list index existed is indexed once when a
// Runtime opens it, and pruned runs leave the index.
func TestRuntimeRunsBackfillsAndPrunesIndex(t *testing.T) {
	store := NewMemoryStore()
	runtime, err := NewRuntime(context.Background(), RuntimeConfig{Store: noCloseStore{Store: store}})
	if err != nil {
		t.Fatal(err)
	}
	ref, err := runtime.Register("agent", "v1", testAgent(&scriptedProvider{turns: []Message{asstText("ok"), asstText("ok")}}))
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for range 2 {
		result, err := runtime.Run(context.Background(), ref, "go")
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, result.RunID)
	}
	if err := runtime.Close(); err != nil {
		t.Fatal(err)
	}
	if err := store.Transaction(context.Background(), true, func(tx StoreTransaction) error {
		if err := deletePrefix(tx, runtimeRunListBucket, ""); err != nil {
			return err
		}
		return tx.Delete(runtimeMetaBucket, runListIndexedMeta)
	}); err != nil {
		t.Fatal(err)
	}
	reopened, err := NewRuntime(context.Background(), RuntimeConfig{Store: noCloseStore{Store: store}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	page, err := reopened.Runs(context.Background(), RunFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Runs) != 2 {
		t.Fatalf("backfilled listing has %d runs", len(page.Runs))
	}
	time.Sleep(2 * time.Millisecond)
	if _, err := reopened.Prune(context.Background(), RetentionPolicy{Runs: time.Nanosecond}); err != nil {
		t.Fatal(err)
	}
	page, err = reopened.Runs(context.Background(), RunFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Runs) != 0 {
		t.Fatalf("pruned runs still listed: %+v", page.Runs)
	}
}
