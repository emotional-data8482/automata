package core

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"time"
)

var (
	// ErrInvalidToolPolicy is returned before a provider call when a tool policy
	// contains a negative timeout, call budget, or parallelism limit.
	ErrInvalidToolPolicy = errors.New("invalid tool policy")

	// ErrToolBudgetExhausted identifies a recoverable tool result produced when
	// a run or per-tool call budget has no capacity remaining.
	ErrToolBudgetExhausted = errors.New("tool budget exhausted")

	// ErrToolTimeout identifies a recoverable tool result produced by an
	// Automata-created per-tool deadline. Parent run cancellation remains fatal.
	ErrToolTimeout = errors.New("tool execution timeout")
)

// RateLimiter is the minimal context-aware contract used by [ToolPolicy].
// Implementations must be safe for concurrent use when the same limiter is
// assigned to a tool that can execute in parallel. For example,
// golang.org/x/time/rate.Limiter satisfies this interface directly.
type RateLimiter interface {
	Wait(ctx context.Context) error
}

// ToolLimits override the default execution controls for one named tool.
// Zero values inherit Timeout and RateLimiter from the enclosing [ToolPolicy];
// MaxCalls == 0 means there is no additional per-tool call budget.
type ToolLimits struct {
	// Timeout bounds the limiter wait plus Execute call. Tools must honor ctx;
	// Go cannot forcibly stop a tool that ignores cancellation.
	Timeout time.Duration
	// MaxCalls caps calls to this tool within one run. Calls reserve budget in
	// model order before approval, and failures still consume their reservation.
	MaxCalls int
	// RateLimiter waits immediately before Execute, using the timeout-wrapped
	// tool context. A nil limiter inherits the policy default.
	RateLimiter RateLimiter
}

// ToolPolicy places deterministic, run-scoped limits around local tool work.
// The zero value preserves the historical behavior: no deadline or call
// budget, no rate limiting, and one goroutine per call in a parallel batch.
//
// A policy belongs to an Agent definition. Its call caps are pinned on each
// run at admission and shared with child runs as persisted subtree caps.
// PerTool entries add a named-tool call budget and may override Timeout and
// RateLimiter.
type ToolPolicy struct {
	// Timeout is the default deadline for each tool execution. It includes a
	// configured rate-limiter wait but not time suspended on a durable wait.
	Timeout time.Duration
	// MaxCalls caps known tool requests in one run. Reservations happen in model
	// order before approval so batch overflow is deterministic. Unknown and
	// terminal structured-output tools do not consume budget.
	MaxCalls int
	// MaxParallel bounds the number of tool-call goroutines started for one
	// batch. Zero retains the previous unbounded batch behavior.
	MaxParallel int
	// RateLimiter is the default limiter for tools without a PerTool override.
	RateLimiter RateLimiter
	// PerTool contains optional limits keyed by Tool.Name().
	PerTool map[string]ToolLimits
}

func (p ToolPolicy) clone() ToolPolicy {
	out := p
	if p.PerTool != nil {
		out.PerTool = make(map[string]ToolLimits, len(p.PerTool))
		maps.Copy(out.PerTool, p.PerTool)
	}
	return out
}

func (p ToolPolicy) validate() error {
	if p.Timeout < 0 {
		return fmt.Errorf("%w: timeout must not be negative", ErrInvalidToolPolicy)
	}
	if p.MaxCalls < 0 {
		return fmt.Errorf("%w: max calls must not be negative", ErrInvalidToolPolicy)
	}
	if p.MaxParallel < 0 {
		return fmt.Errorf("%w: max parallel must not be negative", ErrInvalidToolPolicy)
	}
	for name, limits := range p.PerTool {
		if name == "" {
			return fmt.Errorf("%w: per-tool name must not be empty", ErrInvalidToolPolicy)
		}
		if limits.Timeout < 0 {
			return fmt.Errorf("%w: timeout for %q must not be negative", ErrInvalidToolPolicy, name)
		}
		if limits.MaxCalls < 0 {
			return fmt.Errorf("%w: max calls for %q must not be negative", ErrInvalidToolPolicy, name)
		}
	}
	return nil
}

func (p ToolPolicy) limitsFor(name string) ToolLimits {
	limits := ToolLimits{Timeout: p.Timeout, RateLimiter: p.RateLimiter}
	if override, ok := p.PerTool[name]; ok {
		if override.Timeout > 0 {
			limits.Timeout = override.Timeout
		}
		if override.RateLimiter != nil {
			limits.RateLimiter = override.RateLimiter
		}
		limits.MaxCalls = override.MaxCalls
	}
	return limits
}

// toolBudgetCounter is one persisted cap read into a reservation check. The
// store transaction that charges it makes the check and charge atomic across
// the run's own caps and every capped ancestor.
type toolBudgetCounter struct {
	max  int
	used int
	tool string
}

type toolBudgetUsage struct {
	used     int
	max      int
	toolUsed int
	toolMax  int
}

type storedToolBudget struct {
	// Total counts reservations charged against TotalCap: this run's own known
	// calls plus every call its descendants reserved beneath it. PerTool counts
	// this run's own calls per capped tool.
	Total   int            `json:"total,omitempty"`
	PerTool map[string]int `json:"per_tool,omitempty"`
	// TotalCap and PerToolCap pin the cap values this run was admitted with:
	// TotalCap is the subtree cap every descendant reservation must honor, and
	// PerTool stays run-local. Zero values remain unlimited. Caps persist as
	// values on the record; no context pointers are ever stored.
	TotalCap   int            `json:"total_cap,omitempty"`
	PerToolCap map[string]int `json:"per_tool_cap,omitempty"`
}

// pinnedToolCaps extracts the durable cap values to pin on a run record at
// root or child admission. Zero caps stay unlimited and are omitted.
func pinnedToolCaps(policy ToolPolicy) storedToolBudget {
	var caps storedToolBudget
	if policy.MaxCalls > 0 {
		caps.TotalCap = policy.MaxCalls
	}
	for name, limits := range policy.PerTool {
		if limits.MaxCalls > 0 {
			if caps.PerToolCap == nil {
				caps.PerToolCap = make(map[string]int)
			}
			caps.PerToolCap[name] = limits.MaxCalls
		}
	}
	return caps
}

// chargeBudgetCounters checks every counter, then increments all of them. A
// denial leaves every counter unchanged and reports the exhausted one. Callers
// hold the lock or store transaction that makes the check and charge atomic.
func chargeBudgetCounters(counters []*toolBudgetCounter) (toolBudgetUsage, error) {
	if usage, err := checkBudgetCounters(counters); err != nil {
		return usage, err
	}
	for _, counter := range counters {
		counter.used++
	}
	return toolBudgetUsage{}, nil
}

// checkBudgetCounters reports the first exhausted counter without charging.
func checkBudgetCounters(counters []*toolBudgetCounter) (toolBudgetUsage, error) {
	for _, counter := range counters {
		if counter.used < counter.max {
			continue
		}
		usage := toolBudgetUsage{used: counter.used, max: counter.max}
		if counter.tool != "" {
			usage = toolBudgetUsage{toolUsed: counter.used, toolMax: counter.max}
			return usage, fmt.Errorf("%w for %q: max calls %d", ErrToolBudgetExhausted, counter.tool, counter.max)
		}
		return usage, fmt.Errorf("%w: max calls %d", ErrToolBudgetExhausted, counter.max)
	}
	return toolBudgetUsage{}, nil
}
