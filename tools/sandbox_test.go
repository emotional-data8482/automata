package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/emotional-data8482/automata/core"
)

// recordingExecutor stands in for a sandbox backend.
type recordingExecutor struct {
	mu    sync.Mutex
	calls []Command
	exec  func(context.Context, Command) (CommandResult, error)
}

func (e *recordingExecutor) Exec(ctx context.Context, cmd Command) (CommandResult, error) {
	e.mu.Lock()
	e.calls = append(e.calls, cmd)
	e.mu.Unlock()
	return e.exec(ctx, cmd)
}

func TestShellRunsThroughExecutor(t *testing.T) {
	executor := &recordingExecutor{exec: func(_ context.Context, cmd Command) (CommandResult, error) {
		return CommandResult{Output: []byte("sandboxed " + strings.Join(cmd.Args, " "))}, nil
	}}
	tool := Shell(ShellConfig{Allow: []string{"go"}, Dir: "/work", Executor: executor})
	out, err := executeDomain(t, tool, context.Background(), `{"command":"go","args":["test","./..."]}`)
	if err != nil || out != "sandboxed test ./..." {
		t.Fatalf("out = %q, %v", out, err)
	}
	if _, err := executeDomain(t, tool, context.Background(), `{"command":"rm"}`); err == nil {
		t.Fatal("unlisted command was not denied")
	}
	if len(executor.calls) != 1 {
		t.Fatalf("calls = %#v; the denied command must not reach the executor", executor.calls)
	}
	got := executor.calls[0]
	if got.Path != "go" || got.Dir != "/work" || got.OutputLimit != shellMaxOutput+1 {
		t.Fatalf("command = %#v", got)
	}
}

func TestShellExecutorOutcomes(t *testing.T) {
	big := strings.Repeat("x", shellMaxOutput+10)
	cases := map[string]struct {
		result CommandResult
		err    error
		want   string
		isErr  bool
	}{
		"non-zero exit":   {result: CommandResult{ExitCode: 2, Output: []byte("boom")}, want: "go test: exit status 2\noutput:\nboom", isErr: true},
		"executor error":  {err: errors.New("sandbox gone"), want: "go test: sandbox gone", isErr: true},
		"empty output":    {want: "(no output)"},
		"truncated":       {result: CommandResult{Output: []byte(big)}, want: "[truncated: output exceeds 64KB]"},
		"truncated error": {result: CommandResult{ExitCode: 1, Output: []byte(big)}, want: "[truncated: output exceeds 64KB]", isErr: true},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			executor := &recordingExecutor{exec: func(context.Context, Command) (CommandResult, error) { return tc.result, tc.err }}
			out, err := executeDomain(t, Shell(ShellConfig{Allow: []string{"go"}, Executor: executor}), context.Background(), `{"command":"go","args":["test"]}`)
			if (err != nil) != tc.isErr || !strings.Contains(out, tc.want) {
				t.Fatalf("out = %q, err = %v; want %q (error %v)", out, err, tc.want, tc.isErr)
			}
			if len(out) > shellMaxOutput+100 {
				t.Fatalf("output was not capped: %d bytes", len(out))
			}
		})
	}
}

// A sandbox that honors ctx gets the same timeout and cancellation semantics
// as the host: the tool's own timeout is recoverable, the parent's is fatal.
func TestShellExecutorTimeoutAndCancellation(t *testing.T) {
	blocking := &recordingExecutor{exec: func(ctx context.Context, _ Command) (CommandResult, error) {
		<-ctx.Done()
		return CommandResult{Output: []byte("partial")}, ctx.Err()
	}}
	tool := Shell(ShellConfig{Allow: []string{"sleep"}, Timeout: 20 * time.Millisecond, Executor: blocking})
	out, err := executeDomain(t, tool, context.Background(), `{"command":"sleep"}`)
	if err == nil || !strings.Contains(out, "timed out") || !strings.Contains(out, "partial") {
		t.Fatalf("timeout = %q, %v; want a recoverable timeout carrying the output", out, err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	tool = Shell(ShellConfig{Allow: []string{"sleep"}, Timeout: time.Minute, Executor: &recordingExecutor{
		exec: func(ctx context.Context, _ Command) (CommandResult, error) {
			cancel()
			<-ctx.Done()
			return CommandResult{}, ctx.Err()
		}}})
	if _, err := tool.Execute(ctx, json.RawMessage(`{"command":"sleep"}`)); !errors.Is(err, context.Canceled) {
		t.Fatalf("parent cancellation = %v; want it to abort the run", err)
	}
}

func TestLocalExecutorReportsExitAndBoundsOutput(t *testing.T) {
	res, err := LocalExecutor().Exec(context.Background(), Command{Path: "sh", Args: []string{"-c", "echo out; echo err >&2; exit 4"}})
	if err != nil || res.ExitCode != 4 || !strings.Contains(string(res.Output), "out") || !strings.Contains(string(res.Output), "err") {
		t.Fatalf("result = %+v %q, %v", res, res.Output, err)
	}
	res, err = LocalExecutor().Exec(context.Background(), Command{Path: "sh", Args: []string{"-c", "yes | head -c 100000"}, OutputLimit: 10})
	if err != nil || len(res.Output) != 10 {
		t.Fatalf("bounded output = %d bytes, %v; want 10", len(res.Output), err)
	}
	if _, err := LocalExecutor().Exec(context.Background(), Command{Path: "definitely-not-a-program"}); err == nil {
		t.Fatal("missing program: expected a start error")
	}
}

// memFS stands in for a sandbox filesystem.
type memFS struct {
	mu       sync.Mutex
	files    map[string]string
	writeErr error
	scope    string
}

func (m *memFS) EffectScope() string { return m.scope }

func (m *memFS) ReadFile(_ context.Context, name string, limit int64) ([]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	data, ok := m.files[name]
	if !ok {
		return nil, fmt.Errorf("%s: no such file", name)
	}
	return []byte(data[:min(int64(len(data)), limit)]), nil
}

func (m *memFS) WriteFile(_ context.Context, name string, data []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.writeErr != nil {
		return m.writeErr
	}
	m.files[name] = string(data)
	return nil
}

func TestFileToolsRunThroughFileSystem(t *testing.T) {
	fsys := &memFS{files: map[string]string{"big.txt": strings.Repeat("b", readMaxBytes+10)}, scope: "sandbox:task-1"}
	ctx := context.Background()
	if _, err := executeDomain(t, WriteFileFS(fsys), ctx, `{"path":"notes/a.txt","content":"in the sandbox"}`); err != nil {
		t.Fatal(err)
	}
	if fsys.files["notes/a.txt"] != "in the sandbox" {
		t.Fatalf("files = %v", fsys.files)
	}
	if got, err := executeDomain(t, ReadFileFS(fsys), ctx, `{"path":"notes/a.txt"}`); err != nil || got != "in the sandbox" {
		t.Fatalf("read = %q, %v", got, err)
	}
	if got, _ := executeDomain(t, ReadFileFS(fsys), ctx, `{"path":"big.txt"}`); !strings.HasSuffix(got, "[truncated: file exceeds 256KB]") {
		t.Fatal("missing truncation marker")
	}
	for _, path := range []string{"", "../x", "/etc/passwd"} {
		args, _ := json.Marshal(map[string]string{"path": path, "content": "x"})
		if _, err := executeDomain(t, ReadFileFS(fsys), ctx, string(args)); err == nil {
			t.Errorf("read %q was not rejected", path)
		}
		result, err := WriteFileFS(fsys).Execute(ctx, args)
		if err != nil || !result.IsError || result.Effect.Status != core.EffectNotApplied {
			t.Errorf("write %q = %#v, %v; want not applied", path, result, err)
		}
	}
	if len(fsys.files) != 2 {
		t.Fatalf("rejected paths reached the filesystem: %v", fsys.files)
	}
}

func TestWriteFileFSEffectStatus(t *testing.T) {
	cases := map[string]struct {
		err  error
		want core.EffectStatus
	}{
		"not written": {err: fmt.Errorf("sandbox stopped: %w", ErrNotWritten), want: core.EffectNotApplied},
		"unknown":     {err: errors.New("connection reset mid-copy"), want: core.EffectUnknown},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			result, err := WriteFileFS(&memFS{files: map[string]string{}, writeErr: tc.err}).Execute(context.Background(), json.RawMessage(`{"path":"a.txt","content":"x"}`))
			if err != nil || !result.IsError || result.Effect.Status != tc.want {
				t.Fatalf("result = %#v, %v; want %s", result, err, tc.want)
			}
		})
	}
}

type interruptedWriteFS struct {
	FileSystem
	interrupt  func()
	notWritten bool
}

func (f interruptedWriteFS) WriteFile(ctx context.Context, _ string, _ []byte) error {
	f.interrupt()
	<-ctx.Done()
	if f.notWritten {
		return fmt.Errorf("write interrupted: %w: %w", ErrNotWritten, ctx.Err())
	}
	return ctx.Err()
}

func TestWriteFileFSParentCancellationPreservesEffect(t *testing.T) {
	for _, deadline := range []bool{false, true} {
		for _, notWritten := range []bool{false, true} {
			t.Run(fmt.Sprintf("deadline=%t/notWritten=%t", deadline, notWritten), func(t *testing.T) {
				ctx, cancel := context.WithCancel(context.Background())
				interrupt := cancel
				wantErr := context.Canceled
				if deadline {
					cancel()
					ctx, cancel = context.WithTimeout(context.Background(), 20*time.Millisecond)
					interrupt = func() {}
					wantErr = context.DeadlineExceeded
				}
				defer cancel()
				fsys := interruptedWriteFS{interrupt: interrupt, notWritten: notWritten}
				result, err := WriteFileFS(fsys).Execute(ctx, json.RawMessage(`{"path":"a.txt","content":"x"}`))
				wantStatus := core.EffectUnknown
				if notWritten {
					wantStatus = core.EffectNotApplied
				}
				if !errors.Is(err, wantErr) || result.Effect.Status != wantStatus {
					t.Fatalf("result = %#v, err = %v; want %s and %v", result, err, wantStatus, wantErr)
				}
			})
		}
	}
}

// WriteFileFS declares a filesystem's EffectScope; LocalFS keeps the scope
// WriteFile declared before filesystems were pluggable.
func TestLocalFSEffectScope(t *testing.T) {
	root := t.TempDir()
	scoped, ok := LocalFS(root).(interface{ EffectScope() string })
	if !ok || scoped.EffectScope() != "filesystem:"+root {
		t.Fatalf("LocalFS scope = %v", scoped)
	}
}
