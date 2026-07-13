package retry

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

// retryableErr is a test error whose retryability is configurable.
type retryableErr struct {
	msg   string
	retry bool
}

func (e *retryableErr) Error() string   { return e.msg }
func (e *retryableErr) Retryable() bool { return e.retry }

// fastConfig is a retry config with negligible delays so tests don't sleep for
// real seconds. Retryable errors only (RetryUnknown false).
func fastConfig(maxAttempts int) Config {
	return Config{
		MaxAttempts:  maxAttempts,
		InitialDelay: time.Millisecond,
		MaxDelay:     5 * time.Millisecond,
		Multiplier:   2,
	}
}

func TestDoSucceedsFirstTry(t *testing.T) {
	calls := 0
	out, err := Do(context.Background(), fastConfig(4), func() (int, error) {
		calls++
		return 42, nil
	})
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	if out != 42 {
		t.Errorf("out = %d, want 42", out)
	}
	if calls != 1 {
		t.Errorf("calls = %d, want 1", calls)
	}
}

func TestDoRetriesRetryableThenSucceeds(t *testing.T) {
	calls := 0
	out, err := Do(context.Background(), fastConfig(4), func() (string, error) {
		calls++
		if calls < 3 {
			return "", &retryableErr{msg: "transient", retry: true}
		}
		return "ok", nil
	})
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	if out != "ok" {
		t.Errorf("out = %q, want ok", out)
	}
	if calls != 3 {
		t.Errorf("calls = %d, want 3", calls)
	}
}

func TestDoExhaustsAttemptsAndReturnsLastError(t *testing.T) {
	calls := 0
	last := &retryableErr{msg: "still failing", retry: true}
	_, err := Do(context.Background(), fastConfig(3), func() (int, error) {
		calls++
		return 0, last
	})
	if !errors.Is(err, last) {
		t.Errorf("err = %v, want %v", err, last)
	}
	if calls != 3 {
		t.Errorf("calls = %d, want 3 (MaxAttempts)", calls)
	}
}

func TestDoFailsFastOnNonRetryable(t *testing.T) {
	calls := 0
	sentinel := errors.New("permanent")
	_, err := Do(context.Background(), fastConfig(4), func() (int, error) {
		calls++
		return 0, sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Errorf("err = %v, want %v", err, sentinel)
	}
	if calls != 1 {
		t.Errorf("calls = %d, want 1 (non-retryable fails fast)", calls)
	}
}

func TestDoRetryUnknownRetriesPlainErrors(t *testing.T) {
	cfg := fastConfig(3)
	cfg.RetryUnknown = true
	calls := 0
	_, err := Do(context.Background(), cfg, func() (int, error) {
		calls++
		return 0, errors.New("plain")
	})
	if err == nil {
		t.Fatal("Do: want error")
	}
	if calls != 3 {
		t.Errorf("calls = %d, want 3 (RetryUnknown retries plain errors)", calls)
	}
}

// TestDoRespectsRetryableFalse pins that an error implementing Retryable and
// reporting false is not retried even though it satisfies the interface.
func TestDoRespectsRetryableFalse(t *testing.T) {
	calls := 0
	_, err := Do(context.Background(), fastConfig(4), func() (int, error) {
		calls++
		return 0, &retryableErr{msg: "no", retry: false}
	})
	if err == nil {
		t.Fatal("Do: want error")
	}
	if calls != 1 {
		t.Errorf("calls = %d, want 1 (Retryable()==false fails fast)", calls)
	}
}

// TestDoDispatchesThroughWrappedError pins that errors.As finds a Retryable
// error wrapped with %w.
func TestDoDispatchesThroughWrappedError(t *testing.T) {
	calls := 0
	_, err := Do(context.Background(), fastConfig(3), func() (int, error) {
		calls++
		return 0, fmt.Errorf("context: %w", &retryableErr{msg: "inner", retry: true})
	})
	if err == nil {
		t.Fatal("Do: want error")
	}
	if calls != 3 {
		t.Errorf("calls = %d, want 3 (wrapped Retryable is retried)", calls)
	}
}

// TestDoZeroAttemptsRunsOnce pins that MaxAttempts == 0 runs fn exactly once
// and surfaces its result/error.
func TestDoZeroAttemptsRunsOnce(t *testing.T) {
	calls := 0
	sentinel := errors.New("boom")
	_, err := Do(context.Background(), Config{MaxAttempts: 0}, func() (int, error) {
		calls++
		return 0, sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Errorf("err = %v, want %v", err, sentinel)
	}
	if calls != 1 {
		t.Errorf("calls = %d, want 1", calls)
	}
}

// TestDoNegativeAttemptsRunsOnce is the regression test for the silent
// (zero, nil) bug: a negative MaxAttempts must run fn once, not skip it.
func TestDoNegativeAttemptsRunsOnce(t *testing.T) {
	calls := 0
	out, err := Do(context.Background(), Config{MaxAttempts: -5}, func() (int, error) {
		calls++
		return 7, nil
	})
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	if out != 7 {
		t.Errorf("out = %d, want 7 (fn must actually run)", out)
	}
	if calls != 1 {
		t.Errorf("calls = %d, want 1 (negative attempts must not skip fn)", calls)
	}
}

// TestDoNegativeAttemptsSurfacesError pins that the negative-attempts path
// returns fn's error rather than a nil error with a zero value.
func TestDoNegativeAttemptsSurfacesError(t *testing.T) {
	sentinel := errors.New("negative-boom")
	_, err := Do(context.Background(), Config{MaxAttempts: -1}, func() (int, error) {
		return 0, sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Errorf("err = %v, want %v (must not swallow error)", err, sentinel)
	}
}

func TestDoCanceledContextDuringBackoff(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cfg := Config{
		MaxAttempts:  5,
		InitialDelay: 50 * time.Millisecond,
		MaxDelay:     time.Second,
		Multiplier:   2,
	}
	calls := 0
	_, err := Do(ctx, cfg, func() (int, error) {
		calls++
		cancel() // cancel while the first backoff is pending
		return 0, &retryableErr{msg: "transient", retry: true}
	})
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
	if calls != 1 {
		t.Errorf("calls = %d, want 1 (cancel aborts before second attempt)", calls)
	}
}

// TestDoContextErrorFromFnNotRetried pins that a context error returned by fn
// itself aborts immediately, even though MaxAttempts allows more.
func TestDoContextErrorFromFnNotRetried(t *testing.T) {
	calls := 0
	_, err := Do(context.Background(), fastConfig(4), func() (int, error) {
		calls++
		return 0, context.DeadlineExceeded
	})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err = %v, want context.DeadlineExceeded", err)
	}
	if calls != 1 {
		t.Errorf("calls = %d, want 1 (context error fails fast)", calls)
	}
}

func TestBackoffCapAndJitterBounds(t *testing.T) {
	cfg := Config{
		InitialDelay: time.Second,
		MaxDelay:     4 * time.Second,
		Multiplier:   2,
	}
	// Nominal (pre-jitter) delays: 1s, 2s, 4s, 8s→cap 4s, ... The jittered
	// result must always fall in [0.5*nominalCapped, nominalCapped).
	for attempt := range 6 {
		nominal := float64(cfg.InitialDelay)
		for range attempt {
			nominal *= cfg.Multiplier
		}
		if nominal > float64(cfg.MaxDelay) {
			nominal = float64(cfg.MaxDelay)
		}
		for range 200 {
			d := backoff(cfg, attempt)
			if d < time.Duration(nominal*0.5) || d >= time.Duration(nominal) {
				t.Fatalf("attempt %d: backoff %v outside [%v, %v)",
					attempt, d, time.Duration(nominal*0.5), time.Duration(nominal))
			}
		}
	}
}

// TestBackoffZeroMultiplierDefaultsToOne pins the guard that a zero multiplier
// is treated as 1 (constant backoff) rather than collapsing the delay to zero.
func TestBackoffZeroMultiplierDefaultsToOne(t *testing.T) {
	cfg := Config{InitialDelay: time.Second, MaxDelay: time.Minute, Multiplier: 0}
	for attempt := range 4 {
		d := backoff(cfg, attempt)
		// Multiplier defaults to 1, so nominal stays InitialDelay every attempt.
		if d < 500*time.Millisecond || d >= time.Second {
			t.Errorf("attempt %d: backoff %v outside [500ms, 1s)", attempt, d)
		}
	}
}
