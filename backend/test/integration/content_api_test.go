//go:build integration

package integration

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/fyusufayan/insider-campaign-composer-case/backend/internal/llm"
)

// ---- CP13: content HTTP API over real HTTP and PostgreSQL: the SSE
// generation stream, manual saves, no-op saves, conflicts, immutability.

type apiContentVersion struct {
	ID           int64   `json:"id"`
	Version      int     `json:"version"`
	Origin       string  `json:"origin"`
	Model        *string `json:"model"`
	SMSBody      *string `json:"sms_body"`
	EmailSubject *string `json:"email_subject"`
	EmailBody    *string `json:"email_body"`
}

type apiCurrentContent struct {
	CampaignVersion int                `json:"campaign_version"`
	Content         *apiContentVersion `json:"content"`
	Validation      *struct {
		Valid  bool `json:"valid"`
		Errors []struct {
			Path, Code string
		} `json:"errors"`
	} `json:"validation"`
}

type apiSaved struct {
	Campaign apiCampaign       `json:"campaign"`
	Content  apiCurrentContent `json:"content"`
	Created  bool              `json:"created"`
}

type streamEvent struct {
	name string
	data json.RawMessage
}

// streamGeneration posts to the generation route and reads the event stream
// as it arrives. At the terminal event it calls atTerminal (before reading
// on), so the database can be inspected at the moment the client sees the
// result.
func (e *apiEnv) streamGeneration(id string, atTerminal func(streamEvent)) []streamEvent {
	e.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), apiDeadline)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, e.srv.URL+"/api/v1/campaigns/"+id+"/content/generations", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		e.t.Fatalf("status %d %s", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	var out []streamEvent
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(nil, 1<<20)
	var cur streamEvent
	for sc.Scan() {
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, "event: "):
			cur.name = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			cur.data = json.RawMessage(strings.TrimPrefix(line, "data: "))
		case line == "" && cur.name != "":
			out = append(out, cur)
			if (cur.name == "validated" || cur.name == "error") && atTerminal != nil {
				atTerminal(cur)
			}
			cur = streamEvent{}
		}
	}
	if s := func() string {
		var b strings.Builder
		for _, ev := range out {
			b.Write(ev.data)
		}
		return b.String()
	}(); strings.Contains(s, "@example.test") || strings.Contains(s, "+90555") {
		e.t.Errorf("stream contains contact data")
	}
	return out
}

func (e *apiEnv) putContent(id, draftJSON string, version int) apiResp {
	e.t.Helper()
	body := strings.TrimSuffix(draftJSON, "}") + `,"expected_version":` + strconv.Itoa(version) + `}`
	return e.call(http.MethodPut, "/api/v1/campaigns/"+id+"/content", body)
}

func TestContentAPIStreamCommitsBeforeValidated(t *testing.T) {
	e := newAPIEnv(t, nil)
	c := e.create("Stream")
	in := e.interpret(c.ID, replyIstanbul)
	e.llm.Push(llm.Step{Response: llm.Response{Text: replyContentBoth, StopReason: llm.StopEndTurn},
		Chunks: []string{replyContentBoth[:5], replyContentBoth[5:40], replyContentBoth[40:]}})

	var rowsAtTerminal int
	var statusAtTerminal string
	evs := e.streamGeneration(c.ID, func(streamEvent) {
		// The client sees the result only after the transaction committed.
		rowsAtTerminal = len(e.contentRows(c.ID))
		statusAtTerminal, _, _ = e.pointer(c.ID)
	})
	if last := evs[len(evs)-1]; last.name != "validated" {
		t.Fatalf("terminal = %s %s", last.name, last.data)
	}
	if rowsAtTerminal != 1 || statusAtTerminal != "content_ready" {
		t.Errorf("at validated: rows = %d, status = %s", rowsAtTerminal, statusAtTerminal)
	}
	var saved apiSaved
	if err := json.Unmarshal(evs[len(evs)-1].data, &saved); err != nil {
		t.Fatal(err)
	}
	status, version, cur := e.pointer(c.ID)
	if saved.Campaign.Version != version || version != in.Campaign.Version+1 || status != "content_ready" ||
		cur == nil || *cur != saved.Content.Content.ID || saved.Content.Content.Origin != "ai" {
		t.Errorf("saved = %+v; db = %s %d %v", saved, status, version, cur)
	}
	got := must[apiCurrentContent](t, e.call(http.MethodGet, "/api/v1/campaigns/"+c.ID+"/content", ""), 200)
	if got.Content == nil || got.Content.ID != *cur || deref(got.Content.SMSBody) != cSMS || !got.Validation.Valid {
		t.Errorf("GET content = %+v", got)
	}
}

func TestContentAPIFailedStreamWritesNothing(t *testing.T) {
	e := newAPIEnv(t, nil)
	c := e.create("Stream fail")
	in := e.interpret(c.ID, replyIstanbul)
	e.llm.Push(llm.Reply(contentJSON("Hi {{email}}", cSubject, cBody)), llm.Reply(contentJSON("Hi {{phone}}", cSubject, cBody)))
	evs := e.streamGeneration(c.ID, nil)
	var ev struct {
		Status int `json:"status"`
		Error  struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if last := evs[len(evs)-1]; last.name != "error" || json.Unmarshal(last.data, &ev) != nil || ev.Error.Code != "ai_output_invalid" || ev.Status != 422 {
		t.Fatalf("terminal = %s %s", last.name, last.data)
	}
	if n := len(e.contentRows(c.ID)); n != 0 {
		t.Errorf("rows = %d", n)
	}
	if _, v, cur := e.pointer(c.ID); v != in.Campaign.Version || cur != nil {
		t.Errorf("campaign moved: %d %v", v, cur)
	}
}

// Manual saves: new immutable versions, no-op for unchanged canonical text,
// stale versions rejected, earlier rows byte-for-byte unchanged.
func TestContentAPIManualSaves(t *testing.T) {
	e := newAPIEnv(t, nil)
	c := e.create("Edits")
	in := e.interpret(c.ID, replyIstanbul)
	e.llm.Push(llm.Reply(replyContentBoth))
	gen := e.generate(c.ID)
	v1JSON := e.contentRowJSON(gen.Version.ID)

	edited := contentJSON("Hi {{first_name|there}}! Your İstanbul cart: 30% off today.", cSubject, cBody)
	s := must[apiSaved](t, e.putContent(c.ID, edited, gen.Campaign.Version), 200)
	if !s.Created || s.Content.Content.Version != 2 || s.Content.Content.Origin != "edited" || s.Content.Content.Model != nil ||
		s.Campaign.Version != gen.Campaign.Version+1 {
		t.Fatalf("edit = %+v", s)
	}
	rows := e.contentRows(c.ID)
	if len(rows) != 2 || rows[1].Origin != "edited" || rows[1].Model != nil || rows[1].Prompt != nil {
		t.Fatalf("rows = %+v", rows)
	}
	if e.contentRowJSON(gen.Version.ID) != v1JSON {
		t.Error("version 1 changed")
	}

	// Decomposed "İ" (I + U+0307) and surrounding whitespace are the same
	// canonical text: no new row, version unchanged.
	decomposed := strings.Replace(edited, "İ", "İ", 1)
	same := must[apiSaved](t, e.putContent(c.ID, " "+decomposed, s.Campaign.Version), 200)
	if same.Created || same.Campaign.Version != s.Campaign.Version || same.Content.Content.ID != s.Content.Content.ID {
		t.Errorf("no-op = %+v", same)
	}
	if n := len(e.contentRows(c.ID)); n != 2 {
		t.Errorf("rows after no-op = %d", n)
	}

	// Stale version and invalid content write nothing.
	mustError(t, e.putContent(c.ID, contentJSON("Another 30% off text", cSubject, cBody), gen.Campaign.Version), 409, "version_conflict")
	inv := mustError(t, e.putContent(c.ID, contentJSON(strings.Repeat("x", 161), cSubject, cBody), s.Campaign.Version), 422, "validation_failed")
	if len(inv.Error.Details) == 0 || inv.Error.Details[0].Code != "too_long" {
		t.Errorf("details = %+v", inv.Error.Details)
	}
	if _, v, _ := e.pointer(c.ID); v != s.Campaign.Version || len(e.contentRows(c.ID)) != 2 {
		t.Errorf("state moved: v=%d rows=%d", v, len(e.contentRows(c.ID)))
	}

	// Validation is read-only.
	before := e.totalContentRows()
	r := e.call(http.MethodPost, "/api/v1/campaigns/"+c.ID+"/content/validate", contentJSON("Hi {{nope}}", cSubject, cBody))
	if r.status != 200 || !strings.Contains(string(r.body), `"unknown_variable"`) {
		t.Errorf("validate = %d %s", r.status, r.body)
	}
	if _, v, _ := e.pointer(c.ID); v != s.Campaign.Version || e.totalContentRows() != before {
		t.Error("validate wrote")
	}
	_ = in
}

// Concurrent saves on the same expected version: exactly one row is
// inserted, every other request is a 409; version numbers stay unique.
func TestContentAPIConcurrentSaves(t *testing.T) {
	e := newAPIEnv(t, nil)
	c := e.create("Concurrent edits")
	in := e.interpret(c.ID, replyIstanbul)
	var wg sync.WaitGroup
	statuses := make([]int, 10)
	for i := range statuses {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			r := e.putContent(c.ID, contentJSON(cSMS+strings.Repeat("!", i+1), cSubject, cBody), in.Campaign.Version)
			statuses[i] = r.status
		}(i)
	}
	wg.Wait()
	ok, conflicts := 0, 0
	for _, s := range statuses {
		switch s {
		case 200:
			ok++
		case 409:
			conflicts++
		default:
			t.Errorf("status %d", s)
		}
	}
	if ok != 1 || conflicts != 9 {
		t.Errorf("ok = %d, conflicts = %d", ok, conflicts)
	}
	rows := e.contentRows(c.ID)
	if len(rows) != 1 || rows[0].Version != 1 {
		t.Errorf("rows = %+v", rows)
	}
	if _, v, cur := e.pointer(c.ID); v != in.Campaign.Version+1 || cur == nil || *cur != rows[0].ID {
		t.Errorf("pointer = %d %v", v, cur)
	}
}

// An edit racing with a generation: the one that commits second is based
// on a stale version and is rejected; the pointer always names the newest
// version.
func TestContentAPIEditDuringGeneration(t *testing.T) {
	e := newAPIEnv(t, nil)
	c := e.create("Edit during generation")
	in := e.interpret(c.ID, replyIstanbul)
	release := make(chan struct{})
	started := make(chan struct{})
	e.llm.Push(llm.Step{Func: func(ctx context.Context, _ llm.Request) (llm.Response, error) {
		close(started)
		select {
		case <-release:
		case <-ctx.Done():
			return llm.Response{}, ctx.Err()
		}
		return llm.Response{Text: replyContentBoth, StopReason: llm.StopEndTurn}, nil
	}})
	done := make(chan []streamEvent, 1)
	go func() { done <- e.streamGeneration(c.ID, nil) }()
	<-started
	// The marketer saves an edit while the model is still working.
	must[apiSaved](t, e.putContent(c.ID, contentJSON("Manual 30% off text", cSubject, cBody), in.Campaign.Version), 200)
	close(release)
	evs := <-done
	last := evs[len(evs)-1]
	if last.name != "error" || !strings.Contains(string(last.data), `"version_conflict"`) {
		t.Fatalf("terminal = %s %s", last.name, last.data)
	}
	rows := e.contentRows(c.ID)
	if len(rows) != 1 || rows[0].Origin != "edited" {
		t.Errorf("rows = %+v", rows)
	}
}
