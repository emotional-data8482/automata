package core

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"reflect"

	"github.com/emotional-data8482/automata/retry"
	"github.com/emotional-data8482/automata/tracing"
)

// Agent holds frozen reusable configuration. Run and RunStream may be called
// concurrently. Construct a new Agent to change its configuration.
type Agent struct {
	systemPrompt       string
	tools              []Tool
	provider           Provider
	maxSteps           int
	retryCfg           retry.Config
	tracer             tracing.Tracer
	log                *slog.Logger
	approver           Approver
	defaultCallOptions CallOptions
	toolPolicy         ToolPolicy

	preSendHooks []PreSendHook
	observers    []RunObserver
}

// DefaultMaxTurns bounds a zero-config agent's public invocation.
const DefaultMaxTurns = 10

// AgentConfig is copied and validated by New. Zero MaxTurns selects
// DefaultMaxTurns; a nil Retry selects retry.DefaultConfig. Nil Logger, Tracer,
// and Approver select slog.Default, tracing.Noop, and AllowAll. Other zero
// fields disable optional behavior. Dependency instances and closures are shared
// and must support concurrent calls; their private state is not cloned.
type AgentConfig struct {
	SystemPrompt       string
	Tools              []Tool
	MaxTurns           int
	Retry              *retry.Config
	Tracer             tracing.Tracer
	Logger             *slog.Logger
	Approver           Approver
	DefaultCallOptions CallOptions
	ToolPolicy         ToolPolicy
	PreSendHooks       []PreSendHook
	Observers          []RunObserver
}

// New validates and freezes reusable configuration before any run is admitted.
func New(p Provider, config AgentConfig) (*Agent, error) {
	if nilDependency(p) {
		return nil, fmt.Errorf("nil provider")
	}
	if config.MaxTurns < 0 {
		return nil, ErrInvalidMaxSteps
	}
	if config.MaxTurns == 0 {
		config.MaxTurns = DefaultMaxTurns
	}
	if err := config.ToolPolicy.validate(); err != nil {
		return nil, err
	}
	if err := validateCallOptions(config.DefaultCallOptions); err != nil {
		return nil, err
	}
	frozen, err := freezeTools(config.Tools, "")
	if err != nil {
		return nil, err
	}
	a := &Agent{provider: p, systemPrompt: config.SystemPrompt, tools: frozen,
		maxSteps: config.MaxTurns, retryCfg: retry.DefaultConfig(), tracer: config.Tracer,
		log: config.Logger, approver: config.Approver,
		defaultCallOptions: cloneCallOptions(config.DefaultCallOptions), toolPolicy: config.ToolPolicy.clone(),
		preSendHooks: append([]PreSendHook(nil), config.PreSendHooks...),
		observers:    append([]RunObserver(nil), config.Observers...)}
	if config.Retry != nil {
		a.retryCfg = *config.Retry
	}
	if a.retryCfg.MaxAttempts < 0 || a.retryCfg.InitialDelay < 0 || a.retryCfg.MaxDelay < 0 || a.retryCfg.Multiplier < 0 {
		return nil, fmt.Errorf("invalid retry configuration")
	}
	if nilDependency(a.tracer) {
		a.tracer = tracing.Noop
	}
	if a.log == nil {
		a.log = slog.Default()
	}
	if nilDependency(a.approver) {
		a.approver = AllowAll
	}
	return a, nil
}

func nilDependency(v any) bool {
	if v == nil {
		return true
	}
	switch reflect.ValueOf(v).Kind() {
	case reflect.Pointer, reflect.Func, reflect.Map, reflect.Slice, reflect.Interface, reflect.Chan:
		return reflect.ValueOf(v).IsNil()
	}
	return false
}

// Run executes the agent directly on task and returns the [RunResult]. Options customize
// this run only; [WithCallOptions] overrides the agent's default call options.
// The result is populated as far as the run got, even on error.
//
// Run is a process-local convenience and is not persistent. New durable code
// registers the Agent with [Runtime] and uses Runtime.Run or Runtime.Submit.
func (a *Agent) Run(ctx context.Context, task string, opts ...RunOption) (RunResult, error) {
	cfg := a.newRunConfig(opts)
	s, err := a.beginRun(ctx, cfg, nil, nil, "sync")
	if err != nil {
		return s.finish(s.result, err)
	}
	cfg.scope = s
	result, err := a.runSync(s.ctx, newLoop(a, nil), task, cfg)
	return s.finish(result, err)
}

// runSync drives a pre-built loop through the non-streaming path. Split from
// Run so a [Session] can supply a loop seeded with its transcript.
func (a *Agent) runSync(ctx context.Context, l *loop, task string, cfg runConfig) (RunResult, error) {
	return l.run(ctx, task, "sync", cfg, func(ctx context.Context, _ *slog.Logger, req Request) (Response, error) {
		return retry.Do(ctx, a.retryCfg, func() (Response, error) {
			cfg.scope.providerAttempts++
			resp, err := a.provider.Invoke(ctx, cloneRequest(req))
			if err != nil && responseHasPartial(resp) {
				resp.completionErr = err
				return resp, nil
			}
			return resp, err
		})
	})
}

// newRunConfig resolves the effective run configuration: the agent's default
// CallOptions and ToolPolicy with each RunOption applied in order.
func (a *Agent) newRunConfig(opts []RunOption) runConfig {
	cfg := runConfig{options: cloneCallOptions(a.defaultCallOptions), toolPolicy: a.toolPolicy.clone(), maxTurns: a.maxSteps, observers: append([]RunObserver(nil), a.observers...)}
	for _, opt := range opts {
		if opt != nil {
			opt(&cfg)
		}
	}
	return cfg
}

// BackgroundResult is delivered on the channel returned by [Agent.RunBackground].
type BackgroundResult struct {
	Result RunResult
	Err    error
}

// RunBackground runs the agent in a goroutine and returns a buffered channel
// that receives the result when the run completes. The channel is closed after
// the single result is sent. Cancel the run via ctx.
func (a *Agent) RunBackground(ctx context.Context, task string, opts ...RunOption) <-chan BackgroundResult {
	ch := make(chan BackgroundResult, 1)
	go func() {
		defer close(ch)
		res, err := a.Run(ctx, task, opts...)
		ch <- BackgroundResult{Result: res, Err: err}
	}()
	return ch
}

// agentTool adapts an [Agent] into a [Tool] so it can be registered on another
// agent as a sub-agent. The schema is derived from the type parameter passed to
// [AsTool] or [AsToolFunc]; task converts the model's raw JSON arguments into
// the task string handed to a fresh run of the wrapped agent (identity for
// AsTool, a typed renderer for AsToolFunc).
type agentTool struct {
	definition ToolDefinition
	agent      *Agent
	task       func(args string) (string, error)
}

func (t *agentTool) Definition() ToolDefinition { return cloneToolDefinition(t.definition) }

func (t *agentTool) Execute(ctx context.Context, args json.RawMessage) (ToolResult, error) {
	task, err := t.task(string(args))
	if err != nil {
		return ToolResult{}, err
	}
	// If an enclosing run is streaming, run the sub-agent in streaming mode too
	// and forward its events into the parent's sink, tagged with this tool's
	// name and this invocation's ID. Both are only stamped when empty so a
	// deeper sub-agent's tags survive (innermost wins). Otherwise fall back to a
	// plain non-streaming run.
	if emit := emitterFrom(ctx); emit != nil {
		invocationID := toolCallIDFrom(ctx)
		res, err := t.agent.RunStream(ctx, task, func(ev StreamEvent) {
			if ev.Agent == "" {
				ev.Agent = t.definition.Name
			}
			if ev.InvocationID == "" {
				ev.InvocationID = invocationID
			}
			emit(ev)
		})
		return BlockResult(res.FinalMessage.Blocks...), err
	}
	res, err := t.agent.Run(ctx, task)
	return BlockResult(res.FinalMessage.Blocks...), err
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
// share conversation state with each other or with the orchestrator. A parent
// run's MaxCalls budget is propagated through ctx and shared atomically with
// nested tool calls; the child may also enforce a stricter local ToolPolicy.
// Timeouts, rate limiters, and parallelism remain local to each configured
// agent, except that a timeout on this AsTool call bounds the complete child run.
func AsTool[P any](a *Agent, name, description string) Tool {
	var zero P
	return &agentTool{
		definition: buildDefinition(name, description, reflect.TypeOf(zero)),
		agent:      a,
		task:       func(args string) (string, error) { return args, nil },
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
		definition: buildDefinition(name, description, reflect.TypeOf(zero)),
		agent:      a,
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
