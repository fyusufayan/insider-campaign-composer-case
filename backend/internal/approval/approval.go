// Package approval owns approval preparation and the frozen audience
// snapshot (ARCHITECTURE §5, §9; ADR-005, ADR-006, ADR-007). It is the only
// code that inserts campaign_deliveries rows: prepare inserts them as
// `held`, bound to one pending approval, in the same transaction that locks
// the campaign row, re-validates content and channels and computes the
// audience partition.
//
// CP14 implements preparation and expiry; CP15 adds confirmation,
// withdrawal and the reviewable summary read. Sending follows in Slice 4.
//
// Privacy boundary: recipient rows (user ids, names, cities) are read only
// inside the preparation transaction and written only to campaign_deliveries.
// Nothing in this package calls the LLM, and no type returned by it carries
// customer data: summaries contain counts, a digest over (user_id, channel)
// pairs, and the content review rendered with fictional example values.
package approval

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"strconv"
	"time"

	"github.com/fyusufayan/insider-campaign-composer-case/backend/internal/campaign"
	"github.com/fyusufayan/insider-campaign-composer-case/backend/internal/content"
	"github.com/fyusufayan/insider-campaign-composer-case/backend/internal/segment"
)

// Status is campaign_approvals.status (§5.3).
type Status string

// Statuses.
const (
	StatusPending  Status = "pending"
	StatusApproved Status = "approved"
	StatusConsumed Status = "consumed"
	StatusRevoked  Status = "revoked"
	StatusExpired  Status = "expired"
)

// Live reports whether the approval is pending or approved (it has held rows).
func (s Status) Live() bool { return s == StatusPending || s == StatusApproved }

// RevokeReason is campaign_approvals.revoke_reason.
type RevokeReason string

// Revoke reasons used by preparation and withdrawal.
const (
	RevokeSuperseded RevokeReason = "superseded"
	RevokeWithdrawn  RevokeReason = "withdrawn"
)

// Pending TTL bounds. The default is ARCHITECTURE §16's APPROVAL_PENDING_TTL.
const (
	DefaultPendingTTL = 15 * time.Minute
	MaxPendingTTL     = 24 * time.Hour
	// MinPendingTTL keeps a configuration typo (e.g. "15ms") from creating
	// approvals that expire before the marketer can read them; tests that
	// need an imminent expiry set expires_at directly.
	MinPendingTTL = time.Second
)

// Approved TTL bounds. The default is ARCHITECTURE §16's
// APPROVAL_APPROVED_TTL: a confirmed approval may be consumed until
// approved_at + TTL (§5.3), both from one clock_timestamp() value taken
// after the campaign lock.
const (
	DefaultApprovedTTL = 24 * time.Hour
	MinApprovedTTL     = time.Second
	MaxApprovedTTL     = 7 * 24 * time.Hour
)

// MaxApproverRunes bounds campaign_approvals.approved_by (APPROVER_NAME).
const MaxApproverRunes = 100

var (
	// ErrEmptyAudience: no selected channel has an eligible recipient
	// (422 empty_audience). Nothing is written; a live approval stays live.
	ErrEmptyAudience = errors.New("approval: no eligible recipients")
	// ErrAudienceChanged: customer data changed between the statements of
	// the preparation transaction, so the frozen rows no longer equal the
	// computed partition. Nothing is written; retrying is safe.
	ErrAudienceChanged = errors.New("approval: audience changed during preparation")
	// ErrBusy: the campaign row lock or a statement exceeded its timeout, or
	// the database aborted the transaction (serialization failure,
	// deadlock). Nothing is written; retrying is safe.
	ErrBusy = errors.New("approval: campaign is busy")
	// ErrInconsistent: stored approval state contradicts an invariant
	// (e.g. held rows that do not match the approval's snapshot_count). It
	// indicates a bug; the transaction is rolled back.
	ErrInconsistent = errors.New("approval: inconsistent approval state")
	// ErrNotFound: no approval with the id belongs to the campaign (404).
	ErrNotFound = errors.New("approval: not found")
	// ErrExpired: the approval is expired — it was already, or it became
	// due and was expired (committed) by this request after the campaign
	// lock (409 approval_expired). It can never be confirmed or reactivated.
	ErrExpired = errors.New("approval: expired")
	// ErrMismatch: the approval's binding (segment version, content version,
	// channels) differs from the request or from the campaign (409
	// approval_mismatch); see *MismatchError.
	ErrMismatch = errors.New("approval: binding mismatch")
)

// MismatchError lists which bound values differ. Paths name request fields
// (segment_version, content_version, channels) or, for a difference between
// the approval and the current campaign, campaign.<field>. Messages never
// echo values.
type MismatchError struct {
	Fields []string
}

func (e *MismatchError) Error() string {
	return fmt.Sprintf("approval: binding mismatch (%v)", e.Fields)
}

// Is matches ErrMismatch.
func (e *MismatchError) Is(target error) bool { return target == ErrMismatch }

// EmptyAudienceError carries the partition that was computed, so the
// marketer can see why nobody is eligible (counts only).
type EmptyAudienceError struct {
	Stats segment.AudienceStats
}

func (e *EmptyAudienceError) Error() string {
	return fmt.Sprintf("approval: no eligible recipients (%d matched users)", e.Stats.MatchedUsers)
}

// Is matches ErrEmptyAudience.
func (e *EmptyAudienceError) Is(target error) bool { return target == ErrEmptyAudience }

// ChannelSnapshot describes the held rows of one channel.
type ChannelSnapshot struct {
	// Recipients is the number of held rows (= audience eligible count).
	Recipients int64
	// RenderIssues counts held rows whose frozen values cannot be rendered
	// into this channel's message, by content render code (missing_value,
	// invalid_value, value_too_long, content_too_long). Such a delivery
	// fails permanently at send time (§8.7); values are never truncated or
	// replaced. Never nil.
	RenderIssues map[string]int64
}

// Summary is the reviewable state of an approval: the result of a
// preparation, or an approval read back / confirmed / withdrawn. It never
// contains customer data. It is a domain type; the HTTP DTO is defined in
// httpapi.
type Summary struct {
	ApprovalID campaign.ID
	CampaignID campaign.ID
	Status     Status
	// RevokeReason is set iff Status is revoked.
	RevokeReason *RevokeReason
	// CampaignVersion is the campaign version this summary was read or
	// written at.
	CampaignVersion int
	SegmentVersion  int
	ContentID       int64
	ContentVersion  int
	// ContentProvenance is the origin (ai/edited) and, for AI versions, the
	// model and prompt version of the bound content version.
	ContentProvenance content.Provenance
	Channels          []segment.Channel
	// AudienceStats is the exact partition stored in the approval row,
	// computed in the transaction that wrote the held rows.
	AudienceStats  segment.AudienceStats
	SnapshotCount  int64
	SnapshotDigest string
	PreparedAt     time.Time
	ExpiresAt      time.Time
	ApprovedAt     *time.Time
	ApprovedBy     string
	ClosedAt       *time.Time
	// Snapshot has one entry per selected channel. For a live approval the
	// counts and render issues come from its held rows, verified against
	// the stored count and digest; for a closed approval (held rows deleted)
	// Recipients is the stored eligible count and RenderIssues is nil.
	Snapshot map[segment.Channel]ChannelSnapshot
	// Reviewed reports whether Review was computed: always for a live
	// approval; never for a closed one (its content and brief may no longer
	// be current, and nothing can be decided on it any more).
	Reviewed bool
	// Review is the content re-validation for the selected channels against
	// the current content brief, including offer findings.
	Review campaign.ApprovalReview
	// Superseded is the pending approval this preparation replaced
	// (revoked with reason superseded), if any. A due approval is never
	// superseded: it is expired first (Service.Prepare), which advances the
	// campaign version, so a preparation based on the pre-expiry version is
	// a version conflict.
	Superseded *campaign.ID
}

// RenderIssueCount is the total number of held rows with a render issue.
func (s Summary) RenderIssueCount() int64 {
	var n int64
	for _, cs := range s.Snapshot {
		for _, k := range cs.RenderIssues {
			n += k
		}
	}
	return n
}

// ---- Snapshot digest ----

// The snapshot digest (campaign_approvals.snapshot_digest) is the lower-case
// hex SHA-256 of the snapshot's recipients serialized as lines
// "<user_id>:<channel>", sorted by user_id ascending and then channel in
// canonical order (sms, email — the channel enum order), joined by "\n"
// without a trailing newline. The empty snapshot hashes the empty string.
// segment.ComputeSnapshotAudience computes the same value in PostgreSQL;
// preparation computes it both ways and requires them to agree.

// digester accumulates the digest of recipients added in snapshot order and
// rejects out-of-order or duplicate entries.
type digester struct {
	h          hash.Hash
	n          int64
	lastUser   int64
	lastRank   int
	hasEntries bool
}

func newDigester() *digester { return &digester{h: sha256.New()} }

func channelRank(ch segment.Channel) (int, error) {
	switch ch {
	case segment.ChannelSMS:
		return 0, nil
	case segment.ChannelEmail:
		return 1, nil
	default:
		return 0, fmt.Errorf("%w: %q", segment.ErrUnknownChannel, ch)
	}
}

func (d *digester) add(userID int64, ch segment.Channel) error {
	rank, err := channelRank(ch)
	if err != nil {
		return err
	}
	if d.hasEntries && (userID < d.lastUser || (userID == d.lastUser && rank <= d.lastRank)) {
		return fmt.Errorf("%w: snapshot rows are not strictly ordered by (user_id, channel)", ErrInconsistent)
	}
	if d.hasEntries {
		d.h.Write([]byte{'\n'})
	}
	d.h.Write([]byte(strconv.FormatInt(userID, 10) + ":" + string(ch)))
	d.lastUser, d.lastRank, d.hasEntries = userID, rank, true
	d.n++
	return nil
}

func (d *digester) sum() string { return hex.EncodeToString(d.h.Sum(nil)) }

// Recipient is one (user, channel) entry of a snapshot.
type Recipient struct {
	UserID  int64
	Channel segment.Channel
}

// Digest computes the snapshot digest of recipients, which must be sorted
// by (user_id, channel order) without duplicates.
func Digest(recipients []Recipient) (string, error) {
	d := newDigester()
	for _, r := range recipients {
		if err := d.add(r.UserID, r.Channel); err != nil {
			return "", err
		}
	}
	return d.sum(), nil
}

// ---- Render check of frozen values ----

// renderIssue trial-renders the channel's message for one recipient's frozen
// values and returns the content render code, or "" if it renders. It never
// returns the values or the rendered text.
func renderIssue(c content.Content, ch segment.Channel, v content.Values) (string, error) {
	var err error
	switch ch {
	case segment.ChannelSMS:
		_, err = c.RenderSMS(v)
	case segment.ChannelEmail:
		_, err = c.RenderEmail(v)
	default:
		return "", fmt.Errorf("%w: %q", segment.ErrUnknownChannel, ch)
	}
	if err == nil {
		return "", nil
	}
	var re *content.RenderError
	if errors.As(err, &re) {
		return re.Code, nil
	}
	// ErrChannelAbsent: the reviewed content covers every selected channel,
	// so this is a bug.
	return "", fmt.Errorf("%w: render %s: %w", ErrInconsistent, ch, err)
}
