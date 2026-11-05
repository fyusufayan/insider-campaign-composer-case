package campaign_test

import (
	"errors"
	"testing"

	"github.com/fyusufayan/insider-campaign-composer-case/backend/internal/campaign"
)

// Every campaign status × send (§5.2 approved → send → sending): allowed
// only for the active approval of an approved campaign at the expected
// version; it advances the version by exactly one and changes nothing but
// the status. Everything else is a *StateError, a stale version a
// *ConflictError (checked first, in every status).
func TestSendTransitions(t *testing.T) {
	base, _ := readyCampaign(t, draft(genSMS, genSubject, genBody))
	live, other := approvalID(1), approvalID(2)
	for _, tc := range []struct {
		status campaign.Status
		active *campaign.ID
		ok     bool
	}{
		{campaign.StatusDraft, nil, false},
		{campaign.StatusSegmented, nil, false},
		{campaign.StatusContentReady, nil, false},
		{campaign.StatusPendingApproval, &live, false},
		{campaign.StatusApproved, &live, true},
		{campaign.StatusSending, &live, false},
		{campaign.StatusCompleted, &live, false},
		{campaign.StatusCompletedWithFailures, &live, false},
		{campaign.StatusCancelled, &live, false},
	} {
		c := base
		c.Status, c.ActiveApprovalID, c.Version = tc.status, tc.active, 7

		var ce *campaign.ConflictError
		for _, stale := range []int{6, 8} {
			if err := c.CheckSendAt(stale, live); !errors.As(err, &ce) || ce.Current != 7 || ce.Expected != stale {
				t.Errorf("%s: stale %d: %v", tc.status, stale, err)
			}
		}
		if err := c.CheckSendAt(7, other); !errors.Is(err, campaign.ErrInvalidState) {
			t.Errorf("%s: other approval: %v", tc.status, err)
		}

		next, err := c.WithSendStarted(7, live)
		if !tc.ok {
			var se *campaign.StateError
			if !errors.As(err, &se) || se.Op != campaign.OpSend || se.Status != tc.status || se.Reason == "" {
				t.Errorf("%s: send = %v", tc.status, err)
			}
			continue
		}
		if err != nil {
			t.Fatalf("%s: %v", tc.status, err)
		}
		if next.Status != campaign.StatusSending || next.Version != 8 || *next.ActiveApprovalID != live ||
			next.SegmentVersion != c.SegmentVersion || *next.CurrentContentID != *c.CurrentContentID ||
			len(next.Channels) != len(c.Channels) {
			t.Errorf("next = %+v", next)
		}
		// Pure: the input is unchanged and shares no memory with the result.
		if c.Status != campaign.StatusApproved || c.Version != 7 {
			t.Errorf("input mutated: %+v", c)
		}
		*next.ActiveApprovalID = other
		if *c.ActiveApprovalID != live {
			t.Error("result shares the active approval id with the input")
		}
	}
}
