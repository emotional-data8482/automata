package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
)

// childProcessEnv marks a re-executed demo step. The demo's test uses it to
// route a re-executed test binary into run instead of the test runner.
const childProcessEnv = "DURABLE_HOST_STEP"

// demo runs the reference scenario, each step in a separate process that
// opens the same store, so nothing survives between steps except storage:
//
//  1. Submit ticket T-1; the run stops at its approval and the process exits.
//  2. Another process reads the committed events from a cursor.
//  3. Another process approves; the run publishes once, a reviewer child
//     returns a typed verdict, and the run finishes.
//  4. Resubmitting T-1 returns the same run; the watcher resumes from its
//     saved cursor and sees only the events it missed.
//  5. Ticket T-2 is approved by a process that dies right after the external
//     write, before the outcome commits.
//  6. A new process finds the invocation uncertain, reconciles it from the
//     destination's ledger without writing again, and the run finishes.
//
// It fails unless the destination saw exactly one write per ticket.
func demo(ctx context.Context, dir string, out io.Writer) error {
	if dir == "" {
		tmp, err := os.MkdirTemp("", "automata-durable-host-*")
		if err != nil {
			return err
		}
		defer os.RemoveAll(tmp)
		dir = tmp
	}
	self, err := os.Executable()
	if err != nil {
		return err
	}
	step := func(title string, env []string, args ...string) (string, int, error) {
		fmt.Fprintf(out, "\n$ durable_host %s   # %s\n", strings.Join(args, " "), title)
		cmd := exec.CommandContext(ctx, self, append([]string{"-dir", dir}, args...)...)
		cmd.Env = append(append(os.Environ(), childProcessEnv+"=1"), env...)
		var buf bytes.Buffer
		// One writer for both streams: exec then serializes their writes.
		combined := io.MultiWriter(out, &buf)
		cmd.Stdout, cmd.Stderr = combined, combined
		err := cmd.Run()
		if exit, ok := errors.AsType[*exec.ExitError](err); ok {
			return buf.String(), exit.ExitCode(), nil
		}
		return buf.String(), 0, err
	}
	mustStep := func(title string, args ...string) (string, error) {
		output, code, err := step(title, nil, args...)
		if err == nil && code != 0 {
			err = fmt.Errorf("step %q exited with %d", strings.Join(args, " "), code)
		}
		return output, err
	}
	expect := func(output, want string) error {
		if !strings.Contains(output, want) {
			return fmt.Errorf("expected output containing %q", want)
		}
		return nil
	}

	output, err := mustStep("admit a ticket; the run stops at its approval", "submit", "T-1", "Quarterly report")
	if err == nil {
		err = expect(output, "run is waiting")
	}
	if err != nil {
		return err
	}
	if _, err := mustStep("a separate observer reads committed events", "watch", "T-1"); err != nil {
		return err
	}
	if output, err = mustStep("a later process approves; the run finishes", "approve", "T-1"); err == nil {
		err = expect(output, "destination writes so far: 1")
	}
	if err != nil {
		return err
	}
	if output, err = mustStep("resubmitting the ticket resolves the same run", "submit", "T-1", "Quarterly report"); err == nil {
		err = expect(output, "already admitted")
	}
	if err != nil {
		return err
	}
	if _, err := mustStep("the observer resumes from its saved cursor", "watch", "T-1"); err != nil {
		return err
	}

	if output, err = mustStep("admit a second ticket", "submit", "T-2", "Incident summary"); err == nil {
		err = expect(output, "run is waiting")
	}
	if err != nil {
		return err
	}
	_, code, err := step("approve, but the process dies right after the external write",
		[]string{crashAfterWriteEnv + "=1"}, "approve", "T-2")
	if err != nil {
		return err
	}
	if code != 3 {
		return fmt.Errorf("the crashing approval exited with %d, want 3", code)
	}
	fmt.Fprintln(out, "(process exited before the write's outcome committed)")
	if output, err = mustStep("a new process reconciles from the destination and finishes", "reconcile", "T-2"); err == nil {
		err = expect(output, "destination writes so far: 2")
	}
	if err != nil {
		return err
	}

	entries, err := readLedger(dir)
	if err != nil {
		return err
	}
	perPath := map[string]int{}
	for _, entry := range entries {
		perPath[entry.Path]++
	}
	if len(entries) != 2 || perPath["quarterly-report.md"] != 1 || perPath["incident-summary.md"] != 1 {
		return fmt.Errorf("destination ledger = %+v, want exactly one write per ticket", entries)
	}
	fmt.Fprintf(out, "\nverified: %d tickets, %d destination writes, none repeated\n", len(perPath), len(entries))
	return nil
}
