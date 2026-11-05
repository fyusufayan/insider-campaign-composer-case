package jsonstream

import (
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"
)

var fields = []string{"sms_body", "email_subject", "email_body"}

// collect feeds chunks and joins the deltas per field.
func collect(x *Extractor, chunks []string) (map[string]string, []Delta) {
	got := map[string]string{}
	var all []Delta
	for _, c := range chunks {
		for _, d := range x.Write(c) {
			if !utf8.ValidString(d.Text) || d.Text == "" {
				panic("invalid or empty delta")
			}
			got[d.Field] += d.Text
			all = append(all, d)
		}
	}
	return got, all
}

// splitEvery splits s into byte chunks of size n (possibly inside runes).
func splitEvery(s string, n int) []string {
	var out []string
	for len(s) > n {
		out = append(out, s[:n])
		s = s[n:]
	}
	return append(out, s)
}

func TestExtractsAllowedStringFields(t *testing.T) {
	doc := `{"sms_body":"Merhaba {{first_name|there}} şü 😀 \"q\" \\ \/ \n\t end",` +
		` "other": {"sms_body": "nested is ignored", "a": [1, "x\"y", {"b": null}]}, "n": -1.5e3, "t": true,` +
		`"email_subject":"İndirim %30","email_body":"line1\nline2 ğ"}`
	var want map[string]any
	if err := json.Unmarshal([]byte(doc), &want); err != nil {
		t.Fatal(err)
	}
	for n := 1; n <= len(doc); n++ {
		x := New(0, fields...)
		got, _ := collect(x, splitEvery(doc, n))
		if x.Failed() {
			t.Fatalf("chunk %d: failed", n)
		}
		for _, f := range fields {
			if got[f] != want[f] {
				t.Fatalf("chunk %d: %s = %q, want %q", n, f, got[f], want[f])
			}
		}
		if _, ok := got["other"]; ok || len(got) != 3 {
			t.Fatalf("chunk %d: unexpected fields %v", n, got)
		}
	}
}

func TestDeltasArriveIncrementallyInOrder(t *testing.T) {
	x := New(0, fields...)
	if d := x.Write(`{"sms_body":"Hel`); len(d) != 1 || d[0] != (Delta{"sms_body", "Hel"}) {
		t.Fatalf("first = %v", d)
	}
	// A multi-byte rune split across chunks is held until complete.
	ş := "ş" // 0xC5 0x9F
	if d := x.Write("lo " + ş[:1]); len(d) != 1 || d[0].Text != "lo " {
		t.Fatalf("second = %v", d)
	}
	// An escape split across chunks.
	if d := x.Write(ş[1:] + `\u00`); len(d) != 1 || d[0].Text != "ş" {
		t.Fatalf("third = %v", d)
	}
	if d := x.Write(`fc","email_subject":"S"}`); len(d) != 2 || d[0] != (Delta{"sms_body", "ü"}) || d[1] != (Delta{"email_subject", "S"}) {
		t.Fatalf("fourth = %v", d)
	}
	if x.Failed() {
		t.Fatal("failed")
	}
	if d := x.Write(`  `); d != nil {
		t.Fatalf("after end = %v", d)
	}
}

func TestSurrogatesAndInvalidSequencesMatchEncodingJSON(t *testing.T) {
	for _, doc := range []string{
		`{"sms_body":"😀"}`,
		`{"sms_body":"\ud83d x"}`,
		`{"sms_body":"\ud83dA"}`,
		`{"sms_body":"\ude00"}`,
		`{"sms_body":"\ud83d😀"}`,
		`{"sms_body":"\ud83d"}`,
		"{\"sms_body\":\"a\xffb\xe2\x82c\"}",
	} {
		var want map[string]string
		if err := json.Unmarshal([]byte(doc), &want); err != nil {
			t.Fatalf("%q: %v", doc, err)
		}
		for n := 1; n <= len(doc); n++ {
			got, _ := collect(New(0, fields...), splitEvery(doc, n))
			if got["sms_body"] != want["sms_body"] {
				t.Errorf("%q chunk %d: %q, want %q", doc, n, got["sms_body"], want["sms_body"])
			}
		}
	}
}

func TestMalformedInputStopsWithoutPanicking(t *testing.T) {
	for _, doc := range []string{
		`["sms_body"]`, `{"sms_body" "x"}`, `{"sms_body":"a` + "\x01" + `"}`, `{"sms_body":"\x"}`,
		`{"sms_body":"\u12G4"}`, `{"a":1}}`, `{,}`, `{"a":]}`, `{"a":1]}`, `x`,
	} {
		x := New(0, fields...)
		collect(x, []string{doc})
		if !x.Failed() {
			t.Errorf("%q: not failed", doc)
		}
		if d := x.Write(`"more"`); d != nil {
			t.Errorf("%q: deltas after failure", doc)
		}
	}
}

func TestLimit(t *testing.T) {
	x := New(20, fields...)
	got, _ := collect(x, []string{`{"sms_body":"`, strings.Repeat("a", 100), `"}`})
	if !x.Failed() || got["sms_body"] != strings.Repeat("a", 7) {
		t.Fatalf("failed = %v, got %q", x.Failed(), got["sms_body"])
	}
}

// FuzzExtractor: for any valid JSON object, split at arbitrary byte offsets
// (inside escapes and multi-byte runes), the concatenated deltas of every
// allowed top-level string field equal encoding/json's decoded value; deltas
// are always valid, non-empty UTF-8. For arbitrary input it never panics.
func FuzzExtractor(f *testing.F) {
	seeds := []string{
		`{"sms_body":"Hi {{first_name|there}} şğü 🎉 ç 😀"}`,
		`{"email_subject":"a\"b\\c","email_body":"x\ny","sms_body":"z"}`,
		`{"x":{"sms_body":"no"},"sms_body":"yes","y":[1,2,{"z":"\""}]}`,
		`{"sms_body":"\ud83d"}`,
		"{\"sms_body\":\"\xe2\x82\"}",
		`{"sms_body":1}`,
		`{}`,
	}
	for _, s := range seeds {
		f.Add(s, uint8(1), uint8(3))
	}
	f.Fuzz(func(t *testing.T, doc string, a, b uint8) {
		// Chunk boundaries from the two seeds (sizes 1..16, alternating).
		var chunks []string
		sizes := []int{int(a%16) + 1, int(b%16) + 1}
		for i, rest := 0, doc; len(rest) > 0; i++ {
			n := min(sizes[i%2], len(rest))
			chunks = append(chunks, rest[:n])
			rest = rest[n:]
		}
		x := New(0, fields...)
		got := map[string]string{}
		for _, c := range chunks {
			for _, d := range x.Write(c) {
				if d.Text == "" || !utf8.ValidString(d.Text) {
					t.Fatalf("bad delta %q", d.Text)
				}
				got[d.Field] += d.Text
			}
		}

		var obj map[string]json.RawMessage
		if !json.Valid([]byte(doc)) || json.Unmarshal([]byte(doc), &obj) != nil || obj == nil {
			return // not an object: only "no panic" is required
		}
		if hasDuplicateKeys(doc) {
			return // encoding/json keeps the last value; the stream shows both
		}
		if x.Failed() {
			t.Fatalf("valid object %q failed", doc)
		}
		for _, fld := range fields {
			raw, ok := obj[fld]
			var want string
			if ok && json.Unmarshal(raw, &want) != nil {
				want = "" // not a string: never reported
			}
			if got[fld] != want {
				t.Fatalf("%s = %q, want %q (doc %q)", fld, got[fld], want, doc)
			}
		}
	})
}

func hasDuplicateKeys(doc string) bool {
	dec := json.NewDecoder(strings.NewReader(doc))
	if _, err := dec.Token(); err != nil {
		return false
	}
	seen := map[string]bool{}
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return false
		}
		k, _ := tok.(string)
		if seen[k] {
			return true
		}
		seen[k] = true
		var skip json.RawMessage
		if dec.Decode(&skip) != nil {
			return false
		}
	}
	return false
}
