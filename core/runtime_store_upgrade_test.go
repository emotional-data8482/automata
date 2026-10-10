package core

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

// downgradeToVersion9 makes store look like a version-9 store written by a
// release without the run list index or change feed.
func downgradeToVersion9(t *testing.T, store Store) {
	t.Helper()
	if err := store.Transaction(context.Background(), true, func(tx StoreTransaction) error {
		for _, bucket := range []string{runtimeRunListBucket, runtimeChangesBucket, runtimeRunChangesBucket} {
			if err := deletePrefix(tx, bucket, ""); err != nil {
				return err
			}
		}
		for _, key := range []string{changeHeadMeta, runListClockMeta} {
			if err := tx.Delete(runtimeMetaBucket, key); err != nil {
				return err
			}
		}
		return tx.Put(runtimeMetaBucket, "encoding", []byte("9"))
	}); err != nil {
		t.Fatal(err)
	}
}

// Opening a version-9 store indexes its runs, records a change for each, and
// raises the store version so older releases cannot write past the indexes.
// Runs stored before the change feed are then held by HoldUnsynced
// retention until a consumer syncs them (bug review C6, D1).
func TestRuntimeUpgradesVersion9Store(t *testing.T) {
	store := NewMemoryStore()
	runtime, err := NewRuntime(context.Background(), RuntimeConfig{Store: noCloseStore{Store: store}})
	if err != nil {
		t.Fatal(err)
	}
	ref, err := runtime.Register("agent", "v1", testAgent(&scriptedProvider{turns: []Message{asstText("ok")}}))
	if err != nil {
		t.Fatal(err)
	}
	result, err := runtime.Run(context.Background(), ref, "go")
	if err != nil {
		t.Fatal(err)
	}
	_ = runtime.Close()
	downgradeToVersion9(t, store)

	reopened, err := NewRuntime(context.Background(), RuntimeConfig{Store: noCloseStore{Store: store}})
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	var version int
	if err := store.Transaction(context.Background(), false, func(tx StoreTransaction) error {
		raw, err := tx.Get(runtimeMetaBucket, "encoding")
		if err != nil {
			return err
		}
		return json.Unmarshal(raw, &version)
	}); err != nil || version != runtimeStoreVersion {
		t.Fatalf("store version = %d, %v; want %d", version, err, runtimeStoreVersion)
	}
	page, err := reopened.Runs(context.Background(), RunFilter{})
	if err != nil || len(page.Runs) != 1 || page.Runs[0].RunID != result.RunID {
		t.Fatalf("upgraded listing = %#v, %v", page, err)
	}
	changes, err := reopened.Changes(context.Background(), 0, 0)
	if err != nil || len(changes.Changes) != 1 || changes.Changes[0].RunID != result.RunID || changes.Changes[0].LastEvent == 0 {
		t.Fatalf("upgraded change feed = %#v, %v", changes, err)
	}

	time.Sleep(2 * time.Millisecond)
	report, err := reopened.Prune(context.Background(), RetentionPolicy{Runs: time.Nanosecond, HoldUnsynced: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := reopened.Handle(result.RunID).Snapshot(context.Background()); err != nil {
		t.Fatalf("unsynced pre-feed run was pruned: %v (report %+v)", err, report)
	}
	if _, err := reopened.Prune(context.Background(), RetentionPolicy{Runs: time.Nanosecond, HoldUnsynced: true, SyncedThrough: changes.Head}); err != nil {
		t.Fatal(err)
	}
	if page, _ := reopened.Runs(context.Background(), RunFilter{}); len(page.Runs) != 0 {
		t.Fatalf("synced run was not pruned: %#v", page.Runs)
	}
}
