package core

import (
	"fmt"
	"log/slog"
	"reflect"

	"github.com/emotional-data8482/automata/retry"
	"github.com/emotional-data8482/automata/tracing"
)

// Agent is an immutable executable definition: a provider plus the frozen
// configuration its runs use. It does not run by itself. Register it with a
// [Runtime] under a definition ID and revision, then submit tasks to that
// [DefinitionRef]. Construct a new Agent, and register it as a new revision,
// to change its configuration.
type Agent struct {
	systemPrompt     string
	tools            []Tool
	provider         Provider
	maxTurns         int
	retryCfg         retry.Config
	tracer           tracing.Tracer
	log              *slog.Logger
	callOptions      CallOptions
	toolPolicy       ToolPolicy
	structuredOutput *structuredOutputContract
	preSendHooks     []PreSendHook
}

// DefaultMaxTurns bounds the provider turns of a run when
// AgentConfig.MaxTurns is zero.
const DefaultMaxTurns = 10

// AgentConfig is copied and validated by New. Zero MaxTurns selects
// DefaultMaxTurns; a nil Retry selects retry.DefaultConfig. Nil Logger and
// Tracer select slog.Default and tracing.Noop. Other zero fields disable
// optional behavior. Dependency instances and closures are shared and must
// support concurrent calls; their private state is not cloned.
type AgentConfig struct {
	SystemPrompt string
	Tools        []Tool
	// MaxTurns bounds the provider turns of one run, including
	// structured-output corrections.
	MaxTurns int
	// Retry bounds provider retries within one turn.
	Retry  *retry.Config
	Tracer tracing.Tracer
	Logger *slog.Logger
	// CallOptions are sent on every provider turn.
	CallOptions CallOptions
	// ToolPolicy bounds tool execution; its call caps are pinned on each run
	// at admission.
	ToolPolicy   ToolPolicy
	PreSendHooks []PreSendHook
	// StructuredOutput, when set, requires the final answer as validated
	// structured data (see [StructuredOutputConfig]). The declaration is
	// frozen with the agent, so a registered revision pins it.
	StructuredOutput *StructuredOutputConfig
}

// New validates and freezes the configuration.
func New(p Provider, config AgentConfig) (*Agent, error) {
	if nilDependency(p) {
		return nil, fmt.Errorf("nil provider")
	}
	if config.MaxTurns < 0 {
		return nil, ErrInvalidMaxTurns
	}
	if config.MaxTurns == 0 {
		config.MaxTurns = DefaultMaxTurns
	}
	if err := config.ToolPolicy.validate(); err != nil {
		return nil, err
	}
	if err := validateCallOptions(config.CallOptions); err != nil {
		return nil, err
	}
	frozen, err := freezeTools(config.Tools, "")
	if err != nil {
		return nil, err
	}
	declared, err := validateStructuredOutputDeclaration(config.StructuredOutput)
	if err != nil {
		return nil, err
	}
	a := &Agent{provider: p, systemPrompt: config.SystemPrompt, tools: frozen,
		maxTurns: config.MaxTurns, retryCfg: retry.DefaultConfig(), tracer: config.Tracer,
		log:         config.Logger,
		callOptions: cloneCallOptions(config.CallOptions), toolPolicy: config.ToolPolicy.clone(),
		structuredOutput: declared,
		preSendHooks:     append([]PreSendHook(nil), config.PreSendHooks...)}
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

// newRunConfig resolves a run's configuration from the frozen definition:
// its call options and its declared structured-output contract.
func (a *Agent) newRunConfig() runConfig {
	cfg := runConfig{options: cloneCallOptions(a.callOptions)}
	a.installStructuredOutput(&cfg)
	return cfg
}
