package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
)

// childTool is a model-facing tool whose calls Runtime admits as durable
// child runs of a pinned definition.
type childTool struct {
	definition ToolDefinition
	child      DefinitionRef
	// defect records a constructor-time validation failure. The declaration is
	// kept so Runtime.Register can reject it with the specific reason instead
	// of a generic shape error.
	defect error
}

// ChildTool declares a model-facing tool that delegates to a registered child
// definition. Each call is admitted as its own durable run linked to the
// parent's call: the parent's worker returns while the child runs, and the
// child's accepted structured output (otherwise its final message) becomes
// the tool result. P must be a struct; its exported fields define the input
// schema the model fills, exactly as for [Func]. The child's task is the
// model's arguments, validated against that schema and passed as JSON, so the
// child's system prompt should describe them.
//
//	researcher, err := runtime.Register("researcher", "v1", researcherAgent)
//	lead, err := core.New(provider, core.AgentConfig{Tools: []core.Tool{
//		core.ChildTool[ResearchInput]("research", "Research one topic.", researcher),
//	}})
//
// The child reference is pinned: admission requires exactly that registered
// revision and never substitutes another. Children share the parent's
// persisted call caps, deadline, and cancellation, and are never retried
// implicitly.
func ChildTool[P any](name, description string, child DefinitionRef) Tool {
	return NewChildTool(buildDefinition(name, description, reflect.TypeFor[P]()), child)
}

// NewChildTool is [ChildTool] with an explicit tool definition, for input
// schemas that are not derived from a Go type. definition.InputSchema must be
// an object schema in the supported subset.
func NewChildTool(definition ToolDefinition, child DefinitionRef) Tool {
	t := &childTool{definition: cloneToolDefinition(definition), child: child}
	t.defect = validateChildDeclaration(t.definition, child)
	return t
}

func (t *childTool) Definition() ToolDefinition { return cloneToolDefinition(t.definition) }

// Execute is never called: Runtime admits child calls as runs instead of
// executing them.
func (t *childTool) Execute(context.Context, json.RawMessage) (ToolResult, error) {
	if t.defect != nil {
		return ToolResult{}, t.defect
	}
	return ToolResult{}, errors.New("child tools are admitted by Runtime, never executed directly")
}

func validateChildDeclaration(definition ToolDefinition, child DefinitionRef) error {
	if definition.Name == "" {
		return errors.New("child tool name is empty")
	}
	if strings.ContainsRune(definition.Name, '\x00') ||
		strings.ContainsRune(child.ID, '\x00') || strings.ContainsRune(child.Revision, '\x00') {
		return errors.New("child tool identities cannot contain NUL")
	}
	if child.ID == "" || child.Revision == "" {
		return errors.New("child tool requires a definition id and revision")
	}
	var schema map[string]any
	if err := decodeSchemaRaw(definition.InputSchema, &schema); err != nil {
		return fmt.Errorf("child tool %q input schema: %w", definition.Name, err)
	}
	if schema == nil || schema["type"] != "object" {
		return fmt.Errorf("child tool %q input schema must be an object schema", definition.Name)
	}
	if err := validateSchema(schema); err != nil {
		return fmt.Errorf("child tool %q input schema: %w", definition.Name, err)
	}
	return nil
}

// maxChildToolWrapperDepth bounds first-party wrapper unwrapping. All first-party
// wrappers embed a single wrapped Tool, so a linear walk terminates; the bound
// keeps a hostile wrapper chain from looping a bounded registration check.
const maxChildToolWrapperDepth = 16

// childDeclaration resolves the child declaration behind a tool through
// first-party wrapper chains, so no wrapper order can hide child semantics
// from Runtime. A nil result with a nil error means an ordinary leaf tool.
func childDeclaration(tool Tool) (*childTool, error) {
	var sawRetry, sawDurableWait, sawEffect bool
	current := tool
	for depth := 0; depth < maxChildToolWrapperDepth; depth++ {
		switch wrapper := current.(type) {
		case frozenTool:
			// New wraps every configured tool in a frozen adapter; child
			// semantics must survive the freeze.
			current = wrapper.executor
		case *retryTool:
			sawRetry = true
			current = wrapper.Tool
		case *legacyErrorTool:
			// Delegation preserves child metadata; child-run failures stay
			// classified by the child run.
			current = wrapper.Tool
		case *durableWaitTool:
			sawDurableWait = true
			current = wrapper.Tool
		case *effectPolicyTool:
			sawEffect = true
			current = wrapper.Tool
		case *childTool:
			if wrapper.defect != nil {
				return nil, wrapper.defect
			}
			// Child execution is owned by the child run and Runtime admission.
			// A retry would repeat the whole child run; a durable wait would
			// double-suspend the parent; an effect policy would bypass the
			// child run's effect ownership.
			if sawRetry {
				return nil, errors.New("child tools cannot be retried: a retry would repeat the child run")
			}
			if sawDurableWait {
				return nil, errors.New("child tools cannot be wrapped in a durable wait: child admission suspends the parent itself")
			}
			if sawEffect {
				return nil, errors.New("child tools cannot carry an effect policy: child effect evidence belongs to the child run")
			}
			return wrapper, nil
		default:
			// Host-attested leaf tool.
			return nil, nil
		}
	}
	return nil, errors.New("tool wrapper chain is too deep")
}

// isChildTool reports whether tool declares a child, rejecting invalid
// declarations and wrapper combinations.
func isChildTool(tool Tool) (bool, error) {
	child, err := childDeclaration(tool)
	return child != nil, err
}
