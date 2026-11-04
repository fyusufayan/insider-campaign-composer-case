package segment

import (
	"errors"
	"slices"
)

// Validated is a rule that passed strict decoding (when it came from JSON),
// semantic validation and normalization. Its zero value is not a rule
// (IsZero). Outside this package it can only be obtained from Normalize,
// Parse or UnmarshalJSON — all of which run the full validator — so the SQL
// compiler can require a Validated instead of re-checking a Rule.
type Validated struct {
	rule *Rule
}

// ErrNotValidated is returned when a zero Validated is used.
var ErrNotValidated = errors.New("segment: rule was not validated")

// Parse is the full pipeline for untrusted input:
// strict decode → semantic validation → normalization.
func Parse(data []byte) (Validated, error) {
	r, err := Decode(data)
	if err != nil {
		return Validated{}, err
	}
	return Normalize(r)
}

// Normalize validates r and returns its canonical form: version 1, city keys
// (Turkish-aware folding), upper-case country codes, lower-case enumerated
// values and categories, sorted and de-duplicated lists. Condition and group
// order are preserved. Normalize is idempotent: normalizing the encoding of a
// Validated yields an equal Validated. The returned error is an Errors value.
func Normalize(r Rule) (Validated, error) {
	if err := Check(r); err != nil {
		return Validated{}, err
	}
	n := Rule{Version: Version, Match: r.Match, Conditions: normalizeConditions(r.Conditions)}
	if len(r.Groups) > 0 {
		n.Groups = make([]Group, len(r.Groups))
		for i, g := range r.Groups {
			n.Groups[i] = Group{Match: g.Match, Conditions: normalizeConditions(g.Conditions)}
		}
	}
	return Validated{rule: &n}, nil
}

// IsZero reports whether v holds no rule.
func (v Validated) IsZero() bool { return v.rule == nil }

// Rule returns a deep copy of the normalized rule.
func (v Validated) Rule() Rule {
	if v.rule == nil {
		return Rule{}
	}
	return copyRule(*v.rule)
}

// MarshalJSON encodes the normalized rule (the form persisted in
// campaigns.segment_rule and campaign_approvals.segment_rule).
func (v Validated) MarshalJSON() ([]byte, error) {
	if v.rule == nil {
		return nil, ErrNotValidated
	}
	return v.rule.MarshalJSON()
}

// UnmarshalJSON runs the full Parse pipeline; stored rules are re-validated
// on load, never trusted.
func (v *Validated) UnmarshalJSON(b []byte) error {
	p, err := Parse(b)
	if err != nil {
		return err
	}
	*v = p
	return nil
}

func normalizeConditions(cs []Condition) []Condition {
	out := make([]Condition, len(cs))
	for i, c := range cs {
		switch c := c.(type) {
		case AttributeCondition:
			out[i] = normalizeAttribute(c)
		case EventCondition:
			if c.Count != nil {
				cnt := *c.Count
				c.Count = &cnt
			}
			if c.CategoryIn != nil {
				c.CategoryIn = canonicalSet(c.CategoryIn, canonicalCategory)
			}
			out[i] = c
		case BehaviorCondition:
			out[i] = c
		}
	}
	return out
}

func normalizeAttribute(c AttributeCondition) AttributeCondition {
	spec, _ := LookupField(c.Field) // validated
	switch c.Value.Kind {
	case ValueString:
		s, _ := spec.canonical(c.Value.Str)
		c.Value = StringValue(s)
	case ValueStrings:
		c.Value = StringsValue(canonicalSet(c.Value.Strs, spec.canonical)...)
	}
	return c
}

// canonicalSet maps every item to its canonical form, sorts and de-duplicates.
func canonicalSet(items []string, canon func(string) (string, bool)) []string {
	out := make([]string, len(items))
	for i, s := range items {
		out[i], _ = canon(s)
	}
	slices.Sort(out)
	return slices.Compact(out)
}

func copyRule(r Rule) Rule {
	out := Rule{Version: r.Version, Match: r.Match, Conditions: copyConditions(r.Conditions)}
	if r.Groups != nil {
		out.Groups = make([]Group, len(r.Groups))
		for i, g := range r.Groups {
			out.Groups[i] = Group{Match: g.Match, Conditions: copyConditions(g.Conditions)}
		}
	}
	return out
}

func copyConditions(cs []Condition) []Condition {
	if cs == nil {
		return nil
	}
	out := make([]Condition, len(cs))
	for i, c := range cs {
		switch c := c.(type) {
		case AttributeCondition:
			c.Value.Strs = slices.Clone(c.Value.Strs)
			out[i] = c
		case EventCondition:
			if c.Count != nil {
				cnt := *c.Count
				c.Count = &cnt
			}
			c.CategoryIn = slices.Clone(c.CategoryIn)
			out[i] = c
		default:
			out[i] = c
		}
	}
	return out
}
