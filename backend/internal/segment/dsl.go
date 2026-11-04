package segment

import (
	"encoding/json"
	"fmt"
)

// Version is the only supported DSL version. Normalized rules always carry it.
const Version = 1

// Match combines the conditions of a rule or group.
type Match string

// Match values.
const (
	MatchAll Match = "all"
	MatchAny Match = "any"
)

// ConditionType is the "type" discriminator of a condition.
type ConditionType string

// Condition types.
const (
	ConditionAttribute ConditionType = "attribute"
	ConditionEvent     ConditionType = "event"
	ConditionBehavior  ConditionType = "behavior"
)

// Field is an attribute field name from the registry (registry.go).
type Field string

// Attribute fields.
const (
	FieldCity              Field = "city"
	FieldCountry           Field = "country"
	FieldGender            Field = "gender"
	FieldAge               Field = "age"
	FieldSignupDaysAgo     Field = "signup_days_ago"
	FieldLastActiveDaysAgo Field = "last_active_days_ago"
	FieldLanguage          Field = "language"
)

// Operator is an attribute operator.
type Operator string

// Attribute operators.
const (
	OpEq      Operator = "eq"
	OpNeq     Operator = "neq"
	OpIn      Operator = "in"
	OpNotIn   Operator = "not_in"
	OpGte     Operator = "gte"
	OpLte     Operator = "lte"
	OpBetween Operator = "between"
)

// EventName is a tracked event kind.
type EventName string

// Event names.
const (
	EventCartAdd  EventName = "cart_add"
	EventPurchase EventName = "purchase"
)

// EventOp is an event operator.
type EventOp string

// Event operators. count_gte / count_lte require Count; exists / not_exists forbid it.
const (
	EventExists    EventOp = "exists"
	EventNotExists EventOp = "not_exists"
	EventCountGte  EventOp = "count_gte"
	EventCountLte  EventOp = "count_lte"
)

// BehaviorPredicate is a named behavioural predicate.
type BehaviorPredicate string

// Behavioural predicates. abandoned_cart(N): a cart event in the last N days
// with no purchase at or after that cart event.
const PredicateAbandonedCart BehaviorPredicate = "abandoned_cart"

// Rule is a decoded segment rule (DSL v1). A Rule may be invalid; only a
// Validated (see Normalize / Parse) is guaranteed valid and normalized.
type Rule struct {
	// Version is 0 when absent in the input, otherwise must be 1.
	Version    int
	Match      Match
	Conditions []Condition
	// Groups do not nest: a Group holds conditions only.
	Groups []Group
}

// Group is a non-nesting group of conditions.
type Group struct {
	Match      Match
	Conditions []Condition
}

// Condition is one of AttributeCondition, EventCondition or BehaviorCondition.
// The interface is sealed.
type Condition interface {
	Type() ConditionType
	isCondition()
}

// AttributeCondition compares a user attribute with a value.
type AttributeCondition struct {
	Field Field
	Op    Operator
	Value Value
}

// EventCondition tests the existence or count of events in a window.
//
// Count semantics (binding for the SQL compiler): the count is the number of
// the user's events of kind Event within WithinDays (and in CategoryIn, when
// present); a user with no matching events has count 0. Hence, for the same
// event, window and categories, count_lte 0 ≡ not_exists, count_gte 1 ≡
// exists, and count_gte 0 matches every user.
type EventCondition struct {
	Event EventName
	Op    EventOp
	// Count is required for count_gte / count_lte and forbidden otherwise.
	Count      *int
	WithinDays int
	// CategoryIn is nil when absent; when present it must be non-empty.
	CategoryIn []string
}

// BehaviorCondition is a named behavioural predicate over a window.
type BehaviorCondition struct {
	Predicate  BehaviorPredicate
	WithinDays int
}

func (AttributeCondition) Type() ConditionType { return ConditionAttribute }
func (EventCondition) Type() ConditionType     { return ConditionEvent }
func (BehaviorCondition) Type() ConditionType  { return ConditionBehavior }

func (AttributeCondition) isCondition() {}
func (EventCondition) isCondition()     {}
func (BehaviorCondition) isCondition()  {}

// ValueKind is the JSON shape of an attribute value.
type ValueKind uint8

// Value kinds. Which kind an operator requires is fixed: eq/neq → String,
// in/not_in → Strings, gte/lte → Int, between → Range.
const (
	ValueInvalid ValueKind = iota
	ValueString            // "Istanbul"
	ValueStrings           // ["tr", "en"]
	ValueInt               // 30
	ValueRange             // {"min": 18, "max": 35}, inclusive
)

// Value is an attribute value; only the fields of its Kind are meaningful.
type Value struct {
	Kind ValueKind
	Str  string
	Strs []string
	Int  int
	Min  int
	Max  int
}

// StringValue, StringsValue, IntValue and RangeValue build values.
func StringValue(s string) Value      { return Value{Kind: ValueString, Str: s} }
func StringsValue(s ...string) Value  { return Value{Kind: ValueStrings, Strs: s} }
func IntValue(i int) Value            { return Value{Kind: ValueInt, Int: i} }
func RangeValue(minV, maxV int) Value { return Value{Kind: ValueRange, Min: minV, Max: maxV} }

// ---- JSON encoding (decoding is strict and lives in decode.go) ----

// MarshalJSON encodes the rule; conditions is always an array.
func (r Rule) MarshalJSON() ([]byte, error) {
	type wire struct {
		Version    int         `json:"version,omitempty"`
		Match      Match       `json:"match"`
		Conditions []Condition `json:"conditions"`
		Groups     []Group     `json:"groups,omitempty"`
	}
	return json.Marshal(wire{r.Version, r.Match, nonNil(r.Conditions), r.Groups})
}

// UnmarshalJSON decodes strictly (see Decode). It does not validate semantics.
func (r *Rule) UnmarshalJSON(b []byte) error {
	d, err := Decode(b)
	if err != nil {
		return err
	}
	*r = d
	return nil
}

// MarshalJSON encodes the group; conditions is always an array.
func (g Group) MarshalJSON() ([]byte, error) {
	type wire struct {
		Match      Match       `json:"match"`
		Conditions []Condition `json:"conditions"`
	}
	return json.Marshal(wire{g.Match, nonNil(g.Conditions)})
}

func nonNil(c []Condition) []Condition {
	if c == nil {
		return []Condition{}
	}
	return c
}

// MarshalJSON encodes the condition with its "type" discriminator.
func (c AttributeCondition) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		Type  ConditionType `json:"type"`
		Field Field         `json:"field"`
		Op    Operator      `json:"op"`
		Value Value         `json:"value"`
	}{ConditionAttribute, c.Field, c.Op, c.Value})
}

// MarshalJSON encodes the condition with its "type" discriminator.
func (c EventCondition) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		Type       ConditionType `json:"type"`
		Event      EventName     `json:"event"`
		Op         EventOp       `json:"op"`
		Count      *int          `json:"count,omitempty"`
		WithinDays int           `json:"within_days"`
		CategoryIn []string      `json:"category_in,omitempty"`
	}{ConditionEvent, c.Event, c.Op, c.Count, c.WithinDays, c.CategoryIn})
}

// MarshalJSON encodes the condition with its "type" discriminator.
func (c BehaviorCondition) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		Type       ConditionType     `json:"type"`
		Predicate  BehaviorPredicate `json:"predicate"`
		WithinDays int               `json:"within_days"`
	}{ConditionBehavior, c.Predicate, c.WithinDays})
}

// MarshalJSON encodes the value in the JSON shape of its kind.
func (v Value) MarshalJSON() ([]byte, error) {
	switch v.Kind {
	case ValueString:
		return json.Marshal(v.Str)
	case ValueStrings:
		if v.Strs == nil {
			return []byte("[]"), nil
		}
		return json.Marshal(v.Strs)
	case ValueInt:
		return json.Marshal(v.Int)
	case ValueRange:
		return json.Marshal(struct {
			Min int `json:"min"`
			Max int `json:"max"`
		}{v.Min, v.Max})
	default:
		return nil, fmt.Errorf("segment: cannot encode value of kind %d", v.Kind)
	}
}
