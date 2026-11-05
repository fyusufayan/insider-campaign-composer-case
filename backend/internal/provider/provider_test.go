package provider

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func newTestClient(t *testing.T, h http.HandlerFunc, timeout time.Duration) *Client {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	c, err := NewClient(ClientConfig{BaseURL: srv.URL + "/", RequestTimeout: timeout, RetryAfterCap: 30 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

var testMsg = Message{Channel: ChannelSMS, IdempotencyKey: "c:1:sms", To: "+905550001234", Text: "hello"}

// Wire format: method, path, headers and a deterministic body.
func TestRequestFormatIsDeterministic(t *testing.T) {
	var bodies []string
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		bodies = append(bodies, r.Method+" "+r.URL.Path+" "+r.Header.Get(HeaderIdempotencyKey)+" "+r.Header.Get("Content-Type")+" "+string(b))
		w.WriteHeader(http.StatusAccepted)
		_, _ = io.WriteString(w, `{"message_id":"m1","status":"accepted"}`)
	}, time.Second)
	for i := 0; i < 2; i++ {
		c.Send(context.Background(), testMsg)
	}
	em := Message{Channel: ChannelEmail, IdempotencyKey: "c:1:email", To: "a@example.test", Subject: "S", Text: "T"}
	c.Send(context.Background(), em)
	want := []string{
		`POST /v1/sms c:1:sms application/json {"to":"+905550001234","text":"hello"}`,
		`POST /v1/sms c:1:sms application/json {"to":"+905550001234","text":"hello"}`,
		`POST /v1/email c:1:email application/json {"to":"a@example.test","subject":"S","text":"T"}`,
	}
	if strings.Join(bodies, "\n") != strings.Join(want, "\n") {
		t.Errorf("requests:\n%s", strings.Join(bodies, "\n"))
	}
}

func TestResponseClassification(t *testing.T) {
	for _, tc := range []struct {
		name    string
		status  int
		header  map[string]string
		body    string
		outcome Outcome
		code    string
		id      string
		retry   time.Duration
	}{
		{"accepted", 202, nil, `{"message_id":"m1"}`, OutcomeAccepted, "", "m1", 0},
		{"ok", 200, nil, `{"message_id":"m2"}`, OutcomeAccepted, "", "m2", 0},
		{"replayed", 200, map[string]string{HeaderReplayed: "true"}, `{"message_id":"m1"}`, OutcomeReplayed, "", "m1", 0},
		{"2xx without id", 202, nil, `{}`, OutcomeUnknown, "unreadable_response", "", 0},
		{"2xx garbage", 200, nil, `not json`, OutcomeUnknown, "unreadable_response", "", 0},
		{"429 seconds", 429, map[string]string{HeaderRetryAfter: "3"}, ``, OutcomeRateLimited, "rate_limited", "", 3 * time.Second},
		{"429 missing", 429, nil, ``, OutcomeRateLimited, "rate_limited", "", time.Second},
		{"429 capped", 429, map[string]string{HeaderRetryAfter: "3600"}, ``, OutcomeRateLimited, "rate_limited", "", 30 * time.Second},
		{"503", 503, nil, `{"error":{"code":"unavailable","message":"x +905550001234"}}`, OutcomeTransientError, "unavailable", "", 0},
		{"500 bad code", 500, nil, `{"error":{"code":"Bad Code!"}}`, OutcomeTransientError, "http_500", "", 0},
		{"400", 400, nil, `{"error":{"code":"invalid_request"}}`, OutcomePermanentError, "invalid_request", "", 0},
		{"422 reuse", 422, nil, `{"error":{"code":"idempotency_key_reuse"}}`, OutcomePermanentError, CodeIdempotencyKeyReuse, "", 0},
		{"404", 404, nil, ``, OutcomePermanentError, "http_404", "", 0},
		{"redirect", 302, map[string]string{"Location": "http://elsewhere.invalid/"}, ``, OutcomePermanentError, "http_302", "", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
				for k, v := range tc.header {
					w.Header().Set(k, v)
				}
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.body)
			}, time.Second)
			r := c.Send(context.Background(), testMsg)
			if r.Outcome != tc.outcome || r.ErrorCode != tc.code || r.MessageID != tc.id || r.RetryAfter != tc.retry || r.HTTPStatus != tc.status {
				t.Errorf("result = %+v", r)
			}
			if strings.Contains(r.ErrorCode, "+90") {
				t.Error("error code echoes provider text")
			}
		})
	}
}

// No response within the request timeout → timeout (ambiguous).
func TestTimeoutIsAmbiguous(t *testing.T) {
	release := make(chan struct{})
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}, 100*time.Millisecond)
	t.Cleanup(func() { close(release) }) // runs before the server's Close
	r := c.Send(context.Background(), testMsg)
	if r.Outcome != OutcomeTimeout || !r.Outcome.Ambiguous() || r.HTTPStatus != 0 {
		t.Errorf("result = %+v", r)
	}
}

// Cancelling the caller's context mid-request (shutdown) → unknown: the
// request may already have reached the provider.
func TestAbortedRequestIsUnknown(t *testing.T) {
	arrived := make(chan struct{})
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		close(arrived)
		<-r.Context().Done()
	}, 5*time.Second)
	ctx, cancel := context.WithCancel(context.Background())
	go func() { <-arrived; cancel() }()
	r := c.Send(ctx, testMsg)
	if r.Outcome != OutcomeUnknown || r.ErrorCode != "aborted" {
		t.Errorf("result = %+v", r)
	}
}

// A connection that cannot be established proves nothing was sent:
// transient_error, not ambiguous.
func TestConnectionRefusedIsTransient(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()
	c, err := NewClient(ClientConfig{BaseURL: "http://" + addr, RequestTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	r := c.Send(context.Background(), testMsg)
	if r.Outcome != OutcomeTransientError || r.ErrorCode != "connection_failed" || r.Outcome.Ambiguous() {
		t.Errorf("result = %+v", r)
	}
}

// A request the client cannot build is never sent.
func TestInvalidMessageIsNotSent(t *testing.T) {
	called := false
	c := newTestClient(t, func(http.ResponseWriter, *http.Request) { called = true }, time.Second)
	for _, m := range []Message{{Channel: "fax", IdempotencyKey: "k", To: "x", Text: "t"}, {Channel: ChannelSMS, To: "+90", Text: "t"},
		{Channel: ChannelSMS, IdempotencyKey: "k", Text: "t"}} {
		if r := c.Send(context.Background(), m); r.Outcome != OutcomePermanentError || r.ErrorCode != "invalid_request" {
			t.Errorf("%+v → %+v", m, r)
		}
	}
	if called {
		t.Error("an invalid message reached the provider")
	}
}

func TestParseRetryAfter(t *testing.T) {
	now := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)
	limit := 30 * time.Second
	for _, tc := range []struct {
		in   string
		want time.Duration
	}{
		{"", time.Second}, {"0", time.Second}, {"-5", time.Second}, {"abc", time.Second},
		{"1", time.Second}, {"7", 7 * time.Second}, {"30", 30 * time.Second}, {"31", 30 * time.Second},
		{"99999999999999999999", time.Second}, // overflow → invalid → default
		{now.Add(10 * time.Second).Format(http.TimeFormat), 10 * time.Second},
		{now.Add(-10 * time.Second).Format(http.TimeFormat), time.Second},
		{now.Add(time.Hour).Format(http.TimeFormat), 30 * time.Second},
	} {
		if got := ParseRetryAfter(tc.in, now, limit); got != tc.want {
			t.Errorf("%q = %s, want %s", tc.in, got, tc.want)
		}
	}
}

func TestValidateBaseURL(t *testing.T) {
	for _, ok := range []string{"http://mockprovider:8090", "https://p.example.test/base/", "http://127.0.0.1:1"} {
		if _, err := ValidateBaseURL(ok); err != nil {
			t.Errorf("%s: %v", ok, err)
		}
	}
	for _, bad := range []string{"", "mockprovider:8090", "ftp://x", "http://", "http://u:p@x", "http://x/?q=1", "http://x/#f", "/relative"} {
		if _, err := ValidateBaseURL(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestMockCapabilities(t *testing.T) {
	c, err := NewClient(ClientConfig{BaseURL: "http://mockprovider:8090"})
	if err != nil {
		t.Fatal(err)
	}
	if caps := c.Capabilities(); !caps.IdempotencyKeys || caps.KeyRetention != RetentionUntilReset || caps.Name != "mock" {
		t.Errorf("caps = %+v", caps)
	}
}
