package ai

import (
	"encoding/json"

	"github.com/fyusufayan/insider-campaign-composer-case/backend/internal/content"
	"github.com/fyusufayan/insider-campaign-composer-case/backend/internal/segment"
)

// ContentSchema returns the structured-output JSON schema for content of the
// given (normalized, non-empty) channel selection: a closed, flat object with
// exactly the fields of the selected channels, all required strings. There
// are three possible schemas (sms, email, sms+email); each is deterministic,
// so the provider's compiled-grammar cache is reused. Lengths, placeholders
// and the offer policy are not expressible in the schema; ParseGeneratedContent
// enforces them. It returns a fresh value on every call.
func ContentSchema(chs []segment.Channel) map[string]any {
	desc := map[string]string{
		content.FieldSMSBody:      "SMS text, plain text, at most 160 characters in the worst case after personalization.",
		content.FieldEmailSubject: "Email subject line, single line, plain text, at most 80 characters in the worst case.",
		content.FieldEmailBody:    "Email body, plain text with \\n between paragraphs, at most 5000 characters in the worst case.",
	}
	props := map[string]any{}
	fields := contentFields(chs)
	for _, f := range fields {
		props[f] = map[string]any{"type": "string", "description": desc[f]}
	}
	return obj(props, fields, nil)
}

// ContentSchemaJSON is the canonical encoding of ContentSchema.
func ContentSchemaJSON(chs []segment.Channel) []byte {
	b, err := json.MarshalIndent(ContentSchema(chs), "", "  ")
	if err != nil {
		panic("ai: content schema does not encode: " + err.Error()) // static data
	}
	return b
}
