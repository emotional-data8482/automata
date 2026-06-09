package core

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"reflect"

	"github.com/emotional-data/automata/retry"
	"github.com/emotional-data/automata/tracing"
)

// Agent is the unified definition of an agent, whether it runs as a top-level
// orchestrator or as a sub-agent registered on another agent via [AsTool]. It
// holds the configuration for a run; the per-run conversation state lives in a
// [Loop] created fresh for each Run/RunStream call.
//
// Concurrency: Run and RunStream are safe to call concurrently — each call
// builds its own Loop and reads the Agent's config without mutating it. The
// WithXxx builders and RegisterTool/RegisterFunc mutate the Agent in place and
// are NOT safe to call concurrently with a run; configure the Agent fully
// before starting any runs.
type Agent struct {
	systemPrompt string
	tools        []Tool
	provider     Provider
	maxSteps     int
	retryCfg     retry.Config
	tracer       tracing.Tracer
	log          *slog.Logger
	approver     Approver

	preSendHooks []PreSendHook
}

// DefaultMaxSteps is used when an Agent is constructed without calling
// WithMaxSteps. Chosen to cover typical multi-step tool-using agents without
// letting a runaway loop burn tokens indefinitely.
const DefaultMaxSteps = 10

// New returns an Agent backed by the given provider with default
// configuration: [DefaultMaxSteps] steps, the no-op tracer, the default slog
// logger, [AllowAll] approval, and [retry.DefaultConfig].
func New(p Provider) *Agent {
	return &Agent{
		provider: p,
		tracer:   tracing.Noop,
		log:      slog.Default(),
		maxSteps: DefaultMaxSteps,
		approver: AllowAll,
		retryCfg: retry.DefaultConfig(),
	}
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
	for i, existing := range a.tools {
		if existing.Name() == t.Name() {
			a.tools[i] = t
			return
		}
	}
	a.tools = append(a.tools, t)
}

func (a *Agent) RegisterFunc(name, description string, fn func(context.Context) string) {
	a.RegisterTool(Func(name, description, func(ctx context.Context, _ struct{}) (string, error) {
		return fn(ctx), nil
	}))
}

func (a *Agent) Run(ctx context.Context, task string) (string, error) {
	l := newLoop(a)
	return l.run(ctx, task, "sync", func(ctx context.Context, _ *slog.Logger, messages []Message, tools []Tool) (Message, error) {
		return retry.Do(ctx, a.retryCfg, func() (Message, error) {
			return a.provider.Invoke(ctx, messages, tools)
		})
	})
}

// RunResult is delivered on the channel returned by [Agent.RunBackground].
type RunResult struct {
	Output string
	Err    error
}

// RunBackground runs the agent in a goroutine and returns a buffered channel
// that receives the result when the run completes. The channel is closed after
// the single result is sent. Cancel the run via ctx.
func (a *Agent) RunBackground(ctx context.Context, task string) <-chan RunResult {
	ch := make(chan RunResult, 1)
	go func() {
		defer close(ch)
		out, err := a.Run(ctx, task)
		ch <- RunResult{Output: out, Err: err}
	}()
	return ch
}

// agentTool adapts an [Agent] into a [Tool] so it can be registered on another
// agent as a sub-agent. The schema is derived from the type parameter passed to
// [AsTool] or [AsToolFunc]; task converts the model's raw JSON arguments into
// the task string handed to a fresh run of the wrapped agent (identity for
// AsTool, a typed renderer for AsToolFunc).
type agentTool struct {
	name   string
	schema json.RawMessage
	agent  *Agent
	task   func(args string) (string, error)
}

func (t *agentTool) Name() string            { return t.name }
func (t *agentTool) Schema() json.RawMessage { return t.schema }

func (t *agentTool) Execute(ctx context.Context, args string) (string, error) {
	task, err := t.task(args)
	if err != nil {
		return "", err
	}
	// If an enclosing run is streaming, run the sub-agent in streaming mode too
	// and forward its events into the parent's sink, tagged with this tool's
	// name. The tag is only stamped when empty so a deeper sub-agent's tag
	// survives. Otherwise fall back to a plain non-streaming run.
	if emit := emitterFrom(ctx); emit != nil {
		return t.agent.RunStream(ctx, task, func(ev StreamEvent) {
			if ev.Agent == "" {
				ev.Agent = t.name
			}
			emit(ev)
		})
	}
	return t.agent.Run(ctx, task)
}

// AsTool adapts an Agent into a Tool that an orchestrator can register and the
// model can invoke as a sub-agent. P must be a struct (or struct{} for a no-arg
// sub-agent); its exported fields define the JSON schema the model fills when
// calling the tool. The marshalled arguments are forwarded as the task string
// to the sub-agent's Run, so the sub-agent's system prompt should describe how
// to interpret them. To hand off rendered natural language instead of raw
// JSON, use [AsToolFunc].
//
// Each invocation runs the wrapped agent independently — sub-agent runs do not
// share conversation state with each other or with the orchestrator.
func AsTool[P any](a *Agent, name, description string) Tool {
	var zero P
	return &agentTool{
		name:   name,
		schema: buildSchema(name, description, reflect.TypeOf(zero)),
		agent:  a,
		task:   func(args string) (string, error) { return args, nil },
	}
}

// AsToolFunc is [AsTool] with a renderer: the schema advertised to the model is
// still derived from P, but instead of forwarding the model's raw JSON
// arguments verbatim, render turns the decoded P into the sub-agent's task.
// That lets the sub-agent receive natural language it already understands, with
// no "you will receive JSON…" boilerplate in its system prompt:
//
//	core.AsToolFunc[researchParams](researcher, "researcher", "...",
//	    func(p researchParams) string {
//	        return fmt.Sprintf("Research: %s\nQuestions:\n- %s",
//	            p.Topic, strings.Join(p.Questions, "\n- "))
//	    })
//
// Arguments that fail to decode into P are returned to the model as a tool
// error ("error: invalid args: …"), which it can recover from; see [Func] for
// the tool error semantics. Empty arguments ("", "null", "{}") render the zero
// value of P, matching [Func].
func AsToolFunc[P any](a *Agent, name, description string, render func(P) string) Tool {
	var zero P
	return &agentTool{
		name:   name,
		schema: buildSchema(name, description, reflect.TypeOf(zero)),
		agent:  a,
		task: func(args string) (string, error) {
			var params P
			if args != "" && args != "null" && args != "{}" {
				if err := json.Unmarshal([]byte(args), &params); err != nil {
					return "", fmt.Errorf("invalid args: %w", err)
				}
			}
			return render(params), nil
		},
	}
}
