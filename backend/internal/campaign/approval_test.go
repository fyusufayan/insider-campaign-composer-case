package campaign_test

import (
	"errors"
	"slices"
	"testing"

	"github.com/fyusufayan/insider-campaign-composer-case/backend/internal/ai"
	"github.com/fyusufayan/insider-campaign-composer-case/backend/internal/campaign"
	"github.com/fyusufayan/insider-campaign-composer-case/backend/internal/content"
	"github.com/fyusufayan/insider-campaign-composer-case/backend/internal/segment"
)

// readyCampaign returns an interpreted campaign (sms+email, 30% offer) in
// content_ready whose current version (id 42) has the given text.
func readyCampaign(t *testing.T, d content.Draft) (campaign.Campaign, content.Version) {
	t.Helper()
	f := newFixture(t)
	c := f.interpreted(t)
	chs := []segment.Channel{}
	if d.SMSBody != nil {
		chs = append(chs, segment.ChannelSMS)
	}
	if d.EmailSubject != nil {
		chs = append(chs, segment.ChannelEmail)
	}
	parsed, res := content.Parse(d, chs)
	if !res.OK() {
		t.Fatalf("fixture content: %v", res.Errors)
	}
	v, _, err := content.NextVersion(nil, parsed, content.Provenance{Origin: content.OriginEdited})
	if err != nil {
		t.Fatal(err)
	}
	v.ID = 42
	c.CurrentContentID = &v.ID
	c.Status = campaign.StatusContentReady
	return c, v
}

func approvalID(b byte) campaign.ID {
	var id campaign.ID
	id[0], id[15] = b, b
	return id
}

func TestCheckPrepareApprovalStates(t *testing.T) {
	base, _ := readyCampaign(t, draft(genSMS, genSubject, genBody))
	live := approvalID(1)
	cases := []struct {
		status    campaign.Status
		active    *campaign.ID
		wantState bool // *StateError
		wantOK    bool
	}{
		{campaign.StatusDraft, nil, true, false},
		{campaign.StatusSegmented, nil, true, false},
		{campaign.StatusContentReady, nil, false, true},
		{campaign.StatusPendingApproval, &live, false, true},
		// approved is refused (CP14, §9.1): withdraw first, never a silent supersede.
		{campaign.StatusApproved, &live, true, false},
		{campaign.StatusSending, &live, true, false},
		{campaign.StatusCompleted, &live, true, false},
		{campaign.StatusCompletedWithFailures, &live, true, false},
		{campaign.StatusCancelled, nil, true, false},
	}
	for _, tc := range cases {
		c := base
		c.Status, c.ActiveApprovalID = tc.status, tc.active
		err := c.CheckPrepareApproval()
		var se *campaign.StateError
		switch {
		case tc.wantOK && err != nil:
			t.Errorf("%s: %v", tc.status, err)
		case tc.wantState && (!errors.As(err, &se) || se.Op != campaign.OpPrepareApproval || !errors.Is(err, campaign.ErrInvalidState)):
			t.Errorf("%s: want StateError, got %v", tc.status, err)
		}
	}

	// Inconsistent rows are internal errors, never silently accepted.
	c := base
	c.ActiveApprovalID = &live // content_ready with an approval
	if err := c.CheckPrepareApproval(); err == nil || errors.Is(err, campaign.ErrInvalidState) {
		t.Errorf("content_ready with approval: %v", err)
	}
	c = base
	c.Status = campaign.StatusPendingApproval // pending without an approval
	if err := c.CheckPrepareApproval(); err == nil || errors.Is(err, campaign.ErrInvalidState) {
		t.Errorf("pending without approval: %v", err)
	}

	// No channels → 422 with a channels field error.
	c = base
	c.Channels = []segment.Channel{}
	var ve *campaign.ValidationError
	if err := c.CheckPrepareApproval(); !errors.As(err, &ve) || ve.Problems[0].Path != "channels" || ve.Problems[0].Code != segment.CodeRequired {
		t.Errorf("no channels: %v", err)
	}
	// No content version / no interpretation → state errors.
	c = base
	c.CurrentContentID = nil
	if err := c.CheckPrepareApproval(); !errors.Is(err, campaign.ErrInvalidState) {
		t.Errorf("no content: %v", err)
	}
	c = base
	c.ContentBrief = nil
	if err := c.CheckPrepareApproval(); !errors.Is(err, campaign.ErrInvalidState) {
		t.Errorf("no brief: %v", err)
	}
}

func TestCheckPrepareAtVersion(t *testing.T) {
	c, _ := readyCampaign(t, draft(genSMS, genSubject, genBody))
	var ce *campaign.ConflictError
	if err := c.CheckPrepareAt(c.Version - 1); !errors.As(err, &ce) || ce.Expected != c.Version-1 || ce.Current != c.Version {
		t.Errorf("stale: %v", err)
	}
	// The version check comes first, even in a state that is not preparable.
	d := c
	d.Status = campaign.StatusCancelled
	if err := d.CheckPrepareAt(c.Version + 1); !errors.Is(err, campaign.ErrVersionConflict) {
		t.Errorf("stale + cancelled: %v", err)
	}
	if err := c.CheckPrepareAt(c.Version); err != nil {
		t.Errorf("current: %v", err)
	}
}

func TestWithPreparedApproval(t *testing.T) {
	c, _ := readyCampaign(t, draft(genSMS, genSubject, genBody))
	a := approvalID(7)
	next, err := c.WithPreparedApproval(c.Version, a)
	if err != nil {
		t.Fatal(err)
	}
	if next.Status != campaign.StatusPendingApproval || next.ActiveApprovalID == nil || *next.ActiveApprovalID != a ||
		next.Version != c.Version+1 || next.SegmentVersion != c.SegmentVersion || !slices.Equal(next.Channels, c.Channels) ||
		*next.CurrentContentID != *c.CurrentContentID {
		t.Fatalf("next = %+v", next)
	}
	if c.Status != campaign.StatusContentReady || c.ActiveApprovalID != nil {
		t.Error("receiver modified")
	}
	// Superseding a pending approval is the documented prepare transition
	// (§5.2): the active approval is replaced.
	again, err := next.WithPreparedApproval(next.Version, approvalID(8))
	if err != nil || *again.ActiveApprovalID != approvalID(8) || again.Status != campaign.StatusPendingApproval {
		t.Errorf("pending: %+v %v", again, err)
	}
	// An approved campaign is never re-prepared by a plain prepare.
	approved := next
	approved.Status = campaign.StatusApproved
	var se *campaign.StateError
	if _, err := approved.WithPreparedApproval(approved.Version, approvalID(9)); !errors.As(err, &se) ||
		se.Status != campaign.StatusApproved {
		t.Errorf("approved: %v", err)
	}
	if _, err := c.WithPreparedApproval(c.Version+1, a); !errors.Is(err, campaign.ErrVersionConflict) {
		t.Errorf("stale: %v", err)
	}
}

func TestWithApprovalClosed(t *testing.T) {
	c, _ := readyCampaign(t, draft(genSMS, genSubject, genBody))
	a := approvalID(3)
	pending, err := c.WithPreparedApproval(c.Version, a)
	if err != nil {
		t.Fatal(err)
	}
	for _, st := range []campaign.Status{campaign.StatusPendingApproval, campaign.StatusApproved} {
		cur := pending
		cur.Status = st
		closed, err := cur.WithApprovalClosed(a)
		if err != nil {
			t.Fatal(err)
		}
		// Expiry is a persisted state change: the version advances by one.
		if closed.Status != campaign.StatusContentReady || closed.ActiveApprovalID != nil || closed.Version != cur.Version+1 {
			t.Errorf("%s: closed = %+v", st, closed)
		}
	}
	if _, err := pending.WithApprovalClosed(approvalID(4)); err == nil {
		t.Error("closing another approval must fail")
	}
	if _, err := c.WithApprovalClosed(a); err == nil {
		t.Error("closing in content_ready must fail")
	}
}

func TestReviewForApproval(t *testing.T) {
	t.Run("covers selected channels", func(t *testing.T) {
		c, v := readyCampaign(t, draft(genSMS, genSubject, genBody))
		r, err := c.ReviewForApproval(v)
		if err != nil {
			t.Fatal(err)
		}
		if !r.Validation.OK() || len(r.OfferFindings) != 0 || r.Offer.Kind != ai.OfferPercentDiscount || r.Offer.Value != 30 ||
			r.Validation.RenderedExample == nil {
			t.Errorf("review = %+v", r)
		}
	})
	t.Run("sms only and email only", func(t *testing.T) {
		for _, ch := range []segment.Channel{segment.ChannelSMS, segment.ChannelEmail} {
			c, v := readyCampaign(t, draft(genSMS, genSubject, genBody))
			c.Channels = []segment.Channel{ch}
			r, err := c.ReviewForApproval(v)
			if err != nil {
				t.Fatalf("%s: %v", ch, err)
			}
			// Text of the unselected channel is reported, never sent.
			if !hasCode(r.Validation.Warnings, campaign.CodeChannelNotSelected) {
				t.Errorf("%s: warnings = %v", ch, codes(r.Validation.Warnings))
			}
		}
	})
	t.Run("missing text for a selected channel", func(t *testing.T) {
		c, v := readyCampaign(t, draft(genSMS, "", ""))
		var ve *campaign.ValidationError
		if _, err := c.ReviewForApproval(v); !errors.As(err, &ve) ||
			codes(ve.Problems) != "email_subject:required,email_body:required" {
			t.Errorf("err = %v", err)
		}
	})
	t.Run("offer findings are preserved, not blocking", func(t *testing.T) {
		c, v := readyCampaign(t, draft("Hi {{first_name|there}}, 50% off today, plus a free gift!",
			"{{first_name|Hello}}, save ₺50 now", genBody))
		r, err := c.ReviewForApproval(v)
		if err != nil {
			t.Fatal(err)
		}
		got := codes(r.OfferFindings)
		if got != "sms_body:unauthorized_offer,sms_body:unauthorized_offer,email_subject:unauthorized_claim" {
			t.Errorf("findings = %s", got)
		}
		for _, f := range r.OfferFindings {
			if !slices.ContainsFunc(r.Validation.Warnings, func(w segment.FieldError) bool { return w.Path == f.Path && w.Code == f.Code && w.Message == f.Message }) {
				t.Errorf("finding %v missing from warnings", f)
			}
		}
	})
	t.Run("content brief must parse", func(t *testing.T) {
		c, v := readyCampaign(t, draft(genSMS, genSubject, genBody))
		c.ContentBrief = []byte(`{"goal":"x"}`)
		if _, err := c.ReviewForApproval(v); err == nil || errors.Is(err, campaign.ErrInvalidInput) {
			t.Errorf("err = %v", err)
		}
	})
	t.Run("version must be the current one", func(t *testing.T) {
		c, v := readyCampaign(t, draft(genSMS, genSubject, genBody))
		v.ID = 41
		if _, err := c.ReviewForApproval(v); err == nil {
			t.Error("stale version accepted")
		}
	})
}

func hasCode(ws []segment.FieldError, code string) bool {
	return slices.ContainsFunc(ws, func(w segment.FieldError) bool { return w.Code == code })
}
