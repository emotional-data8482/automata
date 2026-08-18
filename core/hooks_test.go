package core

import (
	"context"
	"errors"
	"reflect"
	"testing"
)

type failingProvider struct {
	err        error
	contextErr bool
}

func (p failingProvider) Invoke(ctx context.Context, _ Request) (Response, error) {
	if p.contextErr {
		return Response{}, ctx.Err()
	}
	return Response{}, p.err
}

func TestPostRunHooksRunInOrderAndJoinErrors(t *testing.T) {
	firstErr := errors.New("first checkpoint failed")
	secondErr := errors.New("second checkpoint failed")
	type contextKey string
	const key contextKey = "checkpoint"
	ctx, cancel := context.WithCancel(context.WithValue(context.Background(), key, "value"))
	cancel()

	var order []int
	var seenResults []RunResult
	makeHook := func(index int, hookErr error) RunOption {
		return WithPostRunHook(func(hookCtx context.Context, result RunResult, runErr error) error {
			order = append(order, index)
			seenResults = append(seenResults, result)
			if runErr != nil {
				t.Errorf("hook %d runErr = %v, want nil", index, runErr)
			}
			if hookCtx.Err() != nil {
				t.Errorf("hook %d context Err = %v, want detached context", index, hookCtx.Err())
			}
			if _, ok := hookCtx.Deadline(); ok {
				t.Errorf("hook %d context retained a deadline", index)
			}
			if got := hookCtx.Value(key); got != "value" {
				t.Errorf("hook %d context value = %v, want value", index, got)
			}
			return hookErr
		})
	}

	result, err := New(&scriptedProvider{turns: []Message{asstText("done")}}).Run(
		ctx,
		"go",
		makeHook(1, firstErr),
		makeHook(2, secondErr),
	)
	if !errors.Is(err, firstErr) || !errors.Is(err, secondErr) {
		t.Fatalf("err = %v, want both checkpoint errors", err)
	}
	if !reflect.DeepEqual(order, []int{1, 2}) {
		t.Errorf("hook order = %v, want [1 2]", order)
	}
	if result.Output != "done" || len(seenResults) != 2 {
		t.Fatalf("result/output snapshots = %q / %d, want done / 2", result.Output, len(seenResults))
	}
	for i, seen := range seenResults {
		if seen.Output != result.Output || !reflect.DeepEqual(seen.Messages, result.Messages) {
			t.Errorf("hook %d saw result %+v, want %+v", i, seen, result)
		}
	}
}

func TestSessionPostRunHookSeesCommittedMaxStepResult(t *testing.T) {
	checkpointErr := errors.New("checkpoint unavailable")
	provider := &optionsProvider{turns: []Message{
		withUsage(asstTool("c1", "echo", `{}`), &Usage{InputTokens: 7, OutputTokens: 3}),
	}}
	agent := New(provider).WithMaxSteps(1)
	agent.RegisterTool(Func("echo", "echoes", func(context.Context, echoArgs) (string, error) {
		return "ok", nil
	}))
	session := agent.NewSession()
	called := false

	result, err := session.Run(context.Background(), "go", WithPostRunHook(
		func(_ context.Context, hookResult RunResult, runErr error) error {
			called = true
			if !errors.Is(runErr, ErrMaxStepsExceeded) {
				t.Errorf("hook runErr = %v, want ErrMaxStepsExceeded", runErr)
			}
			if !reflect.DeepEqual(session.Messages(), hookResult.Messages) {
				t.Errorf("session was not committed before hook:\n session=%+v\n result=%+v", session.Messages(), hookResult.Messages)
			}
			if hookResult.Usage != (Usage{InputTokens: 7, OutputTokens: 3}) {
				t.Errorf("hook usage = %+v, want {7 3}", hookResult.Usage)
			}
			return checkpointErr
		},
	))
	if !called {
		t.Fatal("post-run hook was not called")
	}
	if !errors.Is(err, ErrMaxStepsExceeded) || !errors.Is(err, checkpointErr) {
		t.Fatalf("err = %v, want max-step and checkpoint errors", err)
	}
	if result.StopReason != StopMaxSteps || result.Steps != 1 {
		t.Errorf("partial result = %+v, want one-step StopMaxSteps", result)
	}
	if !reflect.DeepEqual(session.Messages(), result.Messages) {
		t.Errorf("returned result and committed session differ")
	}
}

func TestPostRunHookRunsAfterProviderAndCancellationFailures(t *testing.T) {
	providerErr := errors.New("provider unavailable")
	tests := []struct {
		name     string
		provider Provider
		ctx      func() context.Context
		wantErr  error
		wantStop StopReason
	}{
		{
			name:     "provider",
			provider: failingProvider{err: providerErr},
			ctx:      context.Background,
			wantErr:  providerErr,
			wantStop: StopError,
		},
		{
			name:     "cancellation",
			provider: failingProvider{contextErr: true},
			ctx: func() context.Context {
				ctx, cancel := context.WithCancel(context.Background())
				cancel()
				return ctx
			},
			wantErr:  context.Canceled,
			wantStop: StopCancelled,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			called := false
			result, err := New(tt.provider).Run(tt.ctx(), "go", WithPostRunHook(
				func(hookCtx context.Context, hookResult RunResult, runErr error) error {
					called = true
					if !errors.Is(runErr, tt.wantErr) {
						t.Errorf("hook runErr = %v, want %v", runErr, tt.wantErr)
					}
					if hookCtx.Err() != nil {
						t.Errorf("hook context Err = %v, want detached context", hookCtx.Err())
					}
					if len(hookResult.Messages) != 1 || hookResult.Messages[0].Role != "user" {
						t.Errorf("hook partial transcript = %+v, want user task", hookResult.Messages)
					}
					return nil
				},
			))
			if !called {
				t.Fatal("post-run hook was not called")
			}
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("err = %v, want %v", err, tt.wantErr)
			}
			if result.StopReason != tt.wantStop || len(result.Messages) != 1 {
				t.Errorf("partial result = %+v, want stop %q and transcript", result, tt.wantStop)
			}
		})
	}
}

func TestSessionRunStreamInvokesPostRunHookAfterCommit(t *testing.T) {
	provider := &scriptedStreamProvider{turns: [][]StreamChunk{textChunks("done")}}
	session := New(provider).NewSession()
	called := false

	result, err := session.RunStream(context.Background(), "go", nil, WithPostRunHook(
		func(_ context.Context, hookResult RunResult, runErr error) error {
			called = true
			if runErr != nil {
				t.Errorf("hook runErr = %v, want nil", runErr)
			}
			if !reflect.DeepEqual(session.Messages(), hookResult.Messages) {
				t.Errorf("stream session was not committed before hook")
			}
			return nil
		},
	))
	if err != nil {
		t.Fatalf("RunStream: %v", err)
	}
	if !called {
		t.Fatal("post-run hook was not called")
	}
	if result.Output != "done" {
		t.Errorf("Output = %q, want done", result.Output)
	}
}
