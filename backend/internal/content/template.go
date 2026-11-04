package content

import (
	"strconv"
	"strings"
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"
)

// Template syntax (ADR-018, ARCHITECTURE §8.2)
//
//	{{name}}            name ∈ {first_name, last_name, city}
//	{{name|fallback}}   fallback is literal text used when the value is missing
//
// The characters '{' and '}' are reserved: outside a well-formed placeholder
// they are an error (there is no escape). Inside the braces there is exactly a
// variable name and at most one "|fallback"; no whitespace around the name, no
// filters, function calls, dots, nesting or other expressions. Nothing in a
// template is ever evaluated: rendering is a single pass of literal
// substitution, so a value or fallback that itself looks like a placeholder is
// emitted verbatim and never re-parsed.

// Placeholder is one {{…}} occurrence of a parsed template.
type Placeholder struct {
	Variable    VariableName `json:"variable"`
	Fallback    string       `json:"fallback,omitempty"`
	HasFallback bool         `json:"has_fallback"`
}

// part is a literal run or a placeholder.
type part struct {
	literal string
	ph      *Placeholder
	max     int // declared max runes of ph.Variable
}

// Template is a parsed, validated template. The zero value is empty; values
// with content are only produced by Parse, so a Template can never hold an
// unapproved placeholder.
type Template struct {
	text  string // normalized source (NFC, trimmed): the persisted form
	parts []part
}

// String returns the canonical (normalized) template text, as persisted.
func (t Template) String() string { return t.text }

// Placeholders returns the placeholders in order of appearance (repeats
// included).
func (t Template) Placeholders() []Placeholder {
	var out []Placeholder
	for _, p := range t.parts {
		if p.ph != nil {
			out = append(out, *p.ph)
		}
	}
	return out
}

// Uses reports whether the template references the variable.
func (t Template) Uses(v VariableName) bool {
	for _, p := range t.parts {
		if p.ph != nil && p.ph.Variable == v {
			return true
		}
	}
	return false
}

// WorstCaseRunes is the maximum rune count of any rendering (ARCHITECTURE
// §8.4): literal runes + Σ over placeholders of max(declared_max,
// rune_len(fallback)).
func (t Template) WorstCaseRunes() int {
	n := 0
	for _, p := range t.parts {
		if p.ph == nil {
			n += utf8.RuneCountInString(p.literal)
			continue
		}
		n += max(p.max, utf8.RuneCountInString(p.ph.Fallback))
	}
	return n
}

// render substitutes values (single pass, no re-parsing). It returns a
// *RenderError (never echoing the value) when a value is unusable.
func (t Template) render(field string, v Values) (string, error) {
	var b strings.Builder
	for _, p := range t.parts {
		if p.ph == nil {
			b.WriteString(p.literal)
			continue
		}
		val, err := resolveValue(field, p, v)
		if err != nil {
			return "", err
		}
		b.WriteString(val)
	}
	return norm.NFC.String(b.String()), nil
}

func resolveValue(field string, p part, v Values) (string, error) {
	raw := v.get(p.ph.Variable)
	if !utf8.ValidString(raw) {
		return "", &RenderError{Field: field, Variable: p.ph.Variable, Code: RenderInvalidValue}
	}
	val := strings.TrimSpace(norm.NFC.String(raw))
	// Forbidden characters are checked first: a non-empty but invalid value
	// fails and is never replaced by the fallback.
	if strings.ContainsFunc(val, func(r rune) bool { return forbiddenRune(r, false) != "" }) {
		return "", &RenderError{Field: field, Variable: p.ph.Variable, Code: RenderInvalidValue}
	}
	// Empty, or only invisible characters (e.g. U+200B): the value is missing.
	if !hasVisible(val) {
		if p.ph.HasFallback {
			return p.ph.Fallback, nil
		}
		return "", &RenderError{Field: field, Variable: p.ph.Variable, Code: RenderMissingValue}
	}
	if utf8.RuneCountInString(val) > p.max {
		return "", &RenderError{Field: field, Variable: p.ph.Variable, Code: RenderValueTooLong}
	}
	return val, nil
}

// reporter collects the errors of one field, capped so that a pathological
// input cannot produce an unbounded error list.
type reporter struct {
	path string
	errs []fieldError
	full bool
}

type fieldError struct {
	code, msg string
	allowed   []string
}

// maxErrorsPerField bounds the errors reported for one field; one further
// "too_many" entry marks the truncation.
const maxErrorsPerField = 10

// add records an error and reports whether more may be added.
func (r *reporter) add(code, msg string, allowed ...string) bool {
	if r.full {
		return false
	}
	if len(r.errs) == maxErrorsPerField {
		r.errs = append(r.errs, fieldError{code: codeTooMany, msg: "further errors omitted"})
		r.full = true
		return false
	}
	r.errs = append(r.errs, fieldError{code: code, msg: msg, allowed: allowed})
	return true
}

// at formats a 1-based character position of byte offset i in s.
func at(s string, i int) string {
	return "at character " + strconv.Itoa(utf8.RuneCountInString(s[:i])+1)
}

// parseTemplate splits normalized text into literal and placeholder parts.
// It reports every placeholder/delimiter problem (up to the cap) and returns
// ok=false if there was any.
func parseTemplate(s string, rep *reporter) (Template, bool) {
	t := Template{text: s}
	ok := true
	var lit strings.Builder
	flush := func() {
		if lit.Len() > 0 {
			t.parts = append(t.parts, part{literal: lit.String()})
			lit.Reset()
		}
	}
	for i := 0; i < len(s); {
		switch {
		case strings.HasPrefix(s[i:], "{{"):
			end := strings.Index(s[i+2:], "}}")
			if end < 0 {
				rep.add(CodeUnbalancedDelimiter, "placeholder opened with \"{{\" "+at(s, i)+" is never closed with \"}}\"")
				return Template{}, false
			}
			ph, v, code, msg, allowed := parsePlaceholder(s[i+2 : i+2+end])
			if code != "" {
				ok = false
				if !rep.add(code, "placeholder "+at(s, i)+" "+msg, allowed...) {
					return Template{}, false
				}
			} else {
				flush()
				t.parts = append(t.parts, part{ph: &ph, max: v.MaxRunes})
			}
			i += 2 + end + 2
		case s[i] == '{' || s[i] == '}':
			ok = false
			n := 1
			if strings.HasPrefix(s[i:], "}}") {
				n = 2
			}
			if !rep.add(CodeUnbalancedDelimiter, "unexpected \""+s[i:i+n]+"\" "+at(s, i)+
				"; braces are reserved for {{variable}} placeholders") {
				return Template{}, false
			}
			i += n
		default:
			j := strings.IndexAny(s[i:], "{}")
			if j < 0 {
				j = len(s) - i
			}
			lit.WriteString(s[i : i+j])
			i += j
		}
	}
	if !ok {
		return Template{}, false
	}
	flush()
	return t, true
}

// parsePlaceholder parses the text between "{{" and "}}". On failure it
// returns a non-empty code; messages never echo the input.
func parsePlaceholder(inner string) (ph Placeholder, v Variable, code, msg string, allowed []string) {
	const shape = "must be {{variable}} or {{variable|fallback}}"
	switch {
	case inner == "":
		return ph, v, CodeMalformedPlaceholder, "is empty; " + shape, variableNames()
	case strings.ContainsAny(inner, "{}"):
		return ph, v, CodeMalformedPlaceholder, "is nested or has stray braces; " + shape, variableNames()
	case strings.Count(inner, "|") > 1:
		return ph, v, CodeMalformedPlaceholder, "has more than one \"|\"; " + shape, variableNames()
	}
	name, fallback, hasFallback := strings.Cut(inner, "|")
	if !isIdentifier(name) {
		return ph, v, CodeMalformedPlaceholder,
			"is not a plain variable name (no spaces, expressions, filters or function calls); " + shape, variableNames()
	}
	v, found := LookupVariable(name)
	if !found {
		return ph, v, CodeUnknownVariable, "uses an unknown variable (names are lower-case)", variableNames()
	}
	if hasFallback {
		switch {
		case fallback == "":
			return ph, v, CodeInvalidFallback, "has an empty fallback; omit \"|\" or give fallback text", nil
		case strings.TrimSpace(fallback) != fallback:
			return ph, v, CodeInvalidFallback, "has a fallback that starts or ends with whitespace", nil
		case strings.ContainsFunc(fallback, func(r rune) bool { return forbiddenRune(r, false) != "" }):
			return ph, v, CodeInvalidFallback, "has a fallback with a line break or control character", nil
		case !hasVisible(fallback):
			return ph, v, CodeInvalidFallback, "has a fallback without visible characters", nil
		}
	}
	return Placeholder{Variable: v.Name, Fallback: fallback, HasFallback: hasFallback}, v, "", "", nil
}

func isIdentifier(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_') {
			return false
		}
	}
	return true
}
