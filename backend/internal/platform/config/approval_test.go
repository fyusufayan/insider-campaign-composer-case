package config

import (
	"strings"
	"testing"
	"time"
)

func TestApprovalConfig(t *testing.T) {
	cfg, err := Load(env(validEnv()))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Approval != (Approval{PendingTTL: 15 * time.Minute, ApprovedTTL: 24 * time.Hour, ApproverName: "marketer@demo"}) {
		t.Errorf("defaults = %+v", cfg.Approval)
	}
	cfg, err = Load(env(with(validEnv(), "APPROVAL_PENDING_TTL", "20s", "APPROVAL_APPROVED_TTL", "1m", "APPROVER_NAME", "Ayşe")))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Approval != (Approval{PendingTTL: 20 * time.Second, ApprovedTTL: time.Minute, ApproverName: "Ayşe"}) {
		t.Errorf("overrides = %+v", cfg.Approval)
	}
	for _, kv := range [][2]string{
		{"APPROVAL_PENDING_TTL", "15"}, {"APPROVAL_PENDING_TTL", "500ms"}, {"APPROVAL_PENDING_TTL", "25h"},
		{"APPROVAL_APPROVED_TTL", "-1h"}, {"APPROVAL_APPROVED_TTL", "169h"}, {"APPROVER_NAME", strings.Repeat("x", 101)},
		{"APPROVER_NAME", "a\x07b"},
	} {
		if _, err := Load(env(with(validEnv(), kv[0], kv[1]))); err == nil || !strings.Contains(err.Error(), kv[0]) {
			t.Errorf("%s=%q: %v", kv[0], kv[1], err)
		}
	}
}
