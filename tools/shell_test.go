package tools

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestShellRunsAllowListedCommand(t *testing.T) {
	tool := Shell(ShellConfig{Allow: []string{"echo"}})
	out, err := tool.Execute(context.Background(), `{"command":"echo","args":["hi","there"]}`)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if out != "hi there\n" {
		t.Errorf("output = %q, want %q", out, "hi there\n")
	}
}

func TestShellDeniesUnlistedCommand(t *testing.T) {
	tool := Shell(ShellConfig{Allow: []string{"echo"}})
	if _, err := tool.Execute(context.Background(), `{"command":"rm","args":["-rf","/"]}`); err == nil {
		t.Fatal("unlisted command was not denied")
	}

	empty := Shell(ShellConfig{})
	if _, err := empty.Execute(context.Background(), `{"command":"echo"}`); err == nil {
		t.Fatal("empty allow-list did not deny")
	}
}

func TestShellNoShellInterpretation(t *testing.T) {
	tool := Shell(ShellConfig{Allow: []string{"echo"}})
	out, err := tool.Execute(context.Background(), `{"command":"echo","args":["$(whoami)"]}`)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if strings.TrimSpace(out) != "$(whoami)" {
		t.Errorf("output = %q — argument was shell-expanded", out)
	}
}

func TestShellNonZeroExitIsRecoverableError(t *testing.T) {
	tool := Shell(ShellConfig{Allow: []string{"sh"}})
	_, err := tool.Execute(context.Background(), `{"command":"sh","args":["-c","echo oops >&2; exit 3"]}`)
	if err == nil {
		t.Fatal("non-zero exit: expected error")
	}
	if !strings.Contains(err.Error(), "oops") {
		t.Errorf("error %q does not carry the command output", err)
	}
}

// TestShellTimeoutDoesNotAbortRun pins that the tool's own timeout surfaces as
// a plain error — not context.DeadlineExceeded, which the agent loop treats as
// fatal to the whole run.
func TestShellTimeoutDoesNotAbortRun(t *testing.T) {
	tool := Shell(ShellConfig{Allow: []string{"sleep"}, Timeout: 50 * time.Millisecond})
	start := time.Now()
	_, err := tool.Execute(context.Background(), `{"command":"sleep","args":["5"]}`)
	if err == nil {
		t.Fatal("expected timeout error")
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("timeout took %s, command was not killed", elapsed)
	}
	if err == context.DeadlineExceeded {
		t.Error("timeout returned bare context.DeadlineExceeded, which aborts the run")
	}
	if !strings.Contains(err.Error(), "timed out") {
		t.Errorf("error = %q, want a 'timed out' message", err)
	}
}
