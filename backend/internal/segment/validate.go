package segment

import (
	"strconv"
	"strings"
	"unicode/utf8"
)

// Check is Validate for callers that use the standard error interface: it
// returns a nil error when r is valid and an Errors value otherwise.
//
//	if err := segment.Check(r); err != nil { … }  // safe
//	var err error = segment.Validate(r)           // WRONG: typed nil when valid
func Check(r Rule) error {
	if errs := Validate(r); len(errs) > 0 {
		return errs
	}
	return nil
}

// Validate performs the registry-driven semantic validation of a rule. It is
// the single validator for every rule source (model output, manual edits,
// stored rules) and also re-checks everything Decode checks, so a Rule built
// in Go is held to the same standard. Errors are deterministic: traversal
// order, one entry per problem; invalid conditions are reported, never dropped.
//
// Validate returns nil when r is valid. Because Errors implements error, do
// not assign its result to an error variable (a nil Errors stored in an
// error interface is non-nil); use Check for that.
func Validate(r Rule) Errors {
	v := &validator{}
	v.rule(r)
	return v.result()
}

type validator struct{ collector }

func (v *validator) rule(r Rule) {
	if r.Version != 0 && r.Version != Version {
		v.add("version", CodeUnsupportedVersion, "unsupported rule version", strconv.Itoa(Version))
	}
	v.match("match", r.Match)
	v.conditionList("conditions", r.Conditions)
	total := len(r.Conditions)
	if len(r.Groups) > MaxGroups {
		v.add("groups", CodeTooMany, "at most "+strconv.Itoa(MaxGroups)+" groups are allowed")
	}
	for i, g := range r.Groups {
		p := joinIndex("groups", i)
		v.match(joinKey(p, "match"), g.Match)
		if len(g.Conditions) == 0 {
			v.add(joinKey(p, "conditions"), CodeEmpty, "a group needs at least one condition")
		}
		v.conditionList(joinKey(p, "conditions"), g.Conditions)
		total += len(g.Conditions)
	}
	if total == 0 {
		v.add("conditions", CodeEmpty, "a rule needs at least one condition")
	}
}

func (v *validator) match(path string, m Match) {
	if m != MatchAll && m != MatchAny {
		v.add(path, CodeNotAllowed, "value is not allowed", matchValues...)
	}
}

func (v *validator) conditionList(path string, cs []Condition) {
	if len(cs) > MaxConditionsPerGroup {
		v.add(path, CodeTooMany, "at most "+strconv.Itoa(MaxConditionsPerGroup)+" conditions are allowed")
	}
	for i, c := range cs {
		p := joinIndex(path, i)
		switch c := c.(type) {
		case AttributeCondition:
			v.attribute(p, c)
		case EventCondition:
			v.event(p, c)
		case BehaviorCondition:
			v.behavior(p, c)
		default:
			v.add(joinKey(p, "type"), CodeNotAllowed, "unknown condition type", conditionTypes...)
		}
	}
}

func (v *validator) attribute(path string, c AttributeCondition) {
	spec, ok := LookupField(c.Field)
	if !ok {
		fp := joinKey(path, "field")
		switch {
		case c.Field == "":
			v.add(fp, CodeRequired, "field is required", fieldNameValues...)
		case looksLikeConsentOrContact(string(c.Field)):
			v.add(fp, CodeConsentBackendEnforced, consentMessage)
		default:
			v.add(fp, CodeUnknownField, "unknown field", fieldNameValues...)
		}
		return
	}
	if !contains(operatorValues, string(c.Op)) {
		v.add(joinKey(path, "op"), CodeNotAllowed, "value is not allowed", operatorValues...)
		return
	}
	if !spec.allows(c.Op) {
		v.add(joinKey(path, "op"), CodeOperatorNotAllowed, "operator is not allowed for field "+string(c.Field), opStrings(spec.Operators)...)
		return
	}
	vp := joinKey(path, "value")
	want := valueKindFor(c.Op)
	if c.Value.Kind != want {
		v.add(vp, CodeWrongType, "operator "+string(c.Op)+" requires "+kindDescription(want, spec.Type))
		return
	}
	switch want {
	case ValueString:
		v.stringValue(vp, spec, c.Value.Str)
	case ValueStrings:
		v.list(vp, c.Value.Strs, func(p, s string) { v.stringValue(p, spec, s) })
	case ValueInt:
		v.intValue(vp, spec.Min, spec.Max, c.Value.Int)
	case ValueRange:
		v.intValue(joinKey(vp, "min"), spec.Min, spec.Max, c.Value.Min)
		v.intValue(joinKey(vp, "max"), spec.Min, spec.Max, c.Value.Max)
		if c.Value.Min > c.Value.Max {
			v.add(vp, CodeInvalidRange, "min must not be greater than max")
		}
	}
}

// valueKindFor is the value shape each operator requires.
func valueKindFor(op Operator) ValueKind {
	switch op {
	case OpEq, OpNeq:
		return ValueString
	case OpIn, OpNotIn:
		return ValueStrings
	case OpGte, OpLte:
		return ValueInt
	case OpBetween:
		return ValueRange
	}
	return ValueInvalid
}

func kindDescription(k ValueKind, t ValueType) string {
	switch k {
	case ValueString:
		return "a " + string(t)
	case ValueStrings:
		return "an array of " + string(t) + "s"
	case ValueInt:
		return "an integer"
	default:
		return "an object {\"min\", \"max\"}"
	}
}

func (v *validator) stringValue(path string, spec FieldSpec, s string) {
	switch {
	case strings.TrimSpace(s) == "":
		v.add(path, CodeInvalidValue, "must not be empty")
	case utf8.RuneCountInString(s) > MaxValueRunes:
		v.add(path, CodeTooLong, "must be at most "+strconv.Itoa(MaxValueRunes)+" characters")
	default:
		if _, ok := spec.canonical(s); !ok {
			v.add(path, spec.invalidCode, spec.invalidMsg, spec.Values...)
		}
	}
}

func (v *validator) intValue(path string, minV, maxV, i int) {
	if i < minV || i > maxV {
		v.add(path, CodeOutOfRange, "must be between "+strconv.Itoa(minV)+" and "+strconv.Itoa(maxV))
	}
}

// list checks a present (non-nil) list: 1..MaxListItems items, each checked by item.
func (v *validator) list(path string, items []string, item func(path, s string)) {
	switch {
	case len(items) == 0:
		v.add(path, CodeEmpty, "list must not be empty")
		return
	case len(items) > MaxListItems:
		v.add(path, CodeTooMany, "at most "+strconv.Itoa(MaxListItems)+" items are allowed")
		return
	}
	for i, s := range items {
		item(joinIndex(path, i), s)
	}
}

func (v *validator) event(path string, c EventCondition) {
	if !contains(eventNameValues, string(c.Event)) {
		v.add(joinKey(path, "event"), CodeNotAllowed, "value is not allowed", eventNameValues...)
	}
	cp := joinKey(path, "count")
	switch c.Op {
	case EventCountGte, EventCountLte:
		if c.Count == nil {
			v.add(cp, CodeCountRequired, "count is required for operator "+string(c.Op))
		} else {
			v.intValue(cp, MinEventCount, MaxEventCount, *c.Count)
		}
	case EventExists, EventNotExists:
		if c.Count != nil {
			v.add(cp, CodeCountNotAllowed, "count is only allowed with count_gte and count_lte")
		}
	default:
		v.add(joinKey(path, "op"), CodeNotAllowed, "value is not allowed", eventOpValues...)
	}
	v.intValue(joinKey(path, "within_days"), MinWithinDays, MaxWithinDays, c.WithinDays)
	if c.CategoryIn != nil {
		v.list(joinKey(path, "category_in"), c.CategoryIn, func(p, s string) {
			switch {
			case utf8.RuneCountInString(s) > MaxValueRunes:
				v.add(p, CodeTooLong, "must be at most "+strconv.Itoa(MaxValueRunes)+" characters")
			default:
				if _, ok := canonicalCategory(s); !ok {
					v.add(p, CodeUnknownCategory, "unknown product category", categories...)
				}
			}
		})
	}
}

func (v *validator) behavior(path string, c BehaviorCondition) {
	if c.Predicate != PredicateAbandonedCart {
		v.add(joinKey(path, "predicate"), CodeNotAllowed, "value is not allowed", predicateValues...)
	}
	v.intValue(joinKey(path, "within_days"), MinWithinDays, MaxWithinDays, c.WithinDays)
}

func opStrings(ops []Operator) []string {
	out := make([]string, len(ops))
	for i, o := range ops {
		out[i] = string(o)
	}
	return out
}
