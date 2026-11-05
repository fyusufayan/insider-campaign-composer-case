package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/fyusufayan/insider-campaign-composer-case/backend/internal/ai"
	"github.com/fyusufayan/insider-campaign-composer-case/backend/internal/campaign"
	"github.com/fyusufayan/insider-campaign-composer-case/backend/internal/campaign/campaigntest"
	"github.com/fyusufayan/insider-campaign-composer-case/backend/internal/delivery"
	"github.com/fyusufayan/insider-campaign-composer-case/backend/internal/llm"
	"github.com/fyusufayan/insider-campaign-composer-case/backend/internal/segment"
)

const (
	exampleBrief = "Send a short and friendly 30% discount reminder to users in Istanbul who added products to their cart in the last 30 days but did not complete a purchase."
	validReply   = `{"segment":{"match":"all","conditions":[{"type":"attribute","field":"city","op":"eq","value":"Istanbul"},{"type":"behavior","predicate":"abandoned_cart","within_days":30}]},` +
		`"interpretation":"Users in Istanbul with an abandoned cart.","assumptions":["No purchase after the cart addition."],"unsupported":[],` +
		`"suggested_channels":["sms","email"],` +
		`"content_brief":{"goal":"cart_recovery","tone":"friendly","length":"short","offer":{"kind":"percent_discount","value":30}}}`
	invalidReply = `{"segment":{"match":"all","conditions":[{"type":"attribute","field":"sms_consent","op":"eq","value":"yes"}]},` +
		`"interpretation":"x","assumptions":[],"unsupported":[],"suggested_channels":["sms"],` +
		`"content_brief":{"goal":"g","tone":"t","length":"short","offer":{"kind":"none"}}}`
	wantRule = `{"version":1,"match":"all","conditions":[{"type":"attribute","field":"city","op":"eq","value":"istanbul"},{"type":"behavior","predicate":"abandoned_cart","within_days":30}]}`
)

type apiFixture struct {
	t        *testing.T
	router   http.Handler
	llm      *llm.Scripted
	store    *campaigntest.MemoryStore
	audience *campaigntest.Audience
	logs     *bytes.Buffer
	gen      *ai.Generator
	it       *ai.Interpreter
	svc      *campaign.Service
}

func (f *apiFixture) svcForTest(*testing.T) *campaign.Service { return f.svc }

func newAPIFixture(t *testing.T, client llm.Client) *apiFixture {
	t.Helper()
	return newAPIFixtureTimeout(t, client, 5*time.Second)
}

// newAPIFixtureTimeout is newAPIFixture with the content generation timeout.
func newAPIFixtureTimeout(t *testing.T, client llm.Client, contentTimeout time.Duration) *apiFixture {
	t.Helper()
	log, logs := newTestLogger()
	f := &apiFixture{t: t, store: campaigntest.NewMemoryStore(), audience: &campaigntest.Audience{}, logs: logs}
	if client == nil {
		f.llm = llm.NewScripted()
		client = f.llm
	}
	it, err := ai.NewInterpreter(client, ai.InterpreterConfig{Model: "cfg-model", Effort: llm.EffortLow, Timeout: 5 * time.Second, MaxTokens: 1000})
	if err != nil {
		t.Fatal(err)
	}
	gen, err := ai.NewGenerator(client, ai.GeneratorConfig{Model: "cfg-content-model", Effort: llm.EffortMedium, Timeout: contentTimeout, MaxTokens: 2000})
	if err != nil {
		t.Fatal(err)
	}
	f.gen, f.it = gen, it
	svc, err := campaign.NewService(f.store, f.audience, it, gen, log)
	if err != nil {
		t.Fatal(err)
	}
	f.svc = svc
	f.router = NewRouter(log, Deps{Campaigns: svc})
	return f
}

func (f *apiFixture) do(method, path, body string) *httptest.ResponseRecorder {
	f.t.Helper()
	var req *http.Request
	if body == "" {
		req = httptest.NewRequest(method, path, nil)
	} else {
		req = httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
	}
	rr := httptest.NewRecorder()
	f.router.ServeHTTP(rr, req)
	if rr.Header().Get(RequestIDHeader) == "" {
		f.t.Errorf("%s %s: no request id header", method, path)
	}
	return rr
}

func decode[T any](t *testing.T, rr *httptest.ResponseRecorder) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(rr.Body.Bytes(), &v); err != nil {
		t.Fatalf("decode %s: %v", rr.Body, err)
	}
	return v
}

type campaignResp struct {
	ID             string           `json:"id"`
	Name           string           `json:"name"`
	Brief          string           `json:"brief"`
	Status         string           `json:"status"`
	Channels       []string         `json:"channels"`
	SegmentRule    json.RawMessage  `json:"segment_rule"`
	SegmentVersion int              `json:"segment_version"`
	Interpretation *json.RawMessage `json:"interpretation"`
	ContentBrief   json.RawMessage  `json:"content_brief"`
	Version        int              `json:"version"`
}

type interpretResp struct {
	Campaign      campaignResp           `json:"campaign"`
	Audience      *segment.AudienceStats `json:"audience"`
	AudienceError *errorBody             `json:"audience_error"`
}

func (f *apiFixture) create() campaignResp {
	f.t.Helper()
	rr := f.do(http.MethodPost, "/api/v1/campaigns", `{"name":"Cart recovery","brief":"`+exampleBrief+`"}`)
	if rr.Code != http.StatusCreated {
		f.t.Fatalf("create: %d %s", rr.Code, rr.Body)
	}
	return decode[campaignResp](f.t, rr)
}

func (f *apiFixture) interpret(id string) interpretResp {
	f.t.Helper()
	f.llm.Push(llm.Reply(validReply))
	rr := f.do(http.MethodPost, "/api/v1/campaigns/"+id+"/interpret", "")
	if rr.Code != http.StatusOK {
		f.t.Fatalf("interpret: %d %s", rr.Code, rr.Body)
	}
	return decode[interpretResp](f.t, rr)
}

func TestAPICampaignFlow(t *testing.T) {
	f := newAPIFixture(t, nil)

	rr := f.do(http.MethodPost, "/api/v1/campaigns", `{"name":"  Cart recovery ","brief":"`+exampleBrief+`"}`)
	if rr.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rr.Code, rr.Body)
	}
	created := decode[campaignResp](t, rr)
	if loc := rr.Header().Get("Location"); loc != "/api/v1/campaigns/"+created.ID {
		t.Errorf("Location = %q", loc)
	}
	// Exact draft shape: nulls before interpretation, [] channels.
	var raw map[string]json.RawMessage
	_ = json.Unmarshal(rr.Body.Bytes(), &raw)
	for k, want := range map[string]string{"status": `"draft"`, "name": `"Cart recovery"`, "channels": `[]`,
		"segment_rule": `null`, "segment_version": `0`, "interpretation": `null`, "content_brief": `null`, "version": `1`} {
		if string(raw[k]) != want {
			t.Errorf("draft %s = %s, want %s", k, raw[k], want)
		}
	}

	res := f.interpret(created.ID)
	c := res.Campaign
	if c.Status != "segmented" || c.Version != 2 || c.SegmentVersion != 1 || strings.Join(c.Channels, ",") != "sms,email" {
		t.Errorf("after interpret: %+v", c)
	}
	if string(c.SegmentRule) != wantRule {
		t.Errorf("segment_rule = %s", c.SegmentRule)
	}
	if string(c.ContentBrief) != `{"goal":"cart_recovery","tone":"friendly","length":"short","offer":{"kind":"percent_discount","value":30}}` {
		t.Errorf("content_brief = %s", c.ContentBrief)
	}
	if res.Audience == nil || res.Audience.Messages != 12 || res.AudienceError != nil {
		t.Errorf("audience = %+v / %+v", res.Audience, res.AudienceError)
	}

	// GET returns the same state and does not bump the version.
	for i := 0; i < 2; i++ {
		got := decode[campaignResp](t, f.do(http.MethodGet, "/api/v1/campaigns/"+created.ID, ""))
		if got.Version != 2 || got.SegmentVersion != 1 || string(got.SegmentRule) != wantRule {
			t.Errorf("GET = %+v", got)
		}
	}

	// Audience preview.
	rr = f.do(http.MethodGet, "/api/v1/campaigns/"+created.ID+"/audience", "")
	if rr.Code != http.StatusOK {
		t.Fatalf("audience: %d %s", rr.Code, rr.Body)
	}
	want := `{"campaign_id":"` + created.ID + `","campaign_version":2,"segment_version":1,"channels":["sms","email"],` +
		`"audience":{"matched_users":10,"by_channel":{"email":{"eligible":6,"no_consent":3,"no_contact":1},"sms":{"eligible":6,"no_consent":3,"no_contact":1}},` +
		`"reach_users":{"both":6,"sms_only":0,"email_only":0,"unreachable":4},"messages":12,"computed_at":"2026-11-04T12:00:00Z"}}`
	if got := strings.TrimSpace(rr.Body.String()); got != want {
		t.Errorf("audience body =\n%s\nwant\n%s", got, want)
	}

	// Channels: explicit empty selection.
	rr = f.do(http.MethodPut, "/api/v1/campaigns/"+created.ID+"/channels", `{"channels":[],"expected_version":2}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("channels: %d %s", rr.Code, rr.Body)
	}
	if got := decode[campaignResp](t, rr); got.Version != 3 || len(got.Channels) != 0 || got.Channels == nil {
		t.Errorf("after channels: %+v", got)
	}
	audience := decode[audienceResponseDTO](t, f.do(http.MethodGet, "/api/v1/campaigns/"+created.ID+"/audience", ""))
	if audience.Audience.MatchedUsers != 10 || audience.Audience.Messages != 0 || len(audience.Audience.ByChannel) != 0 {
		t.Errorf("empty-channel audience = %+v", audience.Audience)
	}

	// Re-interpretation keeps the explicit empty selection.
	again := f.interpret(created.ID)
	if len(again.Campaign.Channels) != 0 || again.Campaign.SegmentVersion != 1 || again.Campaign.Version != 4 {
		t.Errorf("re-interpret: %+v", again.Campaign)
	}
}

func TestAPICreateValidation(t *testing.T) {
	f := newAPIFixture(t, nil)
	okBrief := `"brief":"a sufficiently long brief"`
	tests := []struct {
		name   string
		body   string
		status int
		want   []string // path:code
	}{
		{"missing both", `{}`, 422, []string{"name:required", "brief:required"}},
		{"null name", `{"name":null,` + okBrief + `}`, 422, []string{"name:wrong_type"}},
		{"number name", `{"name":5,` + okBrief + `}`, 422, []string{"name:wrong_type"}},
		{"array brief", `{"name":"n","brief":["x"]}`, 422, []string{"brief:wrong_type"}},
		{"unknown property", `{"name":"n",` + okBrief + `,"status":"approved"}`, 422, []string{"status:unknown_property"}},
		{"channels not accepted at create", `{"name":"n",` + okBrief + `,"channels":["sms"]}`, 422, []string{"channels:unknown_property"}},
		{"duplicate key", `{"name":"n","name":"m",` + okBrief + `}`, 422, []string{"name:duplicate_property"}},
		{"trailing data", `{"name":"n",` + okBrief + `} {}`, 422, []string{":trailing_data"}},
		{"malformed", `{"name":`, 422, []string{"name:malformed_json"}},
		{"not an object", `["name"]`, 422, []string{":wrong_type"}},
		{"empty body", ``, 422, []string{":required"}},
		{"brief too short", `{"name":"n","brief":"short"}`, 422, []string{"brief:too_short"}},
		{"brief too long", `{"name":"n","brief":"` + strings.Repeat("a", 2001) + `"}`, 422, []string{"brief:too_long"}},
		{"name too long", `{"name":"` + strings.Repeat("ğ", 121) + `",` + okBrief + `}`, 422, []string{"name:too_long"}},
		{"name blank", `{"name":"   ",` + okBrief + `}`, 422, []string{"name:empty"}},
		{"too large", `{"name":"n","brief":"` + strings.Repeat("a", maxBodyBytes) + `"}`, 422, []string{":too_large"}},
		{"too deep", `{"name":{"a":{"b":{"c":{"d":1}}}},` + okBrief + `}`, 422, []string{"name.a.b.c:too_deep"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rr := f.do(http.MethodPost, "/api/v1/campaigns", tt.body)
			if tt.body == "" {
				req := httptest.NewRequest(http.MethodPost, "/api/v1/campaigns", nil)
				rr = httptest.NewRecorder()
				f.router.ServeHTTP(rr, req)
			}
			body := assertEnvelope(t, rr, tt.status, CodeValidationFailed)
			var got []string
			for _, d := range body["details"].([]any) {
				m := d.(map[string]any)
				got = append(got, m["path"].(string)+":"+m["code"].(string))
			}
			if strings.Join(got, " ") != strings.Join(tt.want, " ") {
				t.Errorf("details = %v, want %v", got, tt.want)
			}
		})
	}

	// Content type is enforced for bodies.
	req := httptest.NewRequest(http.MethodPost, "/api/v1/campaigns", strings.NewReader(`{"name":"n",`+okBrief+`}`))
	req.Header.Set("Content-Type", "text/plain")
	rr := httptest.NewRecorder()
	f.router.ServeHTTP(rr, req)
	assertEnvelope(t, rr, http.StatusUnsupportedMediaType, CodeUnsupportedMediaType)

	req = httptest.NewRequest(http.MethodPost, "/api/v1/campaigns", strings.NewReader(`{"name":"n",`+okBrief+`}`))
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	rr = httptest.NewRecorder()
	f.router.ServeHTTP(rr, req)
	if rr.Code != http.StatusCreated {
		t.Errorf("charset parameter: %d %s", rr.Code, rr.Body)
	}
}

func TestAPIPathAndNotFound(t *testing.T) {
	f := newAPIFixture(t, nil)
	for _, p := range []string{"/api/v1/campaigns/nope", "/api/v1/campaigns/123/audience", "/api/v1/campaigns/0b8f3e2a1c4d4e5f8a9b0c1d2e3f4a5b"} {
		body := assertEnvelope(t, f.do(http.MethodGet, p, ""), 422, CodeValidationFailed)
		if d := body["details"].([]any); len(d) != 1 || d[0].(map[string]any)["path"] != "id" {
			t.Errorf("%s: details = %v", p, d)
		}
	}
	missing := "/api/v1/campaigns/0b8f3e2a-1c4d-4e5f-8a9b-0c1d2e3f4a5b"
	assertEnvelope(t, f.do(http.MethodGet, missing, ""), 404, CodeNotFound)
	assertEnvelope(t, f.do(http.MethodGet, missing+"/audience", ""), 404, CodeNotFound)
	assertEnvelope(t, f.do(http.MethodPost, missing+"/interpret", ""), 404, CodeNotFound)
	assertEnvelope(t, f.do(http.MethodPut, missing+"/channels", `{"channels":[],"expected_version":1}`), 404, CodeNotFound)
	if f.llm.Calls() != 0 {
		t.Error("LLM called for a missing campaign")
	}
	// Undocumented methods/paths.
	assertEnvelope(t, f.do(http.MethodDelete, missing, ""), 405, CodeMethodNotAllowed)
	assertEnvelope(t, f.do(http.MethodPatch, missing, `{"brief":"new brief text"}`), 405, CodeMethodNotAllowed)
	assertEnvelope(t, f.do(http.MethodGet, "/api/v1/campaigns", ""), 405, CodeMethodNotAllowed)
}

func TestAPIChannelsValidationAndConflicts(t *testing.T) {
	f := newAPIFixture(t, nil)
	c := f.create()
	path := "/api/v1/campaigns/" + c.ID + "/channels"

	// Draft: no rule yet.
	body := assertEnvelope(t, f.do(http.MethodPut, path, `{"channels":["sms"],"expected_version":1}`), 409, CodeInvalidState)
	if !strings.Contains(body["message"].(string), "draft") {
		t.Errorf("message = %v", body["message"])
	}
	assertEnvelope(t, f.do(http.MethodGet, "/api/v1/campaigns/"+c.ID+"/audience", ""), 409, CodeInvalidState)
	if f.audience.Calls() != 0 {
		t.Error("audience query for a draft")
	}

	f.interpret(c.ID) // version 2

	tests := []struct {
		name, body string
		want       []string
	}{
		{"missing version", `{"channels":["sms"]}`, []string{"expected_version:required"}},
		{"missing channels", `{"expected_version":2}`, []string{"channels:required"}},
		{"null channels", `{"channels":null,"expected_version":2}`, []string{"channels:wrong_type"}},
		{"string channels", `{"channels":"sms","expected_version":2}`, []string{"channels:wrong_type"}},
		{"unknown channel", `{"channels":["sms","push"],"expected_version":2}`, []string{"channels[1]:not_allowed"}},
		{"case matters", `{"channels":["SMS"],"expected_version":2}`, []string{"channels[0]:not_allowed"}},
		{"non-string channel", `{"channels":[1],"expected_version":2}`, []string{"channels[0]:wrong_type"}},
		{"string version", `{"channels":[],"expected_version":"2"}`, []string{"expected_version:wrong_type"}},
		{"fractional version", `{"channels":[],"expected_version":2.5}`, []string{"expected_version:wrong_type"}},
		{"exponent version", `{"channels":[],"expected_version":2e0}`, []string{"expected_version:wrong_type"}},
		{"zero version", `{"channels":[],"expected_version":0}`, []string{"expected_version:out_of_range"}},
		{"huge version", `{"channels":[],"expected_version":99999999999999999999}`, []string{"expected_version:out_of_range"}},
		{"forbidden status", `{"channels":[],"expected_version":2,"status":"approved"}`, []string{"status:unknown_property"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body := assertEnvelope(t, f.do(http.MethodPut, path, tt.body), 422, CodeValidationFailed)
			var got []string
			for _, d := range body["details"].([]any) {
				m := d.(map[string]any)
				got = append(got, m["path"].(string)+":"+m["code"].(string))
			}
			if strings.Join(got, " ") != strings.Join(tt.want, " ") {
				t.Errorf("details = %v, want %v", got, tt.want)
			}
		})
	}

	// Stale version.
	assertEnvelope(t, f.do(http.MethodPut, path, `{"channels":["email"],"expected_version":1}`), 409, CodeVersionConflict)
	// Duplicates and order are canonicalized.
	rr := f.do(http.MethodPut, path, `{"channels":["email","sms","email"],"expected_version":2}`)
	if got := decode[campaignResp](t, rr); rr.Code != 200 || strings.Join(got.Channels, ",") != "sms,email" || got.Version != 2 {
		t.Errorf("canonical no-op: %d %+v", rr.Code, got)
	}
	rr = f.do(http.MethodPut, path, `{"channels":["email"],"expected_version":2}`)
	if got := decode[campaignResp](t, rr); rr.Code != 200 || strings.Join(got.Channels, ",") != "email" || got.Version != 3 {
		t.Errorf("set email: %d %+v", rr.Code, got)
	}
	// The old version is now stale.
	assertEnvelope(t, f.do(http.MethodPut, path, `{"channels":["sms"],"expected_version":2}`), 409, CodeVersionConflict)
}

func TestAPIInvalidStateFromLiveApproval(t *testing.T) {
	f := newAPIFixture(t, nil)
	c := f.create()
	f.interpret(c.ID)
	id, _ := campaign.ParseID(c.ID)
	stored, _ := f.store.Get(context.Background(), id)
	stored.Status = campaign.StatusPendingApproval
	f.store.Put(stored)

	assertEnvelope(t, f.do(http.MethodPost, "/api/v1/campaigns/"+c.ID+"/interpret", ""), 409, CodeInvalidState)
	assertEnvelope(t, f.do(http.MethodPut, "/api/v1/campaigns/"+c.ID+"/channels",
		`{"channels":["sms"],"expected_version":`+strconv.Itoa(stored.Version)+`}`), 409, CodeInvalidState)
	if rr := f.do(http.MethodGet, "/api/v1/campaigns/"+c.ID+"/audience", ""); rr.Code != 200 {
		t.Errorf("audience in pending_approval: %d", rr.Code)
	}
	if f.llm.Calls() != 1 {
		t.Error("the LLM was called for a campaign that cannot be interpreted")
	}
}

func TestAPIInterpretBody(t *testing.T) {
	f := newAPIFixture(t, nil)
	c := f.create()
	path := "/api/v1/campaigns/" + c.ID + "/interpret"
	assertEnvelope(t, f.do(http.MethodPost, path, `{"brief":"override the brief"}`), 422, CodeValidationFailed)
	assertEnvelope(t, f.do(http.MethodPost, path, `{"expected_version":1}`), 422, CodeValidationFailed)
	assertEnvelope(t, f.do(http.MethodPost, path, `[]`), 422, CodeValidationFailed)
	if f.llm.Calls() != 0 {
		t.Error("LLM called for an invalid request")
	}
	f.llm.Push(llm.Reply(validReply))
	if rr := f.do(http.MethodPost, path, `{}`); rr.Code != 200 {
		t.Errorf("{} body: %d %s", rr.Code, rr.Body)
	}
}

// AI failures map to actionable errors and leave the campaign unchanged.
func TestAPIInterpretFailures(t *testing.T) {
	tests := []struct {
		name      string
		steps     []llm.Step
		status    int
		code      string
		retryable bool
		check     func(t *testing.T, body map[string]any)
	}{
		{"invalid after repair", []llm.Step{llm.Reply(invalidReply), llm.Reply(invalidReply)}, 422, CodeAIOutputInvalid, false,
			func(t *testing.T, body map[string]any) {
				d := body["details"].([]any)
				if len(d) == 0 || d[0].(map[string]any)["path"] != "segment.conditions[0].field" ||
					d[0].(map[string]any)["code"] != segment.CodeConsentBackendEnforced {
					t.Errorf("details = %v", d)
				}
				if body["raw_proposal"] != invalidReply {
					t.Errorf("raw_proposal = %v", body["raw_proposal"])
				}
			}},
		{"refusal", []llm.Step{llm.Stopped(validReply, llm.StopRefusal)}, 422, CodeLLMRefused, false, nil},
		{"truncation", []llm.Step{llm.Stopped(`{"seg`, llm.StopMaxTokens), llm.Stopped(`{"seg`, llm.StopMaxTokens)}, 502, CodeLLMTruncated, false, nil},
		{"provider unavailable", []llm.Step{llm.Fail(&llm.Error{Kind: llm.ErrUnavailable, StatusCode: 529,
			Err: errors.New("overloaded; api key sk-ant-secret")})}, 503, CodeLLMUnavailable, true, nil},
		{"provider rejected", []llm.Step{llm.Fail(&llm.Error{Kind: llm.ErrRejected, StatusCode: 401,
			Err: errors.New("invalid x-api-key sk-ant-secret")})}, 502, CodeLLMRejected, false, nil},
		{"timeout", []llm.Step{llm.Fail(context.DeadlineExceeded)}, 503, CodeLLMUnavailable, true, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newAPIFixture(t, nil)
			c := f.create()
			before := f.interpret(c.ID).Campaign

			f.llm.Push(tt.steps...)
			rr := f.do(http.MethodPost, "/api/v1/campaigns/"+c.ID+"/interpret", "")
			body := assertEnvelope(t, rr, tt.status, tt.code)
			if body["retryable"] != tt.retryable {
				t.Errorf("retryable = %v", body["retryable"])
			}
			if tt.check != nil {
				tt.check(t, body)
			} else if _, ok := body["raw_proposal"]; ok {
				t.Error("raw_proposal outside ai_output_invalid")
			}
			if strings.Contains(rr.Body.String(), "sk-ant") || strings.Contains(rr.Body.String(), "overloaded") {
				t.Errorf("provider error text leaked: %s", rr.Body)
			}
			after := decode[campaignResp](t, f.do(http.MethodGet, "/api/v1/campaigns/"+c.ID, ""))
			if after.Version != before.Version || after.SegmentVersion != before.SegmentVersion ||
				string(after.SegmentRule) != string(before.SegmentRule) || after.Status != before.Status {
				t.Errorf("campaign changed: before %+v after %+v", before, after)
			}
		})
	}
}

func TestAPIInterpretRepairSucceeds(t *testing.T) {
	f := newAPIFixture(t, nil)
	c := f.create()
	f.llm.Push(llm.Reply(`{"segment":`), llm.Reply(validReply))
	rr := f.do(http.MethodPost, "/api/v1/campaigns/"+c.ID+"/interpret", "")
	if rr.Code != 200 {
		t.Fatalf("%d %s", rr.Code, rr.Body)
	}
	if got := decode[interpretResp](t, rr); string(got.Campaign.SegmentRule) != wantRule || f.llm.Calls() != 2 {
		t.Errorf("campaign = %+v, calls = %d", got.Campaign, f.llm.Calls())
	}
}

// The demo provider declines briefs it cannot express; that is a 422 about
// the brief, not a provider failure, and nothing is saved.
func TestAPIInterpretDemoDeclines(t *testing.T) {
	f := newAPIFixture(t, llm.NewDemo(ai.DemoResponders()))
	rr := f.do(http.MethodPost, "/api/v1/campaigns", `{"name":"VIP","brief":"Send a promotion to our VIP customers in Istanbul."}`)
	c := decode[campaignResp](t, rr)
	rr = f.do(http.MethodPost, "/api/v1/campaigns/"+c.ID+"/interpret", "")
	body := assertEnvelope(t, rr, 422, CodeInvalidBrief)
	if d := body["details"].([]any); len(d) == 0 || d[0].(map[string]any)["path"] != "brief" {
		t.Errorf("details = %v", d)
	}
	if strings.Contains(rr.Body.String(), "VIP customers") {
		t.Error("the brief is echoed")
	}
	after := decode[campaignResp](t, f.do(http.MethodGet, "/api/v1/campaigns/"+c.ID, ""))
	if after.Status != "draft" || after.Version != 1 {
		t.Errorf("campaign = %+v", after)
	}

	// The example brief works end to end with the demo provider.
	rr = f.do(http.MethodPost, "/api/v1/campaigns", `{"name":"Example","brief":"`+exampleBrief+`"}`)
	c = decode[campaignResp](t, rr)
	rr = f.do(http.MethodPost, "/api/v1/campaigns/"+c.ID+"/interpret", "")
	if got := decode[interpretResp](t, rr); rr.Code != 200 || string(got.Campaign.SegmentRule) != wantRule {
		t.Errorf("demo example: %d %s", rr.Code, rr.Body)
	}
}

func TestAPIAudienceErrors(t *testing.T) {
	f := newAPIFixture(t, nil)
	c := f.create()

	// Interpretation commits even if the audience query fails afterwards.
	f.audience.Func = func(context.Context, segment.Validated, []segment.Channel) (segment.AudienceStats, error) {
		return segment.AudienceStats{}, context.DeadlineExceeded
	}
	res := f.interpret(c.ID)
	if res.Audience != nil || res.AudienceError == nil || res.AudienceError.Code != CodeAudienceUnavailable || !res.AudienceError.Retryable {
		t.Errorf("interpret audience = %+v / %+v", res.Audience, res.AudienceError)
	}
	if res.Campaign.SegmentVersion != 1 {
		t.Error("interpretation not committed")
	}
	assertEnvelope(t, f.do(http.MethodGet, "/api/v1/campaigns/"+c.ID+"/audience", ""), 503, CodeAudienceUnavailable)

	// Unexpected errors: generic 500, cause only in the (masked) log.
	f.audience.Func = func(context.Context, segment.Validated, []segment.Channel) (segment.AudienceStats, error) {
		return segment.AudienceStats{}, errors.New(`connect postgres://composer:hunter2@db:5432/composer: user ayse@example.com +905551234567`)
	}
	req := httptest.NewRequest(http.MethodGet, "/api/v1/campaigns/"+c.ID+"/audience", nil)
	req.Header.Set(RequestIDHeader, "req-500")
	rr := httptest.NewRecorder()
	f.router.ServeHTTP(rr, req)
	body := assertEnvelope(t, rr, 500, CodeInternal)
	if body["message"] != "internal server error" || strings.Contains(rr.Body.String(), "hunter2") || rr.Header().Get(RequestIDHeader) != "req-500" {
		t.Errorf("500 response = %s", rr.Body)
	}
	logs := f.logs.String()
	if !strings.Contains(logs, `"request_id":"req-500"`) || !strings.Contains(logs, `"campaign_id":"`+c.ID+`"`) {
		t.Errorf("error log lacks request/campaign id: %s", logs)
	}
	if strings.Contains(logs, "ayse@example.com") || strings.Contains(logs, "+905551234567") {
		t.Error("contact data logged unmasked")
	}
}

func TestAPIClientCancellation(t *testing.T) {
	f := newAPIFixture(t, nil)
	c := f.create()
	ctx, cancel := context.WithCancel(context.Background())
	f.llm.Push(llm.Step{Func: func(ctx context.Context, _ llm.Request) (llm.Response, error) {
		cancel()
		<-ctx.Done()
		return llm.Response{}, ctx.Err()
	}})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/campaigns/"+c.ID+"/interpret", nil).WithContext(ctx)
	rr := httptest.NewRecorder()
	f.router.ServeHTTP(rr, req)
	assertEnvelope(t, rr, statusClientClosedRequest, CodeRequestCancelled)
	if f.store.Updates() != 0 {
		t.Error("cancelled interpretation persisted")
	}
}

func TestAICodesMatchEnvelopeCodes(t *testing.T) {
	pairs := map[string]string{
		ai.CodeLLMUnavailable: CodeLLMUnavailable, ai.CodeLLMRejected: CodeLLMRejected,
		ai.CodeLLMRefused: CodeLLMRefused, ai.CodeLLMTruncated: CodeLLMTruncated,
		ai.CodeOutputInvalid: CodeAIOutputInvalid, ai.CodeInvalidBrief: CodeInvalidBrief,
	}
	for a, h := range pairs {
		if a != h {
			t.Errorf("ai code %q != envelope code %q", a, h)
		}
	}
	for _, kind := range []error{ai.ErrLLMUnavailable, ai.ErrLLMRejected, ai.ErrLLMRefused, ai.ErrLLMTruncated, ai.ErrOutputInvalid, ai.ErrInvalidBrief} {
		e := &ai.Error{Kind: kind}
		if got := aiAPIError(e).Code; got != e.Code() {
			t.Errorf("%v: mapped code %q, ai code %q", kind, got, e.Code())
		}
	}
}

func TestReadyz(t *testing.T) {
	log, _ := newTestLogger()
	ready := NewRouter(log, Deps{Readiness: func(context.Context) (ReadinessReport, error) {
		return ReadinessReport{Database: "ok", MigrationVersion: 1, LLMProvider: "demo", Worker: delivery.UnknownWorkerHealth()}, nil
	}})
	rr := httptest.NewRecorder()
	ready.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	want := `{"status":"ready","database":"ok","migration_version":1,"migration_dirty":false,"llm_provider":"demo",` +
		`"worker":{"status":"unknown","last_seen_at":null,"heartbeat_age_seconds":null,"stale_after_seconds":15,"fresh_processes":0},` +
		`"sending_campaigns":null}`
	if rr.Code != 200 || strings.TrimSpace(rr.Body.String()) != want {
		t.Errorf("ready: %d %s", rr.Code, rr.Body)
	}

	notReady := NewRouter(log, Deps{Readiness: func(context.Context) (ReadinessReport, error) {
		return ReadinessReport{Database: "unavailable", LLMProvider: "demo"}, errors.New("dial tcp postgres://u:secret@db")
	}})
	rr = httptest.NewRecorder()
	notReady.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if rr.Code != 503 || !strings.Contains(rr.Body.String(), `"status":"not_ready"`) || strings.Contains(rr.Body.String(), "secret") {
		t.Errorf("not ready: %d %s", rr.Code, rr.Body)
	}
}
