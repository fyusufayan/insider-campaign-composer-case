package httpapi

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fyusufayan/insider-campaign-composer-case/backend/internal/campaign"
	"github.com/fyusufayan/insider-campaign-composer-case/backend/internal/content"
	"github.com/fyusufayan/insider-campaign-composer-case/backend/internal/llm"
)

const (
	genSMS     = "Hi {{first_name|there}}, your cart is waiting. Enjoy 30% off today."
	genSubject = "{{first_name|Hello}}, your cart is waiting: 30% off"
	genBody    = "Hi {{first_name|there}},\n\nYour cart is still waiting. Enjoy 30% off your order.\n\nSee you soon!"
)

func genReply(sms, subject, body string) string {
	m := map[string]string{}
	if sms != "" {
		m["sms_body"] = sms
	}
	if subject != "" {
		m["email_subject"] = subject
	}
	if body != "" {
		m["email_body"] = body
	}
	b, _ := json.Marshal(m)
	return string(b)
}

var okGen = genReply(genSMS, genSubject, genBody)

// sseEvent is one parsed server-sent event.
type sseEvent struct {
	Name string
	Data json.RawMessage
}

// parseSSE parses a complete event stream strictly: every event has a name
// and exactly one data line of valid JSON; comments are skipped.
func parseSSE(t *testing.T, r io.Reader) []sseEvent {
	t.Helper()
	var out []sseEvent
	sc := bufio.NewScanner(r)
	sc.Buffer(nil, 1<<20)
	var cur sseEvent
	for sc.Scan() {
		line := sc.Text()
		switch {
		case line == "":
			if cur.Name != "" || cur.Data != nil {
				if cur.Name == "" || !json.Valid(cur.Data) {
					t.Fatalf("malformed event %+v", cur)
				}
				out = append(out, cur)
			}
			cur = sseEvent{}
		case strings.HasPrefix(line, ":"):
		case strings.HasPrefix(line, "event: "):
			cur.Name = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			if cur.Data != nil {
				t.Fatalf("multi-line data in %q", cur.Name)
			}
			cur.Data = json.RawMessage(strings.TrimPrefix(line, "data: "))
		default:
			t.Fatalf("unexpected line %q", line)
		}
	}
	if cur.Name != "" || cur.Data != nil {
		t.Fatalf("stream ended inside an event: %+v", cur)
	}
	return out
}

func names(evs []sseEvent) []string {
	out := make([]string, len(evs))
	for i, e := range evs {
		out[i] = e.Name
		if e.Name == "status" {
			var st struct {
				Phase string `json:"phase"`
			}
			_ = json.Unmarshal(e.Data, &st)
			out[i] += ":" + st.Phase
		}
	}
	return out
}

func compact(evs []string) string {
	var out []string
	for _, n := range evs {
		if len(out) > 0 && n == "delta" && out[len(out)-1] == "delta" {
			continue
		}
		out = append(out, n)
	}
	return strings.Join(out, " ")
}

type savedResp struct {
	Campaign struct {
		ID               string `json:"id"`
		Status           string `json:"status"`
		Version          int    `json:"version"`
		CurrentContentID *int64 `json:"current_content_id"`
	} `json:"campaign"`
	Content currentContentResp `json:"content"`
	Created bool               `json:"created"`
	Notes   []string           `json:"notes"`
}

type currentContentResp struct {
	CampaignID      string   `json:"campaign_id"`
	CampaignVersion int      `json:"campaign_version"`
	Channels        []string `json:"channels"`
	Content         *struct {
		ID            int64   `json:"id"`
		Version       int     `json:"version"`
		Origin        string  `json:"origin"`
		Model         *string `json:"model"`
		PromptVersion *string `json:"prompt_version"`
		SMSBody       *string `json:"sms_body"`
		EmailSubject  *string `json:"email_subject"`
		EmailBody     *string `json:"email_body"`
	} `json:"content"`
	Validation    *validationResp   `json:"validation"`
	ExampleValues map[string]string `json:"example_values"`
	Limits        map[string]int    `json:"limits"`
}

type validationResp struct {
	Valid    bool         `json:"valid"`
	Errors   []FieldError `json:"errors"`
	Warnings []FieldError `json:"warnings"`
	SMS      *struct {
		CharsWorstCase int `json:"chars_worst_case"`
		Limit          int `json:"limit"`
	} `json:"sms"`
	RenderedExample *struct {
		SMS   *string `json:"sms"`
		Email *struct {
			Subject, Body, Footer string
		} `json:"email"`
	} `json:"rendered_example"`
}

func (f *apiFixture) segmented() campaignResp {
	f.t.Helper()
	c := f.create()
	return f.interpret(c.ID).Campaign
}

func (f *apiFixture) generateSSE(id string) (*httptest.ResponseRecorder, []sseEvent) {
	f.t.Helper()
	rr := f.do(http.MethodPost, "/api/v1/campaigns/"+id+"/content/generations", "")
	if rr.Code != http.StatusOK || !strings.HasPrefix(rr.Header().Get("Content-Type"), "text/event-stream") {
		return rr, nil
	}
	evs := parseSSE(f.t, bytes.NewReader(rr.Body.Bytes()))
	terminals := 0
	for i, e := range evs {
		if e.Name == "validated" || e.Name == "error" {
			terminals++
			if i != len(evs)-1 {
				f.t.Errorf("terminal event %q is not last", e.Name)
			}
		}
	}
	if terminals != 1 {
		f.t.Errorf("stream has %d terminal events", terminals)
	}
	return rr, evs
}

func (f *apiFixture) versions(id string) []content.Version {
	cid, _ := campaign.ParseID(id)
	return f.store.ContentVersions(cid)
}

func hasField(errs []FieldError, path, code string) bool {
	for _, e := range errs {
		if e.Path == path && e.Code == code {
			return true
		}
	}
	return false
}

func TestAPIContentEmptyBeforeGeneration(t *testing.T) {
	f := newAPIFixture(t, nil)
	c := f.segmented()
	rr := f.do(http.MethodGet, "/api/v1/campaigns/"+c.ID+"/content", "")
	if rr.Code != 200 {
		t.Fatalf("status %d: %s", rr.Code, rr.Body)
	}
	got := decode[currentContentResp](t, rr)
	if got.Content != nil || got.Validation != nil || got.CampaignVersion != c.Version || got.CampaignID != c.ID {
		t.Errorf("empty content = %s", rr.Body)
	}
	if got.ExampleValues["first_name"] != "Ayşe" || got.Limits["sms_body"] != 160 || got.Limits["email_body"] != 5000 {
		t.Errorf("example values / limits = %s", rr.Body)
	}
	// Raw JSON: content and validation are explicit nulls.
	var raw map[string]json.RawMessage
	_ = json.Unmarshal(rr.Body.Bytes(), &raw)
	if string(raw["content"]) != "null" || string(raw["validation"]) != "null" {
		t.Errorf("raw = %s", rr.Body)
	}
	// Reading content in draft is allowed too (nothing to show).
	d := f.create()
	if rr := f.do(http.MethodGet, "/api/v1/campaigns/"+d.ID+"/content", ""); rr.Code != 200 {
		t.Errorf("draft: %d", rr.Code)
	}
	assertEnvelope(t, f.do(http.MethodGet, "/api/v1/campaigns/00000000-0000-4000-8000-000000000000/content", ""), 404, CodeNotFound)
}

// orderLog records the commit and the validated write in one total order.
type orderLog struct {
	mu     sync.Mutex
	events []string
}

func (o *orderLog) add(s string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.events = append(o.events, s)
}

type committingStore struct {
	campaign.Store
	log *orderLog
}

func (s committingStore) SaveContent(ctx context.Context, id campaign.ID, m campaign.ContentMutation) (campaign.Campaign, content.Version, error) {
	c, v, err := s.Store.SaveContent(ctx, id, m)
	if err == nil {
		s.log.add("commit")
	}
	return c, v, err
}

type orderWriter struct {
	http.ResponseWriter
	log *orderLog
}

func (w orderWriter) Write(p []byte) (int, error) {
	if bytes.Contains(p, []byte("event: validated")) {
		w.log.add("validated")
	}
	return w.ResponseWriter.Write(p)
}

func (w orderWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func TestAPIGenerateContentStream(t *testing.T) {
	f := newAPIFixture(t, nil)
	c := f.segmented()
	order := &orderLog{}
	// Rebuild the router over a store that records the commit.
	log, _ := newTestLogger()
	svc, err := campaign.NewService(committingStore{f.store, order}, f.audience, f.it, f.gen, log)
	if err != nil {
		t.Fatal(err)
	}
	router := NewRouter(log, Deps{Campaigns: svc})

	f.llm.Push(llm.Step{Response: llm.Response{Text: okGen, StopReason: llm.StopEndTurn},
		Chunks: []string{okGen[:9], okGen[9:31], okGen[31:70], okGen[70:]}})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/campaigns/"+c.ID+"/content/generations", nil)
	rr := httptest.NewRecorder()
	router.ServeHTTP(orderWriter{rr, order}, req)

	if rr.Code != 200 {
		t.Fatalf("status %d: %s", rr.Code, rr.Body)
	}
	h := rr.Header()
	if h.Get("Content-Type") != "text/event-stream; charset=utf-8" || h.Get("Cache-Control") != "no-cache" ||
		h.Get("X-Accel-Buffering") != "no" {
		t.Errorf("headers = %v", h)
	}
	evs := parseSSE(t, bytes.NewReader(rr.Body.Bytes()))
	want := "meta status:generating delta status:validating status:persisting validated"
	if got := compact(names(evs)); got != want {
		t.Fatalf("events = %s\nwant      %s", got, want)
	}
	// The terminal event was written only after the commit.
	if strings.Join(order.events, ",") != "commit,validated" {
		t.Errorf("order = %v", order.events)
	}

	var meta sseMeta
	_ = json.Unmarshal(evs[0].Data, &meta)
	if meta.CampaignID != c.ID || meta.CampaignVersion != c.Version || strings.Join(meta.Fields, ",") != "sms_body,email_subject,email_body" ||
		meta.PromptVersion != "content_v1" {
		t.Errorf("meta = %+v", meta)
	}
	// Deltas are labelled previews and reassemble the streamed fields.
	preview := map[string]string{}
	for _, e := range evs {
		if e.Name != "delta" {
			continue
		}
		var d sseDelta
		_ = json.Unmarshal(e.Data, &d)
		if !d.Preview || d.Attempt != 1 {
			t.Errorf("delta = %+v", d)
		}
		preview[d.Field] += d.Text
	}
	if preview["sms_body"] != genSMS || preview["email_subject"] != genSubject || preview["email_body"] != genBody {
		t.Errorf("preview = %#v", preview)
	}

	saved := decodeJSON[savedResp](t, evs[len(evs)-1].Data)
	if saved.Campaign.Status != "content_ready" || saved.Campaign.Version != c.Version+1 || !saved.Created ||
		saved.Campaign.CurrentContentID == nil || saved.Content.CampaignVersion != saved.Campaign.Version {
		t.Errorf("saved campaign = %+v", saved.Campaign)
	}
	cv := saved.Content.Content
	if cv == nil || cv.Version != 1 || cv.Origin != "ai" || cv.Model == nil || *cv.Model != "cfg-content-model" ||
		cv.PromptVersion == nil || *cv.PromptVersion != "content_v1" || *cv.SMSBody != genSMS || cv.ID != *saved.Campaign.CurrentContentID {
		t.Errorf("content = %+v", cv)
	}
	v := saved.Content.Validation
	if v == nil || !v.Valid || v.SMS == nil || v.SMS.Limit != 160 || v.RenderedExample == nil ||
		!strings.Contains(*v.RenderedExample.SMS, "Ayşe") || v.RenderedExample.Email == nil ||
		v.RenderedExample.Email.Footer != content.EmailFooter {
		t.Errorf("validation = %+v", v)
	}

	// GET /content returns exactly the committed state.
	rr2 := f.do(http.MethodGet, "/api/v1/campaigns/"+c.ID+"/content", "")
	var a, b any
	_ = json.Unmarshal(rr2.Body.Bytes(), &a)
	got, _ := json.Marshal(saved.Content)
	_ = json.Unmarshal(got, &b)
	reGot, _ := json.Marshal(decode[currentContentResp](t, rr2))
	if string(reGot) != string(got) {
		t.Errorf("GET /content = %s\nvalidated  = %s", reGot, got)
	}
	// No system prompt or request wrapper leaks into the stream.
	if strings.Contains(rr.Body.String(), "content_request") || strings.Contains(rr.Body.String(), "You write") {
		t.Errorf("prompt text in the stream")
	}
}

func decodeJSON[T any](t *testing.T, data []byte) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(data, &v); err != nil {
		t.Fatalf("decode %s: %v", data, err)
	}
	return v
}

func TestAPIGenerateContentRepairStreamsTwoAttempts(t *testing.T) {
	f := newAPIFixture(t, nil)
	c := f.segmented()
	f.llm.Push(llm.Reply(genReply("Hi {{email}}", genSubject, genBody)), llm.Reply(okGen))
	_, evs := f.generateSSE(c.ID)
	want := "meta status:generating delta status:validating status:repairing status:generating delta status:validating status:persisting validated"
	if got := compact(names(evs)); got != want {
		t.Fatalf("events = %s", got)
	}
	var st sseStatus
	for _, e := range evs {
		var s sseStatus
		if e.Name == "status" && json.Unmarshal(e.Data, &s) == nil && s.Phase == "generating" {
			st = s // the last (second) attempt
		}
	}
	if st.Attempt != 2 || st.Reason != "repair" {
		t.Errorf("second attempt = %+v", st)
	}
}

func TestAPIGenerateContentFailuresAreErrorEvents(t *testing.T) {
	tests := []struct {
		name      string
		steps     []llm.Step
		status    int
		code      string
		retryable bool
	}{
		{"refusal", []llm.Step{llm.Stopped("", llm.StopRefusal)}, 422, CodeLLMRefused, false},
		{"repair fails", []llm.Step{llm.Reply(genReply("{{x}}", genSubject, genBody)), llm.Reply(genReply("{{y}}", genSubject, genBody))}, 422, CodeAIOutputInvalid, false},
		{"truncated", []llm.Step{llm.Stopped(`{"sms_body":"Hi`, llm.StopMaxTokens), llm.Stopped(`{"sms_body":"Hi`, llm.StopMaxTokens)}, 502, CodeLLMTruncated, false},
		{"unavailable", []llm.Step{llm.Fail(&llm.Error{Kind: llm.ErrUnavailable, StatusCode: 529, Err: io.ErrUnexpectedEOF})}, 503, CodeLLMUnavailable, true},
		{"rejected", []llm.Step{llm.Fail(&llm.Error{Kind: llm.ErrRejected, StatusCode: 401})}, 502, CodeLLMRejected, false},
		{"timeout", []llm.Step{llm.Hang()}, 503, CodeLLMUnavailable, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newAPIFixtureTimeout(t, nil, 100*time.Millisecond)
			c := f.segmented()
			f.llm.Push(tt.steps...)
			rr, evs := f.generateSSE(c.ID)
			if evs == nil {
				t.Fatalf("not a stream: %d %s", rr.Code, rr.Body)
			}
			last := evs[len(evs)-1]
			if last.Name != "error" || names(evs)[0] != "meta" {
				t.Fatalf("events = %v", names(evs))
			}
			for _, e := range evs {
				if e.Name == "validated" || (e.Name == "status" && strings.Contains(string(e.Data), "persisting")) {
					t.Errorf("unexpected %s", e.Name)
				}
			}
			ev := decodeJSON[sseError](t, last.Data)
			if ev.Status != tt.status || ev.Error.Code != tt.code || ev.Error.Retryable != tt.retryable {
				t.Errorf("error = %+v", ev)
			}
			if tt.code == CodeAIOutputInvalid && (ev.Error.RawProposal == "" || len(ev.Error.Details) == 0) {
				t.Errorf("ai_output_invalid lacks details: %+v", ev)
			}
			if strings.Contains(ev.Error.Message, "brief") && tt.code != CodeLLMRefused {
				t.Errorf("message talks about the brief: %q", ev.Error.Message)
			}
			// Nothing was saved and the campaign did not move.
			if len(f.versions(c.ID)) != 0 {
				t.Error("a version was saved")
			}
			cur := decode[campaignResp](t, f.do(http.MethodGet, "/api/v1/campaigns/"+c.ID, ""))
			if cur.Version != c.Version || cur.Status != "segmented" {
				t.Errorf("campaign = %+v", cur)
			}
		})
	}
}

// Errors before the stream starts are ordinary JSON error responses.
func TestAPIGenerateContentPreStreamErrors(t *testing.T) {
	f := newAPIFixture(t, nil)
	draft := f.create()
	assertEnvelope(t, f.do(http.MethodPost, "/api/v1/campaigns/"+draft.ID+"/content/generations", ""), 409, CodeInvalidState)
	assertEnvelope(t, f.do(http.MethodPost, "/api/v1/campaigns/00000000-0000-4000-8000-000000000000/content/generations", ""), 404, CodeNotFound)
	assertEnvelope(t, f.do(http.MethodPost, "/api/v1/campaigns/not-a-uuid/content/generations", ""), 422, CodeValidationFailed)

	c := f.segmented()
	assertEnvelope(t, f.do(http.MethodPost, "/api/v1/campaigns/"+c.ID+"/content/generations", `{"x":1}`), 422, CodeValidationFailed)
	assertEnvelope(t, f.do(http.MethodPost, "/api/v1/campaigns/"+c.ID+"/content/generations", `{`), 422, CodeValidationFailed)

	// No channels selected → 422 before any model call.
	rr := f.do(http.MethodPut, "/api/v1/campaigns/"+c.ID+"/channels", `{"channels":[],"expected_version":`+strconv.Itoa(c.Version)+`}`)
	if rr.Code != 200 {
		t.Fatal(rr.Body)
	}
	body := assertEnvelope(t, f.do(http.MethodPost, "/api/v1/campaigns/"+c.ID+"/content/generations", ""), 422, CodeValidationFailed)
	if !strings.Contains(body["details"].([]any)[0].(map[string]any)["path"].(string), "channels") {
		t.Errorf("details = %v", body["details"])
	}

	// pending_approval / approved: refused until Slice 3 can revoke approvals.
	for _, st := range []campaign.Status{campaign.StatusPendingApproval, campaign.StatusApproved, campaign.StatusSending, campaign.StatusCancelled} {
		c2 := f.segmented()
		id, _ := campaign.ParseID(c2.ID)
		stored, _ := f.store.Get(context.Background(), id)
		stored.Status = st
		f.store.Put(stored)
		assertEnvelope(t, f.do(http.MethodPost, "/api/v1/campaigns/"+c2.ID+"/content/generations", ""), 409, CodeInvalidState)
	}
	if f.llm.Calls() != 5 { // the five interpretations only
		t.Errorf("llm calls = %d", f.llm.Calls())
	}
}

// A client disconnect mid-stream cancels the model request; nothing is saved.
func TestAPIGenerateContentClientDisconnect(t *testing.T) {
	f := newAPIFixture(t, nil)
	c := f.segmented()
	started := make(chan struct{})
	cancelled := make(chan struct{})
	f.llm.Push(llm.Step{Func: func(ctx context.Context, _ llm.Request) (llm.Response, error) {
		close(started)
		<-ctx.Done()
		close(cancelled)
		return llm.Response{}, ctx.Err()
	}})
	handlerDone := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.router.ServeHTTP(w, r)
		close(handlerDone)
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, srv.URL+"/api/v1/campaigns/"+c.ID+"/content/generations", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	// Read until the meta event arrived (headers and first events are
	// flushed immediately).
	br := bufio.NewReader(resp.Body)
	line, err := br.ReadString('\n')
	if err != nil || line != "event: meta\n" {
		t.Fatalf("first line %q: %v", line, err)
	}
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("model call not started")
	}
	cancel()
	_ = resp.Body.Close()
	select {
	case <-cancelled:
	case <-time.After(5 * time.Second):
		t.Fatal("model call was not cancelled after the client disconnected")
	}
	// The handler finishes; nothing was persisted.
	select {
	case <-handlerDone:
	case <-time.After(5 * time.Second):
		t.Fatal("handler did not return after the disconnect")
	}
	if !strings.Contains(f.logs.String(), "content generation aborted") {
		t.Errorf("no abort log: %s", f.logs.String())
	}
	if len(f.versions(c.ID)) != 0 || f.store.Updates() != 1 { // the interpretation only
		t.Errorf("persisted after disconnect: versions=%d updates=%d", len(f.versions(c.ID)), f.store.Updates())
	}
}

func TestAPIValidateContent(t *testing.T) {
	f := newAPIFixture(t, nil)
	c := f.segmented()
	path := "/api/v1/campaigns/" + c.ID + "/content/validate"
	updates := f.store.Updates()

	// Valid draft: statistics and rendered example with fictional values.
	rr := f.do(http.MethodPost, path, okGen)
	if rr.Code != 200 {
		t.Fatalf("%d %s", rr.Code, rr.Body)
	}
	got := decodeJSON[struct {
		CampaignVersion int               `json:"campaign_version"`
		Channels        []string          `json:"channels"`
		Validation      validationResp    `json:"validation"`
		ExampleValues   map[string]string `json:"example_values"`
	}](t, rr.Body.Bytes())
	if !got.Validation.Valid || got.CampaignVersion != c.Version || got.Validation.SMS.CharsWorstCase == 0 ||
		got.Validation.RenderedExample == nil || got.ExampleValues["city"] != "İstanbul" {
		t.Errorf("valid = %s", rr.Body)
	}

	// Invalid content is a 200 with field errors (not a request error).
	long := strings.Repeat("ş", 150) + " {{first_name}}"
	rr = f.do(http.MethodPost, path, genReply(long, "Hi {{email}}", genBody))
	if rr.Code != 200 {
		t.Fatalf("%d %s", rr.Code, rr.Body)
	}
	v := decodeJSON[struct {
		Validation validationResp `json:"validation"`
	}](t, rr.Body.Bytes()).Validation
	if v.Valid || !hasField(v.Errors, "sms_body", "too_long") || !hasField(v.Errors, "email_subject", content.CodeUnknownVariable) ||
		v.RenderedExample != nil || v.SMS == nil || v.SMS.CharsWorstCase != 150+1+20 {
		t.Errorf("invalid = %s", rr.Body)
	}
	// Messages never echo the input.
	if strings.Contains(rr.Body.String(), strings.Repeat("ş", 20)) {
		t.Error("input echoed")
	}

	// Required fields of selected channels; fields of unselected channels.
	rr = f.do(http.MethodPost, path, `{"sms_body":"Hello"}`)
	v = decodeJSON[struct {
		Validation validationResp `json:"validation"`
	}](t, rr.Body.Bytes()).Validation
	if v.Valid || !hasField(v.Errors, "email_subject", "required") || !hasField(v.Errors, "email_body", "required") {
		t.Errorf("missing email = %s", rr.Body)
	}

	// Offer policy is advisory for marketer text.
	rr = f.do(http.MethodPost, path, genReply("Free shipping on your cart today!", genSubject, genBody))
	v = decodeJSON[struct {
		Validation validationResp `json:"validation"`
	}](t, rr.Body.Bytes()).Validation
	if !v.Valid || !hasField(v.Warnings, "sms_body", "unauthorized_offer") {
		t.Errorf("offer advisory = %s", rr.Body)
	}

	// Malformed requests are 422 validation_failed (distinct from content errors).
	for _, body := range []string{`{`, `[]`, `{"sms_body":1}`, `{"sms_body":"a","sms_body":"b"}`, `{"subject":"x"}`, ""} {
		assertEnvelope(t, f.do(http.MethodPost, path, body), 422, CodeValidationFailed)
	}
	if f.store.Updates() != updates || len(f.versions(c.ID)) != 0 {
		t.Error("validation wrote something")
	}

	// Channel selection is the campaign's: SMS only rejects email text.
	rr = f.do(http.MethodPut, "/api/v1/campaigns/"+c.ID+"/channels", `{"channels":["sms"],"expected_version":`+strconv.Itoa(c.Version)+`}`)
	if rr.Code != 200 {
		t.Fatal(rr.Body)
	}
	rr = f.do(http.MethodPost, path, okGen)
	v = decodeJSON[struct {
		Validation validationResp `json:"validation"`
	}](t, rr.Body.Bytes()).Validation
	if v.Valid || !hasField(v.Errors, "email_subject", campaign.CodeChannelNotSelected) {
		t.Errorf("unselected = %s", rr.Body)
	}
	assertEnvelope(t, f.do(http.MethodPost, "/api/v1/campaigns/"+f.create().ID+"/content/validate", okGen), 409, CodeInvalidState)
}

func (f *apiFixture) put(id string, draft string, version int) *httptest.ResponseRecorder {
	f.t.Helper()
	body := strings.TrimSuffix(draft, "}")
	if body != "{" {
		body += ","
	}
	return f.do(http.MethodPut, "/api/v1/campaigns/"+id+"/content", body+`"expected_version":`+strconv.Itoa(version)+`}`)
}

func TestAPISaveContent(t *testing.T) {
	f := newAPIFixture(t, nil)
	c := f.segmented()
	f.llm.Push(llm.Reply(okGen))
	_, evs := f.generateSSE(c.ID)
	ai := decodeJSON[savedResp](t, evs[len(evs)-1].Data)
	v1 := ai.Campaign.Version

	// An edit creates version 2 (origin edited, no model) and bumps the campaign.
	edited := genReply("Hi {{first_name|there}}! Your cart misses you: 30% off today.", genSubject, genBody)
	rr := f.put(c.ID, edited, v1)
	if rr.Code != 200 {
		t.Fatalf("%d %s", rr.Code, rr.Body)
	}
	s := decode[savedResp](t, rr)
	if !s.Created || s.Campaign.Version != v1+1 || s.Content.Content.Version != 2 || s.Content.Content.Origin != "edited" ||
		s.Content.Content.Model != nil || s.Content.Content.PromptVersion != nil || s.Content.CampaignVersion != v1+1 ||
		s.Campaign.Status != "content_ready" || !s.Content.Validation.Valid {
		t.Errorf("saved = %s", rr.Body)
	}

	// Unchanged canonical text (extra whitespace, decomposed ş) is a no-op.
	same := genReply("  Hi {{first_name|there}}! Your cart misses you: 30% off today.\n", genSubject, genBody)
	rr = f.put(c.ID, same, v1+1)
	s2 := decode[savedResp](t, rr)
	if rr.Code != 200 || s2.Created || s2.Campaign.Version != v1+1 || s2.Content.Content.ID != s.Content.Content.ID {
		t.Errorf("no-op = %d %s", rr.Code, rr.Body)
	}
	if len(f.versions(c.ID)) != 2 {
		t.Errorf("versions = %d", len(f.versions(c.ID)))
	}

	// Stale version → 409, nothing written.
	assertEnvelope(t, f.put(c.ID, genReply("Other text 30% off", genSubject, genBody), v1), 409, CodeVersionConflict)
	// Invalid content → 422 with field details; nothing written.
	body := assertEnvelope(t, f.put(c.ID, genReply("Hi {{email}}", genSubject, genBody), v1+1), 422, CodeValidationFailed)
	if d := body["details"].([]any); len(d) != 1 || d[0].(map[string]any)["code"] != content.CodeUnknownVariable {
		t.Errorf("details = %v", body["details"])
	}
	// Request shape errors.
	assertEnvelope(t, f.do(http.MethodPut, "/api/v1/campaigns/"+c.ID+"/content", okGen), 422, CodeValidationFailed)
	assertEnvelope(t, f.put(c.ID, `{"sms_body":5}`, v1+1), 422, CodeValidationFailed)
	if len(f.versions(c.ID)) != 2 {
		t.Errorf("versions = %d", len(f.versions(c.ID)))
	}

	// Versions are immutable: version 1 is still the AI text.
	vs := f.versions(c.ID)
	if sms, _ := vs[0].Content.SMSBody(); sms.String() != genSMS || vs[0].Provenance.Origin != content.OriginAI {
		t.Errorf("v1 = %+v", vs[0])
	}
}

func TestAPISaveContentStates(t *testing.T) {
	f := newAPIFixture(t, nil)
	// Manual authoring from scratch is allowed in segmented.
	c := f.segmented()
	rr := f.put(c.ID, okGen, c.Version)
	if s := decode[savedResp](t, rr); rr.Code != 200 || s.Content.Content.Version != 1 || s.Content.Content.Origin != "edited" {
		t.Fatalf("from scratch = %d %s", rr.Code, rr.Body)
	}
	assertEnvelope(t, f.put(f.create().ID, okGen, 1), 409, CodeInvalidState)
	for _, st := range []campaign.Status{campaign.StatusPendingApproval, campaign.StatusApproved, campaign.StatusSending, campaign.StatusCompleted} {
		c2 := f.segmented()
		id, _ := campaign.ParseID(c2.ID)
		stored, _ := f.store.Get(context.Background(), id)
		stored.Status = st
		f.store.Put(stored)
		assertEnvelope(t, f.put(c2.ID, okGen, stored.Version), 409, CodeInvalidState)
		if len(f.versions(c2.ID)) != 0 {
			t.Errorf("%s: version saved", st)
		}
	}
}

// GET /content validates the stored version for the current selection.
func TestAPIContentAfterChannelChange(t *testing.T) {
	f := newAPIFixture(t, nil)
	c := f.segmented()
	rr := f.put(c.ID, okGen, c.Version)
	s := decode[savedResp](t, rr)
	rr = f.do(http.MethodPut, "/api/v1/campaigns/"+c.ID+"/channels", `{"channels":["sms"],"expected_version":`+strconv.Itoa(s.Campaign.Version)+`}`)
	if rr.Code != 200 {
		t.Fatal(rr.Body)
	}
	got := decode[currentContentResp](t, f.do(http.MethodGet, "/api/v1/campaigns/"+c.ID+"/content", ""))
	if !got.Validation.Valid || !hasField(got.Validation.Warnings, "email_subject", campaign.CodeChannelNotSelected) ||
		got.Validation.RenderedExample.Email != nil || got.Content.EmailBody == nil {
		t.Errorf("sms only = %+v", got.Validation)
	}
}

// Offer-policy findings on marketer text are advisory warnings that survive
// validate → save → GET; they never block the save.
func TestAPIOfferWarningsSurviveSaveAndReload(t *testing.T) {
	f := newAPIFixture(t, nil)
	c := f.segmented() // content brief authorizes 30% off
	draftJSON := genReply("Free shipping and a coupon on your cart today!", genSubject, genBody)

	v := decodeJSON[struct {
		Validation validationResp `json:"validation"`
	}](t, f.do(http.MethodPost, "/api/v1/campaigns/"+c.ID+"/content/validate", draftJSON).Body.Bytes()).Validation
	if !v.Valid || !hasField(v.Warnings, "sms_body", "unauthorized_offer") {
		t.Fatalf("validate = %+v", v)
	}
	rr := f.put(c.ID, draftJSON, c.Version)
	s := decode[savedResp](t, rr)
	if rr.Code != 200 || !s.Created || !hasField(s.Content.Validation.Warnings, "sms_body", "unauthorized_offer") {
		t.Fatalf("save = %d %s", rr.Code, rr.Body)
	}
	got := decode[currentContentResp](t, f.do(http.MethodGet, "/api/v1/campaigns/"+c.ID+"/content", ""))
	if !hasField(got.Validation.Warnings, "sms_body", "unauthorized_offer") {
		t.Errorf("GET /content lost the warning: %+v", got.Validation)
	}
	// The same text from the model is blocked (repair, then failure).
	f.llm.Push(llm.Reply(draftJSON), llm.Reply(draftJSON))
	_, evs := f.generateSSE(c.ID)
	if last := evs[len(evs)-1]; last.Name != "error" || !strings.Contains(string(last.Data), CodeAIOutputInvalid) {
		t.Errorf("AI output with an unauthorized offer: %s %s", last.Name, last.Data)
	}
	if n := len(f.versions(c.ID)); n != 1 {
		t.Errorf("versions = %d", n)
	}
}

// disconnectAfterCommit cancels the client's request right after the
// version committed (before "validated" is written).
type disconnectAfterCommit struct {
	campaign.Store
	disconnect func()
}

func (s disconnectAfterCommit) SaveContent(ctx context.Context, id campaign.ID, m campaign.ContentMutation) (campaign.Campaign, content.Version, error) {
	c, v, err := s.Store.SaveContent(ctx, id, m)
	if err == nil {
		s.disconnect()
		select { // the server sees the closed connection
		case <-ctx.Done():
		case <-time.After(5 * time.Second):
		}
	}
	return c, v, err
}

// A disconnect after the commit never undoes the saved version.
func TestAPIGenerateContentDisconnectAfterCommitKeepsContent(t *testing.T) {
	f := newAPIFixture(t, nil)
	c := f.segmented()
	f.llm.Push(llm.Reply(okGen))
	clientCtx, cancelClient := context.WithCancel(context.Background())
	defer cancelClient()
	log, logs := newTestLogger()
	svc, err := campaign.NewService(disconnectAfterCommit{f.store, cancelClient}, f.audience, f.it, f.gen, log)
	if err != nil {
		t.Fatal(err)
	}
	handlerDone := make(chan struct{})
	router := NewRouter(log, Deps{Campaigns: svc})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		router.ServeHTTP(w, r)
		close(handlerDone)
	}))
	defer srv.Close()
	req, _ := http.NewRequestWithContext(clientCtx, http.MethodPost, srv.URL+"/api/v1/campaigns/"+c.ID+"/content/generations", nil)
	if resp, err := http.DefaultClient.Do(req); err == nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}
	select {
	case <-handlerDone:
	case <-time.After(5 * time.Second):
		t.Fatal("handler did not finish")
	}
	if n := len(f.versions(c.ID)); n != 1 {
		t.Fatalf("versions = %d", n)
	}
	cur := decode[campaignResp](t, f.do(http.MethodGet, "/api/v1/campaigns/"+c.ID, ""))
	if cur.Status != "content_ready" || cur.Version != c.Version+1 {
		t.Errorf("campaign = %+v", cur)
	}
	if strings.Contains(logs.String(), "content generation aborted") {
		t.Error("a committed generation was reported as aborted")
	}
}
