package ai

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"slices"
	"strconv"
	"unicode/utf8"

	"github.com/fyusufayan/insider-campaign-composer-case/backend/internal/segment"
)

// Strict JSON helpers for the interpretation envelope. The nested segment is
// decoded by segment.Parse (its own strict tree decoder); this file covers
// the envelope around it with the same guarantees: one JSON value, no
// duplicate or unknown properties, no null for required values, exact types.

const (
	// maxEnvelopeBytes bounds the accepted model output.
	maxEnvelopeBytes = 128 << 10
	// maxEnvelopeDepth: envelope → segment → groups → group → conditions →
	// condition → value plus headroom; the segment decoder enforces its own.
	maxEnvelopeDepth = 10
)

// scanJSON checks that data is exactly one well-formed JSON value of bounded
// size and depth, valid UTF-8, without duplicate object keys at any level.
func scanJSON(data []byte) *segment.FieldError { return scanJSONLimit(data, maxEnvelopeBytes) }

// scanJSONLimit is scanJSON with an explicit size bound.
func scanJSONLimit(data []byte, maxBytes int) *segment.FieldError {
	if len(data) > maxBytes {
		return &segment.FieldError{Code: segment.CodeTooLarge, Message: "output exceeds " + strconv.Itoa(maxBytes) + " bytes"}
	}
	if !utf8.Valid(data) {
		return &segment.FieldError{Code: segment.CodeMalformedJSON, Message: "output is not valid UTF-8"}
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	if fe := scanValue(dec, "", 1); fe != nil {
		return fe
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return &segment.FieldError{Code: segment.CodeTrailingData, Message: "unexpected data after the JSON value"}
	}
	return nil
}

func syntaxErr(path string, err error) *segment.FieldError {
	msg := "malformed JSON"
	var se *json.SyntaxError
	switch {
	case errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF):
		msg = "unexpected end of JSON input"
	case errors.As(err, &se):
		msg = "malformed JSON at byte offset " + strconv.FormatInt(se.Offset, 10)
	}
	return &segment.FieldError{Path: path, Code: segment.CodeMalformedJSON, Message: msg}
}

func scanValue(dec *json.Decoder, path string, depth int) *segment.FieldError {
	tok, err := dec.Token()
	if err != nil {
		return syntaxErr(path, err)
	}
	d, ok := tok.(json.Delim)
	if !ok {
		return nil
	}
	if depth > maxEnvelopeDepth {
		return &segment.FieldError{Path: path, Code: segment.CodeTooDeep, Message: "JSON nesting exceeds " + strconv.Itoa(maxEnvelopeDepth) + " levels"}
	}
	switch d {
	case '{':
		seen := map[string]struct{}{}
		for dec.More() {
			kt, err := dec.Token()
			if err != nil {
				return syntaxErr(path, err)
			}
			key, _ := kt.(string)
			kp := joinPath(path, key)
			if _, dup := seen[key]; dup {
				return &segment.FieldError{Path: kp, Code: segment.CodeDuplicateProperty, Message: "property appears more than once"}
			}
			seen[key] = struct{}{}
			if fe := scanValue(dec, kp, depth+1); fe != nil {
				return fe
			}
		}
	case '[':
		for i := 0; dec.More(); i++ {
			if fe := scanValue(dec, indexPath(path, i), depth+1); fe != nil {
				return fe
			}
		}
	default:
		return syntaxErr(path, nil)
	}
	if _, err := dec.Token(); err != nil {
		return syntaxErr(path, err)
	}
	return nil
}

func joinPath(base, key string) string {
	if base == "" {
		return key
	}
	return base + "." + key
}

func indexPath(base string, i int) string { return base + "[" + strconv.Itoa(i) + "]" }

// jsonKindOf reports the JSON type of an already well-formed raw value.
func jsonKindOf(raw json.RawMessage) string {
	b := bytes.TrimLeft(raw, " \t\r\n")
	if len(b) == 0 {
		return "missing"
	}
	switch b[0] {
	case '{':
		return "object"
	case '[':
		return "array"
	case '"':
		return "string"
	case 't', 'f':
		return "boolean"
	case 'n':
		return "null"
	default:
		return "number"
	}
}

// fields is a decoded JSON object checked against a fixed property list.
type fields struct {
	errs *segment.Errors
	path string
	m    map[string]json.RawMessage
}

func addErr(errs *segment.Errors, path, code, msg string, allowed ...string) {
	*errs = append(*errs, segment.FieldError{Path: path, Code: code, Message: msg, Allowed: allowed})
}

// object decodes raw as an object with exactly the known properties allowed;
// unknown properties are reported in sorted order. It returns nil if raw is
// not an object.
func object(errs *segment.Errors, path string, raw json.RawMessage, known ...string) *fields {
	if k := jsonKindOf(raw); k != "object" {
		addErr(errs, path, segment.CodeWrongType, "must be an object, got "+k)
		return nil
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		addErr(errs, path, segment.CodeMalformedJSON, "malformed JSON object")
		return nil
	}
	var unknown []string
	for k := range m {
		if !containsStr(known, k) {
			unknown = append(unknown, k)
		}
	}
	slices.Sort(unknown)
	for _, k := range unknown {
		addErr(errs, joinPath(path, k), segment.CodeUnknownProperty, "unknown property", known...)
	}
	return &fields{errs: errs, path: path, m: m}
}

// get returns the raw property; a missing or null required property is
// reported and nil returned.
func (f *fields) get(key string, required bool) json.RawMessage {
	raw, ok := f.m[key]
	if !ok {
		if required {
			addErr(f.errs, f.at(key), segment.CodeRequired, "required property is missing")
		}
		return nil
	}
	if jsonKindOf(raw) == "null" {
		addErr(f.errs, f.at(key), segment.CodeWrongType, "must not be null")
		return nil
	}
	return raw
}

func (f *fields) at(key string) string { return joinPath(f.path, key) }

// str decodes a JSON string (ok=false after reporting a type error).
func str(errs *segment.Errors, path string, raw json.RawMessage) (string, bool) {
	if raw == nil {
		return "", false
	}
	if k := jsonKindOf(raw); k != "string" {
		addErr(errs, path, segment.CodeWrongType, "must be a string, got "+k)
		return "", false
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		addErr(errs, path, segment.CodeMalformedJSON, "malformed string")
		return "", false
	}
	return s, true
}

// integer decodes a JSON integer in the int32 range.
func integer(errs *segment.Errors, path string, raw json.RawMessage) (int, bool) {
	if raw == nil {
		return 0, false
	}
	if k := jsonKindOf(raw); k != "number" {
		addErr(errs, path, segment.CodeWrongType, "must be an integer, got "+k)
		return 0, false
	}
	i, err := strconv.ParseInt(string(bytes.TrimSpace(raw)), 10, 32)
	if err != nil {
		if errors.Is(err, strconv.ErrRange) {
			addErr(errs, path, segment.CodeOutOfRange, "integer is out of range")
		} else {
			addErr(errs, path, segment.CodeWrongType, "must be an integer")
		}
		return 0, false
	}
	return int(i), true
}

// array decodes a JSON array into its raw elements.
func array(errs *segment.Errors, path string, raw json.RawMessage) ([]json.RawMessage, bool) {
	if raw == nil {
		return nil, false
	}
	if k := jsonKindOf(raw); k != "array" {
		addErr(errs, path, segment.CodeWrongType, "must be an array, got "+k)
		return nil, false
	}
	var items []json.RawMessage
	if err := json.Unmarshal(raw, &items); err != nil {
		addErr(errs, path, segment.CodeMalformedJSON, "malformed array")
		return nil, false
	}
	if items == nil {
		items = []json.RawMessage{}
	}
	return items, true
}

func containsStr(set []string, s string) bool {
	for _, v := range set {
		if v == s {
			return true
		}
	}
	return false
}
