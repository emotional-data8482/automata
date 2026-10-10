package coretest_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/emotional-data8482/automata/core"
	"github.com/emotional-data8482/automata/core/coretest"
)

type lookup struct {
	City string `json:"city"`
}

func weatherAgent(t *testing.T, provider core.Provider) *core.Agent {
	weather := core.Func("weather", "look up the weather", func(_ context.Context, in lookup) (string, error) {
		return "sunny in " + in.City, nil
	})
	return coretest.NewAgent(t, provider, core.AgentConfig{Tools: []core.Tool{weather}})
}

func TestProviderReplaysScriptThroughRuntime(t *testing.T) {
	for name, provider := range map[string]interface {
		core.Provider
		Calls() int
		Requests() []core.Request
	}{
		"invoke": coretest.NewProvider(coretest.ToolCall("c1", "weather", lookup{City: "Oslo"}), coretest.Text("It is sunny.")),
		"stream": coretest.NewStreamProvider(coretest.ToolCall("c1", "weather", lookup{City: "Oslo"}), coretest.Text("It is sunny.")),
	} {
		t.Run(name, func(t *testing.T) {
			runtime := coretest.NewRuntime(t, core.RuntimeConfig{})
			ref := coretest.Register(t, runtime, "weather", "v1", weatherAgent(t, provider))
			result, err := runtime.Run(context.Background(), ref, "weather in Oslo?")
			if err != nil || result.Output != "It is sunny." {
				t.Fatalf("result = %+v, %v", result, err)
			}
			if provider.Calls() != 2 {
				t.Fatalf("calls = %d, want 2", provider.Calls())
			}
			second := provider.Requests()[1]
			last := second.Messages[len(second.Messages)-1]
			result0, ok := last.Blocks[0].(core.ToolResultBlock)
			if !ok || (core.Message{Blocks: result0.Content}).Text() != "sunny in Oslo" {
				t.Fatalf("second request ends with %#v", last)
			}
		})
	}
}

func TestProviderReportsExhaustionAndFailures(t *testing.T) {
	boom := errors.New("overloaded")
	provider := coretest.NewProvider(coretest.Fail(boom))
	if _, err := provider.Invoke(context.Background(), core.Request{}); !errors.Is(err, boom) {
		t.Fatalf("scripted failure = %v", err)
	}
	if _, err := provider.Invoke(context.Background(), core.Request{}); !errors.Is(err, coretest.ErrScriptExhausted) {
		t.Fatalf("exhausted = %v", err)
	}
	provider.Append(coretest.Text("again"))
	if response, err := provider.Invoke(context.Background(), core.Request{}); err != nil || response.Message.Text() != "again" || response.StopReason != core.StopEndTurn {
		t.Fatalf("appended = %+v, %v", response, err)
	}
}

func TestStreamProviderFailsMidStream(t *testing.T) {
	boom := errors.New("connection reset")
	provider := coretest.NewStreamProvider(coretest.Turn{Message: coretest.Text("partial").Message, Err: boom})
	runtime := coretest.NewRuntime(t, core.RuntimeConfig{})
	ref := coretest.Register(t, runtime, "agent", "v1", coretest.NewAgent(t, provider, core.AgentConfig{}))
	result, err := runtime.Run(context.Background(), ref, "go")
	// Awaited errors are rebuilt from the persisted failure: the provider's
	// error keeps its message.
	if err == nil || !strings.Contains(err.Error(), boom.Error()) || result.Output != "partial" {
		t.Fatalf("result = %+v, %v; want the partial output and the stream error", result, err)
	}
}

func TestReusableStoreSurvivesRestart(t *testing.T) {
	store := coretest.ReusableStore(core.NewMemoryStore())
	first, err := core.NewRuntime(context.Background(), core.RuntimeConfig{Store: store})
	if err != nil {
		t.Fatal(err)
	}
	ref := coretest.Register(t, first, "agent", "v1", coretest.NewAgent(t, coretest.NewProvider(coretest.Text("done")), core.AgentConfig{}))
	result, err := first.Run(context.Background(), ref, "go")
	if err != nil {
		t.Fatal(err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	second := coretest.NewRuntime(t, core.RuntimeConfig{Store: store})
	snapshot, err := second.Handle(result.RunID).Snapshot(context.Background())
	if err != nil || snapshot.Result.Output != "done" {
		t.Fatalf("snapshot after restart = %+v, %v", snapshot.Result, err)
	}
}
