//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/fyusufayan/insider-campaign-composer-case/backend/internal/ai"
	"github.com/fyusufayan/insider-campaign-composer-case/backend/internal/approval"
	"github.com/fyusufayan/insider-campaign-composer-case/backend/internal/campaign"
	"github.com/fyusufayan/insider-campaign-composer-case/backend/internal/httpapi"
	"github.com/fyusufayan/insider-campaign-composer-case/backend/internal/llm"
	"github.com/fyusufayan/insider-campaign-composer-case/backend/internal/platform/db"
	"github.com/fyusufayan/insider-campaign-composer-case/backend/internal/platform/logging"
	"github.com/fyusufayan/insider-campaign-composer-case/backend/internal/segment"
	"github.com/fyusufayan/insider-campaign-composer-case/backend/test/integration/testsupport"
)

// ---- API integration tests: real HTTP server → campaign service →
// PostgreSQL repository / audience repository, with the scripted or demo LLM.

const (
	apiExampleBrief = "Send a short and friendly 30% discount reminder to users in Istanbul who added products to their cart in the last 30 days but did not complete a purchase."
	apiIstanbulRule = `{"version":1,"match":"all","conditions":[{"type":"attribute","field":"city","op":"eq","value":"istanbul"},{"type":"behavior","predicate":"abandoned_cart","within_days":30}]}`
	apiAnkaraRule   = `{"version":1,"match":"all","conditions":[{"type":"attribute","field":"city","op":"eq","value":"ankara"}]}`
	apiDeadline     = 10 * time.Second
	apiApprovedTTL  = 2 * time.Hour
)

func scriptedReply(segmentJSON string, channels string) string {
	return `{"segment":` + segmentJSON + `,"interpretation":"Scripted interpretation.","assumptions":[],"unsupported":[],` +
		`"suggested_channels":` + channels + `,` +
		`"content_brief":{"goal":"cart_recovery","tone":"friendly","length":"short","offer":{"kind":"percent_discount","value":30}}}`
}

var (
	replyIstanbul = scriptedReply(`{"match":"all","conditions":[{"type":"attribute","field":"city","op":"eq","value":"Istanbul"},{"type":"behavior","predicate":"abandoned_cart","within_days":30}]}`, `["sms","email"]`)
	// The same rule spelled differently: normalizes to apiIstanbulRule.
	replyIstanbulRespelled = scriptedReply(`{"match":"all","conditions":[{"type":"attribute","field":"city","op":"eq","value":"İSTANBUL"},{"type":"behavior","predicate":"abandoned_cart","within_days":30}]}`, `["email"]`)
	replyAnkara            = scriptedReply(`{"match":"all","conditions":[{"type":"attribute","field":"city","op":"eq","value":"ANKARA"}]}`, `["sms"]`)
	// Consent is backend-enforced: invalid on every attempt.
	replyInvalid = scriptedReply(`{"match":"all","conditions":[{"type":"attribute","field":"email_consent","op":"eq","value":"opted_in"}]}`, `["email"]`)
)

// Hand-computed audience of the 30-user fixture (segment_fixture_test.go) for
// İstanbul ∩ abandoned_cart(30) = users {1, 3, 5, 8}:
//
//	user 1: sms CI (eligible),     email CI (eligible)  → both
//	user 3: sms CO (no_consent),   email CI (eligible)  → email_only
//	user 5: sms -I (no_contact),   email CI (eligible)  → email_only
//	user 8: sms CI (eligible),     email C  (no_consent) → sms_only
var wantIstanbulAudience = segment.AudienceStats{
	MatchedUsers: 4,
	ByChannel: map[segment.Channel]segment.ChannelStats{
		segment.ChannelSMS:   {Eligible: 2, NoConsent: 1, NoContact: 1},
		segment.ChannelEmail: {Eligible: 3, NoConsent: 1, NoContact: 0},
	},
	ReachUsers: segment.ReachStats{Both: 1, SMSOnly: 1, EmailOnly: 2, Unreachable: 0},
	Messages:   5,
}

// apiEnv is one test's server and database.
type apiEnv struct {
	t      *testing.T
	pool   *pgxpool.Pool
	srv    *httptest.Server
	llm    *llm.Scripted
	svc    *campaign.Service
	appr   *approval.Service
	logs   *lockedBuffer
	dbURL  string
	tracer *sqlRecorder
}

// newAPIEnv starts the API on a fresh migrated database with the 30-user
// fixture. client nil = scripted.
func newAPIEnv(t *testing.T, client llm.Client) *apiEnv {
	t.Helper()
	e := &apiEnv{t: t, dbURL: pg.NewMigratedDatabase(t), tracer: &sqlRecorder{}}
	e.pool = tracedPool(t, e.dbURL, e.tracer)
	loadFixture(t, e.pool)
	if client == nil {
		e.llm = llm.NewScripted()
		client = e.llm
	}
	it, err := ai.NewInterpreter(client, ai.InterpreterConfig{Model: "integration-model", Effort: llm.EffortLow,
		Timeout: 30 * time.Second, MaxTokens: 1000})
	if err != nil {
		t.Fatal(err)
	}
	gen, err := ai.NewGenerator(client, ai.GeneratorConfig{Model: "integration-content-model", Effort: llm.EffortMedium,
		Timeout: 30 * time.Second, MaxTokens: 2000})
	if err != nil {
		t.Fatal(err)
	}
	// Wired as in cmd/api: approvals with lazy expiry on every campaign
	// operation (§5.3).
	e.logs = &lockedBuffer{}
	log := logging.New(e.logs, slog.LevelInfo)
	e.appr, err = approval.NewService(approval.NewRepository(e.pool), approval.Config{
		PendingTTL: prepTTL, ApprovedTTL: apiApprovedTTL, ApprovedBy: "reviewer@test"}, log)
	if err != nil {
		t.Fatal(err)
	}
	svc, err := campaign.NewService(campaign.NewRepository(e.pool), segment.NewAudienceRepository(e.pool), it, gen, log,
		campaign.WithApprovalExpirer(e.appr))
	if err != nil {
		t.Fatal(err)
	}
	e.svc = svc
	e.srv = httptest.NewServer(httpapi.NewRouter(log, httpapi.Deps{Campaigns: svc, Approvals: e.appr}))
	t.Cleanup(e.srv.Close)
	return e
}

// sqlRecorder records the SQL text of every statement while enabled.
type sqlRecorder struct {
	mu      sync.Mutex
	enabled bool
	stmts   []string
}

func (r *sqlRecorder) TraceQueryStart(ctx context.Context, _ *pgx.Conn, d pgx.TraceQueryStartData) context.Context {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.enabled {
		r.stmts = append(r.stmts, d.SQL)
	}
	return ctx
}

func (r *sqlRecorder) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func (r *sqlRecorder) start() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.enabled, r.stmts = true, nil
}

func (r *sqlRecorder) stop() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.enabled = false
	return append([]string(nil), r.stmts...)
}

func tracedPool(t *testing.T, dbURL string, tracer pgx.QueryTracer) *pgxpool.Pool {
	t.Helper()
	cfg, err := pgxpool.ParseConfig(dbURL)
	if err != nil {
		t.Fatal(err)
	}
	cfg.ConnConfig.RuntimeParams["timezone"] = "UTC"
	cfg.ConnConfig.RuntimeParams["application_name"] = "composer-api-test"
	cfg.ConnConfig.Tracer = tracer
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return pool
}

type apiResp struct {
	status int
	body   []byte
}

func (e *apiEnv) call(method, path, body string) apiResp {
	e.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), apiDeadline)
	defer cancel()
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, e.srv.URL+path, rd)
	if err != nil {
		e.t.Fatal(err)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		e.t.Fatal(err)
	}
	if resp.Header.Get(httpapi.RequestIDHeader) == "" {
		e.t.Errorf("%s %s: no request id", method, path)
	}
	// Contact data never appears in any API response.
	if s := string(b); strings.Contains(s, "@example.test") || strings.Contains(s, "+90555") {
		e.t.Errorf("%s %s: response contains contact data: %s", method, path, s)
	}
	return apiResp{resp.StatusCode, b}
}

type apiCampaign struct {
	ID             string          `json:"id"`
	Status         string          `json:"status"`
	Channels       []string        `json:"channels"`
	SegmentRule    json.RawMessage `json:"segment_rule"`
	SegmentVersion int             `json:"segment_version"`
	Interpretation *ai.Record      `json:"interpretation"`
	ContentBrief   json.RawMessage `json:"content_brief"`
	Version        int             `json:"version"`
	UpdatedAt      time.Time       `json:"updated_at"`
}

type apiInterpret struct {
	Campaign      apiCampaign            `json:"campaign"`
	Audience      *segment.AudienceStats `json:"audience"`
	AudienceError json.RawMessage        `json:"audience_error"`
}

type apiAudience struct {
	CampaignID      string                `json:"campaign_id"`
	CampaignVersion int                   `json:"campaign_version"`
	SegmentVersion  int                   `json:"segment_version"`
	Channels        []string              `json:"channels"`
	Audience        segment.AudienceStats `json:"audience"`
}

type apiError struct {
	Error struct {
		Code        string `json:"code"`
		Message     string `json:"message"`
		Retryable   bool   `json:"retryable"`
		RawProposal string `json:"raw_proposal"`
		Details     []struct {
			Path, Code string
		} `json:"details"`
	} `json:"error"`
}

func must[T any](t *testing.T, r apiResp, status int) T {
	t.Helper()
	if r.status != status {
		t.Fatalf("status = %d, want %d: %s", r.status, status, r.body)
	}
	var v T
	if err := json.Unmarshal(r.body, &v); err != nil {
		t.Fatalf("decode: %v: %s", err, r.body)
	}
	return v
}

func mustError(t *testing.T, r apiResp, status int, code string) apiError {
	t.Helper()
	e := must[apiError](t, r, status)
	if e.Error.Code != code {
		t.Fatalf("code = %q, want %q: %s", e.Error.Code, code, r.body)
	}
	return e
}

func (e *apiEnv) create(name string) apiCampaign {
	e.t.Helper()
	return must[apiCampaign](e.t, e.call(http.MethodPost, "/api/v1/campaigns",
		`{"name":"`+name+`","brief":"`+apiExampleBrief+`"}`), http.StatusCreated)
}

func (e *apiEnv) interpret(id string, reply string) apiInterpret {
	e.t.Helper()
	e.llm.Push(llm.Reply(reply))
	return must[apiInterpret](e.t, e.call(http.MethodPost, "/api/v1/campaigns/"+id+"/interpret", ""), http.StatusOK)
}

func (e *apiEnv) setChannels(id string, channels string, version int) apiResp {
	e.t.Helper()
	return e.call(http.MethodPut, "/api/v1/campaigns/"+id+"/channels",
		`{"channels":`+channels+`,"expected_version":`+strconv.Itoa(version)+`}`)
}

// rowJSON returns the whole stored campaign row (all columns) as JSON text.
func (e *apiEnv) rowJSON(id string) string {
	e.t.Helper()
	var s string
	if err := e.pool.QueryRow(context.Background(),
		`SELECT row_to_json(c)::text FROM campaigns AS c WHERE id = $1::uuid`, id).Scan(&s); err != nil {
		e.t.Fatal(err)
	}
	return s
}

// storedRule reads segment_rule from the database and re-encodes it through
// the validator (jsonb does not preserve key order).
func (e *apiEnv) storedRule(id string) (string, int, int, []string) {
	e.t.Helper()
	var (
		raw      []byte
		sv, ver  int
		channels []string
	)
	if err := e.pool.QueryRow(context.Background(),
		`SELECT segment_rule, segment_version, version, channels::text[] FROM campaigns WHERE id = $1::uuid`, id).
		Scan(&raw, &sv, &ver, &channels); err != nil {
		e.t.Fatal(err)
	}
	if raw == nil {
		return "", sv, ver, channels
	}
	v, err := segment.Parse(raw)
	if err != nil {
		e.t.Fatalf("stored rule invalid: %v", err)
	}
	b, _ := json.Marshal(v)
	return string(b), sv, ver, channels
}

func sameCounts(a, b segment.AudienceStats) bool {
	a.ComputedAt, b.ComputedAt = time.Time{}, time.Time{}
	ja, _ := json.Marshal(a)
	jb, _ := json.Marshal(b)
	return string(ja) == string(jb)
}

// ---- Tests ----

// Create → interpret (demo provider, no key) → preview → get, against the
// fixture with hand-computed expectations and the independently tested
// segment.AudienceRepository.
func TestAPIFlowWithDemoProvider(t *testing.T) {
	e := newAPIEnv(t, llm.NewDemo(ai.DemoResponders()))
	c := e.create("Cart recovery")
	if c.Status != "draft" || c.Version != 1 || c.SegmentVersion != 0 || len(c.Channels) != 0 || string(c.SegmentRule) != "null" {
		t.Fatalf("draft = %+v", c)
	}

	res := must[apiInterpret](t, e.call(http.MethodPost, "/api/v1/campaigns/"+c.ID+"/interpret", ""), 200)
	if res.Campaign.Status != "segmented" || res.Campaign.Version != 2 || res.Campaign.SegmentVersion != 1 ||
		strings.Join(res.Campaign.Channels, ",") != "sms,email" || string(res.Campaign.SegmentRule) != apiIstanbulRule {
		t.Fatalf("interpreted = %+v", res.Campaign)
	}
	if res.Campaign.Interpretation == nil || res.Campaign.Interpretation.Model != llm.DemoModel ||
		res.Campaign.Interpretation.PromptVersion != ai.SegmentPromptVersion {
		t.Errorf("interpretation = %+v", res.Campaign.Interpretation)
	}
	if res.Audience == nil || !sameCounts(*res.Audience, wantIstanbulAudience) {
		t.Fatalf("audience = %+v, want %+v", res.Audience, wantIstanbulAudience)
	}

	// Persisted, normalized and re-validated on readback.
	rule, sv, ver, channels := e.storedRule(c.ID)
	if rule != apiIstanbulRule || sv != 1 || ver != 2 || strings.Join(channels, ",") != "sms,email" {
		t.Errorf("stored = %s sv=%d v=%d ch=%v", rule, sv, ver, channels)
	}
	var offer int
	if err := e.pool.QueryRow(context.Background(),
		`SELECT (content_brief->'offer'->>'value')::int FROM campaigns WHERE id = $1::uuid`, c.ID).Scan(&offer); err != nil || offer != 30 {
		t.Errorf("content_brief offer = %d (%v)", offer, err)
	}

	// Preview agrees with the hand-computed partition and with the
	// segment repository run directly on the same rule and channels.
	before := e.rowJSON(c.ID)
	prev := must[apiAudience](t, e.call(http.MethodGet, "/api/v1/campaigns/"+c.ID+"/audience", ""), 200)
	if prev.CampaignID != c.ID || prev.CampaignVersion != 2 || prev.SegmentVersion != 1 || !sameCounts(prev.Audience, wantIstanbulAudience) {
		t.Errorf("preview = %+v", prev)
	}
	if prev.Audience.ComputedAt.IsZero() || time.Since(prev.Audience.ComputedAt) > time.Minute {
		t.Errorf("computed_at = %v", prev.Audience.ComputedAt)
	}
	direct, err := segment.NewAudienceRepository(e.pool).Stats(context.Background(),
		mustParse(t, apiIstanbulRule), []segment.Channel{segment.ChannelSMS, segment.ChannelEmail})
	if err != nil || !sameCounts(direct, prev.Audience) {
		t.Errorf("repository = %+v (%v), api = %+v", direct, err, prev.Audience)
	}

	// Reads change nothing (same row including updated_at) and create no
	// delivery rows.
	got := must[apiCampaign](t, e.call(http.MethodGet, "/api/v1/campaigns/"+c.ID, ""), 200)
	if got.Version != 2 || string(got.SegmentRule) != apiIstanbulRule {
		t.Errorf("GET = %+v", got)
	}
	if after := e.rowJSON(c.ID); after != before {
		t.Errorf("reads changed the row:\n%s\n%s", before, after)
	}
	var deliveries, approvals int
	_ = e.pool.QueryRow(context.Background(), `SELECT (SELECT count(*) FROM campaign_deliveries), (SELECT count(*) FROM campaign_approvals)`).
		Scan(&deliveries, &approvals)
	if deliveries != 0 || approvals != 0 {
		t.Errorf("preview created deliveries=%d approvals=%d", deliveries, approvals)
	}
}

func TestAPIVersionsAndChannels(t *testing.T) {
	e := newAPIEnv(t, nil)
	c := e.create("Versions")
	id := c.ID

	// Draft: no preview, no channel edits.
	mustError(t, e.call(http.MethodGet, "/api/v1/campaigns/"+id+"/audience", ""), 409, httpapi.CodeInvalidState)
	mustError(t, e.setChannels(id, `["sms"]`, 1), 409, httpapi.CodeInvalidState)

	r1 := e.interpret(id, replyIstanbul)
	if r1.Campaign.Version != 2 || r1.Campaign.SegmentVersion != 1 {
		t.Fatalf("first: %+v", r1.Campaign)
	}
	// Same normalized rule: segment_version stays, version advances, the
	// suggestion (["email"]) does not touch the channels.
	r2 := e.interpret(id, replyIstanbulRespelled)
	if r2.Campaign.Version != 3 || r2.Campaign.SegmentVersion != 1 || strings.Join(r2.Campaign.Channels, ",") != "sms,email" {
		t.Errorf("same rule: %+v", r2.Campaign)
	}
	// Different rule: segment_version advances.
	r3 := e.interpret(id, replyAnkara)
	if r3.Campaign.Version != 4 || r3.Campaign.SegmentVersion != 2 || string(r3.Campaign.SegmentRule) != apiAnkaraRule {
		t.Errorf("new rule: %+v", r3.Campaign)
	}

	// Validation and conflicts.
	mustError(t, e.setChannels(id, `["sms","fax"]`, 4), 422, httpapi.CodeValidationFailed)
	mustError(t, e.setChannels(id, `["sms"]`, 3), 409, httpapi.CodeVersionConflict)
	mustError(t, e.call(http.MethodGet, "/api/v1/campaigns/0b8f3e2a-1c4d-4e5f-8a9b-0c1d2e3f4a5b", ""), 404, httpapi.CodeNotFound)
	mustError(t, e.call(http.MethodGet, "/api/v1/campaigns/not-a-uuid", ""), 422, httpapi.CodeValidationFailed)

	// Explicit empty selection: matched users are preserved, no message.
	empty := must[apiCampaign](t, e.setChannels(id, `[]`, 4), 200)
	if empty.Version != 5 || len(empty.Channels) != 0 || empty.SegmentVersion != 2 || empty.Status != "segmented" {
		t.Errorf("empty channels: %+v", empty)
	}
	prev := must[apiAudience](t, e.call(http.MethodGet, "/api/v1/campaigns/"+id+"/audience", ""), 200)
	want := segment.AudienceStats{MatchedUsers: 5, ByChannel: map[segment.Channel]segment.ChannelStats{},
		ReachUsers: segment.ReachStats{Unreachable: 5}} // Ankara = users 11–15
	if !sameCounts(prev.Audience, want) || len(prev.Channels) != 0 {
		t.Errorf("empty-channel preview = %+v", prev)
	}

	// Re-interpretation never overwrites the explicit selection.
	r4 := e.interpret(id, replyIstanbul)
	if len(r4.Campaign.Channels) != 0 || r4.Campaign.Version != 6 || r4.Campaign.SegmentVersion != 3 ||
		r4.Audience == nil || r4.Audience.MatchedUsers != 4 || r4.Audience.Messages != 0 {
		t.Errorf("re-interpret: %+v audience %+v", r4.Campaign, r4.Audience)
	}
	_, sv, ver, channels := e.storedRule(id)
	if sv != 3 || ver != 6 || len(channels) != 0 {
		t.Errorf("stored sv=%d v=%d channels=%v", sv, ver, channels)
	}

	// SMS only: exact partition from the fixture.
	sms := must[apiCampaign](t, e.setChannels(id, `["sms"]`, 6), 200)
	prev = must[apiAudience](t, e.call(http.MethodGet, "/api/v1/campaigns/"+id+"/audience", ""), 200)
	want = segment.AudienceStats{MatchedUsers: 4,
		ByChannel:  map[segment.Channel]segment.ChannelStats{segment.ChannelSMS: {Eligible: 2, NoConsent: 1, NoContact: 1}},
		ReachUsers: segment.ReachStats{SMSOnly: 2, Unreachable: 2}, Messages: 2}
	if sms.Version != 7 || !sameCounts(prev.Audience, want) {
		t.Errorf("sms preview = %+v", prev.Audience)
	}
}

// Two (or more) concurrent updates with the same expected version: exactly
// one wins, the others get 409 and nothing is lost silently.
func TestAPIConcurrentChannelUpdates(t *testing.T) {
	e := newAPIEnv(t, nil)
	c := e.create("Concurrent")
	r := e.interpret(c.ID, replyIstanbul) // version 2, channels sms,email
	const n = 8
	selections := []string{`["sms"]`, `["email"]`, `[]`}
	type result struct {
		status int
		body   []byte
		sel    string
	}
	results := make(chan result, n)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(sel string) {
			defer wg.Done()
			<-start
			resp := e.setChannels(c.ID, sel, r.Campaign.Version)
			results <- result{resp.status, resp.body, sel}
		}(selections[i%len(selections)])
	}
	close(start)
	wg.Wait()
	close(results)
	ok, conflicts := 0, 0
	var winner string
	for res := range results {
		switch res.status {
		case 200:
			ok++
			winner = res.sel
		case 409:
			conflicts++
			if !strings.Contains(string(res.body), httpapi.CodeVersionConflict) {
				t.Errorf("409 body = %s", res.body)
			}
		default:
			t.Errorf("status %d: %s", res.status, res.body)
		}
	}
	if ok != 1 || conflicts != n-1 {
		t.Fatalf("ok = %d, conflicts = %d", ok, conflicts)
	}
	_, _, ver, channels := e.storedRule(c.ID)
	var wantCh []string
	_ = json.Unmarshal([]byte(winner), &wantCh)
	if ver != 3 || strings.Join(channels, ",") != strings.Join(wantCh, ",") {
		t.Errorf("stored version %d channels %v, winner %s", ver, channels, winner)
	}
}

// An edit that commits while the model is working makes the interpretation
// stale: it is rejected with 409 and not persisted. While the scripted LLM is
// blocked, no transaction is open and no campaign row lock is held.
func TestAPIInterpretRaceWithEdit(t *testing.T) {
	e := newAPIEnv(t, nil)
	c := e.create("Race")
	first := e.interpret(c.ID, replyIstanbul) // version 2

	monitor, err := pgx.Connect(context.Background(), e.dbURL)
	if err != nil {
		t.Fatal(err)
	}
	defer monitor.Close(context.Background())

	entered := make(chan struct{})
	release := make(chan struct{})
	e.llm.Push(llm.Step{Func: func(ctx context.Context, _ llm.Request) (llm.Response, error) {
		close(entered)
		select {
		case <-release:
		case <-ctx.Done():
			return llm.Response{}, ctx.Err()
		}
		return llm.Response{Text: replyAnkara, StopReason: llm.StopEndTurn, Model: "scripted-model"}, nil
	}})
	done := make(chan apiResp, 1)
	go func() { done <- e.call(http.MethodPost, "/api/v1/campaigns/"+c.ID+"/interpret", "") }()

	select {
	case <-entered:
	case <-time.After(apiDeadline):
		t.Fatal("the interpreter was not called")
	}

	// The model call is in progress: no other session of this database has
	// an open transaction or holds a lock on campaigns.
	var openTx, campaignLocks int
	if err := monitor.QueryRow(context.Background(), `
SELECT count(*) FROM pg_stat_activity
WHERE datname = current_database() AND pid <> pg_backend_pid()
  AND (xact_start IS NOT NULL OR state LIKE 'idle in transaction%')`).Scan(&openTx); err != nil {
		t.Fatal(err)
	}
	if err := monitor.QueryRow(context.Background(), `
SELECT count(*) FROM pg_locks AS l JOIN pg_class AS r ON r.oid = l.relation
WHERE r.relname = 'campaigns' AND l.pid <> pg_backend_pid()`).Scan(&campaignLocks); err != nil {
		t.Fatal(err)
	}
	if openTx != 0 || campaignLocks != 0 {
		t.Errorf("during the LLM call: open transactions = %d, campaign locks = %d", openTx, campaignLocks)
	}

	// The marketer's edit commits immediately (nothing blocks it).
	edit := must[apiCampaign](t, e.setChannels(c.ID, `["email"]`, first.Campaign.Version), 200)
	if edit.Version != 3 {
		t.Fatalf("edit = %+v", edit)
	}

	close(release)
	var res apiResp
	select {
	case res = <-done:
	case <-time.After(apiDeadline):
		t.Fatal("interpretation did not finish")
	}
	mustError(t, res, 409, httpapi.CodeVersionConflict)

	rule, sv, ver, channels := e.storedRule(c.ID)
	if rule != apiIstanbulRule || sv != 1 || ver != 3 || strings.Join(channels, ",") != "email" {
		t.Errorf("stale output persisted: rule %s sv=%d v=%d channels=%v", rule, sv, ver, channels)
	}
}

// Failed interpretations (two invalid proposals → 422 with field details;
// refusal; provider outage) leave the committed campaign untouched, and no
// statement touches the customer tables (decision Q2).
func TestAPIFailedInterpretationPreservesStateAndRunsNoAudienceSQL(t *testing.T) {
	e := newAPIEnv(t, nil)
	c := e.create("Failures")
	e.interpret(c.ID, replyIstanbul)
	before := e.rowJSON(c.ID)
	customerTables := regexp.MustCompile(`(?i)\b(users|channel_consents|cart_events|purchases)\b`)

	tests := []struct {
		name   string
		steps  []llm.Step
		status int
		code   string
	}{
		{"invalid twice", []llm.Step{llm.Reply(replyInvalid), llm.Reply(replyInvalid)}, 422, httpapi.CodeAIOutputInvalid},
		{"refusal", []llm.Step{llm.Stopped(replyAnkara, llm.StopRefusal)}, 422, httpapi.CodeLLMRefused},
		{"provider unavailable", []llm.Step{llm.Fail(&llm.Error{Kind: llm.ErrUnavailable, StatusCode: 503})}, 503, httpapi.CodeLLMUnavailable},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e.llm.Push(tt.steps...)
			e.tracer.start()
			res := e.call(http.MethodPost, "/api/v1/campaigns/"+c.ID+"/interpret", "")
			stmts := e.tracer.stop()

			apiErr := mustError(t, res, tt.status, tt.code)
			if tt.code == httpapi.CodeAIOutputInvalid {
				if len(apiErr.Error.Details) == 0 || apiErr.Error.Details[0].Path != "segment.conditions[0].field" ||
					apiErr.Error.Details[0].Code != segment.CodeConsentBackendEnforced || apiErr.Error.RawProposal != replyInvalid {
					t.Errorf("error = %+v", apiErr.Error)
				}
			}
			if len(stmts) == 0 {
				t.Fatal("tracer recorded nothing (the campaign read must be traced)")
			}
			for _, s := range stmts {
				if customerTables.MatchString(s) {
					t.Errorf("statement touched customer tables:\n%s", s)
				}
			}
			if after := e.rowJSON(c.ID); after != before {
				t.Errorf("campaign changed:\nbefore %s\nafter  %s", before, after)
			}
		})
	}
	if e.llm.Remaining() != 0 {
		t.Errorf("%d scripted steps unused", e.llm.Remaining())
	}
}

func mustParse(t *testing.T, js string) segment.Validated {
	t.Helper()
	v, err := segment.Parse([]byte(js))
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func TestReadinessReportsMigrationState(t *testing.T) {
	ctx := context.Background()
	migrated := testsupport.Pool(t, pg.NewMigratedDatabase(t))
	st, err := db.Readiness(ctx, migrated)
	if err != nil || st.Version == 0 || st.Dirty {
		t.Errorf("migrated: state %+v, err %v", st, err)
	}
	empty := testsupport.Pool(t, pg.NewDatabase(t))
	if _, err := db.Readiness(ctx, empty); err == nil {
		t.Error("an unmigrated database reported ready")
	}
}
