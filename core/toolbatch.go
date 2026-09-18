package core

import (
	"context"
	"errors"
	"sync"

	"github.com/emotional-data8482/automata/tracing"
)

type toolBatchJob struct {
	idx    int
	call   ToolUseBlock
	budget toolBudgetUsage
}

type toolBatchOutcome struct {
	idx   int
	msg   Message
	fatal error
}

// executeToolBatch returns exactly one model-ordered result for every call.
// Budget planning is serial and model-ordered; executable calls are then run
// with optional bounded worker concurrency. Live result events still reflect
// completion order.
func (l *loop) executeToolBatch(
	ctx context.Context,
	calls []ToolUseBlock,
	messages []Message,
	policy *toolPolicyState,
) ([]Message, error) {
	results := make([]Message, len(calls))
	jobs := make([]toolBatchJob, 0, len(calls))

	// Reserve known calls before approval. Planning serially here makes overflow
	// deterministic even though execution and nested-agent runs are concurrent.
	for i, call := range calls {
		var usage toolBudgetUsage
		if _, known := l.toolsByName[call.Name]; known {
			var err error
			usage, err = policy.reserve(call.Name)
			if err != nil {
				results[i] = l.policyDeniedToolResult(ctx, call, usage, err)
				continue
			}
		}
		jobs = append(jobs, toolBatchJob{idx: i, call: call, budget: usage})
	}
	if len(jobs) == 0 {
		return results, nil
	}

	outcomes := make(chan toolBatchOutcome, len(jobs))
	batchCtx, cancelBatch := context.WithCancelCause(ctx)
	defer cancelBatch(nil)

	var fatalOnce sync.Once
	process := func(job toolBatchJob) {
		call := job.call
		if cause := context.Cause(batchCtx); cause != nil {
			// If the parent was already canceled, one queued call owns that fatal
			// outcome. If a sibling canceled the batch, fatalOnce is already spent
			// and this call is only a synthetic cancellation result.
			firstFatal := false
			fatalOnce.Do(func() { firstFatal = true })
			err := batchCtx.Err()
			content := canceledToolResult(cause)
			outcome := toolBatchOutcome{idx: job.idx}
			if firstFatal {
				content = "aborted: tool execution failed"
				outcome.fatal = cause
			}
			blocks := Blocks{TextBlock{Text: content}}
			l.emit(StreamEvent{
				Kind: StreamToolResult, ToolCall: call, Result: content,
				ResultBlocks: blocks, IsError: true, Err: err,
			})
			outcome.msg = ToolResultBlockMessage(call.ID, blocks, true)
			outcomes <- outcome
			return
		}

		result, err := l.safelyExecuteTool(batchCtx, call, messages, policy, job.budget)
		if err == nil {
			outcomes <- toolBatchOutcome{
				idx: job.idx,
				msg: ToolResultBlockMessage(call.ID, result.Blocks, result.IsError),
			}
			return
		}

		firstFatal := false
		fatalOnce.Do(func() {
			firstFatal = true
			cancelBatch(err)
		})

		// Preserve this call's real fatal error. A context error caused by an
		// earlier sibling is represented as synthetic cancellation instead.
		content := "aborted: tool execution failed"
		if !firstFatal && (errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)) {
			content = canceledToolResult(context.Cause(batchCtx))
		}
		blocks := Blocks{TextBlock{Text: content}}
		l.emit(StreamEvent{
			Kind: StreamToolResult, ToolCall: call, Result: content,
			ResultBlocks: blocks, IsError: true, Err: err,
		})
		outcome := toolBatchOutcome{idx: job.idx, msg: ToolResultBlockMessage(call.ID, blocks, true)}
		if firstFatal {
			outcome.fatal = err
		}
		outcomes <- outcome
	}

	workers := len(jobs)
	if max := policy.policy.MaxParallel; max > 0 && workers > max {
		workers = max
	}
	jobQueue := make(chan toolBatchJob, len(jobs))
	for _, job := range jobs {
		jobQueue <- job
	}
	close(jobQueue)

	var wg sync.WaitGroup
	wg.Add(workers)
	for range workers {
		go func() {
			defer wg.Done()
			for job := range jobQueue {
				process(job)
			}
		}()
	}
	wg.Wait()
	close(outcomes)

	var fatalErr error
	for outcome := range outcomes {
		results[outcome.idx] = outcome.msg
		if outcome.fatal != nil && fatalErr == nil {
			fatalErr = outcome.fatal
			l.diagnostics = append(l.diagnostics, RunDiagnostic{ToolCallID: calls[outcome.idx].ID, Kind: "tool_execution_error", Message: outcome.fatal.Error(), Data: append([]byte(nil), calls[outcome.idx].Input...)})
		}
	}
	return results, fatalErr
}

func (l *loop) policyDeniedToolResult(
	ctx context.Context,
	call ToolUseBlock,
	usage toolBudgetUsage,
	err error,
) Message {
	a := l.agent
	_, span := a.tracer.Start(ctx, "tool.execute",
		tracing.String("tool", call.Name),
		tracing.String("policy.outcome", "budget_exhausted"),
		tracing.Int("policy.budget_used", usage.used),
		tracing.Int("policy.budget_max", usage.max),
		tracing.Int("policy.tool_budget_used", usage.toolUsed),
		tracing.Int("policy.tool_budget_max", usage.toolMax),
	)
	span.RecordError(err)
	span.SetStatus(err)
	span.End()

	content := "denied: " + err.Error()
	blocks := Blocks{TextBlock{Text: content}}
	l.log.DebugContext(ctx, "tool call denied by policy", "tool", call.Name, "err", err)
	l.emit(StreamEvent{
		Kind: StreamToolResult, ToolCall: call, Result: content,
		ResultBlocks: blocks, IsError: true, Err: err,
	})
	return ToolResultBlockMessage(call.ID, blocks, true)
}
