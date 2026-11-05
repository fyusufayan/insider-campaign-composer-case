//go:build integration

package integration

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/fyusufayan/insider-campaign-composer-case/backend/internal/segment"
)

// ---- CP15: the approval HTTP API over a real server and PostgreSQL:
// prepare → read → confirm → read, withdraw, expiry, error mapping, strict
// JSON and the privacy of every response.

type apiApprovalSnapshot struct {
	Recipients   int64            `json:"recipients"`
	RenderIssues map[string]int64 `json:"render_issues"`
}

type apiApproval struct {
	ApprovalID      string                         `json:"approval_id"`
	CampaignID      string                         `json:"campaign_id"`
	Status          string                         `json:"status"`
	RevokeReason    *string                        `json:"revoke_reason"`
	CampaignVersion int                            `json:"campaign_version"`
	SegmentVersion  int                            `json:"segment_version"`
	ContentID       int64                          `json:"content_id"`
	ContentVersion  int                            `json:"content_version"`
	ContentOrigin   string                         `json:"content_origin"`
	Channels        []string                       `json:"channels"`
	AudienceStats   segment.AudienceStats          `json:"audience_stats"`
	SnapshotCount   int64                          `json:"snapshot_count"`
	SnapshotDigest  string                         `json:"snapshot_digest"`
	Snapshot        map[string]apiApprovalSnapshot `json:"snapshot"`
	PreparedAt      time.Time                      `json:"prepared_at"`
	ExpiresAt       time.Time                      `json:"expires_at"`
	ApprovedAt      *time.Time                     `json:"approved_at"`
	ApprovedBy      *string                        `json:"approved_by"`
	Review          *struct {
		Validation struct {
			Valid    bool `json:"valid"`
			Warnings []struct {
				Path, Code string
			} `json:"warnings"`
		} `json:"validation"`
		OfferFindings []struct {
			Path, Code, Message string
		} `json:"offer_findings"`
		Offer map[string]any `json:"offer"`
	} `json:"review"`
	RenderedContent *struct {
		SMS   *string `json:"sms"`
		Email *struct {
			Subject, Body, Footer string
		} `json:"email"`
	} `json:"rendered_content"`
}

type apiApprovalResp struct {
	Campaign struct {
		apiCampaign
		ActiveApprovalID *string `json:"active_approval_id"`
	} `json:"campaign"`
	Approval   apiApproval `json:"approval"`
	ServerTime time.Time   `json:"server_time"`
}

// readyViaAPI creates, interprets and saves edited content over HTTP.
func (e *apiEnv) readyViaAPI(sms, subject, body string) apiCampaign {
	e.t.Helper()
	c := e.create("Approval API")
	in := e.interpret(c.ID, replyIstanbul)
	draft, _ := json.Marshal(map[string]string{"sms_body": sms, "email_subject": subject, "email_body": body})
	saved := must[apiSaved](e.t, e.putContent(c.ID, string(draft), in.Campaign.Version), 200)
	return saved.Campaign
}

func (e *apiEnv) prepareViaAPI(id string, version int) apiApprovalResp {
	e.t.Helper()
	return must[apiApprovalResp](e.t, e.call("POST", "/api/v1/campaigns/"+id+"/approvals", fmt.Sprintf(`{"expected_version":%d}`, version)), 201)
}

func confirmBody(version int, a apiApproval) string {
	chs, _ := json.Marshal(a.Channels)
	return fmt.Sprintf(`{"expected_version":%d,"segment_version":%d,"content_version":%d,"channels":%s}`,
		version, a.SegmentVersion, a.ContentVersion, chs)
}

func approvalPath(cid, aid, action string) string {
	p := "/api/v1/campaigns/" + cid + "/approvals/" + aid
	if action != "" {
		p += "/" + action
	}
	return p
}

func TestApprovalAPIFlow(t *testing.T) {
	e := newAPIEnv(t, nil)
	c := e.readyViaAPI(prepSMS, prepSubject, prepBody)

	// Prepare.
	resp := e.call("POST", "/api/v1/campaigns/"+c.ID+"/approvals", fmt.Sprintf(`{"expected_version":%d}`, c.Version))
	prep := must[apiApprovalResp](t, resp, 201)
	a := prep.Approval
	if prep.Campaign.Status != "pending_approval" || prep.Campaign.Version != c.Version+1 ||
		prep.Campaign.ActiveApprovalID == nil || *prep.Campaign.ActiveApprovalID != a.ApprovalID {
		t.Errorf("campaign after prepare = %+v", prep.Campaign)
	}
	if a.Status != "pending" || a.CampaignVersion != prep.Campaign.Version || a.SnapshotCount != 5 ||
		a.Snapshot["sms"].Recipients != 2 || a.Snapshot["email"].Recipients != 3 || a.Snapshot["sms"].RenderIssues == nil ||
		!sameCounts(a.AudienceStats, wantIstanbulAudience) || a.ContentOrigin != "edited" || a.ContentVersion != 1 ||
		a.Review == nil || !a.Review.Validation.Valid || a.RenderedContent == nil || a.RenderedContent.Email == nil ||
		a.ApprovedAt != nil || len(a.SnapshotDigest) != 64 || a.ExpiresAt.Sub(a.PreparedAt) != prepTTL {
		t.Errorf("prepared approval = %+v", a)
	}
	if a.Review.Offer["kind"] != "percent_discount" || len(a.Review.OfferFindings) != 0 {
		t.Errorf("review = %+v", a.Review)
	}
	if prep.ServerTime.IsZero() {
		t.Error("no server_time")
	}

	// Read: identical binding, re-verified; campaign included.
	got := must[apiApprovalResp](t, e.call("GET", approvalPath(c.ID, a.ApprovalID, ""), ""), 200)
	if got.Approval.SnapshotDigest != a.SnapshotDigest || got.Approval.Status != "pending" ||
		got.Campaign.Version != prep.Campaign.Version || !got.Approval.ExpiresAt.Equal(a.ExpiresAt) {
		t.Errorf("read = %+v", got)
	}

	// Double prepare with the same (now stale) version → 409, nothing new.
	mustError(t, e.call("POST", "/api/v1/campaigns/"+c.ID+"/approvals", fmt.Sprintf(`{"expected_version":%d}`, c.Version)), 409, "version_conflict")
	// Confirm with a stale version → 409; with a wrong binding → 409 mismatch.
	mustError(t, e.call("POST", approvalPath(c.ID, a.ApprovalID, "confirm"), confirmBody(c.Version, a)), 409, "version_conflict")
	bad := a
	bad.ContentVersion = 7
	mm := mustError(t, e.call("POST", approvalPath(c.ID, a.ApprovalID, "confirm"), confirmBody(prep.Campaign.Version, bad)), 409, "approval_mismatch")
	if len(mm.Error.Details) != 1 || mm.Error.Details[0].Path != "content_version" {
		t.Errorf("mismatch details = %+v", mm.Error.Details)
	}

	// Confirm.
	conf := must[apiApprovalResp](t, e.call("POST", approvalPath(c.ID, a.ApprovalID, "confirm"), confirmBody(prep.Campaign.Version, a)), 200)
	if conf.Campaign.Status != "approved" || conf.Campaign.Version != prep.Campaign.Version+1 ||
		conf.Approval.Status != "approved" || conf.Approval.ApprovedAt == nil || conf.Approval.ApprovedBy == nil ||
		*conf.Approval.ApprovedBy != "reviewer@test" || conf.Approval.ExpiresAt.Sub(*conf.Approval.ApprovedAt) != apiApprovedTTL {
		t.Errorf("confirmed = %+v", conf)
	}
	// Repeat confirm → 409 invalid_state (already confirmed); nothing changes.
	mustError(t, e.call("POST", approvalPath(c.ID, a.ApprovalID, "confirm"), confirmBody(conf.Campaign.Version, a)), 409, "invalid_state")
	// Prepare on an approved campaign stays refused (CP14 decision).
	mustError(t, e.call("POST", "/api/v1/campaigns/"+c.ID+"/approvals", fmt.Sprintf(`{"expected_version":%d}`, conf.Campaign.Version)), 409, "invalid_state")

	// Refresh: the persisted approved state.
	reloaded := must[apiApprovalResp](t, e.call("GET", approvalPath(c.ID, a.ApprovalID, ""), ""), 200)
	camp := must[struct {
		Status           string  `json:"status"`
		Version          int     `json:"version"`
		ActiveApprovalID *string `json:"active_approval_id"`
	}](t, e.call("GET", "/api/v1/campaigns/"+c.ID, ""), 200)
	if reloaded.Approval.Status != "approved" || camp.Status != "approved" || camp.Version != conf.Campaign.Version ||
		camp.ActiveApprovalID == nil || *camp.ActiveApprovalID != a.ApprovalID {
		t.Errorf("after refresh: approval %s, campaign %+v", reloaded.Approval.Status, camp)
	}
	// No sending happened: all rows held, no destinations, no attempts.
	if n := scalar(t, e.pool, `SELECT count(*) FROM campaign_deliveries WHERE status <> 'held' OR destination IS NOT NULL`); n != 0 {
		t.Errorf("non-held rows = %d", n)
	}
	if n := scalar(t, e.pool, `SELECT count(*) FROM delivery_attempts`); n != 0 {
		t.Errorf("attempts = %d", n)
	}

	// Withdraw the approved approval (explicit, version-guarded).
	mustError(t, e.call("POST", approvalPath(c.ID, a.ApprovalID, "withdraw"), fmt.Sprintf(`{"expected_version":%d}`, prep.Campaign.Version)), 409, "version_conflict")
	w := must[apiApprovalResp](t, e.call("POST", approvalPath(c.ID, a.ApprovalID, "withdraw"), fmt.Sprintf(`{"expected_version":%d}`, conf.Campaign.Version)), 200)
	if w.Campaign.Status != "content_ready" || w.Campaign.ActiveApprovalID != nil || w.Campaign.Version != conf.Campaign.Version+1 ||
		w.Approval.Status != "revoked" || w.Approval.RevokeReason == nil || *w.Approval.RevokeReason != "withdrawn" ||
		w.Approval.Review != nil || w.Approval.Snapshot["sms"].RenderIssues != nil || w.Approval.Snapshot["sms"].Recipients != 2 {
		t.Errorf("withdrawn = %+v", w)
	}
	mustError(t, e.call("POST", approvalPath(c.ID, a.ApprovalID, "withdraw"), fmt.Sprintf(`{"expected_version":%d}`, w.Campaign.Version)), 409, "invalid_state")
	if n := scalar(t, e.pool, `SELECT count(*) FROM campaign_deliveries`); n != 0 {
		t.Errorf("held rows after withdraw = %d", n)
	}
}

func TestApprovalAPIExpiry(t *testing.T) {
	e := newAPIEnv(t, nil)
	c := e.readyViaAPI(prepSMS, prepSubject, prepBody)
	prep := e.prepareViaAPI(c.ID, c.Version)
	a := prep.Approval
	if _, err := e.pool.Exec(ctxBG, `UPDATE campaign_approvals SET expires_at = clock_timestamp() - interval '1 millisecond' WHERE id = $1::uuid`, a.ApprovalID); err != nil {
		t.Fatal(err)
	}
	// Confirm after expiry → 409 approval_expired; the expiry is committed.
	mustError(t, e.call("POST", approvalPath(c.ID, a.ApprovalID, "confirm"), confirmBody(prep.Campaign.Version, a)), 409, "approval_expired")
	got := must[apiApprovalResp](t, e.call("GET", approvalPath(c.ID, a.ApprovalID, ""), ""), 200)
	if got.Approval.Status != "expired" || got.Campaign.Status != "content_ready" || got.Campaign.Version != prep.Campaign.Version+1 ||
		got.Campaign.ActiveApprovalID != nil || got.Approval.Review != nil {
		t.Errorf("after expiry = %+v", got)
	}
	mustError(t, e.call("POST", approvalPath(c.ID, a.ApprovalID, "withdraw"), fmt.Sprintf(`{"expected_version":%d}`, got.Campaign.Version)), 409, "approval_expired")

	// Lazy expiry on the campaign read (second approval, made due).
	p2 := e.prepareViaAPI(c.ID, got.Campaign.Version)
	if _, err := e.pool.Exec(ctxBG, `UPDATE campaign_approvals SET expires_at = clock_timestamp() - interval '1 millisecond' WHERE id = $1::uuid`, p2.Approval.ApprovalID); err != nil {
		t.Fatal(err)
	}
	camp := must[apiCampaign](t, e.call("GET", "/api/v1/campaigns/"+c.ID, ""), 200)
	if camp.Status != "content_ready" || camp.Version != p2.Campaign.Version+1 {
		t.Errorf("campaign after lazy expiry = %+v", camp)
	}
}

func TestApprovalAPIOfferFindings(t *testing.T) {
	e := newAPIEnv(t, nil)
	c := e.readyViaAPI("Hi {{first_name|there}}, 50% off today and free shipping!",
		"{{first_name|Hello}}, your cart: 30% off",
		"Hi {{first_name|there}},\n\nOrders over 500 TL ship with a free gift.\n\nSee you!")
	prep := e.prepareViaAPI(c.ID, c.Version)
	read := must[apiApprovalResp](t, e.call("GET", approvalPath(c.ID, prep.Approval.ApprovalID, ""), ""), 200)
	conf := must[apiApprovalResp](t, e.call("POST", approvalPath(c.ID, prep.Approval.ApprovalID, "confirm"), confirmBody(prep.Campaign.Version, prep.Approval)), 200)
	for name, a := range map[string]apiApproval{"prepare": prep.Approval, "read": read.Approval, "confirm": conf.Approval} {
		if a.Review == nil {
			t.Fatalf("%s: no review", name)
		}
		var got []string
		for _, f := range a.Review.OfferFindings {
			got = append(got, f.Path+":"+f.Code)
			if f.Message == "" {
				t.Errorf("%s: finding without message", name)
			}
		}
		if strings.Join(got, ",") != "sms_body:unauthorized_offer,sms_body:unauthorized_offer,email_body:unauthorized_offer,email_body:unauthorized_claim" {
			t.Errorf("%s: findings = %v", name, got)
		}
	}
}

func TestApprovalAPIErrors(t *testing.T) {
	e := newAPIEnv(t, nil)
	c := e.readyViaAPI(prepSMS, prepSubject, prepBody)
	prep := e.prepareViaAPI(c.ID, c.Version)
	a := prep.Approval
	v := prep.Campaign.Version
	const unknown = "00000000-0000-4000-8000-000000000001"

	// 404: unknown campaign, unknown approval, another campaign's approval.
	mustError(t, e.call("GET", approvalPath(unknown, a.ApprovalID, ""), ""), 404, "not_found")
	mustError(t, e.call("GET", approvalPath(c.ID, unknown, ""), ""), 404, "not_found")
	mustError(t, e.call("POST", approvalPath(c.ID, unknown, "confirm"), confirmBody(v, a)), 404, "not_found")
	other := e.readyViaAPI(prepSMS, prepSubject, prepBody)
	mustError(t, e.call("POST", approvalPath(other.ID, a.ApprovalID, "withdraw"), fmt.Sprintf(`{"expected_version":%d}`, other.Version)), 404, "not_found")
	// 409 invalid_state: prepare from draft.
	draft := e.create("Draft only")
	mustError(t, e.call("POST", "/api/v1/campaigns/"+draft.ID+"/approvals", `{"expected_version":1}`), 409, "invalid_state")

	// 422: invalid ids and strict bodies.
	mustError(t, e.call("GET", approvalPath(c.ID, "not-a-uuid", ""), ""), 422, "validation_failed")
	cases := map[string]string{
		"missing":            `{}`,
		"unknown property":   fmt.Sprintf(`{"expected_version":%d,"acknowledged":true}`, v),
		"duplicate property": fmt.Sprintf(`{"expected_version":%d,"expected_version":%d}`, v, v),
		"string version":     `{"expected_version":"3"}`,
		"fraction":           `{"expected_version":3.5}`,
		"zero":               `{"expected_version":0}`,
		"array body":         `[1]`,
		"trailing data":      fmt.Sprintf(`{"expected_version":%d} {}`, v),
		"malformed":          `{"expected_version":`,
	}
	for name, body := range cases {
		for _, path := range []string{"/api/v1/campaigns/" + c.ID + "/approvals", approvalPath(c.ID, a.ApprovalID, "withdraw")} {
			r := e.call("POST", path, body)
			if r.status != 422 || !strings.Contains(string(r.body), `"validation_failed"`) {
				t.Errorf("%s %s: %d %s", name, path, r.status, r.body)
			}
		}
	}
	confirmCases := map[string]string{
		"unknown channel":    fmt.Sprintf(`{"expected_version":%d,"segment_version":1,"content_version":1,"channels":["push"]}`, v),
		"duplicate channel":  fmt.Sprintf(`{"expected_version":%d,"segment_version":1,"content_version":1,"channels":["sms","sms"]}`, v),
		"empty channels":     fmt.Sprintf(`{"expected_version":%d,"segment_version":1,"content_version":1,"channels":[]}`, v),
		"missing binding":    fmt.Sprintf(`{"expected_version":%d}`, v),
		"null channels":      fmt.Sprintf(`{"expected_version":%d,"segment_version":1,"content_version":1,"channels":null}`, v),
		"acknowledge flag":   fmt.Sprintf(`{"expected_version":%d,"segment_version":1,"content_version":1,"channels":["sms","email"],"acknowledge_offer_findings":true}`, v),
		"channel not string": fmt.Sprintf(`{"expected_version":%d,"segment_version":1,"content_version":1,"channels":[1]}`, v),
	}
	for name, body := range confirmCases {
		r := e.call("POST", approvalPath(c.ID, a.ApprovalID, "confirm"), body)
		if r.status != 422 || !strings.Contains(string(r.body), `"validation_failed"`) {
			t.Errorf("confirm %s: %d %s", name, r.status, r.body)
		}
	}
	// 413-equivalent: oversized body → 422 too_large; wrong media type → 415.
	big := `{"expected_version":1,"x":"` + strings.Repeat("a", 70<<10) + `"}`
	if r := e.call("POST", approvalPath(c.ID, a.ApprovalID, "withdraw"), big); r.status != 422 || !strings.Contains(string(r.body), "too_large") {
		t.Errorf("big body: %d", r.status)
	}
	// None of the rejected requests changed anything.
	st := must[apiApprovalResp](t, e.call("GET", approvalPath(c.ID, a.ApprovalID, ""), ""), 200)
	if st.Campaign.Version != v || st.Approval.Status != "pending" {
		t.Errorf("state changed: %+v", st)
	}
}

// Approval responses and logs never carry recipient data: no user ids,
// names, cities, contacts or frozen personalization values. The fixture's
// matched users get unique, greppable names.
func TestApprovalAPIPrivacy(t *testing.T) {
	e := newAPIEnv(t, nil)
	if _, err := e.pool.Exec(ctxBG, `UPDATE users SET first_name = 'Zeynepxq', last_name = 'Kayaxq', city = 'İstanbulxq' WHERE id = ANY($1)`, istanbulMatched); err != nil {
		t.Fatal(err)
	}
	if _, err := e.pool.Exec(ctxBG, `UPDATE users SET city_normalized = 'istanbul' WHERE id = ANY($1)`, istanbulMatched); err != nil {
		t.Fatal(err)
	}
	c := e.readyViaAPI(prepSMS, prepSubject, prepBody)
	prep := e.call("POST", "/api/v1/campaigns/"+c.ID+"/approvals", fmt.Sprintf(`{"expected_version":%d}`, c.Version))
	p := must[apiApprovalResp](t, prep, 201)
	read := e.call("GET", approvalPath(c.ID, p.Approval.ApprovalID, ""), "")
	conf := e.call("POST", approvalPath(c.ID, p.Approval.ApprovalID, "confirm"), confirmBody(p.Campaign.Version, p.Approval))
	cv := must[apiApprovalResp](t, conf, 200)
	wd := e.call("POST", approvalPath(c.ID, p.Approval.ApprovalID, "withdraw"), fmt.Sprintf(`{"expected_version":%d}`, cv.Campaign.Version))
	if wd.status != 200 {
		t.Fatalf("withdraw: %d %s", wd.status, wd.body)
	}
	sensitive := []string{"Zeynepxq", "Kayaxq", "İstanbulxq", "@example.test", "+90555", `"user_id"`, `"vars"`, `"destination"`, `"idempotency_key"`}
	for _, id := range istanbulMatched {
		sensitive = append(sensitive, fmt.Sprintf("%s:%d:", c.ID, id))
	}
	texts := map[string]string{"prepare": string(prep.body), "read": string(read.body), "confirm": string(conf.body),
		"withdraw": string(wd.body), "logs": e.logs.String()}
	for name, text := range texts {
		for _, s := range sensitive {
			if strings.Contains(text, s) {
				t.Errorf("%s contains %q", name, s)
			}
		}
	}
	// The rendered content uses only the fixed fictional example values.
	if p.Approval.RenderedContent == nil || p.Approval.RenderedContent.SMS == nil || strings.Contains(*p.Approval.RenderedContent.SMS, "Zeynep") {
		t.Errorf("rendered = %+v", p.Approval.RenderedContent)
	}
	for _, msg := range []string{`"msg":"approval prepared"`, `"msg":"approval confirmed"`, `"msg":"approval withdrawn"`} {
		if !strings.Contains(e.logs.String(), msg) {
			t.Errorf("missing log %s", msg)
		}
	}
}
