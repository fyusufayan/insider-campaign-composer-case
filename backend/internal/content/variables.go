package content

import (
	"slices"

	"github.com/fyusufayan/insider-campaign-composer-case/backend/internal/segment"
)

// VariableName is the name of a personalization variable as written in a
// template placeholder, e.g. "first_name" in {{first_name}}.
type VariableName string

// The variable allow-list (ARCHITECTURE §8.2). No other name is accepted.
const (
	VarFirstName VariableName = "first_name"
	VarLastName  VariableName = "last_name"
	VarCity      VariableName = "city"
)

// Declared maximum lengths in runes (ARCHITECTURE §8.3). The name maxima are
// the users.first_name / users.last_name CHECK constraints (§4); the city
// maximum is the longest display name in the known-city catalog.
const (
	MaxFirstNameRunes = 20
	MaxLastNameRunes  = 25
)

// Variable is one entry of the variable registry.
type Variable struct {
	Name VariableName `json:"name"`
	// MaxRunes is the declared maximum length of a value. It is the length
	// the worst-case computation assumes and the renderer enforces.
	MaxRunes int `json:"max_runes"`
	// Description is a short human/LLM-facing description (no customer data).
	Description string `json:"description"`
}

// registry is immutable after package initialization (built from constants
// and the immutable city catalog).
var registry = []Variable{
	{Name: VarFirstName, MaxRunes: MaxFirstNameRunes, Description: "recipient's first name"},
	{Name: VarLastName, MaxRunes: MaxLastNameRunes, Description: "recipient's last name"},
	{Name: VarCity, MaxRunes: segment.Cities().MaxNameRunes(), Description: "recipient's city (display name, e.g. İstanbul)"},
}

// Variables returns a copy of the registry in its fixed order
// (first_name, last_name, city).
func Variables() []Variable { return slices.Clone(registry) }

// LookupVariable finds a variable by its exact (case-sensitive) name.
func LookupVariable(name string) (Variable, bool) {
	for _, v := range registry {
		if string(v.Name) == name {
			return v, true
		}
	}
	return Variable{}, false
}

// variableNames lists the allowed names in registry order (used as the
// "allowed" list of placeholder errors).
func variableNames() []string {
	out := make([]string, len(registry))
	for i, v := range registry {
		out[i] = string(v.Name)
	}
	return out
}

// Values are one recipient's personalization values. At prepare time they
// are frozen into campaign_deliveries.vars with exactly this JSON shape; the
// delivery worker renders from them. They must never be sent to the LLM.
// An empty (or whitespace-only) value counts as missing.
type Values struct {
	FirstName string `json:"first_name"`
	LastName  string `json:"last_name"`
	City      string `json:"city"`
}

func (v Values) get(n VariableName) string {
	switch n {
	case VarFirstName:
		return v.FirstName
	case VarLastName:
		return v.LastName
	case VarCity:
		return v.City
	}
	return ""
}

// ExampleValues are fixed, fictional values used for the rendered example of
// a validation result (previews). They are not taken from customer data.
func ExampleValues() Values {
	return Values{FirstName: "Ayşe", LastName: "Yılmaz", City: "İstanbul"}
}
