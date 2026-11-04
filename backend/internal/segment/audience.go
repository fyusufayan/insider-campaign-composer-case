package segment

import (
	"errors"
	"fmt"
	"slices"
	"time"
)

// Channel is a delivery channel (the PostgreSQL enum "channel").
type Channel string

// Channels.
const (
	ChannelSMS   Channel = "sms"
	ChannelEmail Channel = "email"
)

// allChannels is the fixed channel order used for statistics and output.
var allChannels = []Channel{ChannelSMS, ChannelEmail}

// ErrUnknownChannel is returned for a channel outside allChannels.
var ErrUnknownChannel = errors.New("segment: unknown channel")

// ErrPartitionViolation is returned when computed statistics do not satisfy
// the partition identities of ARCHITECTURE §6.5. It indicates a bug and is
// never returned together with statistics.
var ErrPartitionViolation = errors.New("segment: audience partition identities violated")

// NormalizeChannels validates the selection and returns it de-duplicated in
// the fixed order (sms, email). An empty selection is valid.
func NormalizeChannels(chs []Channel) ([]Channel, error) {
	out := make([]Channel, 0, len(allChannels))
	for _, c := range chs {
		if !slices.Contains(allChannels, c) {
			return nil, fmt.Errorf("%w: %q", ErrUnknownChannel, c)
		}
	}
	for _, c := range allChannels {
		if slices.Contains(chs, c) {
			out = append(out, c)
		}
	}
	return out, nil
}

// ChannelStats is the exclusive per-channel partition of the matched users,
// evaluated in order: no_contact → no_consent → eligible. Units: users.
type ChannelStats struct {
	Eligible  int64 `json:"eligible"`
	NoConsent int64 `json:"no_consent"`
	NoContact int64 `json:"no_contact"`
}

// Total is eligible + no_consent + no_contact.
func (s ChannelStats) Total() int64 { return s.Eligible + s.NoConsent + s.NoContact }

// ReachStats partitions the matched users by the selected channels on which
// they are eligible. Units: users.
type ReachStats struct {
	Both        int64 `json:"both"`
	SMSOnly     int64 `json:"sms_only"`
	EmailOnly   int64 `json:"email_only"`
	Unreachable int64 `json:"unreachable"`
}

// Total is both + sms_only + email_only + unreachable.
func (r ReachStats) Total() int64 { return r.Both + r.SMSOnly + r.EmailOnly + r.Unreachable }

// AudienceStats is the audience partition of ARCHITECTURE §6.5 for a rule and
// a channel selection. MatchedUsers and ReachUsers count people; Messages
// counts messages (Σ eligible over the selected channels). The two units are
// never added together.
type AudienceStats struct {
	MatchedUsers int64 `json:"matched_users"`
	// ByChannel holds exactly the selected channels (empty, not nil, when none).
	ByChannel  map[Channel]ChannelStats `json:"by_channel"`
	ReachUsers ReachStats               `json:"reach_users"`
	Messages   int64                    `json:"messages"`
	ComputedAt time.Time                `json:"computed_at"`
}

// Check verifies the partition identities:
//   - per selected channel: matched_users = eligible + no_consent + no_contact;
//   - matched_users = both + sms_only + email_only + unreachable;
//   - messages = Σ eligible over the selected channels;
//   - reach is consistent with the selection (a channel that is not selected
//     contributes no reach and no messages) and with eligibility
//     (both + sms_only = eligible sms, both + email_only = eligible email);
//   - no negative counts.
func (s AudienceStats) Check() error {
	fail := func(format string, a ...any) error {
		return fmt.Errorf("%w: "+format, append([]any{ErrPartitionViolation}, a...)...)
	}
	if s.MatchedUsers < 0 || s.Messages < 0 ||
		s.ReachUsers.Both < 0 || s.ReachUsers.SMSOnly < 0 || s.ReachUsers.EmailOnly < 0 || s.ReachUsers.Unreachable < 0 {
		return fail("negative count")
	}
	var messages int64
	for ch, cs := range s.ByChannel {
		if !slices.Contains(allChannels, ch) {
			return fail("unknown channel %q", ch)
		}
		if cs.Eligible < 0 || cs.NoConsent < 0 || cs.NoContact < 0 {
			return fail("negative count for %s", ch)
		}
		if cs.Total() != s.MatchedUsers {
			return fail("%s: eligible+no_consent+no_contact = %d, matched_users = %d", ch, cs.Total(), s.MatchedUsers)
		}
		messages += cs.Eligible
	}
	if s.ReachUsers.Total() != s.MatchedUsers {
		return fail("reach sums to %d, matched_users = %d", s.ReachUsers.Total(), s.MatchedUsers)
	}
	if messages != s.Messages {
		return fail("messages = %d, Σ eligible = %d", s.Messages, messages)
	}
	sms, hasSMS := s.ByChannel[ChannelSMS]
	email, hasEmail := s.ByChannel[ChannelEmail]
	if s.ReachUsers.Both+s.ReachUsers.SMSOnly != sms.Eligible { // zero when not selected
		return fail("both+sms_only = %d, eligible sms = %d (selected=%t)",
			s.ReachUsers.Both+s.ReachUsers.SMSOnly, sms.Eligible, hasSMS)
	}
	if s.ReachUsers.Both+s.ReachUsers.EmailOnly != email.Eligible {
		return fail("both+email_only = %d, eligible email = %d (selected=%t)",
			s.ReachUsers.Both+s.ReachUsers.EmailOnly, email.Eligible, hasEmail)
	}
	return nil
}

// audienceCells are the raw counts of the audience query: the per-channel
// partition for both channels and the 2×2 eligibility cross-tabulation.
type audienceCells struct {
	matched                                int64
	smsNoContact, smsNoConsent, smsElig    int64
	emailNoContact, emailNoConsent, emElig int64
	// eligible on: both channels / sms not email / email not sms / neither.
	bothElig, smsOnlyElig, emailOnlyElig, neitherElig int64
	computedAt                                        time.Time
}

// project builds the statistics for the selected channels (already
// normalized) from the raw cells and verifies the identities.
func (c audienceCells) project(channels []Channel) (AudienceStats, error) {
	s := AudienceStats{
		MatchedUsers: c.matched,
		ByChannel:    make(map[Channel]ChannelStats, len(channels)),
		ComputedAt:   c.computedAt,
	}
	sel := func(ch Channel) bool { return slices.Contains(channels, ch) }
	if sel(ChannelSMS) {
		s.ByChannel[ChannelSMS] = ChannelStats{Eligible: c.smsElig, NoConsent: c.smsNoConsent, NoContact: c.smsNoContact}
	}
	if sel(ChannelEmail) {
		s.ByChannel[ChannelEmail] = ChannelStats{Eligible: c.emElig, NoConsent: c.emailNoConsent, NoContact: c.emailNoContact}
	}
	switch {
	case sel(ChannelSMS) && sel(ChannelEmail):
		s.ReachUsers = ReachStats{Both: c.bothElig, SMSOnly: c.smsOnlyElig, EmailOnly: c.emailOnlyElig, Unreachable: c.neitherElig}
	case sel(ChannelSMS):
		// Eligible on sms ⇒ sms_only; everyone else is unreachable on the selection.
		s.ReachUsers = ReachStats{SMSOnly: c.smsElig, Unreachable: c.matched - c.smsElig}
	case sel(ChannelEmail):
		s.ReachUsers = ReachStats{EmailOnly: c.emElig, Unreachable: c.matched - c.emElig}
	default:
		// No channel selected: nobody is reachable and no message would be sent.
		s.ReachUsers = ReachStats{Unreachable: c.matched}
	}
	for _, cs := range s.ByChannel {
		s.Messages += cs.Eligible
	}
	// The raw cells must be consistent with each other before any projection
	// is trusted (the per-channel totals and the cross-tabulation are
	// computed independently by the query).
	if c.smsNoContact+c.smsNoConsent+c.smsElig != c.matched ||
		c.emailNoContact+c.emailNoConsent+c.emElig != c.matched ||
		c.bothElig+c.smsOnlyElig+c.emailOnlyElig+c.neitherElig != c.matched ||
		c.bothElig+c.smsOnlyElig != c.smsElig || c.bothElig+c.emailOnlyElig != c.emElig {
		return AudienceStats{}, fmt.Errorf("%w: inconsistent query cells", ErrPartitionViolation)
	}
	if err := s.Check(); err != nil {
		return AudienceStats{}, err
	}
	return s, nil
}
