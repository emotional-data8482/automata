// Package agents is a thin typed layer over [core] for defining agents and
// exposing them as subagent tools. It uses only exported core APIs: every
// helper is shorthand for [core.New], [core.Runtime.Register],
// [core.ChildTool], and [core.Decode].
//
// [Define] builds and registers a definition whose input and output types are
// stated once. The returned [Typed] value runs the definition directly or
// exposes it to a parent agent as a child tool, and both paths hand the
// definition the same JSON task:
//
//	reviewer, err := agents.Define[ReviewRequest, ReviewVerdict](rt, agents.Spec{
//		ID: "reviewer", Revision: "v1", Provider: provider,
//		Config: core.AgentConfig{SystemPrompt: "Review the named artifact."},
//	})
//	lead, err := agents.Register(rt, agents.Spec{
//		ID: "lead", Revision: "v1", Provider: provider,
//		Config: core.AgentConfig{Tools: []core.Tool{
//			reviewer.Tool("review", "Ask the reviewer for a verdict."),
//		}},
//	})
package agents

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"

	"github.com/emotional-data8482/automata/core"
)

// DefaultMaxCorrections is the structured-output correction budget [Define]
// uses when it derives the output contract from O.
const DefaultMaxCorrections = 1

// Spec is everything needed to build and register one definition.
type Spec struct {
	// ID and Revision name the registered definition. Change Revision
	// whenever Config or Provider changes; runs pin the revision they were
	// admitted under.
	ID       string
	Revision string
	Provider core.Provider
	Config   core.AgentConfig
}

// Typed is a registered definition with typed input I and output O. I is a
// struct whose exported fields define the task schema, exactly as for
// [core.ChildTool]. O is either a struct, decoded from the run's validated
// structured output, or string, meaning the run's final text.
type Typed[I, O any] struct{ ref core.DefinitionRef }

// Register builds spec with [core.New] and registers it, returning the pinned
// reference. Use it for definitions without a typed contract, such as a
// top-level agent that takes a free-text task.
func Register(rt *core.Runtime, spec Spec) (core.DefinitionRef, error) {
	if rt == nil {
		return core.DefinitionRef{}, errors.New("agents: nil runtime")
	}
	agent, err := core.New(spec.Provider, spec.Config)
	if err != nil {
		return core.DefinitionRef{}, fmt.Errorf("agents: define %q: %w", spec.ID, err)
	}
	ref, err := rt.Register(spec.ID, spec.Revision, agent)
	if err != nil {
		return core.DefinitionRef{}, fmt.Errorf("agents: register %q: %w", spec.ID, err)
	}
	return ref, nil
}

// Define builds and registers spec with an output contract derived from O:
//
//   - Config.StructuredOutput nil: the contract is O's schema with
//     [DefaultMaxCorrections].
//   - Config.StructuredOutput without a Schema: O's schema is filled in and
//     Native and MaxCorrections are kept.
//   - Config.StructuredOutput with a Schema: rejected, since the schema must
//     come from O.
//   - O is string: no contract; Config.StructuredOutput must be nil.
func Define[I, O any](rt *core.Runtime, spec Spec) (Typed[I, O], error) {
	if t := reflect.TypeFor[I](); t.Kind() != reflect.Struct {
		return Typed[I, O]{}, fmt.Errorf("agents: define %q: input type %s must be a struct", spec.ID, t)
	}
	if textOutput[O]() {
		if spec.Config.StructuredOutput != nil {
			return Typed[I, O]{}, fmt.Errorf("agents: define %q: string output cannot declare StructuredOutput", spec.ID)
		}
	} else {
		if t := reflect.TypeFor[O](); t.Kind() != reflect.Struct {
			return Typed[I, O]{}, fmt.Errorf("agents: define %q: output type %s must be a struct or string", spec.ID, t)
		}
		contract := core.StructuredOutputConfig{MaxCorrections: DefaultMaxCorrections}
		if declared := spec.Config.StructuredOutput; declared != nil {
			if len(declared.Schema) > 0 {
				return Typed[I, O]{}, fmt.Errorf("agents: define %q: StructuredOutput.Schema is derived from the output type; leave it empty", spec.ID)
			}
			contract = *declared
		}
		contract.Schema = core.OutputSchema[O]()
		spec.Config.StructuredOutput = &contract
	}
	ref, err := Register(rt, spec)
	if err != nil {
		return Typed[I, O]{}, err
	}
	return Typed[I, O]{ref: ref}, nil
}

// Ref returns the pinned definition reference.
func (d Typed[I, O]) Ref() core.DefinitionRef { return d.ref }

// Tool exposes the definition to a parent agent as a durable child tool whose
// input schema is derived from I. See [core.ChildTool].
func (d Typed[I, O]) Tool(name, description string) core.Tool {
	return core.ChildTool[I](name, description, d.ref)
}

// Run submits in as the task, JSON-encoded exactly as a child tool call
// delivers it, waits for the run, and returns its output as O. The
// [core.RunResult] is returned on every path, so partial output, transcript,
// and usage survive failures.
func (d Typed[I, O]) Run(ctx context.Context, rt *core.Runtime, in I, opts ...core.SubmitOption) (O, core.RunResult, error) {
	var zero O
	if rt == nil {
		return zero, core.RunResult{}, errors.New("agents: nil runtime")
	}
	task, err := json.Marshal(in)
	if err != nil {
		return zero, core.RunResult{}, fmt.Errorf("agents: encode task: %w", err)
	}
	result, err := rt.Run(ctx, d.ref, string(task), opts...)
	if err != nil {
		return zero, result, err
	}
	out, err := d.Output(result)
	return out, result, err
}

// Output returns a result of this definition as O: the decoded structured
// output, or the final text when O is string. Use it on results the caller
// already holds, such as a child run's RunSnapshot.Result.
func (d Typed[I, O]) Output(result core.RunResult) (O, error) {
	if textOutput[O]() {
		return any(result.Output).(O), nil
	}
	return core.Decode[O](result)
}

// textOutput reports whether O is the plain-text output marker.
func textOutput[O any]() bool { return reflect.TypeFor[O]() == reflect.TypeFor[string]() }
