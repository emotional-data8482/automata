package core

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// PreSendHook fires once per turn, immediately before the provider invocation.
// It receives the messages and tools to be sent and returns (possibly modified)
// versions. Hooks transform a snapshot of the canonical message history into
// a rendered view sent to the provider — they do NOT mutate the canonical
// history maintained by the run loop.
//
// Returning a non-nil error aborts the run.
type PreSendHook func(context.Context, Request) (Request, error)

// CommittedRunHook observes an execution result after Runtime has durably
// committed it and before the run becomes terminal. Name identifies the hook
// in persisted results. Timeout supplies each attempt's context deadline; zero
// selects 30 seconds. Hook implementations must honor context cancellation.
//
// A hook error is recorded in RunSnapshot.Hooks. It never changes the
// execution result or its error. Runtime does not automatically retry hooks: a
// process loss during delivery is an uncertain external effect and recovery
// moves the run to RuntimeNeedsAttention until the host calls
// [RunHandle.AcknowledgeHooks].
type CommittedRunHook struct {
	Name    string
	Timeout time.Duration
	Handle  func(context.Context, RunSnapshot) error
}

// RunHookResult is the persisted outcome of one committed-run hook attempt.
// An empty Error means the hook completed successfully.
type RunHookResult struct {
	Name  string `json:"name"`
	Error string `json:"error,omitempty"`
	// Unknown marks a hook whose delivery was interrupted and later
	// acknowledged: it may or may not have run. Error is also set.
	Unknown bool `json:"unknown,omitempty"`
}

func validateCommittedRunHooks(hooks []CommittedRunHook) ([]CommittedRunHook, error) {
	seen := make(map[string]struct{}, len(hooks))
	frozen := append([]CommittedRunHook(nil), hooks...)
	for i, hook := range frozen {
		if hook.Name == "" || strings.ContainsRune(hook.Name, '\x00') {
			return nil, fmt.Errorf("runtime hook %d has an invalid name", i)
		}
		if hook.Handle == nil {
			return nil, fmt.Errorf("runtime hook %q has no handler", hook.Name)
		}
		if hook.Timeout < 0 {
			return nil, fmt.Errorf("runtime hook %q has a negative timeout", hook.Name)
		}
		if _, ok := seen[hook.Name]; ok {
			return nil, fmt.Errorf("runtime hook name %q is duplicated", hook.Name)
		}
		seen[hook.Name] = struct{}{}
	}
	return frozen, nil
}
