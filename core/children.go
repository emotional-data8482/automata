package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// DurableChildPolicy binds a model-facing child tool to an explicitly
// registered Runtime definition. The referenced definition ID and revision pin
// the executable child binding and its output contract; they are validated
// against Runtime registrations at child admission, never inferred and never
// substituted by the latest revision.
type DurableChildPolicy struct {
	DefinitionID string
	Revision     string
}

type durableChildTool struct {
	definition ToolDefinition
	policy     DurableChildPolicy
	// defect records a constructor-time validation failure. The declaration is
	// kept so Runtime.Register can reject it with the specific reason instead
	// of a generic shape error.
	defect error
}

// DurableChildTool declares a model-facing child tool whose invocations a
// Runtime admits as ordinary durable child runs of the pinned definition. The
// input schema is the frozen ToolDefinition passed here; the task handed to
// the child run is the deterministic projection of the model's arguments that
// the admission boundary computes: the model's raw JSON arguments, validated
// against this frozen schema and forwarded verbatim as the child run's task
// string. There is no optional task callback: the projection must stay
// deterministic so an idempotent admission replay resolves the same child.
//
// The declaration is not executable: Execute always fails, so direct
// process-local Agent and Session runs cannot silently invoke it. Only Runtime
// admission consumes the binding.
func DurableChildTool(definition ToolDefinition, policy DurableChildPolicy) Tool {
	t := &durableChildTool{definition: cloneToolDefinition(definition), policy: policy}
	t.defect = validateDurableChildDeclaration(t.definition, policy)
	return t
}

func (t *durableChildTool) Definition() ToolDefinition { return cloneToolDefinition(t.definition) }

func (t *durableChildTool) Execute(context.Context, json.RawMessage) (ToolResult, error) {
	if t.defect != nil {
		return ToolResult{}, t.defect
	}
	return ToolResult{}, errors.New("durable child tools execute only through Runtime admission, not as process-local tool calls")
}

func validateDurableChildDeclaration(definition ToolDefinition, policy DurableChildPolicy) error {
	if definition.Name == "" {
		return errors.New("durable child tool name is empty")
	}
	if strings.ContainsRune(definition.Name, '\x00') ||
		strings.ContainsRune(policy.DefinitionID, '\x00') || strings.ContainsRune(policy.Revision, '\x00') {
		return errors.New("durable child identities cannot contain NUL")
	}
	if policy.DefinitionID == "" || policy.Revision == "" {
		return errors.New("durable child policy requires a definition id and revision")
	}
	var schema map[string]any
	if err := decodeSchemaRaw(definition.InputSchema, &schema); err != nil {
		return fmt.Errorf("durable child tool %q input schema: %w", definition.Name, err)
	}
	if schema == nil || schema["type"] != "object" {
		return fmt.Errorf("durable child tool %q input schema must be an object schema", definition.Name)
	}
	if err := validateSchema(schema); err != nil {
		return fmt.Errorf("durable child tool %q input schema: %w", definition.Name, err)
	}
	return nil
}

// maxChildToolWrapperDepth bounds first-party wrapper unwrapping. All first-party
// wrappers embed a single wrapped Tool, so a linear walk terminates; the bound
// keeps a hostile wrapper chain from looping a bounded registration check.
const maxChildToolWrapperDepth = 16

// childToolInspection is the metadata walk result for one registered tool.
// transientAdapter marks a detectable process-local child adapter (AsTool or
// AsToolFunc, directly or through first-party wrappers); hasChild marks an
// explicit durable child declaration.
type childToolInspection struct {
	transientAdapter bool
	child            DurableChildPolicy
	hasChild         bool
	// childTool is the resolved durable child declaration, valid only when
	// hasChild is true. It carries the frozen input schema used to validate
	// and project model arguments at admission.
	childTool *durableChildTool
}

// inspectChildTool recognizes durable child metadata and detectable transient
// child adapters through first-party wrapper chains, so no wrapper order can
// hide child semantics from Runtime registration checks.
func inspectChildTool(tool Tool) (childToolInspection, error) {
	var (
		inspection     childToolInspection
		sawRetry       bool
		sawDurableWait bool
		sawEffect      bool
		sawLegacy      bool
	)
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
			sawLegacy = true
			current = wrapper.Tool
		case *durableWaitTool:
			sawDurableWait = true
			current = wrapper.Tool
		case *effectPolicyTool:
			sawEffect = true
			current = wrapper.Tool
		case *agentTool:
			inspection.transientAdapter = true
			return inspection, nil
		case *durableChildTool:
			if wrapper.defect != nil {
				return childToolInspection{}, wrapper.defect
			}
			inspection.hasChild = true
			inspection.child = wrapper.policy
			inspection.childTool = wrapper
			// Child execution is owned by the child run and Runtime admission.
			// A retry would repeat the whole child run; a durable wait would
			// double-suspend the parent; an attached effect policy would bypass
			// the child run's effect ownership; legacy error adaptation would
			// rewrite child-run failures into ordinary tool errors.
			if sawRetry {
				return childToolInspection{}, errors.New("durable child tools cannot be retried: a retry would repeat the child run")
			}
			if sawDurableWait {
				return childToolInspection{}, errors.New("durable child tools cannot be wrapped in a durable wait: child admission suspends the parent itself")
			}
			if sawEffect {
				return childToolInspection{}, errors.New("durable child tools cannot carry an effect policy: child effect evidence belongs to the child run")
			}
			// Legacy error adaptation is preserved by delegation: the wrapper
			// forwards child metadata, and child-run failures stay classified
			// by the child run.
			_ = sawLegacy
			return inspection, nil
		default:
			// Host-attested leaf tool: no child metadata is detectable.
			return inspection, nil
		}
	}
	return childToolInspection{}, errors.New("tool wrapper chain is too deep")
}
