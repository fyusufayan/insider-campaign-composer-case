//go:build integration

package integration

import (
	"context"
	"fmt"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/fyusufayan/insider-campaign-composer-case/backend/internal/delivery"
	"github.com/fyusufayan/insider-campaign-composer-case/backend/internal/mockprovider"
	"github.com/fyusufayan/insider-campaign-composer-case/backend/internal/platform/logging"
	"github.com/fyusufayan/insider-campaign-composer-case/backend/internal/provider"
)

// ---- CP16: POST /send over a real HTTP server and PostgreSQL, then the
// real worker and mock provider over TCP until the campaign completes.

type apiSendResp struct {
	Status         string           `json:"status"`
	ApprovalID     string           `json:"approval_id"`
	Released       map[string]int64 `json:"released"`
	AlreadyStarted bool             `json:"already_started"`
	Campaign       struct {
		apiCampaign
		ActiveApprovalID *string `json:"active_approval_id"`
	} `json:"campaign"`
	ServerTime time.Time `json:"server_time"`
}

func sendBody(aid string, version int) string {
	return fmt.Sprintf(`{"approval_id":%q,"expected_version":%d}`, aid, version)
}

func TestSendAPIFlowAndPrivacy(t *testing.T) {
	e := newAPIEnv(t, nil)
	if _, err := e.pool.Exec(ctxBG, `UPDATE users SET first_name = 'Zeynepxq', last_name = 'Kayaxq' WHERE id = ANY($1)`, istanbulMatched); err != nil {
		t.Fatal(err)
	}
	c := e.readyViaAPI(prepSMS, prepSubject, prepBody)
	p := e.prepareViaAPI(c.ID, c.Version)
	sendPath := "/api/v1/campaigns/" + c.ID + "/send"

	// Not confirmed yet → 409 invalid_state; nothing released.
	mustError(t, e.call("POST", sendPath, sendBody(p.Approval.ApprovalID, p.Campaign.Version)), 409, "invalid_state")
	conf := must[apiApprovalResp](t, e.call("POST", approvalPath(c.ID, p.Approval.ApprovalID, "confirm"), confirmBody(p.Campaign.Version, p.Approval)), 200)
	if conf.Campaign.Status != "approved" {
		t.Fatalf("confirm = %+v", conf.Campaign)
	}
	// Confirmation alone sends nothing.
	if n := e.count(`SELECT count(*) FROM campaign_deliveries WHERE status <> 'held'`); n != 0 {
		t.Fatalf("rows released by confirm: %d", n)
	}
	mustError(t, e.call("POST", sendPath, sendBody(p.Approval.ApprovalID, p.Campaign.Version)), 409, "version_conflict")
	mustError(t, e.call("POST", sendPath, sendBody("00000000-0000-4000-8000-000000000000", conf.Campaign.Version)), 404, "not_found")
	mustError(t, e.call("POST", sendPath, `{"approval_id":"x","expected_version":1}`), 422, "validation_failed")

	first := e.call("POST", sendPath, sendBody(p.Approval.ApprovalID, conf.Campaign.Version))
	sent := must[apiSendResp](t, first, 202)
	if sent.Status != "sending" || sent.AlreadyStarted || sent.ApprovalID != p.Approval.ApprovalID ||
		sent.Released["sms"] != 2 || sent.Released["email"] != 3 || len(sent.Released) != 2 ||
		sent.Campaign.Version != conf.Campaign.Version+1 || sent.Campaign.Status != "sending" ||
		sent.Campaign.ActiveApprovalID == nil || *sent.Campaign.ActiveApprovalID != p.Approval.ApprovalID || sent.ServerTime.IsZero() {
		t.Errorf("send = %+v", sent)
	}
	// Repeat → 200 already_started, idempotent.
	again := e.call("POST", sendPath, sendBody(p.Approval.ApprovalID, conf.Campaign.Version))
	rep := must[apiSendResp](t, again, 200)
	if !rep.AlreadyStarted || rep.Campaign.Version != sent.Campaign.Version || rep.Released["email"] != 3 {
		t.Errorf("repeat = %+v", rep)
	}

	// The worker delivers through the mock provider.
	mock, err := mockprovider.New(mockprovider.Config{}, logging.New(e.logs, slog.LevelInfo))
	if err != nil {
		t.Fatal(err)
	}
	hs := httptest.NewServer(mock.Handler())
	t.Cleanup(hs.Close)
	client, err := provider.NewClient(provider.ClientConfig{BaseURL: hs.URL, RequestTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	w, err := delivery.New(delivery.NewRepository(e.pool), client, delivery.Config{WorkerID: "api-test", PollInterval: 20 * time.Millisecond,
		CompletionInterval: 50 * time.Millisecond, ReaperInterval: 100 * time.Millisecond, RateWindow: testRateWindow, Gate: testGate,
		LockCheckInterval: 100 * time.Millisecond, LockCheckTimeout: 100 * time.Millisecond}, logging.New(e.logs, slog.LevelInfo))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- w.Run(ctx) }()
	deadline := time.Now().Add(30 * time.Second)
	tick := time.NewTicker(20 * time.Millisecond) // deadline-bounded polling of the API
	defer tick.Stop()
	var final apiCampaign
	for {
		final = must[apiCampaign](t, e.call("GET", "/api/v1/campaigns/"+c.ID, ""), 200)
		if final.Status == "completed" || time.Now().After(deadline) {
			break
		}
		<-tick.C
	}
	cancel()
	<-done
	if final.Status != "completed" || final.Version != sent.Campaign.Version+1 {
		t.Fatalf("final = %+v", final)
	}
	// A late repeat on the finished campaign is still idempotent (200).
	late := must[apiSendResp](t, e.call("POST", sendPath, sendBody(p.Approval.ApprovalID, final.Version)), 200)
	if !late.AlreadyStarted || late.Status != "completed" {
		t.Errorf("late repeat = %+v", late)
	}
	if n := len(mock.Messages()); n != 5 {
		t.Errorf("messages = %d", n)
	}

	sensitive := []string{"Zeynepxq", "Kayaxq", "@example.test", "+90555", `"user_id"`, `"vars"`, `"destination"`, `"idempotency_key"`,
		"Hi Zeynepxq"}
	for _, id := range istanbulMatched {
		sensitive = append(sensitive, fmt.Sprintf("%s:%d:", c.ID, id))
	}
	for name, text := range map[string]string{"send": string(first.body), "repeat": string(again.body), "logs": e.logs.String()} {
		for _, s := range sensitive {
			if strings.Contains(text, s) {
				t.Errorf("%s contains %q", name, s)
			}
		}
	}
	// The delivered messages carry the frozen personalization.
	for _, m := range mock.Messages() {
		if !strings.Contains(m.Text, "Zeynepxq") {
			t.Errorf("message without the frozen name: %q", m.Text)
		}
	}
}

func (e *apiEnv) count(sql string, args ...any) int64 {
	e.t.Helper()
	return scalar(e.t, e.pool, sql, args...)
}

// ---- CP17: POST /cancel over HTTP ----

type apiCancelResp struct {
	Status              string           `json:"status"`
	PreviousStatus      string           `json:"previous_status"`
	RevokedApprovalID   *string          `json:"revoked_approval_id"`
	ReleasedHeldRows    int64            `json:"released_held_rows"`
	CancelledDeliveries map[string]int64 `json:"cancelled_deliveries"`
	InFlight            map[string]int64 `json:"in_flight"`
	Campaign            struct {
		apiCampaign
		ActiveApprovalID *string `json:"active_approval_id"`
	} `json:"campaign"`
	ServerTime time.Time `json:"server_time"`
}

func TestCancelAPI(t *testing.T) {
	e := newAPIEnv(t, nil)
	c := e.readyViaAPI(prepSMS, prepSubject, prepBody)
	p := e.prepareViaAPI(c.ID, c.Version)
	conf := must[apiApprovalResp](t, e.call("POST", approvalPath(c.ID, p.Approval.ApprovalID, "confirm"), confirmBody(p.Campaign.Version, p.Approval)), 200)
	cancelPath := "/api/v1/campaigns/" + c.ID + "/cancel"
	body := func(v int) string { return fmt.Sprintf(`{"expected_version":%d}`, v) }

	mustError(t, e.call("POST", cancelPath, body(p.Campaign.Version)), 409, "version_conflict")
	mustError(t, e.call("POST", cancelPath, `{}`), 422, "validation_failed")
	mustError(t, e.call("POST", "/api/v1/campaigns/00000000-0000-4000-8000-000000000000/cancel", body(1)), 404, "not_found")

	res := e.call("POST", cancelPath, body(conf.Campaign.Version))
	out := must[apiCancelResp](t, res, 200)
	if out.Status != "cancelled" || out.PreviousStatus != "approved" || out.RevokedApprovalID == nil ||
		*out.RevokedApprovalID != p.Approval.ApprovalID || out.ReleasedHeldRows != 5 || len(out.CancelledDeliveries) != 0 ||
		len(out.InFlight) != 0 || out.Campaign.Version != conf.Campaign.Version+1 || out.Campaign.ActiveApprovalID != nil ||
		out.ServerTime.IsZero() {
		t.Errorf("cancel = %+v", out)
	}
	if n := e.count(`SELECT count(*) FROM campaign_deliveries`); n != 0 {
		t.Errorf("held rows left: %d", n)
	}
	// Repeats — current or stale version — and a send are refused as invalid_state.
	mustError(t, e.call("POST", cancelPath, body(out.Campaign.Version)), 409, "invalid_state")
	mustError(t, e.call("POST", cancelPath, body(conf.Campaign.Version)), 409, "invalid_state")
	mustError(t, e.call("POST", "/api/v1/campaigns/"+c.ID+"/send", sendBody(p.Approval.ApprovalID, out.Campaign.Version)), 409, "invalid_state")
	for _, s := range []string{"@example.test", "+90555", `"user_id"`, `"vars"`, `"destination"`, `"idempotency_key"`} {
		if strings.Contains(string(res.body), s) {
			t.Errorf("cancel response contains %q", s)
		}
	}
}
