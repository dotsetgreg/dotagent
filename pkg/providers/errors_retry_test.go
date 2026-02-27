package providers

import (
	"context"
	"fmt"
	"net/http"
	"testing"
	"time"
)

func TestInspectError_ContextOverflowFromMessage(t *testing.T) {
	meta := InspectError(fmt.Errorf("context_length_exceeded: too many tokens"))
	if meta.Kind != ErrorKindContextOverflow {
		t.Fatalf("expected context overflow kind, got %q", meta.Kind)
	}
	meta = InspectError(fmt.Errorf("bad_request: prompt too long for model"))
	if meta.Kind != ErrorKindContextOverflow {
		t.Fatalf("expected prompt-too-long to map to context overflow, got %q", meta.Kind)
	}
}

func TestNewHTTPError_RateLimitRetryAfter(t *testing.T) {
	err := NewHTTPError("openai", 429, "rate limit", 3*time.Second)
	meta := InspectError(err)
	if meta.Kind != ErrorKindRateLimited {
		t.Fatalf("expected rate_limited, got %q", meta.Kind)
	}
	if meta.RetryAfter != 3*time.Second {
		t.Fatalf("expected retry-after 3s, got %s", meta.RetryAfter)
	}
	if !IsTransientError(err) {
		t.Fatalf("expected rate limit error to be transient")
	}
}

func TestRetryCall_UsesRetryAfterHint(t *testing.T) {
	ctx := context.Background()
	cfg := RetryConfig{MaxAttempts: 2, MinDelay: 1 * time.Millisecond, MaxDelay: 20 * time.Millisecond, Jitter: 0}

	calls := 0
	var observedDelay time.Duration
	_, err := RetryCall(ctx, cfg, func() (string, error) {
		calls++
		if calls == 1 {
			return "", &Error{Kind: ErrorKindRateLimited, RetryAfter: 7 * time.Millisecond, Message: "limited"}
		}
		return "ok", nil
	}, IsTransientError, func(info RetryInfo) {
		observedDelay = info.Delay
	})
	if err != nil {
		t.Fatalf("RetryCall returned error: %v", err)
	}
	if calls != 2 {
		t.Fatalf("expected 2 calls, got %d", calls)
	}
	if observedDelay != 7*time.Millisecond {
		t.Fatalf("expected observed delay 7ms, got %s", observedDelay)
	}
}

func TestParseRetryAfterHeader(t *testing.T) {
	if got := ParseRetryAfterHeader("5"); got != 5*time.Second {
		t.Fatalf("expected 5s from integer retry-after, got %s", got)
	}
	future := time.Now().Add(2 * time.Second).UTC().Format(http.TimeFormat)
	if got := ParseRetryAfterHeader(future); got <= 0 {
		t.Fatalf("expected positive duration for HTTP-date retry-after, got %s", got)
	}
}

func TestNormalizeProviderError_WrapsUntypedError(t *testing.T) {
	err := NormalizeProviderError("openrouter", fmt.Errorf("request too large"))
	meta := InspectError(err)
	if meta.Kind != ErrorKindContextOverflow {
		t.Fatalf("expected normalized context overflow, got %q", meta.Kind)
	}
	if meta.Provider != "openrouter" {
		t.Fatalf("expected provider openrouter, got %q", meta.Provider)
	}
}
