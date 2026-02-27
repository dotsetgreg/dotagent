package providers

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// ErrorKind identifies a normalized class of provider failures.
type ErrorKind string

const (
	ErrorKindUnknown         ErrorKind = "unknown"
	ErrorKindTransient       ErrorKind = "transient"
	ErrorKindTimeout         ErrorKind = "timeout"
	ErrorKindRateLimited     ErrorKind = "rate_limited"
	ErrorKindContextOverflow ErrorKind = "context_overflow"
	ErrorKindAuth            ErrorKind = "auth"
	ErrorKindBadRequest      ErrorKind = "bad_request"
	ErrorKindUnavailable     ErrorKind = "unavailable"
	ErrorKindCanceled        ErrorKind = "canceled"
)

// Error is a typed provider error used for robust retry and recovery behavior.
type Error struct {
	Provider   string
	Kind       ErrorKind
	StatusCode int
	RetryAfter time.Duration
	Message    string
	Cause      error
}

func (e *Error) Error() string {
	if e == nil {
		return ""
	}
	parts := []string{}
	if strings.TrimSpace(e.Provider) != "" {
		parts = append(parts, e.Provider)
	}
	if e.Kind != "" && e.Kind != ErrorKindUnknown {
		parts = append(parts, string(e.Kind))
	}
	if e.StatusCode > 0 {
		parts = append(parts, fmt.Sprintf("status=%d", e.StatusCode))
	}
	if d := e.RetryAfter; d > 0 {
		parts = append(parts, fmt.Sprintf("retry_after=%s", d))
	}
	if msg := strings.TrimSpace(e.Message); msg != "" {
		parts = append(parts, msg)
	}
	if len(parts) == 0 && e.Cause != nil {
		return e.Cause.Error()
	}
	if e.Cause != nil {
		return strings.Join(parts, " ") + ": " + e.Cause.Error()
	}
	return strings.Join(parts, " ")
}

func (e *Error) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Cause
}

func NewHTTPError(provider string, statusCode int, message string, retryAfter time.Duration) error {
	kind := classifyHTTPStatusKind(statusCode)
	if kind == ErrorKindBadRequest {
		if detected := detectContextOverflowFromMessage(message); detected {
			kind = ErrorKindContextOverflow
		}
	}
	return &Error{
		Provider:   strings.TrimSpace(strings.ToLower(provider)),
		Kind:       kind,
		StatusCode: statusCode,
		RetryAfter: retryAfter,
		Message:    strings.TrimSpace(message),
	}
}

func WrapTransportError(provider string, cause error) error {
	if cause == nil {
		return nil
	}
	kind := classifyTransportError(cause)
	return &Error{
		Provider: strings.TrimSpace(strings.ToLower(provider)),
		Kind:     kind,
		Message:  strings.TrimSpace(cause.Error()),
		Cause:    cause,
	}
}

// NormalizeProviderError ensures provider errors are consistently wrapped
// with normalized metadata for downstream retry/recovery logic.
func NormalizeProviderError(provider string, err error) error {
	if err == nil {
		return nil
	}
	provider = strings.TrimSpace(strings.ToLower(provider))
	var pe *Error
	if errors.As(err, &pe) {
		if provider == "" || pe.Provider != "" {
			return err
		}
		cp := *pe
		cp.Provider = provider
		return &cp
	}
	meta := InspectError(err)
	kind := meta.Kind
	if kind == "" {
		kind = ErrorKindUnknown
	}
	msg := strings.TrimSpace(meta.Message)
	if msg == "" {
		msg = strings.TrimSpace(err.Error())
	}
	return &Error{
		Provider:   provider,
		Kind:       kind,
		StatusCode: meta.StatusCode,
		RetryAfter: meta.RetryAfter,
		Message:    msg,
		Cause:      err,
	}
}

func IsTransientError(err error) bool {
	if err == nil {
		return false
	}
	meta := InspectError(err)
	switch meta.Kind {
	case ErrorKindTransient, ErrorKindTimeout, ErrorKindRateLimited, ErrorKindUnavailable:
		return true
	default:
		return false
	}
}

func IsContextOverflowError(err error) bool {
	if err == nil {
		return false
	}
	return InspectError(err).Kind == ErrorKindContextOverflow
}

func IsRateLimitedError(err error) bool {
	if err == nil {
		return false
	}
	return InspectError(err).Kind == ErrorKindRateLimited
}

func RetryAfterHint(err error) (time.Duration, bool) {
	if err == nil {
		return 0, false
	}
	meta := InspectError(err)
	if meta.RetryAfter > 0 {
		return meta.RetryAfter, true
	}
	return 0, false
}

// ErrorMeta is normalized metadata derived from provider errors.
type ErrorMeta struct {
	Provider   string
	Kind       ErrorKind
	StatusCode int
	RetryAfter time.Duration
	Message    string
}

func InspectError(err error) ErrorMeta {
	if err == nil {
		return ErrorMeta{}
	}

	var pe *Error
	if errors.As(err, &pe) {
		kind := pe.Kind
		if kind == "" {
			kind = ErrorKindUnknown
		}
		msg := strings.TrimSpace(pe.Message)
		if msg == "" && pe.Cause != nil {
			msg = pe.Cause.Error()
		}
		if kind == ErrorKindUnknown && detectContextOverflowFromMessage(msg) {
			kind = ErrorKindContextOverflow
		}
		return ErrorMeta{
			Provider:   pe.Provider,
			Kind:       kind,
			StatusCode: pe.StatusCode,
			RetryAfter: pe.RetryAfter,
			Message:    msg,
		}
	}

	msg := strings.TrimSpace(err.Error())
	kind := classifyMessageKind(msg)
	status := extractStatusCode(msg)
	if status > 0 {
		statusKind := classifyHTTPStatusKind(status)
		if statusKind != ErrorKindUnknown {
			kind = statusKind
		}
	}
	if kind == ErrorKindUnknown && detectContextOverflowFromMessage(msg) {
		kind = ErrorKindContextOverflow
	}
	return ErrorMeta{Kind: kind, StatusCode: status, Message: msg}
}

func classifyHTTPStatusKind(statusCode int) ErrorKind {
	switch {
	case statusCode == http.StatusTooManyRequests:
		return ErrorKindRateLimited
	case statusCode == http.StatusUnauthorized || statusCode == http.StatusForbidden:
		return ErrorKindAuth
	case statusCode == http.StatusRequestTimeout || statusCode == http.StatusGatewayTimeout:
		return ErrorKindTimeout
	case statusCode >= 500 && statusCode <= 599:
		return ErrorKindUnavailable
	case statusCode >= 400 && statusCode <= 499:
		return ErrorKindBadRequest
	default:
		return ErrorKindUnknown
	}
}

func classifyTransportError(err error) ErrorKind {
	if err == nil {
		return ErrorKindUnknown
	}
	if errors.Is(err, context.Canceled) {
		return ErrorKindCanceled
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return ErrorKindTimeout
	}
	if ne, ok := err.(net.Error); ok {
		if ne.Timeout() {
			return ErrorKindTimeout
		}
		if ne.Temporary() {
			return ErrorKindTransient
		}
	}
	msg := strings.ToLower(strings.TrimSpace(err.Error()))
	switch {
	case strings.Contains(msg, "context canceled"):
		return ErrorKindCanceled
	case strings.Contains(msg, "context deadline exceeded"),
		strings.Contains(msg, "timeout"),
		strings.Contains(msg, "i/o timeout"),
		strings.Contains(msg, "tls handshake timeout"):
		return ErrorKindTimeout
	case strings.Contains(msg, "connection reset"),
		strings.Contains(msg, "broken pipe"),
		strings.Contains(msg, "temporary"),
		strings.Contains(msg, "unavailable"):
		return ErrorKindTransient
	default:
		return ErrorKindUnknown
	}
}

func classifyMessageKind(message string) ErrorKind {
	msg := strings.ToLower(strings.TrimSpace(message))
	switch {
	case msg == "":
		return ErrorKindUnknown
	case strings.Contains(msg, "context canceled"):
		return ErrorKindCanceled
	case strings.Contains(msg, "context deadline exceeded") ||
		strings.Contains(msg, "timeout") ||
		strings.Contains(msg, "i/o timeout"):
		return ErrorKindTimeout
	case strings.Contains(msg, "status=429") ||
		strings.Contains(msg, "rate limit") ||
		strings.Contains(msg, "too many requests"):
		return ErrorKindRateLimited
	case strings.Contains(msg, "status=502") ||
		strings.Contains(msg, "status=503") ||
		strings.Contains(msg, "status=504") ||
		strings.Contains(msg, "service unavailable"):
		return ErrorKindUnavailable
	case strings.Contains(msg, "unauthorized") ||
		strings.Contains(msg, "forbidden") ||
		strings.Contains(msg, "status=401") ||
		strings.Contains(msg, "status=403"):
		return ErrorKindAuth
	case detectContextOverflowFromMessage(msg):
		return ErrorKindContextOverflow
	default:
		return ErrorKindUnknown
	}
}

func detectContextOverflowFromMessage(message string) bool {
	msg := strings.ToLower(strings.TrimSpace(message))
	return strings.Contains(msg, "context_length_exceeded") ||
		strings.Contains(msg, "maximum context length") ||
		strings.Contains(msg, "too many tokens") ||
		strings.Contains(msg, "token limit") ||
		strings.Contains(msg, "max message tokens") ||
		strings.Contains(msg, "max input tokens") ||
		strings.Contains(msg, "prompt too long") ||
		strings.Contains(msg, "input too long") ||
		strings.Contains(msg, "input exceeds") ||
		strings.Contains(msg, "input is too large") ||
		strings.Contains(msg, "maximum prompt") ||
		strings.Contains(msg, "prompt size exceeded") ||
		strings.Contains(msg, "request too large") ||
		(strings.Contains(msg, "context") && strings.Contains(msg, "length"))
}

func extractStatusCode(message string) int {
	msg := strings.ToLower(strings.TrimSpace(message))
	idx := strings.Index(msg, "status=")
	if idx < 0 {
		return 0
	}
	start := idx + len("status=")
	if start >= len(msg) {
		return 0
	}
	end := start
	for end < len(msg) && msg[end] >= '0' && msg[end] <= '9' {
		end++
	}
	if end == start {
		return 0
	}
	code, err := strconv.Atoi(msg[start:end])
	if err != nil {
		return 0
	}
	return code
}

func ParseRetryAfterHeader(header string) time.Duration {
	trimmed := strings.TrimSpace(header)
	if trimmed == "" {
		return 0
	}
	if secs, err := strconv.Atoi(trimmed); err == nil {
		if secs <= 0 {
			return 0
		}
		return time.Duration(secs) * time.Second
	}
	if ts, err := http.ParseTime(trimmed); err == nil {
		delta := time.Until(ts)
		if delta > 0 {
			return delta
		}
	}
	return 0
}
