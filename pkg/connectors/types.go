package connectors

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"
)

type InvocationResult struct {
	Content     string
	UserContent string
	IsError     bool
}

// Runtime is the common execution contract for connector implementations.
type Runtime interface {
	ID() string
	Type() string
	Health(ctx context.Context) error
	ToolSchema(ctx context.Context, target string) (description string, parameters map[string]interface{}, err error)
	Invoke(ctx context.Context, target string, args map[string]interface{}) (InvocationResult, error)
	Close() error
}

type RetryPolicy struct {
	MaxAttempts int
	Backoff     time.Duration
}

func normalizeRetryPolicy(policy RetryPolicy) RetryPolicy {
	if policy.MaxAttempts <= 0 {
		policy.MaxAttempts = 2
	}
	if policy.Backoff <= 0 {
		policy.Backoff = 250 * time.Millisecond
	}
	return policy
}

func withRetry(ctx context.Context, policy RetryPolicy, fn func(attempt int) error) error {
	policy = normalizeRetryPolicy(policy)
	var last error
	for i := 1; i <= policy.MaxAttempts; i++ {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		if err := fn(i); err == nil {
			return nil
		} else {
			last = err
		}
		if i == policy.MaxAttempts {
			break
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(policy.Backoff):
		}
	}
	if last != nil {
		return last
	}
	return fmt.Errorf("operation failed without error details")
}

// ResolveSecretRef resolves values in the form "env:VAR_NAME".
func ResolveSecretRef(raw string) string {
	raw = strings.TrimSpace(raw)
	if !strings.HasPrefix(strings.ToLower(raw), "env:") {
		return raw
	}
	key := strings.TrimSpace(raw[4:])
	if key == "" {
		return ""
	}
	return os.Getenv(key)
}

func ResolveStringMap(raw map[string]string) map[string]string {
	if len(raw) == 0 {
		return nil
	}
	out := make(map[string]string, len(raw))
	for k, v := range raw {
		key := strings.TrimSpace(k)
		if key == "" {
			continue
		}
		out[key] = ResolveSecretRef(v)
	}
	return out
}

func compactJSONSchema(schema map[string]interface{}) map[string]interface{} {
	if schema == nil {
		return map[string]interface{}{
			"type":       "object",
			"properties": map[string]interface{}{},
		}
	}
	return schema
}
