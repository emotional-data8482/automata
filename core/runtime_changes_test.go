package core

import (
	"context"
	"errors"
	"testing"
	"time"
)

func readAllChanges(t *testing.T, runtime *Runtime, after uint64, opts ...ChangeOption) []RunChange {
	t.Helper()
	var changes []RunChange
	for {
		page, err := runtime.Changes(context.Background(), after, 3, opts...)
		if err != nil {
			t.Fatal(err)
		}
		changes = append(changes, page.Changes...)
		if page.Next == page.Head {
			return changes
		}
		after = page.Next
	}
}

// The change feed records every committed run change once, in commit order,
// with contiguous global sequences whose event ranges tile each run's own
// committed event log.
func TestRuntimeChangesCoverEveryRunEvent(t *testing.T) {
	runtime := newTestRuntime(t)
	ref, err := runtime.Register("agent", "v1", testAgent(&scriptedProvider{turns: []Message{asstText("one"), asstText("two")}}))
	if err != nil {
		t.Fatal(err)
	}
	var runIDs []string
	for _, task := range []string{"a", "b"} {
		result, err := runtime.Run(context.Background(), ref, task)
		if err != nil {
			t.Fatal(err)
		}
		runIDs = append(runIDs, result.RunID)
	}
	changes := readAllChanges(t, runtime, 0, WithChangeEvents())
	next := map[string]uint64{}
	for i, change := range changes {
		if change.Sequence != uint64(i+1) {
			t.Fatalf("change %d has sequence %d", i, change.Sequence)
		}
		if change.FirstEvent != next[change.RunID]+1 || change.LastEvent < change.FirstEvent {
			t.Fatalf("change %d covers events %d..%d of %s after %d", i, change.FirstEvent, change.LastEvent, change.RunID, next[change.RunID])
		}
		if uint64(len(change.Events)) != change.LastEvent-change.FirstEvent+1 || change.EventsPruned {
			t.Fatalf("change %d carries %d events", i, len(change.Events))
		}
		next[change.RunID] = change.LastEvent
	}
	for _, runID := range runIDs {
		page, err := runtime.Handle(runID).Events(context.Background(), 0, 0)
		if err != nil {
			t.Fatal(err)
		}
		if next[runID] != page.Head {
			t.Fatalf("feed covers %d of run %s's %d events", next[runID], runID, page.Head)
		}
	}
	last := changes[len(changes)-1]
	if last.RunID != runIDs[1] || last.State != RuntimeTerminal {
		t.Fatalf("last change = %+v", last)
	}
	var sawMessages bool
	for _, change := range changes {
		for _, event := range change.Events {
			if event.Kind == CommittedMessages && len(event.Messages) == event.MessageCount && event.MessageCount > 0 {
				sawMessages = true
			}
		}
	}
	if !sawMessages {
		t.Fatal("WithChangeEvents attached no transcript messages")
	}
	if plain := readAllChanges(t, runtime, 0); len(plain) != len(changes) || plain[0].Events != nil {
		t.Fatalf("changes without payloads = %d, first events %v", len(plain), plain[0].Events)
	}
	if _, err := runtime.Changes(context.Background(), last.Sequence+1, 0); !errors.Is(err, ErrEventGap) {
		t.Fatalf("cursor ahead of head = %v, want ErrEventGap", err)
	}
}

func TestRuntimeWaitChangesWakesOnCommit(t *testing.T) {
	runtime := newTestRuntime(t)
	ref, err := runtime.Register("agent", "v1", testAgent(&scriptedProvider{turns: []Message{asstText("one")}}))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := runtime.WaitChanges(ctx, 0, 0); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("WaitChanges on an empty feed = %v", err)
	}
	got := make(chan ChangePage, 1)
	go func() {
		page, err := runtime.WaitChanges(context.Background(), 0, 0)
		if err != nil {
			t.Error(err)
		}
		got <- page
	}()
	time.Sleep(10 * time.Millisecond)
	if _, err := runtime.Submit(context.Background(), ref, "go"); err != nil {
		t.Fatal(err)
	}
	select {
	case page := <-got:
		if len(page.Changes) == 0 || page.Changes[0].State != RuntimeReady {
			t.Fatalf("first change = %+v", page.Changes)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("WaitChanges did not wake on admission")
	}

	closing, err := NewEphemeralRuntime()
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := closing.WaitChanges(context.Background(), 0, 0)
		done <- err
	}()
	time.Sleep(10 * time.Millisecond)
	_ = closing.Close()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("WaitChanges on a closed runtime returned no error")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("WaitChanges did not wake on Close")
	}
}

// Retention holding on the change cursor never removes a run's events,
// history, or record before the consumer synced its last change, and the
// feed itself is pruned only through the cursor.
func TestRuntimePruneHoldsUnsyncedRuns(t *testing.T) {
	runtime := newTestRuntime(t)
	ref, err := runtime.Register("agent", "v1", testAgent(&scriptedProvider{turns: []Message{asstText("one")}}))
	if err != nil {
		t.Fatal(err)
	}
	result, err := runtime.Run(context.Background(), ref, "go")
	if err != nil {
		t.Fatal(err)
	}
	changes := readAllChanges(t, runtime, 0)
	last := changes[len(changes)-1].Sequence
	time.Sleep(2 * time.Millisecond)

	held := RetentionPolicy{Events: time.Nanosecond, History: time.Nanosecond, Runs: time.Nanosecond, Changes: time.Nanosecond, HoldUnsynced: true, SyncedThrough: last - 1}
	report, err := runtime.Prune(context.Background(), held)
	if err != nil {
		t.Fatal(err)
	}
	if report.Events != 0 || report.History != 0 || report.Runs != 0 || report.Changes != int(last-1) {
		t.Fatalf("held prune = %+v", report)
	}
	snapshot, err := runtime.Handle(result.RunID).Snapshot(context.Background())
	if err != nil || snapshot.HistoryPruned {
		t.Fatalf("held run snapshot = %v pruned %v", err, snapshot.HistoryPruned)
	}
	if _, err := runtime.Changes(context.Background(), 0, 0); !errors.Is(err, ErrEventGap) {
		t.Fatalf("cursor below the pruned floor = %v, want ErrEventGap", err)
	}
	page, err := runtime.Changes(context.Background(), last-1, 0)
	if err != nil || len(page.Changes) != 1 {
		t.Fatalf("retained changes = %v, %v", page.Changes, err)
	}

	held.SyncedThrough = last
	report, err = runtime.Prune(context.Background(), held)
	if err != nil {
		t.Fatal(err)
	}
	if report.Events != 1 || report.History != 1 || report.Runs != 1 {
		t.Fatalf("synced prune = %+v", report)
	}
	if _, err := runtime.Handle(result.RunID).Snapshot(context.Background()); !errors.Is(err, ErrRunPruned) {
		t.Fatalf("pruned run snapshot = %v", err)
	}
	page, err = runtime.Changes(context.Background(), last, 0, WithChangeEvents())
	if err != nil {
		t.Fatal(err)
	}
	if n := len(page.Changes); n == 0 || !page.Changes[n-1].Pruned || page.Changes[n-1].RunID != result.RunID {
		t.Fatalf("feed after pruning = %+v", page.Changes)
	}
	for _, change := range page.Changes {
		if !change.Pruned && !change.EventsPruned {
			t.Fatalf("change %+v carries events of a pruned run", change)
		}
	}
}
