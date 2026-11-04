package campaign_test

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fyusufayan/insider-campaign-composer-case/backend/internal/ai"
	"github.com/fyusufayan/insider-campaign-composer-case/backend/internal/campaign"
	"github.com/fyusufayan/insider-campaign-composer-case/backend/internal/content"
	"github.com/fyusufayan/insider-campaign-composer-case/backend/internal/llm"
	"github.com/fyusufayan/insider-campaign-composer-case/backend/internal/segment"
)

const (
	genSMS     = "Hi {{first_name|there}}, your cart is waiting. Enjoy 30% off today."
	genSubject = "{{first_name|Hello}}, your cart is waiting: 30% off"
	genBody    = "Hi {{first_name|there}},\n\nYour cart is still waiting. Enjoy 30% off your order.\n\nSee you soon!"
)

func contentReply(chs ...segment.Channel) string {
	m := map[string]string{}
	for _, ch := range chs {
		if ch == segment.ChannelSMS {
			m[content.FieldSMSBody] = genSMS
		} else {
			m[content.FieldEmailSubject], m[content.FieldEmailBody] = genSubject, genBody
		}
	}
	b, _ := json.Marshal(m)
	return string(b)
}

var smsEmail = []segment.Channel{segment.ChannelSMS, segment.ChannelEmail}

func (f *fixture) generate(t *testing.T, id campaign.ID) campaign.ContentResult {
	t.Helper()
	res, err := f.svc.GenerateContent(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func TestGenerateContentHappyPathAndRegeneration(t *testing.T) {
	f := newFixture(t)
	c := f.interpreted(t) // segmented, version 2, channels sms+email
	f.llm.Push(llm.Reply(contentReply(smsEmail...)))
	res := f.generate(t, c.ID)

	got := res.Campaign
	if got.Status != campaign.StatusContentReady || got.Version != c.Version+1 || got.CurrentContentID == nil ||
		*got.CurrentContentID != res.Version.ID || got.SegmentVersion != c.SegmentVersion || !slices.Equal(got.Channels, c.Channels) {
		t.Fatalf("campaign = %+v", got)
	}
	v := res.Version
	if v.Number != 1 || v.Provenance != (content.Provenance{Origin: content.OriginAI, Model: "scripted-model", PromptVersion: ai.ContentPromptVersion}) {
		t.Errorf("version = %+v", v)
	}
	if !slices.Equal(v.Content.Channels(), smsEmail) || !res.Validation.OK() || res.Validation.RenderedExample == nil {
		t.Errorf("content = %v, validation = %+v", v.Content.Channels(), res.Validation)
	}
	req := f.llm.Requests()[1]
	if req.Task != ai.TaskContentGeneration || req.Model != "cfg-content-model" {
		t.Errorf("request = %+v", req)
	}

	// Regeneration with identical text still creates a new immutable version.
	f.llm.Push(llm.Reply(contentReply(smsEmail...)))
	res2 := f.generate(t, c.ID)
	if res2.Version.Number != 2 || res2.Version.ID == v.ID || *res2.Campaign.CurrentContentID != res2.Version.ID ||
		res2.Campaign.Version != got.Version+1 || res2.Campaign.Status != campaign.StatusContentReady {
		t.Errorf("regeneration = %+v / %+v", res2.Campaign, res2.Version)
	}
	if !res2.Version.Content.Equal(v.Content) {
		t.Error("identical text expected")
	}
	versions := f.store.ContentVersions(c.ID)
	if len(versions) != 2 || versions[0].ID != v.ID || !versions[0].Content.Equal(v.Content) {
		t.Errorf("versions = %+v", versions)
	}

	cur, cv, err := f.svc.CurrentContent(context.Background(), c.ID)
	if err != nil || cv == nil || cv.ID != res2.Version.ID || cur.Version != res2.Campaign.Version {
		t.Errorf("current = %+v %+v %v", cur, cv, err)
	}
	if !strings.Contains(f.logs.String(), `"msg":"content version saved"`) {
		t.Error("no save log line")
	}
}

// Content is generated for exactly the selected channels.
func TestGenerateContentPerChannelSelection(t *testing.T) {
	for _, chs := range [][]segment.Channel{{segment.ChannelSMS}, {segment.ChannelEmail}, smsEmail} {
		t.Run(string(chs[len(chs)-1])+"/"+string(chs[0]), func(t *testing.T) {
			f := newFixture(t)
			c := f.interpreted(t)
			c, err := f.svc.SetChannels(context.Background(), c.ID, chs, c.Version)
			if err != nil {
				t.Fatal(err)
			}
			f.llm.Push(llm.Reply(contentReply(chs...)))
			res := f.generate(t, c.ID)
			if !slices.Equal(res.Version.Content.Channels(), chs) || !slices.Equal(res.Campaign.Channels, chs) {
				t.Errorf("content channels = %v, campaign channels = %v", res.Version.Content.Channels(), res.Campaign.Channels)
			}
			req := f.llm.Requests()[len(f.llm.Requests())-1]
			props := req.JSONSchema["properties"].(map[string]any)
			if len(props) != 2*len(chs)-boolInt(slices.Contains(chs, segment.ChannelSMS)) {
				t.Errorf("schema properties = %v", props)
			}
		})
	}
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// An empty selection is a clear, non-retryable caller error; the model is
// never called and nothing changes.
func TestGenerateContentNoChannels(t *testing.T) {
	f := newFixture(t)
	c := f.interpreted(t)
	c, err := f.svc.SetChannels(context.Background(), c.ID, []segment.Channel{}, c.Version)
	if err != nil {
		t.Fatal(err)
	}
	calls, updates := f.llm.Calls(), f.store.Updates()
	_, err = f.svc.GenerateContent(context.Background(), c.ID)
	var ve *campaign.ValidationError
	if !errors.As(err, &ve) || !errors.Is(err, campaign.ErrInvalidInput) || len(ve.Problems) != 1 ||
		ve.Problems[0].Path != "channels" || ve.Problems[0].Code != segment.CodeRequired {
		t.Fatalf("err = %v", err)
	}
	if f.llm.Calls() != calls || f.store.Updates() != updates || len(f.store.ContentVersions(c.ID)) != 0 {
		t.Error("state changed or model called")
	}
}

// Generation is refused (409) in every status where it could invalidate an
// approval, freeze or send, or before interpretation.
func TestGenerateContentInvalidStatuses(t *testing.T) {
	f := newFixture(t)
	base := f.interpreted(t)
	for i, st := range []campaign.Status{
		campaign.StatusDraft, campaign.StatusPendingApproval, campaign.StatusApproved, campaign.StatusSending,
		campaign.StatusCompleted, campaign.StatusCompletedWithFailures, campaign.StatusCancelled,
	} {
		t.Run(string(st), func(t *testing.T) {
			c := base
			c.ID = campaign.ID{0xAB, byte(i + 1)}
			c.Status = st
			f.store.Put(c)
			calls := f.llm.Calls()
			_, err := f.svc.GenerateContent(context.Background(), c.ID)
			var se *campaign.StateError
			if !errors.As(err, &se) || !errors.Is(err, campaign.ErrInvalidState) || se.Op != campaign.OpGenerateContent || se.Status != st {
				t.Errorf("err = %v", err)
			}
			if f.llm.Calls() != calls || len(f.store.ContentVersions(c.ID)) != 0 {
				t.Error("model called or content stored")
			}
		})
	}
	t.Run("live approval id", func(t *testing.T) {
		c := base
		c.ID = campaign.ID{0xEE}
		aid := campaign.ID{0x01}
		c.ActiveApprovalID = &aid
		f.store.Put(c)
		if _, err := f.svc.GenerateContent(context.Background(), c.ID); !errors.Is(err, campaign.ErrInvalidState) {
			t.Errorf("err = %v", err)
		}
	})
	t.Run("not found", func(t *testing.T) {
		if _, err := f.svc.GenerateContent(context.Background(), campaign.ID{0xFF}); !errors.Is(err, campaign.ErrNotFound) {
			t.Errorf("err = %v", err)
		}
	})
}

// Model failures leave the campaign unchanged and store nothing; logs never
// carry the proposal or generated text.
func TestGenerateContentFailuresChangeNothing(t *testing.T) {
	invalid := `{"sms_body":"Hi {{email}}, 50% off","email_subject":"x","email_body":"<b>y</b>"}`
	tests := []struct {
		name  string
		steps []llm.Step
		kind  error
	}{
		{"invalid twice", []llm.Step{llm.Reply(invalid), llm.Reply(invalid)}, ai.ErrOutputInvalid},
		{"refusal", []llm.Step{llm.Stopped(contentReply(smsEmail...), llm.StopRefusal)}, ai.ErrLLMRefused},
		{"truncated", []llm.Step{llm.Stopped(`{"sms_body":"Hi`, llm.StopMaxTokens), llm.Stopped(`{"sms_body":"Hi`, llm.StopMaxTokens)}, ai.ErrLLMTruncated},
		{"unavailable", []llm.Step{llm.Fail(&llm.Error{Kind: llm.ErrUnavailable, StatusCode: 503})}, ai.ErrLLMUnavailable},
		{"rejected", []llm.Step{llm.Fail(&llm.Error{Kind: llm.ErrRejected, StatusCode: 400})}, ai.ErrLLMRejected},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t)
			c := f.interpreted(t)
			before, _ := f.store.Get(context.Background(), c.ID)
			updates := f.store.Updates()
			f.logs.Reset()
			f.llm.Push(tt.steps...)
			_, err := f.svc.GenerateContent(context.Background(), c.ID)
			if !errors.Is(err, tt.kind) {
				t.Fatalf("err = %v, want %v", err, tt.kind)
			}
			after, _ := f.store.Get(context.Background(), c.ID)
			if !reflect.DeepEqual(before, after) || f.store.Updates() != updates || len(f.store.ContentVersions(c.ID)) != 0 {
				t.Errorf("state changed:\nbefore %+v\nafter  %+v", before, after)
			}
			logs := f.logs.String()
			if !strings.Contains(logs, `"msg":"content generation failed"`) {
				t.Errorf("no failure log: %s", logs)
			}
			for _, s := range []string{"{{email}}", "<b>", "your cart is waiting", "cart_recovery"} {
				if strings.Contains(logs, s) {
					t.Errorf("logs contain %q: %s", s, logs)
				}
			}
		})
	}
}

// An edit that commits while the model is working makes the result stale:
// it is discarded with a version conflict and nothing is inserted.
func TestGenerateContentConcurrentEditDiscardsResult(t *testing.T) {
	f := newFixture(t)
	c := f.interpreted(t)
	entered, release := make(chan struct{}), make(chan struct{})
	f.llm.Push(llm.Step{Func: func(ctx context.Context, _ llm.Request) (llm.Response, error) {
		close(entered)
		select {
		case <-release:
		case <-ctx.Done():
			return llm.Response{}, ctx.Err()
		}
		return llm.Response{Text: contentReply(smsEmail...), StopReason: llm.StopEndTurn, Model: "scripted-model"}, nil
	}})
	done := make(chan error, 1)
	go func() {
		_, err := f.svc.GenerateContent(context.Background(), c.ID)
		done <- err
	}()
	<-entered
	edited, err := f.svc.SetChannels(context.Background(), c.ID, []segment.Channel{segment.ChannelEmail}, c.Version)
	if err != nil {
		t.Fatal(err)
	}
	close(release)
	select {
	case err = <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("generation did not finish")
	}
	var ce *campaign.ConflictError
	if !errors.As(err, &ce) || ce.Expected != c.Version || ce.Current != edited.Version {
		t.Fatalf("err = %v", err)
	}
	after, _ := f.store.Get(context.Background(), c.ID)
	if after.Version != edited.Version || after.CurrentContentID != nil || after.Status != campaign.StatusSegmented ||
		len(f.store.ContentVersions(c.ID)) != 0 {
		t.Errorf("stale content persisted: %+v", after)
	}
	if !strings.Contains(f.logs.String(), "generated content discarded") {
		t.Error("no discard log")
	}
}

// Two generations started from the same version: exactly one is stored.
func TestGenerateContentConcurrentGenerations(t *testing.T) {
	f := newFixture(t)
	c := f.interpreted(t)
	const n = 4
	var ready sync.WaitGroup
	ready.Add(n)
	release := make(chan struct{})
	for range n {
		f.llm.Push(llm.Step{Func: func(ctx context.Context, _ llm.Request) (llm.Response, error) {
			ready.Done()
			<-release
			return llm.Response{Text: contentReply(smsEmail...), StopReason: llm.StopEndTurn, Model: "scripted-model"}, nil
		}})
	}
	errs := make(chan error, n)
	for range n {
		go func() {
			_, err := f.svc.GenerateContent(context.Background(), c.ID)
			errs <- err
		}()
	}
	ready.Wait()
	close(release)
	ok, conflicts := 0, 0
	for range n {
		switch err := <-errs; {
		case err == nil:
			ok++
		case errors.Is(err, campaign.ErrVersionConflict):
			conflicts++
		default:
			t.Errorf("err = %v", err)
		}
	}
	after, _ := f.store.Get(context.Background(), c.ID)
	versions := f.store.ContentVersions(c.ID)
	if ok != 1 || conflicts != n-1 || len(versions) != 1 || after.Version != c.Version+1 ||
		*after.CurrentContentID != versions[0].ID || versions[0].Number != 1 {
		t.Errorf("ok=%d conflicts=%d versions=%d campaign=%+v", ok, conflicts, len(versions), after)
	}
}

// The prompt never contains customer data; the content brief comes from the
// stored, re-validated row.
func TestGenerateContentPromptHasNoCustomerData(t *testing.T) {
	f := newFixture(t)
	c := f.interpreted(t)
	f.llm.Push(llm.Reply(contentReply(smsEmail...)))
	f.generate(t, c.ID)
	req := f.llm.Requests()[len(f.llm.Requests())-1]
	all := req.System
	for _, m := range req.Messages {
		all += m.Content
	}
	ex := content.ExampleValues()
	for _, s := range []string{ex.FirstName, ex.LastName, "@example", "+90", c.Brief} {
		if strings.Contains(all, s) {
			t.Errorf("prompt contains %q", s)
		}
	}
	if !strings.Contains(all, `"goal":"cart_recovery"`) || !strings.Contains(all, `"value":30`) {
		t.Errorf("prompt lacks the content brief:\n%s", all)
	}
}

// A corrupted stored content brief is never sent to the model.
func TestGenerateContentRejectsInvalidStoredBrief(t *testing.T) {
	f := newFixture(t)
	c := f.interpreted(t)
	c.ContentBrief = json.RawMessage(`{"goal":"x","tone":"y","length":"short","offer":{"kind":"none"},"extra":1}`)
	f.store.Put(c)
	calls := f.llm.Calls()
	if _, err := f.svc.GenerateContent(context.Background(), c.ID); err == nil || f.llm.Calls() != calls {
		t.Errorf("err = %v, calls %d → %d", err, calls, f.llm.Calls())
	}
}

// ---- pure transition ----

func aiContent(t *testing.T, chs []segment.Channel) content.Content {
	t.Helper()
	d := content.Draft{}
	for _, ch := range chs {
		if ch == segment.ChannelSMS {
			s := genSMS
			d.SMSBody = &s
		} else {
			s, b := genSubject, genBody
			d.EmailSubject, d.EmailBody = &s, &b
		}
	}
	c, res := content.Parse(d, chs)
	if !res.OK() {
		t.Fatal(res.Errors)
	}
	return c
}

func TestWithAIContent(t *testing.T) {
	f := newFixture(t)
	c := f.interpreted(t)
	prov := content.Provenance{Origin: content.OriginAI, Model: "m", PromptVersion: ai.ContentPromptVersion}
	gen := aiContent(t, smsEmail)

	next, v, err := c.WithAIContent(c.Version, c.Channels, nil, gen, prov)
	if err != nil || next.Status != campaign.StatusContentReady || next.Version != c.Version+1 || v.Number != 1 {
		t.Fatalf("next = %+v, v = %+v, err = %v", next, v, err)
	}
	if c.Status != campaign.StatusSegmented || c.Version == next.Version {
		t.Error("receiver modified")
	}

	if _, _, err := c.WithAIContent(c.Version-1, c.Channels, nil, gen, prov); !errors.Is(err, campaign.ErrVersionConflict) {
		t.Errorf("stale: %v", err)
	}
	if _, _, err := c.WithAIContent(c.Version, []segment.Channel{segment.ChannelSMS}, nil, gen, prov); !errors.Is(err, campaign.ErrVersionConflict) {
		t.Errorf("channels changed: %v", err)
	}
	// Content that covers a channel that is not selected, or misses one.
	if _, _, err := c.WithAIContent(c.Version, c.Channels, nil, aiContent(t, []segment.Channel{segment.ChannelSMS}), prov); err == nil {
		t.Error("missing email content accepted")
	}
	smsOnly := c
	smsOnly.Channels = []segment.Channel{segment.ChannelSMS}
	if _, _, err := smsOnly.WithAIContent(c.Version, smsOnly.Channels, nil, gen, prov); err == nil {
		t.Error("unselected email content accepted")
	}
	if _, _, err := c.WithAIContent(c.Version, c.Channels, nil, gen, content.Provenance{Origin: content.OriginEdited}); err == nil {
		t.Error("edited provenance accepted")
	}
	if _, _, err := c.WithAIContent(c.Version, c.Channels, nil, gen, content.Provenance{Origin: content.OriginAI}); err == nil {
		t.Error("ai provenance without model accepted")
	}
	// The current version must be the one the campaign points at.
	prev := content.Version{ID: 7, Number: 3, Content: gen, Provenance: prov}
	if _, _, err := c.WithAIContent(c.Version, c.Channels, &prev, gen, prov); err == nil {
		t.Error("inconsistent current content accepted")
	}
	id := int64(7)
	c.CurrentContentID = &id
	c.Status = campaign.StatusContentReady
	next, v, err = c.WithAIContent(c.Version, c.Channels, &prev, gen, prov)
	if err != nil || v.Number != 4 || next.Status != campaign.StatusContentReady {
		t.Errorf("after v3: v = %+v, err = %v", v, err)
	}
}

// The edit/no-op version semantics of CP11 are unchanged: an edit with the
// stored canonical text creates nothing, AI regeneration always does.
func TestVersionSemanticsAfterGeneration(t *testing.T) {
	gen := aiContent(t, smsEmail)
	stored := content.Version{ID: 1, Number: 1, Content: gen,
		Provenance: content.Provenance{Origin: content.OriginAI, Model: "m", PromptVersion: ai.ContentPromptVersion}}
	reparsed, err := campaign.ParseStoredContent(gen.Draft())
	if err != nil {
		t.Fatal(err)
	}
	if _, created, err := content.NextVersion(&stored, reparsed, content.Provenance{Origin: content.OriginEdited}); err != nil || created {
		t.Errorf("unchanged edit created a version (err %v)", err)
	}
	if v, created, err := content.NextVersion(&stored, reparsed, stored.Provenance); err != nil || !created || v.Number != 2 {
		t.Errorf("regeneration: v = %+v created = %v err = %v", v, created, err)
	}
}

func TestParseStoredContentRejectsNonCanonicalOrInvalid(t *testing.T) {
	str := func(s string) *string { return &s }
	for _, d := range []content.Draft{
		{SMSBody: str(" Hi {{first_name}} ")},  // not trimmed
		{SMSBody: str("Café {{first_name}}")}, // not NFC
		{SMSBody: str("Hi {{email}}")},         // invalid placeholder
		{EmailSubject: str("Subject only")},    // unpaired email
		{},                                     // no content
	} {
		if _, err := campaign.ParseStoredContent(d); err == nil {
			t.Errorf("accepted %+v", d)
		}
	}
	if _, err := campaign.ParseStoredContent(content.Draft{SMSBody: str(genSMS)}); err != nil {
		t.Errorf("valid stored sms rejected: %v", err)
	}
}
