package delivery

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/fyusufayan/insider-campaign-composer-case/backend/internal/platform/logging"
	"github.com/fyusufayan/insider-campaign-composer-case/backend/internal/provider"
)

// Delivery statistics (ARCHITECTURE §13, CP17B). Everything here is read
// from persisted rows — never from worker memory — in one short read-only
// REPEATABLE READ transaction, so the partition, the attempt counts and the
// campaign status of one snapshot are mutually consistent. Statistics carry
// counts, sanitized reason codes and masked destinations only: never user
// ids, names, contacts, frozen vars, message text or idempotency keys.

// HeartbeatStaleAfter is the documented worker-freshness threshold: three
// heartbeat periods (DefaultHeartbeatInterval = 5 s). A newer heartbeat is
// "fresh"; an older one is "stale"; no row at all is "absent".
const HeartbeatStaleAfter = 3 * DefaultHeartbeatInterval

// RecentAttemptsWindow is the window of attempts_per_sec (§13: provider
// attempts in the last 10 s / 10).
const RecentAttemptsWindow = 10 * time.Second

// Bounds of the statistics and the delivery list.
const (
	// MaxReasonsPerChannel bounds the reason breakdown; the rest is folded
	// into one "other" entry per status.
	MaxReasonsPerChannel = 20
	DefaultPageSize      = 50
	MaxPageSize          = 100
)

// ErrCampaignNotFound is returned for an unknown campaign.
var ErrCampaignNotFound = errors.New("delivery: campaign not found")

// ErrPartitionMismatch means the rows of a consumed approval do not add up
// to its approved totals — a bug, never a normal state (§13 "asserted").
var ErrPartitionMismatch = errors.New("delivery: delivery rows do not match the approved snapshot")

// AttemptOutcomes is the fixed set of attempt outcomes (delivery_attempts).
var AttemptOutcomes = []provider.Outcome{
	provider.OutcomeAccepted, provider.OutcomeReplayed, provider.OutcomeRateLimited, provider.OutcomeTransientError,
	provider.OutcomeTimeout, provider.OutcomePermanentError, provider.OutcomeUnknown,
}

// PartitionStatuses are the delivery statuses of a consumed approval's
// rows (held rows never exist once an approval is consumed).
var PartitionStatuses = []Status{
	StatusPending, StatusInFlight, StatusRetryScheduled, StatusSent, StatusFailed,
	StatusSkippedConsentRevoked, StatusSkippedNoContact, StatusCancelled,
}

// Channels in canonical order.
var Channels = []provider.Channel{provider.ChannelSMS, provider.ChannelEmail}

// Partition is the exclusive per-status count of a set of delivery rows:
// ApprovedTotal = Pending + InFlight + RetryScheduled + Sent + Failed +
// SkippedConsentRevoked + SkippedNoContact + Cancelled. Uncertain is an
// overlay (rows with outcome_uncertain), never added to the partition.
type Partition struct {
	ApprovedTotal         int64 `json:"approved_total"`
	Pending               int64 `json:"pending"`
	InFlight              int64 `json:"in_flight"`
	RetryScheduled        int64 `json:"retry_scheduled"`
	Sent                  int64 `json:"sent"`
	Failed                int64 `json:"failed"`
	SkippedConsentRevoked int64 `json:"skipped_consent_revoked"`
	SkippedNoContact      int64 `json:"skipped_no_contact"`
	Cancelled             int64 `json:"cancelled"`
	Uncertain             int64 `json:"uncertain"`
}

// Active is the number of rows that are not terminal yet.
func (p Partition) Active() int64 { return p.Pending + p.InFlight + p.RetryScheduled }

// Processed is the number of terminal rows.
func (p Partition) Processed() int64 {
	return p.Sent + p.Failed + p.SkippedConsentRevoked + p.SkippedNoContact + p.Cancelled
}

func (p Partition) sum() int64 { return p.Active() + p.Processed() }

func (p *Partition) add(s Status, n int64) error {
	switch s {
	case StatusPending:
		p.Pending += n
	case StatusInFlight:
		p.InFlight += n
	case StatusRetryScheduled:
		p.RetryScheduled += n
	case StatusSent:
		p.Sent += n
	case StatusFailed:
		p.Failed += n
	case StatusSkippedConsentRevoked:
		p.SkippedConsentRevoked += n
	case StatusSkippedNoContact:
		p.SkippedNoContact += n
	case StatusCancelled:
		p.Cancelled += n
	default:
		return fmt.Errorf("%w: unexpected status %q", ErrPartitionMismatch, s)
	}
	return nil
}

func (p *Partition) merge(o Partition) {
	p.ApprovedTotal += o.ApprovedTotal
	p.Pending += o.Pending
	p.InFlight += o.InFlight
	p.RetryScheduled += o.RetryScheduled
	p.Sent += o.Sent
	p.Failed += o.Failed
	p.SkippedConsentRevoked += o.SkippedConsentRevoked
	p.SkippedNoContact += o.SkippedNoContact
	p.Cancelled += o.Cancelled
	p.Uncertain += o.Uncertain
}

// Attempts counts provider attempts (delivery_attempts rows; a delivery
// with several attempts contributes several). Total = Σ ByOutcome.
// Retries = Total − deliveries with at least one attempt.
type Attempts struct {
	Total     int64            `json:"total"`
	ByOutcome map[string]int64 `json:"by_outcome"`
	// Retries counts attempts after a delivery's first (429s included).
	Retries int64 `json:"retries"`
	// RetriedDeliveries counts deliveries with more than one attempt.
	RetriedDeliveries int64 `json:"retried_deliveries"`
	// Recent counts attempts finished within RecentAttemptsWindow.
	Recent int64 `json:"recent"`
	// PerSecond = Recent / 10 (§13 attempts_per_sec).
	PerSecond float64 `json:"per_second"`
}

func newAttempts() Attempts {
	a := Attempts{ByOutcome: make(map[string]int64, len(AttemptOutcomes))}
	for _, o := range AttemptOutcomes {
		a.ByOutcome[string(o)] = 0
	}
	return a
}

func (a *Attempts) merge(o Attempts) {
	a.Total += o.Total
	a.Retries += o.Retries
	a.RetriedDeliveries += o.RetriedDeliveries
	a.Recent += o.Recent
	for k, v := range o.ByOutcome {
		a.ByOutcome[k] += v
	}
	a.PerSecond = perSecond(a.Recent)
}

func perSecond(recent int64) float64 {
	return float64(recent) / RecentAttemptsWindow.Seconds()
}

// Reason is the number of rows in one (status, last_error code) cell.
// Codes are short sanitized identifiers ([a-z0-9_], ≤ 64), e.g.
// campaign_cancelled, no_consent, http_500, idempotency_key_reuse;
// "unspecified" when the row has none, "other" for folded or unexpected
// values.
type Reason struct {
	Status Status `json:"status"`
	Code   string `json:"code"`
	Count  int64  `json:"count"`
}

// ChannelStats are the statistics of one channel.
type ChannelStats struct {
	Partition
	Attempts Attempts `json:"attempts"`
	Reasons  []Reason `json:"reasons"`
}

// WorkerHealth reports the freshest worker heartbeat (worker_heartbeats,
// database time).
type WorkerHealth struct {
	// Status: "fresh" (age ≤ HeartbeatStaleAfter), "stale", "absent" (no
	// heartbeat row) or "unknown" (readiness only: the database is unavailable).
	Status            string     `json:"status"`
	LastSeenAt        *time.Time `json:"last_seen_at"`
	AgeSeconds        *float64   `json:"heartbeat_age_seconds"`
	StaleAfterSeconds int        `json:"stale_after_seconds"`
	// FreshProcesses counts worker processes with a fresh heartbeat (at
	// most one of them dispatches, ADR-025).
	FreshProcesses int `json:"fresh_processes"`
}

// Worker health statuses.
const (
	WorkerFresh  = "fresh"
	WorkerStale  = "stale"
	WorkerAbsent = "absent"
	// WorkerUnknown: the heartbeat could not be read (database unavailable).
	WorkerUnknown = "unknown"
)

// UnknownWorkerHealth is reported when the heartbeat table cannot be read.
func UnknownWorkerHealth() WorkerHealth {
	return WorkerHealth{Status: WorkerUnknown, StaleAfterSeconds: int(HeartbeatStaleAfter / time.Second)}
}

// ClassifyHeartbeat turns the freshest heartbeat age into a status.
func ClassifyHeartbeat(age *time.Duration, freshProcesses int) WorkerHealth {
	h := WorkerHealth{Status: WorkerAbsent, StaleAfterSeconds: int(HeartbeatStaleAfter / time.Second), FreshProcesses: freshProcesses}
	if age == nil {
		return h
	}
	a := *age
	if a < 0 { // container clocks are not involved (database time), but never report negative ages
		a = 0
	}
	secs := float64(a.Milliseconds()) / 1000
	h.AgeSeconds = &secs
	if a <= HeartbeatStaleAfter {
		h.Status = WorkerFresh
	} else {
		h.Status = WorkerStale
	}
	return h
}

// Outcome values of CampaignStats.Outcome.
const (
	OutcomeNotStarted            = "not_started"
	OutcomeSending               = "sending"
	OutcomeSettling              = "settling" // cancelled; in-flight rows still finishing
	OutcomeCompleted             = "completed"
	OutcomeCompletedWithFailures = "completed_with_failures"
	OutcomeCancelled             = "cancelled"
)

// CampaignStats is one consistent statistics snapshot (§13).
type CampaignStats struct {
	CampaignID      string `json:"campaign_id"`
	CampaignStatus  string `json:"campaign_status"`
	CampaignVersion int    `json:"campaign_version"`
	// The consumed approval (null before a send).
	ApprovalID     *string `json:"approval_id"`
	SegmentVersion *int    `json:"segment_version"`
	ContentID      *int64  `json:"content_id"`
	ContentVersion *int    `json:"content_version"`
	// Channels holds exactly the approved channels ({} before a send).
	Channels map[provider.Channel]ChannelStats `json:"channels"`
	// Totals sums the channels (reasons are per channel only).
	Totals   Partition `json:"totals"`
	Attempts Attempts  `json:"attempts"`
	// Processed = terminal rows; Remaining = pending + in_flight + retry_scheduled.
	Processed int64 `json:"processed"`
	Remaining int64 `json:"remaining"`
	// Progress = Processed / approved_total in [0, 1]; 0 for an empty
	// snapshot (no division by zero).
	Progress float64 `json:"progress"`
	// CampaignTerminal: the campaign status is completed,
	// completed_with_failures or cancelled. Terminal additionally requires
	// that no row is still pending/in flight/scheduled (a cancelled
	// campaign settles once its in-flight rows finish): the final numbers.
	CampaignTerminal bool         `json:"campaign_terminal"`
	Terminal         bool         `json:"terminal"`
	Outcome          string       `json:"outcome"`
	SendStartedAt    *time.Time   `json:"send_started_at"`
	CompletedAt      *time.Time   `json:"completed_at"`
	CancelledAt      *time.Time   `json:"cancelled_at"`
	Worker           WorkerHealth `json:"worker"`
	// UpdatedAt is the database time of the snapshot.
	UpdatedAt time.Time `json:"updated_at"`
}

// ---- raw rows (repository) → statistics (pure) ----

type statsHeader struct {
	status                             string
	version                            int
	sendStartedAt, completedAt, cancel *time.Time
	approvalID                         *string
	segmentVersion                     *int
	contentID                          *int64
	contentVersion                     *int
	audienceStats                      []byte
	channels                           []string
	liveDue                            bool
	now                                time.Time
}

type statusRow struct {
	channel   provider.Channel
	status    Status
	rows      int64
	uncertain int64
	retried   int64
	attempted int64
}

type attemptRow struct {
	channel provider.Channel
	outcome string
	total   int64
	recent  int64
}

type reasonRow struct {
	channel provider.Channel
	status  Status
	code    string
	count   int64
}

type statsRows struct {
	header   statsHeader
	statuses []statusRow
	attempts []attemptRow
	reasons  []reasonRow
	worker   WorkerHealth
}

// approvedTotals reads by_channel[ch].eligible of the approval's stored
// audience statistics (the partition the marketer approved).
func approvedTotals(audienceStats []byte, channels []string) (map[provider.Channel]int64, error) {
	var st struct {
		ByChannel map[string]struct {
			Eligible *int64 `json:"eligible"`
		} `json:"by_channel"`
	}
	if err := json.Unmarshal(audienceStats, &st); err != nil {
		return nil, fmt.Errorf("delivery: approval audience statistics: %w", err)
	}
	out := make(map[provider.Channel]int64, len(channels))
	for _, ch := range channels {
		c, ok := st.ByChannel[ch]
		if !ok || c.Eligible == nil || *c.Eligible < 0 {
			return nil, fmt.Errorf("%w: no approved total for channel %s", ErrPartitionMismatch, ch)
		}
		out[provider.Channel(ch)] = *c.Eligible
	}
	return out, nil
}

func knownChannel(ch provider.Channel) bool {
	return ch == provider.ChannelSMS || ch == provider.ChannelEmail
}

func knownOutcome(o string) bool {
	for _, k := range AttemptOutcomes {
		if string(k) == o {
			return true
		}
	}
	return false
}

// SafeCode returns code if it is a short [a-z0-9_] identifier (the only
// form the worker persists), "unspecified" for an empty code and "other"
// otherwise — so no free text ever reaches a response.
func SafeCode(code string) string {
	if code == "" {
		return "unspecified"
	}
	if len(code) > 64 {
		return "other"
	}
	for _, r := range code {
		if (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '_' {
			return "other"
		}
	}
	return code
}

// assemble builds the statistics from raw rows and checks the partition
// identities against the approved totals.
func assemble(campaignID string, r statsRows) (CampaignStats, error) {
	h := r.header
	out := CampaignStats{
		CampaignID: campaignID, CampaignStatus: h.status, CampaignVersion: h.version,
		ApprovalID: h.approvalID, SegmentVersion: h.segmentVersion, ContentID: h.contentID, ContentVersion: h.contentVersion,
		Channels: map[provider.Channel]ChannelStats{}, Attempts: newAttempts(),
		SendStartedAt: h.sendStartedAt, CompletedAt: h.completedAt, CancelledAt: h.cancel,
		Worker: r.worker, UpdatedAt: h.now,
	}
	switch h.status {
	case "completed", "completed_with_failures", "cancelled":
		out.CampaignTerminal = true
	}
	if h.approvalID != nil {
		totals, err := approvedTotals(h.audienceStats, h.channels)
		if err != nil {
			return CampaignStats{}, err
		}
		for _, ch := range Channels {
			if total, ok := totals[ch]; ok {
				out.Channels[ch] = ChannelStats{Partition: Partition{ApprovedTotal: total}, Attempts: newAttempts(), Reasons: []Reason{}}
			}
		}
		for _, s := range r.statuses {
			cs, ok := out.Channels[s.channel]
			if !ok {
				return CampaignStats{}, fmt.Errorf("%w: rows of channel %q outside the approval", ErrPartitionMismatch, s.channel)
			}
			if err := cs.add(s.status, s.rows); err != nil {
				return CampaignStats{}, err
			}
			cs.Uncertain += s.uncertain
			cs.Attempts.RetriedDeliveries += s.retried
			// attempted is subtracted below: Retries = attempts − attempted deliveries.
			cs.Attempts.Retries -= s.attempted
			out.Channels[s.channel] = cs
		}
		for _, a := range r.attempts {
			cs, ok := out.Channels[a.channel]
			if !ok || !knownOutcome(a.outcome) {
				return CampaignStats{}, fmt.Errorf("%w: attempt (%s, %s) outside the approval", ErrPartitionMismatch, a.channel, a.outcome)
			}
			cs.Attempts.ByOutcome[a.outcome] += a.total
			cs.Attempts.Total += a.total
			cs.Attempts.Recent += a.recent
			out.Channels[a.channel] = cs
		}
		for _, rr := range r.reasons {
			cs, ok := out.Channels[rr.channel]
			if !ok {
				continue
			}
			cs.Reasons = append(cs.Reasons, Reason{Status: rr.status, Code: SafeCode(rr.code), Count: rr.count})
			out.Channels[rr.channel] = cs
		}
		for ch, cs := range out.Channels {
			if cs.sum() != cs.ApprovedTotal {
				return CampaignStats{}, fmt.Errorf("%w: channel %s rows %d ≠ approved %d", ErrPartitionMismatch, ch, cs.sum(), cs.ApprovedTotal)
			}
			cs.Attempts.Retries += cs.Attempts.Total
			if cs.Attempts.Retries < 0 {
				return CampaignStats{}, fmt.Errorf("%w: channel %s has fewer attempts than attempted deliveries", ErrPartitionMismatch, ch)
			}
			cs.Attempts.PerSecond = perSecond(cs.Attempts.Recent)
			cs.Reasons = foldReasons(cs.Reasons)
			out.Channels[ch] = cs
			out.Totals.merge(cs.Partition)
			out.Attempts.merge(cs.Attempts)
		}
	}
	out.Processed, out.Remaining = out.Totals.Processed(), out.Totals.Active()
	if out.Totals.ApprovedTotal > 0 {
		out.Progress = float64(out.Processed) / float64(out.Totals.ApprovedTotal)
	}
	out.Terminal = out.CampaignTerminal && out.Remaining == 0
	switch {
	case out.Terminal:
		out.Outcome = h.status
	case out.CampaignTerminal: // cancelled while rows are still in flight
		out.Outcome = OutcomeSettling
	case h.status == "sending":
		out.Outcome = OutcomeSending
	default:
		out.Outcome = OutcomeNotStarted
	}
	return out, nil
}

// foldReasons merges duplicate (status, code) cells (several raw codes can
// map to "other"), orders by count (desc), status, code and keeps at most
// MaxReasonsPerChannel entries, folding the rest into "other" per status.
func foldReasons(in []Reason) []Reason {
	type key struct {
		s Status
		c string
	}
	m := map[key]int64{}
	for _, r := range in {
		m[key{r.Status, r.Code}] += r.Count
	}
	out := make([]Reason, 0, len(m))
	for k, n := range m {
		out = append(out, Reason{Status: k.s, Code: k.c, Count: n})
	}
	order := func(xs []Reason) {
		sort.Slice(xs, func(i, j int) bool {
			if xs[i].Count != xs[j].Count {
				return xs[i].Count > xs[j].Count
			}
			if xs[i].Status != xs[j].Status {
				return xs[i].Status < xs[j].Status
			}
			return xs[i].Code < xs[j].Code
		})
	}
	order(out)
	if len(out) <= MaxReasonsPerChannel {
		return out
	}
	// Keep the largest cells and fold the rest into one "other" cell per
	// status; shrink the kept part until the result fits.
	sorted := out
	for n := MaxReasonsPerChannel - 1; n > 0; n-- {
		merged := map[key]int64{}
		for _, r := range sorted[:n] {
			merged[key{r.Status, r.Code}] += r.Count
		}
		for _, r := range sorted[n:] {
			merged[key{r.Status, "other"}] += r.Count
		}
		if len(merged) > MaxReasonsPerChannel {
			continue
		}
		res := make([]Reason, 0, len(merged))
		for k, c := range merged {
			res = append(res, Reason{Status: k.s, Code: k.c, Count: c})
		}
		order(res)
		return res
	}
	return sorted[:0] // unreachable: n = 1 yields at most 1 + |statuses| cells
}

// ---- delivery list ----

// ListQuery selects one page of a campaign's delivery rows (rows of its
// consumed approval), ordered by delivery id ascending (stable: ids never
// change, so concurrent status updates cannot reorder or duplicate rows
// across pages; a row may, however, leave or enter a status filter
// between pages).
type ListQuery struct {
	Status    *Status
	Channel   *provider.Channel
	Uncertain *bool
	// After is the cursor: the last id of the previous page (0 = start).
	After int64
	Limit int
}

// Item is one delivery row in its safe form.
type Item struct {
	ID                  int64            `json:"id"`
	Channel             provider.Channel `json:"channel"`
	Status              Status           `json:"status"`
	Attempts            int              `json:"attempts"`
	RateLimitedAttempts int              `json:"rate_limited_attempts"`
	OutcomeUncertain    bool             `json:"outcome_uncertain"`
	// Reason is the sanitized last_error code (null when none).
	Reason *string `json:"reason"`
	// DestinationMasked is the persisted destination, masked
	// (+90********67, a***@example.com); null before the first precheck.
	DestinationMasked *string    `json:"destination_masked"`
	SentAt            *time.Time `json:"sent_at"`
	UpdatedAt         time.Time  `json:"updated_at"`
}

// Page is one page of the delivery list.
type Page struct {
	CampaignID string  `json:"campaign_id"`
	ApprovalID *string `json:"approval_id"`
	Items      []Item  `json:"items"`
	// NextCursor is set when more rows may follow.
	NextCursor *string `json:"next_cursor"`
	Limit      int     `json:"limit"`
}

// MaskDestination masks a destination for display.
func MaskDestination(ch provider.Channel, dest string) string {
	if ch == provider.ChannelSMS {
		return logging.MaskPhone(dest)
	}
	return logging.MaskEmail(dest)
}

// ---- repository ----

const (
	sqlStatsHeader = `SELECT c.status, c.version, c.send_started_at, c.completed_at, c.cancelled_at,
       a.id::text, a.segment_version, a.content_id, cc.version, a.audience_stats, a.channels::text[],
       EXISTS (SELECT 1 FROM campaign_approvals AS l WHERE l.campaign_id = c.id
               AND l.status IN ('pending', 'approved') AND l.expires_at <= clock_timestamp()),
       clock_timestamp()
FROM campaigns AS c
LEFT JOIN campaign_approvals AS a ON a.campaign_id = c.id AND a.status = 'consumed'
LEFT JOIN campaign_contents AS cc ON cc.id = a.content_id
WHERE c.id = $1::uuid`

	// One row per (channel, status) of the consumed approval: rows,
	// uncertain overlay, deliveries with more than one attempt (429s
	// included) and deliveries with at least one attempt.
	sqlStatsStatuses = `SELECT d.channel::text, d.status, count(*),
       count(*) FILTER (WHERE d.outcome_uncertain),
       count(*) FILTER (WHERE d.attempts + d.rate_limited_attempts > 1),
       count(*) FILTER (WHERE EXISTS (SELECT 1 FROM delivery_attempts AS x WHERE x.delivery_id = d.id))
FROM campaign_deliveries AS d
WHERE d.campaign_id = $1::uuid AND d.approval_id = $2::uuid
GROUP BY 1, 2`

	sqlStatsAttempts = `SELECT d.channel::text, x.outcome, count(*),
       count(*) FILTER (WHERE x.finished_at > clock_timestamp() - $3::bigint * interval '1 microsecond')
FROM delivery_attempts AS x
JOIN campaign_deliveries AS d ON d.id = x.delivery_id
WHERE d.campaign_id = $1::uuid AND d.approval_id = $2::uuid
GROUP BY 1, 2`

	// Reasons of rows that did not (yet) end sent. Bounded: last_error
	// values are short codes from a small set.
	sqlStatsReasons = `SELECT d.channel::text, d.status, COALESCE(d.last_error, ''), count(*)
FROM campaign_deliveries AS d
WHERE d.campaign_id = $1::uuid AND d.approval_id = $2::uuid
  AND d.status IN ('retry_scheduled', 'failed', 'skipped_consent_revoked', 'skipped_no_contact', 'cancelled')
GROUP BY 1, 2, 3
ORDER BY 4 DESC, 1, 2, 3
LIMIT 200`

	sqlWorkerHealth = `SELECT max(last_seen_at), (EXTRACT(EPOCH FROM (clock_timestamp() - max(last_seen_at))) * 1000000)::bigint,
       count(*) FILTER (WHERE last_seen_at >= clock_timestamp() - $1::bigint * interval '1 microsecond')
FROM worker_heartbeats`

	sqlSendingCount = `SELECT count(*) FROM campaigns WHERE status = 'sending'`

	sqlListCampaign = `SELECT a.id::text
FROM campaigns AS c
LEFT JOIN campaign_approvals AS a ON a.campaign_id = c.id AND a.status = 'consumed'
WHERE c.id = $1::uuid`

	sqlListDeliveries = `SELECT d.id, d.channel::text, d.status, d.attempts, d.rate_limited_attempts, d.outcome_uncertain,
       d.last_error, d.destination, d.sent_at, d.updated_at
FROM campaign_deliveries AS d
WHERE d.campaign_id = $1::uuid AND d.approval_id = $2::uuid AND d.id > $3
  AND ($4::text IS NULL OR d.status = $4::text)
  AND ($5::text IS NULL OR d.channel = $5::channel)
  AND ($6::boolean IS NULL OR d.outcome_uncertain = $6::boolean)
ORDER BY d.id
LIMIT $7`
)

// StatsReader reads delivery statistics and the delivery list.
type StatsReader struct {
	pool *pgxpool.Pool
	// expire performs lazy approval expiry (§5.3 (b)) for a campaign whose
	// live approval is due, so a stats read never presents a stale
	// "approved" status. Optional.
	expire func(ctx context.Context, campaignID string) error
}

// NewStatsReader returns a reader on pool. expire (optional) is called
// when the campaign's live approval is due (approval.Service.ExpireDue).
func NewStatsReader(pool *pgxpool.Pool, expire func(ctx context.Context, campaignID string) error) *StatsReader {
	return &StatsReader{pool: pool, expire: expire}
}

// Stats returns one consistent statistics snapshot of the campaign.
func (r *StatsReader) Stats(ctx context.Context, campaignID string) (CampaignStats, error) {
	rows, err := r.read(ctx, campaignID)
	if err == nil && rows.header.liveDue && r.expire != nil {
		if err := r.expire(ctx, campaignID); err != nil {
			return CampaignStats{}, fmt.Errorf("delivery: lazy expiry of %s: %w", campaignID, err)
		}
		rows, err = r.read(ctx, campaignID)
	}
	if err != nil {
		return CampaignStats{}, err
	}
	return assemble(campaignID, rows)
}

func readOnlyTx(ctx context.Context, pool *pgxpool.Pool, fn func(pgx.Tx) error) error {
	return pgx.BeginTxFunc(ctx, pool, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly}, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SET LOCAL statement_timeout = '5s'`); err != nil {
			return err
		}
		return fn(tx)
	})
}

func (r *StatsReader) read(ctx context.Context, campaignID string) (statsRows, error) {
	var out statsRows
	err := readOnlyTx(ctx, r.pool, func(tx pgx.Tx) error {
		out = statsRows{}
		h := &out.header
		var sv, cv *int32
		err := tx.QueryRow(ctx, sqlStatsHeader, campaignID).Scan(&h.status, &h.version, &h.sendStartedAt, &h.completedAt, &h.cancel,
			&h.approvalID, &sv, &h.contentID, &cv, &h.audienceStats, &h.channels, &h.liveDue, &h.now)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrCampaignNotFound
		}
		if err != nil {
			return err
		}
		if sv != nil {
			v := int(*sv)
			h.segmentVersion = &v
		}
		if cv != nil {
			v := int(*cv)
			h.contentVersion = &v
		}
		if out.worker, err = workerHealth(ctx, tx); err != nil {
			return err
		}
		if h.approvalID == nil {
			return nil
		}
		rows, err := tx.Query(ctx, sqlStatsStatuses, campaignID, *h.approvalID)
		if err != nil {
			return err
		}
		out.statuses, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (statusRow, error) {
			var (
				s      statusRow
				ch, st string
			)
			err := row.Scan(&ch, &st, &s.rows, &s.uncertain, &s.retried, &s.attempted)
			s.channel, s.status = provider.Channel(ch), Status(st)
			return s, err
		})
		if err != nil {
			return err
		}
		rows, err = tx.Query(ctx, sqlStatsAttempts, campaignID, *h.approvalID, micros(RecentAttemptsWindow))
		if err != nil {
			return err
		}
		out.attempts, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (attemptRow, error) {
			var (
				a  attemptRow
				ch string
			)
			err := row.Scan(&ch, &a.outcome, &a.total, &a.recent)
			a.channel = provider.Channel(ch)
			return a, err
		})
		if err != nil {
			return err
		}
		rows, err = tx.Query(ctx, sqlStatsReasons, campaignID, *h.approvalID)
		if err != nil {
			return err
		}
		out.reasons, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (reasonRow, error) {
			var (
				rr     reasonRow
				ch, st string
			)
			err := row.Scan(&ch, &st, &rr.code, &rr.count)
			rr.channel, rr.status = provider.Channel(ch), Status(st)
			return rr, err
		})
		return err
	})
	if err != nil {
		if errors.Is(err, ErrCampaignNotFound) {
			return statsRows{}, err
		}
		return statsRows{}, fmt.Errorf("delivery: stats of %s: %w", campaignID, err)
	}
	return out, nil
}

type queryRower interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

func workerHealth(ctx context.Context, q queryRower) (WorkerHealth, error) {
	var (
		lastSeen  *time.Time
		ageMicros *int64
		fresh     int64
	)
	if err := q.QueryRow(ctx, sqlWorkerHealth, micros(HeartbeatStaleAfter)).Scan(&lastSeen, &ageMicros, &fresh); err != nil {
		return WorkerHealth{}, err
	}
	var age *time.Duration
	if ageMicros != nil {
		d := time.Duration(*ageMicros) * time.Microsecond
		age = &d
	}
	h := ClassifyHeartbeat(age, int(fresh))
	if age != nil && lastSeen != nil {
		t := lastSeen.UTC()
		h.LastSeenAt = &t
	}
	return h, nil
}

// Readiness is the delivery part of GET /readyz.
type Readiness struct {
	Worker           WorkerHealth `json:"worker"`
	SendingCampaigns int64        `json:"sending_campaigns"`
}

// Readiness reports the worker heartbeat freshness and the number of
// sending campaigns.
func (r *StatsReader) Readiness(ctx context.Context) (Readiness, error) {
	var out Readiness
	var err error
	if out.Worker, err = workerHealth(ctx, r.pool); err != nil {
		return Readiness{}, fmt.Errorf("delivery: worker health: %w", err)
	}
	if err := r.pool.QueryRow(ctx, sqlSendingCount).Scan(&out.SendingCampaigns); err != nil {
		return Readiness{}, fmt.Errorf("delivery: sending campaigns: %w", err)
	}
	return out, nil
}

// List returns one page of the campaign's delivery rows in safe form.
func (r *StatsReader) List(ctx context.Context, campaignID string, q ListQuery) (Page, error) {
	if q.Limit <= 0 || q.Limit > MaxPageSize {
		return Page{}, fmt.Errorf("delivery: page size %d outside 1..%d", q.Limit, MaxPageSize)
	}
	out := Page{CampaignID: campaignID, Items: []Item{}, Limit: q.Limit}
	err := readOnlyTx(ctx, r.pool, func(tx pgx.Tx) error {
		out.Items, out.NextCursor, out.ApprovalID = []Item{}, nil, nil
		err := tx.QueryRow(ctx, sqlListCampaign, campaignID).Scan(&out.ApprovalID)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrCampaignNotFound
		}
		if err != nil || out.ApprovalID == nil {
			return err
		}
		var status, channel *string
		if q.Status != nil {
			s := string(*q.Status)
			status = &s
		}
		if q.Channel != nil {
			c := string(*q.Channel)
			channel = &c
		}
		rows, err := tx.Query(ctx, sqlListDeliveries, campaignID, *out.ApprovalID, q.After, status, channel, q.Uncertain, q.Limit+1)
		if err != nil {
			return err
		}
		items, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Item, error) {
			var (
				it              Item
				ch, st          string
				att, rl         int32
				lastError, dest *string
			)
			err := row.Scan(&it.ID, &ch, &st, &att, &rl, &it.OutcomeUncertain, &lastError, &dest, &it.SentAt, &it.UpdatedAt)
			it.Channel, it.Status, it.Attempts, it.RateLimitedAttempts = provider.Channel(ch), Status(st), int(att), int(rl)
			if lastError != nil {
				c := SafeCode(*lastError)
				it.Reason = &c
			}
			if dest != nil {
				m := MaskDestination(it.Channel, *dest)
				it.DestinationMasked = &m
			}
			return it, err
		})
		if err != nil {
			return err
		}
		if len(items) > q.Limit {
			items = items[:q.Limit]
			c := fmt.Sprint(items[len(items)-1].ID)
			out.NextCursor = &c
		}
		out.Items = items
		return nil
	})
	if err != nil {
		if errors.Is(err, ErrCampaignNotFound) {
			return Page{}, err
		}
		return Page{}, fmt.Errorf("delivery: list deliveries of %s: %w", campaignID, err)
	}
	return out, nil
}

// ValidStatus reports whether s is a partition status (the list filter).
func ValidStatus(s string) bool {
	for _, p := range PartitionStatuses {
		if string(p) == s {
			return true
		}
	}
	return false
}

// ValidChannel reports whether s is a known channel.
func ValidChannel(s string) bool { return knownChannel(provider.Channel(s)) }
