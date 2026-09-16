package sqlite

import (
	"bufio"
	"context"
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
	if snapshot.State != core.RuntimeNeedsAttention ||
		!strings.Contains(snapshot.AttentionReason, "previous owner stopped during execution") {
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
