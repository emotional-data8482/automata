package sqlite

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/emotional-data8482/automata/core"
)

// turnScript answers by the number of assistant turns already in the request,
// so the same script continues correctly in a reopened process. Every turn
// reports one input token.
type turnScript struct {
	turns []core.Message
	calls *atomic.Int32
}

func (p turnScript) Invoke(_ context.Context, request core.Request) (core.Response, error) {
	if p.calls != nil {
		p.calls.Add(1)
	}
	turn := 0
	for _, message := range request.Messages {
		if message.Role == "assistant" {
			turn++
		}
	}
	if turn >= len(p.turns) {
		return core.Response{}, fmt.Errorf("no script for turn %d", turn)
	}
	message := p.turns[turn]
	message.Usage = &core.Usage{InputTokens: 1}
	stop := core.StopEndTurn
	if len(message.ToolUses()) > 0 {
		stop = core.StopToolUse
	}
	return core.Response{Message: message, StopReason: stop}, nil
}

func toolTurn(id, name, input string) core.Message {
	return core.AssistantMessage(core.ToolUseBlock{ID: id, Name: name, Input: json.RawMessage(input)})
}

func textTurn(text string) core.Message { return core.AssistantMessage(core.TextBlock{Text: text}) }

func delegateTool() core.Tool {
	return core.DurableChildTool(core.ToolDefinition{
		Name:        "delegate",
		Description: "delegate to a durable child",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"topic":{"type":"string"}},"required":["topic"]}`),
	}, core.DurableChildPolicy{DefinitionID: "child", Revision: "v1"})
}

// capParentAgent writes once, delegates to a child, then proposes a second
// write that the persisted subtree cap (write + delegate + the child's own
// call) must deny after restart.
func capParentAgent(oracle string) (*core.Agent, error) {
	return core.New(turnScript{turns: []core.Message{
		toolTurn("w1", "write_effect", `{}`),
		toolTurn("d1", "delegate", `{"topic":"tea"}`),
		toolTurn("w2", "write_effect", `{}`),
		textTurn("parent done"),
	}}, core.AgentConfig{
		Tools:      []core.Tool{effectOracleTool(oracle, false), delegateTool()},
		ToolPolicy: core.ToolPolicy{MaxCalls: 3},
	})
}

func questionChildAgent() (*core.Agent, error) {
	ask := core.WithDurableWait(core.Func("ask", "ask a human", func(context.Context, struct{}) (string, error) {
		return "unused", nil
	}), core.DurableWaitPolicy{Kind: core.WaitQuestion})
	return core.New(turnScript{turns: []core.Message{toolTurn("q1", "ask", `{}`), textTurn("child done")}}, core.AgentConfig{Tools: []core.Tool{ask}})
}

func delegatingParentAgent() (*core.Agent, error) {
	return core.New(turnScript{turns: []core.Message{toolTurn("d1", "delegate", `{"topic":"tea"}`), textTurn("parent done")}},
		core.AgentConfig{Tools: []core.Tool{delegateTool()}})
}

// childOwnerRuntime opens the store (optionally wrapped) and registers the
// parent and child definitions shared by an owner process and its reopener.
func childOwnerRuntime(ctx context.Context, wrap func(core.Store) core.Store, parent, child func() (*core.Agent, error)) *core.Runtime {
	store, err := Open(ctx, os.Getenv("AUTOMATA_SQLITE_TEST_PATH"))
	if err != nil {
		fmt.Println("open-error: " + err.Error())
		os.Exit(3)
	}
	var backing core.Store = store
	if wrap != nil {
		backing = wrap(store)
	}
	rt, err := core.NewRuntime(ctx, core.RuntimeConfig{Store: backing})
	if err != nil {
		fmt.Println("runtime-error: " + err.Error())
		os.Exit(3)
	}
	for id, build := range map[string]func() (*core.Agent, error){"parent": parent, "child": child} {
		agent, err := build()
		if err != nil {
			fmt.Println("agent-error: " + err.Error())
			os.Exit(3)
		}
		if err := rt.Register(id, "v1", agent); err != nil {
			fmt.Println("register-error: " + err.Error())
			os.Exit(3)
		}
	}
	return rt
}

func submitOwnerParent(ctx context.Context, rt *core.Runtime, key string) *core.RunHandle {
	handle, err := rt.Submit(ctx, "parent", "v1", "work", core.SubmitOptions{Scope: "subprocess", Key: key})
	if err != nil {
		fmt.Println("submit-error: " + err.Error())
		os.Exit(3)
	}
	return handle
}

// pollOwner waits in the owner process until ready reports true.
func pollOwner(ready func() (bool, error)) {
	deadline := time.Now().Add(10 * time.Second)
	for {
		ok, err := ready()
		if err != nil {
			fmt.Println("poll-error: " + err.Error())
			os.Exit(3)
		}
		if ok {
			return
		}
		if time.Now().After(deadline) {
			fmt.Println("poll-timeout")
			os.Exit(3)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func linkedChildID(snapshot core.RunSnapshot) string {
	for _, batch := range snapshot.ToolBatches {
		for _, invocation := range batch.Invocations {
			if invocation.ChildRunID != "" {
				return invocation.ChildRunID
			}
		}
	}
	return ""
}

// runChildWaitOwner leaves a parent waiting on an admitted child that waits
// on a durable question, after one external write, then waits to be killed.
func runChildWaitOwner() {
	ctx := context.Background()
	oracle := os.Getenv("AUTOMATA_SQLITE_TEST_ORACLE")
	rt := childOwnerRuntime(ctx, nil, func() (*core.Agent, error) { return capParentAgent(oracle) }, questionChildAgent)
	parent := submitOwnerParent(ctx, rt, "child-wait")
	var childID, waitID string
	pollOwner(func() (bool, error) {
		snapshot, err := parent.Snapshot(ctx)
		if err != nil || snapshot.State != core.RuntimeWaiting {
			return false, err
		}
		if childID = linkedChildID(snapshot); childID == "" {
			return false, nil
		}
		child, err := rt.Handle(childID).Snapshot(ctx)
		if err != nil || child.State != core.RuntimeWaiting || len(child.Waits) != 1 {
			return false, err
		}
		waitID = child.Waits[0].ID
		return true, nil
	})
	fmt.Printf("child-waiting %s %s %s\n", parent.ID(), childID, waitID)
	select {}
}

// terminalChildStore stops the owner right after a child run's terminal
// state commits, before the parent-side wake transaction can run.
type terminalChildStore struct{ core.Store }

type terminalChildTx struct {
	core.StoreTransaction
	terminal *string
}

func (tx terminalChildTx) Put(bucket, key string, value []byte) error {
	if bucket == "runtime_runs" {
		var record struct {
			State       string `json:"state"`
			ParentRunID string `json:"parent_run_id"`
		}
		if json.Unmarshal(value, &record) == nil && record.State == string(core.RuntimeTerminal) && record.ParentRunID != "" {
			*tx.terminal = key
		}
	}
	return tx.StoreTransaction.Put(bucket, key, value)
}

func (s terminalChildStore) Transaction(ctx context.Context, writable bool, fn func(core.StoreTransaction) error) error {
	terminal := ""
	err := s.Store.Transaction(ctx, writable, func(tx core.StoreTransaction) error {
		return fn(terminalChildTx{StoreTransaction: tx, terminal: &terminal})
	})
	if err == nil && terminal != "" {
		fmt.Println("child-terminal " + terminal)
		select {} // Killed before the parent is woken.
	}
	return err
}

func runChildLostWakeOwner() {
	ctx := context.Background()
	rt := childOwnerRuntime(ctx, func(store core.Store) core.Store { return terminalChildStore{Store: store} },
		delegatingParentAgent,
		func() (*core.Agent, error) {
			return core.New(turnScript{turns: []core.Message{textTurn("child done")}}, core.AgentConfig{})
		})
	parent := submitOwnerParent(ctx, rt, "lost-wake")
	fmt.Println("parent " + parent.ID())
	select {}
}

// ignoringProvider never returns and ignores cancellation, like a
// non-cooperative external call.
type ignoringProvider struct{ started chan struct{} }

func (p ignoringProvider) Invoke(context.Context, core.Request) (core.Response, error) {
	close(p.started)
	select {}
}

func runChildCancelOwner() {
	ctx := context.Background()
	started := make(chan struct{})
	rt := childOwnerRuntime(ctx, nil, delegatingParentAgent, func() (*core.Agent, error) {
		return core.New(ignoringProvider{started: started}, core.AgentConfig{})
	})
	parent := submitOwnerParent(ctx, rt, "cancel")
	<-started
	if err := parent.Cancel(ctx); err != nil {
		fmt.Println("cancel-error: " + err.Error())
		os.Exit(3)
	}
	snapshot, err := parent.Snapshot(ctx)
	if err != nil {
		fmt.Println("snapshot-error: " + err.Error())
		os.Exit(3)
	}
	fmt.Printf("canceled %s %s\n", parent.ID(), linkedChildID(snapshot))
	select {} // The child worker never stops; the process is killed.
}

func reopenChildRuntime(t *testing.T, path string, parent, child *core.Agent) *core.Runtime {
	t.Helper()
	ctx := context.Background()
	store, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	reopened, err := core.NewRuntime(ctx, core.RuntimeConfig{Store: store})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	if err := reopened.Register("child", "v1", child); err != nil {
		t.Fatal(err)
	}
	if err := reopened.Register("parent", "v1", parent); err != nil {
		t.Fatal(err)
	}
	if err := reopened.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	return reopened
}

func killOwner(t *testing.T, role, path string, line string) []string {
	t.Helper()
	cmd, scanner := startSubprocess(t, role, path)
	fields := strings.Fields(waitSubprocessLine(t, scanner, line))
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait()
	return fields[1:]
}

func awaitBounded(t *testing.T, handle *core.RunHandle) (core.RunResult, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return handle.Await(ctx)
}

// TestChildWaitSurvivesProcessKill kills the owner while a parent waits on an
// admitted child that waits on a question. The reopened runtime resumes the
// same child, wakes the parent once, keeps the persisted subtree cap and
// write count, and totals usage without double counting.
func TestChildWaitSurvivesProcessKill(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("flock-based ownership is unsupported on windows")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "runtime.sqlite")
	oracle := filepath.Join(dir, "effects.log")
	t.Setenv("AUTOMATA_SQLITE_TEST_ORACLE", oracle)
	ids := killOwner(t, "child-wait-owner", path, "child-waiting ")
	parentID, childID, waitID := ids[0], ids[1], ids[2]

	parent, err := capParentAgent(oracle)
	if err != nil {
		t.Fatal(err)
	}
	child, err := questionChildAgent()
	if err != nil {
		t.Fatal(err)
	}
	reopened := reopenChildRuntime(t, path, parent, child)
	ctx := context.Background()
	childSnapshot, err := reopened.Handle(childID).Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if childSnapshot.State != core.RuntimeWaiting || childSnapshot.Parent == nil || childSnapshot.Parent.RunID != parentID {
		t.Fatalf("reopened child = %s parent %#v", childSnapshot.State, childSnapshot.Parent)
	}
	if err := reopened.Handle(childID).ResolveWait(ctx, waitID, core.WaitResolution{Answer: json.RawMessage(`"yes"`), Decision: core.Allow}); err != nil {
		t.Fatal(err)
	}
	result, err := awaitBounded(t, reopened.Handle(parentID))
	if err != nil || result.Output != "parent done" {
		t.Fatalf("parent = %q, %v", result.Output, err)
	}
	snapshot, err := reopened.Handle(parentID).Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got := linkedChildID(snapshot); got != childID {
		t.Fatalf("linked child = %q, want the original %q", got, childID)
	}
	if len(snapshot.ToolBatches) != 3 || snapshot.ToolBatches[1].Invocations[0].Result.Text() != "child done" {
		t.Fatalf("parent batches = %#v", snapshot.ToolBatches)
	}
	denied := snapshot.ToolBatches[2].Invocations[0]
	if !strings.Contains(denied.Result.Text(), core.ErrToolBudgetExhausted.Error()) || denied.Effect.Status != core.EffectNotApplied {
		t.Fatalf("post-restart write = %#v, want denied by the persisted cap", denied)
	}
	want := core.TreeAccounting{Runs: 2, Usage: core.Usage{InputTokens: 6}, ProviderAttempts: 6}
	if snapshot.Accounting.Tree != want {
		t.Fatalf("tree accounting = %#v, want %#v", snapshot.Accounting.Tree, want)
	}
	data, err := os.ReadFile(oracle)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(string(data), "write\n"); got != 1 {
		t.Fatalf("external effect count = %d, want 1", got)
	}
}

// TestChildCompletionWakeSurvivesProcessKill kills the owner after the child
// run's terminal state committed but before the parent was woken. Recovery
// consumes the committed child outcome once without re-running the child.
func TestChildCompletionWakeSurvivesProcessKill(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("flock-based ownership is unsupported on windows")
	}
	path := filepath.Join(t.TempDir(), "runtime.sqlite")
	childID := killOwner(t, "child-lost-wake-owner", path, "child-terminal ")[0]

	parent, err := delegatingParentAgent()
	if err != nil {
		t.Fatal(err)
	}
	var childCalls atomic.Int32
	child, err := core.New(turnScript{turns: []core.Message{textTurn("child done")}, calls: &childCalls}, core.AgentConfig{})
	if err != nil {
		t.Fatal(err)
	}
	reopened := reopenChildRuntime(t, path, parent, child)
	childSnapshot, err := reopened.Handle(childID).Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if childSnapshot.Parent == nil {
		t.Fatal("reopened child has no parent")
	}
	parentID := childSnapshot.Parent.RunID
	result, err := awaitBounded(t, reopened.Handle(parentID))
	if err != nil || result.Output != "parent done" {
		t.Fatalf("parent = %q, %v", result.Output, err)
	}
	if got := childCalls.Load(); got != 0 {
		t.Fatalf("child re-ran %d provider turns after restart", got)
	}
	snapshot, err := reopened.Handle(parentID).Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if linkedChildID(snapshot) != childID || snapshot.ToolBatches[0].Invocations[0].Result.Text() != "child done" {
		t.Fatalf("parent batches = %#v", snapshot.ToolBatches)
	}
	if want := (core.TreeAccounting{Runs: 2, Usage: core.Usage{InputTokens: 3}, ProviderAttempts: 3}); snapshot.Accounting.Tree != want {
		t.Fatalf("tree accounting = %#v, want %#v", snapshot.Accounting.Tree, want)
	}
}

// TestChildCancellationSurvivesProcessKill kills the owner after a parent
// cancellation committed while its child's worker ignored the signal. The
// persisted propagation finishes the child as canceled on reopen without any
// new dispatch, and the interrupted child attempt is reported as unknown.
func TestChildCancellationSurvivesProcessKill(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("flock-based ownership is unsupported on windows")
	}
	path := filepath.Join(t.TempDir(), "runtime.sqlite")
	ids := killOwner(t, "child-cancel-owner", path, "canceled ")
	parentID, childID := ids[0], ids[1]

	parent, err := delegatingParentAgent()
	if err != nil {
		t.Fatal(err)
	}
	var childCalls atomic.Int32
	child, err := core.New(turnScript{turns: []core.Message{textTurn("child done")}, calls: &childCalls}, core.AgentConfig{})
	if err != nil {
		t.Fatal(err)
	}
	reopened := reopenChildRuntime(t, path, parent, child)
	childResult, err := awaitBounded(t, reopened.Handle(childID))
	if !errors.Is(err, context.Canceled) || childResult.Status != core.RunCancelled {
		t.Fatalf("child = %s, %v; want canceled", childResult.Status, err)
	}
	if got := childCalls.Load(); got != 0 {
		t.Fatalf("child dispatched %d provider turns under a canceled parent", got)
	}
	snapshot, err := reopened.Handle(parentID).Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.State != core.RuntimeTerminal || snapshot.Result.Status != core.RunCancelled {
		t.Fatalf("parent = %s %s", snapshot.State, snapshot.Result.Status)
	}
	if snapshot.Accounting.Tree.Unsettled != 0 || snapshot.Accounting.Tree.UnknownAttempts != 1 {
		t.Fatalf("tree accounting = %#v, want settled with one unknown child attempt", snapshot.Accounting.Tree)
	}
}
