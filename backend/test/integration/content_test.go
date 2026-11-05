//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/fyusufayan/insider-campaign-composer-case/backend/internal/ai"
	"github.com/fyusufayan/insider-campaign-composer-case/backend/internal/campaign"
	"github.com/fyusufayan/insider-campaign-composer-case/backend/internal/llm"
)

// ---- CP12: AI content generation persisted as immutable versions in
// PostgreSQL (real repository, real schema, scripted or demo LLM).

const (
	cSMS     = "Hi {{first_name|there}}, your cart is waiting. Enjoy 30% off today."
	cSubject = "{{first_name|Hello}}, your cart is waiting: 30% off"
	cBody    = "Hi {{first_name|there}},\n\nYour cart is still waiting. Enjoy 30% off your order.\n\nSee you soon!"
)

func contentJSON(sms, subject, body string) string {
	m := map[string]string{}
	if sms != "" {
		m["sms_body"] = sms
	}
	if subject != "" {
		m["email_subject"], m["email_body"] = subject, body
	}
	b, _ := json.Marshal(m)
	return string(b)
}

var replyContentBoth = contentJSON(cSMS, cSubject, cBody)

// contentRow is a campaign_contents row.
type contentRow struct {
	ID                                int64
	Version                           int
	SMS, Subject, Body, Model, Prompt *string
	Origin                            string
}

func (e *apiEnv) contentRows(id string) []contentRow {
	e.t.Helper()
	rows, err := e.pool.Query(context.Background(), `
SELECT id, version, sms_body, email_subject, email_body, origin, model, prompt_version
FROM campaign_contents WHERE campaign_id = $1::uuid ORDER BY version`, id)
	if err != nil {
		e.t.Fatal(err)
	}
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (contentRow, error) {
		var c contentRow
		err := r.Scan(&c.ID, &c.Version, &c.SMS, &c.Subject, &c.Body, &c.Origin, &c.Model, &c.Prompt)
		return c, err
	})
	if err != nil {
		e.t.Fatal(err)
	}
	return out
}

func (e *apiEnv) contentRowJSON(id int64) string {
	e.t.Helper()
	var s string
	if err := e.pool.QueryRow(context.Background(),
		`SELECT row_to_json(c)::text FROM campaign_contents AS c WHERE id = $1`, id).Scan(&s); err != nil {
		e.t.Fatal(err)
	}
	return s
}

func (e *apiEnv) pointer(id string) (status string, version int, current *int64) {
	e.t.Helper()
	if err := e.pool.QueryRow(context.Background(),
		`SELECT status, version, current_content_id FROM campaigns WHERE id = $1::uuid`, id).Scan(&status, &version, &current); err != nil {
		e.t.Fatal(err)
	}
	return status, version, current
}

func (e *apiEnv) totalContentRows() int {
	e.t.Helper()
	var n int
	if err := e.pool.QueryRow(context.Background(), `SELECT count(*) FROM campaign_contents`).Scan(&n); err != nil {
		e.t.Fatal(err)
	}
	return n
}

func mustID(t *testing.T, s string) campaign.ID {
	t.Helper()
	id, err := campaign.ParseID(s)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func (e *apiEnv) generate(id string) campaign.ContentResult {
	e.t.Helper()
	res, err := e.svc.GenerateContent(context.Background(), mustID(e.t, id))
	if err != nil {
		e.t.Fatalf("generate: %v", err)
	}
	return res
}

func deref(p *string) string {
	if p == nil {
		return "<nil>"
	}
	return *p
}

// Generation inserts one immutable version and moves pointer, status and
// version together; regeneration inserts another and never touches the
// first. The segment, rule and audience preview are unaffected.
func TestContentGenerationPersistsImmutableVersions(t *testing.T) {
	e := newAPIEnv(t, nil)
	c := e.create("Content")
	in := e.interpret(c.ID, replyIstanbul) // segmented, version 2, sms+email
	ruleBefore, svBefore, _, _ := e.storedRule(c.ID)

	e.llm.Push(llm.Reply(replyContentBoth))
	e.tracer.start()
	res := e.generate(c.ID)
	stmts := e.tracer.stop()

	rows := e.contentRows(c.ID)
	if len(rows) != 1 {
		t.Fatalf("rows = %+v", rows)
	}
	r := rows[0]
	if r.Version != 1 || deref(r.SMS) != cSMS || deref(r.Subject) != cSubject || deref(r.Body) != cBody ||
		r.Origin != "ai" || deref(r.Model) != "scripted-model" || deref(r.Prompt) != ai.ContentPromptVersion {
		t.Errorf("row = %+v", r)
	}
	status, version, current := e.pointer(c.ID)
	if status != "content_ready" || version != in.Campaign.Version+1 || current == nil || *current != r.ID ||
		res.Version.ID != r.ID || res.Campaign.Version != version || *res.Campaign.CurrentContentID != r.ID {
		t.Errorf("campaign status=%s version=%d current=%v; result %+v", status, version, current, res.Campaign)
	}
	if rule, sv, _, _ := e.storedRule(c.ID); rule != ruleBefore || sv != svBefore {
		t.Errorf("segment changed: %s sv=%d", rule, sv)
	}

	// The generation transaction: lock the campaign first, then read/insert
	// content, then write the campaign — no UPDATE/DELETE of content rows.
	lock, insert := -1, -1
	for i, s := range stmts {
		switch {
		case strings.Contains(s, "FOR UPDATE") && strings.Contains(s, "FROM campaigns"):
			if lock < 0 {
				lock = i
			}
		case strings.HasPrefix(strings.TrimSpace(s), "INSERT INTO campaign_contents"):
			insert = i
		}
		if regexp.MustCompile(`(?i)(UPDATE|DELETE FROM)\s+campaign_contents`).MatchString(s) {
			t.Errorf("content row mutated: %s", s)
		}
	}
	if lock < 0 || insert < lock {
		t.Errorf("lock at %d, insert at %d:\n%s", lock, insert, strings.Join(stmts, "\n---\n"))
	}

	// Read back through content.Parse.
	_, cv, err := e.svc.CurrentContent(context.Background(), mustID(t, c.ID))
	if err != nil || cv == nil || cv.ID != r.ID || !cv.Content.Equal(res.Version.Content) ||
		cv.Provenance != res.Version.Provenance || cv.CreatedAt.IsZero() {
		t.Fatalf("read back = %+v, %v", cv, err)
	}

	// Regenerate (identical text): a new version, the first row unchanged.
	first := e.contentRowJSON(r.ID)
	e.llm.Push(llm.Reply(replyContentBoth))
	res2 := e.generate(c.ID)
	rows = e.contentRows(c.ID)
	if len(rows) != 2 || rows[1].Version != 2 || rows[1].ID != res2.Version.ID || deref(rows[1].SMS) != cSMS {
		t.Fatalf("rows = %+v", rows)
	}
	if e.contentRowJSON(r.ID) != first {
		t.Error("version 1 row changed")
	}
	if _, v2, cur := e.pointer(c.ID); v2 != version+1 || *cur != rows[1].ID {
		t.Errorf("pointer after regeneration: version=%d current=%d", v2, *cur)
	}

	// The audience preview is unchanged and still allowed in content_ready.
	aud := must[apiAudience](t, e.call(http.MethodGet, "/api/v1/campaigns/"+c.ID+"/audience", ""), 200)
	if !sameCounts(aud.Audience, wantIstanbulAudience) || aud.CampaignVersion != version+1 || aud.SegmentVersion != svBefore {
		t.Errorf("audience = %+v", aud)
	}
	// The campaign DTO keeps its contract.
	got := must[apiCampaign](t, e.call(http.MethodGet, "/api/v1/campaigns/"+c.ID, ""), 200)
	if got.Status != "content_ready" || got.Version != version+1 {
		t.Errorf("campaign DTO = %+v", got)
	}
}

// Demo provider (no key, no network): interpretation and generation end to
// end; the version is stored with model "demo".
func TestContentGenerationWithDemoProvider(t *testing.T) {
	e := newAPIEnv(t, llm.NewDemo(ai.DemoResponders()))
	c := e.create("Demo content")
	in := must[apiInterpret](t, e.call(http.MethodPost, "/api/v1/campaigns/"+c.ID+"/interpret", ""), 200)
	if !slices.Equal(in.Campaign.Channels, []string{"sms", "email"}) {
		t.Fatalf("channels = %v", in.Campaign.Channels)
	}
	res := e.generate(c.ID)
	if res.Version.Provenance.Model != llm.DemoModel || !slices.Equal(res.Notes, []string{ai.DemoContentNotice}) {
		t.Errorf("result = %+v", res)
	}
	rows := e.contentRows(c.ID)
	if len(rows) != 1 || deref(rows[0].Model) != "demo" || rows[0].SMS == nil || rows[0].Subject == nil {
		t.Fatalf("rows = %+v", rows)
	}
	// The example brief authorizes 30%: exactly that offer is mentioned.
	if !strings.Contains(*rows[0].SMS, "30% off") || strings.Contains(strings.ToLower(*rows[0].Body), "free shipping") {
		t.Errorf("sms = %q body = %q", *rows[0].SMS, *rows[0].Body)
	}

	// Email only → only email content in the next version.
	cur := must[apiCampaign](t, e.call(http.MethodGet, "/api/v1/campaigns/"+c.ID, ""), 200)
	must[apiCampaign](t, e.setChannels(c.ID, `["email"]`, cur.Version), 200)
	e.generate(c.ID)
	rows = e.contentRows(c.ID)
	if len(rows) != 2 || rows[1].SMS != nil || rows[1].Subject == nil || rows[1].Body == nil {
		t.Errorf("email-only version = %+v", rows[1])
	}
}

// No customer data (names, emails, phones of the fixture users) reaches the
// model, even for users who are in the audience.
func TestContentGenerationSendsNoCustomerData(t *testing.T) {
	e := newAPIEnv(t, nil)
	if _, err := e.pool.Exec(context.Background(),
		`UPDATE users SET first_name = 'Zeynepcan', last_name = 'Karabulutoglu' WHERE id IN (1, 3, 5, 8)`); err != nil {
		t.Fatal(err)
	}
	c := e.create("Privacy")
	e.interpret(c.ID, replyIstanbul)
	e.llm.Push(llm.Reply(replyContentBoth))
	e.generate(c.ID)

	var secrets []string
	rows, err := e.pool.Query(context.Background(), `SELECT first_name, last_name, coalesce(email, ''), coalesce(phone, '') FROM users`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var f, l, em, ph string
		if err := rows.Scan(&f, &l, &em, &ph); err != nil {
			t.Fatal(err)
		}
		for _, s := range []string{f, l, em, ph} {
			if len(s) > 4 { // skip the generic fixture names "Test"/"User"
				secrets = append(secrets, s)
			}
		}
	}
	rows.Close()
	reqs := e.llm.Requests()
	gen := reqs[len(reqs)-1]
	if gen.Task != ai.TaskContentGeneration {
		t.Fatalf("last request = %s", gen.Task)
	}
	text := gen.System
	for _, m := range gen.Messages {
		text += "\n" + m.Content
	}
	for _, s := range append(secrets, "@example.test", "+90555") {
		if strings.Contains(text, s) {
			t.Errorf("prompt contains customer data %q", s)
		}
	}
}

// While the model works no transaction is open and no campaign lock is
// held; an edit that commits meanwhile makes the result stale and nothing is
// inserted.
func TestContentGenerationRaceWithEdit(t *testing.T) {
	e := newAPIEnv(t, nil)
	c := e.create("Race")
	in := e.interpret(c.ID, replyIstanbul)

	monitor, err := pgx.Connect(context.Background(), e.dbURL)
	if err != nil {
		t.Fatal(err)
	}
	defer monitor.Close(context.Background())

	entered, release := make(chan struct{}), make(chan struct{})
	e.llm.Push(llm.Step{Func: func(ctx context.Context, _ llm.Request) (llm.Response, error) {
		close(entered)
		select {
		case <-release:
		case <-ctx.Done():
			return llm.Response{}, ctx.Err()
		}
		return llm.Response{Text: replyContentBoth, StopReason: llm.StopEndTurn, Model: "scripted-model"}, nil
	}})
	done := make(chan error, 1)
	go func() {
		_, err := e.svc.GenerateContent(context.Background(), mustID(t, c.ID))
		done <- err
	}()
	select {
	case <-entered:
	case <-time.After(apiDeadline):
		t.Fatal("the generator was not called")
	}

	var openTx, campaignLocks int
	if err := monitor.QueryRow(context.Background(), `
SELECT count(*) FROM pg_stat_activity
WHERE datname = current_database() AND pid <> pg_backend_pid()
  AND (xact_start IS NOT NULL OR state LIKE 'idle in transaction%')`).Scan(&openTx); err != nil {
		t.Fatal(err)
	}
	if err := monitor.QueryRow(context.Background(), `
SELECT count(*) FROM pg_locks AS l JOIN pg_class AS r ON r.oid = l.relation
WHERE r.relname IN ('campaigns', 'campaign_contents') AND l.pid <> pg_backend_pid()`).Scan(&campaignLocks); err != nil {
		t.Fatal(err)
	}
	if openTx != 0 || campaignLocks != 0 {
		t.Errorf("during the LLM call: open transactions = %d, locks = %d", openTx, campaignLocks)
	}

	edit := must[apiCampaign](t, e.setChannels(c.ID, `["email"]`, in.Campaign.Version), 200)
	close(release)
	select {
	case err = <-done:
	case <-time.After(apiDeadline):
		t.Fatal("generation did not finish")
	}
	var ce *campaign.ConflictError
	if !errors.As(err, &ce) || ce.Current != edit.Version {
		t.Fatalf("err = %v", err)
	}
	status, version, current := e.pointer(c.ID)
	if status != "segmented" || version != edit.Version || current != nil || len(e.contentRows(c.ID)) != 0 {
		t.Errorf("stale content persisted: status=%s version=%d current=%v", status, version, current)
	}
}

// Concurrent generations never produce inconsistent versions: numbers are
// contiguous, the pointer is the newest row, and the campaign version moved
// once per stored version.
func TestContentGenerationConcurrent(t *testing.T) {
	e := newAPIEnv(t, nil)
	c := e.create("Concurrent")
	in := e.interpret(c.ID, replyIstanbul)
	reply := func(ctx context.Context, _ llm.Request) (llm.Response, error) {
		return llm.Response{Text: replyContentBoth, StopReason: llm.StopEndTurn, Model: "scripted-model"}, nil
	}

	// conflictsFrom counts conflicts per observed (expected) version.
	conflictsFrom := map[int]int{}
	run := func(n int, barrier bool) (ok int) {
		var ready sync.WaitGroup
		ready.Add(n)
		release := make(chan struct{})
		for range n {
			e.llm.Push(llm.Step{Func: func(ctx context.Context, r llm.Request) (llm.Response, error) {
				if barrier {
					ready.Done()
					select {
					case <-release:
					case <-ctx.Done():
						return llm.Response{}, ctx.Err()
					}
				}
				return reply(ctx, r)
			}})
		}
		errs := make(chan error, n)
		for range n {
			go func() {
				_, err := e.svc.GenerateContent(context.Background(), mustID(t, c.ID))
				errs <- err
			}()
		}
		if barrier {
			ready.Wait()
			close(release)
		}
		for range n {
			switch err := <-errs; {
			case err == nil:
				ok++
			case errors.Is(err, campaign.ErrVersionConflict):
				var ce *campaign.ConflictError
				if errors.As(err, &ce) && barrier {
					conflictsFrom[ce.Expected]++
				}
			default:
				t.Errorf("err = %v", err)
			}
		}
		return ok
	}

	// Stale shared version: every generation reads the campaign before any
	// model call returns (the barrier is inside the model call), so all 8
	// observed the same version. Exactly one persists; the other 7 are
	// conflicts against that one observed version.
	if ok := run(8, true); ok != 1 {
		t.Errorf("barrier round: %d stored", ok)
	}
	if len(conflictsFrom) != 1 || conflictsFrom[in.Campaign.Version] != 7 {
		t.Errorf("barrier round conflicts by observed version = %v, want {%d: 7}", conflictsFrom, in.Campaign.Version)
	}
	// Free-running: any interleaving (some start from fresh versions after
	// earlier ones commit, some share a version); only consistency is
	// asserted.
	ok := 1 + run(8, false)

	rows := e.contentRows(c.ID)
	if len(rows) != ok {
		t.Fatalf("%d successes, %d rows", ok, len(rows))
	}
	for i, r := range rows {
		if r.Version != i+1 {
			t.Errorf("row %d has version %d", i, r.Version)
		}
	}
	_, version, current := e.pointer(c.ID)
	if current == nil || *current != rows[len(rows)-1].ID || version != in.Campaign.Version+ok {
		t.Errorf("version=%d current=%v rows=%+v", version, current, rows)
	}
}

// Failed generations (invalid twice, refusal, outage) insert nothing and
// leave the campaign row byte-for-byte unchanged.
func TestContentGenerationFailuresInsertNothing(t *testing.T) {
	e := newAPIEnv(t, nil)
	c := e.create("Failures")
	e.interpret(c.ID, replyIstanbul)
	before := e.rowJSON(c.ID)
	invalid := contentJSON("Hi {{email}}, 50% off", cSubject, "<b>"+cBody+"</b>")
	for _, tt := range []struct {
		name  string
		steps []llm.Step
		kind  error
	}{
		{"invalid twice", []llm.Step{llm.Reply(invalid), llm.Reply(invalid)}, ai.ErrOutputInvalid},
		{"unknown field", []llm.Step{llm.Reply(`{"sms_body":"x","email_subject":"y","email_body":"z","note":1}`), llm.Reply(invalid)}, ai.ErrOutputInvalid},
		{"refusal", []llm.Step{llm.Stopped(replyContentBoth, llm.StopRefusal)}, ai.ErrLLMRefused},
		{"unavailable", []llm.Step{llm.Fail(&llm.Error{Kind: llm.ErrUnavailable, StatusCode: 503})}, ai.ErrLLMUnavailable},
	} {
		t.Run(tt.name, func(t *testing.T) {
			e.llm.Push(tt.steps...)
			_, err := e.svc.GenerateContent(context.Background(), mustID(t, c.ID))
			if !errors.Is(err, tt.kind) {
				t.Fatalf("err = %v", err)
			}
			if n := e.totalContentRows(); n != 0 {
				t.Errorf("%d content rows", n)
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

// A failure after the content row was inserted rolls back both the row and
// the campaign pointer; the next generation is version 1 again (no orphan).
func TestContentGenerationRollbackProtectsRowAndPointer(t *testing.T) {
	e := newAPIEnv(t, nil)
	c := e.create("Rollback")
	e.interpret(c.ID, replyIstanbul)
	before := e.rowJSON(c.ID)
	ctx := context.Background()
	if _, err := e.pool.Exec(ctx, `
CREATE FUNCTION test_fail_content_pointer() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  IF NEW.current_content_id IS DISTINCT FROM OLD.current_content_id THEN
    RAISE EXCEPTION 'injected failure after the content insert';
  END IF;
  RETURN NEW;
END $$;
CREATE TRIGGER test_fail_content_pointer BEFORE UPDATE ON campaigns
  FOR EACH ROW EXECUTE FUNCTION test_fail_content_pointer();`); err != nil {
		t.Fatal(err)
	}
	e.llm.Push(llm.Reply(replyContentBoth))
	e.tracer.start()
	_, err := e.svc.GenerateContent(ctx, mustID(t, c.ID))
	stmts := e.tracer.stop()
	if err == nil || !strings.Contains(err.Error(), "injected failure") {
		t.Fatalf("err = %v", err)
	}
	inserted := false
	for _, s := range stmts {
		inserted = inserted || strings.HasPrefix(strings.TrimSpace(s), "INSERT INTO campaign_contents")
	}
	if !inserted {
		t.Fatal("the failure did not happen after the insert")
	}
	if n := e.totalContentRows(); n != 0 {
		t.Errorf("orphaned content rows: %d", n)
	}
	if after := e.rowJSON(c.ID); after != before {
		t.Errorf("campaign changed:\nbefore %s\nafter  %s", before, after)
	}

	if _, err := e.pool.Exec(ctx, `DROP TRIGGER test_fail_content_pointer ON campaigns`); err != nil {
		t.Fatal(err)
	}
	e.llm.Push(llm.Reply(replyContentBoth))
	res := e.generate(c.ID)
	if res.Version.Number != 1 || len(e.contentRows(c.ID)) != 1 {
		t.Errorf("after recovery: %+v", res.Version)
	}
}

// Stored content is re-validated on read: a tampered row is never returned
// or built upon.
func TestContentReadBackRevalidates(t *testing.T) {
	e := newAPIEnv(t, nil)
	c := e.create("Tamper")
	e.interpret(c.ID, replyIstanbul)
	e.llm.Push(llm.Reply(replyContentBoth))
	res := e.generate(c.ID)
	ctx := context.Background()
	if _, err := e.pool.Exec(ctx, `UPDATE campaign_contents SET sms_body = 'Hi {{email}}' WHERE id = $1`, res.Version.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := e.svc.CurrentContent(ctx, mustID(t, c.ID)); err == nil {
		t.Error("tampered content returned")
	}
	e.llm.Push(llm.Reply(replyContentBoth))
	if _, err := e.svc.GenerateContent(ctx, mustID(t, c.ID)); err == nil {
		t.Error("generation built on tampered content")
	}
	if n := len(e.contentRows(c.ID)); n != 1 {
		t.Errorf("rows = %d", n)
	}
}

// Statuses that would need approval revocation, or that are final, refuse
// generation without calling the model.
func TestContentGenerationRefusedStatuses(t *testing.T) {
	e := newAPIEnv(t, nil)
	draft := e.create("Draft")
	if _, err := e.svc.GenerateContent(context.Background(), mustID(t, draft.ID)); !errors.Is(err, campaign.ErrInvalidState) {
		t.Errorf("draft: err = %v", err)
	}
	// pending_approval / approved through real approvals (a raw status
	// update would fabricate a live status without an approval, which lazy
	// expiry rightly reports as inconsistent).
	r := e.readyViaAPI(prepSMS, prepSubject, prepBody)
	prep := e.prepareViaAPI(r.ID, r.Version)
	check := func(name, id string) {
		t.Helper()
		calls := e.llm.Calls()
		_, err := e.svc.GenerateContent(context.Background(), mustID(t, id))
		if !errors.Is(err, campaign.ErrInvalidState) || e.llm.Calls() != calls {
			t.Errorf("%s: err = %v, model calls %d → %d", name, err, calls, e.llm.Calls())
		}
	}
	check("pending_approval", r.ID)
	must[apiApprovalResp](t, e.call("POST", approvalPath(r.ID, prep.Approval.ApprovalID, "confirm"), confirmBody(prep.Campaign.Version, prep.Approval)), 200)
	check("approved", r.ID)
	if n := len(e.contentRows(r.ID)); n != 1 {
		t.Errorf("approval campaign content rows = %d", n)
	}

	c := e.create("States")
	e.interpret(c.ID, replyIstanbul)
	for _, st := range []string{"sending", "completed", "cancelled"} {
		if _, err := e.pool.Exec(context.Background(), `UPDATE campaigns SET status = $2 WHERE id = $1::uuid`, c.ID, st); err != nil {
			t.Fatal(err)
		}
		calls := e.llm.Calls()
		_, err := e.svc.GenerateContent(context.Background(), mustID(t, c.ID))
		if !errors.Is(err, campaign.ErrInvalidState) || e.llm.Calls() != calls {
			t.Errorf("%s: err = %v, model calls %d → %d", st, err, calls, e.llm.Calls())
		}
	}
	if n := len(e.contentRows(c.ID)); n != 0 {
		t.Errorf("%d content rows", n)
	}
	if _, cv, err := e.svc.CurrentContent(context.Background(), mustID(t, c.ID)); err != nil || cv != nil {
		t.Errorf("current content without versions: %+v, %v", cv, err)
	}
}
