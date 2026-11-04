package main

import (
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"

	"github.com/fyusufayan/insider-campaign-composer-case/backend/internal/segment"
)

var defaultRef = time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)

func defaultParams() Params {
	return Params{Users: 5000, RandomSeed: 42, ReferenceTime: defaultRef}
}

func mustGenerate(t *testing.T, p Params) *Dataset {
	t.Helper()
	ds, err := Generate(p)
	if err != nil {
		t.Fatalf("Generate(%+v): %v", p, err)
	}
	return ds
}

func TestGenerateIsDeterministic(t *testing.T) {
	a := mustGenerate(t, defaultParams())
	b := mustGenerate(t, defaultParams())
	if !reflect.DeepEqual(a, b) {
		t.Fatal("two runs with the same params produced different datasets")
	}
	if a.Hash() != b.Hash() {
		t.Fatalf("hash differs: %s vs %s", a.Hash(), b.Hash())
	}
	if len(a.Hash()) != 64 {
		t.Errorf("hash %q is not hex SHA-256", a.Hash())
	}
}

func TestGenerateInputsChangeHash(t *testing.T) {
	base := mustGenerate(t, defaultParams()).Hash()
	variants := map[string]Params{
		"random seed":    {Users: 5000, RandomSeed: 43, ReferenceTime: defaultRef},
		"reference time": {Users: 5000, RandomSeed: 42, ReferenceTime: defaultRef.Add(24 * time.Hour)},
		"reference µs":   {Users: 5000, RandomSeed: 42, ReferenceTime: defaultRef.Add(time.Microsecond)},
		"user count":     {Users: 4999, RandomSeed: 42, ReferenceTime: defaultRef},
	}
	for name, p := range variants {
		if h := mustGenerate(t, p).Hash(); h == base {
			t.Errorf("%s: hash unchanged (%s)", name, h)
		}
	}
}

// A reference time with sub-microsecond precision is truncated, never leaked into data.
func TestGenerateTruncatesReferenceTime(t *testing.T) {
	p := defaultParams()
	p.Users = 50
	q := p
	q.ReferenceTime = p.ReferenceTime.Add(999 * time.Nanosecond)
	if mustGenerate(t, p).Hash() != mustGenerate(t, q).Hash() {
		t.Error("sub-microsecond reference offset changed the dataset")
	}
}

// Fixed golden hash: catches accidental changes to draw order, lists or format.
// Update deliberately (and say so in the change) if the dataset is meant to change.
func TestGenerateGoldenHash(t *testing.T) {
	const golden = "c553fe3638a6664a1c0de01035237a64d0dc4de72cde58c771cf9ed64fe2da25"
	if got := mustGenerate(t, defaultParams()).Hash(); got != golden {
		t.Errorf("default dataset hash = %s, want %s", got, golden)
	}
}

func TestGenerateRejectsInvalidParams(t *testing.T) {
	for _, p := range []Params{
		{Users: 0, ReferenceTime: defaultRef},
		{Users: -1, ReferenceTime: defaultRef},
		{Users: 10},
	} {
		if _, err := Generate(p); err == nil {
			t.Errorf("Generate(%+v) succeeded, want error", p)
		}
	}
}

var (
	phonePattern = regexp.MustCompile(`^\+905\d{9}$`)
	emailPattern = regexp.MustCompile(`^[a-z0-9.]+@example\.(com|net|org)$`)
)

func TestGeneratedValuesSatisfySchema(t *testing.T) {
	ds := mustGenerate(t, defaultParams())
	if len(ds.Users) != 5000 {
		t.Fatalf("users = %d, want 5000", len(ds.Users))
	}
	phones := map[string]bool{}
	emails := map[string]bool{}
	genders := map[string]bool{"female": true, "male": true, "other": true, "unknown": true}
	for _, u := range ds.Users {
		if n := utf8.RuneCountInString(u.FirstName); n < 1 || n > 20 {
			t.Errorf("user %d first_name %q has %d runes", u.ID, u.FirstName, n)
		}
		if n := utf8.RuneCountInString(u.LastName); n < 1 || n > 25 {
			t.Errorf("user %d last_name %q has %d runes", u.ID, u.LastName, n)
		}
		for _, s := range []string{u.FirstName, u.LastName, u.City} {
			if !norm.NFC.IsNormalString(s) || strings.ContainsAny(s, "\t\n\\") {
				t.Errorf("user %d: %q is not NFC or contains a separator", u.ID, s)
			}
		}
		if !genders[u.Gender] {
			t.Errorf("user %d gender %q", u.ID, u.Gender)
		}
		if u.Country != "TR" || (u.Language != "tr" && u.Language != "en") {
			t.Errorf("user %d country %q language %q", u.ID, u.Country, u.Language)
		}
		if u.Phone != nil {
			if !phonePattern.MatchString(*u.Phone) {
				t.Errorf("user %d phone %q is not E.164 TR mobile", u.ID, *u.Phone)
			}
			if phones[*u.Phone] {
				t.Errorf("duplicate phone %q", *u.Phone)
			}
			phones[*u.Phone] = true
		}
		if u.Email != nil {
			if !emailPattern.MatchString(*u.Email) {
				t.Errorf("user %d email %q", u.ID, *u.Email)
			}
			if emails[*u.Email] {
				t.Errorf("duplicate email %q", *u.Email)
			}
			emails[*u.Email] = true
		}
		c, ok := segment.Cities().Lookup(u.City)
		if !ok || c.Key != u.CityNormalized || segment.FoldKey(u.City) != u.CityNormalized {
			t.Errorf("user %d city %q normalized %q (catalog %+v, %v)", u.ID, u.City, u.CityNormalized, c, ok)
		}
		// The {{city}} declared maximum is the longest catalog name; source spellings must not exceed it.
		if utf8.RuneCountInString(u.City) > segment.Cities().MaxNameRunes() {
			t.Errorf("user %d city %q longer than the declared maximum", u.ID, u.City)
		}
	}
	for _, e := range ds.CartEvents {
		if e.Quantity <= 0 || e.UnitPriceCents <= 0 || e.ProductID == "" || e.Category == "" {
			t.Errorf("cart event %+v", e)
		}
		if e.UnitPriceCents >= 1e12 { // NUMERIC(12,2) holds < 10^10 units
			t.Errorf("cart event %d price overflows NUMERIC(12,2)", e.ID)
		}
	}
	for _, p := range ds.Purchases {
		if p.OrderTotalCents <= 0 || p.OrderTotalCents >= 1e12 {
			t.Errorf("purchase %+v", p)
		}
	}
	for _, c := range ds.Consents {
		if c.Status != "opted_in" && c.Status != "opted_out" {
			t.Errorf("consent %+v", c)
		}
		if c.Source == "" {
			t.Errorf("consent %+v without source", c)
		}
	}
}

func TestGeneratedTimestamps(t *testing.T) {
	p := defaultParams()
	ds := mustGenerate(t, p)
	ref := p.ReferenceTime
	windowStart := ref.Add(-activityWindow)
	check := func(what string, ts time.Time) {
		t.Helper()
		if ts.After(ref) {
			t.Errorf("%s %v is after the reference time", what, ts)
		}
		if ts.Location() != time.UTC {
			t.Errorf("%s %v is not UTC", what, ts)
		}
		if ts.Nanosecond()%1000 != 0 {
			t.Errorf("%s %v has sub-microsecond precision", what, ts)
		}
	}
	signup := make(map[int64]time.Time, len(ds.Users))
	for _, u := range ds.Users {
		check("signup_at", u.SignupAt)
		signup[u.ID] = u.SignupAt
		if u.LastActiveAt != nil {
			check("last_active_at", *u.LastActiveAt)
			if u.LastActiveAt.Before(u.SignupAt) {
				t.Errorf("user %d last active before signup", u.ID)
			}
		}
		if u.BirthDate != nil {
			check("birth_date", *u.BirthDate)
			bd := *u.BirthDate
			if bd.Hour() != 0 || bd.Minute() != 0 || bd.Second() != 0 || bd.Nanosecond() != 0 {
				t.Errorf("user %d birth_date %v is not a date", u.ID, bd)
			}
			if age := ref.Year() - bd.Year(); age < 18 || age > 77 {
				t.Errorf("user %d age %d", u.ID, age)
			}
		}
	}
	for _, c := range ds.Consents {
		check("consent updated_at", c.UpdatedAt)
		if c.UpdatedAt.Before(signup[c.UserID]) {
			t.Errorf("consent %+v before signup", c)
		}
	}
	for _, e := range ds.CartEvents {
		check("cart occurred_at", e.OccurredAt)
		if e.OccurredAt.Before(windowStart) || e.OccurredAt.Before(signup[e.UserID]) {
			t.Errorf("cart event %d at %v outside the 90-day window or before signup", e.ID, e.OccurredAt)
		}
	}
	for _, pu := range ds.Purchases {
		check("purchased_at", pu.PurchasedAt)
		if pu.PurchasedAt.Before(windowStart) || pu.PurchasedAt.Before(signup[pu.UserID]) {
			t.Errorf("purchase %d at %v outside the 90-day window or before signup", pu.ID, pu.PurchasedAt)
		}
	}
}

func TestGeneratedOrdering(t *testing.T) {
	ds := mustGenerate(t, defaultParams())
	for i, u := range ds.Users {
		if u.ID != int64(i+1) {
			t.Fatalf("users[%d].ID = %d, want %d", i, u.ID, i+1)
		}
	}
	for i, e := range ds.CartEvents {
		if e.ID != int64(i+1) {
			t.Fatalf("cart_events[%d].ID = %d", i, e.ID)
		}
		if i > 0 && e.UserID < ds.CartEvents[i-1].UserID {
			t.Fatalf("cart events not grouped by user at %d", i)
		}
		if i > 0 && e.UserID == ds.CartEvents[i-1].UserID && e.OccurredAt.Before(ds.CartEvents[i-1].OccurredAt) {
			t.Fatalf("cart events of user %d not chronological at %d", e.UserID, i)
		}
	}
	for i, p := range ds.Purchases {
		if p.ID != int64(i+1) {
			t.Fatalf("purchases[%d].ID = %d", i, p.ID)
		}
		if i > 0 && p.UserID < ds.Purchases[i-1].UserID {
			t.Fatalf("purchases not grouped by user at %d", i)
		}
	}
	rank := map[string]int{"sms": 0, "email": 1}
	for i := 1; i < len(ds.Consents); i++ {
		a, b := ds.Consents[i-1], ds.Consents[i]
		if a.UserID > b.UserID || (a.UserID == b.UserID && rank[a.Channel] >= rank[b.Channel]) {
			t.Fatalf("consents out of (user_id, channel) order at %d: %+v, %+v", i, a, b)
		}
	}
	userIDs := int64(len(ds.Users))
	for _, e := range ds.CartEvents {
		if e.UserID < 1 || e.UserID > userIDs {
			t.Fatalf("cart event %d references unknown user %d", e.ID, e.UserID)
		}
	}
}

// Distribution bounds hold for the default dataset and for other seeds.
func TestDistributionBounds(t *testing.T) {
	for _, seed := range []uint64{42, 1, 7, 2026, 1 << 40} {
		p := defaultParams()
		p.RandomSeed = seed
		ds := mustGenerate(t, p)
		s := summarize(ds, p.ReferenceTime)
		n := float64(len(ds.Users))
		within := func(name string, got int, lo, hi float64) {
			t.Helper()
			if f := float64(got) / n; f < lo || f > hi {
				t.Errorf("seed %d: %s = %d (%.3f), want share in [%.2f, %.2f]", seed, name, got, f, lo, hi)
			}
		}
		within("istanbul", s.istanbul, 0.27, 0.33)
		within("no phone", s.noPhone, 0.05, 0.11)
		within("no email", s.noEmail, 0.035, 0.085)
		within("null birth_date", s.nullBirth, 0.07, 0.13)
		within("null last_active_at", s.nullLastActive, 0.045, 0.095)
		within("phones ending 0000", s.phone0000, 0.01, 0.035)
		for _, ch := range []string{"sms", "email"} {
			within(ch+" opted_in", s.optedIn[ch], 0.57, 0.67)
			within(ch+" opted_out", s.optedOut[ch], 0.16, 0.24)
			within(ch+" no consent row", s.noConsent[ch], 0.14, 0.22)
		}
		within("users with carts", s.cartUsers, 0.40, 0.50)
		if s.recoveredSessions == 0 || s.abandonedSessions == 0 {
			t.Errorf("seed %d: recovered=%d abandoned=%d, want both > 0", seed, s.recoveredSessions, s.abandonedSessions)
		}
		// The example brief (İstanbul, abandoned cart in the last 30 days) matches a meaningful audience.
		if s.istanbulAbandoned30d < 100 {
			t.Errorf("seed %d: İstanbul abandoned-cart-30d users = %d, want ≥ 100", seed, s.istanbulAbandoned30d)
		}
		for _, sp := range []string{"İstanbul", "Istanbul", "ISTANBUL", "istanbul"} {
			if s.istanbulSpellings[sp] == 0 {
				t.Errorf("seed %d: İstanbul spelling %q never generated", seed, sp)
			}
		}
		if s.cities < 60 {
			t.Errorf("seed %d: only %d distinct cities", seed, s.cities)
		}
		for _, g := range []string{"female", "male", "other", "unknown"} {
			if s.genders[g] == 0 {
				t.Errorf("seed %d: gender %q never generated", seed, g)
			}
		}
		if s.languages["en"] == 0 || s.languages["tr"] == 0 {
			t.Errorf("seed %d: languages %v", seed, s.languages)
		}
		if s.nullPurchaseCategory == 0 {
			t.Errorf("seed %d: no purchase with NULL category", seed)
		}
	}
}

type summary struct {
	istanbul, noPhone, noEmail, nullBirth, nullLastActive, phone0000 int
	optedIn, optedOut, noConsent                                     map[string]int
	cartUsers, recoveredSessions, abandonedSessions                  int
	istanbulAbandoned30d, cities, nullPurchaseCategory               int
	istanbulSpellings, genders, languages                            map[string]int
}

func summarize(ds *Dataset, ref time.Time) summary {
	s := summary{
		optedIn: map[string]int{}, optedOut: map[string]int{}, noConsent: map[string]int{},
		istanbulSpellings: map[string]int{}, genders: map[string]int{}, languages: map[string]int{},
	}
	cities := map[string]bool{}
	for _, u := range ds.Users {
		cities[u.CityNormalized] = true
		s.genders[u.Gender]++
		s.languages[u.Language]++
		if u.CityNormalized == "istanbul" {
			s.istanbul++
			s.istanbulSpellings[u.City]++
		}
		if u.Phone == nil {
			s.noPhone++
		} else if strings.HasSuffix(*u.Phone, "0000") {
			s.phone0000++
		}
		if u.Email == nil {
			s.noEmail++
		}
		if u.BirthDate == nil {
			s.nullBirth++
		}
		if u.LastActiveAt == nil {
			s.nullLastActive++
		}
	}
	s.cities = len(cities)

	type key struct {
		user int64
		ch   string
	}
	consent := map[key]string{}
	for _, c := range ds.Consents {
		consent[key{c.UserID, c.Channel}] = c.Status
	}
	for _, u := range ds.Users {
		for _, ch := range []string{"sms", "email"} {
			switch consent[key{u.ID, ch}] {
			case "opted_in":
				s.optedIn[ch]++
			case "opted_out":
				s.optedOut[ch]++
			default:
				s.noConsent[ch]++
			}
		}
	}

	carts := map[int64][]time.Time{}
	purchases := map[int64][]time.Time{}
	for _, e := range ds.CartEvents {
		carts[e.UserID] = append(carts[e.UserID], e.OccurredAt)
	}
	for _, p := range ds.Purchases {
		purchases[p.UserID] = append(purchases[p.UserID], p.PurchasedAt)
		if p.Category == nil {
			s.nullPurchaseCategory++
		}
	}
	s.cartUsers = len(carts)
	purchasedAtOrAfter := func(user int64, t time.Time) bool {
		for _, pt := range purchases[user] {
			if !pt.Before(t) {
				return true
			}
		}
		return false
	}
	for user, ts := range carts {
		for _, t := range ts {
			if purchasedAtOrAfter(user, t) {
				s.recoveredSessions++
			} else {
				s.abandonedSessions++
			}
		}
	}
	since := ref.Add(-30 * 24 * time.Hour)
	for _, u := range ds.Users {
		if u.CityNormalized != "istanbul" {
			continue
		}
		for _, t := range carts[u.ID] {
			if !t.Before(since) && !purchasedAtOrAfter(u.ID, t) {
				s.istanbulAbandoned30d++
				break
			}
		}
	}
	return s
}

func TestCanonicalFormat(t *testing.T) {
	email := "a.b.1@example.com"
	cat := "books"
	bd := time.Date(1990, 2, 3, 0, 0, 0, 0, time.UTC)
	ts := time.Date(2026, 10, 30, 12, 34, 56, 789000, time.UTC)
	ds := &Dataset{
		Users: []User{{ID: 1, FirstName: "Ayşe", LastName: "Yılmaz", Email: &email, City: "ISTANBUL",
			CityNormalized: "istanbul", Country: "TR", Gender: "female", BirthDate: &bd, Language: "tr", SignupAt: ts}},
		Consents:   []Consent{{UserID: 1, Channel: "sms", Status: "opted_in", Source: "import", UpdatedAt: ts}},
		CartEvents: []CartEvent{{ID: 1, UserID: 1, ProductID: "BKS-0001", Category: "books", Quantity: 2, UnitPriceCents: 1205, OccurredAt: ts}},
		Purchases:  []Purchase{{ID: 1, UserID: 1, OrderTotalCents: 7, Category: &cat, PurchasedAt: ts}, {ID: 2, UserID: 1, OrderTotalCents: 100, PurchasedAt: ts}},
	}
	var b strings.Builder
	if err := ds.WriteCanonical(&b); err != nil {
		t.Fatal(err)
	}
	want := "#users\n" +
		"1\tAyşe\tYılmaz\ta.b.1@example.com\t\\N\tISTANBUL\tistanbul\tTR\tfemale\t1990-02-03\ttr\t2026-10-30T12:34:56.000789Z\t\\N\n" +
		"#channel_consents\n" +
		"1\tsms\topted_in\timport\t2026-10-30T12:34:56.000789Z\n" +
		"#cart_events\n" +
		"1\t1\tBKS-0001\tbooks\t2\t12.05\t2026-10-30T12:34:56.000789Z\n" +
		"#purchases\n" +
		"1\t1\t0.07\tbooks\t2026-10-30T12:34:56.000789Z\n" +
		"2\t1\t1.00\t\\N\t2026-10-30T12:34:56.000789Z\n"
	if b.String() != want {
		t.Errorf("canonical form:\n%s\nwant:\n%s", b.String(), want)
	}
}
