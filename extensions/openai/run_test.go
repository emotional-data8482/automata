package openai

import (
	"context"
	"testing"

	"github.com/emotional-data8482/automata/core"
)

// runTask runs task on agent through an ephemeral Runtime, the only way an
// Agent executes. stream selects the live-view path.
func runTask(t *testing.T, agent *core.Agent, task string, stream bool) (core.RunResult, error) {
	t.Helper()
	runtime, err := core.NewEphemeralRuntime()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = runtime.Close() })
	ref, err := runtime.Register("test", "v1", agent)
	if err != nil {
		t.Fatal(err)
	}
	if stream {
		return runtime.RunStream(context.Background(), ref, task, nil)
	}
	return runtime.Run(context.Background(), ref, task)
}

// invokeOnly hides a provider's streaming method so a run exercises the
// non-streaming Invoke conversion.
type invokeOnly struct{ core.Provider }
