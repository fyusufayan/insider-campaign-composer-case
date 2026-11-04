package ai

import (
	"encoding/json"
	"slices"

	"github.com/fyusufayan/insider-campaign-composer-case/backend/internal/segment"
)

// InterpretationSchema returns the structured-output JSON schema of the
// interpretation envelope (generation-time shape, §6.3 step 1). It is built
// from the segment registry and vocabulary, so it cannot drift from the
// validator. It returns a fresh value on every call.
//
// Only keywords supported by structured outputs are used: type, enum,
// properties, required, additionalProperties:false, items, anyOf, $ref/$defs,
// description. Lengths, list sizes and numeric ranges are deliberately not
// expressed (unsupported by the API); ParseInterpretation enforces them, and
// the prompt states them.
func InterpretationSchema() map[string]any {
	return obj(map[string]any{
		keySegment: map[string]any{"$ref": "#/$defs/rule"},
		keyInterpretation: map[string]any{
			"type":        "string",
			"description": "One plain-language sentence describing the selected audience.",
		},
		keyAssumptions: stringArray("Interpretation choices you made where the brief was ambiguous."),
		keyUnsupported: stringArray("Requested criteria that cannot be expressed with the supported fields; never approximated in the rule."),
		keySuggestedChannels: map[string]any{
			"type":  "array",
			"items": enumOf(channelValues),
		},
		keyContentBrief: closed(map[string]any{
			keyGoal:   map[string]any{"type": "string", "description": "Short snake_case campaign goal, e.g. cart_recovery."},
			keyTone:   map[string]any{"type": "string", "description": "Tone of voice, e.g. friendly."},
			keyLength: enumOf(lengthValues),
			keyOffer: map[string]any{"anyOf": []any{
				closed(map[string]any{keyKind: enumOf([]string{string(OfferNone), string(OfferFreeShipping)})}),
				closed(map[string]any{
					keyKind:  enumOf([]string{string(OfferPercentDiscount)}),
					keyValue: map[string]any{"type": "integer"},
				}),
			}},
		}),
	}, envelopeKeys, map[string]any{
		"rule":      ruleSchema(),
		"group":     groupSchema(),
		"condition": conditionSchema(),
	})
}

// InterpretationSchemaJSON is the canonical encoding of InterpretationSchema
// (encoding/json sorts map keys, so it is byte-for-byte deterministic).
func InterpretationSchemaJSON() []byte {
	b, err := json.MarshalIndent(InterpretationSchema(), "", "  ")
	if err != nil {
		panic("ai: interpretation schema does not encode: " + err.Error()) // static data
	}
	return b
}

func ruleSchema() map[string]any {
	return objOptional(map[string]any{
		"match":      enumOf(segment.MatchValues()),
		"conditions": conditionsArray(),
		"groups": map[string]any{
			"type":  "array",
			"items": map[string]any{"$ref": "#/$defs/group"},
		},
	}, []string{"match", "conditions"})
}

func groupSchema() map[string]any {
	return obj(map[string]any{
		"match":      enumOf(segment.MatchValues()),
		"conditions": conditionsArray(),
	}, []string{"match", "conditions"}, nil)
}

func conditionsArray() map[string]any {
	return map[string]any{"type": "array", "items": map[string]any{"$ref": "#/$defs/condition"}}
}

func conditionSchema() map[string]any {
	fieldNames := make([]string, 0, len(segment.Fields()))
	for _, f := range segment.Fields() {
		fieldNames = append(fieldNames, string(f.Field))
	}
	attribute := obj(map[string]any{
		"type":  enumOf([]string{string(segment.ConditionAttribute)}),
		"field": enumOf(fieldNames),
		"op":    enumOf(segment.Operators()),
		"value": map[string]any{"anyOf": []any{
			map[string]any{"type": "string"},
			map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
			map[string]any{"type": "integer"},
			obj(map[string]any{
				"min": map[string]any{"type": "integer"},
				"max": map[string]any{"type": "integer"},
			}, []string{"min", "max"}, nil),
		}},
	}, []string{"type", "field", "op", "value"}, nil)
	event := objOptional(map[string]any{
		"type":        enumOf([]string{string(segment.ConditionEvent)}),
		"event":       enumOf(segment.EventNames()),
		"op":          enumOf(segment.EventOps()),
		"count":       map[string]any{"type": "integer"},
		"within_days": map[string]any{"type": "integer"},
		"category_in": map[string]any{"type": "array", "items": enumOf(segment.Categories())},
	}, []string{"type", "event", "op", "within_days"})
	behavior := obj(map[string]any{
		"type":        enumOf([]string{string(segment.ConditionBehavior)}),
		"predicate":   enumOf(segment.BehaviorPredicates()),
		"within_days": map[string]any{"type": "integer"},
	}, []string{"type", "predicate", "within_days"}, nil)
	return map[string]any{"anyOf": []any{attribute, event, behavior}}
}

// obj is a closed object schema. With required == nil every property is
// required; defs, when non-nil, becomes $defs.
func obj(props map[string]any, required []string, defs map[string]any) map[string]any {
	if required == nil {
		required = sortedKeys(props)
	}
	m := map[string]any{
		"type":                 "object",
		"properties":           props,
		"required":             stringsToAny(required),
		"additionalProperties": false,
	}
	if defs != nil {
		m["$defs"] = defs
	}
	return m
}

// closed is a closed object schema with every property required.
func closed(props map[string]any) map[string]any { return obj(props, nil, nil) }

// objOptional is a closed object schema with only the listed properties required.
func objOptional(props map[string]any, required []string) map[string]any {
	return obj(props, required, nil)
}

func enumOf(values []string) map[string]any {
	return map[string]any{"type": "string", "enum": stringsToAny(values)}
}

func stringArray(desc string) map[string]any {
	return map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": desc}
}

func stringsToAny(s []string) []any {
	out := make([]any, len(s))
	for i, v := range s {
		out[i] = v
	}
	return out
}

func sortedKeys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}
