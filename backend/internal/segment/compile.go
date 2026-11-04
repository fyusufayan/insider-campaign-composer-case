package segment

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// ErrUncompilable is returned when a Validated rule contains something the
// compiler has no trusted definition for. It cannot happen for rules produced
// by Normalize; it exists so that a registry/compiler mismatch fails closed
// (an error, never a panic and never a weaker query).
var ErrUncompilable = errors.New("segment: rule cannot be compiled")

// Compile turns a validated, normalized rule into one parameterized
// PostgreSQL statement selecting the matched user ids:
//
//	SELECT u.id FROM users AS u WHERE (<predicate>)
//
// Each user appears at most once (u.id is the primary key and the statement
// has no joins; events are tested in correlated subqueries). Placeholders are
// numbered $1..$n in the order they appear in the SQL text, and args holds the
// values in the same order, so the same rule always compiles to the same
// statement and arguments.
//
// Safety boundary: every byte of the SQL text comes from the Go constants in
// this file, selected by the rule's closed vocabulary (field, operator, event,
// predicate, match); every value of the rule — strings, lists, integers — is
// passed only as a bind parameter. Only a Validated is accepted (raw JSON and
// Rule must go through Parse/Normalize first); a zero Validated returns
// ErrNotValidated.
//
// Time semantics (ARCHITECTURE §6.4, §18; evaluation time = now(), UTC):
//   - event windows: occurred_at/purchased_at in [now() − within_days days,
//     now()], both bounds inclusive; future-dated events never match;
//   - *_days_ago: the number of whole elapsed days, floor((now() − t) / 1 day);
//     a future-dated t satisfies no days-ago condition;
//   - age: whole years between birth_date and the current UTC date;
//   - NULL birth_date / last_active_at never satisfy an attribute condition.
func Compile(v Validated) (sql string, args []any, err error) {
	if v.IsZero() {
		return "", nil, ErrNotValidated
	}
	c := &compiler{}
	pred, err := c.rule(*v.rule)
	if err != nil {
		return "", nil, err
	}
	return sqlSelectMatched + pred, c.args, nil
}

// ---- Trusted SQL vocabulary (the only source of SQL text) ----

const (
	sqlSelectMatched = "SELECT u.id FROM users AS u WHERE "

	// sqlTodayUTC is the current date in UTC, independent of the session
	// time zone.
	sqlTodayUTC = "(now() AT TIME ZONE 'UTC')::date"

	sqlAnd = " AND "
	sqlOr  = " OR "
)

// attrKind selects the fragment family of an attribute column.
type attrKind uint8

const (
	attrText    attrKind = iota + 1 // eq/neq/in/not_in on a text expression
	attrAge                         // gte/lte/between in whole years from a DATE column
	attrDaysAgo                     // gte/lte/between in whole elapsed days from a TIMESTAMPTZ column
)

type attrColumn struct {
	kind attrKind
	// expr is a constant SQL expression over the users alias "u".
	expr string
}

// attributeColumns maps every registry field to its column. A field missing
// here cannot be compiled (ErrUncompilable); a test asserts the two sets agree.
var attributeColumns = map[Field]attrColumn{
	FieldCity:              {attrText, "u.city_normalized"},
	FieldCountry:           {attrText, "u.country::text"},
	FieldGender:            {attrText, "u.gender"},
	FieldLanguage:          {attrText, "u.language"},
	FieldAge:               {attrAge, "u.birth_date"},
	FieldSignupDaysAgo:     {attrDaysAgo, "u.signup_at"},
	FieldLastActiveDaysAgo: {attrDaysAgo, "u.last_active_at"},
}

type eventTable struct {
	// from is the constant table reference with alias "e"; at is its event time column.
	from, at string
}

var eventTables = map[EventName]eventTable{
	EventCartAdd:  {"cart_events AS e", "e.occurred_at"},
	EventPurchase: {"purchases AS e", "e.purchased_at"},
}

// ---- Compiler ----

type compiler struct {
	args []any
}

// param appends a bind value and returns its placeholder.
func (c *compiler) param(v any) string {
	c.args = append(c.args, v)
	return "$" + strconv.Itoa(len(c.args))
}

func joiner(m Match) (string, error) {
	switch m {
	case MatchAll:
		return sqlAnd, nil
	case MatchAny:
		return sqlOr, nil
	default:
		return "", fmt.Errorf("%w: match %q", ErrUncompilable, m)
	}
}

// rule composes the top level: the rule's match combines each top-level
// condition and each group; a group is one operand whose conditions are
// combined with the group's own match. Every operand is parenthesized.
func (c *compiler) rule(r Rule) (string, error) {
	j, err := joiner(r.Match)
	if err != nil {
		return "", err
	}
	operands := make([]string, 0, len(r.Conditions)+len(r.Groups))
	for _, cond := range r.Conditions {
		f, err := c.condition(cond)
		if err != nil {
			return "", err
		}
		operands = append(operands, f)
	}
	for _, g := range r.Groups {
		gj, err := joiner(g.Match)
		if err != nil {
			return "", err
		}
		if len(g.Conditions) == 0 {
			return "", fmt.Errorf("%w: empty group", ErrUncompilable)
		}
		parts := make([]string, 0, len(g.Conditions))
		for _, cond := range g.Conditions {
			f, err := c.condition(cond)
			if err != nil {
				return "", err
			}
			parts = append(parts, f)
		}
		operands = append(operands, "("+strings.Join(parts, gj)+")")
	}
	if len(operands) == 0 {
		// Never return an unfiltered query.
		return "", fmt.Errorf("%w: no conditions", ErrUncompilable)
	}
	return "(" + strings.Join(operands, j) + ")", nil
}

func (c *compiler) condition(cond Condition) (string, error) {
	var (
		f   string
		err error
	)
	switch cond := cond.(type) {
	case AttributeCondition:
		f, err = c.attribute(cond)
	case EventCondition:
		f, err = c.event(cond)
	case BehaviorCondition:
		f, err = c.behavior(cond)
	default:
		err = fmt.Errorf("%w: condition %T", ErrUncompilable, cond)
	}
	if err != nil {
		return "", err
	}
	return "(" + f + ")", nil
}

func (c *compiler) attribute(a AttributeCondition) (string, error) {
	col, ok := attributeColumns[a.Field]
	if !ok {
		return "", fmt.Errorf("%w: field %q", ErrUncompilable, a.Field)
	}
	if want := valueKindFor(a.Op); want == ValueInvalid || a.Value.Kind != want {
		return "", fmt.Errorf("%w: field %q operator %q", ErrUncompilable, a.Field, a.Op)
	}
	if a.Value.Kind == ValueStrings && len(a.Value.Strs) == 0 {
		// "<> ALL('{}')" is true for everyone: fail closed instead.
		return "", fmt.Errorf("%w: field %q empty list", ErrUncompilable, a.Field)
	}
	switch col.kind {
	case attrText:
		switch a.Op {
		case OpEq:
			return col.expr + " = " + c.param(a.Value.Str) + "::text", nil
		case OpNeq:
			return col.expr + " <> " + c.param(a.Value.Str) + "::text", nil
		case OpIn:
			return col.expr + " = ANY(" + c.param(cloneStrings(a.Value.Strs)) + "::text[])", nil
		case OpNotIn:
			return col.expr + " <> ALL(" + c.param(cloneStrings(a.Value.Strs)) + "::text[])", nil
		}
	case attrAge:
		switch a.Op {
		case OpGte:
			return c.ageAtLeast(col.expr, a.Value.Int), nil
		case OpLte:
			return c.ageAtMost(col.expr, a.Value.Int), nil
		case OpBetween:
			lo := c.ageAtLeast(col.expr, a.Value.Min)
			return lo + sqlAnd + c.ageAtMost(col.expr, a.Value.Max), nil
		}
	case attrDaysAgo:
		switch a.Op {
		case OpGte:
			return c.daysAgoAtLeast(col.expr, a.Value.Int), nil
		case OpLte:
			return c.daysAgoAtMost(col.expr, a.Value.Int), nil
		case OpBetween:
			lo := c.daysAgoAtLeast(col.expr, a.Value.Min)
			return lo + sqlAnd + c.daysAgoAtMost(col.expr, a.Value.Max), nil
		}
	}
	return "", fmt.Errorf("%w: field %q operator %q", ErrUncompilable, a.Field, a.Op)
}

// age ≥ n  ⇔  birth_date ≤ today − n years.
func (c *compiler) ageAtLeast(col string, n int) string {
	return col + " <= (" + sqlTodayUTC + " - make_interval(years => " + c.param(n) + "::integer))::date"
}

// age ≤ n  ⇔  age < n+1  ⇔  birth_date > today − (n+1) years.
func (c *compiler) ageAtMost(col string, n int) string {
	return col + " > (" + sqlTodayUTC + " - make_interval(years => " + c.param(n) + "::integer + 1))::date"
}

// days_ago ≥ n  ⇔  t ≤ now() − n days.
func (c *compiler) daysAgoAtLeast(col string, n int) string {
	return col + " <= now() - make_interval(days => " + c.param(n) + "::integer)"
}

// days_ago ≤ n  ⇔  days_ago < n+1  ⇔  t > now() − (n+1) days, and t ≤ now()
// (a future timestamp has no days-ago value; the ≥ form already implies it).
func (c *compiler) daysAgoAtMost(col string, n int) string {
	return col + " > now() - make_interval(days => " + c.param(n) + "::integer + 1) AND " + col + " <= now()"
}

// event compiles to a correlated subquery over the user's events, so users
// with no matching events are kept (count 0) and counts are never multiplied
// by joins. Placeholders: within_days, then category_in (if present), then count.
func (c *compiler) event(e EventCondition) (string, error) {
	t, ok := eventTables[e.Event]
	if !ok {
		return "", fmt.Errorf("%w: event %q", ErrUncompilable, e.Event)
	}
	if e.WithinDays < MinWithinDays || e.WithinDays > MaxWithinDays {
		return "", fmt.Errorf("%w: within_days out of range", ErrUncompilable)
	}
	if e.CategoryIn != nil && len(e.CategoryIn) == 0 {
		return "", fmt.Errorf("%w: empty category_in", ErrUncompilable)
	}
	hasCount := e.Op == EventCountGte || e.Op == EventCountLte
	if hasCount != (e.Count != nil) {
		return "", fmt.Errorf("%w: event operator %q count", ErrUncompilable, e.Op)
	}

	where := "e.user_id = u.id AND " + t.at + " >= now() - make_interval(days => " + c.param(e.WithinDays) + "::integer)" +
		" AND " + t.at + " <= now()"
	if e.CategoryIn != nil {
		where += " AND e.category = ANY(" + c.param(cloneStrings(e.CategoryIn)) + "::text[])"
	}
	sub := "SELECT 1 FROM " + t.from + " WHERE " + where
	countSub := "(SELECT count(*) FROM " + t.from + " WHERE " + where + ")"

	switch e.Op {
	case EventExists:
		return "EXISTS (" + sub + ")", nil
	case EventNotExists:
		return "NOT EXISTS (" + sub + ")", nil
	case EventCountGte:
		return countSub + " >= " + c.param(*e.Count) + "::bigint", nil
	case EventCountLte:
		return countSub + " <= " + c.param(*e.Count) + "::bigint", nil
	default:
		return "", fmt.Errorf("%w: event operator %q", ErrUncompilable, e.Op)
	}
}

// behavior compiles named predicates.
//
// abandoned_cart(N) (ARCHITECTURE §6.1): the user has a cart event with
// occurred_at in [now() − N days, now()] for which no purchase of the user has
// purchased_at in [that cart event's occurred_at, now()]. Any purchase
// recovers the cart, whatever its category (the definition has no category
// matching); a purchase at the same instant recovers it ("at or after"); a
// future-dated purchase does not.
func (c *compiler) behavior(b BehaviorCondition) (string, error) {
	if b.WithinDays < MinWithinDays || b.WithinDays > MaxWithinDays {
		return "", fmt.Errorf("%w: within_days out of range", ErrUncompilable)
	}
	switch b.Predicate {
	case PredicateAbandonedCart:
		return "EXISTS (SELECT 1 FROM cart_events AS c WHERE c.user_id = u.id" +
			" AND c.occurred_at >= now() - make_interval(days => " + c.param(b.WithinDays) + "::integer)" +
			" AND c.occurred_at <= now()" +
			" AND NOT EXISTS (SELECT 1 FROM purchases AS p WHERE p.user_id = c.user_id" +
			" AND p.purchased_at >= c.occurred_at AND p.purchased_at <= now()))", nil
	default:
		return "", fmt.Errorf("%w: predicate %q", ErrUncompilable, b.Predicate)
	}
}

func cloneStrings(s []string) []string { return append([]string(nil), s...) }
