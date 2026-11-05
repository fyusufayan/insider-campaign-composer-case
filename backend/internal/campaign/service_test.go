package campaign_test

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/fyusufayan/insider-campaign-composer-case/backend/internal/ai"
	"github.com/fyusufayan/insider-campaign-composer-case/backend/internal/campaign"
	"github.com/fyusufayan/insider-campaign-composer-case/backend/internal/campaign/campaigntest"
	"github.com/fyusufayan/insider-campaign-composer-case/backend/internal/llm"
	"github.com/fyusufayan/insider-campaign-composer-case/backend/internal/platform/logging"
	"github.com/fyusufayan/insider-campaign-composer-case/backend/internal/segment"
)

const (
	brief      = "Send a short and friendly 30% discount reminder to users in Istanbul who added products to their cart in the last 30 days but did not complete a purchase."
	validReply = `{"segment":{"match":"all","conditions":[{"type":"attribute","field":"city","op":"eq","value":"Istanbul"},{"type":"behavior","predicate":"abandoned_cart","within_days":30}]},` +
		`"interpretation":"Users in Istanbul with an abandoned cart.","assumptions":[],"unsupported":[],` +
		`"suggested_channels":["sms","email"],` +
		`"content_brief":{"goal":"cart_recovery","tone":"friendly","length":"short","offer":{"kind":"percent_discount","value":30}}}`
	invalidReply = `{"segment":{"match":"all","conditions":[{"type":"attribute","field":"email_opt_in","op":"eq","value":"yes"}]},` +
		`"interpretation":"x","assumptions":[],"unsupported":[],"suggested_channels":["sms"],` +
		`"content_brief":{"goal":"g","tone":"t","length":"short","offer":{"kind":"none"}}}`
)

type fixture struct {
	svc      *campaign.Service
	store    *campaigntest.MemoryStore
	audience *campaigntest.Audience
	llm      *llm.Scripted
	logs     *bytes.Buffer
	it       *ai.Interpreter
	gen      *ai.Generator
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	f := &fixture{store: campaigntest.NewMemoryStore(), audience: &campaigntest.Audience{}, llm: llm.NewScripted(), logs: &bytes.Buffer{}}
	it, err := ai.NewInterpreter(f.llm, ai.InterpreterConfig{Model: "cfg-model", Effort: llm.EffortLow, Timeout: 5 * time.Second, MaxTokens: 1000})
	if err != nil {
		t.Fatal(err)
	}
	gen, err := ai.NewGenerator(f.llm, ai.GeneratorConfig{Model: "cfg-content-model", Effort: llm.EffortMedium, Timeout: 5 * time.Second, MaxTokens: 2000})
	if err != nil {
		t.Fatal(err)
	}
	f.it, f.gen = it, gen
	f.svc, err = campaign.NewService(f.store, f.audience, it, gen, logging.New(f.logs, slog.LevelDebug))
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *fixture) create(t *testing.T) campaign.Campaign {
	t.Helper()
	c, err := f.svc.Create(context.Background(), "Cart recovery", brief)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func (f *fixture) interpreted(t *testing.T) campaign.Campaign {
	t.Helper()
	c := f.create(t)
	f.llm.Push(llm.Reply(validReply))
	res, err := f.svc.Interpret(context.Background(), c.ID)
	if err != nil {
		t.Fatal(err)
	}
	return res.Campaign
}

func TestServiceInterpretHappyPath(t *testing.T) {
	f := newFixture(t)
	c := f.create(t)
	f.llm.Push(llm.Reply(validReply))
	res, err := f.svc.Interpret(context.Background(), c.ID)
	if err != nil {
		t.Fatal(err)
	}
	if res.Campaign.Status != campaign.StatusSegmented || res.Campaign.Version != 2 || res.Campaign.SegmentVersion != 1 {
		t.Errorf("campaign = %+v", res.Campaign)
	}
	if res.Audience == nil || res.AudienceErr != nil || res.Audience.Messages != 12 {
		t.Errorf("audience = %+v err %v", res.Audience, res.AudienceErr)
	}
	// The brief stored at creation is what the model received.
	reqs := f.llm.Requests()
	if len(reqs) != 1 || !strings.Contains(reqs[0].Messages[0].Content, "Istanbul who added products") {
		t.Errorf("requests = %+v", reqs)
	}
	got, _ := f.svc.Get(context.Background(), c.ID)
	if !reflect.DeepEqual(got, res.Campaign) {
		t.Errorf("stored != returned")
	}
	if strings.Contains(f.logs.String(), "Istanbul who added products") {
		t.Error("the brief was logged")
	}
}

func TestServiceInterpretRepair(t *testing.T) {
	f := newFixture(t)
	c := f.create(t)
	f.llm.Push(llm.Reply(invalidReply), llm.Reply(validReply))
	res, err := f.svc.Interpret(context.Background(), c.ID)
	if err != nil {
		t.Fatal(err)
	}
	if f.llm.Calls() != 2 || res.Campaign.SegmentVersion != 1 {
		t.Errorf("calls = %d, campaign = %+v", f.llm.Calls(), res.Campaign)
	}
}

// Every failure leaves the previously committed campaign untouched.
func TestServiceInterpretFailurePreservesState(t *testing.T) {
	tests := []struct {
		name  string
		steps []llm.Step
		kind  error
	}{
		{"invalid twice", []llm.Step{llm.Reply(invalidReply), llm.Reply(invalidReply)}, ai.ErrOutputInvalid},
		{"refusal", []llm.Step{llm.Stopped(validReply, llm.StopRefusal)}, ai.ErrLLMRefused},
		{"truncated twice", []llm.Step{llm.Stopped("{", llm.StopMaxTokens), llm.Stopped("{", llm.StopMaxTokens)}, ai.ErrLLMTruncated},
		{"provider unavailable", []llm.Step{llm.Fail(&llm.Error{Kind: llm.ErrUnavailable, StatusCode: 529})}, ai.ErrLLMUnavailable},
		{"provider rejected", []llm.Step{llm.Fail(&llm.Error{Kind: llm.ErrRejected, StatusCode: 401})}, ai.ErrLLMRejected},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t)
			before := f.interpreted(t)
			updates := f.store.Updates()
			audienceCalls := f.audience.Calls()

			f.llm.Push(tt.steps...)
			_, err := f.svc.Interpret(context.Background(), before.ID)
			if !errors.Is(err, tt.kind) {
				t.Fatalf("err = %v, want %v", err, tt.kind)
			}
			after, _ := f.svc.Get(context.Background(), before.ID)
			if !reflect.DeepEqual(after, before) || f.store.Updates() != updates {
				t.Errorf("campaign changed:\nbefore %+v\nafter  %+v", before, after)
			}
			if f.audience.Calls() != audienceCalls {
				t.Error("an audience query ran for a failed interpretation")
			}
		})
	}
}

func TestServiceInterpretCancelled(t *testing.T) {
	f := newFixture(t)
	c := f.create(t)
	ctx, cancel := context.WithCancel(context.Background())
	f.llm.Push(llm.Step{Func: func(ctx context.Context, _ llm.Request) (llm.Response, error) {
		cancel()
		<-ctx.Done()
		return llm.Response{}, ctx.Err()
	}})
	if _, err := f.svc.Interpret(ctx, c.ID); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if f.store.Updates() != 0 {
		t.Error("a cancelled interpretation was persisted")
	}
}

// A channel edit that commits while the model is working makes the result
// stale: it is discarded, and the edit survives.
func TestServiceInterpretStaleResultDiscarded(t *testing.T) {
	f := newFixture(t)
	c := f.interpreted(t)
	f.llm.Push(llm.Step{Func: func(ctx context.Context, req llm.Request) (llm.Response, error) {
		if _, err := f.svc.SetChannels(ctx, c.ID, []segment.Channel{segment.ChannelEmail}, c.Version); err != nil {
			t.Errorf("concurrent edit: %v", err)
		}
		return llm.Response{Text: strings.Replace(validReply, `"Istanbul"`, `"Ankara"`, 1), StopReason: llm.StopEndTurn}, nil
	}})
	_, err := f.svc.Interpret(context.Background(), c.ID)
	var ce *campaign.ConflictError
	if !errors.As(err, &ce) || ce.Expected != c.Version || ce.Current != c.Version+1 {
		t.Fatalf("err = %v, want conflict", err)
	}
	got, _ := f.svc.Get(context.Background(), c.ID)
	if !slices.Equal(got.Channels, []segment.Channel{segment.ChannelEmail}) || got.SegmentVersion != 1 ||
		!strings.Contains(mustRule(t, got), "istanbul") {
		t.Errorf("stored = %+v", got)
	}
	if !strings.Contains(f.logs.String(), "interpretation discarded") {
		t.Error("discard not logged")
	}
}

func TestServiceInterpretNotAllowed(t *testing.T) {
	f := newFixture(t)
	c := f.interpreted(t)
	c.Status = campaign.StatusSending
	f.store.Put(c)
	if _, err := f.svc.Interpret(context.Background(), c.ID); !errors.Is(err, campaign.ErrInvalidState) {
		t.Fatalf("err = %v", err)
	}
	if f.llm.Calls() != 1 {
		t.Error("the LLM was called for a campaign that cannot be interpreted")
	}
	if _, err := f.svc.Interpret(context.Background(), campaign.ID{9}); !errors.Is(err, campaign.ErrNotFound) {
		t.Errorf("missing: err = %v", err)
	}
}

func TestServiceInterpretAudienceFailureKeepsInterpretation(t *testing.T) {
	f := newFixture(t)
	c := f.create(t)
	f.audience.Func = func(context.Context, segment.Validated, []segment.Channel) (segment.AudienceStats, error) {
		return segment.AudienceStats{}, &pgconn.PgError{Code: "57014", Message: "canceling statement due to statement timeout"}
	}
	f.llm.Push(llm.Reply(validReply))
	res, err := f.svc.Interpret(context.Background(), c.ID)
	if err != nil {
		t.Fatal(err)
	}
	if res.Audience != nil || !errors.Is(res.AudienceErr, campaign.ErrAudienceUnavailable) {
		t.Errorf("audience = %v, err = %v", res.Audience, res.AudienceErr)
	}
	if res.Campaign.SegmentVersion != 1 {
		t.Error("interpretation not committed")
	}
}

func TestServiceAudience(t *testing.T) {
	f := newFixture(t)
	d := f.create(t)
	if _, err := f.svc.Audience(context.Background(), d.ID); !errors.Is(err, campaign.ErrInvalidState) {
		t.Errorf("draft: err = %v", err)
	}
	if f.audience.Calls() != 0 {
		t.Error("audience query for a draft")
	}

	c := f.interpreted(t)
	c, err := f.svc.SetChannels(context.Background(), c.ID, []segment.Channel{}, c.Version)
	if err != nil {
		t.Fatal(err)
	}
	p, err := f.svc.Audience(context.Background(), c.ID)
	if err != nil {
		t.Fatal(err)
	}
	if p.Stats.MatchedUsers != 10 || p.Stats.Messages != 0 || len(p.Channels) != 0 || p.CampaignVersion != c.Version {
		t.Errorf("preview = %+v", p)
	}
	after, _ := f.svc.Get(context.Background(), c.ID)
	if after.Version != c.Version {
		t.Error("audience preview changed the version")
	}

	f.audience.Func = func(context.Context, segment.Validated, []segment.Channel) (segment.AudienceStats, error) {
		return segment.AudienceStats{}, context.DeadlineExceeded
	}
	if _, err := f.svc.Audience(context.Background(), c.ID); !errors.Is(err, campaign.ErrAudienceUnavailable) {
		t.Errorf("timeout: err = %v", err)
	}
}

func TestServiceSetChannelsConcurrent(t *testing.T) {
	f := newFixture(t)
	c := f.interpreted(t)
	const n = 8
	errs := make(chan error, n)
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		go func() {
			<-start
			_, err := f.svc.SetChannels(context.Background(), c.ID, []segment.Channel{segment.ChannelEmail}, c.Version)
			errs <- err
		}()
	}
	close(start)
	ok, conflicts := 0, 0
	for i := 0; i < n; i++ {
		switch err := <-errs; {
		case err == nil:
			ok++
		case errors.Is(err, campaign.ErrVersionConflict):
			conflicts++
		default:
			t.Errorf("err = %v", err)
		}
	}
	if ok != 1 || conflicts != n-1 {
		t.Errorf("ok = %d, conflicts = %d", ok, conflicts)
	}
}

func mustRule(t *testing.T, c campaign.Campaign) string {
	t.Helper()
	b, err := c.Segment.MarshalJSON()
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
