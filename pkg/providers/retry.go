package providers

import (
	"context"
	"math"
	"math/rand"
	"time"
)

// RetryConfig controls retry behavior for provider calls.
type RetryConfig struct {
	MaxAttempts int
	MinDelay    time.Duration
	MaxDelay    time.Duration
	Jitter      float64
}

// RetryInfo is emitted before a retry sleep.
type RetryInfo struct {
	Attempt int
	Delay   time.Duration
	Err     error
}

func DefaultRetryConfig() RetryConfig {
	return RetryConfig{
		MaxAttempts: 3,
		MinDelay:    1200 * time.Millisecond,
		MaxDelay:    10 * time.Second,
		Jitter:      0.15,
	}
}

func (cfg RetryConfig) normalized() RetryConfig {
	if cfg.MaxAttempts <= 0 {
		cfg.MaxAttempts = 1
	}
	if cfg.MinDelay <= 0 {
		cfg.MinDelay = 1200 * time.Millisecond
	}
	if cfg.MaxDelay <= 0 {
		cfg.MaxDelay = 10 * time.Second
	}
	if cfg.MaxDelay < cfg.MinDelay {
		cfg.MaxDelay = cfg.MinDelay
	}
	if cfg.Jitter < 0 {
		cfg.Jitter = 0
	}
	if cfg.Jitter > 1 {
		cfg.Jitter = 1
	}
	return cfg
}

func retryDelayForAttempt(cfg RetryConfig, attempt int, err error) time.Duration {
	cfg = cfg.normalized()
	if ra, ok := RetryAfterHint(err); ok && ra > 0 {
		d := ra
		if d < cfg.MinDelay {
			d = cfg.MinDelay
		}
		if d > cfg.MaxDelay {
			d = cfg.MaxDelay
		}
		return applyJitter(d, cfg.Jitter)
	}
	pow := math.Pow(2, float64(maxInt(0, attempt-1)))
	base := time.Duration(float64(cfg.MinDelay) * pow)
	if base > cfg.MaxDelay {
		base = cfg.MaxDelay
	}
	return applyJitter(base, cfg.Jitter)
}

func applyJitter(delay time.Duration, jitter float64) time.Duration {
	if jitter <= 0 || delay <= 0 {
		return delay
	}
	spread := 1 + ((rand.Float64()*2 - 1) * jitter)
	if spread < 0.05 {
		spread = 0.05
	}
	return time.Duration(float64(delay) * spread)
}

func RetryCall[T any](ctx context.Context, cfg RetryConfig, call func() (T, error), shouldRetry func(error) bool, onRetry func(RetryInfo)) (T, error) {
	cfg = cfg.normalized()
	var zero T
	for attempt := 1; attempt <= cfg.MaxAttempts; attempt++ {
		value, err := call()
		if err == nil {
			return value, nil
		}
		if attempt >= cfg.MaxAttempts || shouldRetry == nil || !shouldRetry(err) {
			return zero, err
		}
		delay := retryDelayForAttempt(cfg, attempt, err)
		if onRetry != nil {
			onRetry(RetryInfo{Attempt: attempt, Delay: delay, Err: err})
		}
		if delay <= 0 {
			continue
		}
		select {
		case <-ctx.Done():
			return zero, ctx.Err()
		case <-time.After(delay):
		}
	}
	return zero, context.Canceled
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
