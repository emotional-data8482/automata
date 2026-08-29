package core

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/emotional-data8482/automata/retry"
)

type rateLimiterFunc func(context.Context) error

func (f rateLimiterFunc) Wait(ctx context.Context) error { return f(ctx) }

func TestToolPolicyTimeoutIsRecoverable(t *testing.T) {
	provider := &scriptedProvider{turns: []Message{
		asstTool("slow-1", "slow", `{}`),
		asstText("recovered"),
	}}
	agent := New(provider).WithToolPolicy(ToolPolicy{Timeout: 20 * time.Millisecond})
	agent.RegisterTool(Func("slow", "wait for cancellation", func(ctx context.Context, _ struct{}) (string, error) {
		<-ctx.Done()
		return "", ctx.Err()
	}))

	var event StreamEvent
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	result, err := agent.RunStream(ctx, "go", func(ev StreamEvent) {
		if ev.Kind == StreamToolResult {
			event = ev
		}
	})
	if err != nil {
		t.Fatalf("RunStream: %v", err)
	}
	if result.Output != "recovered" {
		t.Fatalf("output = %q, want recovered", result.Output)
	}
	if !event.IsError || !errors.Is(event.Err, ErrToolTimeout) {
		t.Fatalf("timeout event = %+v, want ErrToolTimeout", event)
	}
	if !strings.Contains(event.Result, "timeout: tool \"slow\"") {
		t.Errorf("timeout result = %q", event.Result)
	}
	results := transcriptToolResults(result.Messages)
	if len(results) != 1 || !results[0].IsError {
		t.Fatalf("transcript results = %+v", results)
	}
}

func TestToolPolicyTimeoutKeepsSiblingOutcome(t *testing.T) {
	provider := &scriptedProvider{turns: []Message{
		AssistantMessage(toolUse("slow-1", "slow", `{}`), toolUse("fast-1", "fast", `{}`)),
		asstText("done"),
	}}
	agent := New(provider).WithToolPolicy(ToolPolicy{Timeout: 20 * time.Millisecond})
	agent.RegisterTool(Func("slow", "wait for timeout", func(ctx context.Context, _ struct{}) (string, error) {
		<-ctx.Done()
		return "", ctx.Err()
	}))
	agent.RegisterTool(Func("fast", "finish before timeout", func(context.Context, struct{}) (string, error) {
		return "fast result", nil
	}))

	result, err := agent.Run(context.Background(), "go")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	results := transcriptToolResults(result.Messages)
	if len(results) != 2 || results[0].ToolUseID != "slow-1" || results[1].ToolUseID != "fast-1" {
		t.Fatalf("results = %+v", results)
	}
	if !results[0].IsError || results[1].IsError {
		t.Errorf("timeout sibling errors = [%v %v], want [true false]", results[0].IsError, results[1].IsError)
	}
	if got := (Message{Blocks: results[1].Content}).Text(); got != "fast result" {
		t.Errorf("fast result = %q", got)
	}
}

func TestToolPolicyParentDeadlineRemainsFatal(t *testing.T) {
	provider := &scriptedProvider{turns: []Message{asstTool("slow-1", "slow", `{}`)}}
	agent := New(provider).WithToolPolicy(ToolPolicy{Timeout: time.Second})
	agent.RegisterTool(Func("slow", "wait for cancellation", func(ctx context.Context, _ struct{}) (string, error) {
		<-ctx.Done()
		return "", ctx.Err()
	}))

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	result, err := agent.Run(ctx, "go")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want parent deadline", err)
	}
	if got := transcriptToolResults(result.Messages); len(got) != 1 || !got[0].IsError {
		t.Fatalf("fatal timeout transcript = %+v", got)
	}
}

func TestToolPolicyBudgetDeniesBatchOverflowInModelOrder(t *testing.T) {
	provider := &scriptedProvider{turns: []Message{
		AssistantMessage(
			toolUse("c1", "work", `{}`),
			toolUse("c2", "work", `{}`),
			toolUse("c3", "work", `{}`),
		),
		asstText("done"),
	}}
	var executed atomic.Int64
	agent := New(provider).WithToolPolicy(ToolPolicy{MaxCalls: 2})
	agent.RegisterTool(Func("work", "count calls", func(context.Context, struct{}) (string, error) {
		executed.Add(1)
		return "ok", nil
	}))

	result, err := agent.Run(context.Background(), "go")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := executed.Load(); got != 2 {
		t.Fatalf("executed = %d, want 2", got)
	}
	results := transcriptToolResults(result.Messages)
	if len(results) != 3 {
		t.Fatalf("results = %+v", results)
	}
	for i, id := range []string{"c1", "c2", "c3"} {
		if results[i].ToolUseID != id {
			t.Errorf("result %d ID = %q, want %q", i, results[i].ToolUseID, id)
		}
	}
	if results[0].IsError || results[1].IsError || !results[2].IsError {
		t.Errorf("result errors = [%v %v %v], want [false false true]", results[0].IsError, results[1].IsError, results[2].IsError)
	}
	if got := (Message{Blocks: results[2].Content}).Text(); got != "denied: tool budget exhausted: max calls 2" {
		t.Errorf("budget denial = %q", got)
	}
}

func TestToolPolicyBudgetPersistsAcrossProviderSteps(t *testing.T) {
	provider := &scriptedProvider{turns: []Message{
		asstTool("c1", "work", `{}`),
		asstTool("c2", "work", `{}`),
		asstText("done"),
	}}
	var executed atomic.Int64
	agent := New(provider).WithToolPolicy(ToolPolicy{MaxCalls: 1})
	agent.RegisterTool(Func("work", "count calls", func(context.Context, struct{}) (string, error) {
		executed.Add(1)
		return "ok", nil
	}))

	result, err := agent.Run(context.Background(), "go")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := executed.Load(); got != 1 {
		t.Fatalf("executed = %d, want 1", got)
	}
	results := transcriptToolResults(result.Messages)
	if len(results) != 2 || results[0].IsError || !results[1].IsError {
		t.Errorf("cross-step results = %+v", results)
	}
}

func TestToolPolicyPerToolBudgetDoesNotLimitOtherTools(t *testing.T) {
	provider := &scriptedProvider{turns: []Message{
		AssistantMessage(
			toolUse("w1", "work", `{}`),
			toolUse("w2", "work", `{}`),
			toolUse("o1", "other", `{}`),
		),
		asstText("done"),
	}}
	var workCalls atomic.Int64
	var otherCalls atomic.Int64
	agent := New(provider).WithToolPolicy(ToolPolicy{PerTool: map[string]ToolLimits{
		"work": {MaxCalls: 1},
	}})
	agent.RegisterTool(Func("work", "limited calls", func(context.Context, struct{}) (string, error) {
		workCalls.Add(1)
		return "work", nil
	}))
	agent.RegisterTool(Func("other", "unlimited calls", func(context.Context, struct{}) (string, error) {
		otherCalls.Add(1)
		return "other", nil
	}))

	result, err := agent.Run(context.Background(), "go")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if workCalls.Load() != 1 || otherCalls.Load() != 1 {
		t.Fatalf("work=%d other=%d, want 1/1", workCalls.Load(), otherCalls.Load())
	}
	results := transcriptToolResults(result.Messages)
	if !results[1].IsError || results[2].IsError {
		t.Errorf("results = %+v", results)
	}
	if got := (Message{Blocks: results[1].Content}).Text(); got != "denied: tool budget exhausted for \"work\": max calls 1" {
		t.Errorf("per-tool denial = %q", got)
	}
}

func TestToolPolicyBudgetReservationPrecedesApproval(t *testing.T) {
	provider := &scriptedProvider{turns: []Message{
		AssistantMessage(toolUse("c1", "work", `{}`), toolUse("c2", "work", `{}`)),
		asstText("done"),
	}}
	var approvals atomic.Int64
	var executions atomic.Int64
	agent := New(provider).
		WithToolPolicy(ToolPolicy{MaxCalls: 1}).
		WithApprover(ApproverFunc(func(_ context.Context, _ ToolUseBlock, _ []Message) (Decision, error) {
			approvals.Add(1)
			return Decision{Outcome: Deny, Reason: "operator denied"}, nil
		}))
	agent.RegisterTool(Func("work", "never executes", func(context.Context, struct{}) (string, error) {
		executions.Add(1)
		return "unexpected", nil
	}))

	result, err := agent.Run(context.Background(), "go")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := approvals.Load(); got != 1 {
		t.Errorf("approvals = %d, want 1 (overflow is rejected before approval)", got)
	}
	if got := executions.Load(); got != 0 {
		t.Errorf("executions = %d, want 0", got)
	}
	results := transcriptToolResults(result.Messages)
	if got := (Message{Blocks: results[0].Content}).Text(); got != "denied: operator denied" {
		t.Errorf("first result = %q", got)
	}
	if got := (Message{Blocks: results[1].Content}).Text(); !strings.Contains(got, "tool budget exhausted") {
		t.Errorf("second result = %q", got)
	}
}

func TestRunToolPolicyReplacesAgentDefaultAndBudgetsReset(t *testing.T) {
	batch := AssistantMessage(toolUse("c1", "work", `{}`), toolUse("c2", "work", `{}`))
	provider := &scriptedProvider{turns: []Message{batch, asstText("one"), batch, asstText("two")}}
	var executed atomic.Int64
	agent := New(provider).WithToolPolicy(ToolPolicy{MaxCalls: 1})
	agent.RegisterTool(Func("work", "count calls", func(context.Context, struct{}) (string, error) {
		executed.Add(1)
		return "ok", nil
	}))

	first, err := agent.Run(context.Background(), "first", WithToolPolicy(ToolPolicy{MaxCalls: 2}))
	if err != nil {
		t.Fatalf("first Run: %v", err)
	}
	second, err := agent.Run(context.Background(), "second")
	if err != nil {
		t.Fatalf("second Run: %v", err)
	}
	if got := executed.Load(); got != 3 {
		t.Fatalf("executed across runs = %d, want 3", got)
	}
	if results := transcriptToolResults(first.Messages); results[0].IsError || results[1].IsError {
		t.Errorf("per-run replacement did not allow both calls: %+v", results)
	}
	if results := transcriptToolResults(second.Messages); results[0].IsError || !results[1].IsError {
		t.Errorf("agent default was not restored/reset: %+v", results)
	}
}

func TestToolPolicyRateLimiterDoesNotThrottleOtherTools(t *testing.T) {
	provider := &scriptedProvider{turns: []Message{
		AssistantMessage(toolUse("l1", "limited", `{}`), toolUse("f1", "free", `{}`)),
		asstText("done"),
	}}
	waitStarted := make(chan struct{})
	release := make(chan struct{})
	limitedDone := make(chan struct{})
	freeDone := make(chan struct{})
	limiter := rateLimiterFunc(func(ctx context.Context) error {
		close(waitStarted)
		select {
		case <-release:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	})
	agent := New(provider).WithToolPolicy(ToolPolicy{PerTool: map[string]ToolLimits{
		"limited": {RateLimiter: limiter},
	}})
	agent.RegisterTool(Func("limited", "limited work", func(context.Context, struct{}) (string, error) {
		close(limitedDone)
		return "limited", nil
	}))
	agent.RegisterTool(Func("free", "unlimited work", func(context.Context, struct{}) (string, error) {
		close(freeDone)
		return "free", nil
	}))

	done := make(chan error, 1)
	go func() {
		_, err := agent.Run(context.Background(), "go")
		done <- err
	}()
	select {
	case <-waitStarted:
	case <-time.After(time.Second):
		t.Fatal("limiter did not start")
	}
	select {
	case <-freeDone:
	case <-time.After(time.Second):
		t.Fatal("unrelated tool was throttled")
	}
	select {
	case <-limitedDone:
		t.Fatal("limited tool executed before limiter allowed it")
	default:
	}
	close(release)
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("run did not finish")
	}
}

func TestToolPolicyTimeoutIncludesRateLimiterWait(t *testing.T) {
	provider := &scriptedProvider{turns: []Message{
		asstTool("l1", "limited", `{}`),
		asstText("recovered"),
	}}
	var executed atomic.Bool
	limiter := rateLimiterFunc(func(ctx context.Context) error {
		<-ctx.Done()
		return ctx.Err()
	})
	agent := New(provider).WithToolPolicy(ToolPolicy{PerTool: map[string]ToolLimits{
		"limited": {Timeout: 20 * time.Millisecond, RateLimiter: limiter},
	}})
	agent.RegisterTool(Func("limited", "never reaches execute", func(context.Context, struct{}) (string, error) {
		executed.Store(true)
		return "unexpected", nil
	}))

	var event StreamEvent
	result, err := agent.RunStream(context.Background(), "go", func(ev StreamEvent) {
		if ev.Kind == StreamToolResult {
			event = ev
		}
	})
	if err != nil {
		t.Fatalf("RunStream: %v", err)
	}
	if result.Output != "recovered" || executed.Load() {
		t.Fatalf("output=%q executed=%v", result.Output, executed.Load())
	}
	if !errors.Is(event.Err, ErrToolTimeout) {
		t.Fatalf("event error = %v, want ErrToolTimeout", event.Err)
	}
}

func TestToolPolicyRateLimiterErrorIsRecoverable(t *testing.T) {
	provider := &scriptedProvider{turns: []Message{
		asstTool("l1", "limited", `{}`),
		asstText("recovered"),
	}}
	var executed atomic.Bool
	limiterErr := errors.New("quota service unavailable")
	agent := New(provider).WithToolPolicy(ToolPolicy{RateLimiter: rateLimiterFunc(func(context.Context) error {
		return limiterErr
	})})
	agent.RegisterTool(Func("limited", "never reaches execute", func(context.Context, struct{}) (string, error) {
		executed.Store(true)
		return "unexpected", nil
	}))

	result, err := agent.Run(context.Background(), "go")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if executed.Load() {
		t.Fatal("tool executed after limiter failure")
	}
	results := transcriptToolResults(result.Messages)
	if got := (Message{Blocks: results[0].Content}).Text(); got != "rate limit: "+limiterErr.Error() {
		t.Errorf("limiter result = %q", got)
	}
}

func TestToolPolicyMaxParallel(t *testing.T) {
	const calls = 6
	blocks := make([]Block, 0, calls)
	for i := range calls {
		blocks = append(blocks, toolUse(string(rune('a'+i)), "work", `{}`))
	}
	provider := &scriptedProvider{turns: []Message{AssistantMessage(blocks...), asstText("done")}}
	started := make(chan struct{}, calls)
	release := make(chan struct{})
	var active atomic.Int64
	var maxActive atomic.Int64
	agent := New(provider).WithToolPolicy(ToolPolicy{MaxParallel: 2})
	agent.RegisterTool(Func("work", "bounded work", func(context.Context, struct{}) (string, error) {
		current := active.Add(1)
		for {
			seen := maxActive.Load()
			if current <= seen || maxActive.CompareAndSwap(seen, current) {
				break
			}
		}
		started <- struct{}{}
		<-release
		active.Add(-1)
		return "ok", nil
	}))

	done := make(chan error, 1)
	go func() {
		_, err := agent.Run(context.Background(), "go")
		done <- err
	}()
	for range 2 {
		select {
		case <-started:
		case <-time.After(time.Second):
			t.Fatal("expected two concurrent tools")
		}
	}
	select {
	case <-started:
		t.Fatal("third tool started while both worker slots were occupied")
	default:
	}
	close(release)
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("bounded batch did not finish")
	}
	if got := maxActive.Load(); got != 2 {
		t.Errorf("max active = %d, want 2", got)
	}
}

func TestToolPolicyWithToolRetryUsesOneBudgetReservation(t *testing.T) {
	provider := &scriptedProvider{turns: []Message{
		AssistantMessage(toolUse("c1", "work", `{}`), toolUse("c2", "work", `{}`)),
		asstText("done"),
	}}
	var attempts atomic.Int64
	agent := New(provider).WithToolPolicy(ToolPolicy{MaxCalls: 1})
	tool := Func("work", "retry transient work", func(context.Context, struct{}) (string, error) {
		if attempts.Add(1) == 1 {
			return "", errors.New("transient")
		}
		return "ok", nil
	})
	agent.RegisterTool(WithToolRetry(tool, retry.Config{MaxAttempts: 2, RetryUnknown: true}))

	result, err := agent.Run(context.Background(), "go")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := attempts.Load(); got != 2 {
		t.Fatalf("physical attempts = %d, want 2", got)
	}
	results := transcriptToolResults(result.Messages)
	if results[0].IsError || !results[1].IsError {
		t.Errorf("results = %+v, retry should consume one logical call", results)
	}
}

type nestedBudgetProvider struct{}

func (nestedBudgetProvider) Invoke(_ context.Context, req Request) (Response, error) {
	for _, message := range req.Messages {
		if message.Role == "tool" {
			return Response{Message: asstText("child done")}, nil
		}
	}
	return Response{Message: asstTool("leaf-1", "leaf", `{}`)}, nil
}

func TestToolPolicyBudgetIsSharedAcrossConcurrentAsToolRuns(t *testing.T) {
	child := New(nestedBudgetProvider{})
	var leafCalls atomic.Int64
	child.RegisterTool(Func("leaf", "nested work", func(context.Context, struct{}) (string, error) {
		leafCalls.Add(1)
		return "leaf done", nil
	}))

	parentProvider := &scriptedProvider{turns: []Message{
		AssistantMessage(toolUse("sub-1", "child", `{}`), toolUse("sub-2", "child", `{}`)),
		asstText("parent done"),
	}}
	parent := New(parentProvider).WithToolPolicy(ToolPolicy{MaxCalls: 3})
	parent.RegisterTool(AsTool[struct{}](child, "child", "delegate"))

	result, err := parent.Run(context.Background(), "go")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.Output != "parent done" {
		t.Errorf("output = %q", result.Output)
	}
	// The two AsTool calls consume two slots; their concurrent child runs race
	// for the one remaining shared slot, and exactly one nested tool may run.
	if got := leafCalls.Load(); got != 1 {
		t.Fatalf("nested leaf calls = %d, want 1", got)
	}
}

func TestToolPolicyNestedBudgetStreamEventKeepsSubAgentTags(t *testing.T) {
	child := New(nestedBudgetProvider{})
	var leafCalls atomic.Int64
	child.RegisterTool(Func("leaf", "nested work", func(context.Context, struct{}) (string, error) {
		leafCalls.Add(1)
		return "unexpected", nil
	}))

	parentProvider := &scriptedProvider{turns: []Message{
		asstTool("sub-1", "child", `{}`),
		asstText("parent done"),
	}}
	parent := New(parentProvider).WithToolPolicy(ToolPolicy{MaxCalls: 1})
	parent.RegisterTool(AsTool[struct{}](child, "child", "delegate"))

	var budgetEvent StreamEvent
	_, err := parent.RunStream(context.Background(), "go", func(ev StreamEvent) {
		if ev.Kind == StreamToolResult && errors.Is(ev.Err, ErrToolBudgetExhausted) {
			budgetEvent = ev
		}
	})
	if err != nil {
		t.Fatalf("RunStream: %v", err)
	}
	if leafCalls.Load() != 0 {
		t.Fatalf("leaf executed %d times, want 0", leafCalls.Load())
	}
	if budgetEvent.Agent != "child" || budgetEvent.InvocationID != "sub-1" {
		t.Errorf("nested budget tags = agent %q invocation %q", budgetEvent.Agent, budgetEvent.InvocationID)
	}
}

type terminalBatchValue struct {
	Answer string `json:"answer"`
}

func TestTerminalToolProducesResultsForEverySiblingCall(t *testing.T) {
	provider := &scriptedProvider{turns: []Message{
		AssistantMessage(
			toolUse("w1", "work", `{}`),
			toolUse("s1", structuredOutputToolName, `{"answer":"done"}`),
		),
	}}
	var executed atomic.Bool
	agent := New(provider)
	agent.RegisterTool(Func("work", "must not execute", func(context.Context, struct{}) (string, error) {
		executed.Store(true)
		return "unexpected", nil
	}))

	value, result, err := RunTyped[terminalBatchValue](context.Background(), agent, "go")
	if err != nil {
		t.Fatalf("RunTyped: %v", err)
	}
	if value.Answer != "done" || executed.Load() {
		t.Fatalf("value=%+v executed=%v", value, executed.Load())
	}
	results := transcriptToolResults(result.Messages)
	if len(results) != 2 || results[0].ToolUseID != "w1" || results[1].ToolUseID != "s1" {
		t.Fatalf("terminal batch results = %+v", results)
	}
	if !results[0].IsError || results[1].IsError {
		t.Errorf("terminal batch errors = [%v %v], want [true false]", results[0].IsError, results[1].IsError)
	}
}

func TestToolPolicyInvalidConfigurationFailsBeforeProviderCall(t *testing.T) {
	provider := &capturingProvider{turns: []Message{asstText("unexpected")}}
	agent := New(provider).WithToolPolicy(ToolPolicy{MaxParallel: -1})
	result, err := agent.Run(context.Background(), "go")
	if !errors.Is(err, ErrInvalidToolPolicy) {
		t.Fatalf("err = %v, want ErrInvalidToolPolicy", err)
	}
	if provider.calls != 0 {
		t.Fatalf("provider calls = %d, want 0", provider.calls)
	}
	if len(result.Messages) != 0 {
		t.Errorf("messages = %+v, want untouched transcript", result.Messages)
	}
}
