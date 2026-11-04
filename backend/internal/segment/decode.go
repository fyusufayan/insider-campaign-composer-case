package segment

import (
	"errors"
	"strconv"
)

// Decode strictly decodes an untrusted rule document into the typed DSL.
// It rejects malformed JSON, trailing data, duplicate or unknown properties
// (per condition variant), wrong JSON types (including null), missing
// required properties, invalid structural enum values (match, type, op,
// event, predicate) and nested groups. It does not check the field registry,
// ranges or limits; use Parse (or Normalize) to obtain a usable rule.
//
// The returned error is an Errors value.
func Decode(data []byte) (Rule, error) {
	root, ferr := parseJSON(data)
	if ferr != nil {
		return Rule{}, Errors{*ferr}
	}
	d := &decoder{}
	r := d.rule(root)
	if errs := d.result(); len(errs) > 0 {
		return Rule{}, errs
	}
	return r, nil
}

// AsErrors extracts Errors from an error returned by this package.
func AsErrors(err error) (Errors, bool) {
	var errs Errors
	ok := errors.As(err, &errs)
	return errs, ok
}

type decoder struct{ collector }

// object is a JSON object being decoded against a fixed property list.
type object struct {
	d     *decoder
	path  string
	n     *jsonNode
	known []string
}

// object checks that n is an object whose keys are all in known; unknown
// keys are reported in input order (special codes via override).
func (d *decoder) object(path string, n *jsonNode, override func(key string) (code, msg string, ok bool), known ...string) *object {
	if n.kind != jsonObject {
		d.add(path, CodeWrongType, "must be an object, got "+n.kind.String())
		return nil
	}
	for _, k := range n.keys {
		if contains(known, k) {
			continue
		}
		if override != nil {
			if code, msg, ok := override(k); ok {
				d.add(joinKey(path, k), code, msg)
				continue
			}
		}
		d.add(joinKey(path, k), CodeUnknownProperty, "unknown property", known...)
	}
	return &object{d: d, path: path, n: n, known: known}
}

// get returns the property node; a missing required property is reported.
func (o *object) get(key string, required bool) *jsonNode {
	v, ok := o.n.obj[key]
	if !ok && required {
		o.d.add(joinKey(o.path, key), CodeRequired, "required property is missing")
	}
	return v
}

func (o *object) at(key string) string { return joinKey(o.path, key) }

// enum decodes a string from a closed set. consent selects the consent
// detector used for a better error code on non-members (may be nil).
func (d *decoder) enum(path string, n *jsonNode, allowed []string, consent func(string) bool) string {
	if n == nil {
		return ""
	}
	if n.kind != jsonString {
		d.add(path, CodeWrongType, "must be a string, got "+n.kind.String())
		return ""
	}
	if !contains(allowed, n.str) {
		if consent != nil && consent(n.str) {
			d.add(path, CodeConsentBackendEnforced, consentMessage)
		} else {
			d.add(path, CodeNotAllowed, "value is not allowed", allowed...)
		}
		return ""
	}
	return n.str
}

const consentMessage = "consent and contact availability are enforced by the backend and cannot be part of a segment rule"

func (d *decoder) str(path string, n *jsonNode) string {
	if n == nil {
		return ""
	}
	if n.kind != jsonString {
		d.add(path, CodeWrongType, "must be a string, got "+n.kind.String())
		return ""
	}
	return n.str
}

// integer accepts only JSON integers in the int32 range (no fraction, no exponent).
func (d *decoder) integer(path string, n *jsonNode) int {
	if n == nil {
		return 0
	}
	if n.kind != jsonNumber {
		d.add(path, CodeWrongType, "must be an integer, got "+n.kind.String())
		return 0
	}
	i, err := strconv.ParseInt(string(n.num), 10, 32)
	if err != nil {
		if errors.Is(err, strconv.ErrRange) {
			d.add(path, CodeOutOfRange, "integer is out of range")
		} else {
			d.add(path, CodeWrongType, "must be an integer")
		}
		return 0
	}
	return int(i)
}

// strings decodes an array of strings; the result is non-nil.
func (d *decoder) strings(path string, n *jsonNode) []string {
	if n == nil {
		return nil
	}
	if n.kind != jsonArray {
		d.add(path, CodeWrongType, "must be an array of strings, got "+n.kind.String())
		return nil
	}
	out := make([]string, 0, len(n.arr))
	for i, e := range n.arr {
		out = append(out, d.str(joinIndex(path, i), e))
	}
	return out
}

func (d *decoder) rule(n *jsonNode) Rule {
	o := d.object("", n, nil, "version", "match", "conditions", "groups")
	if o == nil {
		return Rule{}
	}
	var r Rule
	if v := o.get("version", false); v != nil {
		r.Version = d.integer(o.at("version"), v)
	}
	r.Match = Match(d.enum(o.at("match"), o.get("match", true), matchValues, nil))
	r.Conditions = d.conditions(o.at("conditions"), o.get("conditions", true))
	if v := o.get("groups", false); v != nil {
		if v.kind != jsonArray {
			d.add(o.at("groups"), CodeWrongType, "must be an array of groups, got "+v.kind.String())
		} else {
			r.Groups = make([]Group, 0, len(v.arr))
			for i, g := range v.arr {
				r.Groups = append(r.Groups, d.group(joinIndex(o.at("groups"), i), g))
			}
		}
	}
	return r
}

func nestedGroupOverride(key string) (string, string, bool) {
	if key == "groups" {
		return CodeNestedGroups, "groups cannot contain groups", true
	}
	return "", "", false
}

func (d *decoder) group(path string, n *jsonNode) Group {
	o := d.object(path, n, nestedGroupOverride, "match", "conditions")
	if o == nil {
		return Group{}
	}
	return Group{
		Match:      Match(d.enum(o.at("match"), o.get("match", true), matchValues, nil)),
		Conditions: d.conditions(o.at("conditions"), o.get("conditions", true)),
	}
}

// conditions decodes an array of conditions; the result is non-nil when the
// node is an array.
func (d *decoder) conditions(path string, n *jsonNode) []Condition {
	if n == nil {
		return nil
	}
	if n.kind != jsonArray {
		d.add(path, CodeWrongType, "must be an array of conditions, got "+n.kind.String())
		return nil
	}
	out := make([]Condition, 0, len(n.arr))
	for i, c := range n.arr {
		out = append(out, d.condition(joinIndex(path, i), c))
	}
	return out
}

// condition dispatches on the "type" discriminator; each variant is then
// decoded against its own property list. An unknown discriminator stops
// decoding of that element (its other properties have no defined meaning).
func (d *decoder) condition(path string, n *jsonNode) Condition {
	if n.kind != jsonObject {
		d.add(path, CodeWrongType, "must be an object, got "+n.kind.String())
		return nil
	}
	tn, ok := n.obj["type"]
	tp := joinKey(path, "type")
	switch {
	case !ok:
		d.add(tp, CodeRequired, "required property is missing", conditionTypes...)
		return nil
	case tn.kind != jsonString:
		d.add(tp, CodeWrongType, "must be a string, got "+tn.kind.String())
		return nil
	}
	switch ConditionType(tn.str) {
	case ConditionAttribute:
		return d.attribute(path, n)
	case ConditionEvent:
		return d.event(path, n)
	case ConditionBehavior:
		return d.behavior(path, n)
	}
	switch {
	case tn.str == "group":
		d.add(tp, CodeNestedGroups, "groups are only allowed in the rule's top-level \"groups\" array")
	case looksLikeConsentOrContact(tn.str):
		d.add(tp, CodeConsentBackendEnforced, consentMessage)
	default:
		d.add(tp, CodeNotAllowed, "unknown condition type", conditionTypes...)
	}
	return nil
}

func (d *decoder) attribute(path string, n *jsonNode) Condition {
	o := d.object(path, n, nil, "type", "field", "op", "value")
	c := AttributeCondition{
		Field: Field(d.str(o.at("field"), o.get("field", true))),
		Op:    Operator(d.enum(o.at("op"), o.get("op", true), operatorValues, nil)),
	}
	if v := o.get("value", true); v != nil {
		c.Value = d.value(o.at("value"), v)
	}
	return c
}

// value decodes the JSON shape of an attribute value. Whether the shape
// fits the operator is a semantic check (Validate).
func (d *decoder) value(path string, n *jsonNode) Value {
	switch n.kind {
	case jsonString:
		return StringValue(n.str)
	case jsonNumber:
		return IntValue(d.integer(path, n))
	case jsonArray:
		return StringsValue(d.strings(path, n)...)
	case jsonObject:
		o := d.object(path, n, nil, "min", "max")
		return RangeValue(d.integer(o.at("min"), o.get("min", true)), d.integer(o.at("max"), o.get("max", true)))
	default:
		d.add(path, CodeWrongType, "must be a string, an array of strings, an integer or {\"min\", \"max\"}, got "+n.kind.String())
		return Value{}
	}
}

func (d *decoder) event(path string, n *jsonNode) Condition {
	o := d.object(path, n, nil, "type", "event", "op", "count", "within_days", "category_in")
	c := EventCondition{
		Event:      EventName(d.enum(o.at("event"), o.get("event", true), eventNameValues, looksLikeConsent)),
		Op:         EventOp(d.enum(o.at("op"), o.get("op", true), eventOpValues, nil)),
		WithinDays: d.integer(o.at("within_days"), o.get("within_days", true)),
	}
	if v := o.get("count", false); v != nil {
		cnt := d.integer(o.at("count"), v)
		c.Count = &cnt
	}
	if v := o.get("category_in", false); v != nil {
		c.CategoryIn = d.strings(o.at("category_in"), v)
	}
	return c
}

func (d *decoder) behavior(path string, n *jsonNode) Condition {
	o := d.object(path, n, nil, "type", "predicate", "within_days")
	return BehaviorCondition{
		Predicate:  BehaviorPredicate(d.enum(o.at("predicate"), o.get("predicate", true), predicateValues, looksLikeConsent)),
		WithinDays: d.integer(o.at("within_days"), o.get("within_days", true)),
	}
}
