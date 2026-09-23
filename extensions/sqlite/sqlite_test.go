package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

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
	if _, err := runtime.Register("agent", "v1", agent); err != nil {
		t.Fatal(err)
	}
	result, err := runtime.Run(context.Background(), core.DefinitionRef{ID: "agent", Revision: "v1"}, "work", core.WithIdempotencyKey("test", "one"))
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
	// The transcript reassembles from append-only fact chunks across reopen.
	if len(snapshot.Result.Messages) != 2 || snapshot.Result.Messages[1].Text() != "persisted" {
		t.Fatalf("reopened transcript = %#v", snapshot.Result.Messages)
	}
	if _, err := reopened.Register("agent", "v1", agent); err != nil {
		t.Fatal(err)
	}
	retried, err := reopened.Submit(context.Background(), core.DefinitionRef{ID: "agent", Revision: "v1"}, "work", core.WithIdempotencyKey("test", "one"))
	if err != nil || retried.ID() != result.RunID {
		t.Fatalf("reopened admission = %q, %v; want %q", retried.ID(), err, result.RunID)
	}
}

func TestDurableApprovalReopensAndLostResponseIsIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "approval.sqlite")
	var writes atomic.Int64
	tool := core.FuncResult("write", "write", func(context.Context, struct {
		Path string `json:"path"`
	}) (core.ToolResult, error) {
		writes.Add(1)
		result := core.TextResult("written")
		result.Effect = core.EffectReport{Status: core.EffectApplied, Receipt: "write-1"}
		return result, nil
	})
	tool = core.WithToolEffectPolicy(tool, core.ToolEffectPolicy{Kind: core.ToolEffectMutating})
	tool = core.WithDurableWait(tool, core.DurableWaitPolicy{
		Kind: core.WaitApproval,
		Target: func(raw json.RawMessage) (string, error) {
			var input struct {
				Path string `json:"path"`
			}
			if err := json.Unmarshal(raw, &input); err != nil {
				return "", err
			}
			return input.Path, nil
		},
	})
	agent, err := core.New(approvalProvider{}, core.AgentConfig{Tools: []core.Tool{tool}})
	if err != nil {
		t.Fatal(err)
	}
	authorizer := core.ApprovalAuthorizerFunc(func(context.Context, core.ApprovalAuthorization) error { return nil })
	store, err := Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	runtime, err := core.NewRuntime(context.Background(), core.RuntimeConfig{Store: store, Authorizer: authorizer})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := runtime.Register("writer", "v1", agent); err != nil {
		t.Fatal(err)
	}
	handle, err := runtime.Submit(context.Background(), core.DefinitionRef{ID: "writer", Revision: "v1"}, "write")
	if err != nil {
		t.Fatal(err)
	}
	var wait core.WaitSnapshot
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		snapshot, snapshotErr := handle.Snapshot(context.Background())
		if snapshotErr != nil {
			t.Fatal(snapshotErr)
		}
		if snapshot.State == core.RuntimeWaiting && len(snapshot.Waits) == 1 {
			wait = snapshot.Waits[0]
			break
		}
		time.Sleep(time.Millisecond)
	}
	if wait.ID == "" {
		t.Fatal("run did not persist approval wait")
	}
	if err := runtime.Close(); err != nil {
		t.Fatal(err)
	}

	store, err = Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	reopened, err := core.NewRuntime(context.Background(), core.RuntimeConfig{Store: store, Authorizer: authorizer})
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if _, err := reopened.Register("writer", "v1", agent); err != nil {
		t.Fatal(err)
	}
	if err := reopened.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	handle = reopened.Handle(handle.ID())
	resolution := core.WaitResolution{Decision: core.Allow, Actor: "host-user", ActionDigest: wait.ActionDigest}
	if err := handle.ResolveWait(context.Background(), wait.ID, resolution); err != nil {
		t.Fatal(err)
	}
	result, err := handle.Await(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if result.Output != "complete" || writes.Load() != 1 {
		t.Fatalf("result = %#v, writes = %d", result, writes.Load())
	}
	if err := handle.ResolveWait(context.Background(), wait.ID, resolution); err != nil {
		t.Fatalf("lost-response retry: %v", err)
	}
	if writes.Load() != 1 {
		t.Fatalf("duplicate resolution repeated write: %d", writes.Load())
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

type approvalProvider struct{}

func (approvalProvider) Invoke(_ context.Context, request core.Request) (core.Response, error) {
	for _, message := range request.Messages {
		if message.Role == "tool" {
			return core.Response{Message: core.AssistantMessage(core.TextBlock{Text: "complete"}), StopReason: core.StopEndTurn}, nil
		}
	}
	return core.Response{Message: core.AssistantMessage(core.ToolUseBlock{ID: "write-1", Name: "write", Input: json.RawMessage(`{"path":"report.txt"}`)}), StopReason: core.StopToolUse}, nil
}

type staticProvider struct{}

func (staticProvider) Invoke(context.Context, core.Request) (core.Response, error) {
	return core.Response{
		Message:    core.AssistantMessage(core.TextBlock{Text: "persisted"}),
		StopReason: core.StopEndTurn,
	}, nil
}
