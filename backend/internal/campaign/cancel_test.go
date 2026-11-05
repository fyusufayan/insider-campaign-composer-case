package campaign_test

import (
	"errors"
	"testing"

	"github.com/fyusufayan/insider-campaign-composer-case/backend/internal/campaign"
)

// Every campaign status × cancel (§5.2): every non-terminal status may be
// cancelled at the expected version; the version advances by exactly one;
// a live approval is detached (the caller revokes it), a sending campaign
// keeps its consumed approval. Finished or cancelled campaigns are a
// *StateError whatever the version (a repeat learns why); otherwise a
// stale version is a *ConflictError.
func TestCancelTransitions(t *testing.T) {
	base, _ := readyCampaign(t, draft(genSMS, genSubject, genBody))
	live := approvalID(1)
	for _, tc := range []struct {
		status     campaign.Status
		active     *campaign.ID
		ok         bool
		keepActive bool
	}{
		{campaign.StatusDraft, nil, true, false},
		{campaign.StatusSegmented, nil, true, false},
		{campaign.StatusContentReady, nil, true, false},
		{campaign.StatusPendingApproval, &live, true, false},
		{campaign.StatusApproved, &live, true, false},
		{campaign.StatusSending, &live, true, true},
		{campaign.StatusCompleted, &live, false, false},
		{campaign.StatusCompletedWithFailures, &live, false, false},
		{campaign.StatusCancelled, &live, false, false},
	} {
		c := base
		c.Status, c.ActiveApprovalID, c.Version = tc.status, tc.active, 7

		if !tc.ok {
			for _, v := range []int{6, 7, 8} {
				var se *campaign.StateError
				if _, err := c.WithCancelled(v); !errors.As(err, &se) || se.Op != campaign.OpCancel || se.Status != tc.status || se.Reason == "" {
					t.Errorf("%s v%d: %v", tc.status, v, err)
				}
			}
			continue
		}
		var ce *campaign.ConflictError
		for _, stale := range []int{6, 8} {
			if err := c.CheckCancelAt(stale); !errors.As(err, &ce) || ce.Current != 7 || ce.Expected != stale {
				t.Errorf("%s: stale %d: %v", tc.status, stale, err)
			}
		}
		next, err := c.WithCancelled(7)
		if err != nil {
			t.Fatalf("%s: %v", tc.status, err)
		}
		if next.Status != campaign.StatusCancelled || next.Version != 8 || next.SegmentVersion != c.SegmentVersion ||
			*next.CurrentContentID != *c.CurrentContentID || len(next.Channels) != len(c.Channels) {
			t.Errorf("%s: next = %+v", tc.status, next)
		}
		if tc.keepActive != (next.ActiveApprovalID != nil) {
			t.Errorf("%s: active approval after cancel = %v", tc.status, next.ActiveApprovalID)
		}
		if c.Status != tc.status || c.Version != 7 { // pure
			t.Errorf("%s: input mutated: %+v", tc.status, c)
		}
	}
	// Inconsistent rows are errors, never silently cancelled.
	bad := base
	bad.Status, bad.ActiveApprovalID, bad.Version = campaign.StatusSending, nil, 3
	if _, err := bad.WithCancelled(3); err == nil {
		t.Error("sending without an approval cancelled")
	}
}
