// Package logging builds the process logger: slog JSON with contact masking and
// request-scoped attributes taken from the context.
package logging

import (
	"context"
	"io"
	"log/slog"
)

// New returns a JSON logger that writes to w, masks emails and phone numbers in
// every string it emits and adds request_id from the context of *Context calls.
func New(w io.Writer, level slog.Leveler) *slog.Logger {
	h := slog.NewJSONHandler(w, &slog.HandlerOptions{
		Level:       level,
		ReplaceAttr: maskAttr,
	})
	return slog.New(contextHandler{h})
}

func maskAttr(_ []string, a slog.Attr) slog.Attr {
	switch a.Value.Kind() {
	case slog.KindString:
		if s := a.Value.String(); s != "" {
			a.Value = slog.StringValue(MaskText(s))
		}
	case slog.KindAny:
		if err, ok := a.Value.Any().(error); ok {
			a.Value = slog.StringValue(MaskText(err.Error()))
		}
	}
	return a
}

type ctxKey int

const requestIDKey ctxKey = iota

// WithRequestID returns a context carrying the request id.
func WithRequestID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, requestIDKey, id)
}

// RequestID returns the request id stored in ctx, or "".
func RequestID(ctx context.Context) string {
	id, _ := ctx.Value(requestIDKey).(string)
	return id
}

// contextHandler adds request-scoped attributes from the context to each record.
type contextHandler struct{ slog.Handler }

func (h contextHandler) Handle(ctx context.Context, r slog.Record) error {
	if id := RequestID(ctx); id != "" {
		r.AddAttrs(slog.String("request_id", id))
	}
	return h.Handler.Handle(ctx, r)
}

func (h contextHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return contextHandler{h.Handler.WithAttrs(attrs)}
}

func (h contextHandler) WithGroup(name string) slog.Handler {
	return contextHandler{h.Handler.WithGroup(name)}
}
