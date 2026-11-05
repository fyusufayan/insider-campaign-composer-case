//go:build integration

package integration

import (
	"errors"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fyusufayan/insider-campaign-composer-case/backend/internal/approval"
	"github.com/fyusufayan/insider-campaign-composer-case/backend/internal/campaign"
	"github.com/fyusufayan/insider-campaign-composer-case/backend/internal/platform/logging"
	"github.com/fyusufayan/insider-campaign-composer-case/backend/internal/segment"
)

// ---- CP15: confirmation, withdrawal and the approval read, against real
// PostgreSQL (real schema, constraints, locks and clock_timestamp()).

const decisionApprovedTTL = 2 * time.Hour

// newDecisionEnv is newPrepEnv with an explicit approved TTL and approver,
// and lazy expiry wired into the campaign service as in cmd/api.
func newDecisionEnv(t *testing.T) *prepEnv {
	t.Helper()
	e := newPrepEnv(t)
	log := logging.New(e.logs, slog.LevelDebug)
	var err error
	e.approvals, err = approval.NewService(approval.NewRepository(e.pool), approval.Config{
		PendingTTL: prepTTL, ApprovedTTL: decisionApprovedTTL, ApprovedBy: "reviewer@test"}, log)
	if err != nil {
		t.Fatal(err)
	}
	e.campaigns, err = campaign.NewService(campaign.NewRepository(e.pool), segment.NewAudienceRepository(e.pool), e.it, e.gen, log,
		campaign.WithApprovalExpirer(e.approvals))
	if err != nil {
		t.Fatal(err)
	}
	return e
}

// prepared returns a campaign with a pending approval (sms + email, 5 held
// rows) and its summary.
func (e *prepEnv) prepared() (campaign.Campaign, approval.Summary) {
	e.t.Helper()
	c := e.ready(replyIstanbul, draftOf(prepSMS, prepSubject, prepBody))
	s := e.prepare(c)
	return e.get(c.ID), s
}

func confirmReq(c campaign.Campaign, s approval.Summary) approval.ConfirmRequest {
	return approval.ConfirmRequest{ExpectedVersion: c.Version, SegmentVersion: s.SegmentVersion,
		ContentVersion: s.ContentVersion, Channels: s.Channels}
}

// snapshotState is everything a rejected decision must leave untouched.
type snapshotState struct {
	campaign  campaign.Campaign
	approval  approvalRow
	rows      []deliveryRow
	approvals int64
	contents  int64
}

func (e *prepEnv) state(c campaign.Campaign, approvalID campaign.ID) snapshotState {
	e.t.Helper()
	return snapshotState{
		campaign: e.get(c.ID), approval: e.approvalRow(approvalID), rows: e.deliveries(approvalID),
		approvals: e.count(`SELECT count(*) FROM campaign_approvals WHERE campaign_id = $1::uuid`, c.ID.String()),
		contents:  e.count(`SELECT count(*) FROM campaign_contents WHERE campaign_id = $1::uuid`, c.ID.String()),
	}
}

func (e *prepEnv) assertUnchanged(before snapshotState, approvalID campaign.ID) {
	e.t.Helper()
	after := e.state(before.campaign, approvalID)
	b, a := before.campaign, after.campaign
	if a.Version != b.Version || a.Status != b.Status || !a.UpdatedAt.Equal(b.UpdatedAt) ||
		!ptrEq(a.ActiveApprovalID, b.ActiveApprovalID) {
		e.t.Errorf("campaign changed: %+v → %+v", b, a)
	}
	ba, aa := before.approval, after.approval
	if aa.Status != ba.Status || !aa.ExpiresAt.Equal(ba.ExpiresAt) || !timePtrEq(aa.ApprovedAt, ba.ApprovedAt) ||
		!timePtrEq(aa.ClosedAt, ba.ClosedAt) || !ptrEq(aa.RevokeReason, ba.RevokeReason) {
		e.t.Errorf("approval changed: %+v → %+v", ba, aa)
	}
	if !slices.Equal(after.rows, before.rows) {
		e.t.Errorf("delivery rows changed: %d → %d", len(before.rows), len(after.rows))
	}
	if after.approvals != before.approvals || after.contents != before.contents {
		e.t.Errorf("rows inserted: approvals %d → %d, contents %d → %d", before.approvals, after.approvals, before.contents, after.contents)
	}
}

func ptrEq[T comparable](a, b *T) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

func timePtrEq(a, b *time.Time) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.Equal(*b)
}

func (e *prepEnv) makeDue(id campaign.ID) {
	e.t.Helper()
	e.exec(`UPDATE campaign_approvals SET expires_at = clock_timestamp() - interval '1 millisecond' WHERE id = $1::uuid`, id.String())
}

// ---- Confirm ----

func TestConfirmBeforeExpiry(t *testing.T) {
	e := newDecisionEnv(t)
	c, s := e.prepared()
	heldBefore := e.deliveries(s.ApprovalID)
	rowBefore := e.approvalRow(s.ApprovalID)
	contentBefore := e.count(`SELECT count(*) FROM campaign_contents`)

	out, err := e.approvals.Confirm(ctxBG, c.ID, s.ApprovalID, confirmReq(c, s))
	if err != nil {
		t.Fatal(err)
	}
	// Campaign: approved, version + 1 exactly, same active approval.
	got := e.get(c.ID)
	if got.Status != campaign.StatusApproved || got.Version != c.Version+1 || *got.ActiveApprovalID != s.ApprovalID ||
		out.Campaign.Version != got.Version || out.Campaign.Status != campaign.StatusApproved {
		t.Errorf("campaign = %+v, outcome = %+v", got, out.Campaign)
	}
	// Approval row: approved; expires_at = approved_at + approved TTL (one
	// clock_timestamp() value); approved_by recorded; prepared_at unchanged.
	row := e.approvalRow(s.ApprovalID)
	var approvedTTL int64
	if err := e.pool.QueryRow(ctxBG, `SELECT (extract(epoch FROM expires_at - approved_at) * 1000000)::bigint, approved_by
	    FROM campaign_approvals WHERE id = $1::uuid`, s.ApprovalID.String()).Scan(&approvedTTL, new(string)); err != nil {
		t.Fatal(err)
	}
	if row.Status != "approved" || row.ApprovedAt == nil || row.ClosedAt != nil || row.RevokeReason != nil ||
		approvedTTL != decisionApprovedTTL.Microseconds() || !row.PreparedAt.Equal(s.PreparedAt) || !row.ApprovedAt.After(row.PreparedAt) {
		t.Errorf("approval row = %+v (ttl %dµs)", row, approvedTTL)
	}
	if n := e.count(`SELECT count(*) FROM campaign_approvals WHERE id = $1::uuid AND approved_by = 'reviewer@test'`, s.ApprovalID.String()); n != 1 {
		t.Errorf("approved_by not recorded")
	}
	// Only lifecycle and TTL changed: the frozen binding is byte-identical.
	if row.SegmentVersion != rowBefore.SegmentVersion || row.ContentID != rowBefore.ContentID ||
		!slices.Equal(row.Channels, rowBefore.Channels) || row.SnapshotCount != rowBefore.SnapshotCount ||
		row.Digest != rowBefore.Digest || string(row.Rule) != string(rowBefore.Rule) || string(row.Stats) != string(rowBefore.Stats) {
		t.Errorf("binding changed: %+v → %+v", rowBefore, row)
	}
	if got.SegmentVersion != c.SegmentVersion || *got.CurrentContentID != *c.CurrentContentID ||
		!slices.Equal(got.Channels, c.Channels) {
		t.Errorf("campaign binding changed: %+v", got)
	}
	if d := mustDigest(t, recipientsOf(e.deliveries(s.ApprovalID))); d != s.SnapshotDigest {
		t.Errorf("recomputed digest %s differs", d)
	}
	// The frozen snapshot and the content are untouched; nothing is sent.
	if got := e.deliveries(s.ApprovalID); !slices.Equal(got, heldBefore) {
		t.Errorf("held rows changed: %v", got)
	}
	if n := e.count(`SELECT count(*) FROM campaign_deliveries WHERE status <> 'held' OR destination IS NOT NULL`); n != 0 {
		t.Errorf("%d delivery rows left held", n)
	}
	if n := e.count(`SELECT count(*) FROM delivery_attempts`); n != 0 {
		t.Errorf("attempts = %d", n)
	}
	if e.count(`SELECT count(*) FROM campaign_contents`) != contentBefore {
		t.Error("content rows changed")
	}
	// Summary: approved state, the reviewed binding and verified snapshot.
	sum := out.Summary
	if sum.Status != approval.StatusApproved || sum.ApprovedAt == nil || !sum.ApprovedAt.Equal(*row.ApprovedAt) ||
		!sum.ExpiresAt.Equal(row.ExpiresAt) || sum.ApprovedBy != "reviewer@test" || sum.CampaignVersion != got.Version ||
		!sum.Reviewed || sum.SnapshotDigest != s.SnapshotDigest || sum.Snapshot[segment.ChannelSMS].Recipients != 2 ||
		sum.Snapshot[segment.ChannelEmail].Recipients != 3 || sum.ContentProvenance.Origin != "edited" {
		t.Errorf("summary = %+v", sum)
	}
	if !strings.Contains(e.logs.String(), `"msg":"approval confirmed"`) {
		t.Error("no confirmation log")
	}
}

func TestConfirmRejectsExpired(t *testing.T) {
	for _, tc := range []struct {
		name string
		due  string // SQL setting expires_at relative to the database clock
	}{
		{"exactly at expiry", `clock_timestamp()`}, // expires_at > clock_timestamp() fails at equality and later
		{"after expiry", `clock_timestamp() - interval '1 second'`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newDecisionEnv(t)
			c, s := e.prepared()
			e.exec(`UPDATE campaign_approvals SET expires_at = `+tc.due+` WHERE id = $1::uuid`, s.ApprovalID.String())
			_, err := e.approvals.Confirm(ctxBG, c.ID, s.ApprovalID, confirmReq(c, s))
			if !errors.Is(err, approval.ErrExpired) {
				t.Fatalf("err = %v", err)
			}
			row := e.approvalRow(s.ApprovalID)
			if row.Status != "expired" || row.ApprovedAt != nil || row.RevokeReason != nil {
				t.Errorf("approval = %+v", row)
			}
			after := e.get(c.ID)
			if after.Status != campaign.StatusContentReady || after.Version != c.Version+1 || after.ActiveApprovalID != nil {
				t.Errorf("campaign = %+v", after)
			}
			if n := len(e.deliveries(s.ApprovalID)); n != 0 {
				t.Errorf("held rows = %d", n)
			}
			// Never reactivated: confirm and withdraw stay approval_expired,
			// whatever version is sent; nothing changes any more.
			before := e.state(after, s.ApprovalID)
			for _, v := range []int{c.Version, after.Version} {
				req := confirmReq(c, s)
				req.ExpectedVersion = v
				if _, err := e.approvals.Confirm(ctxBG, c.ID, s.ApprovalID, req); !errors.Is(err, approval.ErrExpired) {
					t.Errorf("confirm again (v%d): %v", v, err)
				}
				if _, err := e.approvals.Withdraw(ctxBG, c.ID, s.ApprovalID, v); !errors.Is(err, approval.ErrExpired) {
					t.Errorf("withdraw (v%d): %v", v, err)
				}
			}
			e.assertUnchanged(before, s.ApprovalID)
		})
	}
}

func TestConfirmStaleVersion(t *testing.T) {
	e := newDecisionEnv(t)
	c, s := e.prepared()
	before := e.state(c, s.ApprovalID)
	for _, v := range []int{c.Version - 1, c.Version + 1} {
		req := confirmReq(c, s)
		req.ExpectedVersion = v
		var ce *campaign.ConflictError
		if _, err := e.approvals.Confirm(ctxBG, c.ID, s.ApprovalID, req); !errors.As(err, &ce) || ce.Current != c.Version {
			t.Errorf("v%d: %v", v, err)
		}
	}
	e.assertUnchanged(before, s.ApprovalID)
}

func TestConfirmConcurrentOneWinner(t *testing.T) {
	e := newDecisionEnv(t)
	c, s := e.prepared()
	const n = 8
	start := make(chan struct{})
	errs := make(chan error, n)
	var wg sync.WaitGroup
	for range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, err := e.approvals.Confirm(ctxBG, c.ID, s.ApprovalID, confirmReq(c, s))
			errs <- err
		}()
	}
	close(start)
	wg.Wait()
	close(errs)
	ok := 0
	for err := range errs {
		switch {
		case err == nil:
			ok++
		case errors.Is(err, campaign.ErrInvalidState), errors.Is(err, campaign.ErrVersionConflict):
		default:
			t.Errorf("unexpected: %v", err)
		}
	}
	if ok != 1 {
		t.Fatalf("winners = %d", ok)
	}
	if got := e.get(c.ID); got.Version != c.Version+1 || got.Status != campaign.StatusApproved {
		t.Errorf("campaign = %+v", got)
	}
	if n := e.count(`SELECT count(*) FROM campaign_approvals WHERE status = 'approved'`); n != 1 {
		t.Errorf("approved approvals = %d", n)
	}
	if n := len(e.deliveries(s.ApprovalID)); n != 5 {
		t.Errorf("held rows = %d", n)
	}
}

// Confirm and withdraw racing on the same version: exactly one applies and
// the final state is consistent with it.
func TestConfirmWithdrawRace(t *testing.T) {
	for range 3 {
		e := newDecisionEnv(t)
		c, s := e.prepared()
		start := make(chan struct{})
		var cErr, wErr error
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			_, cErr = e.approvals.Confirm(ctxBG, c.ID, s.ApprovalID, confirmReq(c, s))
		}()
		go func() {
			defer wg.Done()
			<-start
			_, wErr = e.approvals.Withdraw(ctxBG, c.ID, s.ApprovalID, c.Version)
		}()
		close(start)
		wg.Wait()
		if (cErr == nil) == (wErr == nil) {
			t.Fatalf("confirm %v, withdraw %v: exactly one must win", cErr, wErr)
		}
		got, row := e.get(c.ID), e.approvalRow(s.ApprovalID)
		held := len(e.deliveries(s.ApprovalID))
		if got.Version != c.Version+1 {
			t.Errorf("version = %d", got.Version)
		}
		if cErr == nil && (got.Status != campaign.StatusApproved || row.Status != "approved" || held != 5) {
			t.Errorf("confirm won but state = %s/%s/%d", got.Status, row.Status, held)
		}
		if wErr == nil && (got.Status != campaign.StatusContentReady || row.Status != "revoked" || held != 0) {
			t.Errorf("withdraw won but state = %s/%s/%d", got.Status, row.Status, held)
		}
	}
}

func TestConfirmBindingMismatch(t *testing.T) {
	e := newDecisionEnv(t)
	c, s := e.prepared()
	before := e.state(c, s.ApprovalID)
	cases := []struct {
		name string
		mod  func(*approval.ConfirmRequest)
		want []string
	}{
		{"segment version", func(r *approval.ConfirmRequest) { r.SegmentVersion++ }, []string{"segment_version"}},
		{"content version", func(r *approval.ConfirmRequest) { r.ContentVersion++ }, []string{"content_version"}},
		{"channels", func(r *approval.ConfirmRequest) { r.Channels = smsOnly }, []string{"channels"}},
		{"all", func(r *approval.ConfirmRequest) {
			r.SegmentVersion, r.ContentVersion, r.Channels = 9, 9, emailOnly
		}, []string{"segment_version", "content_version", "channels"}},
	}
	for _, tc := range cases {
		req := confirmReq(c, s)
		tc.mod(&req)
		var me *approval.MismatchError
		if _, err := e.approvals.Confirm(ctxBG, c.ID, s.ApprovalID, req); !errors.As(err, &me) || !slices.Equal(me.Fields, tc.want) {
			t.Errorf("%s: %v", tc.name, err)
		}
	}
	e.assertUnchanged(before, s.ApprovalID)

	// The campaign itself no longer matches its frozen binding (impossible
	// through the API while the approval is live — edits are refused — so it
	// is forced with SQL): confirm is refused, nothing changes.
	e.exec(`UPDATE campaigns SET channels = '{sms}' WHERE id = $1::uuid`, c.ID.String())
	forced := e.state(c, s.ApprovalID)
	var me *approval.MismatchError
	if _, err := e.approvals.Confirm(ctxBG, c.ID, s.ApprovalID, confirmReq(c, s)); !errors.As(err, &me) ||
		!slices.Equal(me.Fields, []string{"campaign.channels"}) {
		t.Errorf("campaign drift: %v", err)
	}
	e.assertUnchanged(forced, s.ApprovalID)
}

// A snapshot that is incomplete (a held row missing) or altered is never
// confirmed: the transaction verifies count and digest and rolls back.
func TestConfirmRejectsInconsistentSnapshot(t *testing.T) {
	e := newDecisionEnv(t)
	c, s := e.prepared()
	e.exec(`DELETE FROM campaign_deliveries WHERE id = (SELECT min(id) FROM campaign_deliveries WHERE approval_id = $1::uuid)`, s.ApprovalID.String())
	before := e.state(c, s.ApprovalID)
	if _, err := e.approvals.Confirm(ctxBG, c.ID, s.ApprovalID, confirmReq(c, s)); !errors.Is(err, approval.ErrInconsistent) {
		t.Fatalf("err = %v", err)
	}
	e.assertUnchanged(before, s.ApprovalID)
}

func TestConfirmUnknownAndForeignApproval(t *testing.T) {
	e := newDecisionEnv(t)
	c, s := e.prepared()
	other, os := e.prepared()
	if _, err := e.approvals.Confirm(ctxBG, c.ID, campaign.ID{7}, confirmReq(c, s)); !errors.Is(err, approval.ErrNotFound) {
		t.Errorf("unknown: %v", err)
	}
	// Another campaign's approval is not found under this campaign.
	if _, err := e.approvals.Confirm(ctxBG, c.ID, os.ApprovalID, confirmReq(c, s)); !errors.Is(err, approval.ErrNotFound) {
		t.Errorf("foreign: %v", err)
	}
	if _, err := e.approvals.Withdraw(ctxBG, other.ID, s.ApprovalID, other.Version); !errors.Is(err, approval.ErrNotFound) {
		t.Errorf("foreign withdraw: %v", err)
	}
	if _, err := e.approvals.Confirm(ctxBG, campaign.ID{9}, s.ApprovalID, confirmReq(c, s)); !errors.Is(err, campaign.ErrNotFound) {
		t.Errorf("unknown campaign: %v", err)
	}
	// A superseded (revoked) approval of the same campaign cannot be confirmed.
	fresh := e.prepare(e.get(c.ID))
	cur := e.get(c.ID)
	req := confirmReq(cur, s)
	if _, err := e.approvals.Confirm(ctxBG, c.ID, s.ApprovalID, req); !errors.Is(err, campaign.ErrInvalidState) {
		t.Errorf("superseded: %v", err)
	}
	if _, err := e.approvals.Confirm(ctxBG, c.ID, fresh.ApprovalID, confirmReq(cur, fresh)); err != nil {
		t.Errorf("fresh: %v", err)
	}
}

// A failure at the final campaign update rolls back the approval CAS.
func TestConfirmAndWithdrawRollBack(t *testing.T) {
	e := newDecisionEnv(t)
	c, s := e.prepared()
	e.exec(`CREATE FUNCTION fail_status() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN RAISE EXCEPTION 'injected failure'; END $$`)
	e.exec(`CREATE TRIGGER fail_approved BEFORE UPDATE ON campaigns FOR EACH ROW
WHEN (NEW.status IN ('approved', 'content_ready') AND OLD.status <> NEW.status) EXECUTE FUNCTION fail_status()`)
	before := e.state(c, s.ApprovalID)
	if _, err := e.approvals.Confirm(ctxBG, c.ID, s.ApprovalID, confirmReq(c, s)); err == nil || !strings.Contains(err.Error(), "injected failure") {
		t.Fatalf("confirm: %v", err)
	}
	if _, err := e.approvals.Withdraw(ctxBG, c.ID, s.ApprovalID, c.Version); err == nil || !strings.Contains(err.Error(), "injected failure") {
		t.Fatalf("withdraw: %v", err)
	}
	e.assertUnchanged(before, s.ApprovalID)
	e.exec(`DROP TRIGGER fail_approved ON campaigns`)
	if _, err := e.approvals.Confirm(ctxBG, c.ID, s.ApprovalID, confirmReq(c, s)); err != nil {
		t.Fatalf("confirm after the failure: %v", err)
	}
}

// §5.3 verification for confirm: the confirming request's transaction
// starts before expiry, waits on the campaign lock held by another
// transaction, acquires it after expiry — and must be rejected as
// approval_expired with the approval expired and its held rows released.
// Two variants: through the service (its committed lazy expiry is the
// waiting transaction) and the confirmation transaction alone (lazy expiry
// inside it, committed although the request fails).
func TestConfirmLockWaitExpiry(t *testing.T) {
	for _, viaService := range []bool{true, false} {
		name := "repository transaction"
		if viaService {
			name = "service"
		}
		t.Run(name, func(t *testing.T) {
			e := newDecisionEnv(t)
			c, s := e.prepared()
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
					_, err = e.approvals.Confirm(ctxBG, c.ID, s.ApprovalID, confirmReq(c, s))
				} else {
					_, err = approval.NewRepository(e.pool).Confirm(ctxBG, approval.ConfirmParams{
						CampaignID: c.ID, ApprovalID: s.ApprovalID, ExpectedVersion: c.Version,
						SegmentVersion: s.SegmentVersion, ContentVersion: s.ContentVersion, Channels: s.Channels,
						ApprovedTTL: decisionApprovedTTL, ApprovedBy: "reviewer@test"})
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
				t.Fatal("confirm did not finish")
			}
			if !errors.Is(err, approval.ErrExpired) {
				t.Fatalf("err = %v", err)
			}
			row := e.approvalRow(s.ApprovalID)
			if row.Status != "expired" || row.ApprovedAt != nil || row.ClosedAt == nil || !row.ClosedAt.After(expiresAt) {
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

// ---- Withdraw ----

func TestWithdrawPendingAndApproved(t *testing.T) {
	for _, confirmFirst := range []bool{false, true} {
		name := "pending"
		if confirmFirst {
			name = "approved"
		}
		t.Run(name, func(t *testing.T) {
			e := newDecisionEnv(t)
			c, s := e.prepared()
			if confirmFirst {
				out, err := e.approvals.Confirm(ctxBG, c.ID, s.ApprovalID, confirmReq(c, s))
				if err != nil {
					t.Fatal(err)
				}
				c = out.Campaign
			}
			// Stale version: refused, nothing changes.
			before := e.state(c, s.ApprovalID)
			if _, err := e.approvals.Withdraw(ctxBG, c.ID, s.ApprovalID, c.Version-1); !errors.Is(err, campaign.ErrVersionConflict) {
				t.Errorf("stale: %v", err)
			}
			e.assertUnchanged(before, s.ApprovalID)

			out, err := e.approvals.Withdraw(ctxBG, c.ID, s.ApprovalID, c.Version)
			if err != nil {
				t.Fatal(err)
			}
			got := e.get(c.ID)
			if got.Status != campaign.StatusContentReady || got.ActiveApprovalID != nil || got.Version != c.Version+1 ||
				out.Campaign.Version != got.Version {
				t.Errorf("campaign = %+v", got)
			}
			row := e.approvalRow(s.ApprovalID)
			if row.Status != "revoked" || row.RevokeReason == nil || *row.RevokeReason != "withdrawn" || row.ClosedAt == nil ||
				row.SnapshotCount != 5 || row.Digest != s.SnapshotDigest {
				t.Errorf("approval = %+v", row)
			}
			if confirmFirst && row.ApprovedAt == nil {
				t.Error("approved_at lost")
			}
			if n := e.count(`SELECT count(*) FROM campaign_deliveries WHERE campaign_id = $1::uuid`, c.ID.String()); n != 0 {
				t.Errorf("held rows left = %d", n)
			}
			sum := out.Summary
			if sum.Status != approval.StatusRevoked || sum.RevokeReason == nil || *sum.RevokeReason != approval.RevokeWithdrawn ||
				sum.Reviewed || sum.Snapshot[segment.ChannelSMS].Recipients != 2 || sum.Snapshot[segment.ChannelSMS].RenderIssues != nil {
				t.Errorf("summary = %+v", sum)
			}
			// Repeated / late actions are deterministic and change nothing.
			closed := e.state(got, s.ApprovalID)
			if _, err := e.approvals.Withdraw(ctxBG, c.ID, s.ApprovalID, got.Version); !errors.Is(err, campaign.ErrInvalidState) {
				t.Errorf("repeat withdraw: %v", err)
			}
			req := confirmReq(got, s)
			if _, err := e.approvals.Confirm(ctxBG, c.ID, s.ApprovalID, req); !errors.Is(err, campaign.ErrInvalidState) {
				t.Errorf("confirm after withdraw: %v", err)
			}
			e.assertUnchanged(closed, s.ApprovalID)
			// The marketer can prepare again; a new approval with new rows.
			fresh := e.prepare(got)
			if fresh.ApprovalID == s.ApprovalID || fresh.Superseded != nil || fresh.SnapshotDigest != s.SnapshotDigest {
				t.Errorf("fresh = %+v", fresh)
			}
		})
	}
}

// §5.3 for withdraw: an approval that expires while the withdrawal waits
// for the campaign lock is expired, never withdrawn. In the repository
// variant the expiry is found inside the withdrawal transaction itself and
// must be committed (approval expired, held rows released, version + 1)
// although the request returns ErrExpired.
func TestWithdrawLockWaitExpiry(t *testing.T) {
	for _, viaService := range []bool{true, false} {
		name := "repository transaction"
		if viaService {
			name = "service"
		}
		t.Run(name, func(t *testing.T) {
			e := newDecisionEnv(t)
			c, s := e.prepared()
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
					_, err = e.approvals.Withdraw(ctxBG, c.ID, s.ApprovalID, c.Version)
				} else {
					_, err = approval.NewRepository(e.pool).Withdraw(ctxBG, approval.WithdrawParams{
						CampaignID: c.ID, ApprovalID: s.ApprovalID, ExpectedVersion: c.Version})
				}
				done <- err
			}()
			waitForLockWait(t, e.pool, lockStmtPrefix)
			waitForDBClockPast(t, e.pool, expiresAt)
			if err := tx.Commit(ctxBG); err != nil {
				t.Fatal(err)
			}
			var err error
			select {
			case err = <-done:
			case <-time.After(30 * time.Second):
				t.Fatal("withdraw did not finish")
			}
			if !errors.Is(err, approval.ErrExpired) {
				t.Fatalf("err = %v", err)
			}
			if row := e.approvalRow(s.ApprovalID); row.Status != "expired" || row.RevokeReason != nil || row.ClosedAt == nil {
				t.Errorf("approval = %+v", row)
			}
			if got := e.get(c.ID); got.Status != campaign.StatusContentReady || got.Version != c.Version+1 || got.ActiveApprovalID != nil {
				t.Errorf("campaign = %+v", got)
			}
			if n := e.count(`SELECT count(*) FROM campaign_deliveries WHERE campaign_id = $1::uuid`, c.ID.String()); n != 0 {
				t.Errorf("delivery rows = %d", n)
			}
		})
	}
}

// Sending and finished campaigns: their consumed approval is never
// confirmed, withdrawn or expired — even with its old expires_at in the past.
func TestDecisionsRefusedAfterConsumption(t *testing.T) {
	for _, status := range []campaign.Status{campaign.StatusSending, campaign.StatusCompleted,
		campaign.StatusCompletedWithFailures, campaign.StatusCancelled} {
		t.Run(string(status), func(t *testing.T) {
			e := newDecisionEnv(t)
			c, s := e.prepared()
			out, err := e.approvals.Confirm(ctxBG, c.ID, s.ApprovalID, confirmReq(c, s))
			if err != nil {
				t.Fatal(err)
			}
			// Simulated send (Slice 4): consume, release, campaign status.
			e.exec(`UPDATE campaign_approvals SET status = 'consumed', consumed_at = clock_timestamp(),
			        expires_at = clock_timestamp() - interval '1 hour' WHERE id = $1::uuid`, s.ApprovalID.String())
			e.exec(`UPDATE campaign_deliveries SET status = 'pending' WHERE approval_id = $1::uuid`, s.ApprovalID.String())
			e.exec(`UPDATE campaigns SET status = $2 WHERE id = $1::uuid`, c.ID.String(), string(status))
			cur := e.get(c.ID)
			before := e.state(cur, s.ApprovalID)
			if _, err := e.approvals.Confirm(ctxBG, c.ID, s.ApprovalID, confirmReq(cur, out.Summary)); !errors.Is(err, campaign.ErrInvalidState) {
				t.Errorf("confirm: %v", err)
			}
			if _, err := e.approvals.Withdraw(ctxBG, c.ID, s.ApprovalID, cur.Version); !errors.Is(err, campaign.ErrInvalidState) {
				t.Errorf("withdraw: %v", err)
			}
			if _, err := e.approvals.Get(ctxBG, c.ID, s.ApprovalID); err != nil {
				t.Errorf("get: %v", err)
			}
			if _, err := e.campaigns.Get(ctxBG, c.ID); err != nil {
				t.Errorf("campaign get: %v", err)
			}
			e.assertUnchanged(before, s.ApprovalID)
		})
	}
}

// ---- Read and lazy expiry ----

func TestApprovalReadAndLazyExpiry(t *testing.T) {
	e := newDecisionEnv(t) // campaign service with lazy expiry, as wired in cmd/api
	c, s := e.prepared()

	// Read: identical to the preparation, re-verified from the rows.
	out, err := e.approvals.Get(ctxBG, c.ID, s.ApprovalID)
	if err != nil {
		t.Fatal(err)
	}
	g := out.Summary
	if g.Status != approval.StatusPending || g.SnapshotDigest != s.SnapshotDigest || g.SnapshotCount != 5 || !g.Reviewed ||
		g.CampaignVersion != c.Version || out.Campaign.Version != c.Version || !g.ExpiresAt.Equal(s.ExpiresAt) ||
		g.ContentVersion != s.ContentVersion || !sameCounts(g.AudienceStats, s.AudienceStats) {
		t.Errorf("get = %+v", g)
	}
	// Reads do not write.
	if again := e.get(c.ID); again.Version != c.Version || !again.UpdatedAt.Equal(c.UpdatedAt) {
		t.Errorf("read wrote the campaign: %+v", again)
	}

	// Confirm; then the approved approval reaches its own expires_at.
	conf, err := e.approvals.Confirm(ctxBG, c.ID, s.ApprovalID, confirmReq(c, s))
	if err != nil {
		t.Fatal(err)
	}
	approvedV := conf.Campaign.Version
	e.makeDue(s.ApprovalID)
	// Any campaign read performs the expiry (committed, version + 1) …
	got, err := e.campaigns.Get(ctxBG, c.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != campaign.StatusContentReady || got.Version != approvedV+1 || got.ActiveApprovalID != nil {
		t.Errorf("after lazy expiry = %+v", got)
	}
	// … repeated checks are no-ops …
	for range 3 {
		if again, err := e.campaigns.Get(ctxBG, c.ID); err != nil || again.Version != got.Version || !again.UpdatedAt.Equal(got.UpdatedAt) {
			t.Errorf("repeated: %+v %v", again, err)
		}
	}
	// … and the approval reads back as expired, without held rows.
	exp, err := e.approvals.Get(ctxBG, c.ID, s.ApprovalID)
	if err != nil {
		t.Fatal(err)
	}
	if exp.Summary.Status != approval.StatusExpired || exp.Summary.Reviewed || exp.Summary.ApprovedAt == nil ||
		exp.Summary.Snapshot[segment.ChannelEmail].Recipients != 3 || exp.Summary.Snapshot[segment.ChannelEmail].RenderIssues != nil {
		t.Errorf("expired summary = %+v", exp.Summary)
	}
	if n := len(e.deliveries(s.ApprovalID)); n != 0 {
		t.Errorf("held rows = %d", n)
	}

	// Lazy expiry before a version-guarded edit: the stale request is a
	// conflict, the expiry stays committed.
	c2, s2 := e.prepared()
	e.makeDue(s2.ApprovalID)
	var ce *campaign.ConflictError
	if _, err := e.campaigns.SetChannels(ctxBG, c2.ID, smsOnly, c2.Version); !errors.As(err, &ce) || ce.Current != c2.Version+1 {
		t.Errorf("stale channels: %v", err)
	}
	if row := e.approvalRow(s2.ApprovalID); row.Status != "expired" {
		t.Errorf("approval = %s", row.Status)
	}
}
