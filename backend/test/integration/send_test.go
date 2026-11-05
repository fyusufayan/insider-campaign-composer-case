//go:build integration

package integration

import (
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fyusufayan/insider-campaign-composer-case/backend/internal/approval"
	"github.com/fyusufayan/insider-campaign-composer-case/backend/internal/campaign"
	"github.com/fyusufayan/insider-campaign-composer-case/backend/internal/segment"
)

// ---- CP16: the send transaction (consume the confirmed approval, release
// its held rows) against real PostgreSQL: locks, clock_timestamp(),
// constraints and triggers for injected failures.

// approved returns a campaign with a confirmed approval (sms + email, 5
// held rows: sms users 1 and 8, email users 1, 3 and 5).
func (e *prepEnv) approved() (campaign.Campaign, approval.Summary) {
	e.t.Helper()
	c, s := e.prepared()
	out, err := e.approvals.Confirm(ctxBG, c.ID, s.ApprovalID, confirmReq(c, s))
	if err != nil {
		e.t.Fatalf("confirm: %v", err)
	}
	return out.Campaign, out.Summary
}

// sendState is the send-relevant state of a campaign and its approval.
type sendState struct {
	status       string
	version      int
	sendStarted  bool
	approval     string
	consumed     bool
	rowsByStatus map[string]int64
	attemptRows  int64
}

func (e *prepEnv) sendState(c campaign.Campaign, approvalID campaign.ID) sendState {
	e.t.Helper()
	st := sendState{rowsByStatus: map[string]int64{}}
	if err := e.pool.QueryRow(ctxBG, `SELECT status, version, send_started_at IS NOT NULL FROM campaigns WHERE id = $1::uuid`,
		c.ID.String()).Scan(&st.status, &st.version, &st.sendStarted); err != nil {
		e.t.Fatal(err)
	}
	if err := e.pool.QueryRow(ctxBG, `SELECT status, consumed_at IS NOT NULL FROM campaign_approvals WHERE id = $1::uuid`,
		approvalID.String()).Scan(&st.approval, &st.consumed); err != nil {
		e.t.Fatal(err)
	}
	rows, err := e.pool.Query(ctxBG, `SELECT status, count(*) FROM campaign_deliveries WHERE campaign_id = $1::uuid GROUP BY status`, c.ID.String())
	if err != nil {
		e.t.Fatal(err)
	}
	for rows.Next() {
		var (
			s string
			n int64
		)
		if err := rows.Scan(&s, &n); err != nil {
			e.t.Fatal(err)
		}
		st.rowsByStatus[s] = n
	}
	st.attemptRows = e.count(`SELECT count(*) FROM delivery_attempts`)
	return st
}

func (e *prepEnv) assertNotSent(c campaign.Campaign, s approval.Summary, wantStatus campaign.Status, wantApproval string, wantHeld int64) {
	e.t.Helper()
	st := e.sendState(c, s.ApprovalID)
	if st.status != string(wantStatus) || st.sendStarted || st.approval != wantApproval || st.consumed ||
		st.rowsByStatus["held"] != wantHeld || len(st.rowsByStatus) > 1 || st.attemptRows != 0 {
		e.t.Errorf("state = %+v, want campaign %s, approval %s, %d held rows", st, wantStatus, wantApproval, wantHeld)
	}
}

func TestSendStartsSending(t *testing.T) {
	e := newDecisionEnv(t)
	c, s := e.approved()
	before := e.approvalRow(s.ApprovalID)

	out, err := e.approvals.Send(ctxBG, c.ID, s.ApprovalID, c.Version)
	if err != nil {
		t.Fatal(err)
	}
	if !out.Started || out.ApprovalID != s.ApprovalID || out.Released[segment.ChannelSMS] != 2 || out.Released[segment.ChannelEmail] != 3 {
		t.Errorf("outcome = %+v", out)
	}
	if out.Campaign.Status != campaign.StatusSending || out.Campaign.Version != c.Version+1 ||
		*out.Campaign.ActiveApprovalID != s.ApprovalID || out.Campaign.CurrentContentID == nil || *out.Campaign.CurrentContentID != s.ContentID {
		t.Errorf("campaign = %+v", out.Campaign)
	}
	row := e.approvalRow(s.ApprovalID)
	if row.Status != "consumed" || row.ConsumedAt == nil || !timePtrEq(row.ApprovedAt, before.ApprovedAt) ||
		!row.ExpiresAt.Equal(before.ExpiresAt) || row.ConsumedAt.After(row.ExpiresAt) || row.Digest != before.Digest {
		t.Errorf("approval = %+v", row)
	}
	st := e.sendState(c, s.ApprovalID)
	if !st.sendStarted || st.rowsByStatus["pending"] != 5 || len(st.rowsByStatus) != 1 || st.attemptRows != 0 {
		t.Errorf("state = %+v", st)
	}
	// Released rows are the frozen snapshot, unchanged: same recipients,
	// vars and keys; destination still NULL (resolved at the first precheck).
	for _, d := range e.deliveries(s.ApprovalID) {
		if !d.DestinationNull || d.Attempts != 0 || !d.LeaseNull || d.Status != "pending" {
			t.Errorf("row = %+v", d)
		}
	}
	if got := mustDigest(t, recipientsOf(e.deliveries(s.ApprovalID))); got != s.SnapshotDigest {
		t.Error("released rows differ from the approved snapshot")
	}

	// A repeat (same or newer version) is idempotent: nothing is written.
	updated := e.get(c.ID)
	for _, v := range []int{c.Version, updated.Version} {
		again, err := e.approvals.Send(ctxBG, c.ID, s.ApprovalID, v)
		if err != nil || again.Started || again.Campaign.Version != updated.Version ||
			again.Released[segment.ChannelSMS] != 2 || again.Released[segment.ChannelEmail] != 3 {
			t.Errorf("repeat with version %d: %+v %v", v, again, err)
		}
	}
	if after := e.get(c.ID); !after.UpdatedAt.Equal(updated.UpdatedAt) || after.Version != updated.Version {
		t.Errorf("repeat wrote the campaign: %+v", after)
	}
	if !strings.Contains(e.logs.String(), `"msg":"campaign send started"`) {
		t.Error("send start not logged")
	}
}

func TestSendRefusals(t *testing.T) {
	t.Run("pending approval", func(t *testing.T) {
		e := newDecisionEnv(t)
		c, s := e.prepared()
		_, err := e.approvals.Send(ctxBG, c.ID, s.ApprovalID, c.Version)
		var se *campaign.StateError
		if !errors.As(err, &se) || !strings.Contains(se.Reason, "not been confirmed") {
			t.Fatalf("err = %v", err)
		}
		e.assertNotSent(c, s, campaign.StatusPendingApproval, "pending", 5)
	})
	t.Run("withdrawn approval", func(t *testing.T) {
		e := newDecisionEnv(t)
		c, s := e.approved()
		w, err := e.approvals.Withdraw(ctxBG, c.ID, s.ApprovalID, c.Version)
		if err != nil {
			t.Fatal(err)
		}
		for _, v := range []int{c.Version, w.Campaign.Version} {
			if _, err := e.approvals.Send(ctxBG, c.ID, s.ApprovalID, v); !errors.Is(err, campaign.ErrInvalidState) {
				t.Errorf("version %d: %v", v, err)
			}
		}
		e.assertNotSent(c, s, campaign.StatusContentReady, "revoked", 0)
	})
	t.Run("stale version", func(t *testing.T) {
		e := newDecisionEnv(t)
		c, s := e.approved()
		for _, v := range []int{c.Version - 1, c.Version + 1} {
			var ce *campaign.ConflictError
			if _, err := e.approvals.Send(ctxBG, c.ID, s.ApprovalID, v); !errors.As(err, &ce) || ce.Current != c.Version {
				t.Errorf("version %d: %v", v, err)
			}
		}
		e.assertNotSent(c, s, campaign.StatusApproved, "approved", 5)
	})
	t.Run("unknown and foreign approval", func(t *testing.T) {
		e := newDecisionEnv(t)
		c, s := e.approved()
		other, os := e.approved()
		if _, err := e.approvals.Send(ctxBG, c.ID, campaign.ID{1, 2, 3}, c.Version); !errors.Is(err, approval.ErrNotFound) {
			t.Errorf("unknown: %v", err)
		}
		if _, err := e.approvals.Send(ctxBG, c.ID, os.ApprovalID, c.Version); !errors.Is(err, approval.ErrNotFound) {
			t.Errorf("foreign: %v", err)
		}
		if _, err := e.approvals.Send(ctxBG, campaign.ID{9}, s.ApprovalID, c.Version); !errors.Is(err, campaign.ErrNotFound) {
			t.Errorf("unknown campaign: %v", err)
		}
		e.assertNotSent(c, s, campaign.StatusApproved, "approved", 5)
		e.assertNotSent(other, os, campaign.StatusApproved, "approved", 5)
	})
	t.Run("expired approved approval", func(t *testing.T) {
		e := newDecisionEnv(t)
		c, s := e.approved()
		e.makeDue(s.ApprovalID)
		if _, err := e.approvals.Send(ctxBG, c.ID, s.ApprovalID, c.Version); !errors.Is(err, approval.ErrExpired) {
			t.Fatalf("err = %v", err)
		}
		// Lazy expiry committed: approval expired, held rows deleted,
		// campaign content_ready with version + 1; nothing was released.
		after := e.get(c.ID)
		if after.Status != campaign.StatusContentReady || after.ActiveApprovalID != nil || after.Version != c.Version+1 {
			t.Errorf("campaign = %+v", after)
		}
		if row := e.approvalRow(s.ApprovalID); row.Status != "expired" || row.ConsumedAt != nil {
			t.Errorf("approval = %+v", row)
		}
		if n := e.count(`SELECT count(*) FROM campaign_deliveries`); n != 0 {
			t.Errorf("rows = %d", n)
		}
		if _, err := e.approvals.Send(ctxBG, c.ID, s.ApprovalID, after.Version); !errors.Is(err, approval.ErrExpired) {
			t.Errorf("second send: %v", err)
		}
	})
	t.Run("binding mismatch", func(t *testing.T) {
		e := newDecisionEnv(t)
		c, s := e.approved()
		// Simulated drift between campaign and approval (edits are refused
		// while an approval is live, so only a bug could cause it).
		e.exec(`UPDATE campaign_approvals SET segment_version = segment_version + 1 WHERE id = $1::uuid`, s.ApprovalID.String())
		var me *approval.MismatchError
		if _, err := e.approvals.Send(ctxBG, c.ID, s.ApprovalID, c.Version); !errors.As(err, &me) ||
			len(me.Fields) != 1 || me.Fields[0] != "campaign.segment_version" {
			t.Fatalf("err = %v", err)
		}
		e.assertNotSent(c, s, campaign.StatusApproved, "approved", 5)
	})
	t.Run("snapshot differs from approval", func(t *testing.T) {
		for name, tamper := range map[string]string{
			"one held row missing": `DELETE FROM campaign_deliveries WHERE id = (SELECT min(id) FROM campaign_deliveries WHERE approval_id = $1::uuid)`,
			"no held rows":         `DELETE FROM campaign_deliveries WHERE approval_id = $1::uuid`,
			"row already released": `UPDATE campaign_deliveries SET status = 'pending' WHERE id = (SELECT min(id) FROM campaign_deliveries WHERE approval_id = $1::uuid)`,
		} {
			t.Run(name, func(t *testing.T) {
				e := newDecisionEnv(t)
				c, s := e.approved()
				e.exec(tamper, s.ApprovalID.String())
				if _, err := e.approvals.Send(ctxBG, c.ID, s.ApprovalID, c.Version); !errors.Is(err, approval.ErrInconsistent) {
					t.Fatalf("err = %v", err)
				}
				st := e.sendState(c, s.ApprovalID)
				if st.status != "approved" || st.approval != "approved" || st.consumed || st.sendStarted {
					t.Errorf("state = %+v", st)
				}
			})
		}
	})
	t.Run("cancelled after consumption", func(t *testing.T) {
		e := newDecisionEnv(t)
		c, s := e.approved()
		out, err := e.approvals.Send(ctxBG, c.ID, s.ApprovalID, c.Version)
		if err != nil {
			t.Fatal(err)
		}
		e.exec(`UPDATE campaigns SET status = 'cancelled' WHERE id = $1::uuid`, c.ID.String())
		if _, err := e.approvals.Send(ctxBG, c.ID, s.ApprovalID, out.Campaign.Version); !errors.Is(err, campaign.ErrInvalidState) {
			t.Errorf("err = %v", err)
		}
	})
}

// A failure injected at the last statement of the send transaction (the
// campaign update) rolls everything back: the approval stays approved
// (not consumed) and its rows held.
func TestSendInjectedFailureRollsBack(t *testing.T) {
	e := newDecisionEnv(t)
	c, s := e.approved()
	e.exec(`CREATE FUNCTION fail_send() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'injected failure'; END $$`)
	e.exec(`CREATE TRIGGER fail_sending BEFORE UPDATE ON campaigns FOR EACH ROW WHEN (NEW.status = 'sending') EXECUTE FUNCTION fail_send()`)
	if _, err := e.approvals.Send(ctxBG, c.ID, s.ApprovalID, c.Version); err == nil || !strings.Contains(err.Error(), "injected failure") {
		t.Fatalf("err = %v", err)
	}
	e.assertNotSent(c, s, campaign.StatusApproved, "approved", 5)
	if after := e.get(c.ID); after.Version != c.Version {
		t.Errorf("version = %d", after.Version)
	}
	// A failure in the release statement rolls back the consumption too.
	e.exec(`DROP TRIGGER fail_sending ON campaigns`)
	e.exec(`CREATE TRIGGER fail_release BEFORE UPDATE ON campaign_deliveries FOR EACH ROW WHEN (NEW.status = 'pending') EXECUTE FUNCTION fail_send()`)
	if _, err := e.approvals.Send(ctxBG, c.ID, s.ApprovalID, c.Version); err == nil || !strings.Contains(err.Error(), "injected failure") {
		t.Fatalf("err = %v", err)
	}
	e.assertNotSent(c, s, campaign.StatusApproved, "approved", 5)
	e.exec(`DROP TRIGGER fail_release ON campaign_deliveries`)
	if out, err := e.approvals.Send(ctxBG, c.ID, s.ApprovalID, c.Version); err != nil || !out.Started {
		t.Fatalf("after recovery: %+v %v", out, err)
	}
}

// Concurrent sends serialize on the campaign lock: exactly one consumes
// the approval and releases the rows; the others are idempotent repeats.
func TestSendConcurrentExactlyOneStarts(t *testing.T) {
	e := newDecisionEnv(t)
	c, s := e.approved()
	const n = 8
	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		started int
		errs    []error
	)
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			out, err := e.approvals.Send(ctxBG, c.ID, s.ApprovalID, c.Version)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				errs = append(errs, err)
				return
			}
			if out.Started {
				started++
			}
		}()
	}
	close(start)
	wg.Wait()
	if started != 1 || len(errs) != 0 {
		t.Fatalf("started = %d, errors = %v", started, errs)
	}
	st := e.sendState(c, s.ApprovalID)
	if st.status != "sending" || st.version != c.Version+1 || st.approval != "consumed" || st.rowsByStatus["pending"] != 5 {
		t.Errorf("state = %+v", st)
	}
	if n := e.count(`SELECT count(*) FROM campaign_approvals WHERE campaign_id = $1::uuid AND status = 'consumed'`, c.ID.String()); n != 1 {
		t.Errorf("consumed approvals = %d", n)
	}
}

// §5.3 verification for send: the sending transaction starts before
// expiry, waits on the campaign lock, acquires it after expiry and must be
// rejected as approval_expired — the approval expired, never consumed, its
// held rows deleted, nothing released.
func TestSendLockWaitExpiry(t *testing.T) {
	for _, viaService := range []bool{true, false} {
		name := map[bool]string{true: "service", false: "repository transaction"}[viaService]
		t.Run(name, func(t *testing.T) {
			e := newDecisionEnv(t)
			c, s := e.approved()
			var expiresAt time.Time
			if err := e.pool.QueryRow(ctxBG, `UPDATE campaign_approvals SET expires_at = clock_timestamp() + interval '300 milliseconds'
			    WHERE id = $1::uuid RETURNING expires_at`, s.ApprovalID.String()).Scan(&expiresAt); err != nil {
				t.Fatal(err)
			}
			tx := e.lockCampaign(c.ID)
			done := make(chan error, 1)
			go func() {
				var err error
				if viaService {
					_, err = e.approvals.Send(ctxBG, c.ID, s.ApprovalID, c.Version)
				} else {
					_, err = approval.NewRepository(e.pool).Send(ctxBG, approval.SendParams{
						CampaignID: c.ID, ApprovalID: s.ApprovalID, ExpectedVersion: c.Version})
				}
				done <- err
			}()
			waitForLockWait(t, e.pool, lockStmtPrefix)
			var xactStart time.Time
			if err := e.pool.QueryRow(ctxBG, `SELECT xact_start FROM pg_stat_activity
			    WHERE datname = current_database() AND wait_event_type = 'Lock' AND starts_with(ltrim(query), $1)`,
				lockStmtPrefix).Scan(&xactStart); err != nil {
				t.Fatal(err)
			}
			if !xactStart.Before(expiresAt) {
				t.Fatalf("waiting transaction started at %s, after expiry %s", xactStart, expiresAt)
			}
			waitForDBClockPast(t, e.pool, expiresAt)
			if err := tx.Commit(ctxBG); err != nil {
				t.Fatal(err)
			}
			var err error
			select {
			case err = <-done:
			case <-time.After(30 * time.Second):
				t.Fatal("send did not finish")
			}
			if !errors.Is(err, approval.ErrExpired) {
				t.Fatalf("err = %v", err)
			}
			row := e.approvalRow(s.ApprovalID)
			if row.Status != "expired" || row.ConsumedAt != nil || row.ClosedAt == nil || !row.ClosedAt.After(expiresAt) {
				t.Errorf("approval = %+v", row)
			}
			after := e.get(c.ID)
			if after.Status != campaign.StatusContentReady || after.ActiveApprovalID != nil || after.Version != c.Version+1 {
				t.Errorf("campaign = %+v", after)
			}
			if n := e.count(`SELECT count(*) FROM campaign_deliveries WHERE campaign_id = $1::uuid`, c.ID.String()); n != 0 {
				t.Errorf("delivery rows = %d", n)
			}
		})
	}
}

// Once consumed, the approval's old expires_at never interrupts sending:
// lazy expiry (campaign and approval reads, explicit ExpireDue) leaves the
// sending campaign and its consumed approval untouched.
func TestConsumedApprovalNeverExpires(t *testing.T) {
	e := newDecisionEnv(t)
	c, s := e.approved()
	out, err := e.approvals.Send(ctxBG, c.ID, s.ApprovalID, c.Version)
	if err != nil {
		t.Fatal(err)
	}
	e.exec(`UPDATE campaign_approvals SET expires_at = clock_timestamp() - interval '1 hour' WHERE id = $1::uuid`, s.ApprovalID.String())
	if id, err := e.approvals.ExpireDue(ctxBG, c.ID); err != nil || id != nil {
		t.Errorf("ExpireDue = %v %v", id, err)
	}
	if _, err := e.approvals.Get(ctxBG, c.ID, s.ApprovalID); err != nil {
		t.Errorf("approval get: %v", err)
	}
	got := e.get(c.ID)
	if got.Status != campaign.StatusSending || got.Version != out.Campaign.Version || *got.ActiveApprovalID != s.ApprovalID {
		t.Errorf("campaign = %+v", got)
	}
	if row := e.approvalRow(s.ApprovalID); row.Status != "consumed" {
		t.Errorf("approval = %+v", row)
	}
	if n := e.count(`SELECT count(*) FROM campaign_deliveries WHERE approval_id = $1::uuid AND status = 'pending'`, s.ApprovalID.String()); n != 5 {
		t.Errorf("pending rows = %d", n)
	}
}

// A repeat of Send for the consumed approval changes nothing — not the
// campaign (version, updated_at) and not a single delivery row — while
// sending and after completion; another approval id is never a replay.
func TestSendReplayChangesNothing(t *testing.T) {
	e := newDelEnv(t)
	// A withdrawn first approval, then a second one that is sent.
	c1, s1 := e.approved()
	if _, err := e.approvals.Withdraw(ctxBG, c1.ID, s1.ApprovalID, c1.Version); err != nil {
		t.Fatal(err)
	}
	cur := e.get(c1.ID)
	o, err := e.approvals.Prepare(ctxBG, cur.ID, cur.Version)
	if err != nil {
		t.Fatal(err)
	}
	s2 := o.Summary
	conf, err := e.approvals.Confirm(ctxBG, cur.ID, s2.ApprovalID, confirmReq(o.Campaign, s2))
	if err != nil {
		t.Fatal(err)
	}
	sent, err := e.approvals.Send(ctxBG, cur.ID, s2.ApprovalID, conf.Campaign.Version)
	if err != nil || !sent.Started {
		t.Fatal(sent, err)
	}

	type snapshot struct {
		c    campaign.Campaign
		rows map[string]delRow
		att  int64
	}
	snap := func() snapshot {
		return snapshot{c: e.get(cur.ID), rows: e.rows(s2.ApprovalID),
			att: e.count(`SELECT count(*) FROM delivery_attempts`)}
	}
	same := func(a, b snapshot) bool {
		if a.c.Version != b.c.Version || !a.c.UpdatedAt.Equal(b.c.UpdatedAt) || a.c.Status != b.c.Status || a.att != b.att || len(a.rows) != len(b.rows) {
			return false
		}
		for k, r := range a.rows {
			o := b.rows[k]
			if r.Status != o.Status || r.Attempts != o.Attempts || !ptrEq(r.Destination, o.Destination) || r.Leased != o.Leased {
				return false
			}
		}
		return true
	}
	repeatAll := func(phase string, versions ...int) {
		t.Helper()
		before := snap()
		for _, v := range versions {
			out, err := e.approvals.Send(ctxBG, cur.ID, s2.ApprovalID, v)
			if err != nil || out.Started || out.Campaign.Version != before.c.Version {
				t.Errorf("%s: repeat with version %d = %+v %v", phase, v, out, err)
			}
			// The withdrawn approval of the same campaign, and unknown ids, are never replays.
			if _, err := e.approvals.Send(ctxBG, cur.ID, s1.ApprovalID, v); !errors.Is(err, campaign.ErrInvalidState) {
				t.Errorf("%s: other approval: %v", phase, err)
			}
			if _, err := e.approvals.Send(ctxBG, cur.ID, campaign.ID{7}, v); !errors.Is(err, approval.ErrNotFound) {
				t.Errorf("%s: unknown approval: %v", phase, err)
			}
		}
		if !same(before, snap()) {
			t.Errorf("%s: a repeat changed state", phase)
		}
	}
	repeatAll("sending", conf.Campaign.Version, sent.Campaign.Version)

	e.start(e.workerConfig("w1"))
	if st := e.waitTerminal(cur.ID); st != "completed" {
		t.Fatalf("status = %s", st)
	}
	final := e.get(cur.ID)
	repeatAll("completed", conf.Campaign.Version, sent.Campaign.Version, final.Version)
	if n := e.count(`SELECT count(*) FROM campaign_deliveries WHERE approval_id = $1::uuid`, s1.ApprovalID.String()); n != 0 {
		t.Errorf("withdrawn approval has %d rows", n)
	}
}
