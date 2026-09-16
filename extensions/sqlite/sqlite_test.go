package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/emotional-data8482/automata/core"
)

// The schema stamp is a fixed literal because SQLite cannot bind PRAGMA
// arguments; this guard fails when the literal drifts from schemaVersion.
func TestUserVersionPragmaMatchesSchemaVersion(t *testing.T) {
	if userVersionPragma != fmt.Sprintf("PRAGMA user_version=%d", schemaVersion) {
		t.Fatalf("user_version stamp %q does not match schema version %d", userVersionPragma, schemaVersion)
	}
}

func TestPersistentRuntimeReopensTerminalRun(t *testing.T) {
	path := filepath.Join(t.TempDir(), "runtime.sqlite")
	store, err := Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	runtime, err := core.NewRuntime(context.Background(), core.RuntimeConfig{Store: store})
	if err != nil {
		t.Fatal(err)
	}
	agent, err := core.New(staticProvider{}, core.AgentConfig{})
	if err != nil {
		t.Fatal(err)
	}
	if err := runtime.Register("agent", "v1", agent); err != nil {
		t.Fatal(err)
	}
	result, err := runtime.Run(context.Background(), "agent", "v1", "work", core.SubmitOptions{Scope: "test", Key: "one"})
	if err != nil {
		t.Fatal(err)
	}
	if err := runtime.Close(); err != nil {
		t.Fatal(err)
	}

	reopenedStore, err := Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	reopened, err := core.NewRuntime(context.Background(), core.RuntimeConfig{Store: reopenedStore})
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	snapshot, err := reopened.Handle(result.RunID).Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.State != core.RuntimeTerminal || snapshot.Result.Output != "persisted" || snapshot.RunID != result.RunID {
		t.Fatalf("reopened snapshot = %#v", snapshot)
	}
	if err := reopened.Register("agent", "v1", agent); err != nil {
		t.Fatal(err)
	}
	retried, err := reopened.Submit(context.Background(), "agent", "v1", "work", core.SubmitOptions{Scope: "test", Key: "one"})
	if err != nil || retried.ID() != result.RunID {
		t.Fatalf("reopened admission = %q, %v; want %q", retried.ID(), err, result.RunID)
	}
}

func TestUnsupportedSchemaIsRejectedWithoutRewrite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "runtime.sqlite")
	store, err := Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("PRAGMA journal_mode=DELETE"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("PRAGMA user_version=99"); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()
	if _, err := Open(context.Background(), path); err == nil {
		t.Fatal("unsupported schema opened")
	}
	db, _ = sql.Open("sqlite", path)
	defer db.Close()
	var version int
	var journal string
	if err := db.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow("PRAGMA journal_mode").Scan(&journal); err != nil {
		t.Fatal(err)
	}
	if version != 99 || journal != "delete" {
		t.Fatalf("unsupported open rewrote database: version=%d journal=%q", version, journal)
	}
}

func TestExclusiveLocalOwner(t *testing.T) {
	path := filepath.Join(t.TempDir(), "runtime.sqlite")
	first, err := Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	if _, err := Open(context.Background(), path); !errors.Is(err, ErrOwned) {
		t.Fatalf("second owner = %v", err)
	}
}

type staticProvider struct{}

func (staticProvider) Invoke(context.Context, core.Request) (core.Response, error) {
	return core.Response{
		Message:    core.AssistantMessage(core.TextBlock{Text: "persisted"}),
		StopReason: core.StopEndTurn,
	}, nil
}
