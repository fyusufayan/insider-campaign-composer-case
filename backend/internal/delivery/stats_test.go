package delivery

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/fyusufayan/insider-campaign-composer-case/backend/internal/provider"
)

const (
	sms   = provider.ChannelSMS
	email = provider.ChannelEmail
)

// consumed builds the header of a campaign with a consumed approval whose
// approved totals are sms/email (a channel < 0 is not approved).
func consumed(status string, smsTotal, emailTotal int64) statsHeader {
	by := map[string]any{}
	chs := []string{}
	if smsTotal >= 0 {
		by["sms"] = map[string]any{"eligible": smsTotal, "no_consent": 3, "no_contact": 1}
		chs = append(chs, "sms")
	}
	if emailTotal >= 0 {
		by["email"] = map[string]any{"eligible": emailTotal, "no_consent": 0, "no_contact": 0}
		chs = append(chs, "email")
	}
	raw, _ := json.Marshal(map[string]any{"matched_users": 99, "by_channel": by})
	sv, cv, cid := 2, 3, int64(7)
	return statsHeader{status: status, version: 12, approvalID: sp("a-1"), segmentVersion: &sv, contentID: &cid, contentVersion: &cv,
		audienceStats: raw, channels: chs, now: time.Unix(1_700_000_000, 0).UTC()}
}

func TestAssembleReconcilesPartitionPerChannel(t *testing.T) {
	rows := statsRows{
		header: consumed("sending", 10, 5),
		statuses: []statusRow{
			{channel: sms, status: StatusSent, rows: 4, attempted: 4, retried: 1},
			{channel: sms, status: StatusPending, rows: 2},
			{channel: sms, status: StatusInFlight, rows: 1, attempted: 0},
			{channel: sms, status: StatusRetryScheduled, rows: 1, attempted: 1, retried: 1},
			{channel: sms, status: StatusFailed, rows: 1, attempted: 1, retried: 1, uncertain: 1},
			{channel: sms, status: StatusSkippedConsentRevoked, rows: 1},
			{channel: email, status: StatusSent, rows: 3, attempted: 3},
			{channel: email, status: StatusSkippedNoContact, rows: 1},
			{channel: email, status: StatusCancelled, rows: 1, attempted: 1, uncertain: 1},
		},
		attempts: []attemptRow{
			// sms: 4 sent deliveries took 5 attempts (one 429 then accepted),
			// the retry row 2 (5xx twice), the failed row 5 (timeouts).
			{channel: sms, outcome: "accepted", total: 4, recent: 2},
			{channel: sms, outcome: "rate_limited", total: 1, recent: 1},
			{channel: sms, outcome: "transient_error", total: 2},
			{channel: sms, outcome: "timeout", total: 5},
			{channel: email, outcome: "accepted", total: 2, recent: 1},
			{channel: email, outcome: "replayed", total: 1},
			{channel: email, outcome: "unknown", total: 1},
		},
		reasons: []reasonRow{
			{channel: sms, status: StatusFailed, code: "timeout", count: 1},
			{channel: sms, status: StatusRetryScheduled, code: "http_503", count: 1},
			{channel: sms, status: StatusSkippedConsentRevoked, code: "no_consent", count: 1},
			{channel: email, status: StatusSkippedNoContact, code: "no_contact", count: 1},
			{channel: email, status: StatusCancelled, code: "campaign_cancelled", count: 1},
		},
		worker: ClassifyHeartbeat(durPtr(2*time.Second), 1),
	}
	st, err := assemble("c-1", rows)
	if err != nil {
		t.Fatal(err)
	}
	s, e := st.Channels[sms], st.Channels[email]
	wantSMS := Partition{ApprovedTotal: 10, Pending: 2, InFlight: 1, RetryScheduled: 1, Sent: 4, Failed: 1, SkippedConsentRevoked: 1, Uncertain: 1}
	if s.Partition != wantSMS {
		t.Errorf("sms = %+v", s.Partition)
	}
	wantEmail := Partition{ApprovedTotal: 5, Sent: 3, SkippedNoContact: 1, Cancelled: 1, Uncertain: 1}
	if e.Partition != wantEmail {
		t.Errorf("email = %+v", e.Partition)
	}
	for ch, cs := range st.Channels {
		if cs.sum() != cs.ApprovedTotal {
			t.Errorf("%s partition %d ≠ %d", ch, cs.sum(), cs.ApprovedTotal)
		}
	}
	// Multiple attempts do not inflate delivery counts: 12 sms attempts, 4 sent.
	if s.Attempts.Total != 12 || s.Sent != 4 || s.Attempts.ByOutcome["accepted"] != 4 || s.Attempts.ByOutcome["timeout"] != 5 {
		t.Errorf("sms attempts = %+v", s.Attempts)
	}
	// Retries = attempts − attempted deliveries = 12 − 6.
	if s.Attempts.Retries != 6 || s.Attempts.RetriedDeliveries != 3 {
		t.Errorf("sms retries = %+v", s.Attempts)
	}
	if e.Attempts.Total != 4 || e.Attempts.Retries != 0 || e.Attempts.ByOutcome["replayed"] != 1 {
		t.Errorf("email attempts = %+v", e.Attempts)
	}
	if s.Attempts.PerSecond != 0.3 || st.Attempts.PerSecond != 0.4 || st.Attempts.Recent != 4 {
		t.Errorf("rate sms=%v total=%v", s.Attempts.PerSecond, st.Attempts.PerSecond)
	}
	for _, o := range AttemptOutcomes { // fixed keys, zero-filled
		if _, ok := e.Attempts.ByOutcome[string(o)]; !ok {
			t.Errorf("email by_outcome lacks %s", o)
		}
	}
	if st.Totals.ApprovedTotal != 15 || st.Totals.Sent != 7 || st.Totals.Uncertain != 2 || st.Attempts.Total != 16 {
		t.Errorf("totals = %+v attempts %+v", st.Totals, st.Attempts)
	}
	if st.Processed != 11 || st.Remaining != 4 || st.Processed+st.Remaining != st.Totals.ApprovedTotal {
		t.Errorf("processed %d remaining %d", st.Processed, st.Remaining)
	}
	if fmt.Sprintf("%.4f", st.Progress) != "0.7333" || st.Terminal || st.CampaignTerminal || st.Outcome != OutcomeSending {
		t.Errorf("progress %v terminal %v outcome %s", st.Progress, st.Terminal, st.Outcome)
	}
	if len(s.Reasons) != 3 || len(e.Reasons) != 2 {
		t.Errorf("reasons sms=%v email=%v", s.Reasons, e.Reasons)
	}
	if *st.ApprovalID != "a-1" || *st.ContentVersion != 3 || *st.SegmentVersion != 2 || st.CampaignVersion != 12 {
		t.Errorf("identity = %+v", st)
	}
	if st.Worker.Status != WorkerFresh {
		t.Errorf("worker = %+v", st.Worker)
	}
}

func durPtr(d time.Duration) *time.Duration { return &d }

func TestAssembleUncertainIsAnOverlay(t *testing.T) {
	rows := statsRows{header: consumed("completed_with_failures", 3, -1), statuses: []statusRow{
		{channel: sms, status: StatusSent, rows: 1, attempted: 1},
		{channel: sms, status: StatusFailed, rows: 2, attempted: 2, uncertain: 2},
	}, attempts: []attemptRow{{channel: sms, outcome: "accepted", total: 1}, {channel: sms, outcome: "unknown", total: 2}}}
	st, err := assemble("c", rows)
	if err != nil {
		t.Fatal(err)
	}
	if st.Totals.Uncertain != 2 || st.Totals.sum() != 3 || st.Totals.ApprovedTotal != 3 {
		t.Errorf("totals = %+v", st.Totals)
	}
	if !st.Terminal || st.Outcome != OutcomeCompletedWithFailures || st.Progress != 1 {
		t.Errorf("terminal=%v outcome=%s progress=%v", st.Terminal, st.Outcome, st.Progress)
	}
	if _, ok := st.Channels[email]; ok {
		t.Error("unapproved channel reported")
	}
}

func TestAssembleCancelledSettlesBeforeTerminal(t *testing.T) {
	rows := statsRows{header: consumed("cancelled", 4, -1), statuses: []statusRow{
		{channel: sms, status: StatusSent, rows: 1, attempted: 1},
		{channel: sms, status: StatusInFlight, rows: 1},
		{channel: sms, status: StatusCancelled, rows: 2},
	}, attempts: []attemptRow{{channel: sms, outcome: "accepted", total: 1}}}
	st, err := assemble("c", rows)
	if err != nil {
		t.Fatal(err)
	}
	if !st.CampaignTerminal || st.Terminal || st.Outcome != OutcomeSettling {
		t.Errorf("settling: %+v", st)
	}
	rows.statuses[1] = statusRow{channel: sms, status: StatusSent, rows: 1, attempted: 1}
	rows.attempts[0].total = 2
	st, err = assemble("c", rows)
	if err != nil {
		t.Fatal(err)
	}
	if !st.Terminal || st.Outcome != OutcomeCancelled || st.Totals.Sent != 2 || st.Totals.Cancelled != 2 {
		t.Errorf("settled: %+v", st)
	}
}

func TestAssembleBeforeSendAndEmpty(t *testing.T) {
	for _, status := range []string{"draft", "approved", "cancelled"} {
		st, err := assemble("c", statsRows{header: statsHeader{status: status, version: 3}, worker: ClassifyHeartbeat(nil, 0)})
		if err != nil {
			t.Fatal(err)
		}
		if st.ApprovalID != nil || len(st.Channels) != 0 || st.Totals.ApprovedTotal != 0 || st.Progress != 0 {
			t.Errorf("%s: %+v", status, st)
		}
		wantTerminal := status == "cancelled"
		if st.Terminal != wantTerminal || st.CampaignTerminal != wantTerminal {
			t.Errorf("%s: terminal %v", status, st.Terminal)
		}
		wantOutcome := OutcomeNotStarted
		if wantTerminal {
			wantOutcome = OutcomeCancelled
		}
		if st.Outcome != wantOutcome || st.Worker.Status != WorkerAbsent || st.Worker.AgeSeconds != nil {
			t.Errorf("%s: outcome %s worker %+v", status, st.Outcome, st.Worker)
		}
		raw, _ := json.Marshal(st)
		if !strings.Contains(string(raw), `"channels":{}`) {
			t.Errorf("%s: channels not an empty object: %s", status, raw)
		}
	}
}

func TestAssembleRejectsInconsistentRows(t *testing.T) {
	cases := map[string]statsRows{
		"fewer rows than approved": {header: consumed("sending", 3, -1), statuses: []statusRow{{channel: sms, status: StatusPending, rows: 2}}},
		"more rows than approved":  {header: consumed("sending", 1, -1), statuses: []statusRow{{channel: sms, status: StatusPending, rows: 2}}},
		"held row after consume":   {header: consumed("sending", 1, -1), statuses: []statusRow{{channel: sms, status: StatusHeld, rows: 1}}},
		"unapproved channel":       {header: consumed("sending", 1, -1), statuses: []statusRow{{channel: sms, status: StatusPending, rows: 1}, {channel: email, status: StatusPending, rows: 1}}},
		"unknown outcome": {header: consumed("sending", 1, -1), statuses: []statusRow{{channel: sms, status: StatusSent, rows: 1, attempted: 1}},
			attempts: []attemptRow{{channel: sms, outcome: "bogus", total: 1}}},
		"attempts below attempted": {header: consumed("sending", 1, -1), statuses: []statusRow{{channel: sms, status: StatusSent, rows: 1, attempted: 1}}},
	}
	for name, rows := range cases {
		if _, err := assemble("c", rows); !errors.Is(err, ErrPartitionMismatch) {
			t.Errorf("%s: err = %v", name, err)
		}
	}
	bad := consumed("sending", 1, -1)
	bad.audienceStats = []byte(`{"by_channel":{}}`)
	if _, err := assemble("c", statsRows{header: bad}); !errors.Is(err, ErrPartitionMismatch) {
		t.Errorf("missing approved total: %v", err)
	}
}

func TestReasonsSanitizedAndBounded(t *testing.T) {
	for in, want := range map[string]string{
		"": "unspecified", "http_500": "http_500", "no_consent": "no_consent", "Bad Code": "other",
		"+905551234567": "other", "a@b.c": "other", strings.Repeat("a", 65): "other",
	} {
		if got := SafeCode(in); got != want {
			t.Errorf("SafeCode(%q) = %q, want %q", in, got, want)
		}
	}
	var in []Reason
	var total int64
	for i := 0; i < 30; i++ {
		in = append(in, Reason{Status: StatusFailed, Code: fmt.Sprintf("code_%02d", i), Count: int64(100 - i)})
		total += int64(100 - i)
	}
	in = append(in, Reason{Status: StatusCancelled, Code: "other", Count: 1}, Reason{Status: StatusCancelled, Code: "other", Count: 2})
	total += 3
	out := foldReasons(in)
	if len(out) > MaxReasonsPerChannel {
		t.Fatalf("len = %d", len(out))
	}
	var got int64
	kept := false
	for _, r := range out {
		got += r.Count
		kept = kept || r.Code == "code_00"
	}
	if got != total || !kept {
		t.Errorf("fold lost counts: %d ≠ %d (%v)", got, total, out)
	}
}

func TestClassifyHeartbeat(t *testing.T) {
	cases := []struct {
		age  *time.Duration
		want string
	}{
		{nil, WorkerAbsent},
		{durPtr(0), WorkerFresh},
		{durPtr(-time.Second), WorkerFresh},
		{durPtr(HeartbeatStaleAfter), WorkerFresh},
		{durPtr(HeartbeatStaleAfter + time.Millisecond), WorkerStale},
		{durPtr(time.Hour), WorkerStale},
	}
	for _, c := range cases {
		h := ClassifyHeartbeat(c.age, 1)
		if h.Status != c.want || h.StaleAfterSeconds != 15 {
			t.Errorf("age %v → %+v", c.age, h)
		}
		if c.age != nil && (h.AgeSeconds == nil || *h.AgeSeconds < 0) {
			t.Errorf("age %v → seconds %v", c.age, h.AgeSeconds)
		}
	}
	if HeartbeatStaleAfter != 15*time.Second {
		t.Errorf("documented threshold changed: %v", HeartbeatStaleAfter)
	}
}

func TestMaskDestination(t *testing.T) {
	if got := MaskDestination(sms, "+905551234567"); got != "+90********67" {
		t.Errorf("sms = %q", got)
	}
	if got := MaskDestination(email, "ayse.yilmaz@example.test"); got != "a***@example.test" {
		t.Errorf("email = %q", got)
	}
}

func TestListFiltersValidation(t *testing.T) {
	for _, s := range PartitionStatuses {
		if !ValidStatus(string(s)) {
			t.Errorf("%s rejected", s)
		}
	}
	for _, s := range []string{"held", "", "SENT", "sent "} {
		if ValidStatus(s) {
			t.Errorf("%q accepted", s)
		}
	}
	if !ValidChannel("sms") || !ValidChannel("email") || ValidChannel("push") {
		t.Error("channel validation")
	}
}
