package segment

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"
	"time"
)

func TestNormalizeChannels(t *testing.T) {
	tests := []struct {
		in   []Channel
		want []Channel
	}{
		{nil, []Channel{}},
		{[]Channel{}, []Channel{}},
		{[]Channel{ChannelEmail}, []Channel{ChannelEmail}},
		{[]Channel{ChannelEmail, ChannelSMS, ChannelEmail}, []Channel{ChannelSMS, ChannelEmail}},
	}
	for _, tc := range tests {
		got, err := NormalizeChannels(tc.in)
		if err != nil || !reflect.DeepEqual(got, tc.want) {
			t.Errorf("NormalizeChannels(%v) = %v, %v; want %v", tc.in, got, err, tc.want)
		}
	}
	for _, bad := range [][]Channel{{"push"}, {ChannelSMS, "SMS"}, {""}} {
		if _, err := NormalizeChannels(bad); !errors.Is(err, ErrUnknownChannel) {
			t.Errorf("NormalizeChannels(%v) err = %v, want ErrUnknownChannel", bad, err)
		}
	}
}

// cellsFixture: 10 matched users.
//
//	sms:   eligible 5, no_consent 3, no_contact 2
//	email: eligible 4, no_consent 4, no_contact 2
//	eligible on both 3, sms only 2, email only 1, neither 4
func cellsFixture() audienceCells {
	return audienceCells{
		matched:      10,
		smsNoContact: 2, smsNoConsent: 3, smsElig: 5,
		emailNoContact: 2, emailNoConsent: 4, emElig: 4,
		bothElig: 3, smsOnlyElig: 2, emailOnlyElig: 1, neitherElig: 4,
		computedAt: time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC),
	}
}

func TestProjectBySelection(t *testing.T) {
	sms := ChannelStats{Eligible: 5, NoConsent: 3, NoContact: 2}
	email := ChannelStats{Eligible: 4, NoConsent: 4, NoContact: 2}
	tests := []struct {
		name     string
		channels []Channel
		by       map[Channel]ChannelStats
		reach    ReachStats
		messages int64
	}{
		{"both", []Channel{ChannelSMS, ChannelEmail}, map[Channel]ChannelStats{ChannelSMS: sms, ChannelEmail: email},
			ReachStats{Both: 3, SMSOnly: 2, EmailOnly: 1, Unreachable: 4}, 9},
		{"sms", []Channel{ChannelSMS}, map[Channel]ChannelStats{ChannelSMS: sms},
			ReachStats{SMSOnly: 5, Unreachable: 5}, 5},
		{"email", []Channel{ChannelEmail}, map[Channel]ChannelStats{ChannelEmail: email},
			ReachStats{EmailOnly: 4, Unreachable: 6}, 4},
		{"none", []Channel{}, map[Channel]ChannelStats{},
			ReachStats{Unreachable: 10}, 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s, err := cellsFixture().project(tc.channels)
			if err != nil {
				t.Fatal(err)
			}
			if s.MatchedUsers != 10 || !reflect.DeepEqual(s.ByChannel, tc.by) || s.ReachUsers != tc.reach || s.Messages != tc.messages {
				t.Errorf("got %+v", s)
			}
			if err := s.Check(); err != nil {
				t.Error(err)
			}
		})
	}
}

func TestProjectEmptyAudience(t *testing.T) {
	s, err := audienceCells{}.project([]Channel{ChannelSMS, ChannelEmail})
	if err != nil {
		t.Fatal(err)
	}
	if s.MatchedUsers != 0 || s.Messages != 0 || s.ReachUsers != (ReachStats{}) || len(s.ByChannel) != 2 {
		t.Errorf("got %+v", s)
	}
}

func TestProjectRejectsInconsistentCells(t *testing.T) {
	mutations := map[string]func(*audienceCells){
		"sms partition short":   func(c *audienceCells) { c.smsNoConsent-- },
		"email partition long":  func(c *audienceCells) { c.emailNoContact++ },
		"cross tab short":       func(c *audienceCells) { c.neitherElig-- },
		"cross tab vs sms":      func(c *audienceCells) { c.bothElig++; c.neitherElig-- },
		"cross tab vs email":    func(c *audienceCells) { c.emailOnlyElig++; c.neitherElig-- },
		"matched off by one":    func(c *audienceCells) { c.matched++ },
		"duplicate both sides":  func(c *audienceCells) { c.smsElig++; c.smsNoContact-- },
		"sms-only from neither": func(c *audienceCells) { c.smsOnlyElig++; c.neitherElig-- },
	}
	for name, mut := range mutations {
		for _, sel := range [][]Channel{{ChannelSMS, ChannelEmail}, {ChannelSMS}, {ChannelEmail}, {}} {
			c := cellsFixture()
			mut(&c)
			if _, err := c.project(sel); !errors.Is(err, ErrPartitionViolation) {
				t.Errorf("%s %v: err = %v, want ErrPartitionViolation", name, sel, err)
			}
		}
	}
}

func TestCheckIdentities(t *testing.T) {
	valid := func() AudienceStats {
		return AudienceStats{
			MatchedUsers: 10,
			ByChannel: map[Channel]ChannelStats{
				ChannelSMS:   {Eligible: 5, NoConsent: 3, NoContact: 2},
				ChannelEmail: {Eligible: 4, NoConsent: 4, NoContact: 2},
			},
			ReachUsers: ReachStats{Both: 3, SMSOnly: 2, EmailOnly: 1, Unreachable: 4},
			Messages:   9,
		}
	}
	if err := valid().Check(); err != nil {
		t.Fatal(err)
	}
	bad := map[string]func(*AudienceStats){
		"channel sum": func(s *AudienceStats) {
			s.ByChannel[ChannelSMS] = ChannelStats{Eligible: 5, NoConsent: 3, NoContact: 3}
		},
		"reach sum":              func(s *AudienceStats) { s.ReachUsers.Unreachable = 5 },
		"messages":               func(s *AudienceStats) { s.Messages = 19 },
		"users added to message": func(s *AudienceStats) { s.Messages = s.MatchedUsers },
		"reach vs eligible sms": func(s *AudienceStats) {
			s.ReachUsers = ReachStats{Both: 2, SMSOnly: 2, EmailOnly: 2, Unreachable: 4}
		},
		"reach on unselected channel": func(s *AudienceStats) {
			delete(s.ByChannel, ChannelEmail)
			s.Messages = 5
		},
		"unknown channel": func(s *AudienceStats) { s.ByChannel["push"] = ChannelStats{Eligible: 10} },
		"negative": func(s *AudienceStats) {
			s.ByChannel[ChannelSMS] = ChannelStats{Eligible: 6, NoConsent: 5, NoContact: -1}
			s.ReachUsers = ReachStats{Both: 3, SMSOnly: 3, EmailOnly: 1, Unreachable: 3}
			s.Messages = 10
		},
	}
	for name, mut := range bad {
		s := valid()
		mut(&s)
		if err := s.Check(); !errors.Is(err, ErrPartitionViolation) {
			t.Errorf("%s: err = %v, want ErrPartitionViolation", name, err)
		}
	}
}

func TestAudienceStatsJSONShape(t *testing.T) {
	s, err := cellsFixture().project([]Channel{ChannelSMS, ChannelEmail})
	if err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"matched_users":10,"by_channel":{"email":{"eligible":4,"no_consent":4,"no_contact":2},"sms":{"eligible":5,"no_consent":3,"no_contact":2}},` +
		`"reach_users":{"both":3,"sms_only":2,"email_only":1,"unreachable":4},"messages":9,"computed_at":"2026-11-04T12:00:00Z"}`
	if string(b) != want {
		t.Errorf("JSON\n got: %s\nwant: %s", b, want)
	}
	none, err := cellsFixture().project(nil)
	if err != nil {
		t.Fatal(err)
	}
	b, _ = json.Marshal(none)
	wantNone := `{"matched_users":10,"by_channel":{},"reach_users":{"both":0,"sms_only":0,"email_only":0,"unreachable":10},"messages":0,"computed_at":"2026-11-04T12:00:00Z"}`
	if string(b) != wantNone {
		t.Errorf("JSON (no channels)\n got: %s\nwant: %s", b, wantNone)
	}
}

func TestCleanupContextIsBoundedAndIgnoresParentCancellation(t *testing.T) {
	type key struct{}
	parent, cancelParent := context.WithCancel(context.WithValue(context.Background(), key{}, "v"))
	cancelParent()

	before := time.Now()
	ctx, cancel := cleanupContext(parent)
	after := time.Now()
	if err := ctx.Err(); err != nil {
		t.Fatalf("cleanup context inherits parent cancellation: %v", err)
	}
	deadline, ok := ctx.Deadline()
	if !ok {
		t.Fatal("cleanup context has no deadline")
	}
	if deadline.Before(before.Add(rollbackTimeout)) || deadline.After(after.Add(rollbackTimeout)) {
		t.Errorf("deadline %v not %v after creation", deadline, rollbackTimeout)
	}
	if ctx.Value(key{}) != "v" {
		t.Error("cleanup context lost the parent's values")
	}
	cancel()
	select {
	case <-ctx.Done():
	default:
		t.Fatal("cleanup context not done after cancel")
	}
	if !errors.Is(ctx.Err(), context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", ctx.Err())
	}
}
