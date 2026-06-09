package tools

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/emotional-data/automata/core"
)

const shellMaxOutput = 64 << 10 // 64 KiB

// ShellConfig configures the [Shell] tool. Allow is mandatory in practice: an
// empty allow-list denies every command.
type ShellConfig struct {
	// Allow lists the exact program names (or absolute paths) the model may
	// run. A requested command must match an entry verbatim — there is no
	// pattern matching.
	Allow []string
	// Dir is the working directory for commands ("" = the process's cwd).
	Dir string
	// Timeout bounds each invocation (default 30s).
	Timeout time.Duration
}

type shellParams struct {
	Command string   `json:"command" desc:"the program to run; must be on the allow-list"`
	Args    []string `json:"args,omitempty" desc:"arguments passed to the program"`
}

// Shell returns an opt-in "shell" tool that runs allow-listed programs.
// Commands execute argv-style via [exec.CommandContext] — there is no shell
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

	return core.Func("shell",
		"Run an allow-listed program with arguments and return its combined output. No shell features (pipes, globs, variables) are available.",
		func(ctx context.Context, p shellParams) (string, error) {
			if !allowed[p.Command] {
				return "", fmt.Errorf("command %q is not allow-listed", p.Command)
			}

			cctx, cancel := context.WithTimeout(ctx, timeout)
			defer cancel()

			cmd := exec.CommandContext(cctx, p.Command, p.Args...)
			cmd.Dir = cfg.Dir
			out, err := cmd.CombinedOutput()
			if len(out) > shellMaxOutput {
				out = append(out[:shellMaxOutput], "\n\n[truncated: output exceeds 64KB]"...)
			}

			if err != nil {
				// Propagate a parent cancellation so the run aborts, but report
				// this tool's own timeout as a plain, model-recoverable error —
				// returning context.DeadlineExceeded would kill the whole run.
				if ctx.Err() != nil {
					return "", ctx.Err()
				}
				if cctx.Err() != nil {
					return "", fmt.Errorf("%s timed out after %s\noutput:\n%s", p.Command, timeout, out)
				}
				return "", fmt.Errorf("%s %s: %v\noutput:\n%s", p.Command, strings.Join(p.Args, " "), err, out)
			}
			if len(out) == 0 {
				return "(no output)", nil
			}
			return string(out), nil
		})
}
