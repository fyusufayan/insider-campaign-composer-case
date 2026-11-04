package segment

import (
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"
)

// City is an entry of the known-city catalog.
type City struct {
	// Key is the canonical folded key stored in users.city_normalized and used
	// in normalized segment rules, e.g. "istanbul".
	Key string
	// Name is the Turkish display name, e.g. "İstanbul".
	Name string
}

// cityNames is the catalog: the 81 provinces of Türkiye in licence-plate order.
// The order is part of the contract (deterministic seed, prompt catalog), so
// append-only changes are preferred.
var cityNames = [...]string{
	"Adana", "Adıyaman", "Afyonkarahisar", "Ağrı", "Amasya", "Ankara", "Antalya", "Artvin",
	"Aydın", "Balıkesir", "Bilecik", "Bingöl", "Bitlis", "Bolu", "Burdur", "Bursa",
	"Çanakkale", "Çankırı", "Çorum", "Denizli", "Diyarbakır", "Edirne", "Elazığ", "Erzincan",
	"Erzurum", "Eskişehir", "Gaziantep", "Giresun", "Gümüşhane", "Hakkari", "Hatay", "Isparta",
	"Mersin", "İstanbul", "İzmir", "Kars", "Kastamonu", "Kayseri", "Kırklareli", "Kırşehir",
	"Kocaeli", "Konya", "Kütahya", "Malatya", "Manisa", "Kahramanmaraş", "Mardin", "Muğla",
	"Muş", "Nevşehir", "Niğde", "Ordu", "Rize", "Sakarya", "Samsun", "Siirt",
	"Sinop", "Sivas", "Tekirdağ", "Tokat", "Trabzon", "Tunceli", "Şanlıurfa", "Uşak",
	"Van", "Yozgat", "Zonguldak", "Aksaray", "Bayburt", "Karaman", "Kırıkkale", "Batman",
	"Şırnak", "Bartın", "Ardahan", "Iğdır", "Yalova", "Karabük", "Kilis", "Osmaniye",
	"Düzce",
}

// CityCatalog is the immutable, ordered known-city catalog with lookup by
// folded key. It is built once from cityNames and shared by the seed
// generator, the segment validator, the prompt catalog and rule descriptions.
type CityCatalog struct {
	cities   []City
	byKey    map[string]int
	maxRunes int
}

var defaultCatalog = mustBuildCatalog(cityNames[:])

// Cities returns the default catalog.
func Cities() *CityCatalog { return defaultCatalog }

func mustBuildCatalog(names []string) *CityCatalog {
	c := &CityCatalog{byKey: make(map[string]int, len(names))}
	for _, n := range names {
		name := norm.NFC.String(n)
		key := FoldKey(name)
		if key == "" {
			panic("segment: empty city key for " + n)
		}
		if _, dup := c.byKey[key]; dup {
			panic("segment: duplicate city key " + key)
		}
		c.byKey[key] = len(c.cities)
		c.cities = append(c.cities, City{Key: key, Name: name})
		if r := utf8.RuneCountInString(name); r > c.maxRunes {
			c.maxRunes = r
		}
	}
	return c
}

// All returns a copy of the catalog in its fixed order.
func (c *CityCatalog) All() []City {
	out := make([]City, len(c.cities))
	copy(out, c.cities)
	return out
}

// Len reports the number of known cities.
func (c *CityCatalog) Len() int { return len(c.cities) }

// At returns the i-th city in catalog order.
func (c *CityCatalog) At(i int) City { return c.cities[i] }

// Lookup resolves any spelling of a known city (case, dotted/dotless I,
// Turkish letters or their ASCII forms, decomposed Unicode, extra whitespace).
func (c *CityCatalog) Lookup(s string) (City, bool) {
	i, ok := c.byKey[FoldKey(s)]
	if !ok {
		return City{}, false
	}
	return c.cities[i], true
}

// MaxNameRunes is the rune length of the longest display name; content
// validation uses it as the declared maximum of {{city}}.
func (c *CityCatalog) MaxNameRunes() int { return c.maxRunes }

// turkishFold maps the Turkish letters without an ASCII base letter that
// survive lowercasing to their ASCII forms.
var turkishFold = strings.NewReplacer(
	"ı", "i",
	"ş", "s",
	"ğ", "g",
	"ü", "u",
	"ö", "o",
	"ç", "c",
)

// FoldKey is the Turkish-aware folding used for city keys (Q4 decision):
//  1. Unicode NFC (so a decomposed "İ" becomes "İ");
//  2. Turkish lowercase (unicode.TurkishCase: "I"→"ı", "İ"→"i");
//  3. fold ı→i, ş→s, ğ→g, ü→u, ö→o, ç→c;
//  4. trim and collapse whitespace to single spaces.
//
// "İstanbul", "ISTANBUL", "Istanbul", "istanbul" and "İstanbul" all fold
// to "istanbul".
func FoldKey(s string) string {
	s = norm.NFC.String(s)
	s = strings.ToLowerSpecial(unicode.TurkishCase, s)
	s = turkishFold.Replace(s)
	return strings.Join(strings.Fields(s), " ")
}
