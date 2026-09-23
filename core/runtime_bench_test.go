package core

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"
)

// Benchmarks for the T08 bounds. Byte and commit counts are store-independent
// (they measure core's encoding and transaction shape); wall-clock numbers use
// the in-memory store here, and extensions/sqlite measures real storage.

// quietBenchLogs discards the run logs agents write to the default logger so
// they do not interleave with benchmark results.
func quietBenchLogs(b *testing.B) {
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
	b.Cleanup(func() { slog.SetDefault(previous) })
}

func toolTurnsAgent(tb testing.TB, turns int) *Agent {
	tb.Helper()
	script := make([]Message, 0, turns)
	for i := 0; i < turns-1; i++ {
		script = append(script, asstTool(fmt.Sprintf("c%d", i), "echo", `{}`))
	}
	script = append(script, asstText("done"))
	agent, err := New(&scriptedProvider{turns: script}, AgentConfig{MaxTurns: turns + 1})
	if err != nil {
		tb.Fatal(err)
	}
	agent.RegisterTool(WithToolEffectPolicy(Func("echo", "echo", func(context.Context, struct{}) (string, error) {
		return strings.Repeat("x", 2048), nil
	}), ToolEffectPolicy{Kind: ToolEffectReadOnly}))
	return agent
}

// BenchmarkRuntimeToolTurns runs one run of N tool turns (2 KiB results) and
// reports bytes written and commits per turn. Per-turn cost staying flat as N
// grows is the no-history-rewrite bound.
func BenchmarkRuntimeToolTurns(b *testing.B) {
	quietBenchLogs(b)
	for _, turns := range []int{1, 16, 64} {
		b.Run(fmt.Sprintf("turns=%d", turns), func(b *testing.B) {
			var bytes, commits int64
			for range b.N {
				counting := &countingBytesStore{Store: NewMemoryStore()}
				store := &countingTransactionsStore{Store: counting}
				runtime, err := NewRuntime(context.Background(), RuntimeConfig{Store: store})
				if err != nil {
					b.Fatal(err)
				}
				if err := runtime.Register("agent", "v1", toolTurnsAgent(b, turns)); err != nil {
					b.Fatal(err)
				}
				before, beforeCommits := counting.writes.Load(), store.writes.Load()
				if _, err := runtime.Run(context.Background(), "agent", "v1", "work", SubmitOptions{}); err != nil {
					b.Fatal(err)
				}
				bytes += counting.writes.Load() - before
				commits += store.writes.Load() - beforeCommits
				_ = runtime.Close()
			}
			b.ReportMetric(float64(bytes)/float64(b.N*turns), "bytes/turn")
			b.ReportMetric(float64(commits)/float64(b.N*turns), "commits/turn")
		})
	}
}

// BenchmarkRuntimeConversationTurn reports the bytes one new conversation
// turn writes after a history of N turns. It stays flat because turns
// reference the head's transcript instead of copying it.
func BenchmarkRuntimeConversationTurn(b *testing.B) {
	quietBenchLogs(b)
	for _, history := range []int{1, 16, 64} {
		b.Run(fmt.Sprintf("history=%d", history), func(b *testing.B) {
			counting := &countingBytesStore{Store: NewMemoryStore()}
			runtime, err := NewRuntime(context.Background(), RuntimeConfig{Store: counting})
			if err != nil {
				b.Fatal(err)
			}
			defer runtime.Close()
			if err := runtime.Register("chat", "v1", testAgent(fixedAnswerProvider{answer: strings.Repeat("a", 2048)})); err != nil {
				b.Fatal(err)
			}
			head := ""
			for i := range history {
				result, err := runtime.Run(context.Background(), "chat", "v1", fmt.Sprintf("q%d", i), SubmitOptions{Conversation: ConversationOptions{ID: "bench", ExpectedHead: head}})
				if err != nil {
					b.Fatal(err)
				}
				head = result.RunID
			}
			var bytes int64
			b.ResetTimer()
			for i := range b.N {
				before := counting.writes.Load()
				result, err := runtime.Run(context.Background(), "chat", "v1", fmt.Sprintf("next %d", i), SubmitOptions{Conversation: ConversationOptions{ID: "bench", ExpectedHead: head}})
				if err != nil {
					b.Fatal(err)
				}
				bytes += counting.writes.Load() - before
				head = result.RunID
			}
			b.ReportMetric(float64(bytes)/float64(b.N), "bytes/turn")
		})
	}
}

// seedRuns commits count compact run records in state through the index
// maintenance path, with the given definition.
func seedRuns(tb testing.TB, store Store, prefix string, count int, state RuntimeState, definition string) {
	tb.Helper()
	const perTx = 512
	for start := 0; start < count; start += perTx {
		if err := store.Transaction(context.Background(), true, func(tx StoreTransaction) error {
			for i := start; i < min(start+perTx, count); i++ {
				runID := fmt.Sprintf("%s-%08d", prefix, i)
				if state == RuntimeTerminal {
					// Index as terminal the way a real terminal commit does.
					active := storedRuntimeRun{Version: runtimeEncodingVersion, RunID: runID, DefinitionID: definition, DefinitionRevision: "v1", State: RuntimeRunning, Generation: 1}
					if err := putRuntimeRun(tx, active); err != nil {
						return err
					}
				}
				record := storedRuntimeRun{
					Version: runtimeEncodingVersion, RunID: runID, DefinitionID: definition, DefinitionRevision: "v1",
					Task: "work", State: state, Generation: 2, Result: RunResult{RunID: runID, Status: RunCompleted, Output: "done"},
				}
				if err := putRuntimeRun(tx, record); err != nil {
					return err
				}
			}
			return nil
		}); err != nil {
			tb.Fatal(err)
		}
	}
}

// BenchmarkRuntimeRecover measures one Recover pass over active runs (admitted
// runs of an unregistered definition, which recovery leaves ready) with a
// backlog of terminal runs. Its cost follows the active count only.
func BenchmarkRuntimeRecover(b *testing.B) {
	quietBenchLogs(b)
	for _, shape := range []struct{ active, terminal int }{{0, 0}, {0, 20000}, {1000, 0}, {1000, 20000}} {
		b.Run(fmt.Sprintf("active=%d/terminal=%d", shape.active, shape.terminal), func(b *testing.B) {
			base := NewMemoryStore()
			runtime, err := NewRuntime(context.Background(), RuntimeConfig{Store: noCloseStore{Store: base}})
			if err != nil {
				b.Fatal(err)
			}
			defer runtime.Close()
			seedRuns(b, base, "active", shape.active, RuntimeReady, "unregistered")
			seedRuns(b, base, "terminal", shape.terminal, RuntimeTerminal, "agent")
			b.ResetTimer()
			for range b.N {
				if err := runtime.Recover(context.Background()); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// completedToolRun runs a finished run of N tool turns and returns its ID.
func completedToolRun(b *testing.B, runtime *Runtime, turns int) string {
	b.Helper()
	if err := runtime.Register("agent", "v1", toolTurnsAgent(b, turns)); err != nil {
		b.Fatal(err)
	}
	result, err := runtime.Run(context.Background(), "agent", "v1", "work", SubmitOptions{})
	if err != nil {
		b.Fatal(err)
	}
	return result.RunID
}

// BenchmarkRuntimeEventsPage reads the first page of 256 committed events of
// a 64-turn run, transcript payloads attached.
func BenchmarkRuntimeEventsPage(b *testing.B) {
	quietBenchLogs(b)
	runtime := newBenchRuntime(b)
	handle := runtime.Handle(completedToolRun(b, runtime, 64))
	b.ResetTimer()
	for range b.N {
		page, err := handle.Events(context.Background(), 0, defaultEventPageLimit)
		if err != nil || len(page.Events) == 0 {
			b.Fatal(err)
		}
	}
}

// BenchmarkRuntimeSnapshot reads the full authoritative snapshot of runs of
// growing history. Its cost is proportional to history by design; waiting
// and incremental observation do not read it.
func BenchmarkRuntimeSnapshot(b *testing.B) {
	quietBenchLogs(b)
	for _, turns := range []int{16, 64} {
		b.Run(fmt.Sprintf("turns=%d", turns), func(b *testing.B) {
			runtime := newBenchRuntime(b)
			handle := runtime.Handle(completedToolRun(b, runtime, turns))
			b.ResetTimer()
			for range b.N {
				if _, err := handle.Snapshot(context.Background()); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// BenchmarkRuntimeObserverLag measures the time from a committing call
// (ResolveWait) returning to a WaitEvents consumer holding the committed
// page.
func BenchmarkRuntimeObserverLag(b *testing.B) {
	quietBenchLogs(b)
	runtime := newBenchRuntime(b)
	provider := &benchQuestionProvider{}
	if err := runtime.Register("asker", "v1", questionTestAgent(b, provider)); err != nil {
		b.Fatal(err)
	}
	var lag time.Duration
	b.ResetTimer()
	for range b.N {
		b.StopTimer()
		handle, err := runtime.Submit(context.Background(), "asker", "v1", "help", SubmitOptions{})
		if err != nil {
			b.Fatal(err)
		}
		wait := benchWaitForQuestion(b, handle)
		snapshot, err := handle.Snapshot(context.Background())
		if err != nil {
			b.Fatal(err)
		}
		got := make(chan time.Time, 1)
		go func() {
			if _, err := handle.WaitEvents(context.Background(), snapshot.EventSequence, 16); err != nil {
				b.Error(err)
			}
			got <- time.Now()
		}()
		b.StartTimer()
		if err := handle.ResolveWait(context.Background(), wait.ID, WaitResolution{Answer: json.RawMessage(`"eu"`)}); err != nil {
			b.Fatal(err)
		}
		committed := time.Now()
		lag += (<-got).Sub(committed)
		b.StopTimer()
		if _, err := handle.Await(context.Background()); err != nil {
			b.Fatal(err)
		}
		b.StartTimer()
	}
	b.ReportMetric(float64(lag.Nanoseconds())/float64(b.N), "lag-ns/commit")
}

type benchQuestionProvider struct{ calls int }

func (p *benchQuestionProvider) Invoke(_ context.Context, request Request) (Response, error) {
	last := request.Messages[len(request.Messages)-1]
	if last.Role == "tool" {
		return fixtureResponse(asstText("answered")), nil
	}
	p.calls++
	return fixtureResponse(asstTool(fmt.Sprintf("q-%d", p.calls), "ask_user", `{"prompt":"region?"}`)), nil
}

func benchWaitForQuestion(b *testing.B, handle *RunHandle) WaitSnapshot {
	b.Helper()
	for {
		page, err := handle.WaitEvents(context.Background(), 0, maxEventPageLimit)
		if err != nil {
			b.Fatal(err)
		}
		for _, event := range page.Events {
			if event.Kind == CommittedRunState && event.State == RuntimeWaiting {
				snapshot, err := handle.Snapshot(context.Background())
				if err != nil {
					b.Fatal(err)
				}
				return snapshot.Waits[len(snapshot.Waits)-1]
			}
		}
		time.Sleep(time.Millisecond)
	}
}

func newBenchRuntime(b *testing.B) *Runtime {
	b.Helper()
	runtime, err := NewEphemeralRuntime()
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = runtime.Close() })
	return runtime
}
