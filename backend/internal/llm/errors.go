package llm

import (
	"context"
	"errors"
	"strconv"
)

// Provider-neutral failure classes. Implementations return an *Error whose
// Kind is one of these, or a context error when the caller's context ended.
var (
	// ErrUnavailable: transport failure, timeout, 429, 5xx, overload —
	// retryable later (after the implementation's own retries).
	ErrUnavailable = errors.New("llm: provider unavailable")
	// ErrRejected: the provider rejected the request (4xx other than 408/429:
	// invalid request, authentication, permission). Not retryable.
	ErrRejected = errors.New("llm: request rejected")
)

// Error is a classified provider failure.
type Error struct {
	// Kind is ErrUnavailable or ErrRejected.
	Kind error
	// StatusCode is the provider HTTP status, 0 if none (transport errors).
	StatusCode int
	// RequestID is the provider request id when known (safe to log).
	RequestID string
	// Err is the underlying cause (never contains the API key).
	Err error
}

func (e *Error) Error() string {
	s := e.Kind.Error()
	if e.StatusCode != 0 {
		s += " (status " + strconv.Itoa(e.StatusCode) + ")"
	}
	if e.Err != nil {
		s += ": " + e.Err.Error()
	}
	return s
}

// Is matches the failure class, so errors.Is(err, ErrUnavailable) works.
func (e *Error) Is(target error) bool { return target == e.Kind }

// Unwrap returns the cause.
func (e *Error) Unwrap() error { return e.Err }

// Retryable reports whether a later identical request may succeed: provider
// unavailability or a deadline, but not caller cancellation or rejection.
func Retryable(err error) bool {
	switch {
	case err == nil, errors.Is(err, context.Canceled), errors.Is(err, ErrRejected):
		return false
	case errors.Is(err, ErrUnavailable), errors.Is(err, context.DeadlineExceeded):
		return true
	default:
		return false
	}
}
