package llm

import (
	"context"
	"errors"
	"fmt"
	"sync"
)

// ErrScriptExhausted is returned when Scripted receives more calls than steps.
var ErrScriptExhausted = errors.New("llm: scripted client has no more steps")

// Step is one scripted reaction to a call. Exactly one of the forms applies:
//   - Func set: it is called and its result returned;
//   - Block set: the call waits until ctx is done and returns ctx.Err()
//     (simulates a hung provider for timeout / cancellation tests);
//   - Err set: the error is returned;
//   - otherwise Response is returned (Chunks, if set, are the stream deltas
//     and must concatenate to Response.Text).
type Step struct {
	Response Response
	Chunks   []string
	Err      error
	Block    bool
	Func     func(ctx context.Context, req Request) (Response, error)
}

// Reply is a complete, successful structured answer.
func Reply(text string) Step {
	return Step{Response: Response{Text: text, StopReason: StopEndTurn, Model: "scripted-model"}}
}

// Stopped is an answer that ended with the given stop reason.
func Stopped(text string, reason StopReason) Step {
	return Step{Response: Response{Text: text, StopReason: reason, Model: "scripted-model"}}
}

// Fail returns err.
func Fail(err error) Step { return Step{Err: err} }

// Hang blocks until the context ends.
func Hang() Step { return Step{Block: true} }

// Scripted is a deterministic Client for tests: it plays Steps in order and
// records every request. It never performs I/O. Safe for concurrent use.
type Scripted struct {
	mu       sync.Mutex
	steps    []Step
	requests []Request
}

// NewScripted returns a client that plays steps in order.
func NewScripted(steps ...Step) *Scripted {
	return &Scripted{steps: append([]Step(nil), steps...)}
}

// Push appends steps.
func (s *Scripted) Push(steps ...Step) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.steps = append(s.steps, steps...)
}

// Requests returns copies of the requests received so far.
func (s *Scripted) Requests() []Request {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Request, len(s.requests))
	for i, r := range s.requests {
		r.Messages = append([]Message(nil), r.Messages...)
		out[i] = r
	}
	return out
}

// Calls returns the number of calls received.
func (s *Scripted) Calls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.requests)
}

// Remaining returns the number of unplayed steps.
func (s *Scripted) Remaining() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.steps)
}

func (s *Scripted) next(req Request) (Step, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	req.Messages = append([]Message(nil), req.Messages...)
	s.requests = append(s.requests, req)
	if len(s.steps) == 0 {
		return Step{}, fmt.Errorf("%w (call %d)", ErrScriptExhausted, len(s.requests))
	}
	st := s.steps[0]
	s.steps = s.steps[1:]
	return st, nil
}

// Complete implements Client.
func (s *Scripted) Complete(ctx context.Context, req Request) (Response, error) {
	if err := ctx.Err(); err != nil {
		return Response{}, err
	}
	st, err := s.next(req)
	if err != nil {
		return Response{}, err
	}
	return play(ctx, req, st)
}

// Stream implements Client: it emits the step's Chunks (or the whole text as
// one delta) and returns the full Response.
func (s *Scripted) Stream(ctx context.Context, req Request, onEvent func(StreamEvent) error) (Response, error) {
	if err := ctx.Err(); err != nil {
		return Response{}, err
	}
	st, err := s.next(req)
	if err != nil {
		return Response{}, err
	}
	resp, err := play(ctx, req, st)
	if err != nil {
		return Response{}, err
	}
	chunks := st.Chunks
	if chunks == nil && resp.Text != "" {
		chunks = []string{resp.Text}
	}
	for _, c := range chunks {
		if err := ctx.Err(); err != nil {
			return Response{}, err
		}
		if err := onEvent(StreamEvent{Kind: EventTextDelta, Text: c}); err != nil {
			return Response{}, err
		}
	}
	return resp, nil
}

func play(ctx context.Context, req Request, st Step) (Response, error) {
	switch {
	case st.Func != nil:
		return st.Func(ctx, req)
	case st.Block:
		<-ctx.Done()
		return Response{}, ctx.Err()
	case st.Err != nil:
		return Response{}, st.Err
	default:
		resp := st.Response
		if resp.Model == "" {
			resp.Model = req.Model
		}
		return resp, nil
	}
}
