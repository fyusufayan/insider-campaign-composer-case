package approval

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/fyusufayan/insider-campaign-composer-case/backend/internal/campaign"
	"github.com/fyusufayan/insider-campaign-composer-case/backend/internal/platform/config"
	"github.com/fyusufayan/insider-campaign-composer-case/backend/internal/segment"
)

func TestDecisionConfig(t *testing.T) {
	// Zero optional fields take the documented defaults (§16).
	c := Config{PendingTTL: DefaultPendingTTL}.withDefaults()
	if c.ApprovedTTL != 24*time.Hour || c.ApprovedBy != "marketer@demo" {
		t.Errorf("defaults = %+v", c)
	}
	for _, ttl := range []time.Duration{-time.Second, time.Millisecond, MaxApprovedTTL + time.Second} {
		if err := (Config{PendingTTL: DefaultPendingTTL, ApprovedTTL: ttl}).Validate(); err == nil {
			t.Errorf("approved ttl %s accepted", ttl)
		}
	}
	for _, name := range []string{" lead", "trail ", "tab\there", "nl\nx", strings.Repeat("a", MaxApproverRunes+1), "\xff"} {
		if err := (Config{PendingTTL: DefaultPendingTTL, ApprovedBy: name}).Validate(); err == nil {
			t.Errorf("approver %q accepted", name)
		}
	}
	if err := (Config{PendingTTL: DefaultPendingTTL, ApprovedTTL: time.Second, ApprovedBy: "Ayşe Yılmaz"}).Validate(); err != nil {
		t.Error(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	if _, err := NewService(NewRepository(nil), Config{PendingTTL: DefaultPendingTTL, ApprovedTTL: 8 * 24 * time.Hour}, log); err == nil {
		t.Error("service accepted an out-of-range approved TTL")
	}
	// The repository refuses invalid parameters before touching the pool.
	repo := NewRepository(nil)
	ctx := context.Background()
	if _, err := repo.Confirm(ctx, ConfirmParams{ApprovedTTL: 0, ApprovedBy: "x"}); err == nil {
		t.Error("zero approved TTL accepted")
	}
	if _, err := repo.Confirm(ctx, ConfirmParams{ApprovedTTL: time.Hour, ApprovedBy: ""}); err == nil {
		t.Error("empty approver accepted")
	}
	var me *MismatchError
	if _, err := repo.Confirm(ctx, ConfirmParams{ApprovedTTL: time.Hour, ApprovedBy: "x", Channels: []segment.Channel{"push"}}); !errors.As(err, &me) {
		t.Errorf("unknown channel: %v", err)
	}
}

func TestDecisionErrors(t *testing.T) {
	me := &MismatchError{Fields: []string{"channels"}}
	if !errors.Is(me, ErrMismatch) || errors.Is(me, ErrExpired) {
		t.Error("MismatchError.Is")
	}
	if strings.Contains(me.Error(), "sms") {
		t.Error("mismatch message echoes values")
	}
	for _, e := range []error{ErrNotFound, ErrExpired, ErrMismatch} {
		if errors.Is(e, ErrInconsistent) || errors.Is(e, ErrBusy) {
			t.Errorf("%v overlaps", e)
		}
	}
}

// The confirming CAS evaluates expiry with clock_timestamp() after the lock
// (never now()), requires pending, binds the reviewed values, and is the
// only statement that sets an approval to approved; approved_at and
// expires_at derive from one clock value.
func TestConfirmSQL(t *testing.T) {
	low := strings.ToLower(sqlConfirmApproval)
	for _, want := range []string{"clock_timestamp()", "a.status = 'pending'", "a.expires_at > t.ts", "a.segment_version = $5",
		"a.content_id = $6", "a.channels = $7", "approved_at = t.ts", "expires_at = t.ts +"} {
		if !strings.Contains(low, want) {
			t.Errorf("confirm CAS lacks %q", want)
		}
	}
	if strings.Contains(low, "now()") || strings.Contains(low, "current_timestamp") {
		t.Error("confirm CAS uses now()")
	}
	// Reads never write; locked reads lock.
	if !strings.HasSuffix(sqlSelectApprovalForUpdate, "FOR UPDATE") || strings.Contains(strings.ToLower(sqlSelectApproval), "update") {
		t.Error("approval selects")
	}
	// Withdrawal reuses the CP14 revoke statement (live approvals only).
	if !strings.Contains(sqlRevokeApproval, "status IN ('pending', 'approved')") {
		t.Error("revoke not restricted to live approvals")
	}
}

func TestBindingVsCampaign(t *testing.T) {
	id := campaign.ID{1}
	cid := int64(5)
	cur := campaign.Campaign{SegmentVersion: 2, CurrentContentID: &cid, Channels: []segment.Channel{segment.ChannelSMS}, ActiveApprovalID: &id}
	r := row{segmentVersion: 2, contentID: 5, channels: []segment.Channel{segment.ChannelSMS}}
	if f := bindingVsCampaign(cur, r); len(f) != 0 {
		t.Errorf("equal binding: %v", f)
	}
	r = row{segmentVersion: 3, contentID: 6, channels: []segment.Channel{segment.ChannelEmail}}
	if f := bindingVsCampaign(cur, r); strings.Join(f, ",") != "campaign.segment_version,campaign.content_version,campaign.channels" {
		t.Errorf("fields = %v", f)
	}
	cur.CurrentContentID = nil
	if f := bindingVsCampaign(cur, row{segmentVersion: 2, contentID: 5, channels: cur.Channels}); len(f) != 1 {
		t.Errorf("no content: %v", f)
	}
}

// The configuration bounds and defaults (platform/config) are exactly this
// package's, so a value accepted at startup is accepted by NewService.
func TestBoundsMatchConfig(t *testing.T) {
	pairs := []struct {
		name      string
		cfg, here time.Duration
	}{
		{"default pending", config.DefaultApprovalPendingTTL, DefaultPendingTTL},
		{"default approved", config.DefaultApprovalApprovedTTL, DefaultApprovedTTL},
		{"min pending", config.MinApprovalTTL, MinPendingTTL},
		{"min approved", config.MinApprovalTTL, MinApprovedTTL},
		{"max pending", config.MaxApprovalPendingTTL, MaxPendingTTL},
		{"max approved", config.MaxApprovalApprovedTTL, MaxApprovedTTL},
	}
	for _, p := range pairs {
		if p.cfg != p.here {
			t.Errorf("%s: config %s, approval %s", p.name, p.cfg, p.here)
		}
	}
	if config.MaxApproverNameRunes != MaxApproverRunes || config.DefaultApproverName != DefaultApprover {
		t.Error("approver bounds differ")
	}
}
