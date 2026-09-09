package core

import (
	"context"
	"github.com/emotional-data8482/automata/retry"
	"github.com/emotional-data8482/automata/tracing"
	"log/slog"
)

// testAgent retains setup for pre-migration characterization fixtures only.
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

const DefaultMaxSteps = DefaultMaxTurns

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

func (a *Agent) WithMaxSteps(maxSteps int) *Agent {
	a.maxSteps = maxSteps
	return a
}

func (a *Agent) WithRetry(cfg retry.Config) *Agent {
	a.retryCfg = cfg
	return a
}

// WithApprover sets the Approver that gates tool calls before execution. The
// default is [AllowAll], which permits every call unconditionally.
func (a *Agent) WithApprover(ap Approver) *Agent {
	a.approver = ap
	return a
}

// WithToolPolicy sets the default deterministic limits for local tool work.
// The zero policy preserves the historical unbounded behavior. The policy is
// copied; configure the agent before starting any runs. Use the package-level
// [WithToolPolicy] RunOption to replace it for one run.
func (a *Agent) WithToolPolicy(policy ToolPolicy) *Agent {
	a.toolPolicy = policy.clone()
	return a
}

// WithDefaultCallOptions sets the [CallOptions] applied to every run of this
// agent. A per-run [WithCallOptions] override is merged over these defaults.
func (a *Agent) WithDefaultCallOptions(o CallOptions) *Agent {
	a.defaultCallOptions = o
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
// Tool errors are not fatal: if the tool's Execute returns an error, the run
// converts it to "error: <msg>" and feeds it back to the model as the tool
// result, letting the model recover. The exceptions are [context.Canceled]
// and [context.DeadlineExceeded], which abort the run. See [Func] for the
// typed-handler convenience wrapper.
func (a *Agent) RegisterTool(t Tool) {
	a.tools = append(a.tools, t)
}

func (a *Agent) RegisterFunc(name, description string, fn func(context.Context) string) {
	a.RegisterTool(Func(name, description, func(ctx context.Context, _ struct{}) (string, error) {
		return fn(ctx), nil
	}))
}

func legacyCallOptions(o CallOptions) RunOption {
	return func(c *runConfig) { c.options = c.options.merge(o) }
}
func (a *Agent) testResumeSession(m []Message) *Session {
	s, e := a.ResumeSession(m)
	if e != nil {
		panic(e)
	}
	return s
}

func fixtureResponse(m Message) Response {
	r := StopEndTurn
	if len(m.ToolUses()) > 0 {
		r = StopToolUse
	}
	return Response{Message: m, StopReason: r}
}
