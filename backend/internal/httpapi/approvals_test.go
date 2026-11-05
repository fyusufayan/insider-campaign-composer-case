package httpapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/fyusufayan/insider-campaign-composer-case/backend/internal/ai"
	"github.com/fyusufayan/insider-campaign-composer-case/backend/internal/approval"
	"github.com/fyusufayan/insider-campaign-composer-case/backend/internal/campaign"
	"github.com/fyusufayan/insider-campaign-composer-case/backend/internal/content"
	"github.com/fyusufayan/insider-campaign-composer-case/backend/internal/segment"
)

// newApprovalFixture is the API with approval routes whose repository has no
// database: these tests exercise only what the transport decides before
// any service call (ids, strict JSON) and the error mapping. Database
// behaviour is covered by test/integration.
func newApprovalFixture(t *testing.T) *apiFixture {
	t.Helper()
	f := newAPIFixture(t, nil)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	svc, err := approval.NewService(approval.NewRepository(nil), approval.Config{PendingTTL: approval.DefaultPendingTTL}, log)
	if err != nil {
		t.Fatal(err)
	}
	f.router = NewRouter(log, Deps{Campaigns: f.svc, Approvals: svc})
	return f
}

const (
	testCID = "11111111-1111-4111-8111-111111111111"
	testAID = "22222222-2222-4222-8222-222222222222"
)

func TestApprovalRoutesRejectInvalidIDs(t *testing.T) {
	f := newApprovalFixture(t)
	for _, tc := range []struct{ method, path, body, wantPath string }{
		{"POST", "/api/v1/campaigns/nope/approvals", `{"expected_version":1}`, "id"},
		{"GET", "/api/v1/campaigns/" + testCID + "/approvals/nope", "", "aid"},
		{"GET", "/api/v1/campaigns/nope/approvals/" + testAID, "", "id"},
		{"POST", "/api/v1/campaigns/" + testCID + "/approvals/" + strings.ToUpper(testAID) + "x/confirm", `{}`, "aid"},
		{"POST", "/api/v1/campaigns/" + testCID + "/approvals/123/withdraw", `{"expected_version":1}`, "aid"},
	} {
		rr := f.do(tc.method, tc.path, tc.body)
		env := assertEnvelope(t, rr, http.StatusUnprocessableEntity, CodeValidationFailed)
		details, _ := env["details"].([]any)
		if len(details) != 1 || details[0].(map[string]any)["path"] != tc.wantPath {
			t.Errorf("%s %s: details = %v", tc.method, tc.path, details)
		}
	}
	// Unknown sub-resources and methods use the envelope.
	assertEnvelope(t, f.do("DELETE", "/api/v1/campaigns/"+testCID+"/approvals/"+testAID, ""), http.StatusMethodNotAllowed, CodeMethodNotAllowed)
	assertEnvelope(t, f.do("POST", "/api/v1/campaigns/"+testCID+"/approvals/"+testAID+"/reopen", `{}`), http.StatusNotFound, CodeNotFound)
}

func TestApprovalRoutesStrictJSON(t *testing.T) {
	f := newApprovalFixture(t)
	prep := "/api/v1/campaigns/" + testCID + "/approvals"
	withdraw := prep + "/" + testAID + "/withdraw"
	confirm := prep + "/" + testAID + "/confirm"
	okConfirm := `"expected_version":3,"segment_version":1,"content_version":2,"channels":["sms","email"]`
	cases := []struct {
		path, body, code, field string
	}{
		{prep, ``, fieldRequired, ""},
		{prep, `{}`, fieldRequired, "expected_version"},
		{prep, `{"expected_version":1,"force":true}`, fieldUnknownProperty, "force"},
		{prep, `{"expected_version":1,"expected_version":2}`, fieldDuplicate, "expected_version"},
		{prep, `{"expected_version":"1"}`, fieldWrongType, "expected_version"},
		{prep, `{"expected_version":null}`, fieldWrongType, "expected_version"},
		{prep, `{"expected_version":1.0}`, fieldWrongType, "expected_version"},
		{prep, `{"expected_version":0}`, fieldOutOfRange, "expected_version"},
		{prep, `{"expected_version":99999999999}`, fieldOutOfRange, "expected_version"},
		{prep, `[]`, fieldWrongType, ""},
		{prep, `{"expected_version":`, fieldMalformedJSON, "expected_version"},
		{prep, `{"expected_version":1}{}`, fieldTrailingData, ""},
		{withdraw, `{"expected_version":true}`, fieldWrongType, "expected_version"},
		{withdraw, `{"expected_version":1,"reason":"x"}`, fieldUnknownProperty, "reason"},
		{confirm, `{` + okConfirm + `,"acknowledge":true}`, fieldUnknownProperty, "acknowledge"},
		{confirm, `{"expected_version":3}`, fieldRequired, "segment_version"},
		{confirm, `{"expected_version":3,"segment_version":1,"content_version":2,"channels":["push"]}`, fieldNotAllowed, "channels[0]"},
		{confirm, `{"expected_version":3,"segment_version":1,"content_version":2,"channels":["sms","sms"]}`, fieldDuplicate, "channels[1]"},
		{confirm, `{"expected_version":3,"segment_version":1,"content_version":2,"channels":[]}`, fieldRequired, "channels"},
		{confirm, `{"expected_version":3,"segment_version":1,"content_version":2,"channels":"sms"}`, fieldWrongType, "channels"},
		{confirm, `{"expected_version":3,"segment_version":1,"content_version":"2","channels":["sms"]}`, fieldWrongType, "content_version"},
		{confirm, `{"expected_version":3,"segment_version":1,"content_version":2,"channels":["sms"],"channels":["email"]}`, fieldDuplicate, "channels"},
	}
	for _, tc := range cases {
		rr := f.do("POST", tc.path, tc.body)
		env := assertEnvelope(t, rr, http.StatusUnprocessableEntity, CodeValidationFailed)
		details, _ := env["details"].([]any)
		found := false
		for _, d := range details {
			m := d.(map[string]any)
			if m["code"] == tc.code && m["path"] == tc.field {
				found = true
			}
		}
		if !found {
			t.Errorf("%s %s: details = %v, want %s at %q", tc.path, tc.body, details, tc.code, tc.field)
		}
	}
	// Body size bound and media type.
	big := `{"expected_version":1,"x":"` + strings.Repeat("a", maxBodyBytes) + `"}`
	env := assertEnvelope(t, f.do("POST", prep, big), http.StatusUnprocessableEntity, CodeValidationFailed)
	if !strings.Contains(fmt.Sprint(env["details"]), fieldTooLarge) {
		t.Errorf("big body: %v", env)
	}
	req := httptest.NewRequest("POST", withdraw, strings.NewReader(`{"expected_version":1}`))
	req.Header.Set("Content-Type", "text/plain")
	rr := httptest.NewRecorder()
	f.router.ServeHTTP(rr, req)
	assertEnvelope(t, rr, http.StatusUnsupportedMediaType, CodeUnsupportedMediaType)
}

func TestApprovalErrorMapping(t *testing.T) {
	log, logs := newTestLogger()
	h := &campaignHandlers{log: log}
	r := httptest.NewRequest("POST", "/api/v1/campaigns/"+testCID+"/approvals", nil)
	secret := "SELECT secret FROM users WHERE phone = '+905551234567'"
	cases := []struct {
		err       error
		status    int
		code      string
		retryable bool
	}{
		{approval.ErrNotFound, 404, CodeNotFound, false},
		{fmt.Errorf("wrapped: %w", approval.ErrExpired), 409, CodeApprovalExpired, false},
		{&approval.MismatchError{Fields: []string{"content_version", "campaign.channels"}}, 409, CodeApprovalMismatch, false},
		{&approval.EmptyAudienceError{}, 422, CodeEmptyAudience, false},
		{fmt.Errorf("%w: %s", approval.ErrAudienceChanged, secret), 409, CodeAudienceChanged, true},
		{fmt.Errorf("%w: %s", approval.ErrBusy, secret), 503, CodeCampaignBusy, true},
		{fmt.Errorf("%w: %s", approval.ErrInconsistent, secret), 500, CodeInternal, false},
		{&campaign.ConflictError{Expected: 1, Current: 2}, 409, CodeVersionConflict, false},
		{&campaign.StateError{Op: campaign.OpConfirmApproval, Status: campaign.StatusApproved, Reason: "the approval is already confirmed"}, 409, CodeInvalidState, false},
		{campaign.ErrNotFound, 404, CodeNotFound, false},
		{errors.New(secret), 500, CodeInternal, false},
	}
	for _, tc := range cases {
		e := h.toAPIError(r, tc.err)
		if e.Status != tc.status || e.Code != tc.code || e.Retryable != tc.retryable {
			t.Errorf("%v → %d %s %v", tc.err, e.Status, e.Code, e.Retryable)
		}
		if strings.Contains(e.Message, "SELECT") || strings.Contains(e.Message, "+90555") {
			t.Errorf("%v: message leaks: %s", tc.err, e.Message)
		}
	}
	mm := h.toAPIError(r, &approval.MismatchError{Fields: []string{"content_version", "channels"}})
	if len(mm.Details) != 2 || mm.Details[0].Path != "content_version" || mm.Details[1].Path != "channels" {
		t.Errorf("mismatch details = %+v", mm.Details)
	}
	_ = logs
}

// The approval DTO's JSON has the documented fields and nothing that could
// carry recipient data.
func TestApprovalDTOShape(t *testing.T) {
	id, sup := campaign.ID{1}, campaign.ID{2}
	approvedAt := time.Date(2026, 11, 5, 10, 0, 0, 0, time.UTC)
	sum := approval.Summary{
		ApprovalID: id, CampaignID: campaign.ID{3}, Status: approval.StatusApproved, CampaignVersion: 9, SegmentVersion: 2,
		ContentID: 11, ContentVersion: 3, ContentProvenance: content.Provenance{Origin: content.OriginAI, Model: "demo", PromptVersion: "content_v1"},
		Channels:      []segment.Channel{segment.ChannelSMS},
		AudienceStats: segment.AudienceStats{MatchedUsers: 4, ByChannel: map[segment.Channel]segment.ChannelStats{segment.ChannelSMS: {Eligible: 2, NoConsent: 1, NoContact: 1}}, Messages: 2},
		SnapshotCount: 2, SnapshotDigest: strings.Repeat("a", 64), PreparedAt: approvedAt.Add(-time.Minute), ExpiresAt: approvedAt.Add(24 * time.Hour),
		ApprovedAt: &approvedAt, ApprovedBy: "marketer@demo",
		Snapshot: map[segment.Channel]approval.ChannelSnapshot{segment.ChannelSMS: {Recipients: 2, RenderIssues: map[string]int64{"missing_value": 1}}},
		Reviewed: true,
		Review: campaign.ApprovalReview{Validation: content.Result{},
			OfferFindings: segment.Errors{{Path: "sms_body", Code: ai.CodeUnauthorizedOffer, Message: "mentions an offer the content brief does not authorize"}},
			Offer:         ai.Offer{Kind: ai.OfferPercentDiscount, Value: 30}},
		Superseded: &sup,
	}
	b, err := json.Marshal(toApprovalDTO(sum))
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	var keys []string
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	want := []string{"approval_id", "approved_at", "approved_by", "audience_stats", "campaign_id", "campaign_version", "channels",
		"closed_at", "content_id", "content_model", "content_origin", "content_prompt_version", "content_version", "expires_at",
		"prepared_at", "rendered_content", "review", "revoke_reason", "segment_version", "snapshot", "snapshot_count",
		"snapshot_digest", "status", "superseded_approval_id"}
	if !slices.Equal(keys, want) {
		t.Errorf("keys = %v", keys)
	}
	s := string(b)
	for _, bad := range []string{"user_id", "first_name", "last_name", "phone", "email\":", "destination", "vars", "idempotency"} {
		if strings.Contains(s, bad) {
			t.Errorf("DTO contains %q: %s", bad, s)
		}
	}
	if !strings.Contains(s, `"offer":{"kind":"percent_discount","value":30}`) || !strings.Contains(s, `"render_issues":{"missing_value":1}`) {
		t.Errorf("dto = %s", s)
	}
	// A closed approval: no review, no render issues (null, not {}).
	sum.Status, sum.Reviewed = approval.StatusExpired, false
	sum.Snapshot = map[segment.Channel]approval.ChannelSnapshot{segment.ChannelSMS: {Recipients: 2}}
	b, _ = json.Marshal(toApprovalDTO(sum))
	if s := string(b); !strings.Contains(s, `"review":null`) || !strings.Contains(s, `"rendered_content":null`) ||
		!strings.Contains(s, `"render_issues":null`) {
		t.Errorf("closed dto = %s", s)
	}
}

func TestCampaignDTOActiveApproval(t *testing.T) {
	id := campaign.ID{4}
	dto, err := toCampaignDTO(campaign.Campaign{ActiveApprovalID: &id, Channels: []segment.Channel{}})
	if err != nil || dto.ActiveApprovalID == nil || *dto.ActiveApprovalID != id.String() {
		t.Errorf("dto = %+v, %v", dto, err)
	}
	b, _ := json.Marshal(campaignDTO{Channels: []string{}})
	if !strings.Contains(string(b), `"active_approval_id":null`) {
		t.Errorf("json = %s", b)
	}
}
