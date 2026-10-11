package tools

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/emotional-data8482/automata/core"
)

const shellMaxOutput = 64 << 10 // 64 KiB

// Executor runs one argv command for [Shell]. [LocalExecutor] runs it on the
// host; a sandbox backend (a container, a VM, a remote workspace) implements
// Executor to run the same tool inside its own environment.
//
// Exec must run [Command.Path] with [Command.Args] directly, with no shell
// interpretation, and stop the command when ctx ends. Shell has already
// checked the command against its allow-list and applied its timeout to ctx.
// Methods are called concurrently when a model requests several tool calls at
// once. ctx carries the run's [core.ToolOperation], so an executor shared by
// many runs can resolve the run's own sandbox per call.
type Executor interface {
	// Exec returns a nil error when the command ran to completion, whatever
	// its exit code. It returns an error when the command could not be
	// started or did not finish, including when ctx ends.
	Exec(ctx context.Context, cmd Command) (CommandResult, error)
}

// Command is one argv invocation.
type Command struct {
	// Path is the program, exactly as the model named it.
	Path string
	Args []string
	// Dir is the working directory ("" = the executor's default).
	Dir string
	// OutputLimit is how many bytes of combined output the executor should
	// keep (0 = no limit). Output past the limit may be dropped.
	OutputLimit int
}

// CommandResult is the outcome of a command that ran to completion.
type CommandResult struct {
	ExitCode int
	// Output is the combined stdout and stderr.
	Output []byte
}

// LocalExecutor returns an [Executor] that runs commands on the host via
// [exec.CommandContext].
func LocalExecutor() Executor { return localExecutor{} }

type localExecutor struct{}

func (localExecutor) Exec(ctx context.Context, c Command) (CommandResult, error) {
	cmd := exec.CommandContext(ctx, c.Path, c.Args...)
	cmd.Dir = c.Dir
	out := &boundedBuffer{limit: c.OutputLimit}
	cmd.Stdout, cmd.Stderr = out, out
	err := cmd.Run()
	result := CommandResult{Output: out.buf}
	if err == nil {
		return result, nil
	}
	if ctx.Err() != nil {
		return result, ctx.Err()
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && exitErr.Exited() {
		result.ExitCode = exitErr.ExitCode()
		return result, nil
	}
	return result, err
}

// boundedBuffer keeps the first limit bytes written (all of them when limit
// is 0) and discards the rest, so a chatty command cannot exhaust memory.
type boundedBuffer struct {
	limit int
	buf   []byte
}

func (b *boundedBuffer) Write(p []byte) (int, error) {
	keep := p
	if b.limit > 0 {
		keep = p[:min(len(p), max(b.limit-len(b.buf), 0))]
	}
	b.buf = append(b.buf, keep...)
	return len(p), nil
}

// ShellConfig configures the [Shell] tool. Allow is mandatory in practice: an
// empty allow-list denies every command.
type ShellConfig struct {
	// Allow lists the exact program names (or absolute paths) the model may
	// run. A requested command must match an entry verbatim — there is no
	// pattern matching.
	Allow []string
	// Dir is the working directory for commands ("" = the executor's
	// default, the process's cwd for [LocalExecutor]).
	Dir string
	// Timeout bounds each invocation (default 30s).
	Timeout time.Duration
	// Executor runs allowed commands (nil = [LocalExecutor]).
	Executor Executor
}

type shellParams struct {
	Command string   `json:"command" desc:"the program to run; must be on the allow-list"`
	Args    []string `json:"args,omitempty" desc:"arguments passed to the program"`
}

// Shell returns an opt-in "shell" tool that runs allow-listed programs through
// cfg.Executor. Commands execute argv-style — there is no shell
// interpretation, so pipes, globs and `$(...)` have no effect (and no
// injection surface). To expose a pipeline, allow-list a script that wraps it.
//
// Combined stdout+stderr is returned, capped at 64 KiB. A non-zero exit is a
// tool error (recoverable by the model) carrying the output; a timeout is
// reported as a plain error so it never aborts the enclosing run.
func Shell(cfg ShellConfig) core.Tool {
	allowed := make(map[string]bool, len(cfg.Allow))
	for _, c := range cfg.Allow {
		allowed[c] = true
	}
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	executor := cfg.Executor
	if executor == nil {
		executor = LocalExecutor()
	}

	return domainTool("shell",
		"Run an allow-listed program with arguments and return its combined output. No shell features (pipes, globs, variables) are available.",
		func(ctx context.Context, p shellParams) (string, error) {
			if !allowed[p.Command] {
				return "", fmt.Errorf("command %q is not allow-listed", p.Command)
			}

			cctx, cancel := context.WithTimeout(ctx, timeout)
			defer cancel()

			res, err := executor.Exec(cctx, Command{
				Path: p.Command, Args: p.Args, Dir: cfg.Dir,
				// One byte past the cap tells truncation apart from a full fit.
				OutputLimit: shellMaxOutput + 1,
			})
			out := res.Output
			if len(out) > shellMaxOutput {
				out = append(out[:shellMaxOutput:shellMaxOutput], "\n\n[truncated: output exceeds 64KB]"...)
			}

			if err != nil || res.ExitCode != 0 {
				// Propagate a parent cancellation so the run aborts, but report
				// this tool's own timeout as a plain, model-recoverable error —
				// returning context.DeadlineExceeded would kill the whole run.
				if ctx.Err() != nil {
					return "", ctx.Err()
				}
				if cctx.Err() != nil {
					return "", fmt.Errorf("%s timed out after %s\noutput:\n%s", p.Command, timeout, out)
				}
				if err == nil {
					err = fmt.Errorf("exit status %d", res.ExitCode)
				}
				return "", fmt.Errorf("%s %s: %v\noutput:\n%s", p.Command, strings.Join(p.Args, " "), err, out)
			}
			if len(out) == 0 {
				return "(no output)", nil
			}
			return string(out), nil
		})
}
