package campaign_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"

	"github.com/fyusufayan/insider-campaign-composer-case/backend/internal/campaign"
	"github.com/fyusufayan/insider-campaign-composer-case/backend/internal/segment"
)

// Every campaign status × confirm/withdraw (§5.2): allowed only for the
// active approval of pending_approval (confirm) or pending_approval /
// approved (withdraw); everything else is a *StateError, a stale version
// a *ConflictError; allowed transitions advance the version by one.
func TestConfirmAndWithdrawTransitions(t *testing.T) {
	base, _ := readyCampaign(t, draft(genSMS, genSubject, genBody))
	live, other := approvalID(1), approvalID(2)
	type want struct{ confirm, withdraw bool }
	cases := []struct {
		status campaign.Status
		active *campaign.ID
		want   want
	}{
		{campaign.StatusDraft, nil, want{}},
		{campaign.StatusSegmented, nil, want{}},
		{campaign.StatusContentReady, nil, want{}},
		{campaign.StatusPendingApproval, &live, want{confirm: true, withdraw: true}},
		{campaign.StatusApproved, &live, want{confirm: false, withdraw: true}},
		{campaign.StatusSending, &live, want{}},
		{campaign.StatusCompleted, &live, want{}},
		{campaign.StatusCompletedWithFailures, &live, want{}},
		{campaign.StatusCancelled, &live, want{}},
	}
	for _, tc := range cases {
		c := base
		c.Status, c.ActiveApprovalID, c.Version = tc.status, tc.active, 7

		// Stale versions are conflicts in every status.
		var ce *campaign.ConflictError
		if err := c.CheckConfirmAt(6, live); !errors.As(err, &ce) || ce.Current != 7 {
			t.Errorf("%s: stale confirm: %v", tc.status, err)
		}
		if err := c.CheckWithdrawAt(8, live); !errors.As(err, &ce) {
			t.Errorf("%s: stale withdraw: %v", tc.status, err)
		}
		// Another approval id is never accepted.
		if err := c.CheckConfirmAt(7, other); !errors.Is(err, campaign.ErrInvalidState) {
			t.Errorf("%s: confirm other id: %v", tc.status, err)
		}
		if err := c.CheckWithdrawAt(7, other); !errors.Is(err, campaign.ErrInvalidState) {
			t.Errorf("%s: withdraw other id: %v", tc.status, err)
		}

		next, err := c.WithConfirmedApproval(7, live)
		if tc.want.confirm {
			if err != nil || next.Status != campaign.StatusApproved || next.Version != 8 || *next.ActiveApprovalID != live ||
				next.SegmentVersion != c.SegmentVersion || *next.CurrentContentID != *c.CurrentContentID {
				t.Errorf("%s: confirm = %+v, %v", tc.status, next, err)
			}
			if c.Status != tc.status || c.Version != 7 {
				t.Errorf("%s: confirm mutated its receiver", tc.status)
			}
		} else {
			var se *campaign.StateError
			if !errors.As(err, &se) || se.Op != campaign.OpConfirmApproval || se.Status != tc.status || se.Reason == "" {
				t.Errorf("%s: confirm err = %v", tc.status, err)
			}
		}

		err = c.CheckWithdrawAt(7, live)
		if tc.want.withdraw {
			if err != nil {
				t.Errorf("%s: withdraw: %v", tc.status, err)
			}
			closed, err := c.WithApprovalClosed(live)
			if err != nil || closed.Status != campaign.StatusContentReady || closed.ActiveApprovalID != nil || closed.Version != 8 {
				t.Errorf("%s: closed = %+v, %v", tc.status, closed, err)
			}
		} else {
			var se *campaign.StateError
			if !errors.As(err, &se) || se.Op != campaign.OpWithdrawApproval || se.Status != tc.status {
				t.Errorf("%s: withdraw err = %v", tc.status, err)
			}
		}
	}
}

// fakeExpirer records lazy-expiry calls; when due it "expires" the
// campaign in the memory store like approval.ExpireDue (content_ready,
// no approval, version + 1).
type fakeExpirer struct {
	f     *fixture
	calls int
	due   bool
	err   error
}

func (x *fakeExpirer) ExpireDue(ctx context.Context, id campaign.ID) (*campaign.ID, error) {
	x.calls++
	if x.err != nil {
		return nil, x.err
	}
	if !x.due {
		return nil, nil
	}
	x.due = false
	c, err := x.f.store.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	expired := *c.ActiveApprovalID
	c.Status, c.ActiveApprovalID, c.Version = campaign.StatusContentReady, nil, c.Version+1
	x.f.store.Put(c)
	return &expired, nil
}

func TestServiceLazyExpiry(t *testing.T) {
	f := newFixture(t)
	x := &fakeExpirer{f: f}
	svc, err := campaign.NewService(f.store, f.audience, f.it, f.gen, slog.New(slog.NewTextHandler(io.Discard, nil)), campaign.WithApprovalExpirer(x))
	if err != nil {
		t.Fatal(err)
	}
	c := f.interpreted(t)
	ctx := context.Background()

	// No live approval: the expirer is never consulted.
	if _, err := svc.Get(ctx, c.ID); err != nil || x.calls != 0 {
		t.Fatalf("calls = %d, err = %v", x.calls, err)
	}
	// Live approval, not due: consulted, nothing changes.
	live := approvalID(3)
	pending := c
	pending.Status, pending.ActiveApprovalID = campaign.StatusPendingApproval, &live
	f.store.Put(pending)
	pending, _ = f.store.Get(ctx, c.ID)
	got, err := svc.Get(ctx, c.ID)
	if err != nil || x.calls != 1 || got.Version != pending.Version || got.Status != campaign.StatusPendingApproval {
		t.Fatalf("not due: %+v %v (calls %d)", got, err, x.calls)
	}
	// Due: expired first, the post-expiry state is returned.
	x.due = true
	got, err = svc.Get(ctx, c.ID)
	if err != nil || got.Status != campaign.StatusContentReady || got.Version != pending.Version+1 || got.ActiveApprovalID != nil {
		t.Fatalf("due: %+v %v", got, err)
	}
	// A version-guarded mutation on the pre-expiry view is a conflict: lazy
	// expiry ran (and stays committed) before the CAS.
	f.store.Put(pending)
	pending, _ = f.store.Get(ctx, c.ID)
	x.due = true
	var ce *campaign.ConflictError
	if _, err := svc.SetChannels(ctx, c.ID, []segment.Channel{segment.ChannelSMS}, pending.Version); !errors.As(err, &ce) {
		t.Errorf("set channels on the pre-expiry view: %v", err)
	}
	// Expiry failures surface (never a stale answer).
	f.store.Put(pending)
	x.err = errors.New("busy")
	if _, err := svc.Get(ctx, c.ID); err == nil {
		t.Error("expiry error swallowed")
	}
}
