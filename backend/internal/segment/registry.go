package segment

import (
	"strings"
)

// Limits of DSL v1 (ARCHITECTURE §6.1).
const (
	// MaxRuleBytes bounds the encoded rule accepted by Decode.
	MaxRuleBytes = 64 << 10
	// MaxConditionsPerGroup applies to the top-level conditions and to each group.
	MaxConditionsPerGroup = 10
	MaxGroups             = 5
	MaxListItems          = 50
	// MaxValueRunes bounds every string value before catalog lookup.
	MaxValueRunes = 100
	MinWithinDays = 1
	MaxWithinDays = 365
	// MinEventCount is 0: a user with no matching events has count 0, so
	// count_lte 0 ≡ not_exists and count_gte 0 matches everyone (see EventCondition).
	MinEventCount = 0
	MaxEventCount = 100

	// maxJSONDepth is the deepest nesting a valid rule needs (root → groups →
	// group → conditions → condition → value) plus headroom.
	maxJSONDepth = 8
)

// ValueType is the scalar type of a field's values.
type ValueType string

// Value types.
const (
	TypeString  ValueType = "string"
	TypeInteger ValueType = "integer"
)

// FieldSpec is one allow-listed attribute field. It is the single source for
// validation, normalization, the prompt field catalog and the JSON schema; the
// SQL compiler maps Field to column expressions with Go constants.
type FieldSpec struct {
	Field     Field
	Type      ValueType
	Operators []Operator
	// Min and Max bound integer values (inclusive).
	Min, Max int
	// Values is the closed value set of enumerated string fields (canonical
	// form); nil for catalog-backed fields (city, country).
	Values      []string
	Description string

	// canonical resolves a string value to its canonical form.
	canonical func(string) (string, bool)
	// invalidCode / invalidMsg describe an unresolvable string value.
	invalidCode string
	invalidMsg  string
}

var (
	stringEqOps = []Operator{OpEq, OpNeq, OpIn, OpNotIn}
	setOps      = []Operator{OpEq, OpIn}
	rangeOps    = []Operator{OpGte, OpLte, OpBetween}
	boundOps    = []Operator{OpGte, OpLte}

	genders   = []string{"female", "male", "other", "unknown"}
	languages = []string{"tr", "en"}
	// categories is the canonical product-category catalog (sorted). The seed
	// generator takes its category names from the constants below, so a
	// category outside this list cannot occur in cart_events / purchases.
	categories = []string{CategoryBeauty, CategoryBooks, CategoryElectronics, CategoryFashion,
		CategoryGrocery, CategoryHome, CategorySports, CategoryToys}
)

// Product categories (canonical names) of cart_events / purchases.
const (
	CategoryBeauty      = "beauty"
	CategoryBooks       = "books"
	CategoryElectronics = "electronics"
	CategoryFashion     = "fashion"
	CategoryGrocery     = "grocery"
	CategoryHome        = "home"
	CategorySports      = "sports"
	CategoryToys        = "toys"
)

// registry is the ordered allow-list of attribute fields.
var registry = []FieldSpec{
	{
		Field: FieldCity, Type: TypeString, Operators: stringEqOps,
		Description: "City of residence; one of the 81 provinces of Türkiye. Any spelling (İstanbul, ISTANBUL, Istanbul) is accepted and matched with Turkish-aware folding.",
		canonical: func(s string) (string, bool) {
			c, ok := Cities().Lookup(s)
			return c.Key, ok
		},
		invalidCode: CodeUnknownCity, invalidMsg: "unknown city; must be one of the 81 provinces of Türkiye",
	},
	{
		Field: FieldCountry, Type: TypeString, Operators: setOps,
		Description: "Country as an ISO 3166-1 alpha-2 code, e.g. TR.",
		canonical:   canonicalCountry,
		invalidCode: CodeUnknownCountry, invalidMsg: "must be an ISO 3166-1 alpha-2 country code such as TR",
	},
	{
		Field: FieldGender, Type: TypeString, Operators: setOps, Values: genders,
		Description: "Gender as recorded at signup.",
		canonical:   enumCanonical(genders),
		invalidCode: CodeNotAllowed, invalidMsg: "unknown gender",
	},
	{
		Field: FieldAge, Type: TypeInteger, Operators: rangeOps, Min: 13, Max: 120,
		Description: "Age in whole years (users without a birth date never match).",
	},
	{
		Field: FieldSignupDaysAgo, Type: TypeInteger, Operators: rangeOps, Min: 0, Max: 3650,
		Description: "Days since signup.",
	},
	{
		Field: FieldLastActiveDaysAgo, Type: TypeInteger, Operators: boundOps, Min: 0, Max: 3650,
		Description: "Days since last activity (users never active never match).",
	},
	{
		Field: FieldLanguage, Type: TypeString, Operators: setOps, Values: languages,
		Description: "Preferred language.",
		canonical:   enumCanonical(languages),
		invalidCode: CodeNotAllowed, invalidMsg: "unsupported language",
	},
}

var registryByField = func() map[Field]int {
	m := make(map[Field]int, len(registry))
	for i, s := range registry {
		m[s.Field] = i
	}
	return m
}()

// Fields returns copies of the registry entries in their fixed order.
func Fields() []FieldSpec {
	out := make([]FieldSpec, len(registry))
	for i, s := range registry {
		out[i] = s.clone()
	}
	return out
}

// LookupField returns a copy of the registry entry for f.
func LookupField(f Field) (FieldSpec, bool) {
	i, ok := registryByField[f]
	if !ok {
		return FieldSpec{}, false
	}
	return registry[i].clone(), true
}

func (s FieldSpec) clone() FieldSpec {
	s.Operators = append([]Operator(nil), s.Operators...)
	if s.Values != nil {
		s.Values = append([]string(nil), s.Values...)
	}
	return s
}

// allows reports whether op is permitted for the field.
func (s FieldSpec) allows(op Operator) bool {
	for _, o := range s.Operators {
		if o == op {
			return true
		}
	}
	return false
}

// Categories returns the known event categories (canonical, sorted).
func Categories() []string { return append([]string(nil), categories...) }

// Enumerations of the structural DSL vocabulary, in a fixed order.
var (
	matchValues     = []string{string(MatchAll), string(MatchAny)}
	conditionTypes  = []string{string(ConditionAttribute), string(ConditionEvent), string(ConditionBehavior)}
	operatorValues  = []string{string(OpEq), string(OpNeq), string(OpIn), string(OpNotIn), string(OpGte), string(OpLte), string(OpBetween)}
	eventNameValues = []string{string(EventCartAdd), string(EventPurchase)}
	eventOpValues   = []string{string(EventExists), string(EventNotExists), string(EventCountGte), string(EventCountLte)}
	predicateValues = []string{string(PredicateAbandonedCart)}
	fieldNameValues = func() []string {
		out := make([]string, len(registry))
		for i, s := range registry {
			out[i] = string(s.Field)
		}
		return out
	}()
)

// Operators, EventNames, EventOps, BehaviorPredicates, ConditionTypes and
// MatchValues expose the vocabulary for the prompt catalog and JSON schema.
func Operators() []string          { return append([]string(nil), operatorValues...) }
func EventNames() []string         { return append([]string(nil), eventNameValues...) }
func EventOps() []string           { return append([]string(nil), eventOpValues...) }
func BehaviorPredicates() []string { return append([]string(nil), predicateValues...) }
func ConditionTypes() []string     { return append([]string(nil), conditionTypes...) }
func MatchValues() []string        { return append([]string(nil), matchValues...) }

func contains(set []string, s string) bool {
	for _, v := range set {
		if v == s {
			return true
		}
	}
	return false
}

// enumCanonical accepts any case/whitespace variant of a closed value set.
func enumCanonical(values []string) func(string) (string, bool) {
	return func(s string) (string, bool) {
		k := FoldKey(s)
		return k, contains(values, k)
	}
}

func canonicalCategory(s string) (string, bool) {
	k := FoldKey(s)
	return k, contains(categories, k)
}

// iso3166Alpha2 lists the 249 officially assigned ISO 3166-1 alpha-2 codes.
const iso3166Alpha2 = "" +
	"AD AE AF AG AI AL AM AO AQ AR AS AT AU AW AX AZ " +
	"BA BB BD BE BF BG BH BI BJ BL BM BN BO BQ BR BS BT BV BW BY BZ " +
	"CA CC CD CF CG CH CI CK CL CM CN CO CR CU CV CW CX CY CZ " +
	"DE DJ DK DM DO DZ " +
	"EC EE EG EH ER ES ET " +
	"FI FJ FK FM FO FR " +
	"GA GB GD GE GF GG GH GI GL GM GN GP GQ GR GS GT GU GW GY " +
	"HK HM HN HR HT HU " +
	"ID IE IL IM IN IO IQ IR IS IT " +
	"JE JM JO JP " +
	"KE KG KH KI KM KN KP KR KW KY KZ " +
	"LA LB LC LI LK LR LS LT LU LV LY " +
	"MA MC MD ME MF MG MH MK ML MM MN MO MP MQ MR MS MT MU MV MW MX MY MZ " +
	"NA NC NE NF NG NI NL NO NP NR NU NZ " +
	"OM " +
	"PA PE PF PG PH PK PL PM PN PR PS PT PW PY " +
	"QA " +
	"RE RO RS RU RW " +
	"SA SB SC SD SE SG SH SI SJ SK SL SM SN SO SR SS ST SV SX SY SZ " +
	"TC TD TF TG TH TJ TK TL TM TN TO TR TT TV TW TZ " +
	"UA UG UM US UY UZ " +
	"VA VC VE VG VI VN VU " +
	"WF WS " +
	"YE YT " +
	"ZA ZM ZW"

var countryCodes = func() map[string]struct{} {
	m := make(map[string]struct{}, 249)
	for _, c := range strings.Fields(iso3166Alpha2) {
		m[c] = struct{}{}
	}
	return m
}()

// canonicalCountry accepts a code in any case; the canonical form is upper case.
func canonicalCountry(s string) (string, bool) {
	k := strings.ToUpper(strings.TrimSpace(s))
	if len(k) != 2 || k[0] < 'A' || k[0] > 'Z' || k[1] < 'A' || k[1] > 'Z' {
		return "", false
	}
	_, ok := countryCodes[k]
	return k, ok
}

// Consent and contact are backend-enforced (ADR-004) and not expressible in
// the DSL. Names that look like either are rejected with
// consent_is_backend_enforced instead of a generic unknown-field error, so the
// caller (or the model's repair turn) learns why.
var (
	consentMarkers = []string{"consent", "optin", "optout", "opted", "subscri", "permission", "kvkk", "gdpr", "iys"}
	contactMarkers = []string{"email", "mail", "phone", "msisdn", "gsm", "mobile", "sms", "contact", "address", "reachab"}
)

// looksLikeConsent reports whether a name refers to consent / opt-in state.
func looksLikeConsent(name string) bool { return hasMarker(name, consentMarkers) }

// looksLikeConsentOrContact additionally covers contact data (attribute
// fields and condition types only; event names such as "email_open" are
// unknown events, not consent rules).
func looksLikeConsentOrContact(name string) bool {
	return hasMarker(name, consentMarkers) || hasMarker(name, contactMarkers)
}

func hasMarker(name string, markers []string) bool {
	k := strings.NewReplacer("_", "", "-", "", " ", "", ".", "").Replace(FoldKey(name))
	for _, m := range markers {
		if strings.Contains(k, m) {
			return true
		}
	}
	return false
}
