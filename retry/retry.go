package retry

import (
	"context"
	"errors"
	"math"
	"math/rand/v2"
	"time"
)

type Config struct {
	MaxAttempts  int
	InitialDelay time.Duration
	MaxDelay     time.Duration
	Multiplier   float64
	RetryUnknown bool
}

// DefaultConfig returns the recommended retry configuration: 4 attempts with
// exponential backoff (2s → 4s → 8s, capped at 30s, then scaled by a random
// factor in [0.5, 1.0) to decorrelate concurrent retries). RetryUnknown is
// false, so only errors implementing [Retryable] are retried — plain errors
// fail fast on the first attempt.
//
// Returns a fresh value each call so caller mutations don't bleed into anyone
// else's config.
func DefaultConfig() Config {
	return Config{
		MaxAttempts:  4,
		InitialDelay: 2 * time.Second,
		MaxDelay:     30 * time.Second,
		Multiplier:   2,
		RetryUnknown: false,
	}
}

type Retryable interface {
	Retryable() bool
}

func Do[T any](ctx context.Context, cfg Config, fn func() (T, error)) (T, error) {
	// MaxAttempts <= 0 means "no retries": run fn exactly once. Without this
	// floor a negative value would skip the loop entirely and silently return
	// the zero value with a nil error.
	maxAttempts := cfg.MaxAttempts
	if maxAttempts <= 0 {
		maxAttempts = 1
	}

	var zero T
	for attempt := 0; attempt < maxAttempts; attempt++ {
		result, err := fn()
		if err == nil {
			return result, nil
		}

		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return zero, err
		}

		if attempt == maxAttempts-1 {
			return zero, err
		}

		if !shouldRetry(err, cfg.RetryUnknown) {
			return zero, err
		}

		delay := backoff(cfg, attempt)
		select {
		case <-ctx.Done():
			return zero, ctx.Err()
		case <-time.After(delay):
		}
	}

	return zero, nil
}

func shouldRetry(err error, retryUnknown bool) bool {
	// Retryable is not an error type, so errors.AsType (which constrains its
	// type parameter to error) can't be used here; errors.As with a pointer to
	// the interface is the way to unwrap to it.
	var r Retryable
	if errors.As(err, &r) {
		return r.Retryable()
	}
	return retryUnknown
}

func backoff(cfg Config, attempt int) time.Duration {
	multiplier := cfg.Multiplier
	if multiplier == 0 {
		multiplier = 1
	}
	delay := float64(cfg.InitialDelay) * math.Pow(multiplier, float64(attempt))
	if max := float64(cfg.MaxDelay); delay > max {
		delay = max
	}
	// Scale by a random factor in [0.5, 1.0): jitter down from the nominal
	// delay only, so a jittered delay never exceeds MaxDelay after the cap
	// above. This decorrelates retries across concurrent callers.
	delay *= 0.5 + rand.Float64()*0.5
	return time.Duration(delay)
}
