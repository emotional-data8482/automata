package core

import (
	"encoding/json"
	"maps"
	"slices"
	"time"
)

// AgentManifest is the declarative description of an Agent definition: what
// a model sees and how Runtime executes each tool. It holds no provider,
// closures, or credentials, and encodes to JSON, so registries, consoles,
// and deploy checks can compare revisions without running them.
type AgentManifest struct {
	SystemPrompt     string                    `json:"system_prompt,omitempty"`
	MaxTurns         int                       `json:"max_turns"`
	Tools            []ToolManifest            `json:"tools,omitempty"`
	ToolPolicy       ToolPolicyManifest        `json:"tool_policy"`
	StructuredOutput *StructuredOutputManifest `json:"structured_output,omitempty"`
	// Children lists the distinct child definitions the tools delegate to,
	// in tool order. Each must be registered before runs that call it.
	Children []DefinitionRef `json:"children,omitempty"`
}

// ToolManifest describes one tool binding.
type ToolManifest struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"input_schema,omitempty"`
	// Effect is the declared recovery contract; EffectScope and
	// SemanticGuard describe a mutating tool's semantic duplicate guard.
	Effect        ToolEffectKind `json:"effect,omitempty"`
	EffectScope   string         `json:"effect_scope,omitempty"`
	SemanticGuard bool           `json:"semantic_guard,omitempty"`
	// Retry reports that the binding retries failed executions in place.
	Retry bool `json:"retry,omitempty"`
	// Wait is set when a call suspends its run on a durable question or
	// approval.
	Wait *WaitManifest `json:"wait,omitempty"`
	// Child is set when calls are admitted as durable child runs.
	Child *DefinitionRef `json:"child,omitempty"`
}

// WaitManifest describes a tool's durable wait policy.
type WaitManifest struct {
	Kind          WaitKind      `json:"kind"`
	PolicyContext string        `json:"policy_context,omitempty"`
	ExpiresAfter  time.Duration `json:"expires_after,omitempty"`
}

// ToolPolicyManifest describes the definition's tool limits. Rate limiters
// are reported only as present.
type ToolPolicyManifest struct {
	Timeout     time.Duration                 `json:"timeout,omitempty"`
	MaxCalls    int                           `json:"max_calls,omitempty"`
	MaxParallel int                           `json:"max_parallel,omitempty"`
	RateLimited bool                          `json:"rate_limited,omitempty"`
	PerTool     map[string]ToolLimitsManifest `json:"per_tool,omitempty"`
}

// ToolLimitsManifest describes one tool's limit overrides.
type ToolLimitsManifest struct {
	Timeout     time.Duration `json:"timeout,omitempty"`
	MaxCalls    int           `json:"max_calls,omitempty"`
	RateLimited bool          `json:"rate_limited,omitempty"`
}

// StructuredOutputManifest describes a declared structured final answer.
type StructuredOutputManifest struct {
	Schema         json.RawMessage `json:"schema"`
	Native         bool            `json:"native,omitempty"`
	MaxCorrections int             `json:"max_corrections,omitempty"`
}

// Describe returns the definition's manifest. Each call returns a fresh
// copy; tools appear in the order the model sees them.
func (a *Agent) Describe() AgentManifest {
	manifest := AgentManifest{
		SystemPrompt: a.systemPrompt,
		MaxTurns:     a.maxTurns,
		ToolPolicy: ToolPolicyManifest{
			Timeout: a.toolPolicy.Timeout, MaxCalls: a.toolPolicy.MaxCalls,
			MaxParallel: a.toolPolicy.MaxParallel, RateLimited: a.toolPolicy.RateLimiter != nil,
		},
	}
	if len(a.toolPolicy.PerTool) > 0 {
		manifest.ToolPolicy.PerTool = make(map[string]ToolLimitsManifest, len(a.toolPolicy.PerTool))
		for _, name := range slices.Sorted(maps.Keys(a.toolPolicy.PerTool)) {
			limits := a.toolPolicy.PerTool[name]
			manifest.ToolPolicy.PerTool[name] = ToolLimitsManifest{
				Timeout: limits.Timeout, MaxCalls: limits.MaxCalls, RateLimited: limits.RateLimiter != nil,
			}
		}
	}
	if a.structuredOutput != nil {
		config := a.structuredOutput.config
		manifest.StructuredOutput = &StructuredOutputManifest{
			Schema: append(json.RawMessage(nil), config.Schema...), Native: config.Native, MaxCorrections: config.MaxCorrections,
		}
	}
	for _, tool := range a.tools {
		entry := describeTool(tool)
		if entry.Child != nil && !slices.Contains(manifest.Children, *entry.Child) {
			manifest.Children = append(manifest.Children, *entry.Child)
		}
		manifest.Tools = append(manifest.Tools, entry)
	}
	return manifest
}

func describeTool(tool Tool) ToolManifest {
	definition := tool.Definition()
	entry := ToolManifest{
		Name: definition.Name, Description: definition.Description,
		InputSchema: append(json.RawMessage(nil), definition.InputSchema...),
	}
	if child, err := childDeclaration(tool); err == nil && child != nil {
		ref := child.child
		entry.Child = &ref
		return entry
	}
	if policy, err := effectPolicyFor(tool); err == nil {
		entry.Effect, entry.EffectScope, entry.SemanticGuard = policy.Kind, policy.Scope, policy.SemanticKey != nil
	}
	if policy, configured, err := waitPolicyFor(tool); err == nil && configured {
		entry.Wait = &WaitManifest{Kind: policy.Kind, PolicyContext: policy.PolicyContext, ExpiresAfter: policy.ExpiresAfter}
	}
	entry.Retry = retriedTool(tool)
	return entry
}

// retriedTool reports a retry wrapper anywhere in a first-party wrapper
// chain.
func retriedTool(tool Tool) bool {
	current := tool
	for range maxChildToolWrapperDepth {
		switch wrapper := current.(type) {
		case frozenTool:
			current = wrapper.executor
		case *retryTool:
			return true
		case *legacyErrorTool:
			current = wrapper.Tool
		case *durableWaitTool:
			current = wrapper.Tool
		case *effectPolicyTool:
			current = wrapper.Tool
		default:
			return false
		}
	}
	return false
}

// Describe returns the manifest of a registered definition revision, or
// [ErrDefinitionNotRegistered].
func (r *Runtime) Describe(ref DefinitionRef) (AgentManifest, error) {
	binding, err := r.binding(ref)
	if err != nil {
		return AgentManifest{}, err
	}
	return binding.agent.Describe(), nil
}
