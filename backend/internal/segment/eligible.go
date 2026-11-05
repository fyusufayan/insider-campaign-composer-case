package segment

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// ErrNoChannels is returned by CompileEligible for an empty channel
// selection: there is no recipient set to freeze.
var ErrNoChannels = errors.New("segment: no channel selected")

// channelsMarker in sqlEligibleCTE is replaced by the channels placeholder
// ("$n"), so the statement text stays constant apart from placeholder numbers.
const channelsMarker = "%CHANNELS%"

// sqlEligibleCTE follows sqlClassifiedCTE. It defines eligible(user_id,
// channel): one row per matched user and selected channel whose partition
// cell is eligible (code 2: contact present and an opted_in consent row,
// ARCHITECTURE §6.5). It is derived from classified, so the frozen
// recipients are by construction the eligible cell of the partition.
//
// Uniqueness: classified has one row per matched user (matched selects the
// primary key; each LEFT JOIN matches at most one consent row by PRIMARY KEY
// (user_id, channel)), and the selected channels are de-duplicated before
// they are bound, so (user_id, channel) is unique in eligible.
const sqlEligibleCTE = `,
eligible AS (
  SELECT c.user_id, ch.channel
  FROM classified AS c
  CROSS JOIN unnest(%CHANNELS%::text[]::channel[]) AS ch(channel)
  WHERE (CASE ch.channel WHEN 'sms' THEN c.sms WHEN 'email' THEN c.email END) = 2
)
`

// sqlSnapshotDigest is the snapshot digest over eligible: lower-case hex
// SHA-256 of the lines "<user_id>:<channel>" ordered by user_id, then
// channel in enum order (sms, email), joined by "\n" without a trailing
// newline (the empty set hashes ""). approval.Digest computes the same value
// in Go.
const sqlSnapshotDigest = `encode(sha256(convert_to(COALESCE(
  string_agg(user_id::text || ':' || channel::text, E'\n' ORDER BY user_id, channel), ''), 'UTF8')), 'hex')`

// sqlSnapshotAudienceTail follows EligibleQuery.With: the partition cells
// and, in the same statement (one snapshot), the eligible counts and digest.
const sqlSnapshotAudienceTail = sqlAudienceCells + `,
  (SELECT count(*) FROM eligible WHERE channel = 'sms'),
  (SELECT count(*) FROM eligible WHERE channel = 'email'),
  (SELECT ` + sqlSnapshotDigest + ` FROM eligible)
FROM classified`

// EligibleQuery is a WITH clause defining the CTEs matched(id),
// classified(user_id, sms, email) and eligible(user_id, channel) for a rule
// and a channel selection. The caller appends one statement that reads
// eligible (SELECT, or INSERT … SELECT) and binds its own values starting
// at placeholder NextParam.
type EligibleQuery struct {
	// With starts with "WITH matched AS (" and ends after the eligible CTE.
	With string
	// Args are the bind values of With, in placeholder order ($1..$n).
	Args []any
	// NextParam is n+1, the first placeholder number free for the caller.
	NextParam int
}

// CompileEligible builds the eligible-recipient query of rule for the
// selected channels. Like Compile, every byte of SQL text is a constant of
// this package; the rule's values and the channels are bind parameters. It
// is the single definition of the recipients of a frozen audience
// (approval preparation).
func CompileEligible(rule Validated, channels []Channel) (EligibleQuery, error) {
	matchedSQL, args, err := Compile(rule)
	if err != nil {
		return EligibleQuery{}, err
	}
	sel, err := NormalizeChannels(channels)
	if err != nil {
		return EligibleQuery{}, err
	}
	if len(sel) == 0 {
		return EligibleQuery{}, ErrNoChannels
	}
	names := make([]string, len(sel))
	for i, ch := range sel {
		names[i] = string(ch)
	}
	args = append(args, names)
	placeholder := "$" + strconv.Itoa(len(args))
	return EligibleQuery{
		With:      sqlAudienceStatsHead + matchedSQL + sqlClassifiedCTE + strings.Replace(sqlEligibleCTE, channelsMarker, placeholder, 1),
		Args:      args,
		NextParam: len(args) + 1,
	}, nil
}

// EligibleSnapshot summarizes the eligible recipients of a selection.
type EligibleSnapshot struct {
	SMS, Email int64
	// Digest is the snapshot digest (see sqlSnapshotDigest).
	Digest string
}

// ComputeSnapshotAudience computes, in ONE statement and therefore from one
// database snapshot, the audience partition of rule for the selected
// channels and the per-channel counts and digest of its eligible recipients.
// The partition shown to the marketer and the recipient set an approval
// freezes thus describe the same instant; the caller verifies that the rows
// it inserts afterwards (another statement) have the same digest.
func ComputeSnapshotAudience(ctx context.Context, q Querier, rule Validated, channels []Channel) (AudienceStats, EligibleSnapshot, error) {
	eq, err := CompileEligible(rule, channels)
	if err != nil {
		return AudienceStats{}, EligibleSnapshot{}, err
	}
	sel, _ := NormalizeChannels(channels) // validated by CompileEligible
	var (
		c    audienceCells
		snap EligibleSnapshot
	)
	err = q.QueryRow(ctx, eq.With+sqlSnapshotAudienceTail, eq.Args...).Scan(
		&c.matched,
		&c.smsNoContact, &c.smsNoConsent, &c.smsElig,
		&c.emailNoContact, &c.emailNoConsent, &c.emElig,
		&c.bothElig, &c.smsOnlyElig, &c.emailOnlyElig, &c.neitherElig,
		&c.computedAt,
		&snap.SMS, &snap.Email, &snap.Digest,
	)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil && !errors.Is(err, ctxErr) {
			err = fmt.Errorf("%w (%w)", ctxErr, err)
		}
		return AudienceStats{}, EligibleSnapshot{}, fmt.Errorf("segment: snapshot audience query: %w", err)
	}
	c.computedAt = c.computedAt.UTC()
	stats, err := c.project(sel)
	if err != nil {
		return AudienceStats{}, EligibleSnapshot{}, err
	}
	// Same statement, same snapshot: a mismatch is a bug, never a race.
	if snap.SMS != stats.ByChannel[ChannelSMS].Eligible || snap.Email != stats.ByChannel[ChannelEmail].Eligible {
		return AudienceStats{}, EligibleSnapshot{}, fmt.Errorf("%w: eligible recipients (sms %d, email %d) differ from the partition",
			ErrPartitionViolation, snap.SMS, snap.Email)
	}
	return stats, snap, nil
}
