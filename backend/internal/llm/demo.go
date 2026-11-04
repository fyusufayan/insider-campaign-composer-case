package llm

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"unicode/utf8"
)

// DemoModel is the model name reported by every Demo response, so persisted
// AI results are recognisable as demo output (the UI shows a "Mock AI" badge).
const DemoModel = "demo"

// ErrDemoUnknownTask is returned for a task without a registered responder.
var ErrDemoUnknownTask = errors.New("llm: demo has no responder for task")

// DemoResponder computes the demo answer text for a request. It must be a
// pure, deterministic function of the request.
type DemoResponder func(req Request) (string, error)

// Demo is the no-key Client used to run the app (never in place of a failing
// Anthropic client). It dispatches on Request.Task to responders supplied by
// the owning use case (package ai), so this package stays domain-agnostic.
// Responses are deterministic; Stream simulates streaming by chunking.
type Demo struct {
	responders map[string]DemoResponder
}

// NewDemo returns a Demo with the given task responders.
func NewDemo(responders map[string]DemoResponder) *Demo {
	m := make(map[string]DemoResponder, len(responders))
	for k, v := range responders {
		m[k] = v
	}
	return &Demo{responders: m}
}

// Tasks lists the registered tasks (sorted).
func (d *Demo) Tasks() []string {
	out := make([]string, 0, len(d.responders))
	for k := range d.responders {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Complete implements Client.
func (d *Demo) Complete(ctx context.Context, req Request) (Response, error) {
	if err := ctx.Err(); err != nil {
		return Response{}, err
	}
	resp, ok := d.responders[req.Task]
	if !ok {
		return Response{}, &Error{Kind: ErrRejected, Err: fmt.Errorf("%w %q", ErrDemoUnknownTask, req.Task)}
	}
	text, err := resp(req)
	if err != nil {
		return Response{}, &Error{Kind: ErrRejected, Err: err}
	}
	return Response{Text: text, StopReason: StopEndTurn, Model: DemoModel}, nil
}

// demoChunkRunes is the size of simulated stream deltas.
const demoChunkRunes = 24

// Stream implements Client by emitting the Complete text in fixed-size,
// rune-aligned chunks.
func (d *Demo) Stream(ctx context.Context, req Request, onEvent func(StreamEvent) error) (Response, error) {
	resp, err := d.Complete(ctx, req)
	if err != nil {
		return Response{}, err
	}
	text := resp.Text
	for len(text) > 0 {
		if err := ctx.Err(); err != nil {
			return Response{}, err
		}
		n, runes := 0, 0
		for n < len(text) && runes < demoChunkRunes {
			_, size := utf8.DecodeRuneInString(text[n:])
			n += size
			runes++
		}
		if err := onEvent(StreamEvent{Kind: EventTextDelta, Text: text[:n]}); err != nil {
			return Response{}, err
		}
		text = text[n:]
	}
	return resp, nil
}
