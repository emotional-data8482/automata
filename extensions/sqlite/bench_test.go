package sqlite

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/emotional-data8482/automata/core"
)

// Benchmarks for the T08 operating envelope on the supported local store
// (WAL, synchronous=FULL, one connection). They measure latency on real
// storage; core's benchmarks measure bytes and commits per transition.

func quietBenchLogs(b *testing.B) {
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
	b.Cleanup(func() { slog.SetDefault(previous) })
}

type benchTurnsProvider struct{ turns, calls int }

func (p *benchTurnsProvider) Invoke(context.Context, core.Request) (core.Response, error) {
	p.calls++
	if p.calls < p.turns {
		return core.Response{
			Message:    core.AssistantMessage(core.ToolUseBlock{ID: fmt.Sprintf("c%d", p.calls), Name: "echo", Input: []byte(`{}`)}),
			StopReason: core.StopToolUse,
		}, nil
	}
	return core.Response{Message: core.AssistantMessage(core.TextBlock{Text: "done"}), StopReason: core.StopEndTurn}, nil
}

func benchTurnsAgent(b *testing.B, turns int) *core.Agent {
	b.Helper()
	echo := core.WithToolEffectPolicy(core.Func("echo", "echo", func(context.Context, struct{}) (string, error) {
		return strings.Repeat("x", 2048), nil
	}), core.ToolEffectPolicy{Kind: core.ToolEffectReadOnly})
	agent, err := core.New(&benchTurnsProvider{turns: turns}, core.AgentConfig{Tools: []core.Tool{echo}, MaxTurns: turns + 1})
	if err != nil {
		b.Fatal(err)
	}
	return agent
}

func benchRuntime(b *testing.B, config core.RuntimeConfig) *core.Runtime {
	b.Helper()
	store, err := Open(context.Background(), filepath.Join(b.TempDir(), "bench.sqlite"))
	if err != nil {
		b.Fatal(err)
	}
	config.Store = store
	runtime, err := core.NewRuntime(context.Background(), config)
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = runtime.Close() })
	return runtime
}

// BenchmarkSQLiteToolTurns runs whole runs of N tool turns and reports the
// latency per turn on durable storage.
func BenchmarkSQLiteToolTurns(b *testing.B) {
	quietBenchLogs(b)
	for _, turns := range []int{1, 16} {
		b.Run(fmt.Sprintf("turns=%d", turns), func(b *testing.B) {
			runtime := benchRuntime(b, core.RuntimeConfig{})
			start := time.Now()
			for i := range b.N {
				revision := fmt.Sprintf("v%d", i)
				if err := runtime.Register("agent", revision, benchTurnsAgent(b, turns)); err != nil {
					b.Fatal(err)
				}
				if _, err := runtime.Run(context.Background(), "agent", revision, "work", core.SubmitOptions{}); err != nil {
					b.Fatal(err)
				}
			}
			b.ReportMetric(float64(time.Since(start).Nanoseconds())/float64(b.N*turns), "ns/turn")
		})
	}
}

// BenchmarkSQLiteRecover measures a Recover pass with a backlog of terminal
// runs and no active work: the pass reads only the active index.
func BenchmarkSQLiteRecover(b *testing.B) {
	quietBenchLogs(b)
	for _, backlog := range []int{0, 1000} {
		b.Run(fmt.Sprintf("terminal=%d", backlog), func(b *testing.B) {
			runtime := benchRuntime(b, core.RuntimeConfig{})
			if err := runtime.Register("agent", "v1", benchTurnsAgent(b, 1)); err != nil {
				b.Fatal(err)
			}
			for range backlog {
				if _, err := runtime.Run(context.Background(), "agent", "v1", "work", core.SubmitOptions{}); err != nil {
					b.Fatal(err)
				}
			}
			b.ResetTimer()
			for range b.N {
				if err := runtime.Recover(context.Background()); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// BenchmarkSQLiteReads measures the committed-event page (256 events with
// transcript attached) and the full snapshot of a 64-turn run.
func BenchmarkSQLiteReads(b *testing.B) {
	quietBenchLogs(b)
	runtime := benchRuntime(b, core.RuntimeConfig{})
	if err := runtime.Register("agent", "v1", benchTurnsAgent(b, 64)); err != nil {
		b.Fatal(err)
	}
	result, err := runtime.Run(context.Background(), "agent", "v1", "work", core.SubmitOptions{})
	if err != nil {
		b.Fatal(err)
	}
	handle := runtime.Handle(result.RunID)
	b.Run("events-page", func(b *testing.B) {
		for range b.N {
			if _, err := handle.Events(context.Background(), 0, 256); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("snapshot", func(b *testing.B) {
		for range b.N {
			if _, err := handle.Snapshot(context.Background()); err != nil {
				b.Fatal(err)
			}
		}
	})
}

// BenchmarkSQLitePrune measures deleting settled one-turn runs, all classes.
func BenchmarkSQLitePrune(b *testing.B) {
	quietBenchLogs(b)
	runtime := benchRuntime(b, core.RuntimeConfig{})
	if err := runtime.Register("agent", "v1", benchTurnsAgent(b, 1)); err != nil {
		b.Fatal(err)
	}
	for range b.N {
		if _, err := runtime.Run(context.Background(), "agent", "v1", "work", core.SubmitOptions{}); err != nil {
			b.Fatal(err)
		}
	}
	b.ResetTimer()
	report, err := runtime.Prune(context.Background(), core.RetentionPolicy{
		Events: time.Nanosecond, History: time.Nanosecond, Runs: time.Nanosecond, Limit: b.N,
	})
	if err != nil || report.Runs != b.N {
		b.Fatalf("prune = %#v, %v", report, err)
	}
}
