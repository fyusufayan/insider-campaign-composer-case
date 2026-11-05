//go:build integration

package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/fyusufayan/insider-campaign-composer-case/backend/internal/ai"
	"github.com/fyusufayan/insider-campaign-composer-case/backend/internal/approval"
	"github.com/fyusufayan/insider-campaign-composer-case/backend/internal/campaign"
	"github.com/fyusufayan/insider-campaign-composer-case/backend/internal/content"
	"github.com/fyusufayan/insider-campaign-composer-case/backend/internal/llm"
	"github.com/fyusufayan/insider-campaign-composer-case/backend/internal/platform/logging"
	"github.com/fyusufayan/insider-campaign-composer-case/backend/internal/segment"
	"github.com/fyusufayan/insider-campaign-composer-case/backend/test/integration/testsupport"
)

// ---- CP14: approval preparation and the frozen audience, against real
// PostgreSQL (real schema, constraints and audience queries). The LLM is
// scripted and is only used to interpret the brief while setting up a
// campaign; preparation itself never calls it.

const prepTTL = 15 * time.Minute

// lockedBuffer is a goroutine-safe log sink.
type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

type prepEnv struct {
	t         *testing.T
	pool      *pgxpool.Pool
	llm       *llm.Scripted
	campaigns *campaign.Service
	approvals *approval.Service
	logs      *lockedBuffer
	it        *ai.Interpreter
	gen       *ai.Generator
}

// newPrepEnv uses a fresh migrated database with the 30-user fixture.
func newPrepEnv(t *testing.T) *prepEnv {
	t.Helper()
	pool := testsupport.Pool(t, pg.NewMigratedDatabase(t))
	loadFixture(t, pool)
	return newPrepEnvWithPool(t, pool)
}

func newPrepEnvWithPool(t *testing.T, pool *pgxpool.Pool) *prepEnv {
	t.Helper()
	e := &prepEnv{t: t, pool: pool, llm: llm.NewScripted(), logs: &lockedBuffer{}}
	it, err := ai.NewInterpreter(e.llm, ai.InterpreterConfig{Model: "integration-model", Effort: llm.EffortLow,
		Timeout: 30 * time.Second, MaxTokens: 1000})
	if err != nil {
		t.Fatal(err)
	}
	gen, err := ai.NewGenerator(e.llm, ai.GeneratorConfig{Model: "integration-content-model", Effort: llm.EffortMedium,
		Timeout: 30 * time.Second, MaxTokens: 2000})
	if err != nil {
		t.Fatal(err)
	}
	e.it, e.gen = it, gen
	log := logging.New(e.logs, slog.LevelDebug)
	e.campaigns, err = campaign.NewService(campaign.NewRepository(pool), segment.NewAudienceRepository(pool), it, gen, log)
	if err != nil {
		t.Fatal(err)
	}
	e.approvals, err = approval.NewService(approval.NewRepository(pool), approval.Config{PendingTTL: prepTTL}, log)
	if err != nil {
		t.Fatal(err)
	}
	return e
}

var (
	ctxBG = context.Background()

	prepSMS     = "Hi {{first_name|there}}, your cart is waiting. Enjoy 30% off today."
	prepSubject = "{{first_name|Hello}}, your cart is waiting: 30% off"
	prepBody    = "Hi {{first_name|there}},\n\nYour cart is still waiting. Enjoy 30% off your order.\n\nSee you soon!"

	smsAndEmail = []segment.Channel{segment.ChannelSMS, segment.ChannelEmail}
	smsOnly     = []segment.Channel{segment.ChannelSMS}
	emailOnly   = []segment.Channel{segment.ChannelEmail}
)

func sp(s string) *string { return &s }

func draftOf(sms, subject, body string) content.Draft {
	d := content.Draft{}
	if sms != "" {
		d.SMSBody = sp(sms)
	}
	if subject != "" {
		d.EmailSubject, d.EmailBody = sp(subject), sp(body)
	}
	return d
}

// interpreted creates a campaign and interprets it with one scripted reply
// (suggested channels sms+email on the first interpretation).
func (e *prepEnv) interpreted(reply string) campaign.Campaign {
	e.t.Helper()
	c, err := e.campaigns.Create(ctxBG, "Cart recovery", apiExampleBrief)
	if err != nil {
		e.t.Fatal(err)
	}
	e.llm.Push(llm.Reply(reply))
	res, err := e.campaigns.Interpret(ctxBG, c.ID)
	if err != nil {
		e.t.Fatal(err)
	}
	return res.Campaign
}

// ready returns an interpreted campaign with an edited content version for
// both channels (content_ready). No LLM call is made for the content.
func (e *prepEnv) ready(reply string, d content.Draft) campaign.Campaign {
	e.t.Helper()
	c := e.interpreted(reply)
	res, err := e.campaigns.EditContent(ctxBG, c.ID, d, c.Version)
	if err != nil {
		e.t.Fatal(err)
	}
	return res.Campaign
}

func (e *prepEnv) setChannels(c campaign.Campaign, chs []segment.Channel) campaign.Campaign {
	e.t.Helper()
	next, err := e.campaigns.SetChannels(ctxBG, c.ID, chs, c.Version)
	if err != nil {
		e.t.Fatal(err)
	}
	return next
}

func (e *prepEnv) get(id campaign.ID) campaign.Campaign {
	e.t.Helper()
	c, err := e.campaigns.Get(ctxBG, id)
	if err != nil {
		e.t.Fatal(err)
	}
	return c
}

func (e *prepEnv) prepare(c campaign.Campaign) approval.Summary {
	e.t.Helper()
	o, err := e.approvals.Prepare(ctxBG, c.ID, c.Version)
	if err != nil {
		e.t.Fatalf("prepare: %v", err)
	}
	if o.Campaign.Version != o.Summary.CampaignVersion || o.Campaign.Status != campaign.StatusPendingApproval {
		e.t.Fatalf("prepare outcome campaign = %+v", o.Campaign)
	}
	return o.Summary
}

func (e *prepEnv) exec(sql string, args ...any) {
	e.t.Helper()
	if _, err := e.pool.Exec(ctxBG, sql, args...); err != nil {
		e.t.Fatalf("%v\n%s", err, sql)
	}
}

func (e *prepEnv) count(sql string, args ...any) int64 {
	e.t.Helper()
	return scalar(e.t, e.pool, sql, args...)
}

// approvalRow is a campaign_approvals row.
type approvalRow struct {
	Status, Digest        string
	RevokeReason          *string
	SegmentVersion        int
	ContentID             int64
	Channels              []string
	Rule, Stats           []byte
	SnapshotCount         int64
	PreparedAt, ExpiresAt time.Time
	ApprovedAt, ClosedAt  *time.Time
	ConsumedAt            *time.Time
	TTLMicros             int64
}

func (e *prepEnv) approvalRow(id campaign.ID) approvalRow {
	e.t.Helper()
	var r approvalRow
	err := e.pool.QueryRow(ctxBG, `
SELECT status, snapshot_digest, revoke_reason, segment_version, content_id, channels::text[], segment_rule, audience_stats,
       snapshot_count, prepared_at, expires_at, approved_at, closed_at, consumed_at,
       (extract(epoch FROM expires_at - prepared_at) * 1000000)::bigint
FROM campaign_approvals WHERE id = $1::uuid`, id.String()).Scan(
		&r.Status, &r.Digest, &r.RevokeReason, &r.SegmentVersion, &r.ContentID, &r.Channels, &r.Rule, &r.Stats,
		&r.SnapshotCount, &r.PreparedAt, &r.ExpiresAt, &r.ApprovedAt, &r.ClosedAt, &r.ConsumedAt, &r.TTLMicros)
	if err != nil {
		e.t.Fatalf("approval %s: %v", id, err)
	}
	return r
}

// deliveryRow is a campaign_deliveries row as written by prepare.
type deliveryRow struct {
	UserID                 int64
	Channel, Status, Key   string
	Vars                   string
	DestinationNull        bool
	Attempts, RateLimited  int
	Uncertain, LeaseNull   bool
	ApprovalID, CampaignID string
}

func (e *prepEnv) deliveries(approvalID campaign.ID) []deliveryRow {
	e.t.Helper()
	rows, err := e.pool.Query(ctxBG, `
SELECT d.user_id, d.channel::text, d.status, d.idempotency_key, d.vars::text, d.destination IS NULL, d.attempts,
       d.rate_limited_attempts, d.outcome_uncertain, d.lease_until IS NULL AND d.leased_by IS NULL,
       d.approval_id::text, d.campaign_id::text
FROM campaign_deliveries AS d WHERE d.approval_id = $1::uuid ORDER BY d.user_id, d.channel`, approvalID.String())
	if err != nil {
		e.t.Fatal(err)
	}
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (deliveryRow, error) {
		var d deliveryRow
		err := r.Scan(&d.UserID, &d.Channel, &d.Status, &d.Key, &d.Vars, &d.DestinationNull, &d.Attempts, &d.RateLimited,
			&d.Uncertain, &d.LeaseNull, &d.ApprovalID, &d.CampaignID)
		return d, err
	})
	if err != nil {
		e.t.Fatal(err)
	}
	return out
}

// independentEligible computes the eligible (user, channel) pairs for the
// given matched users with hand-written SQL that shares nothing with the
// segment package: contact present and an opted_in consent row.
func (e *prepEnv) independentEligible(userIDs []int64, chs []segment.Channel) []approval.Recipient {
	e.t.Helper()
	names := make([]string, len(chs))
	for i, ch := range chs {
		names[i] = string(ch)
	}
	rows, err := e.pool.Query(ctxBG, `
SELECT u.id, ch.c
FROM users AS u, unnest($2::text[]) AS ch(c)
WHERE u.id = ANY($1)
  AND ((ch.c = 'sms' AND u.phone IS NOT NULL) OR (ch.c = 'email' AND u.email IS NOT NULL))
  AND EXISTS (SELECT 1 FROM channel_consents AS cc
              WHERE cc.user_id = u.id AND cc.channel::text = ch.c AND cc.status = 'opted_in')
ORDER BY u.id, CASE ch.c WHEN 'sms' THEN 0 ELSE 1 END`, userIDs, names)
	if err != nil {
		e.t.Fatal(err)
	}
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (approval.Recipient, error) {
		var rc approval.Recipient
		var ch string
		err := r.Scan(&rc.UserID, &ch)
		rc.Channel = segment.Channel(ch)
		return rc, err
	})
	if err != nil {
		e.t.Fatal(err)
	}
	return out
}

func recipientsOf(ds []deliveryRow) []approval.Recipient {
	out := make([]approval.Recipient, len(ds))
	for i, d := range ds {
		out[i] = approval.Recipient{UserID: d.UserID, Channel: segment.Channel(d.Channel)}
	}
	return out
}

func mustDigest(t *testing.T, rs []approval.Recipient) string {
	t.Helper()
	d, err := approval.Digest(rs)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

// istanbulMatched are the hand-computed matched users of İstanbul ∩
// abandoned_cart(30) in the fixture (see wantIstanbulAudience).
var istanbulMatched = []int64{1, 3, 5, 8}

// checkPrepared verifies a fresh preparation against the database and
// independent SQL.
func (e *prepEnv) checkPrepared(before campaign.Campaign, s approval.Summary, wantStats segment.AudienceStats) {
	t := e.t
	t.Helper()
	// Summary binding.
	if s.Status != approval.StatusPending || s.CampaignID != before.ID || s.CampaignVersion != before.Version+1 ||
		s.SegmentVersion != before.SegmentVersion || s.ContentID != *before.CurrentContentID ||
		!slices.Equal(s.Channels, before.Channels) || s.SnapshotCount != wantStats.Messages {
		t.Errorf("summary binding = %+v (before %+v)", s, before)
	}
	if !sameCounts(s.AudienceStats, wantStats) {
		t.Errorf("summary stats = %+v, want %+v", s.AudienceStats, wantStats)
	}
	if !s.ExpiresAt.Equal(s.PreparedAt.Add(prepTTL)) {
		t.Errorf("expires_at %s, prepared_at %s", s.ExpiresAt, s.PreparedAt)
	}
	// The live preview of the same rule/channels agrees (same audience
	// semantics, recomputed at preparation).
	preview, err := e.campaigns.Audience(ctxBG, before.ID)
	if err != nil || !sameCounts(preview.Stats, s.AudienceStats) {
		t.Errorf("preview %+v (%v) differs from prepare %+v", preview.Stats, err, s.AudienceStats)
	}

	// Approval row.
	row := e.approvalRow(s.ApprovalID)
	var stored segment.AudienceStats
	if err := json.Unmarshal(row.Stats, &stored); err != nil {
		t.Fatal(err)
	}
	wantRule, _ := json.Marshal(before.Segment)
	var gotRule, wantRuleAny any
	_ = json.Unmarshal(row.Rule, &gotRule)
	_ = json.Unmarshal(wantRule, &wantRuleAny)
	chNames := make([]string, len(before.Channels))
	for i, ch := range before.Channels {
		chNames[i] = string(ch)
	}
	if row.Status != "pending" || row.RevokeReason != nil || row.ApprovedAt != nil || row.ClosedAt != nil || row.ConsumedAt != nil ||
		row.SegmentVersion != before.SegmentVersion || row.ContentID != *before.CurrentContentID ||
		!slices.Equal(row.Channels, chNames) || row.SnapshotCount != wantStats.Messages || row.Digest != s.SnapshotDigest ||
		!sameCounts(stored, s.AudienceStats) || fmt.Sprint(gotRule) != fmt.Sprint(wantRuleAny) ||
		row.TTLMicros != prepTTL.Microseconds() || !row.PreparedAt.Equal(s.PreparedAt) {
		t.Errorf("approval row = %+v", row)
	}

	// Held rows = independent eligibility, one per (user, channel).
	ds := e.deliveries(s.ApprovalID)
	want := e.independentEligible(istanbulMatched, before.Channels)
	if got := recipientsOf(ds); !slices.Equal(got, want) {
		t.Errorf("held rows = %v, independent SQL = %v", got, want)
	}
	if mustDigest(t, want) != s.SnapshotDigest {
		t.Errorf("digest %s differs from the independent digest", s.SnapshotDigest)
	}
	perChannel := map[segment.Channel]int64{}
	for _, d := range ds {
		perChannel[segment.Channel(d.Channel)]++
		wantKey := before.ID.String() + ":" + fmt.Sprint(d.UserID) + ":" + d.Channel
		if d.Status != "held" || d.Key != wantKey || !d.DestinationNull || d.Attempts != 0 || d.RateLimited != 0 ||
			d.Uncertain || !d.LeaseNull || d.ApprovalID != s.ApprovalID.String() || d.CampaignID != before.ID.String() {
			t.Errorf("delivery row = %+v", d)
		}
	}
	for _, ch := range before.Channels {
		if perChannel[ch] != wantStats.ByChannel[ch].Eligible || s.Snapshot[ch].Recipients != perChannel[ch] {
			t.Errorf("%s: rows %d, eligible %d, summary %d", ch, perChannel[ch], wantStats.ByChannel[ch].Eligible, s.Snapshot[ch].Recipients)
		}
	}
	if len(s.Snapshot) != len(before.Channels) {
		t.Errorf("snapshot channels = %v", s.Snapshot)
	}

	// Campaign state.
	after := e.get(before.ID)
	if after.Status != campaign.StatusPendingApproval || after.ActiveApprovalID == nil || *after.ActiveApprovalID != s.ApprovalID ||
		after.Version != before.Version+1 {
		t.Errorf("campaign after = %+v", after)
	}
	// Exactly one live approval for the campaign.
	if n := e.count(`SELECT count(*) FROM campaign_approvals WHERE campaign_id = $1::uuid AND status IN ('pending','approved')`,
		before.ID.String()); n != 1 {
		t.Errorf("live approvals = %d", n)
	}
}

func singleChannelStats(ch segment.Channel) segment.AudienceStats {
	cs := wantIstanbulAudience.ByChannel[ch]
	s := segment.AudienceStats{MatchedUsers: 4, ByChannel: map[segment.Channel]segment.ChannelStats{ch: cs}, Messages: cs.Eligible}
	if ch == segment.ChannelSMS {
		s.ReachUsers = segment.ReachStats{SMSOnly: cs.Eligible, Unreachable: 4 - cs.Eligible}
	} else {
		s.ReachUsers = segment.ReachStats{EmailOnly: cs.Eligible, Unreachable: 4 - cs.Eligible}
	}
	return s
}

// ---- Tests ----

func TestPrepareChannelSelections(t *testing.T) {
	cases := []struct {
		name string
		chs  []segment.Channel
		want segment.AudienceStats
	}{
		{"sms and email", smsAndEmail, wantIstanbulAudience},
		{"sms only", smsOnly, singleChannelStats(segment.ChannelSMS)},
		{"email only", emailOnly, singleChannelStats(segment.ChannelEmail)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newPrepEnv(t)
			c := e.ready(replyIstanbul, draftOf(prepSMS, prepSubject, prepBody))
			if !slices.Equal(c.Channels, tc.chs) {
				c = e.setChannels(c, tc.chs)
			}
			calls := e.llm.Calls()
			s := e.prepare(c)
			e.checkPrepared(c, s, tc.want)
			if e.llm.Calls() != calls {
				t.Errorf("prepare called the LLM (%d → %d calls)", calls, e.llm.Calls())
			}
			// Content of an unselected channel is reported, never frozen.
			if len(tc.chs) == 1 && !slices.ContainsFunc(s.Review.Validation.Warnings,
				func(w segment.FieldError) bool { return w.Code == campaign.CodeChannelNotSelected }) {
				t.Errorf("warnings = %+v", s.Review.Validation.Warnings)
			}
			if len(s.Review.OfferFindings) != 0 || s.RenderIssueCount() != 0 {
				t.Errorf("review = %+v, snapshot = %+v", s.Review.OfferFindings, s.Snapshot)
			}
		})
	}
}

// Expected sms/email recipients of the İstanbul rule: sms {1, 8}, email {1, 3, 5}.
func TestPrepareFrozenPersonalization(t *testing.T) {
	e := newPrepEnv(t)
	e.exec(`UPDATE users SET first_name = 'Ayşe', last_name = 'Yılmaz' WHERE id = 1`)
	e.exec(`UPDATE users SET first_name = '   ' WHERE id = 3`) // blank: passes the CHECK, unusable without a fallback
	e.exec(`UPDATE users SET first_name = 'Ömer', last_name = 'Çelik' WHERE id = 5`)
	// A display city longer than the declared {{city}} maximum (users.city has
	// no length CHECK); its folded key still matches İstanbul.
	longCity := strings.Repeat("İstanbul", 6)
	e.exec(`UPDATE users SET city = $1 WHERE id = 8`, longCity)

	c := e.ready(replyIstanbul, draftOf(
		"Hi {{first_name|there}} in {{city}}, your cart is waiting. 30% off today.",
		"{{first_name}}, your cart is waiting: 30% off", // no fallback
		prepBody))
	s := e.prepare(c)

	want := map[int64]map[string]string{
		1: {"first_name": "Ayşe", "last_name": "Yılmaz", "city": "İstanbul"},
		3: {"first_name": "   ", "last_name": "User", "city": "Istanbul"},
		5: {"first_name": "Ömer", "last_name": "Çelik", "city": "İSTANBUL"},
		8: {"first_name": "Test", "last_name": "User", "city": longCity},
	}
	for _, d := range e.deliveries(s.ApprovalID) {
		var got map[string]string
		if err := json.Unmarshal([]byte(d.Vars), &got); err != nil {
			t.Fatal(err)
		}
		// Exactly the three allowed variables, verbatim (not trimmed,
		// normalized, truncated or replaced by a fallback); no contact data.
		if fmt.Sprint(got) != fmt.Sprint(want[d.UserID]) {
			t.Errorf("user %d %s vars = %q, want %q", d.UserID, d.Channel, got, want[d.UserID])
		}
		if strings.Contains(d.Vars, "@") || strings.Contains(d.Vars, "+90") {
			t.Errorf("vars contain contact data: %s", d.Vars)
		}
	}
	// Render review: user 8's SMS exceeds the {{city}} maximum; user 3's
	// email subject has no fallback for the blank first name (user 3 is not
	// sms-eligible). Users 1 and 5 render fine on both channels.
	if got := s.Snapshot[segment.ChannelSMS].RenderIssues; fmt.Sprint(got) != fmt.Sprint(map[string]int64{content.RenderValueTooLong: 1}) {
		t.Errorf("sms render issues = %v", got)
	}
	if got := s.Snapshot[segment.ChannelEmail].RenderIssues; fmt.Sprint(got) != fmt.Sprint(map[string]int64{content.RenderMissingValue: 1}) {
		t.Errorf("email render issues = %v", got)
	}
	// Render issues never exclude anyone: the snapshot is the partition.
	if s.SnapshotCount != 5 {
		t.Errorf("snapshot_count = %d", s.SnapshotCount)
	}

	// Frozen: later changes to users never change held rows, and users who
	// start matching after prepare are never added (no re-evaluation).
	before := e.deliveries(s.ApprovalID)
	e.exec(`UPDATE users SET first_name = 'Changed', email = 'changed@example.test' WHERE id = 1`)
	e.exec(`INSERT INTO users (id, first_name, last_name, email, phone, city, city_normalized, gender, signup_at)
	        VALUES (31, 'New', 'User', 'user31@example.test', '+905550000031', 'İstanbul', 'istanbul', 'female', now())`)
	e.exec(`INSERT INTO channel_consents (user_id, channel, status, source) VALUES (31, 'sms', 'opted_in', 't'), (31, 'email', 'opted_in', 't')`)
	e.exec(`INSERT INTO cart_events (user_id, product_id, category, quantity, unit_price, occurred_at) VALUES (31, 'P', 'toys', 1, 1, now())`)
	if after := e.deliveries(s.ApprovalID); fmt.Sprint(after) != fmt.Sprint(before) {
		t.Errorf("held rows changed after prepare:\n%v\n%v", before, after)
	}
}

func TestPrepareEmptyAudience(t *testing.T) {
	e := newPrepEnv(t)
	// Çanakkale ∩ male = user 22, who has no phone and no email.
	reply := scriptedReply(`{"match":"all","conditions":[{"type":"attribute","field":"city","op":"eq","value":"Çanakkale"},`+
		`{"type":"attribute","field":"gender","op":"eq","value":"male"}]}`, `["sms","email"]`)
	c := e.ready(reply, draftOf(prepSMS, prepSubject, prepBody))
	_, err := e.approvals.Prepare(ctxBG, c.ID, c.Version)
	var ee *approval.EmptyAudienceError
	if !errors.As(err, &ee) || !errors.Is(err, approval.ErrEmptyAudience) || ee.Stats.MatchedUsers != 1 || ee.Stats.Messages != 0 ||
		ee.Stats.ByChannel[segment.ChannelSMS].NoContact != 1 || ee.Stats.ByChannel[segment.ChannelEmail].NoContact != 1 {
		t.Fatalf("err = %v (%+v)", err, ee)
	}
	if n := e.count(`SELECT count(*) FROM campaign_approvals`) + e.count(`SELECT count(*) FROM campaign_deliveries`); n != 0 {
		t.Errorf("rows written: %d", n)
	}
	if after := e.get(c.ID); after.Status != campaign.StatusContentReady || after.Version != c.Version || after.ActiveApprovalID != nil {
		t.Errorf("campaign changed: %+v", after)
	}
}

// A re-preparation that finds nobody eligible rolls back completely: the
// live approval it would have superseded stays live with its held rows.
func TestPrepareEmptyAudienceKeepsLiveApproval(t *testing.T) {
	e := newPrepEnv(t)
	c := e.ready(replyIstanbul, draftOf(prepSMS, prepSubject, prepBody))
	first := e.prepare(c)
	e.exec(`UPDATE channel_consents SET status = 'opted_out' WHERE user_id = ANY($1)`, istanbulMatched)
	pending := e.get(c.ID)
	if _, err := e.approvals.Prepare(ctxBG, c.ID, pending.Version); !errors.Is(err, approval.ErrEmptyAudience) {
		t.Fatalf("err = %v", err)
	}
	if row := e.approvalRow(first.ApprovalID); row.Status != "pending" || row.RevokeReason != nil {
		t.Errorf("first approval = %+v", row)
	}
	if n := len(e.deliveries(first.ApprovalID)); n != 5 {
		t.Errorf("held rows = %d", n)
	}
	if after := e.get(c.ID); after.Version != pending.Version || *after.ActiveApprovalID != first.ApprovalID {
		t.Errorf("campaign = %+v", after)
	}
}

func TestPrepareRejectsInvalidCampaigns(t *testing.T) {
	e := newPrepEnv(t)
	prep := func(c campaign.Campaign) error {
		_, err := e.approvals.Prepare(ctxBG, c.ID, c.Version)
		return err
	}
	var se *campaign.StateError
	var ve *campaign.ValidationError

	// Draft and segmented (no content).
	draftC, err := e.campaigns.Create(ctxBG, "Draft", apiExampleBrief)
	if err != nil {
		t.Fatal(err)
	}
	if err := prep(draftC); !errors.As(err, &se) || se.Status != campaign.StatusDraft {
		t.Errorf("draft: %v", err)
	}
	seg := e.interpreted(replyIstanbul)
	if err := prep(seg); !errors.As(err, &se) || se.Status != campaign.StatusSegmented {
		t.Errorf("segmented: %v", err)
	}

	// No channels selected.
	c := e.ready(replyIstanbul, draftOf(prepSMS, prepSubject, prepBody))
	none := e.setChannels(c, []segment.Channel{})
	if err := prep(none); !errors.As(err, &ve) || ve.Problems[0].Path != "channels" {
		t.Errorf("no channels: %v", err)
	}

	// Content that does not cover the selection: sms-only content, then
	// email is selected as well.
	smsC := e.setChannels(e.interpreted(replyIstanbul), smsOnly)
	res, err := e.campaigns.EditContent(ctxBG, smsC.ID, draftOf(prepSMS, "", ""), smsC.Version)
	if err != nil {
		t.Fatal(err)
	}
	both := e.setChannels(res.Campaign, smsAndEmail)
	if both.Status != campaign.StatusContentReady {
		t.Fatalf("status = %s", both.Status)
	}
	if err := prep(both); !errors.As(err, &ve) || codes(ve.Problems) != "email_subject:required,email_body:required" {
		t.Errorf("uncovered channel: %v", err)
	}

	// Stale campaign version.
	ok := e.ready(replyIstanbul, draftOf(prepSMS, prepSubject, prepBody))
	var ce *campaign.ConflictError
	if _, err := e.approvals.Prepare(ctxBG, ok.ID, ok.Version-1); !errors.As(err, &ce) || ce.Current != ok.Version {
		t.Errorf("stale: %v", err)
	}

	// Sending and terminal states.
	for _, st := range []campaign.Status{campaign.StatusSending, campaign.StatusCompleted,
		campaign.StatusCompletedWithFailures, campaign.StatusCancelled} {
		x := e.ready(replyIstanbul, draftOf(prepSMS, prepSubject, prepBody))
		e.exec(`UPDATE campaigns SET status = $2 WHERE id = $1::uuid`, x.ID.String(), string(st))
		if err := prep(x); !errors.As(err, &se) || se.Status != st {
			t.Errorf("%s: %v", st, err)
		}
	}

	// Unknown campaign.
	if _, err := e.approvals.Prepare(ctxBG, campaign.ID{1}, 1); !errors.Is(err, campaign.ErrNotFound) {
		t.Errorf("not found: %v", err)
	}

	// Nothing was written by any rejected preparation.
	if n := e.count(`SELECT count(*) FROM campaign_approvals`) + e.count(`SELECT count(*) FROM campaign_deliveries`); n != 0 {
		t.Errorf("rows written: %d", n)
	}
}

func codes(errs segment.Errors) string {
	var out []string
	for _, e := range errs {
		out = append(out, e.Path+":"+e.Code)
	}
	return strings.Join(out, ",")
}

func TestPrepareRepeatedAndSupersede(t *testing.T) {
	e := newPrepEnv(t)
	c := e.ready(replyIstanbul, draftOf(prepSMS, prepSubject, prepBody))
	first := e.prepare(c)

	// Repeating the same request (same expected version) is a conflict: it
	// never silently replaces the approval that the first request created.
	if _, err := e.approvals.Prepare(ctxBG, c.ID, c.Version); !errors.Is(err, campaign.ErrVersionConflict) {
		t.Fatalf("repeat: %v", err)
	}
	if row := e.approvalRow(first.ApprovalID); row.Status != "pending" {
		t.Fatalf("first approval after repeat = %s", row.Status)
	}

	// An explicit re-preparation at the current version supersedes it
	// (§5.2): revoked(superseded), its held rows deleted, history kept.
	pending := e.get(c.ID)
	second := e.prepare(pending)
	if second.Superseded == nil || *second.Superseded != first.ApprovalID {
		t.Errorf("superseded = %v", second.Superseded)
	}
	old := e.approvalRow(first.ApprovalID)
	if old.Status != "revoked" || old.RevokeReason == nil || *old.RevokeReason != "superseded" || old.ClosedAt == nil ||
		old.SnapshotCount != 5 || old.Digest != first.SnapshotDigest {
		t.Errorf("superseded row = %+v", old)
	}
	if n := len(e.deliveries(first.ApprovalID)); n != 0 {
		t.Errorf("superseded approval still has %d rows", n)
	}
	// Same audience ⇒ same digest; the delivery rows are new.
	if second.SnapshotDigest != first.SnapshotDigest || second.CampaignVersion != pending.Version+1 {
		t.Errorf("second = %+v", second)
	}
	if n := e.count(`SELECT count(*) FROM campaign_deliveries WHERE campaign_id = $1::uuid`, c.ID.String()); n != 5 {
		t.Errorf("delivery rows = %d", n)
	}

	if n := e.count(`SELECT count(*) FROM campaign_approvals WHERE campaign_id = $1::uuid`, c.ID.String()); n != 2 {
		t.Errorf("approval history = %d rows", n)
	}
}

// markApproved simulates a confirmed approval (confirm is CP15): approved
// with the 24 h approved TTL, campaign approved.
func (e *prepEnv) markApproved(c campaign.Campaign, id campaign.ID) campaign.Campaign {
	e.t.Helper()
	e.exec(`UPDATE campaign_approvals SET status = 'approved', approved_at = clock_timestamp(), approved_by = 'test',
	        expires_at = clock_timestamp() + interval '24 hours' WHERE id = $1::uuid`, id.String())
	e.exec(`UPDATE campaigns SET status = 'approved' WHERE id = $1::uuid`, c.ID.String())
	return e.get(c.ID)
}

// A plain prepare never invalidates a confirmed approval (deliberate CP14
// deviation from §5.2, see §9.1): approved → 409 invalid_state, nothing
// changes. Once the approved approval has expired (its own 24 h TTL, not the
// pending TTL), lazy expiry closes it with one version increment; the
// marketer reloads and prepares from content_ready.
func TestPrepareRefusesApprovedCampaign(t *testing.T) {
	e := newPrepEnv(t)
	c := e.ready(replyIstanbul, draftOf(prepSMS, prepSubject, prepBody))
	first := e.prepare(c)
	approved := e.markApproved(c, first.ApprovalID)
	rowBefore := e.approvalRow(first.ApprovalID)
	if !rowBefore.ExpiresAt.After(rowBefore.PreparedAt.Add(prepTTL)) {
		t.Fatalf("approved TTL not distinct from the pending TTL: %+v", rowBefore)
	}

	for _, version := range []int{approved.Version, approved.Version - 1} {
		_, err := e.approvals.Prepare(ctxBG, c.ID, version)
		var se *campaign.StateError
		var ce *campaign.ConflictError
		if version == approved.Version && (!errors.As(err, &se) || se.Status != campaign.StatusApproved) {
			t.Errorf("current version: %v", err)
		}
		if version != approved.Version && !errors.As(err, &ce) {
			t.Errorf("stale version: %v", err)
		}
	}
	// The pending TTL (15 min) has long been exceeded relative to a pending
	// approval's clock, but an approved approval is governed by its own
	// expires_at: lazy expiry leaves it alone.
	e.exec(`UPDATE campaign_approvals SET prepared_at = clock_timestamp() - interval '1 hour' WHERE id = $1::uuid`, first.ApprovalID.String())
	if id, err := e.approvals.ExpireDue(ctxBG, c.ID); err != nil || id != nil {
		t.Errorf("approved approval expired early: %v %v", id, err)
	}
	row := e.approvalRow(first.ApprovalID)
	if row.Status != "approved" || row.RevokeReason != nil || row.ClosedAt != nil || !row.ExpiresAt.Equal(rowBefore.ExpiresAt) {
		t.Errorf("approved approval changed: %+v", row)
	}
	if got := recipientsOf(e.deliveries(first.ApprovalID)); mustDigest(t, got) != first.SnapshotDigest {
		t.Errorf("approved snapshot changed: %v", got)
	}
	if after := e.get(c.ID); after.Status != campaign.StatusApproved || after.Version != approved.Version ||
		*after.ActiveApprovalID != first.ApprovalID {
		t.Errorf("campaign changed: %+v", after)
	}
	if n := e.count(`SELECT count(*) FROM campaign_approvals`); n != 1 {
		t.Errorf("approvals = %d", n)
	}

	// The approved approval reaches its own expires_at (set by database
	// time): a prepare based on the approved view expires it (committed,
	// version + 1) and is itself a version conflict; it never supersedes it.
	e.exec(`UPDATE campaign_approvals SET expires_at = clock_timestamp() - interval '1 second' WHERE id = $1::uuid`, first.ApprovalID.String())
	var ce *campaign.ConflictError
	if _, err := e.approvals.Prepare(ctxBG, c.ID, approved.Version); !errors.As(err, &ce) || ce.Current != approved.Version+1 {
		t.Fatalf("prepare on the expired approved view: %v", err)
	}
	if row := e.approvalRow(first.ApprovalID); row.Status != "expired" || row.RevokeReason != nil || row.ApprovedAt == nil {
		t.Errorf("expired approved row = %+v", row)
	}
	reloaded := e.get(c.ID)
	if reloaded.Status != campaign.StatusContentReady || reloaded.Version != approved.Version+1 || reloaded.ActiveApprovalID != nil {
		t.Fatalf("after approved expiry = %+v", reloaded)
	}
	s := e.prepare(reloaded)
	if s.Superseded != nil || s.ApprovalID == first.ApprovalID || s.CampaignVersion != reloaded.Version+1 {
		t.Errorf("fresh = %+v", s)
	}
}

func TestPrepareConcurrentAttempts(t *testing.T) {
	e := newPrepEnv(t)
	c := e.ready(replyIstanbul, draftOf(prepSMS, prepSubject, prepBody))
	const n = 8
	start := make(chan struct{})
	errs := make(chan error, n)
	var wg sync.WaitGroup
	for range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, err := e.approvals.Prepare(ctxBG, c.ID, c.Version)
			errs <- err
		}()
	}
	close(start)
	wg.Wait()
	close(errs)
	var ok, conflicts int
	for err := range errs {
		switch {
		case err == nil:
			ok++
		case errors.Is(err, campaign.ErrVersionConflict):
			conflicts++
		default:
			t.Errorf("unexpected error: %v", err)
		}
	}
	if ok != 1 || conflicts != n-1 {
		t.Errorf("ok = %d, conflicts = %d", ok, conflicts)
	}
	if a := e.count(`SELECT count(*) FROM campaign_approvals`); a != 1 {
		t.Errorf("approvals = %d", a)
	}
	if d := e.count(`SELECT count(*) FROM campaign_deliveries`); d != 5 {
		t.Errorf("deliveries = %d", d)
	}
	if d := e.count(`SELECT count(DISTINCT (campaign_id, user_id, channel)) FROM campaign_deliveries`); d != 5 {
		t.Errorf("distinct deliveries = %d", d)
	}
}

// lockCampaign opens a transaction holding the campaign row lock.
func (e *prepEnv) lockCampaign(id campaign.ID) pgx.Tx {
	e.t.Helper()
	tx, err := e.pool.Begin(ctxBG)
	if err != nil {
		e.t.Fatal(err)
	}
	e.t.Cleanup(func() { _ = tx.Rollback(ctxBG) })
	if _, err := tx.Exec(ctxBG, `SELECT 1 FROM campaigns WHERE id = $1::uuid FOR UPDATE`, id.String()); err != nil {
		e.t.Fatal(err)
	}
	return tx
}

type prepResult struct {
	s   approval.Summary
	err error
}

func (e *prepEnv) prepareAsync(id campaign.ID, version int) <-chan prepResult {
	ch := make(chan prepResult, 1)
	go func() {
		o, err := e.approvals.Prepare(ctxBG, id, version)
		ch <- prepResult{o.Summary, err}
	}()
	return ch
}

func awaitPrep(t *testing.T, ch <-chan prepResult) prepResult {
	t.Helper()
	select {
	case r := <-ch:
		return r
	case <-time.After(30 * time.Second):
		t.Fatal("prepare did not finish")
		return prepResult{}
	}
}

// The campaign lock statement of the approval transaction.
const lockStmtPrefix = "SELECT id, name, brief, status"

// A campaign edit that commits while preparation waits for the campaign
// lock makes the preparation stale: it is rejected, nothing is written.
func TestPrepareRacingCampaignEdit(t *testing.T) {
	e := newPrepEnv(t)
	c := e.ready(replyIstanbul, draftOf(prepSMS, prepSubject, prepBody))

	tx := e.lockCampaign(c.ID)
	done := e.prepareAsync(c.ID, c.Version)
	waitForLockWait(t, e.pool, lockStmtPrefix)
	// The concurrent edit: a channel change (version + 1) by another session.
	if _, err := tx.Exec(ctxBG, `UPDATE campaigns SET channels = '{sms}', version = version + 1 WHERE id = $1::uuid`, c.ID.String()); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctxBG); err != nil {
		t.Fatal(err)
	}
	r := awaitPrep(t, done)
	var ce *campaign.ConflictError
	if !errors.As(r.err, &ce) || ce.Expected != c.Version || ce.Current != c.Version+1 {
		t.Fatalf("err = %v", r.err)
	}
	if n := e.count(`SELECT count(*) FROM campaign_approvals`) + e.count(`SELECT count(*) FROM campaign_deliveries`); n != 0 {
		t.Errorf("rows written: %d", n)
	}
}

// Consent changes are evaluated at preparation time, per channel.
func TestPrepareChannelsFilteredIndependently(t *testing.T) {
	e := newPrepEnv(t)
	c := e.ready(replyIstanbul, draftOf(prepSMS, prepSubject, prepBody))
	// User 1 opts out of sms only; user 3 opts in to sms (has a phone);
	// user 5 loses the email address.
	e.exec(`UPDATE channel_consents SET status = 'opted_out' WHERE user_id = 1 AND channel = 'sms'`)
	e.exec(`UPDATE channel_consents SET status = 'opted_in' WHERE user_id = 3 AND channel = 'sms'`)
	e.exec(`UPDATE users SET email = NULL WHERE id = 5`)
	s := e.prepare(c)
	got := recipientsOf(e.deliveries(s.ApprovalID))
	rc := func(id int64, ch segment.Channel) approval.Recipient {
		return approval.Recipient{UserID: id, Channel: ch}
	}
	want := []approval.Recipient{rc(1, segment.ChannelEmail), rc(3, segment.ChannelSMS), rc(3, segment.ChannelEmail), rc(8, segment.ChannelSMS)}
	if !slices.Equal(got, want) || !slices.Equal(got, e.independentEligible(istanbulMatched, smsAndEmail)) {
		t.Errorf("held = %v, want %v", got, want)
	}
	sms, email := s.AudienceStats.ByChannel[segment.ChannelSMS], s.AudienceStats.ByChannel[segment.ChannelEmail]
	if sms != (segment.ChannelStats{Eligible: 2, NoConsent: 1, NoContact: 1}) ||
		email != (segment.ChannelStats{Eligible: 2, NoConsent: 1, NoContact: 1}) || s.SnapshotCount != 4 {
		t.Errorf("stats = %+v", s.AudienceStats)
	}
}

func TestPrepareExpiry(t *testing.T) {
	e := newPrepEnv(t)
	c := e.ready(replyIstanbul, draftOf(prepSMS, prepSubject, prepBody))
	first := e.prepare(c)

	// Not due: nothing happens, nothing is written.
	pending := e.get(c.ID)
	if id, err := e.approvals.ExpireDue(ctxBG, c.ID); err != nil || id != nil {
		t.Fatalf("expire not due: %v %v", id, err)
	}
	if again := e.get(c.ID); again.Version != pending.Version || !again.UpdatedAt.Equal(pending.UpdatedAt) {
		t.Errorf("not-due expiry wrote the campaign: %+v", again)
	}
	// Make it due (test-only: moves expires_at into the past by database time).
	e.exec(`UPDATE campaign_approvals SET expires_at = clock_timestamp() - interval '1 second' WHERE id = $1::uuid`, first.ApprovalID.String())
	id, err := e.approvals.ExpireDue(ctxBG, c.ID)
	if err != nil || id == nil || *id != first.ApprovalID {
		t.Fatalf("expire: %v %v", id, err)
	}
	row := e.approvalRow(first.ApprovalID)
	if row.Status != "expired" || row.RevokeReason != nil || row.ClosedAt == nil || row.SnapshotCount != 5 {
		t.Errorf("expired row = %+v", row)
	}
	if n := len(e.deliveries(first.ApprovalID)); n != 0 {
		t.Errorf("held rows after expiry = %d", n)
	}
	after := e.get(c.ID)
	// content_ready, no approval, version advanced exactly once.
	if after.Status != campaign.StatusContentReady || after.ActiveApprovalID != nil || after.Version != pending.Version+1 {
		t.Errorf("campaign after expiry = %+v", after)
	}
	// Repeated expiry checks are no-ops: no write, no version bump.
	for range 3 {
		if id, err := e.approvals.ExpireDue(ctxBG, c.ID); err != nil || id != nil {
			t.Errorf("repeated expiry: %v %v", id, err)
		}
	}
	if again := e.get(c.ID); again.Version != after.Version || !again.UpdatedAt.Equal(after.UpdatedAt) {
		t.Errorf("repeated expiry wrote the campaign: %+v", again)
	}
	// A new preparation from the reloaded state starts a fresh approval.
	second := e.prepare(after)
	if second.Superseded != nil || second.ApprovalID == first.ApprovalID || second.CampaignVersion != after.Version+1 {
		t.Errorf("second = %+v", second)
	}

	// Lazy expiry inside Prepare: a request based on the pre-expiry view
	// commits the expiry (version + 1) and is rejected as a conflict; the due
	// approval is expired, never superseded.
	beforeLazy := e.get(c.ID)
	e.exec(`UPDATE campaign_approvals SET expires_at = clock_timestamp() - interval '1 millisecond' WHERE id = $1::uuid`, second.ApprovalID.String())
	var ce *campaign.ConflictError
	if _, err := e.approvals.Prepare(ctxBG, c.ID, beforeLazy.Version); !errors.As(err, &ce) ||
		ce.Expected != beforeLazy.Version || ce.Current != beforeLazy.Version+1 {
		t.Fatalf("prepare on the pre-expiry view: %v", err)
	}
	if row := e.approvalRow(second.ApprovalID); row.Status != "expired" || row.RevokeReason != nil {
		t.Errorf("lazily expired row = %+v", row)
	}
	reloaded := e.get(c.ID)
	if reloaded.Version != beforeLazy.Version+1 || reloaded.Status != campaign.StatusContentReady {
		t.Errorf("after lazy expiry = %+v", reloaded)
	}
	third := e.prepare(reloaded)
	if third.Superseded != nil || third.CampaignVersion != reloaded.Version+1 {
		t.Errorf("third = %+v", third)
	}
	// Expired approvals keep their binding and audit data.
	if n := e.count(`SELECT count(*) FROM campaign_approvals WHERE status = 'expired' AND snapshot_count = 5 AND snapshot_digest = $1`,
		first.SnapshotDigest); n != 2 {
		t.Errorf("expired history rows = %d", n)
	}
}

// If an approval becomes due between Service.Prepare's lazy expiry and the
// preparation transaction, the preparation transaction expires it after its
// own lock, which advances the version: the request is a conflict and the
// whole transaction — including that expiry — rolls back. The approval is
// never superseded; the next lazy expiry commits it.
func TestPrepareTransactionNeverSupersedesADueApproval(t *testing.T) {
	e := newPrepEnv(t)
	c := e.ready(replyIstanbul, draftOf(prepSMS, prepSubject, prepBody))
	first := e.prepare(c)
	pending := e.get(c.ID)
	e.exec(`UPDATE campaign_approvals SET expires_at = clock_timestamp() - interval '1 second' WHERE id = $1::uuid`, first.ApprovalID.String())

	repo := approval.NewRepository(e.pool) // the transaction alone, without the service's lazy expiry
	var ce *campaign.ConflictError
	_, err := repo.Prepare(ctxBG, approval.PrepareParams{CampaignID: c.ID, ExpectedVersion: pending.Version, PendingTTL: prepTTL})
	if !errors.As(err, &ce) || ce.Current != pending.Version+1 {
		t.Fatalf("err = %v", err)
	}
	if row := e.approvalRow(first.ApprovalID); row.Status != "pending" || row.RevokeReason != nil {
		t.Errorf("rolled-back approval = %+v", row)
	}
	if after := e.get(c.ID); after.Version != pending.Version || after.Status != campaign.StatusPendingApproval {
		t.Errorf("campaign = %+v", after)
	}
	if n := len(e.deliveries(first.ApprovalID)); n != 5 {
		t.Errorf("held rows = %d", n)
	}
	if n := e.count(`SELECT count(*) FROM campaign_approvals`); n != 1 {
		t.Errorf("approvals = %d", n)
	}
}

// An approval that expires while the lazy expiry of a preparation waits for
// the campaign lock is expired after the lock (clock_timestamp()), even
// though that transaction started before expiry; the preparation, based on
// the pre-expiry version, is then a conflict. It is never superseded.
func TestPrepareLockWaitExpiry(t *testing.T) {
	e := newPrepEnv(t)
	c := e.ready(replyIstanbul, draftOf(prepSMS, prepSubject, prepBody))
	first := e.prepare(c)
	pending := e.get(c.ID)

	var expiresAt time.Time
	if err := e.pool.QueryRow(ctxBG, `UPDATE campaign_approvals SET expires_at = clock_timestamp() + interval '300 milliseconds'
	    WHERE id = $1::uuid RETURNING expires_at`, first.ApprovalID.String()).Scan(&expiresAt); err != nil {
		t.Fatal(err)
	}
	tx := e.lockCampaign(c.ID)
	done := e.prepareAsync(c.ID, pending.Version)
	waitForLockWait(t, e.pool, lockStmtPrefix)
	var xactStart time.Time
	if err := e.pool.QueryRow(ctxBG, `SELECT xact_start FROM pg_stat_activity
	    WHERE datname = current_database() AND wait_event_type = 'Lock' AND starts_with(ltrim(query), $1)`,
		lockStmtPrefix).Scan(&xactStart); err != nil {
		t.Fatal(err)
	}
	if !xactStart.Before(expiresAt) {
		t.Fatalf("the waiting transaction started at %s, after expiry %s", xactStart, expiresAt)
	}
	waitForDBClockPast(t, e.pool, expiresAt)
	if err := tx.Commit(ctxBG); err != nil {
		t.Fatal(err)
	}
	r := awaitPrep(t, done)
	var ce *campaign.ConflictError
	if !errors.As(r.err, &ce) || ce.Current != pending.Version+1 {
		t.Fatalf("err = %v", r.err)
	}
	if row := e.approvalRow(first.ApprovalID); row.Status != "expired" || row.RevokeReason != nil || !row.ClosedAt.After(expiresAt) {
		t.Errorf("first approval = %+v", row)
	}
	after := e.get(c.ID)
	if after.Status != campaign.StatusContentReady || after.Version != pending.Version+1 || after.ActiveApprovalID != nil {
		t.Errorf("campaign = %+v", after)
	}
	if n := e.count(`SELECT count(*) FROM campaign_deliveries`); n != 0 {
		t.Errorf("rows = %d", n)
	}
}

// waitForDBClockPast polls clock_timestamp() until it passes t (2 s deadline).
func waitForDBClockPast(t *testing.T, pool *pgxpool.Pool, at time.Time) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for {
		var past bool
		if err := pool.QueryRow(ctxBG, `SELECT clock_timestamp() > $1`, at).Scan(&past); err != nil {
			t.Fatal(err)
		}
		if past {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("database clock did not pass the expiry")
		}
		<-tick.C
	}
}

// Failures injected at two points of the preparation transaction (after the
// held rows are inserted, and at the final campaign update) roll back
// everything, including the supersede of the previous approval.
func TestPrepareInjectedFailureRollsBack(t *testing.T) {
	e := newPrepEnv(t)
	c := e.ready(replyIstanbul, draftOf(prepSMS, prepSubject, prepBody))
	first := e.prepare(c)
	pending := e.get(c.ID)

	e.exec(`CREATE FUNCTION test_fail() RETURNS trigger LANGUAGE plpgsql AS $$
	        BEGIN RAISE EXCEPTION 'injected failure'; END $$`)
	triggers := []string{
		`CREATE TRIGGER fail_held AFTER INSERT ON campaign_deliveries FOR EACH STATEMENT EXECUTE FUNCTION test_fail()`,
		`CREATE TRIGGER fail_campaign BEFORE UPDATE ON campaigns FOR EACH ROW
		 WHEN (NEW.status = 'pending_approval' AND NEW.version > OLD.version) EXECUTE FUNCTION test_fail()`,
	}
	for _, trg := range triggers {
		e.exec(trg)
		_, err := e.approvals.Prepare(ctxBG, c.ID, pending.Version)
		if err == nil || !strings.Contains(err.Error(), "injected failure") {
			t.Fatalf("%s: err = %v", trg, err)
		}
		if row := e.approvalRow(first.ApprovalID); row.Status != "pending" || row.RevokeReason != nil || row.ClosedAt != nil {
			t.Errorf("first approval after failure = %+v", row)
		}
		if n := e.count(`SELECT count(*) FROM campaign_approvals`); n != 1 {
			t.Errorf("approvals = %d", n)
		}
		if got := recipientsOf(e.deliveries(first.ApprovalID)); mustDigest(t, got) != first.SnapshotDigest {
			t.Errorf("held rows of the live approval changed: %v", got)
		}
		if n := e.count(`SELECT count(*) FROM campaign_deliveries`); n != 5 {
			t.Errorf("deliveries = %d", n)
		}
		if after := e.get(c.ID); after.Version != pending.Version || *after.ActiveApprovalID != first.ApprovalID ||
			after.Status != campaign.StatusPendingApproval {
			t.Errorf("campaign = %+v", after)
		}
		name := strings.Fields(trg)[2]
		e.exec(`DROP TRIGGER ` + name + ` ON ` + map[string]string{"fail_held": "campaign_deliveries", "fail_campaign": "campaigns"}[name])
	}
	// Without the triggers the same request succeeds.
	if s := e.prepare(pending); s.Superseded == nil || *s.Superseded != first.ApprovalID {
		t.Errorf("after recovery: %+v", s)
	}
}

// No recipient data in logs or in the review summary; the scripted LLM saw
// only the brief (interpretation during setup), never recipient rows.
func TestPrepareLeaksNoRecipientData(t *testing.T) {
	e := newPrepEnv(t)
	e.exec(`UPDATE users SET first_name = 'Zeynepxq', last_name = 'Kayaxq' WHERE id = ANY($1)`, istanbulMatched)
	c := e.ready(replyIstanbul, draftOf(prepSMS, prepSubject, prepBody))
	s := e.prepare(c)
	e.exec(`UPDATE campaign_approvals SET expires_at = clock_timestamp() - interval '1 second' WHERE id = $1::uuid`, s.ApprovalID.String())
	if _, err := e.approvals.ExpireDue(ctxBG, c.ID); err != nil {
		t.Fatal(err)
	}
	_ = e.prepare(e.get(c.ID))
	_, _ = e.approvals.Prepare(ctxBG, c.ID, 0) // a failure is logged too

	summary, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	var reqs strings.Builder
	for _, r := range e.llm.Requests() {
		b, _ := json.Marshal(r)
		reqs.Write(b)
	}
	sensitive := []string{"Zeynepxq", "Kayaxq", "@example.test", "+90555"}
	for _, id := range istanbulMatched {
		sensitive = append(sensitive, fmt.Sprintf("user%d@", id), fmt.Sprintf("+90555000%04d", id))
	}
	for name, text := range map[string]string{"logs": e.logs.String(), "summary": string(summary), "llm requests": reqs.String()} {
		for _, s := range sensitive {
			if strings.Contains(text, s) {
				t.Errorf("%s contain %q", name, s)
			}
		}
	}
	if !strings.Contains(e.logs.String(), `"msg":"approval prepared"`) || !strings.Contains(e.logs.String(), `"approval_id"`) {
		t.Errorf("expected structured approval logs, got:\n%s", e.logs.String())
	}
	// The rendered example in the review uses only fictional values.
	if ex := s.Review.Validation.RenderedExample; ex == nil || !strings.Contains(*ex.SMS, content.ExampleValues().FirstName) {
		t.Errorf("rendered example = %+v", ex)
	}
}

// Offer findings of edited content survive into the approval review
// (§7.9): preparation succeeds, the findings are listed for confirm.
func TestPrepareKeepsOfferFindings(t *testing.T) {
	e := newPrepEnv(t)
	c := e.ready(replyIstanbul, draftOf(
		"Hi {{first_name|there}}, 50% off today and free shipping!",
		"{{first_name|Hello}}, your cart: 30% off",
		"Hi {{first_name|there}},\n\nOrders over 500 TL ship with a free gift.\n\nSee you!"))
	s := e.prepare(c)
	if got := codes(s.Review.OfferFindings); got != "sms_body:unauthorized_offer,sms_body:unauthorized_offer,email_body:unauthorized_offer,email_body:unauthorized_claim" {
		t.Errorf("offer findings = %s", got)
	}
	if s.Review.Offer.Kind != ai.OfferPercentDiscount || s.Review.Offer.Value != 30 {
		t.Errorf("offer = %+v", s.Review.Offer)
	}
	if row := e.approvalRow(s.ApprovalID); row.Status != "pending" {
		t.Errorf("approval = %+v", row)
	}
}

// Against the deterministic 5,000-user seed: held rows equal independently
// computed eligible pairs for İstanbul ∩ abandoned_cart(30).
func TestPrepareSeededAudience(t *testing.T) {
	pool := seededPool(t, nil)
	e := newPrepEnvWithPool(t, pool)
	c := e.ready(replyIstanbul, draftOf(prepSMS, prepSubject, prepBody))
	s := e.prepare(c)
	matched := matchedIDs(t, pool, c.Segment)
	want := e.independentEligible(matched, smsAndEmail)
	got := recipientsOf(e.deliveries(s.ApprovalID))
	if len(want) == 0 || !slices.Equal(got, want) || s.SnapshotCount != int64(len(want)) || mustDigest(t, want) != s.SnapshotDigest {
		t.Errorf("held %d rows, independent %d, snapshot_count %d", len(got), len(want), s.SnapshotCount)
	}
	if s.AudienceStats.MatchedUsers != int64(len(matched)) {
		t.Errorf("matched = %d, independent %d", s.AudienceStats.MatchedUsers, len(matched))
	}
	if s.RenderIssueCount() != 0 {
		t.Errorf("seeded users produced render issues: %+v", s.Snapshot)
	}
	t.Logf("seed: matched=%d sms=%d email=%d", len(matched), s.Snapshot[segment.ChannelSMS].Recipients, s.Snapshot[segment.ChannelEmail].Recipients)
}

// Concurrent lazy expiry (ExpireDue, as the sweeper and API will call it)
// and preparation: the campaign lock serializes them, a due approval is
// expired exactly once (one version increment, never superseded), every
// preparation based on the pre-expiry version is a conflict, and there is
// never more than one live approval.
func TestPrepareConcurrentWithExpiry(t *testing.T) {
	e := newPrepEnv(t)
	c := e.ready(replyIstanbul, draftOf(prepSMS, prepSubject, prepBody))
	first := e.prepare(c)
	pending := e.get(c.ID)
	e.exec(`UPDATE campaign_approvals SET expires_at = clock_timestamp() - interval '1 second' WHERE id = $1::uuid`, first.ApprovalID.String())

	race := func(version int) (prepared []approval.Summary, expiredReturned int) {
		const n = 6
		type out struct {
			prep    *approval.Summary
			expired *campaign.ID
			err     error
		}
		start := make(chan struct{})
		results := make(chan out, 2*n)
		var wg sync.WaitGroup
		for i := range 2 * n {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				if i%2 == 0 {
					o, err := e.approvals.Prepare(ctxBG, c.ID, version)
					if err != nil {
						results <- out{err: err}
						return
					}
					results <- out{prep: &o.Summary}
					return
				}
				id, err := e.approvals.ExpireDue(ctxBG, c.ID)
				results <- out{expired: id, err: err}
			}()
		}
		close(start)
		wg.Wait()
		close(results)
		for r := range results {
			switch {
			case r.prep != nil:
				prepared = append(prepared, *r.prep)
				if r.prep.Superseded != nil {
					t.Errorf("an approval was superseded: %v", *r.prep.Superseded)
				}
			case r.err != nil && !errors.Is(r.err, campaign.ErrVersionConflict):
				t.Errorf("unexpected error: %v", r.err)
			case r.expired != nil:
				expiredReturned++
			}
		}
		return prepared, expiredReturned
	}

	// Phase 1: everybody holds the pre-expiry version.
	prepared, _ := race(pending.Version)
	if len(prepared) != 0 {
		t.Fatalf("%d preparations succeeded on the pre-expiry version", len(prepared))
	}
	if row := e.approvalRow(first.ApprovalID); row.Status != "expired" || row.RevokeReason != nil {
		t.Errorf("first approval = %+v", row)
	}
	expired := e.get(c.ID)
	if expired.Version != pending.Version+1 || expired.Status != campaign.StatusContentReady || expired.ActiveApprovalID != nil {
		t.Fatalf("after phase 1 = %+v (exactly one increment expected)", expired)
	}

	// Phase 2: the reloaded version; nothing is due any more.
	prepared, expiredReturned := race(expired.Version)
	if len(prepared) != 1 || expiredReturned != 0 {
		t.Fatalf("prepared = %d, expired = %d", len(prepared), expiredReturned)
	}
	final := e.get(c.ID)
	if final.Version != expired.Version+1 || *final.ActiveApprovalID != prepared[0].ApprovalID {
		t.Errorf("final = %+v", final)
	}
	if live := e.count(`SELECT count(*) FROM campaign_approvals WHERE status IN ('pending','approved')`); live != 1 {
		t.Errorf("live approvals = %d", live)
	}
	if n := e.count(`SELECT count(*) FROM campaign_deliveries WHERE approval_id <> $1::uuid`, prepared[0].ApprovalID.String()); n != 0 {
		t.Errorf("%d rows of another approval remain", n)
	}
}

// Expiry advances the campaign version, so a client whose view predates it
// is rejected by every version-guarded mutation (409 version_conflict); after
// an explicit reload it proceeds through the valid transitions.
func TestStaleClientAfterExpiry(t *testing.T) {
	e := newPrepEnv(t)
	c := e.ready(replyIstanbul, draftOf(prepSMS, prepSubject, prepBody))
	s := e.prepare(c)
	stale := e.get(c.ID) // the client's view: pending_approval at version V

	// While the approval is live, edits are refused (until CP15 can revoke it).
	edited := draftOf(prepSMS+" Now!", prepSubject, prepBody)
	if _, err := e.campaigns.EditContent(ctxBG, c.ID, edited, stale.Version); !errors.Is(err, campaign.ErrInvalidState) {
		t.Fatalf("edit while pending: %v", err)
	}
	e.exec(`UPDATE campaign_approvals SET expires_at = clock_timestamp() - interval '1 second' WHERE id = $1::uuid`, s.ApprovalID.String())
	if id, err := e.approvals.ExpireDue(ctxBG, c.ID); err != nil || id == nil {
		t.Fatalf("expire: %v %v", id, err)
	}
	if n := e.count(`SELECT count(*) FROM campaign_deliveries WHERE campaign_id = $1::uuid`, c.ID.String()); n != 0 {
		t.Fatalf("rows after expiry = %d", n)
	}

	// Every version-guarded mutation based on the pre-expiry view is a conflict.
	var ce *campaign.ConflictError
	if _, err := e.campaigns.EditContent(ctxBG, c.ID, edited, stale.Version); !errors.As(err, &ce) || ce.Current != stale.Version+1 {
		t.Errorf("stale edit: %v", err)
	}
	if _, err := e.campaigns.SetChannels(ctxBG, c.ID, smsOnly, stale.Version); !errors.Is(err, campaign.ErrVersionConflict) {
		t.Errorf("stale channels: %v", err)
	}
	if _, err := e.approvals.Prepare(ctxBG, c.ID, stale.Version); !errors.Is(err, campaign.ErrVersionConflict) {
		t.Errorf("stale prepare: %v", err)
	}
	if after := e.get(c.ID); after.Version != stale.Version+1 {
		t.Errorf("rejected requests changed the version: %d", after.Version)
	}

	// Explicit reload, then valid transitions: edit → prepare.
	reloaded := e.get(c.ID)
	if reloaded.Status != campaign.StatusContentReady || reloaded.ActiveApprovalID != nil {
		t.Fatalf("reloaded = %+v", reloaded)
	}
	res, err := e.campaigns.EditContent(ctxBG, c.ID, edited, reloaded.Version)
	if err != nil || res.Campaign.Version != reloaded.Version+1 {
		t.Fatalf("edit after reload: %v", err)
	}
	fresh := e.prepare(res.Campaign)
	if fresh.ApprovalID == s.ApprovalID || fresh.ContentID == s.ContentID || fresh.Superseded != nil ||
		fresh.CampaignVersion != res.Campaign.Version+1 {
		t.Errorf("fresh = %+v", fresh)
	}
	if row := e.approvalRow(s.ApprovalID); row.Status != "expired" {
		t.Errorf("expired approval = %s", row.Status)
	}
	for _, d := range e.deliveries(fresh.ApprovalID) {
		if d.ApprovalID != fresh.ApprovalID.String() {
			t.Errorf("row of another approval: %+v", d)
		}
	}
}

// The real constraints: one live approval per campaign, one delivery row
// per (campaign, user, channel) across ALL approvals of the campaign, and a
// globally unique idempotency key equal to campaign:user:channel.
func TestApprovalUniquenessConstraints(t *testing.T) {
	e := newPrepEnv(t)
	c := e.ready(replyIstanbul, draftOf(prepSMS, prepSubject, prepBody))
	first := e.prepare(c)
	firstKeys := keysOf(e.deliveries(first.ApprovalID))

	// A second live approval cannot exist (approvals_one_live).
	_, err := e.pool.Exec(ctxBG, `INSERT INTO campaign_approvals (campaign_id, status, segment_version, segment_rule, content_id,
	    channels, audience_stats, snapshot_count, snapshot_digest, prepared_at, expires_at)
	  SELECT campaign_id, 'pending', segment_version, segment_rule, content_id, channels, audience_stats, snapshot_count,
	    snapshot_digest, clock_timestamp(), clock_timestamp() + interval '15 minutes' FROM campaign_approvals WHERE id = $1::uuid`,
		first.ApprovalID.String())
	assertUniqueViolation(t, err, "approvals_one_live")

	// Re-preparation reuses exactly the same keys for the same recipients:
	// the superseded approval's held rows are deleted in the same
	// transaction first, so they never collide, and the new rows belong to
	// the new approval only.
	second := e.prepare(e.get(c.ID))
	secondRows := e.deliveries(second.ApprovalID)
	if !slices.Equal(keysOf(secondRows), firstKeys) || len(e.deliveries(first.ApprovalID)) != 0 {
		t.Errorf("keys first %v, second %v", firstKeys, keysOf(secondRows))
	}

	// The same recipient cannot be frozen twice for one campaign, under any
	// approval (UNIQUE (campaign_id, user_id, channel)), nor reuse a key.
	_, err = e.pool.Exec(ctxBG, `INSERT INTO campaign_deliveries (campaign_id, approval_id, user_id, channel, vars, idempotency_key)
	    VALUES ($1::uuid, $2::uuid, 1, 'sms', '{}', 'other-key')`, c.ID.String(), first.ApprovalID.String())
	assertUniqueViolation(t, err, "campaign_deliveries_campaign_id_user_id_channel_key")
	_, err = e.pool.Exec(ctxBG, `INSERT INTO campaign_deliveries (campaign_id, approval_id, user_id, channel, vars, idempotency_key)
	    VALUES ($1::uuid, $2::uuid, 30, 'sms', '{}', $3)`, c.ID.String(), second.ApprovalID.String(), secondRows[0].Key)
	assertUniqueViolation(t, err, "campaign_deliveries_idempotency_key_key")

	// Once a snapshot has been released for sending (simulated: confirm and
	// send are CP15), its rows are permanent and the campaign can never be
	// prepared again, so the campaign-scoped key can never be reused by a
	// second snapshot (approvals_one_consumed).
	e.exec(`UPDATE campaign_approvals SET status = 'consumed', approved_at = clock_timestamp(), approved_by = 't',
	        consumed_at = clock_timestamp() WHERE id = $1::uuid`, second.ApprovalID.String())
	e.exec(`UPDATE campaign_deliveries SET status = 'pending' WHERE approval_id = $1::uuid`, second.ApprovalID.String())
	e.exec(`UPDATE campaigns SET status = 'sending' WHERE id = $1::uuid`, c.ID.String())
	// The consumed approval's old expires_at has passed: lazy expiry and
	// prepare must not touch a sending campaign or its consumed approval.
	e.exec(`UPDATE campaign_approvals SET expires_at = clock_timestamp() - interval '1 hour' WHERE id = $1::uuid`, second.ApprovalID.String())
	sending := e.get(c.ID)
	if _, err := e.approvals.Prepare(ctxBG, c.ID, sending.Version); !errors.Is(err, campaign.ErrInvalidState) {
		t.Errorf("prepare while sending: %v", err)
	}
	if id, err := e.approvals.ExpireDue(ctxBG, c.ID); err != nil || id != nil {
		t.Errorf("expire while sending: %v %v", id, err)
	}
	if after := e.get(c.ID); after.Version != sending.Version || !after.UpdatedAt.Equal(sending.UpdatedAt) ||
		after.Status != campaign.StatusSending || *after.ActiveApprovalID != second.ApprovalID {
		t.Errorf("sending campaign modified: %+v", after)
	}
	if row := e.approvalRow(second.ApprovalID); row.Status != "consumed" || row.ClosedAt != nil {
		t.Errorf("consumed approval modified: %+v", row)
	}
	if n := e.count(`SELECT count(*) FROM campaign_deliveries WHERE approval_id = $1::uuid AND status = 'pending'`,
		second.ApprovalID.String()); n != 5 {
		t.Errorf("released rows = %d", n)
	}
}

func keysOf(ds []deliveryRow) []string {
	out := make([]string, len(ds))
	for i, d := range ds {
		out[i] = d.Key
	}
	return out
}

func assertUniqueViolation(t *testing.T, err error, constraint string) {
	t.Helper()
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "23505" || pgErr.ConstraintName != constraint {
		t.Errorf("want unique violation of %s, got %v", constraint, err)
	}
}

// Customer data that changes between the snapshot statement and the
// INSERT … SELECT is detected by the digest, even when the counts stay the
// same, and the whole preparation rolls back. The change is injected
// deterministically by a trigger on the approval insert (which runs between
// the two statements, in the same transaction).
func TestPrepareDetectsAudienceChangeBetweenStatements(t *testing.T) {
	cases := map[string]string{
		// sms recipients {1, 8} become {3, 8}: same count, different set.
		"same count, different recipients": `UPDATE channel_consents SET status = CASE user_id WHEN 1 THEN 'opted_out' ELSE 'opted_in' END
		    WHERE channel = 'sms' AND user_id IN (1, 3)`,
		"fewer recipients": `UPDATE channel_consents SET status = 'opted_out' WHERE channel = 'email' AND user_id = 5`,
		"contact removed":  `UPDATE users SET phone = NULL WHERE id = 8`,
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			e := newPrepEnv(t)
			c := e.ready(replyIstanbul, draftOf(prepSMS, prepSubject, prepBody))
			e.exec(`CREATE FUNCTION test_change() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN ` + change + `; RETURN NULL; END $$`)
			e.exec(`CREATE TRIGGER change_audience AFTER INSERT ON campaign_approvals FOR EACH ROW EXECUTE FUNCTION test_change()`)
			_, err := e.approvals.Prepare(ctxBG, c.ID, c.Version)
			if !errors.Is(err, approval.ErrAudienceChanged) {
				t.Fatalf("err = %v", err)
			}
			if n := e.count(`SELECT count(*) FROM campaign_approvals`) + e.count(`SELECT count(*) FROM campaign_deliveries`); n != 0 {
				t.Errorf("rows written: %d", n)
			}
			// The injected change was part of the rolled-back transaction.
			if got := e.independentEligible(istanbulMatched, smsAndEmail); len(got) != 5 {
				t.Errorf("customer data changed: %v", got)
			}
			if after := e.get(c.ID); after.Status != campaign.StatusContentReady || after.Version != c.Version {
				t.Errorf("campaign = %+v", after)
			}
		})
	}
}
