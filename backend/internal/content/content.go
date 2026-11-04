// Package content owns campaign message content: the {{var}} template
// parser and renderer (ADR-018), the personalization variable registry, the
// content validator (ARCHITECTURE §8, ADR-013) and the rules of immutable
// content versions.
//
// Everything here is pure and deterministic. The same Parse is applied to AI
// output, marketer edits, the editor's debounced validation, approval
// preparation and stored versions read back from the database. A Content can
// only be obtained from Parse, so text that reaches the renderer (and the
// delivery worker) has always passed validation.
//
// The package never sees customer data except in Values passed to the
// renderer at delivery time; errors and messages never echo the input.
package content

import (
	"errors"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"

	"github.com/fyusufayan/insider-campaign-composer-case/backend/internal/segment"
)

// Hard limits on the worst-case rune count after personalization and NFC
// (ARCHITECTURE §8.5, ADR-013). Violations are blocking errors.
const (
	MaxSMSRunes          = 160
	MaxEmailSubjectRunes = 80
	MaxEmailBodyRunes    = 5000
)

// Field paths, identical to the content DTO property names (§11) and the
// campaign_contents columns.
const (
	FieldSMSBody      = "sms_body"
	FieldEmailSubject = "email_subject"
	FieldEmailBody    = "email_body"
)

// Error and warning codes specific to content. Generic codes are shared with
// segment (required, empty, too_long, too_many, not_allowed).
const (
	CodeInvalidEncoding      = "invalid_encoding"
	CodeInvalidCharacter     = "invalid_character"
	CodeMarkupNotAllowed     = "markup_not_allowed"
	CodeUnbalancedDelimiter  = "unbalanced_delimiter"
	CodeMalformedPlaceholder = "malformed_placeholder"
	CodeUnknownVariable      = "unknown_variable"
	CodeInvalidFallback      = "invalid_fallback"

	// WarnFirstNameNotUsed: the email body does not use {{first_name}} (§8.6).
	WarnFirstNameNotUsed = "first_name_not_used"

	codeTooMany = segment.CodeTooMany
)

// ErrInvalidContent is matched by *ValidationError.
var ErrInvalidContent = errors.New("content: invalid content")

// ValidationError carries the field errors of a failed Parse.
type ValidationError struct {
	Problems segment.Errors
}

func (e *ValidationError) Error() string { return "content: invalid content: " + e.Problems.Error() }

// Is matches ErrInvalidContent.
func (e *ValidationError) Is(target error) bool { return target == ErrInvalidContent }

// Draft is untrusted content input (AI output, an edit, or a stored row read
// back). nil means "not provided"; its JSON shape is the content DTO.
type Draft struct {
	SMSBody      *string `json:"sms_body"`
	EmailSubject *string `json:"email_subject"`
	EmailBody    *string `json:"email_body"`
}

// Content is validated content: well-formed templates within the hard
// limits, with the email subject and body either both present or both absent.
// The zero value has no channel content and is never returned by a
// successful Parse.
type Content struct {
	sms, subject, body *Template
}

// SMSBody returns the SMS template, if present.
func (c Content) SMSBody() (Template, bool) { return deref(c.sms) }

// EmailSubject returns the email subject template, if present.
func (c Content) EmailSubject() (Template, bool) { return deref(c.subject) }

// EmailBody returns the email body template, if present.
func (c Content) EmailBody() (Template, bool) { return deref(c.body) }

func deref(t *Template) (Template, bool) {
	if t == nil {
		return Template{}, false
	}
	return *t, true
}

// IsZero reports whether c holds no content.
func (c Content) IsZero() bool { return c.sms == nil && c.subject == nil && c.body == nil }

// Has reports whether c has content for the channel.
func (c Content) Has(ch segment.Channel) bool {
	switch ch {
	case segment.ChannelSMS:
		return c.sms != nil
	case segment.ChannelEmail:
		return c.subject != nil && c.body != nil
	}
	return false
}

// Channels lists the channels c has content for, in the fixed order.
func (c Content) Channels() []segment.Channel {
	var out []segment.Channel
	for _, ch := range []segment.Channel{segment.ChannelSMS, segment.ChannelEmail} {
		if c.Has(ch) {
			out = append(out, ch)
		}
	}
	return out
}

// Draft returns the canonical (normalized) text for persistence. Parsing it
// again yields content Equal to c.
func (c Content) Draft() Draft {
	str := func(t *Template) *string {
		if t == nil {
			return nil
		}
		s := t.text
		return &s
	}
	return Draft{SMSBody: str(c.sms), EmailSubject: str(c.subject), EmailBody: str(c.body)}
}

// Equal reports whether both have the same fields with identical canonical
// text.
func (c Content) Equal(o Content) bool {
	eq := func(a, b *Template) bool {
		if a == nil || b == nil {
			return a == b
		}
		return a.text == b.text
	}
	return eq(c.sms, o.sms) && eq(c.subject, o.subject) && eq(c.body, o.body)
}

// LengthStats reports a field's worst-case length against its limit.
type LengthStats struct {
	CharsWorstCase int `json:"chars_worst_case"`
	Limit          int `json:"limit"`
}

// Result is the deterministic outcome of validation (§8 result shape). Errors
// and Warnings are never nil. Length statistics are present for every field
// whose template parsed (even if it is too long); RenderedExample only when
// there are no errors.
type Result struct {
	Errors          segment.Errors       `json:"errors"`
	Warnings        []segment.FieldError `json:"warnings"`
	SMS             *LengthStats         `json:"sms,omitempty"`
	Subject         *LengthStats         `json:"subject,omitempty"`
	Body            *LengthStats         `json:"body,omitempty"`
	RenderedExample *Rendered            `json:"rendered_example,omitempty"`
}

// OK reports whether there are no errors (warnings do not block).
func (r Result) OK() bool { return len(r.Errors) == 0 }

// Err returns a *ValidationError, or nil if OK.
func (r Result) Err() error {
	if r.OK() {
		return nil
	}
	return &ValidationError{Problems: r.Errors}
}

type fieldSpec struct {
	path         string
	limit        int
	allowNewline bool
}

var (
	specSMS     = fieldSpec{path: FieldSMSBody, limit: MaxSMSRunes, allowNewline: true}
	specSubject = fieldSpec{path: FieldEmailSubject, limit: MaxEmailSubjectRunes, allowNewline: false}
	specBody    = fieldSpec{path: FieldEmailBody, limit: MaxEmailBodyRunes, allowNewline: true}
)

// Validate is Parse without the content.
func Validate(d Draft, channels []segment.Channel) Result {
	_, r := Parse(d, channels)
	return r
}

// Parse normalizes (NFC, trim) and validates d for the selected channels.
//
//   - Every provided field is validated, whether or not its channel is
//     selected: a content version is only stored when it is well-formed as a
//     whole.
//   - A selected channel's fields are required (sms: sms_body; email:
//     email_subject and email_body). The email subject and body are a pair:
//     if one is provided, the other is required.
//   - A field that is provided but blank counts as absent unless it is
//     required ("empty"); content must contain at least one channel.
//
// On any error Content is the zero value. channels must be known channels.
func Parse(d Draft, channels []segment.Channel) (Content, Result) {
	res := Result{Errors: segment.Errors{}, Warnings: []segment.FieldError{}}
	chs, err := segment.NormalizeChannels(channels)
	if err != nil {
		res.Errors = append(res.Errors, segment.FieldError{Path: "channels", Code: segment.CodeNotAllowed,
			Message: "unknown channel", Allowed: []string{string(segment.ChannelSMS), string(segment.ChannelEmail)}})
		return Content{}, res
	}
	selected := func(ch segment.Channel) bool {
		for _, c := range chs {
			if c == ch {
				return true
			}
		}
		return false
	}

	sms := normalizeField(d.SMSBody)
	subject := normalizeField(d.EmailSubject)
	body := normalizeField(d.EmailBody)
	emailRequired := selected(segment.ChannelEmail) || subject.present() || body.present()

	var c Content
	c.sms, res.SMS = checkField(specSMS, sms, selected(segment.ChannelSMS), &res.Errors)
	c.subject, res.Subject = checkField(specSubject, subject, emailRequired, &res.Errors)
	c.body, res.Body = checkField(specBody, body, emailRequired, &res.Errors)

	if len(res.Errors) == 0 && c.IsZero() {
		res.Errors = append(res.Errors, segment.FieldError{Path: "", Code: segment.CodeEmpty,
			Message: "content must contain text for at least one channel"})
	}
	if len(res.Errors) > 0 {
		return Content{}, res
	}

	if c.body != nil && !c.body.Uses(VarFirstName) {
		res.Warnings = append(res.Warnings, segment.FieldError{Path: FieldEmailBody, Code: WarnFirstNameNotUsed,
			Message: "the email body does not address the recipient with {{first_name}}"})
	}
	if ex, err := c.render(ExampleValues()); err == nil {
		res.RenderedExample = &ex
	}
	return c, res
}

// field is a normalized input field.
type field struct {
	provided bool   // non-nil input
	valid    bool   // valid UTF-8
	text     string // NFC, trimmed
}

func (f field) present() bool { return f.provided && (!f.valid || f.text != "") }

func normalizeField(p *string) field {
	if p == nil {
		return field{}
	}
	if !utf8.ValidString(*p) {
		return field{provided: true}
	}
	return field{provided: true, valid: true, text: strings.TrimSpace(norm.NFC.String(*p))}
}

// checkField validates one field and appends its errors in a fixed order:
// presence, encoding, characters, markup, placeholders, length.
func checkField(spec fieldSpec, f field, required bool, out *segment.Errors) (*Template, *LengthStats) {
	rep := reporter{path: spec.path}
	defer func() {
		for _, e := range rep.errs {
			*out = append(*out, segment.FieldError{Path: spec.path, Code: e.code, Message: e.msg, Allowed: e.allowed})
		}
	}()
	switch {
	case !f.provided:
		if required {
			rep.add(segment.CodeRequired, "is required for the selected channels")
		}
		return nil, nil
	case !f.valid:
		rep.add(CodeInvalidEncoding, "is not valid UTF-8 text")
		return nil, nil
	case f.text == "":
		if required {
			rep.add(segment.CodeEmpty, "must not be empty or whitespace only")
		}
		return nil, nil
	case !hasVisible(f.text):
		rep.add(segment.CodeEmpty, "must contain visible text")
		return nil, nil
	}
	s := f.text
	if i, what := firstForbidden(s, spec.allowNewline); i >= 0 {
		rep.add(CodeInvalidCharacter, "contains "+what+" "+at(s, i))
	}
	if i := firstMarkup(s); i >= 0 {
		rep.add(CodeMarkupNotAllowed, "contains markup "+at(s, i)+"; content is plain text only")
	}
	t, ok := parseTemplate(s, &rep)
	if !ok || len(rep.errs) > 0 {
		return nil, nil
	}
	n := t.WorstCaseRunes()
	stats := &LengthStats{CharsWorstCase: n, Limit: spec.limit}
	if n > spec.limit {
		rep.add(segment.CodeTooLong, "must be at most "+strconv.Itoa(spec.limit)+
			" characters in the worst case after personalization (is "+strconv.Itoa(n)+")")
		return nil, stats
	}
	return &t, stats
}

// hasVisible reports whether s contains a letter, number, punctuation or
// symbol (so whitespace/zero-width-only text counts as empty).
func hasVisible(s string) bool {
	return strings.ContainsFunc(s, func(r rune) bool {
		return unicode.IsLetter(r) || unicode.IsNumber(r) || unicode.IsPunct(r) || unicode.IsSymbol(r)
	})
}

// forbiddenRune returns a description if r may not appear in content text.
// Allowed: any printable text incl. emoji and joiners; '\n' only where
// allowNewline. Forbidden: other C0/C1 controls (incl. '\r', '\t'), Unicode
// line/paragraph separators, bidirectional controls (spoofing), the BOM and
// noncharacters.
func forbiddenRune(r rune, allowNewline bool) string {
	switch {
	case r == '\n':
		if allowNewline {
			return ""
		}
		return "a line break"
	case unicode.IsControl(r):
		return "a control character"
	case r == 0x2028 || r == 0x2029:
		return "a line or paragraph separator"
	case r == 0x061C || r == 0x200E || r == 0x200F || (r >= 0x202A && r <= 0x202E) || (r >= 0x2066 && r <= 0x2069):
		return "a bidirectional control character"
	case r == 0xFEFF:
		return "a byte-order mark"
	case r >= 0xFDD0 && r <= 0xFDEF, r&0xFFFE == 0xFFFE:
		return "a Unicode noncharacter"
	}
	return ""
}

func firstForbidden(s string, allowNewline bool) (int, string) {
	for i, r := range s {
		if d := forbiddenRune(r, allowNewline); d != "" {
			return i, d
		}
	}
	return -1, ""
}

// firstMarkup finds an HTML/XML-like tag opener ("<" followed by a letter,
// '/', '!' or '?'). Plain text such as "<3" or "a < b" is allowed. This is a
// plain-text policy check, not a sanitizer: rendering never interprets markup.
func firstMarkup(s string) int {
	for i := 0; i+1 < len(s); i++ {
		if s[i] != '<' {
			continue
		}
		c := s[i+1]
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c == '/' || c == '!' || c == '?' {
			return i
		}
	}
	return -1
}
