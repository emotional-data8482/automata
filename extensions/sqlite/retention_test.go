package sqlite

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/emotional-data8482/automata/core"
)

// Retention deletes through the SQLite adapter, and a pruned run's tombstone
// survives reopening: an exact admission retry still resolves to
// ErrRunPruned rather than admitting new work.
func TestPrunedRunTombstoneSurvivesReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "runtime.sqlite")
	ctx := context.Background()
	open := func() *core.Runtime {
		t.Helper()
		store, err := Open(ctx, path)
		if err != nil {
			t.Fatal(err)
		}
		runtime, err := core.NewRuntime(ctx, core.RuntimeConfig{Store: store})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := runtime.Register("agent", "v1", staticAgent(t)); err != nil {
			t.Fatal(err)
		}
		return runtime
	}
	options := core.WithIdempotencyKey("tenant", "job")
	first := open()
	result, err := first.Run(ctx, core.DefinitionRef{ID: "agent", Revision: "v1"}, "work", options)
	if err != nil {
		t.Fatal(err)
	}
	report, err := first.Prune(ctx, core.RetentionPolicy{Events: time.Nanosecond, History: time.Nanosecond, Runs: time.Nanosecond})
	if err != nil || report.Runs != 1 {
		t.Fatalf("prune = %#v, %v", report, err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	reopened := open()
	t.Cleanup(func() { _ = reopened.Close() })
	if _, err := reopened.Handle(result.RunID).Snapshot(ctx); !errors.Is(err, core.ErrRunPruned) {
		t.Fatalf("snapshot after reopen = %v, want ErrRunPruned", err)
	}
	if _, err := reopened.Submit(ctx, core.DefinitionRef{ID: "agent", Revision: "v1"}, "work", options); !errors.Is(err, core.ErrRunPruned) {
		t.Fatalf("admission retry after reopen = %v, want ErrRunPruned", err)
	}
}
