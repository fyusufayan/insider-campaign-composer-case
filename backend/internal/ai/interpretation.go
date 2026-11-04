package ai

import (
	"encoding/json"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"

	"github.com/fyusufayan/insider-campaign-composer-case/backend/internal/llm"
	"github.com/fyusufayan/insider-campaign-composer-case/backend/internal/segment"
)

// Interpretation envelope (ARCHITECTURE §6.2, decision Q3). Wire property
// names; "interpretation" is the human-readable summary of the rule and is
// persisted as interpretation.summary (§4).
const (
	keySegment           = "segment"
	keyInterpretation    = "interpretation"
	keyAssumptions       = "assumptions"
	keyUnsupported       = "unsupported"
	keySuggestedChannels = "suggested_channels"
	keyContentBrief      = "content_brief"

	keyGoal   = "goal"
	keyTone   = "tone"
	keyLength = "length"
	keyOffer  = "offer"
	keyKind   = "kind"
	keyValue  = "value"
)

var envelopeKeys = []string{keySegment, keyInterpretation, keyAssumptions, keyUnsupported, keySuggestedChannels, keyContentBrief}

// Envelope limits (Q3). Lengths are counted in runes after NFC and trimming.
const (
	MaxSummaryRunes    = 500
	MaxNotes           = 10
	MaxNoteRunes       = 300
	MaxGoalRunes       = 64
	MaxToneRunes       = 32
	MinPercentDiscount = 1
	MaxPercentDiscount = 100
)

// Length is the requested copy length.
type Length string

// Lengths.
const (
	LengthShort  Length = "short"
	LengthMedium Length = "medium"
	LengthLong   Length = "long"
)

// OfferKind is the kind of incentive. amount_discount is deliberately absent
// (decision U1: no currency semantics in v1).
type OfferKind string

// Offer kinds.
const (
	OfferNone            OfferKind = "none"
	OfferPercentDiscount OfferKind = "percent_discount"
	OfferFreeShipping    OfferKind = "free_shipping"
)

var (
	lengthValues    = []string{string(LengthShort), string(LengthMedium), string(LengthLong)}
	offerKindValues = []string{string(OfferNone), string(OfferPercentDiscount), string(OfferFreeShipping)}
	channelValues   = []string{string(segment.ChannelSMS), string(segment.ChannelEmail)}
)

// Offer is the proposed incentive. Value is set only for percent_discount.
type Offer struct {
	Kind  OfferKind
	Value int
}

// MarshalJSON encodes {"kind"} or {"kind","value"} (percent_discount).
func (o Offer) MarshalJSON() ([]byte, error) {
	if o.Kind == OfferPercentDiscount {
		return json.Marshal(struct {
			Kind  OfferKind `json:"kind"`
			Value int       `json:"value"`
		}{o.Kind, o.Value})
	}
	return json.Marshal(struct {
		Kind OfferKind `json:"kind"`
	}{o.Kind})
}

// ContentBrief is the brief for content generation; its JSON form is the one
// persisted in campaigns.content_brief.
type ContentBrief struct {
	Goal   string `json:"goal"`
	Tone   string `json:"tone"`
	Length Length `json:"length"`
	Offer  Offer  `json:"offer"`
}

// Interpretation is a validated, normalized interpretation of a brief. All
// values are untrusted proposals that passed validation; the campaign service
// (CP6) persists Segment, Record() and ContentBrief.
type Interpretation struct {
	Segment           segment.Validated
	Summary           string
	Assumptions       []string
	Unsupported       []string
	SuggestedChannels []segment.Channel
	ContentBrief      ContentBrief
	Meta              Metadata
}

// Metadata describes how an interpretation was produced.
type Metadata struct {
	// Model is the model that produced the accepted output (llm.DemoModel
	// for demo output).
	Model         string
	PromptVersion string
	// Calls is the number of LLM calls made (1–3: initial, at most one
	// truncation retry, at most one repair turn).
	Calls    int
	Repaired bool
	Usage    llm.Usage
}

// Record is the persisted shape of campaigns.interpretation (§4).
type Record struct {
	Summary       string   `json:"summary"`
	Assumptions   []string `json:"assumptions"`
	Unsupported   []string `json:"unsupported"`
	Model         string   `json:"model"`
	PromptVersion string   `json:"prompt_version"`
}

// Record returns the persisted interpretation metadata.
func (in Interpretation) Record() Record {
	return Record{
		Summary:       in.Summary,
		Assumptions:   nonNilStrings(in.Assumptions),
		Unsupported:   nonNilStrings(in.Unsupported),
		Model:         in.Meta.Model,
		PromptVersion: in.Meta.PromptVersion,
	}
}

func nonNilStrings(s []string) []string {
	return append([]string{}, s...)
}

// ParseInterpretation is the full pipeline for one model output: bounded
// strict decode of the whole envelope, field-level semantic validation, and
// segment.Parse of the nested rule. Every problem is reported (the segment's
// paths are prefixed with "segment"); nothing is silently dropped or
// defaulted. The returned Errors is non-empty iff the output is unusable.
// Meta is left for the caller to fill.
func ParseInterpretation(data []byte) (Interpretation, segment.Errors) {
	if fe := scanJSON(data); fe != nil {
		return Interpretation{}, segment.Errors{*fe}
	}
	var errs segment.Errors
	root := object(&errs, "", data, envelopeKeys...)
	if root == nil {
		return Interpretation{}, errs
	}

	var out Interpretation

	if raw := root.get(keySegment, true); raw != nil {
		v, err := segment.Parse(raw)
		if err != nil {
			if se, ok := segment.AsErrors(err); ok {
				errs = append(errs, se.WithPrefix(keySegment)...)
			} else {
				addErr(&errs, keySegment, segment.CodeInvalidValue, "invalid segment rule")
			}
		} else {
			out.Segment = v
		}
	}

	if s, ok := text(&errs, keyInterpretation, root.get(keyInterpretation, true), MaxSummaryRunes); ok {
		out.Summary = s
	}
	out.Assumptions = notes(&errs, keyAssumptions, root.get(keyAssumptions, true))
	out.Unsupported = notes(&errs, keyUnsupported, root.get(keyUnsupported, true))
	out.SuggestedChannels = channels(&errs, keySuggestedChannels, root.get(keySuggestedChannels, true))
	if cb, ok := contentBrief(&errs, keyContentBrief, root.get(keyContentBrief, true)); ok {
		out.ContentBrief = cb
	}

	if len(errs) > 0 {
		return Interpretation{}, errs
	}
	return out, nil
}

// text decodes a required free-text string: NFC, trimmed, non-empty, at most
// maxRunes runes, no control characters other than tab and newline.
func text(errs *segment.Errors, path string, raw json.RawMessage, maxRunes int) (string, bool) {
	s, ok := str(errs, path, raw)
	if !ok {
		return "", false
	}
	s = strings.TrimSpace(norm.NFC.String(s))
	switch {
	case s == "":
		addErr(errs, path, segment.CodeEmpty, "must not be empty")
		return "", false
	case utf8.RuneCountInString(s) > maxRunes:
		addErr(errs, path, segment.CodeTooLong, "must be at most "+strconv.Itoa(maxRunes)+" characters")
		return "", false
	case strings.IndexFunc(s, func(r rune) bool { return unicode.IsControl(r) && r != '\n' && r != '\t' }) >= 0:
		addErr(errs, path, segment.CodeInvalidValue, "must not contain control characters")
		return "", false
	}
	return s, true
}

// notes decodes assumptions / unsupported: at most MaxNotes entries of at
// most MaxNoteRunes each. The result is non-nil when the array is valid.
func notes(errs *segment.Errors, path string, raw json.RawMessage) []string {
	items, ok := array(errs, path, raw)
	if !ok {
		return nil
	}
	if len(items) > MaxNotes {
		addErr(errs, path, segment.CodeTooMany, "must have at most "+strconv.Itoa(MaxNotes)+" entries")
		return nil
	}
	out := make([]string, 0, len(items))
	for i, it := range items {
		if s, ok := text(errs, indexPath(path, i), it, MaxNoteRunes); ok {
			out = append(out, s)
		}
	}
	return out
}

// channels decodes suggested_channels: a non-empty subset of {sms, email};
// duplicates are collapsed and the order is canonical (sms, email).
func channels(errs *segment.Errors, path string, raw json.RawMessage) []segment.Channel {
	items, ok := array(errs, path, raw)
	if !ok {
		return nil
	}
	if len(items) == 0 {
		addErr(errs, path, segment.CodeEmpty, "must contain at least one channel", channelValues...)
		return nil
	}
	if len(items) > segment.MaxListItems {
		addErr(errs, path, segment.CodeTooMany, "too many entries")
		return nil
	}
	var chs []segment.Channel
	valid := true
	for i, it := range items {
		s, ok := str(errs, indexPath(path, i), it)
		if !ok {
			valid = false
			continue
		}
		if !containsStr(channelValues, s) {
			addErr(errs, indexPath(path, i), segment.CodeNotAllowed, "unknown channel", channelValues...)
			valid = false
			continue
		}
		chs = append(chs, segment.Channel(s))
	}
	if !valid {
		return nil
	}
	out, err := segment.NormalizeChannels(chs)
	if err != nil { // unreachable: every entry was checked above
		addErr(errs, path, segment.CodeNotAllowed, "unknown channel", channelValues...)
		return nil
	}
	return out
}

func contentBrief(errs *segment.Errors, path string, raw json.RawMessage) (ContentBrief, bool) {
	if raw == nil {
		return ContentBrief{}, false
	}
	n := len(*errs)
	f := object(errs, path, raw, keyGoal, keyTone, keyLength, keyOffer)
	if f == nil {
		return ContentBrief{}, false
	}
	var cb ContentBrief
	cb.Goal, _ = text(errs, f.at(keyGoal), f.get(keyGoal, true), MaxGoalRunes)
	cb.Tone, _ = text(errs, f.at(keyTone), f.get(keyTone, true), MaxToneRunes)
	if s, ok := str(errs, f.at(keyLength), f.get(keyLength, true)); ok {
		if containsStr(lengthValues, s) {
			cb.Length = Length(s)
		} else {
			addErr(errs, f.at(keyLength), segment.CodeNotAllowed, "value is not allowed", lengthValues...)
		}
	}
	cb.Offer = offer(errs, f.at(keyOffer), f.get(keyOffer, true))
	return cb, len(*errs) == n
}

func offer(errs *segment.Errors, path string, raw json.RawMessage) Offer {
	if raw == nil {
		return Offer{}
	}
	f := object(errs, path, raw, keyKind, keyValue)
	if f == nil {
		return Offer{}
	}
	kind, ok := str(errs, f.at(keyKind), f.get(keyKind, true))
	if !ok {
		return Offer{}
	}
	if !containsStr(offerKindValues, kind) {
		addErr(errs, f.at(keyKind), segment.CodeNotAllowed, "value is not allowed", offerKindValues...)
		return Offer{}
	}
	o := Offer{Kind: OfferKind(kind)}
	valueRaw, hasValue := f.m[keyValue]
	if o.Kind != OfferPercentDiscount {
		if hasValue {
			addErr(errs, f.at(keyValue), segment.CodeNotAllowed, "value is only allowed for percent_discount")
		}
		return o
	}
	if !hasValue {
		addErr(errs, f.at(keyValue), segment.CodeRequired, "percent_discount requires an integer value")
		return o
	}
	if v, ok := integer(errs, f.at(keyValue), valueRaw); ok {
		if v < MinPercentDiscount || v > MaxPercentDiscount {
			addErr(errs, f.at(keyValue), segment.CodeOutOfRange,
				"must be between "+strconv.Itoa(MinPercentDiscount)+" and "+strconv.Itoa(MaxPercentDiscount))
		} else {
			o.Value = v
		}
	}
	return o
}

// maxContentBriefBytes bounds a stored content brief (goal ≤64 and tone ≤32
// runes plus a fixed structure).
const maxContentBriefBytes = 4 << 10

// ParseContentBrief strictly decodes and validates a content brief, e.g.
// campaigns.content_brief read back from the database (never trusted
// blindly). It applies exactly the rules of the interpretation envelope's
// content_brief; field paths have no prefix.
func ParseContentBrief(data []byte) (ContentBrief, segment.Errors) {
	if fe := scanJSONLimit(data, maxContentBriefBytes); fe != nil {
		return ContentBrief{}, segment.Errors{*fe}
	}
	var errs segment.Errors
	cb, ok := contentBrief(&errs, "", data)
	if !ok || len(errs) > 0 {
		return ContentBrief{}, errs
	}
	return cb, nil
}
