package core

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
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
// committed it and before the run becomes terminal, including suspended runs
// and descendants finalized by cancellation or a waiting deadline. Cancellation
// with unavailable history skips hooks rather than fabricating a snapshot, and
// acknowledged interrupted deliveries are never repeated. Name identifies the
// hook in persisted results. Timeout supplies each attempt's context deadline; zero
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

// CurrentTime returns a [PreSendHook] that tells the model the time, which it
// otherwise does not know. It prefixes the latest user message in the view
// sent to the provider with "Current time: <RFC 3339 time>". Each distinct
// message keeps the time the hook first saw it, so the request prefix stays
// stable across a run's tool turns and provider prompt caches stay warm. The
// canonical transcript is unchanged. A nil now selects [time.Now].
func CurrentTime(now func() time.Time) PreSendHook {
	if now == nil {
		now = time.Now
	}
	var mu sync.Mutex
	stamps := map[string]time.Time{}
	return func(_ context.Context, req Request) (Request, error) {
		for i := len(req.Messages) - 1; i >= 0; i-- {
			message := req.Messages[i]
			if message.Role != "user" {
				continue
			}
			key := hashMessages([]Message{message})
			mu.Lock()
			at, ok := stamps[key]
			if !ok {
				if len(stamps) >= 1024 {
					clear(stamps)
				}
				at = now()
				stamps[key] = at
			}
			mu.Unlock()
			stamp := TextBlock{Text: "Current time: " + at.Format(time.RFC3339) + "\n\n"}
			message.Blocks = append(Blocks{stamp}, message.Blocks...)
			req.Messages = slices.Clone(req.Messages)
			req.Messages[i] = message
			break
		}
		return req, nil
	}
}
