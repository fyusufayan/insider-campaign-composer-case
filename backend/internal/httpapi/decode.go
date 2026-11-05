package httpapi

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"slices"
	"strconv"
	"unicode/utf8"
)

// Request-body limits.
const (
	maxBodyBytes = 64 << 10
	maxBodyDepth = 4
)

// Field error codes of request decoding (the shape matches segment's).
const (
	fieldMalformedJSON   = "malformed_json"
	fieldTrailingData    = "trailing_data"
	fieldTooLarge        = "too_large"
	fieldTooDeep         = "too_deep"
	fieldDuplicate       = "duplicate_property"
	fieldUnknownProperty = "unknown_property"
	fieldWrongType       = "wrong_type"
	fieldRequired        = "required"
	fieldOutOfRange      = "out_of_range"
	fieldNotAllowed      = "not_allowed"
	fieldInvalidValue    = "invalid_value"
)

// body is a strictly decoded JSON object request body: exactly one object,
// valid UTF-8, bounded size and depth, no duplicate keys at any level, only
// known properties. Typed accessors record field errors instead of failing
// fast, so every problem is reported together.
type body struct {
	fields map[string]json.RawMessage
	errs   []FieldError
}

// decodeBody reads and checks r's body. allowed lists the accepted top-level
// properties. With optional = true an empty body is accepted as {}.
// A non-nil *APIError means the body is unusable.
func decodeBody(w http.ResponseWriter, r *http.Request, optional bool, allowed ...string) (*body, *APIError) {
	data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	if err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			return nil, invalidBody(FieldError{Code: fieldTooLarge, Message: "request body exceeds " + strconv.Itoa(maxBodyBytes) + " bytes"})
		}
		return nil, invalidBody(FieldError{Code: fieldMalformedJSON, Message: "request body could not be read"})
	}
	if len(bytes.TrimSpace(data)) == 0 {
		if optional {
			return &body{fields: map[string]json.RawMessage{}}, nil
		}
		return nil, invalidBody(FieldError{Code: fieldRequired, Message: "a JSON object body is required"})
	}
	if apiErr := checkContentType(r); apiErr != nil {
		return nil, apiErr
	}
	if !utf8.Valid(data) {
		return nil, invalidBody(FieldError{Code: fieldMalformedJSON, Message: "request body is not valid UTF-8"})
	}
	if fe := scanBody(data); fe != nil {
		return nil, invalidBody(*fe)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil || fields == nil {
		return nil, invalidBody(FieldError{Code: fieldWrongType, Message: "request body must be a JSON object"})
	}
	b := &body{fields: fields}
	keys := make([]string, 0, len(fields))
	for k := range fields {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	for _, k := range keys {
		if !slices.Contains(allowed, k) {
			b.errs = append(b.errs, FieldError{Path: k, Code: fieldUnknownProperty, Message: "unknown property", Allowed: allowed})
		}
	}
	return b, nil
}

func checkContentType(r *http.Request) *APIError {
	mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mt != "application/json" {
		return &APIError{Status: http.StatusUnsupportedMediaType, Code: CodeUnsupportedMediaType,
			Message: "request body must be application/json"}
	}
	return nil
}

func invalidBody(fe FieldError) *APIError {
	return &APIError{Status: http.StatusUnprocessableEntity, Code: CodeValidationFailed,
		Message: "request body is invalid", Details: []FieldError{fe}}
}

// scanBody verifies that data is exactly one JSON object without duplicate
// keys, within the depth limit.
func scanBody(data []byte) *FieldError {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	tok, err := dec.Token()
	if err != nil {
		return &FieldError{Code: fieldMalformedJSON, Message: "malformed JSON"}
	}
	if d, ok := tok.(json.Delim); !ok || d != '{' {
		return &FieldError{Code: fieldWrongType, Message: "request body must be a JSON object"}
	}
	if fe := scanObject(dec, "", 1); fe != nil {
		return fe
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return &FieldError{Code: fieldTrailingData, Message: "unexpected data after the JSON object"}
	}
	return nil
}

// scanObject consumes an object whose '{' was read.
func scanObject(dec *json.Decoder, path string, depth int) *FieldError {
	if depth > maxBodyDepth {
		return &FieldError{Path: path, Code: fieldTooDeep, Message: "nesting is too deep"}
	}
	seen := map[string]bool{}
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return &FieldError{Path: path, Code: fieldMalformedJSON, Message: "malformed JSON"}
		}
		key, _ := tok.(string)
		p := joinPath(path, key)
		if seen[key] {
			return &FieldError{Path: p, Code: fieldDuplicate, Message: "duplicate property"}
		}
		seen[key] = true
		if fe := scanValue(dec, p, depth); fe != nil {
			return fe
		}
	}
	if _, err := dec.Token(); err != nil { // '}'
		return &FieldError{Path: path, Code: fieldMalformedJSON, Message: "malformed JSON"}
	}
	return nil
}

func scanValue(dec *json.Decoder, path string, depth int) *FieldError {
	tok, err := dec.Token()
	if err != nil {
		return &FieldError{Path: path, Code: fieldMalformedJSON, Message: "malformed JSON"}
	}
	d, ok := tok.(json.Delim)
	if !ok {
		return nil
	}
	switch d {
	case '{':
		return scanObject(dec, path, depth+1)
	case '[':
		if depth+1 > maxBodyDepth {
			return &FieldError{Path: path, Code: fieldTooDeep, Message: "nesting is too deep"}
		}
		for i := 0; dec.More(); i++ {
			if fe := scanValue(dec, path+"["+strconv.Itoa(i)+"]", depth+1); fe != nil {
				return fe
			}
		}
		if _, err := dec.Token(); err != nil { // ']'
			return &FieldError{Path: path, Code: fieldMalformedJSON, Message: "malformed JSON"}
		}
	}
	return nil
}

func joinPath(base, key string) string {
	if base == "" {
		return key
	}
	return base + "." + key
}

// err returns the collected problems as a 422, or nil.
func (b *body) err() *APIError {
	if len(b.errs) == 0 {
		return nil
	}
	return &APIError{Status: http.StatusUnprocessableEntity, Code: CodeValidationFailed,
		Message: "request body is invalid", Details: b.errs}
}

func (b *body) add(path, code, msg string, allowed ...string) {
	b.errs = append(b.errs, FieldError{Path: path, Code: code, Message: msg, Allowed: allowed})
}

// raw returns a required, non-null property.
func (b *body) raw(key, kind string) (json.RawMessage, bool) {
	v, ok := b.fields[key]
	if !ok {
		b.add(key, fieldRequired, "is required")
		return nil, false
	}
	if string(bytes.TrimSpace(v)) == "null" {
		b.add(key, fieldWrongType, "must be "+kind+", not null")
		return nil, false
	}
	return v, true
}

// string returns a required string property.
func (b *body) string(key string) (string, bool) {
	v, ok := b.raw(key, "a string")
	if !ok {
		return "", false
	}
	var s string
	if v[0] != '"' || json.Unmarshal(v, &s) != nil {
		b.add(key, fieldWrongType, "must be a string")
		return "", false
	}
	return s, true
}

// positiveInt returns a required integer property in 1..MaxInt32.
func (b *body) positiveInt(key string) (int, bool) {
	v, ok := b.raw(key, "an integer")
	if !ok {
		return 0, false
	}
	// A JSON integer literal only: no fraction, exponent, string or bool.
	n, err := strconv.ParseInt(string(bytes.TrimSpace(v)), 10, 64)
	if errors.Is(err, strconv.ErrRange) {
		b.add(key, fieldOutOfRange, "must be a positive integer")
		return 0, false
	}
	if err != nil {
		b.add(key, fieldWrongType, "must be an integer")
		return 0, false
	}
	if n < 1 || n > 1<<31-1 {
		b.add(key, fieldOutOfRange, "must be a positive integer")
		return 0, false
	}
	return int(n), true
}

// stringArray returns a required array of strings (possibly empty).
func (b *body) stringArray(key string) ([]string, bool) {
	v, ok := b.raw(key, "an array")
	if !ok {
		return nil, false
	}
	var items []json.RawMessage
	if v[0] != '[' || json.Unmarshal(v, &items) != nil {
		b.add(key, fieldWrongType, "must be an array of strings")
		return nil, false
	}
	out := make([]string, 0, len(items))
	valid := true
	for i, it := range items {
		var s string
		if len(it) == 0 || it[0] != '"' || json.Unmarshal(it, &s) != nil {
			b.add(key+"["+strconv.Itoa(i)+"]", fieldWrongType, "must be a string")
			valid = false
			continue
		}
		out = append(out, s)
	}
	return out, valid
}

// optionalString returns an optional string property: absent or null → nil.
func (b *body) optionalString(key string) *string {
	v, ok := b.fields[key]
	if !ok || string(bytes.TrimSpace(v)) == "null" {
		return nil
	}
	var s string
	if v[0] != '"' || json.Unmarshal(v, &s) != nil {
		b.add(key, fieldWrongType, "must be a string or null")
		return nil
	}
	return &s
}
