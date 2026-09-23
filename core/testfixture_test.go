package core

import (
	"context"
	"log/slog"
	"testing"

	"github.com/emotional-data8482/automata/retry"
	"github.com/emotional-data8482/automata/tracing"
)

// runAgent runs task on agent the only way an Agent runs: admitted to a fresh
// ephemeral Runtime, awaited, and returned with its committed result.
func runAgent(t testing.TB, agent *Agent, task string, opts ...SubmitOption) (RunResult, error) {
	t.Helper()
	return runAgentStream(t, agent, task, nil, opts...)
}

// runAgentStream is runAgent with a live view.
func runAgentStream(t testing.TB, agent *Agent, task string, onEvent func(StreamEvent), opts ...SubmitOption) (RunResult, error) {
	t.Helper()
	runtime, err := NewEphemeralRuntime()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = runtime.Close() })
	ref, err := runtime.Register("agent", "test", agent)
	if err != nil {
		return RunResult{}, err
	}
	if onEvent == nil {
		return runtime.Run(context.Background(), ref, task, opts...)
	}
	return runtime.RunStream(context.Background(), ref, task, onEvent, opts...)
}

// testAgent builds a zero-config agent; the With* fixtures below adjust it
// before registration, which freezes a copy.
func testAgent(p Provider) *Agent {
	if p == nil {
		p = &scriptedProvider{}
	}
	a, err := New(p, AgentConfig{})
	if err != nil {
		panic(err)
	}
	return a
}

func (a *Agent) WithLogger(l *slog.Logger) *Agent {
	a.log = l
	return a
}

func (a *Agent) WithTracer(t tracing.Tracer) *Agent {
	a.tracer = t
	return a
}

func (a *Agent) WithSystemPrompt(prompt string) *Agent {
	a.systemPrompt = prompt
	return a
}

func (a *Agent) WithMaxTurns(maxTurns int) *Agent {
	a.maxTurns = maxTurns
	return a
}

func (a *Agent) WithRetry(cfg retry.Config) *Agent {
	a.retryCfg = cfg
	return a
}

func (a *Agent) WithToolPolicy(policy ToolPolicy) *Agent {
	a.toolPolicy = policy.clone()
	return a
}

func (a *Agent) WithCallOptions(o CallOptions) *Agent {
	a.callOptions = o
	return a
}

// WithPreSendHook registers a PreSendHook. Hooks fire in registration order
// once per turn, immediately before each provider invocation. Each hook
// receives the output of the previous hook.
//
// Like the other WithXxx methods, this mutates the Agent in place and is not
// safe to call concurrently with a run — configure all hooks before starting
// any runs.
func (a *Agent) WithPreSendHook(hook PreSendHook) *Agent {
	a.preSendHooks = append(a.preSendHooks, hook)
	return a
}

// WithTools replaces the agent's tool set with the given tools. Each call
// replaces the previous set; call once with all tools the agent should have.
// For incremental registration, use [Agent.RegisterTool].
func (a *Agent) WithTools(tools ...Tool) *Agent {
	a.tools = tools
	return a
}

// RegisterTool adds a tool to the agent. The tool's name (from t.Name()) is
// what the model uses to invoke it; registering a tool whose name matches an
// already-registered tool silently replaces the existing one.
//
// A non-nil Go error from Execute is fatal to the run; a model-visible
// recoverable failure is a ToolResult with IsError set. See [Func] for the
// typed-handler convenience wrapper.
func (a *Agent) RegisterTool(t Tool) {
	a.tools = append(a.tools, t)
}

func (a *Agent) RegisterFunc(name, description string, fn func(context.Context) string) {
	a.RegisterTool(Func(name, description, func(ctx context.Context, _ struct{}) (string, error) {
		return fn(ctx), nil
	}))
}

func roles(messages []Message) []string {
	out := make([]string, len(messages))
	for i, m := range messages {
		out[i] = m.Role
	}
	return out
}

func fixtureResponse(m Message) Response {
	r := StopEndTurn
	if len(m.ToolUses()) > 0 {
		r = StopToolUse
	}
	return Response{Message: m, StopReason: r}
}
