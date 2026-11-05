package approval

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"go/parser"
	"go/token"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/fyusufayan/insider-campaign-composer-case/backend/internal/content"
	"github.com/fyusufayan/insider-campaign-composer-case/backend/internal/segment"
)

func sha(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

func TestDigest(t *testing.T) {
	empty, err := Digest(nil)
	if err != nil || empty != sha("") {
		t.Fatalf("empty = %s, %v", empty, err)
	}
	got, err := Digest([]Recipient{{1, segment.ChannelSMS}, {1, segment.ChannelEmail}, {3, segment.ChannelEmail}, {10, segment.ChannelSMS}})
	if err != nil || got != sha("1:sms\n1:email\n3:email\n10:sms") {
		t.Fatalf("digest = %s, %v", got, err)
	}
	// Numeric (not lexicographic) user order, channel order sms < email,
	// no duplicates.
	for _, bad := range [][]Recipient{
		{{10, segment.ChannelSMS}, {9, segment.ChannelSMS}},
		{{1, segment.ChannelEmail}, {1, segment.ChannelSMS}},
		{{1, segment.ChannelSMS}, {1, segment.ChannelSMS}},
		{{1, "push"}},
	} {
		if _, err := Digest(bad); err == nil {
			t.Errorf("accepted %v", bad)
		}
	}
}

func mustContent(t *testing.T, sms, subject, body string) content.Content {
	t.Helper()
	d := content.Draft{}
	var chs []segment.Channel
	if sms != "" {
		d.SMSBody = &sms
		chs = append(chs, segment.ChannelSMS)
	}
	if subject != "" {
		d.EmailSubject, d.EmailBody = &subject, &body
		chs = append(chs, segment.ChannelEmail)
	}
	c, res := content.Parse(d, chs)
	if !res.OK() {
		t.Fatalf("content: %v", res.Errors)
	}
	return c
}

// Frozen values are rendered as they are; problems are reported per code
// and never repaired (§8.1 rendering rules, §8.7).
func TestRenderIssue(t *testing.T) {
	strict := mustContent(t, "Hi {{first_name}} from {{city}}", "Hello {{last_name}}", "Dear {{first_name}},\nsee you.")
	fallback := mustContent(t, "Hi {{first_name|there}}", "Hello {{last_name|friend}}", "Dear {{first_name|customer}},\nsee you.")
	ok := content.Values{FirstName: "Ayşe", LastName: "Yılmaz", City: "İstanbul"}
	cases := []struct {
		name string
		c    content.Content
		ch   segment.Channel
		v    content.Values
		want string
	}{
		{"valid sms", strict, segment.ChannelSMS, ok, ""},
		{"valid email", strict, segment.ChannelEmail, ok, ""},
		{"blank name without fallback", strict, segment.ChannelSMS, content.Values{FirstName: "   ", City: "Ankara"}, content.RenderMissingValue},
		{"zero-width name without fallback", strict, segment.ChannelEmail, content.Values{FirstName: "\u200b", LastName: "X"}, content.RenderMissingValue},
		{"blank name with fallback", fallback, segment.ChannelSMS, content.Values{FirstName: "   "}, ""},
		{"blank last name with fallback", fallback, segment.ChannelEmail, content.Values{FirstName: "A", LastName: " "}, ""},
		{"control character", fallback, segment.ChannelSMS, content.Values{FirstName: "A\nB"}, content.RenderInvalidValue},
		{"invalid UTF-8 is never replaced by the fallback", fallback, segment.ChannelSMS, content.Values{FirstName: "\xff"}, content.RenderInvalidValue},
		{"name over the declared maximum", fallback, segment.ChannelSMS, content.Values{FirstName: strings.Repeat("a", content.MaxFirstNameRunes+1)}, content.RenderValueTooLong},
		{"city over the declared maximum", strict, segment.ChannelSMS, content.Values{FirstName: "A", City: strings.Repeat("ş", 60)}, content.RenderValueTooLong},
		{"unused variable is irrelevant", fallback, segment.ChannelSMS, content.Values{FirstName: "A", City: strings.Repeat("x", 500)}, ""},
	}
	for _, tc := range cases {
		got, err := renderIssue(tc.c, tc.ch, tc.v)
		if err != nil || got != tc.want {
			t.Errorf("%s: got %q, %v; want %q", tc.name, got, err, tc.want)
		}
	}
	smsOnly := mustContent(t, "Hi", "", "")
	if _, err := renderIssue(smsOnly, segment.ChannelEmail, ok); !errors.Is(err, ErrInconsistent) {
		t.Errorf("absent channel: %v", err)
	}
}

func TestDecodeVars(t *testing.T) {
	v, err := decodeVars([]byte(`{"first_name":"Ayşe","last_name":"Yılmaz","city":"İstanbul"}`))
	if err != nil || v != (content.Values{FirstName: "Ayşe", LastName: "Yılmaz", City: "İstanbul"}) {
		t.Fatalf("v = %+v, %v", v, err)
	}
	for _, bad := range []string{
		`{"first_name":"A","last_name":"B"}`,
		`{"first_name":"A","last_name":"B","city":"C","email":"x@example.com"}`,
		`{"first_name":"A","last_name":"B","phone":"+90"}`,
		`{"first_name":1,"last_name":"B","city":"C"}`,
		`{"first_name":null,"last_name":"B","city":"C"}`,
		`[]`, `null`,
	} {
		if _, err := decodeVars([]byte(bad)); err == nil {
			t.Errorf("accepted %s", bad)
		}
	}
}

func TestConfigAndService(t *testing.T) {
	for _, ttl := range []time.Duration{0, time.Millisecond, -time.Minute, 25 * time.Hour} {
		if err := (Config{PendingTTL: ttl}).Validate(); err == nil {
			t.Errorf("ttl %s accepted", ttl)
		}
	}
	if err := (Config{PendingTTL: DefaultPendingTTL}).Validate(); err != nil {
		t.Error(err)
	}
	if DefaultPendingTTL != 15*time.Minute {
		t.Errorf("default pending TTL = %s, ADR-006 says 15m", DefaultPendingTTL)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	if _, err := NewService(nil, Config{PendingTTL: DefaultPendingTTL}, log); err == nil {
		t.Error("nil repository accepted")
	}
	if _, err := NewService(NewRepository(nil), Config{PendingTTL: DefaultPendingTTL}, nil); err == nil {
		t.Error("nil logger accepted")
	}
	if _, err := NewService(NewRepository(nil), Config{}, log); err == nil {
		t.Error("zero TTL accepted")
	}
	// The repository refuses an out-of-range TTL before touching the pool.
	if _, err := NewRepository(nil).Prepare(context.Background(), PrepareParams{PendingTTL: 0}); err == nil {
		t.Error("repository accepted zero TTL")
	}
}

func TestClassify(t *testing.T) {
	live := context.Background()
	for _, code := range []string{"55P03", "40001", "40P01", "57014"} {
		err := fmt.Errorf("wrapped: %w", &pgconn.PgError{Code: code})
		if !errors.Is(classify(live, err), ErrBusy) {
			t.Errorf("%s not busy", code)
		}
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if errors.Is(classify(cancelled, &pgconn.PgError{Code: "57014"}), ErrBusy) {
		t.Error("caller cancellation reported as busy")
	}
	if errors.Is(classify(live, &pgconn.PgError{Code: "23505"}), ErrBusy) {
		t.Error("unique violation reported as busy")
	}
	if classify(live, nil) != nil {
		t.Error("nil error changed")
	}
}

// The summary type graph holds no recipient values: no content.Values and
// no field that could carry a contact.
func TestSummaryCarriesNoRecipientData(t *testing.T) {
	seen := map[reflect.Type]bool{}
	valuesType := reflect.TypeOf(content.Values{})
	var walk func(reflect.Type, string)
	walk = func(rt reflect.Type, path string) {
		if seen[rt] {
			return
		}
		seen[rt] = true
		if rt == valuesType {
			t.Errorf("%s: summary contains content.Values", path)
		}
		switch rt.Kind() {
		case reflect.Pointer, reflect.Slice, reflect.Array:
			walk(rt.Elem(), path+"[]")
		case reflect.Map:
			walk(rt.Key(), path+"{key}")
			walk(rt.Elem(), path+"{}")
		case reflect.Struct:
			for i := range rt.NumField() {
				f := rt.Field(i)
				n := strings.ToLower(f.Name)
				for _, bad := range []string{"phone", "address", "destination", "firstname", "lastname", "vars", "userid", "recipientid"} {
					if strings.Contains(n, bad) {
						t.Errorf("%s.%s: suspicious field", path, f.Name)
					}
				}
				walk(f.Type, path+"."+f.Name)
			}
		}
	}
	walk(reflect.TypeOf(Summary{}), "Summary")
}

// approval must not reach the LLM: no import of ai/llm (directly) and no
// delivery/provider code (CP14 freezes the snapshot; it never sends).
func TestImportBoundaries(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	const mod = "github.com/fyusufayan/insider-campaign-composer-case/backend/internal/"
	forbidden := []string{mod + "ai", mod + "llm", mod + "delivery", mod + "provider", mod + "mockprovider", mod + "httpapi"}
	fset := token.NewFileSet()
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		af, err := parser.ParseFile(fset, f, src, parser.ImportsOnly)
		if err != nil {
			t.Fatal(err)
		}
		for _, imp := range af.Imports {
			p, _ := strconv.Unquote(imp.Path.Value)
			for _, bad := range forbidden {
				if p == bad || strings.HasPrefix(p, bad+"/") {
					t.Errorf("%s imports %s", f, p)
				}
			}
		}
	}
}

// The approval statements never use now() for expiry: every expiry
// comparison and expiry-defining timestamp uses clock_timestamp() (ADR-006).
func TestExpirySQLUsesClockTimestamp(t *testing.T) {
	for name, stmt := range map[string]string{
		"lock": sqlLockApproval, "expire": sqlExpireApproval, "revoke": sqlRevokeApproval, "insert": sqlInsertApproval,
	} {
		low := strings.ToLower(stmt)
		if strings.Contains(low, "now()") || strings.Contains(low, "current_timestamp") {
			t.Errorf("%s uses now()/CURRENT_TIMESTAMP", name)
		}
		if !strings.Contains(low, "clock_timestamp()") {
			t.Errorf("%s does not use clock_timestamp()", name)
		}
	}
}

// Approval statuses only move forward: no statement of this package sets
// an approval back to pending or approved, so an expired or revoked
// approval can never become live again. The only statement that creates a
// pending approval is the INSERT of a new row (a new id).
func TestNoStatementReactivatesAnApproval(t *testing.T) {
	stmts := map[string]string{
		"lock": sqlLockApproval, "expire": sqlExpireApproval, "revoke": sqlRevokeApproval,
		"delete": sqlDeleteHeld, "count": sqlCountApprovalRows, "insert approval": sqlInsertApproval,
		"insert held": sqlInsertHeldTail, "read": sqlReadSnapshot,
	}
	for name, stmt := range stmts {
		low := strings.ToLower(stmt)
		for _, bad := range []string{"status = 'pending'", "status = 'approved'", "status='pending'", "status='approved'"} {
			if strings.Contains(low, "update") && strings.Contains(low, "set "+bad) {
				t.Errorf("%s sets %s", name, bad)
			}
		}
		if strings.HasPrefix(strings.TrimSpace(low), "delete") && !strings.Contains(low, "status = 'held'") {
			t.Errorf("%s deletes non-held rows", name)
		}
	}
	// Closing statements only act on live approvals.
	for _, stmt := range []string{sqlExpireApproval, sqlRevokeApproval} {
		if !strings.Contains(stmt, "status IN ('pending', 'approved')") {
			t.Errorf("closing statement is not restricted to live approvals: %s", stmt)
		}
	}
	if !strings.Contains(sqlInsertApproval, "'pending'") {
		t.Error("new approvals must start pending")
	}
}
