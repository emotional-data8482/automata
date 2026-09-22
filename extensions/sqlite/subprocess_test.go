package sqlite

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/emotional-data8482/automata/core"
)

// TestMain re-enters the test binary as a subprocess for cross-process
// ownership tests. A role other than "" never reaches the test suite.
func TestMain(m *testing.M) {
	switch role := os.Getenv("AUTOMATA_SQLITE_TEST_ROLE"); role {
	case "second-owner":
		_, err := Open(context.Background(), os.Getenv("AUTOMATA_SQLITE_TEST_PATH"))
		if err != nil {
			fmt.Println("open-error: " + err.Error())
			os.Exit(3)
		}
		fmt.Println("opened-unexpectedly")
		os.Exit(4)
	case "interrupted-owner":
		runInterruptedOwnerChild()
		os.Exit(0)
	case "effect-dispatch-owner":
		runEffectDispatchOwnerChild()
		os.Exit(0)
	case "structured-correction-owner":
		runStructuredCorrectionOwnerChild()
		os.Exit(0)
	case "child-wait-owner":
		runChildWaitOwner()
	case "child-lost-wake-owner":
		runChildLostWakeOwner()
	case "child-cancel-owner":
		runChildCancelOwner()
	}
	os.Exit(m.Run())
}

func runInterruptedOwnerChild() {
	ctx := context.Background()
	store, err := Open(ctx, os.Getenv("AUTOMATA_SQLITE_TEST_PATH"))
	if err != nil {
		fmt.Println("open-error: " + err.Error())
		os.Exit(3)
	}
	runtime, err := core.NewRuntime(ctx, core.RuntimeConfig{Store: store})
	if err != nil {
		fmt.Println("runtime-error: " + err.Error())
		os.Exit(3)
	}
	agent, err := core.New(blockingProvider{}, core.AgentConfig{})
	if err != nil {
		fmt.Println("agent-error: " + err.Error())
		os.Exit(3)
	}
	if err := runtime.Register("agent", "v1", agent); err != nil {
		fmt.Println("register-error: " + err.Error())
		os.Exit(3)
	}
	handle, err := runtime.Submit(ctx, "agent", "v1", "work", core.SubmitOptions{Scope: "subprocess", Key: "one"})
	if err != nil {
		fmt.Println("submit-error: " + err.Error())
		os.Exit(3)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		snapshot, err := handle.Snapshot(ctx)
		if err != nil {
			fmt.Println("snapshot-error: " + err.Error())
			os.Exit(3)
		}
		if snapshot.State == core.RuntimeRunning {
			break
		}
		if time.Now().After(deadline) {
			fmt.Println("never-started: " + string(snapshot.State))
			os.Exit(3)
		}
		time.Sleep(2 * time.Millisecond)
	}
	fmt.Println("running " + handle.ID())
	select {} // Killed by the parent test process.
}

func runEffectDispatchOwnerChild() {
	ctx := context.Background()
	store, err := Open(ctx, os.Getenv("AUTOMATA_SQLITE_TEST_PATH"))
	if err != nil {
		fmt.Println("open-error: " + err.Error())
		os.Exit(3)
	}
	runtime, err := core.NewRuntime(ctx, core.RuntimeConfig{Store: store})
	if err != nil {
		fmt.Println("runtime-error: " + err.Error())
		os.Exit(3)
	}
	agent, err := core.New(effectDispatchProvider{}, core.AgentConfig{Tools: []core.Tool{effectOracleTool(os.Getenv("AUTOMATA_SQLITE_TEST_ORACLE"), true)}})
	if err != nil {
		fmt.Println("agent-error: " + err.Error())
		os.Exit(3)
	}
	if err := runtime.Register("agent", "v1", agent); err != nil {
		fmt.Println("register-error: " + err.Error())
		os.Exit(3)
	}
	if _, err := runtime.Submit(ctx, "agent", "v1", "work", core.SubmitOptions{Scope: "subprocess", Key: "effect"}); err != nil {
		fmt.Println("submit-error: " + err.Error())
		os.Exit(3)
	}
	select {}
}

func runStructuredCorrectionOwnerChild() {
	ctx := context.Background()
	store, err := Open(ctx, os.Getenv("AUTOMATA_SQLITE_TEST_PATH"))
	if err != nil {
		fmt.Println("open-error: " + err.Error())
		os.Exit(3)
	}
	runtime, err := core.NewRuntime(ctx, core.RuntimeConfig{Store: store})
	if err != nil {
		fmt.Println("runtime-error: " + err.Error())
		os.Exit(3)
	}
	agent, err := structuredCorrectionAgent(os.Getenv("AUTOMATA_SQLITE_TEST_ORACLE"), &structuredCorrectionProvider{})
	if err != nil {
		fmt.Println("agent-error: " + err.Error())
		os.Exit(3)
	}
	if err := runtime.Register("agent", "v1", agent); err != nil {
		fmt.Println("register-error: " + err.Error())
		os.Exit(3)
	}
	if _, err := runtime.Submit(ctx, "agent", "v1", "produce summary", core.SubmitOptions{Scope: "subprocess", Key: "structured"}); err != nil {
		fmt.Println("submit-error: " + err.Error())
		os.Exit(3)
	}
	select {} // Killed by the parent test process after the correction commits.
}

// structuredCorrectionAgent builds the declared structured-output definition
// shared by the killed owner and the reopening parent; only the provider
// script differs (the provider is not part of the binding identity).
func structuredCorrectionAgent(oracle string, p core.Provider) (*core.Agent, error) {
	write := core.FuncResult("write", "write a report", func(_ context.Context, input struct {
		Path string `json:"path"`
	}) (core.ToolResult, error) {
		file, err := os.OpenFile(oracle, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
		if err != nil {
			return core.ToolResult{}, err
		}
		if _, err := file.WriteString("write\n"); err != nil {
			_ = file.Close()
			return core.ToolResult{}, err
		}
		if err := file.Sync(); err != nil {
			_ = file.Close()
			return core.ToolResult{}, err
		}
		_ = file.Close()
		result := core.TextResult("written")
		result.Effect = core.EffectReport{Status: core.EffectApplied, Receipt: "write-1"}
		return result, nil
	})
	write = core.WithToolEffectPolicy(write, core.ToolEffectPolicy{
		Kind: core.ToolEffectMutating, Scope: "docs",
		SemanticKey: func(raw json.RawMessage) (string, error) {
			var in struct {
				Path string `json:"path"`
			}
			if err := json.Unmarshal(raw, &in); err != nil {
				return "", err
			}
			return in.Path, nil
		},
	})
	return core.New(p, core.AgentConfig{
		Tools: []core.Tool{write},
		StructuredOutput: &core.StructuredOutputConfig{
			Schema:         json.RawMessage(`{"type":"object","properties":{"summary":{"type":"string"}},"required":["summary"]}`),
			MaxCorrections: 2,
		},
	})
}

// structuredCorrectionProvider scripts the killed owner: one accepted write,
// one invalid structured payload whose correction commits, then block so the
// parent kills the process before the corrected turn.
type structuredCorrectionProvider struct{ calls int }

func (p *structuredCorrectionProvider) Invoke(_ context.Context, _ core.Request) (core.Response, error) {
	switch p.calls {
	case 0:
		p.calls++
		return core.Response{
			Message:    core.AssistantMessage(core.ToolUseBlock{ID: "write-1", Name: "write", Input: json.RawMessage(`{"path":"report.txt"}`)}),
			StopReason: core.StopToolUse,
		}, nil
	case 1:
		p.calls++
		return core.Response{
			Message: core.AssistantMessage(core.ToolUseBlock{
				ID: "call-1", Name: "automata_structured_output", Input: json.RawMessage(`{"summary":5}`),
			}),
			StopReason: core.StopToolUse,
		}, nil
	default:
		// The correction evidence committed atomically with the transcript;
		// report that and wait to be killed.
		fmt.Println("correction-committed")
		select {}
	}
}

func effectOracleTool(path string, block bool) core.Tool {
	tool := core.FuncResult("write_effect", "append one external effect", func(ctx context.Context, _ struct{}) (core.ToolResult, error) {
		op, ok := core.ToolOperationFromContext(ctx)
		if !ok {
			return core.ToolResult{}, errors.New("missing durable operation identity")
		}
		file, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
		if err != nil {
			return core.ToolResult{}, err
		}
		if _, err := file.WriteString("write\n"); err != nil {
			_ = file.Close()
			return core.ToolResult{}, err
		}
		if err := file.Sync(); err != nil {
			_ = file.Close()
			return core.ToolResult{}, err
		}
		if err := file.Close(); err != nil {
			return core.ToolResult{}, err
		}
		fmt.Println("effect " + op.ID)
		if block {
			select {}
		}
		result := core.TextResult("write already exists")
		result.Effect = core.EffectReport{Status: core.EffectApplied, Receipt: op.ID}
		return result, nil
	})
	return core.WithToolEffectPolicy(tool, core.ToolEffectPolicy{Kind: core.ToolEffectMutating})
}

type effectDispatchProvider struct{}

func (effectDispatchProvider) Invoke(context.Context, core.Request) (core.Response, error) {
	return core.Response{
		Message:    core.AssistantMessage(core.ToolUseBlock{ID: "write-1", Name: "write_effect", Input: []byte(`{}`)}),
		StopReason: core.StopToolUse,
	}, nil
}

type blockingProvider struct{}

func (blockingProvider) Invoke(ctx context.Context, _ core.Request) (core.Response, error) {
	<-ctx.Done()
	<-make(chan struct{}) // A killed owner never unwinds gracefully.
	return core.Response{}, ctx.Err()
}

func staticAgent(t *testing.T) *core.Agent {
	t.Helper()
	agent, err := core.New(staticProvider{}, core.AgentConfig{})
	if err != nil {
		t.Fatal(err)
	}
	return agent
}

func subprocessEnv(role, path string) []string {
	return append(os.Environ(),
		"AUTOMATA_SQLITE_TEST_ROLE="+role,
		"AUTOMATA_SQLITE_TEST_PATH="+path,
	)
}

func startSubprocess(t *testing.T, role, path string) (*exec.Cmd, *bufio.Scanner) {
	t.Helper()
	cmd := exec.Command(os.Args[0])
	cmd.Env = subprocessEnv(role, path)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	return cmd, bufio.NewScanner(stdout)
}

func waitSubprocessLine(t *testing.T, scanner *bufio.Scanner, prefix string) string {
	t.Helper()
	lineCh := make(chan string, 1)
	go func() {
		for scanner.Scan() {
			if strings.HasPrefix(scanner.Text(), prefix) {
				lineCh <- scanner.Text()
				return
			}
		}
		lineCh <- ""
	}()
	select {
	case line := <-lineCh:
		if line == "" {
			t.Fatalf("subprocess ended without a %q line", prefix)
		}
		return line
	case <-time.After(10 * time.Second):
		t.Fatalf("timed out waiting for a %q line", prefix)
		return ""
	}
}

func TestSecondProcessCannotBecomeOwner(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("flock-based ownership is unsupported on windows")
	}
	path := filepath.Join(t.TempDir(), "runtime.sqlite")
	first, err := Open(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = first.Close() })

	cmd, scanner := startSubprocess(t, "second-owner", path)
	line := waitSubprocessLine(t, scanner, "open-error: ")
	if !strings.Contains(line, ErrOwned.Error()) {
		t.Fatalf("second owner error = %q", line)
	}
	// The child has exited by the time it printed its error.
	_ = cmd.Wait()
	t.Cleanup(func() {}) // already reaped; the cleanup kill is a no-op
}

func TestInterruptedDispatchedEffectRequiresReconciliation(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("flock-based ownership is unsupported on windows")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "runtime.sqlite")
	oracle := filepath.Join(dir, "effects.log")
	t.Setenv("AUTOMATA_SQLITE_TEST_ORACLE", oracle)

	cmd, scanner := startSubprocess(t, "effect-dispatch-owner", path)
	line := waitSubprocessLine(t, scanner, "effect ")
	operationID := strings.TrimPrefix(line, "effect ")
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait()

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
	agent, err := core.New(staticProvider{}, core.AgentConfig{Tools: []core.Tool{effectOracleTool(oracle, false)}})
	if err != nil {
		t.Fatal(err)
	}
	if err := reopened.Register("agent", "v1", agent); err != nil {
		t.Fatal(err)
	}
	if err := reopened.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	handle, err := reopened.Submit(ctx, "agent", "v1", "work", core.SubmitOptions{Scope: "subprocess", Key: "effect"})
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := handle.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.State != core.RuntimeNeedsAttention || len(snapshot.ToolBatches) != 1 ||
		len(snapshot.ToolBatches[0].Invocations) != 1 ||
		snapshot.ToolBatches[0].Invocations[0].State != core.ToolInvocationUncertain {
		t.Fatalf("recovered effect snapshot = %#v", snapshot)
	}
	if snapshot.ToolBatches[0].Invocations[0].OperationID != operationID {
		t.Fatalf("operation ID = %q, want %q", snapshot.ToolBatches[0].Invocations[0].OperationID, operationID)
	}
	if err := handle.Reconcile(ctx, operationID, core.EffectResolution{
		Result: core.TextResult("verified existing write"),
		Effect: core.EffectReport{Status: core.EffectApplied, Receipt: "oracle-write-1"},
	}); err != nil {
		t.Fatal(err)
	}
	result, err := handle.Await(ctx)
	if err != nil || result.Output != "persisted" {
		t.Fatalf("reconciled run = %#v, %v", result, err)
	}
	data, err := os.ReadFile(oracle)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(string(data), "write\n"); got != 1 {
		t.Fatalf("external effect count = %d, want 1", got)
	}
}

func TestInterruptedOwnerRunBecomesAttention(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("flock-based ownership is unsupported on windows")
	}
	path := filepath.Join(t.TempDir(), "runtime.sqlite")
	ctx := context.Background()

	cmd, scanner := startSubprocess(t, "interrupted-owner", path)
	line := waitSubprocessLine(t, scanner, "running ")
	runID := strings.TrimPrefix(line, "running ")
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait()

	// The OS releases the owner lock when the killed process dies; the store
	// reopens and the interrupted run is classified without inventing a result.
	store, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	reopened, err := core.NewRuntime(ctx, core.RuntimeConfig{Store: store})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	if err := reopened.Register("agent", "v1", staticAgent(t)); err != nil {
		t.Fatal(err)
	}
	if err := reopened.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	snapshot, err := reopened.Handle(runID).Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.State != core.RuntimeNeedsAttention || snapshot.Attention == nil ||
		!strings.Contains(snapshot.Attention.Reason, "previous owner stopped during execution") {
		t.Fatalf("recovered snapshot = %#v", snapshot)
	}

	// The exact admission identity still resolves the same run.
	handle, err := reopened.Submit(ctx, "agent", "v1", "work", core.SubmitOptions{Scope: "subprocess", Key: "one"})
	if err != nil || handle.ID() != runID {
		t.Fatalf("resolved admission = %q, %v; want %q", handle.ID(), err, runID)
	}
	if _, err := handle.Await(ctx); !errors.Is(err, core.ErrRunNeedsAttention) {
		t.Fatalf("await = %v", err)
	}
}

// structuredReopenProvider drives the reopened run: it proposes the duplicate
// write alone (rejected by the semantic guard without dispatch) and then the
// valid structured payload.
type structuredReopenProvider struct{ calls int }

func (p *structuredReopenProvider) Invoke(_ context.Context, _ core.Request) (core.Response, error) {
	switch p.calls {
	case 0:
		p.calls++
		return core.Response{
			Message:    core.AssistantMessage(core.ToolUseBlock{ID: "write-2", Name: "write", Input: json.RawMessage(`{"path":"report.txt"}`)}),
			StopReason: core.StopToolUse,
		}, nil
	case 1:
		p.calls++
		return core.Response{
			Message: core.AssistantMessage(core.ToolUseBlock{
				ID: "call-2", Name: "automata_structured_output", Input: json.RawMessage(`{"summary":"done"}`),
			}),
			StopReason: core.StopToolUse,
		}, nil
	default:
		return core.Response{}, errors.New("no script for turn")
	}
}

// TestStructuredCorrectionSurvivesProcessKill kills the owner after the
// invalid structured payload's correction evidence committed, reopens the
// store, and proves the same run continues inside the original budgets: the
// duplicate write is rejected by the configured guard, the accepted receipt is
// retained, and the valid payload becomes the run's structured output.
func TestStructuredCorrectionSurvivesProcessKill(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("flock-based ownership is unsupported on windows")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "runtime.sqlite")
	oracle := filepath.Join(dir, "effects.log")
	t.Setenv("AUTOMATA_SQLITE_TEST_ORACLE", oracle)

	cmd, scanner := startSubprocess(t, "structured-correction-owner", path)
	waitSubprocessLine(t, scanner, "correction-committed")
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait()

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
	agent, err := structuredCorrectionAgent(oracle, &structuredReopenProvider{})
	if err != nil {
		t.Fatal(err)
	}
	if err := reopened.Register("agent", "v1", agent); err != nil {
		t.Fatal(err)
	}
	if err := reopened.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	handle, err := reopened.Submit(ctx, "agent", "v1", "produce summary", core.SubmitOptions{Scope: "subprocess", Key: "structured"})
	if err != nil {
		t.Fatal(err)
	}
	result, err := handle.Await(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if string(result.StructuredOutput) != `{"summary":"done"}` || result.Status != core.RunCompleted {
		t.Fatalf("reopened result = %#v", result)
	}
	if result.Turns != 4 {
		t.Fatalf("turns = %d, want 4 cumulative provider turns across restart", result.Turns)
	}
	data, err := os.ReadFile(oracle)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(string(data), "write\n"); got != 1 {
		t.Fatalf("external effect count = %d, want 1 (guard rejected the duplicate)", got)
	}
	snapshot, err := handle.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	receipt := ""
	denied := false
	for _, batch := range snapshot.ToolBatches {
		for _, invocation := range batch.Invocations {
			if invocation.Effect.Status == core.EffectApplied {
				receipt = invocation.Effect.Receipt
			}
			if strings.Contains(invocation.Result.Text(), "denied: semantic mutation already applied or unresolved") {
				denied = true
			}
		}
	}
	if receipt != "write-1" || !denied {
		t.Fatalf("receipt = %q, duplicate denied = %v; batches = %#v", receipt, denied, snapshot.ToolBatches)
	}
}
