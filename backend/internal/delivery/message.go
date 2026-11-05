package delivery

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/fyusufayan/insider-campaign-composer-case/backend/internal/content"
	"github.com/fyusufayan/insider-campaign-composer-case/backend/internal/provider"
	"github.com/fyusufayan/insider-campaign-composer-case/backend/internal/segment"
)

// Permanent failure codes recorded in last_error when no request is made.
const (
	// CodeInvalidVars: the frozen personalization values are not exactly
	// {first_name, last_name, city} strings (a bug; never repaired).
	CodeInvalidVars = "invalid_vars"
	// CodeContentInvalid: the approved content version no longer parses
	// (a bug or a tampered row; unvalidated text is never sent).
	CodeContentInvalid = "content_invalid"
)

// errContentInvalid marks a stored content version that fails content.Parse.
var errContentInvalid = errors.New("delivery: approved content is invalid")

// ParseApprovedContent re-validates the approved content version with
// content.Parse for the approval's channels (the same entry point as AI
// output, edits and prepare). The stored text must already be canonical
// (NFC, trimmed), i.e. exactly what Parse produces; nothing is repaired.
func ParseApprovedContent(d content.Draft, channels []segment.Channel) (content.Content, error) {
	c, res := content.Parse(d, channels)
	if err := res.Err(); err != nil {
		return content.Content{}, fmt.Errorf("%w: %w", errContentInvalid, err)
	}
	cd := c.Draft()
	if !strPtrEq(cd.SMSBody, d.SMSBody) || !strPtrEq(cd.EmailSubject, d.EmailSubject) || !strPtrEq(cd.EmailBody, d.EmailBody) {
		return content.Content{}, fmt.Errorf("%w: stored text is not canonical", errContentInvalid)
	}
	for _, ch := range channels {
		if !c.Has(ch) {
			return content.Content{}, fmt.Errorf("%w: no %s text", errContentInvalid, ch)
		}
	}
	return c, nil
}

func strPtrEq(a, b *string) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

// decodeVars strictly decodes frozen personalization values: exactly
// first_name, last_name and city, all strings (the content.Values shape
// written by approval.Prepare).
func decodeVars(data []byte) (content.Values, error) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil || len(raw) != 3 {
		return content.Values{}, errors.New("vars must be an object with exactly first_name, last_name and city")
	}
	for _, k := range []string{"first_name", "last_name", "city"} {
		if s := raw[k]; len(s) == 0 || s[0] != '"' {
			return content.Values{}, errors.New("vars values must be strings")
		}
	}
	var v content.Values
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&v); err != nil {
		return content.Values{}, errors.New("vars could not be decoded")
	}
	return v, nil
}

// BuildMessage renders the message of one delivery from the immutable
// approved content and the row's frozen values, with the deterministic
// renderer (same content + same values → same bytes, so every retry sends
// a byte-identical body under the same idempotency key). The destination
// is filled in later, from the precheck. code is a permanent failure code
// (a content render code such as missing_value / value_too_long /
// content_too_long, or invalid_vars): nothing is truncated or replaced, and
// such a delivery fails without a provider request. err is a bug (unknown
// channel, content without the channel).
func BuildMessage(c content.Content, ch provider.Channel, key string, vars []byte) (m provider.Message, code string, err error) {
	v, verr := decodeVars(vars)
	if verr != nil {
		return provider.Message{}, CodeInvalidVars, nil
	}
	m = provider.Message{Channel: ch, IdempotencyKey: key}
	var rerr error
	switch ch {
	case provider.ChannelSMS:
		m.Text, rerr = c.RenderSMS(v)
	case provider.ChannelEmail:
		var e content.RenderedEmail
		e, rerr = c.RenderEmail(v)
		// The fixed footer (unsubscribe text) is always attached (§8.8).
		m.Subject, m.Text = e.Subject, e.Text()
	default:
		return provider.Message{}, "", fmt.Errorf("delivery: unknown channel %q", ch)
	}
	if rerr != nil {
		var re *content.RenderError
		if errors.As(rerr, &re) {
			return provider.Message{}, re.Code, nil
		}
		return provider.Message{}, "", fmt.Errorf("delivery: render %s: %w", ch, rerr)
	}
	return m, "", nil
}
