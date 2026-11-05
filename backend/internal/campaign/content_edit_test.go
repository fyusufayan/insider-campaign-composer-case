package campaign_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/fyusufayan/insider-campaign-composer-case/backend/internal/ai"
	"github.com/fyusufayan/insider-campaign-composer-case/backend/internal/campaign"
	"github.com/fyusufayan/insider-campaign-composer-case/backend/internal/content"
	"github.com/fyusufayan/insider-campaign-composer-case/backend/internal/llm"
	"github.com/fyusufayan/insider-campaign-composer-case/backend/internal/segment"
)

func ptr(s string) *string { return &s }

func draft(sms, subject, body string) content.Draft {
	d := content.Draft{}
	if sms != "" {
		d.SMSBody = ptr(sms)
	}
	if subject != "" {
		d.EmailSubject = ptr(subject)
	}
	if body != "" {
		d.EmailBody = ptr(body)
	}
	return d
}

func codes(errs []segment.FieldError) string {
	var out []string
	for _, e := range errs {
		out = append(out, e.Path+":"+e.Code)
	}
	return strings.Join(out, ",")
}

func TestWithEditedContent(t *testing.T) {
	f := newFixture(t)
	c := f.interpreted(t)
	d := draft(genSMS, genSubject, genBody)

	next, v, created, err := c.WithEditedContent(c.Version, nil, d)
	if err != nil || !created || next.Version != c.Version+1 || next.Status != campaign.StatusContentReady ||
		v.Number != 1 || v.Provenance.Origin != content.OriginEdited || v.Provenance.Model != "" {
		t.Fatalf("next = %+v, v = %+v, created = %v, err = %v", next, v, created, err)
	}
	if c.Status != campaign.StatusSegmented {
		t.Error("receiver modified")
	}

	// Stale version is checked before anything else.
	if _, _, _, err := c.WithEditedContent(c.Version-1, nil, draft("{{x}}", "", "")); !errors.Is(err, campaign.ErrVersionConflict) {
		t.Errorf("stale: %v", err)
	}
	// Invalid content → *ValidationError with all field problems.
	_, _, _, err = c.WithEditedContent(c.Version, nil, draft("{{x}}", "", ""))
	var ve *campaign.ValidationError
	if !errors.As(err, &ve) || codes(ve.Problems) != "sms_body:unknown_variable,email_subject:required,email_body:required" {
		t.Errorf("invalid: %v", err)
	}
	// Content for an unselected channel is refused.
	smsOnly := c
	smsOnly.Channels = []segment.Channel{segment.ChannelSMS}
	if _, _, _, err := smsOnly.WithEditedContent(c.Version, nil, d); !errors.As(err, &ve) ||
		codes(ve.Problems) != "email_subject:channel_not_selected,email_body:channel_not_selected" {
		t.Errorf("unselected: %v", err)
	}
	// No channels: nothing can be written.
	none := c
	none.Channels = []segment.Channel{}
	if _, _, _, err := none.WithEditedContent(c.Version, nil, draft(genSMS, "", "")); !errors.As(err, &ve) || ve.Problems[0].Path != "channels" {
		t.Errorf("no channels: %v", err)
	}

	// Unchanged canonical text (NFC + trim) → no-op, same version.
	id := int64(9)
	cur := content.Version{ID: id, Number: 1, Content: v.Content, Provenance: content.Provenance{Origin: content.OriginAI, Model: "m", PromptVersion: "p"}}
	withContent := next
	withContent.CurrentContentID = &id
	same := draft("  "+genSMS+"\n", genSubject, genBody)
	n2, v2, created, err := withContent.WithEditedContent(withContent.Version, &cur, same)
	if err != nil || created || v2.ID != id || n2.Version != withContent.Version {
		t.Errorf("no-op: v = %+v, created = %v, err = %v", v2, created, err)
	}
	// Any change → n+1.
	_, v3, created, err := withContent.WithEditedContent(withContent.Version, &cur, draft(genSMS+"!", genSubject, genBody))
	if err != nil || !created || v3.Number != 2 {
		t.Errorf("change: %+v %v %v", v3, created, err)
	}

	// States.
	for _, st := range []campaign.Status{campaign.StatusDraft, campaign.StatusPendingApproval, campaign.StatusApproved,
		campaign.StatusSending, campaign.StatusCompleted, campaign.StatusCompletedWithFailures, campaign.StatusCancelled} {
		x := c
		x.Status = st
		if _, _, _, err := x.WithEditedContent(c.Version, nil, d); !errors.Is(err, campaign.ErrInvalidState) {
			t.Errorf("%s: %v", st, err)
		}
	}
}

func TestValidateDraftAndStored(t *testing.T) {
	f := newFixture(t)
	c := f.interpreted(t) // sms + email, 30% discount authorized

	_, res := c.ValidateDraft(draft("Free shipping and a coupon! 30% off.", genSubject, genBody))
	if !res.OK() || !strings.Contains(codes(res.Warnings), "sms_body:unauthorized_offer") {
		t.Errorf("advisory: %+v", res)
	}
	_, res = c.ValidateDraft(draft("Hello {{first_name}}", genSubject, genBody))
	if !res.OK() || !strings.Contains(codes(res.Warnings), "sms_body:"+ai.WarnOfferNotMentioned) {
		t.Errorf("not mentioned: %+v", res)
	}

	// A stored version shown after the selection changed.
	full, _ := content.Parse(draft(genSMS, genSubject, genBody), smsEmail)
	v := content.Version{ID: 1, Number: 1, Content: full, Provenance: content.Provenance{Origin: content.OriginEdited}}
	smsOnly := c
	smsOnly.Channels = []segment.Channel{segment.ChannelSMS}
	res = smsOnly.ValidateStored(v)
	if !res.OK() || !strings.Contains(codes(res.Warnings), "email_subject:channel_not_selected") ||
		res.RenderedExample == nil || res.RenderedExample.Email != nil || res.RenderedExample.SMS == nil {
		t.Errorf("sms only: %+v", res)
	}
	smsContent, _ := content.Parse(draft(genSMS, "", ""), []segment.Channel{segment.ChannelSMS})
	res = c.ValidateStored(content.Version{ID: 2, Number: 2, Content: smsContent, Provenance: content.Provenance{Origin: content.OriginEdited}})
	if res.OK() || codes(res.Errors) != "email_subject:required,email_body:required" {
		t.Errorf("email newly selected: %+v", res)
	}
	emailOnly, _ := content.Parse(draft("", genSubject, genBody), []segment.Channel{segment.ChannelEmail})
	res = smsOnly.ValidateStored(content.Version{ID: 3, Number: 1, Content: emailOnly, Provenance: content.Provenance{Origin: content.OriginEdited}})
	if res.OK() || !strings.Contains(codes(res.Errors), "sms_body:required") {
		t.Errorf("only unselected content: %+v", res)
	}
}

func TestEditContentService(t *testing.T) {
	f := newFixture(t)
	c := f.interpreted(t)
	ctx := context.Background()

	res, err := f.svc.EditContent(ctx, c.ID, draft(genSMS, genSubject, genBody), c.Version)
	if err != nil || !res.Created || res.Version.Number != 1 || res.Campaign.Version != c.Version+1 || !res.Validation.OK() {
		t.Fatalf("res = %+v, err = %v", res, err)
	}
	again, err := f.svc.EditContent(ctx, c.ID, draft(genSMS, genSubject, genBody), res.Campaign.Version)
	if err != nil || again.Created || again.Version.ID != res.Version.ID || again.Campaign.Version != res.Campaign.Version {
		t.Fatalf("no-op = %+v, err = %v", again, err)
	}
	if _, err := f.svc.EditContent(ctx, c.ID, draft(genSMS+"!", genSubject, genBody), c.Version); !errors.Is(err, campaign.ErrVersionConflict) {
		t.Errorf("stale: %v", err)
	}
	view, err := f.svc.Content(ctx, c.ID)
	if err != nil || view.Version == nil || view.Version.ID != res.Version.ID || view.Validation == nil || !view.Validation.OK() {
		t.Errorf("view = %+v, err = %v", view, err)
	}
	dv, err := f.svc.ValidateContent(ctx, c.ID, draft("{{nope}}", genSubject, genBody))
	if err != nil || dv.Result.OK() || dv.CampaignVersion != res.Campaign.Version {
		t.Errorf("validate = %+v, err = %v", dv, err)
	}
	if n := len(f.store.ContentVersions(c.ID)); n != 1 {
		t.Errorf("versions = %d", n)
	}
}

// Two concurrent edits on the same version: exactly one wins, the other is a
// version conflict; never two versions with the same number.
func TestEditContentConcurrent(t *testing.T) {
	f := newFixture(t)
	c := f.interpreted(t)
	var wg sync.WaitGroup
	errs := make([]error, 8)
	for i := range errs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = f.svc.EditContent(context.Background(), c.ID, draft(genSMS+strings.Repeat("!", i+1), genSubject, genBody), c.Version)
		}(i)
	}
	wg.Wait()
	ok := 0
	for _, err := range errs {
		switch {
		case err == nil:
			ok++
		case !errors.Is(err, campaign.ErrVersionConflict):
			t.Errorf("err = %v", err)
		}
	}
	if ok != 1 || len(f.store.ContentVersions(c.ID)) != 1 {
		t.Errorf("winners = %d, versions = %d", ok, len(f.store.ContentVersions(c.ID)))
	}
}

// GenerateContentStream emits started → progress… → persisting, and an
// observer error stops it without saving.
func TestGenerateContentStreamEvents(t *testing.T) {
	f := newFixture(t)
	c := f.interpreted(t)
	f.llm.Push(llm.Reply(contentReply(smsEmail...)))
	var kinds []string
	res, err := f.svc.GenerateContentStream(context.Background(), c.ID, func(ev campaign.GenerationEvent) error {
		k := string(ev.Kind)
		if ev.Kind == campaign.GenerationProgress {
			k += ":" + string(ev.Progress.Kind)
		}
		if ev.Kind == campaign.GenerationStarted && (ev.Start.CampaignVersion != c.Version || len(ev.Start.Channels) != 2) {
			t.Errorf("start = %+v", ev.Start)
		}
		if n := len(kinds); n == 0 || kinds[n-1] != k {
			kinds = append(kinds, k)
		}
		return nil
	})
	if err != nil || !res.Created {
		t.Fatal(err)
	}
	want := "started progress:attempt progress:delta progress:validating persisting"
	if got := strings.Join(kinds, " "); got != want {
		t.Errorf("events = %s", got)
	}

	stop := errors.New("client gone")
	f.llm.Push(llm.Reply(contentReply(smsEmail...)))
	_, err = f.svc.GenerateContentStream(context.Background(), c.ID, func(ev campaign.GenerationEvent) error {
		if ev.Kind == campaign.GenerationPersisting {
			return stop
		}
		return nil
	})
	if !errors.Is(err, stop) || len(f.store.ContentVersions(c.ID)) != 1 {
		t.Errorf("err = %v, versions = %d", err, len(f.store.ContentVersions(c.ID)))
	}
}
