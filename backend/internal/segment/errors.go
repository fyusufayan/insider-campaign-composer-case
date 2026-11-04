package segment

import (
	"strconv"
	"strings"
)

// Validation error codes. Paths are relative to the rule root and use the
// API's notation: "groups[1].conditions[0].value". The root itself is "".
const (
	CodeMalformedJSON          = "malformed_json"
	CodeTrailingData           = "trailing_data"
	CodeTooLarge               = "too_large"
	CodeTooDeep                = "too_deep"
	CodeDuplicateProperty      = "duplicate_property"
	CodeUnknownProperty        = "unknown_property"
	CodeWrongType              = "wrong_type"
	CodeRequired               = "required"
	CodeNotAllowed             = "not_allowed"
	CodeUnknownField           = "unknown_field"
	CodeOperatorNotAllowed     = "operator_not_allowed"
	CodeNestedGroups           = "nested_groups_not_supported"
	CodeConsentBackendEnforced = "consent_is_backend_enforced"
	CodeUnsupportedVersion     = "unsupported_version"
	CodeOutOfRange             = "out_of_range"
	CodeInvalidRange           = "invalid_range"
	CodeTooMany                = "too_many"
	CodeEmpty                  = "empty"
	CodeTooLong                = "too_long"
	CodeInvalidValue           = "invalid_value"
	CodeUnknownCity            = "unknown_city"
	CodeUnknownCountry         = "unknown_country"
	CodeUnknownCategory        = "unknown_category"
	CodeCountRequired          = "count_required"
	CodeCountNotAllowed        = "count_not_allowed"
)

// FieldError is one problem at a JSON path. Its JSON shape is identical to
// httpapi.FieldError (the API error envelope's details entries); messages
// never echo the offending input.
type FieldError struct {
	Path    string   `json:"path"`
	Code    string   `json:"code"`
	Message string   `json:"message"`
	Allowed []string `json:"allowed,omitempty"`
}

// Errors is a deterministically ordered list of FieldErrors. Functions of
// this package that return error never return an empty (typed-nil) Errors:
// a non-nil error always holds at least one FieldError. Validate returns
// Errors directly; use Check when an error value is needed.
type Errors []FieldError

func (e Errors) Error() string {
	var b strings.Builder
	b.WriteString("segment: invalid rule")
	for i, fe := range e {
		if i == 3 {
			b.WriteString("; …")
			break
		}
		b.WriteString("; ")
		if fe.Path != "" {
			b.WriteString(fe.Path)
			b.WriteString(": ")
		}
		b.WriteString(fe.Code)
	}
	return b.String()
}

// WithPrefix returns a copy whose paths are nested under prefix, for rules
// embedded in a larger document (e.g. "segment").
func (e Errors) WithPrefix(prefix string) Errors {
	out := make(Errors, len(e))
	for i, fe := range e {
		fe.Path = joinKey(prefix, fe.Path)
		if fe.Allowed != nil {
			fe.Allowed = append([]string(nil), fe.Allowed...)
		}
		out[i] = fe
	}
	return out
}

func joinKey(base, key string) string {
	switch {
	case base == "":
		return key
	case key == "":
		return base
	case strings.HasPrefix(key, "["):
		return base + key
	default:
		return base + "." + key
	}
}

func joinIndex(base string, i int) string { return base + "[" + strconv.Itoa(i) + "]" }

// collector accumulates errors in traversal order.
type collector struct{ errs Errors }

func (c *collector) add(path, code, msg string, allowed ...string) {
	c.errs = append(c.errs, FieldError{Path: path, Code: code, Message: msg, Allowed: allowed})
}

func (c *collector) result() Errors {
	if len(c.errs) == 0 {
		return nil
	}
	return c.errs
}
