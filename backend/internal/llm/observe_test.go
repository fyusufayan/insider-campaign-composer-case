package llm

import (
	"context"
	"errors"
	"testing"
	"time"
)

type observation struct {
	task, outcome string
	d             time.Duration
}

func TestObserveReportsEveryRequestOnce(t *testing.T) {
	var got []observation
	s := NewScripted(Reply(`{"a":1}`), Stopped("cut", StopMaxTokens), Stopped("", StopRefusal),
		Fail(&Error{Kind: ErrUnavailable, StatusCode: 529}), Fail(&Error{Kind: ErrRejected, StatusCode: 400}), Reply("streamed text"))
	c := Observe(s, func(task, outcome string, d time.Duration) { got = append(got, observation{task, outcome, d}) })
	ctx := context.Background()
	for i := 0; i < 5; i++ {
		_, _ = c.Complete(ctx, Request{Task: "segment_interpretation"})
	}
	stop := errors.New("consumer stopped")
	_, err := c.Stream(ctx, Request{Task: "content_generation"}, func(StreamEvent) error { return stop })
	if !errors.Is(err, stop) {
		t.Fatalf("stream err = %v (must pass through unchanged)", err)
	}
	want := []string{OutcomeOK, OutcomeMaxTokens, OutcomeRefused, OutcomeUnavailable, OutcomeRejected, OutcomeAborted}
	if len(got) != len(want) {
		t.Fatalf("observations = %+v", got)
	}
	for i, o := range got {
		if o.outcome != want[i] || o.d < 0 {
			t.Errorf("request %d: %+v, want %s", i, o, want[i])
		}
	}
	if got[0].task != "segment_interpretation" || got[5].task != "content_generation" {
		t.Errorf("tasks = %+v", got)
	}
}

func TestClassifyContextOutcomes(t *testing.T) {
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	expired, cancel2 := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel2()
	cases := []struct {
		ctx  context.Context
		err  error
		want string
	}{
		{cancelled, context.Canceled, OutcomeCancelled},
		{expired, context.DeadlineExceeded, OutcomeTimeout},
		{context.Background(), errors.New("boom"), OutcomeError},
		{context.Background(), nil, OutcomeError}, // a response without a known stop reason
	}
	for _, c := range cases {
		if got := Classify(c.ctx, Response{}, c.err, false); got != c.want {
			t.Errorf("%v → %s, want %s", c.err, got, c.want)
		}
	}
	if Observe(NewScripted(), nil) == nil {
		t.Error("nil observer must return the client")
	}
}
