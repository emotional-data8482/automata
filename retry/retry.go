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
// exponential backoff (2s → 4s → 8s, capped at 30s, ±50% jitter). RetryUnknown
// is false, so only errors implementing [Retryable] are retried — plain errors
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
	if cfg.MaxAttempts == 0 {
		return fn()
	}

	var zero T
	for attempt := 0; attempt < cfg.MaxAttempts; attempt++ {
		result, err := fn()
		if err == nil {
			return result, nil
		}

		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return zero, err
		}

		if attempt == cfg.MaxAttempts-1 {
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
	// ±50% jitter
	delay *= 0.5 + rand.Float64()*0.5
	return time.Duration(delay)
}
