package llm

import (
	"context"
	"errors"
	"time"
)

// Request outcomes reported by Observe (one provider request each).
const (
	OutcomeOK          = "ok"          // a complete end_turn answer
	OutcomeMaxTokens   = "max_tokens"  // truncated (the caller may retry once)
	OutcomeRefused     = "refused"     // stop_reason refusal
	OutcomeUnavailable = "unavailable" // ErrUnavailable (transport, 429, 5xx, overload)
	OutcomeRejected    = "rejected"    // ErrRejected (4xx)
	OutcomeTimeout     = "timeout"     // the request's deadline passed
	OutcomeCancelled   = "cancelled"   // the caller's context was cancelled (client left)
	OutcomeAborted     = "aborted"     // the stream consumer stopped the stream
	OutcomeError       = "error"       // anything else (e.g. another stop reason)
)

// ObserveFunc receives one finished provider request: its task, outcome
// and duration. It must be cheap and must not block.
type ObserveFunc func(task, outcome string, d time.Duration)

// Observe wraps c so that every Complete/Stream request is reported to fn
// (metrics). Requests, responses and errors pass through unchanged; the
// observer never sees prompts, outputs or keys.
func Observe(c Client, fn ObserveFunc) Client {
	if fn == nil {
		return c
	}
	return &observed{inner: c, fn: fn}
}

type observed struct {
	inner Client
	fn    ObserveFunc
}

func (o *observed) Complete(ctx context.Context, req Request) (Response, error) {
	start := time.Now()
	resp, err := o.inner.Complete(ctx, req)
	o.fn(req.Task, Classify(ctx, resp, err, false), time.Since(start))
	return resp, err
}

func (o *observed) Stream(ctx context.Context, req Request, onEvent func(StreamEvent) error) (Response, error) {
	start := time.Now()
	var consumerErr error
	resp, err := o.inner.Stream(ctx, req, func(ev StreamEvent) error {
		e := onEvent(ev)
		if e != nil {
			consumerErr = e
		}
		return e
	})
	o.fn(req.Task, Classify(ctx, resp, err, consumerErr != nil && errors.Is(err, consumerErr)), time.Since(start))
	return resp, err
}

// Classify maps one request's result to an outcome.
func Classify(ctx context.Context, resp Response, err error, consumerStopped bool) string {
	switch {
	case consumerStopped:
		return OutcomeAborted
	case err == nil:
		switch resp.StopReason {
		case StopEndTurn:
			return OutcomeOK
		case StopMaxTokens:
			return OutcomeMaxTokens
		case StopRefusal:
			return OutcomeRefused
		}
		return OutcomeError
	case errors.Is(err, ErrUnavailable):
		return OutcomeUnavailable
	case errors.Is(err, ErrRejected):
		return OutcomeRejected
	case errors.Is(err, context.DeadlineExceeded) || errors.Is(ctx.Err(), context.DeadlineExceeded):
		return OutcomeTimeout
	case errors.Is(err, context.Canceled) || ctx.Err() != nil:
		return OutcomeCancelled
	}
	return OutcomeError
}
