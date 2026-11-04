package segment

import (
	"regexp"
	"testing"
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"
)

func TestFoldKey(t *testing.T) {
	tests := []struct{ in, want string }{
		{"İstanbul", "istanbul"},
		{"ISTANBUL", "istanbul"},
		{"Istanbul", "istanbul"},
		{"istanbul", "istanbul"},
		{"İstanbul", "istanbul"}, // decomposed İ
		{"İSTANBUL", "istanbul"},
		{"  İstanbul \t", "istanbul"},
		{"ıstanbul", "istanbul"},
		{"Şanlıurfa", "sanliurfa"},
		{"SANLIURFA", "sanliurfa"},
		{"şanliurfa", "sanliurfa"}, // decomposed ş
		{"Muğla", "mugla"},
		{"GÜMÜŞHANE", "gumushane"},
		{"Kütahya", "kutahya"},
		{"Bingöl", "bingol"},
		{"Çanakkale", "canakkale"},
		{"IĞDIR", "igdir"},
		{"new   york", "new york"},
		{"  a \n b  ", "a b"},
		{"", ""},
		{"   ", ""},
	}
	for _, tt := range tests {
		if got := FoldKey(tt.in); got != tt.want {
			t.Errorf("FoldKey(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestIstanbulSpellingsResolveToOneCity(t *testing.T) {
	for _, s := range []string{"İstanbul", "ISTANBUL", "Istanbul", "istanbul", "İstanbul", " istanbul  "} {
		c, ok := Cities().Lookup(s)
		if !ok {
			t.Fatalf("Lookup(%q) not found", s)
		}
		if c.Key != "istanbul" || c.Name != "İstanbul" {
			t.Errorf("Lookup(%q) = %+v, want {istanbul İstanbul}", s, c)
		}
	}
}

func TestLookupUnknown(t *testing.T) {
	for _, s := range []string{"", "Paris", "istanbul'; DROP TABLE users; --", "İst"} {
		if c, ok := Cities().Lookup(s); ok {
			t.Errorf("Lookup(%q) = %+v, want not found", s, c)
		}
	}
}

var keyPattern = regexp.MustCompile(`^[a-z]+$`)

func TestCatalogInvariants(t *testing.T) {
	cat := Cities()
	if cat.Len() != 81 {
		t.Fatalf("catalog has %d cities, want 81", cat.Len())
	}
	maxRunes := 0
	for i, c := range cat.All() {
		if !keyPattern.MatchString(c.Key) {
			t.Errorf("city %d key %q is not plain ASCII lowercase", i, c.Key)
		}
		if !norm.NFC.IsNormalString(c.Name) {
			t.Errorf("city %q display name is not NFC", c.Name)
		}
		if FoldKey(c.Name) != c.Key {
			t.Errorf("FoldKey(%q) = %q, want key %q", c.Name, FoldKey(c.Name), c.Key)
		}
		got, ok := cat.Lookup(c.Name)
		if !ok || got != c {
			t.Errorf("Lookup(%q) = %+v, %v", c.Name, got, ok)
		}
		if cat.At(i) != c {
			t.Errorf("At(%d) = %+v, want %+v", i, cat.At(i), c)
		}
		maxRunes = max(maxRunes, utf8.RuneCountInString(c.Name))
	}
	if cat.MaxNameRunes() != maxRunes || maxRunes != len("Afyonkarahisar") {
		t.Errorf("MaxNameRunes = %d, computed %d", cat.MaxNameRunes(), maxRunes)
	}
}

func TestAllReturnsCopy(t *testing.T) {
	all := Cities().All()
	all[0] = City{Key: "x", Name: "x"}
	if Cities().At(0).Key != "adana" {
		t.Error("All() exposed the catalog's backing array")
	}
}

func TestBuildCatalogRejectsCollidingKeys(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("mustBuildCatalog accepted two names with the same key")
		}
	}()
	mustBuildCatalog([]string{"İzmir", "Izmir"})
}
