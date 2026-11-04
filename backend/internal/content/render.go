package content

import (
	"errors"
	"unicode/utf8"
)

// EmailFooter is the fixed footer the backend appends to every email
// (ARCHITECTURE §8.8). It is not part of the 5,000-rune body budget and is
// never written by the LLM or the marketer.
const EmailFooter = "You are receiving this email because you agreed to receive marketing emails from us. " +
	"To stop receiving them, update your communication preferences."

// emailFooterSeparator separates the body from the footer (plain-text
// signature convention).
const emailFooterSeparator = "\n\n-- \n"

// Render error codes.
const (
	// RenderMissingValue: the value is empty or has no visible characters
	// (e.g. only U+200B) and the placeholder has no fallback. Never rendered
	// as an empty string ("Hi ,").
	RenderMissingValue = "missing_value"
	// RenderInvalidValue: the value is not valid UTF-8 or contains control
	// characters (e.g. a line break that would break an email subject).
	RenderInvalidValue = "invalid_value"
	// RenderValueTooLong: the value exceeds the variable's declared maximum,
	// which the worst-case validation relied on.
	RenderValueTooLong = "value_too_long"
	// RenderContentTooLong: the rendered text exceeds the field's hard limit
	// (§8.7; impossible for validated content and valid values).
	RenderContentTooLong = "content_too_long"
)

var (
	// ErrRender is matched by every *RenderError. A render error is a
	// permanent delivery failure; text is never truncated.
	ErrRender = errors.New("content: render failed")
	// ErrChannelAbsent: the content has no text for the requested channel.
	ErrChannelAbsent = errors.New("content: no content for channel")
)

// RenderError describes why a message could not be rendered. It never
// contains the recipient's values.
type RenderError struct {
	Field    string
	Variable VariableName // empty for RenderContentTooLong
	Code     string
}

func (e *RenderError) Error() string {
	s := "content: render " + e.Field
	if e.Variable != "" {
		s += " variable " + string(e.Variable)
	}
	return s + ": " + e.Code
}

// Is matches ErrRender.
func (e *RenderError) Is(target error) bool { return target == ErrRender }

// RenderedEmail is a rendered email. Body excludes the footer; Text joins
// them as sent.
type RenderedEmail struct {
	Subject string `json:"subject"`
	Body    string `json:"body"`
	Footer  string `json:"footer"`
}

// Text is the full plain-text body as sent: body, separator, footer.
func (e RenderedEmail) Text() string { return e.Body + emailFooterSeparator + e.Footer }

// Rendered holds the rendered channels of a content (previews).
type Rendered struct {
	SMS   *string        `json:"sms,omitempty"`
	Email *RenderedEmail `json:"email,omitempty"`
}

// RenderSMS renders the SMS for one recipient. Deterministic: the same
// content and values always produce the same bytes (needed for idempotent
// retries, §10.6).
func (c Content) RenderSMS(v Values) (string, error) {
	if c.sms == nil {
		return "", ErrChannelAbsent
	}
	return renderField(*c.sms, specSMS, v)
}

// RenderEmail renders the email for one recipient (deterministic).
func (c Content) RenderEmail(v Values) (RenderedEmail, error) {
	if c.subject == nil || c.body == nil {
		return RenderedEmail{}, ErrChannelAbsent
	}
	subject, err := renderField(*c.subject, specSubject, v)
	if err != nil {
		return RenderedEmail{}, err
	}
	body, err := renderField(*c.body, specBody, v)
	if err != nil {
		return RenderedEmail{}, err
	}
	return RenderedEmail{Subject: subject, Body: body, Footer: EmailFooter}, nil
}

func renderField(t Template, spec fieldSpec, v Values) (string, error) {
	out, err := t.render(spec.path, v)
	if err != nil {
		return "", err
	}
	if utf8.RuneCountInString(out) > spec.limit {
		return "", &RenderError{Field: spec.path, Code: RenderContentTooLong}
	}
	return out, nil
}

// render renders every present channel.
func (c Content) render(v Values) (Rendered, error) {
	var r Rendered
	if c.sms != nil {
		s, err := c.RenderSMS(v)
		if err != nil {
			return Rendered{}, err
		}
		r.SMS = &s
	}
	if c.subject != nil && c.body != nil {
		e, err := c.RenderEmail(v)
		if err != nil {
			return Rendered{}, err
		}
		r.Email = &e
	}
	return r, nil
}
