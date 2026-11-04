package main

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"slices"
	"strings"
	"time"

	"github.com/fyusufayan/insider-campaign-composer-case/backend/internal/segment"
)

// pcgStream is the fixed second PCG seed word. Changing it changes every dataset.
const pcgStream uint64 = 0x5eed_c0de_2026_0001

// Generation windows.
const (
	day            = 24 * time.Hour
	activityWindow = 90 * day      // carts and purchases span the 90 days before the reference time
	maxSignupAge   = 3 * 365 * day // signups span the three years before the reference time
	minSignupAge   = 1 * day
	maxIdleWindow  = 120 * day // last_active_at is within this window (or the last event)
)

// Params are the seed inputs. The same Params always produce the same Dataset.
type Params struct {
	Users         int
	RandomSeed    uint64
	ReferenceTime time.Time
}

// User mirrors a users row. ID is the identifier the database assigns: rows
// are loaded in slice order into a freshly restarted sequence.
type User struct {
	ID             int64
	FirstName      string
	LastName       string
	Email          *string
	Phone          *string
	City           string // source spelling, e.g. "ISTANBUL"
	CityNormalized string // segment.FoldKey(City), always a catalog key
	Country        string
	Gender         string
	BirthDate      *time.Time // UTC midnight
	Language       string
	SignupAt       time.Time
	LastActiveAt   *time.Time
}

// Consent mirrors a channel_consents row. A user without a row for a channel
// has no consent for it.
type Consent struct {
	UserID    int64
	Channel   string // "sms" | "email"
	Status    string // "opted_in" | "opted_out"
	Source    string
	UpdatedAt time.Time
}

// CartEvent mirrors a cart_events row; prices are integer minor units (kuruş).
type CartEvent struct {
	ID             int64
	UserID         int64
	ProductID      string
	Category       string
	Quantity       int32
	UnitPriceCents int64
	OccurredAt     time.Time
}

// Purchase mirrors a purchases row.
type Purchase struct {
	ID              int64
	UserID          int64
	OrderTotalCents int64
	Category        *string
	PurchasedAt     time.Time
}

// Dataset is a complete generated customer dataset in insertion order.
type Dataset struct {
	Users      []User
	Consents   []Consent // ordered by (user_id, channel) with sms before email
	CartEvents []CartEvent
	Purchases  []Purchase
}

var (
	femaleNames = []string{
		"Ayşe", "Fatma", "Emine", "Hatice", "Zeynep", "Elif", "Meryem", "Şerife", "Zehra", "Sultan",
		"Hanife", "Merve", "Havva", "Zeliha", "Esra", "Fadime", "Özlem", "Hülya", "Yasemin", "Büşra",
		"Gülsüm", "Derya", "Ebru", "Sevgi", "Dilek", "Gamze", "Tuğba", "Kübra", "Esma", "Rabia",
		"Selin", "Defne", "Ecrin", "Nehir", "Asya", "Azra", "Eylül", "İrem", "Damla", "Ceren",
		"Buse", "Gizem", "Cansu", "Pınar", "Nur", "Melek", "Sibel", "Şule", "Gülay", "Nazlı",
		"Aslı", "Didem", "Öykü", "Çağla", "Ilgın", "Işıl", "İpek", "Gökçe", "Begüm", "Sıla",
		"Mehtap", "Gülizar", "Nurgül", "Gülşen", "Feride", "Şükran", "Ümran", "Nesrin", "Berfin", "Hacer",
	}
	maleNames = []string{
		"Mehmet", "Mustafa", "Ahmet", "Ali", "Hüseyin", "Hasan", "İbrahim", "İsmail", "Osman", "Yusuf",
		"Murat", "Ömer", "Ramazan", "Halil", "Süleyman", "Abdullah", "Mahmut", "Salih", "Recep", "Kemal",
		"Emre", "Burak", "Can", "Cem", "Deniz", "Eren", "Barış", "Oğuz", "Serkan", "Volkan",
		"Tolga", "Uğur", "Kaan", "Berk", "Arda", "Alp", "Emir", "Efe", "Yiğit", "Çağrı",
		"Gökhan", "Selçuk", "Erdem", "Onur", "Umut", "Furkan", "Enes", "Kerem", "Mert", "Doruk",
		"Ilgaz", "Ozan", "Tuncay", "Şahin", "Özgür", "Görkem", "Batuhan", "Abdurrahman", "İlker", "Ümit",
		"Şükrü", "Tayfun", "Cihan", "Levent", "Hakan", "Sinan", "Taner", "Orhan", "Erkan", "Bülent",
	}
	lastNames = []string{
		"Yılmaz", "Kaya", "Demir", "Şahin", "Çelik", "Yıldız", "Yıldırım", "Öztürk", "Aydın", "Özdemir",
		"Arslan", "Doğan", "Kılıç", "Aslan", "Çetin", "Kara", "Koç", "Kurt", "Özkan", "Şimşek",
		"Polat", "Özcan", "Korkmaz", "Çakır", "Erdoğan", "Yavuz", "Can", "Acar", "Şen", "Aktaş",
		"Güler", "Yalçın", "Güneş", "Bozkurt", "Bulut", "Keskin", "Ünal", "Turan", "Gül", "Özer",
		"Işık", "Kaplan", "Avcı", "Sarı", "Tekin", "Taş", "Köse", "Yüksel", "Ateş", "Aksoy",
		"Karakaya", "Tunç", "Bayram", "Uçar", "Toprak", "Altun", "Karadeniz", "Çiftçi", "Erdem", "Karataş",
		"Kocabıyık", "Büyükkaya", "Karaosmanoğlu", "Hacıhasanoğlu", "Kahramanoğulları", "Ilıcak", "İnce", "Ölmez", "Üstün", "Çağlar",
	}

	// istanbulSpellings are the source spellings of İstanbul with cumulative weights (out of 100).
	istanbulSpellings = []weighted{{"İstanbul", 55}, {"Istanbul", 75}, {"ISTANBUL", 85}, {"istanbul", 100}}

	// majorCities are drawn for most non-İstanbul users, with cumulative weights.
	majorCities = cumulative([]weighted{
		{"Ankara", 14}, {"İzmir", 11}, {"Bursa", 7}, {"Antalya", 7}, {"Adana", 5}, {"Konya", 5},
		{"Gaziantep", 4}, {"Kocaeli", 4}, {"Mersin", 4}, {"Kayseri", 3}, {"Eskişehir", 3},
		{"Trabzon", 2}, {"Samsun", 2}, {"Diyarbakır", 2}, {"Şanlıurfa", 2}, {"Denizli", 2}, {"Muğla", 2},
	})

	// mobilePrefixes are Turkish mobile operator codes (+90 5xx).
	mobilePrefixes = []string{
		"501", "505", "506", "507", "530", "531", "532", "533", "534", "535", "536", "537", "538", "539",
		"540", "541", "542", "543", "544", "545", "546", "547", "548", "549",
		"551", "552", "553", "554", "555", "559",
	}

	emailDomains   = []string{"example.com", "example.net", "example.org"}
	consentSources = []string{"signup_form", "checkout", "preference_center", "import"}

	categories = []category{
		{"electronics", "ELC", 49900, 2499900},
		{"fashion", "FSH", 9900, 349900},
		{"home", "HOM", 4900, 499900},
		{"beauty", "BTY", 2900, 149900},
		{"sports", "SPT", 7900, 399900},
		{"books", "BKS", 2900, 49900},
		{"grocery", "GRC", 990, 29900},
		{"toys", "TOY", 4900, 249900},
	}
)

type weighted struct {
	value string
	cum   int // cumulative weight
}

type category struct {
	name     string
	sku      string
	minCents int64
	maxCents int64
}

// cumulative turns per-item weights into cumulative weights.
func cumulative(ws []weighted) []weighted {
	out := make([]weighted, len(ws))
	sum := 0
	for i, w := range ws {
		sum += w.cum
		out[i] = weighted{w.value, sum}
	}
	return out
}

type generator struct {
	rng    *rand.Rand
	ref    time.Time
	cities *segment.CityCatalog
	phones map[string]struct{} // membership only; never iterated
	emails map[string]struct{}
	ds     *Dataset
}

// Generate builds the dataset for p. It reads no clock and no global random
// state: every value comes from a PCG generator seeded with (p.RandomSeed,
// pcgStream) and drawn in a fixed order, and every timestamp is derived from
// p.ReferenceTime at microsecond precision.
func Generate(p Params) (*Dataset, error) {
	if p.Users < 1 {
		return nil, errors.New("seed: Users must be at least 1")
	}
	if p.ReferenceTime.IsZero() {
		return nil, errors.New("seed: ReferenceTime is required")
	}
	g := &generator{
		rng:    rand.New(rand.NewPCG(p.RandomSeed, pcgStream)),
		ref:    p.ReferenceTime.UTC().Truncate(time.Microsecond),
		cities: segment.Cities(),
		phones: make(map[string]struct{}, p.Users),
		emails: make(map[string]struct{}, p.Users),
		ds: &Dataset{
			Users:    make([]User, 0, p.Users),
			Consents: make([]Consent, 0, 2*p.Users),
		},
	}
	for id := int64(1); id <= int64(p.Users); id++ {
		if err := g.user(id); err != nil {
			return nil, err
		}
	}
	return g.ds, nil
}

// user generates one user and all of its dependent rows. The draw order below
// is part of the determinism contract.
func (g *generator) user(id int64) error {
	u := User{ID: id, Country: "TR"}

	switch r := g.rng.IntN(100); {
	case r < 48:
		u.Gender = "female"
	case r < 96:
		u.Gender = "male"
	case r < 97:
		u.Gender = "other"
	default:
		u.Gender = "unknown"
	}
	names := femaleNames
	switch u.Gender {
	case "male":
		names = maleNames
	case "other", "unknown":
		if g.rng.IntN(2) == 1 {
			names = maleNames
		}
	}
	u.FirstName = pick(g.rng, names)
	u.LastName = pick(g.rng, lastNames)

	u.City = g.city()
	city, ok := g.cities.Lookup(u.City)
	if !ok {
		return fmt.Errorf("seed: generated unknown city %q", u.City)
	}
	u.CityNormalized = city.Key

	u.SignupAt = g.before(g.ref, minSignupAge, maxSignupAge)

	if g.rng.IntN(100) >= 10 { // ~10 % NULL birth_date
		age := 18 + g.rng.IntN(58)
		refDate := time.Date(g.ref.Year(), g.ref.Month(), g.ref.Day(), 0, 0, 0, 0, time.UTC)
		bd := refDate.AddDate(-age, 0, -g.rng.IntN(365))
		u.BirthDate = &bd
	}

	u.Language = "tr"
	if g.rng.IntN(100) < 12 {
		u.Language = "en"
	}

	if g.rng.IntN(100) >= 8 { // ~8 % without a phone
		p := g.phone()
		u.Phone = &p
	}
	if g.rng.IntN(100) >= 6 { // ~6 % without an email
		e, err := g.email(u.FirstName, u.LastName, id)
		if err != nil {
			return err
		}
		u.Email = &e
	}

	for _, ch := range []string{"sms", "email"} { // enum order
		var status string
		switch r := g.rng.IntN(100); {
		case r < 62:
			status = "opted_in"
		case r < 82:
			status = "opted_out"
		default:
			continue // ~18 % without a consent row
		}
		g.ds.Consents = append(g.ds.Consents, Consent{
			UserID:    id,
			Channel:   ch,
			Status:    status,
			Source:    pick(g.rng, consentSources),
			UpdatedAt: g.between(u.SignupAt, g.ref),
		})
	}

	lastEvent := g.activity(id, u.SignupAt)

	if g.rng.IntN(100) >= 7 { // ~7 % NULL last_active_at
		la := g.between(maxTime(u.SignupAt, g.ref.Add(-maxIdleWindow)), g.ref)
		la = maxTime(la, lastEvent)
		u.LastActiveAt = &la
	}

	g.ds.Users = append(g.ds.Users, u)
	return nil
}

// city returns a source spelling of a catalog city: ~30 % İstanbul (in mixed
// spellings), most of the rest from the large cities, the remainder uniform.
func (g *generator) city() string {
	r := g.rng.IntN(1000)
	switch {
	case r < 300:
		return pickWeighted(g.rng, istanbulSpellings)
	case r < 790:
		return pickWeighted(g.rng, majorCities)
	default:
		istanbul, _ := g.cities.Lookup("istanbul")
		for {
			c := g.cities.At(g.rng.IntN(g.cities.Len()))
			if c.Key != istanbul.Key {
				return c.Name
			}
		}
	}
}

// phone returns a unique E.164 Turkish mobile number. About 2 % end in 0000
// (the mock provider's permanent-failure pattern); no other number does.
func (g *generator) phone() string {
	permanentFail := g.rng.IntN(100) < 2
	for {
		prefix := pick(g.rng, mobilePrefixes)
		var sub int
		if permanentFail {
			sub = g.rng.IntN(1000) * 10000
		} else {
			sub = g.rng.IntN(10_000_000)
			if sub%10000 == 0 {
				continue
			}
		}
		p := fmt.Sprintf("+90%s%07d", prefix, sub)
		if _, dup := g.phones[p]; dup {
			continue
		}
		g.phones[p] = struct{}{}
		return p
	}
}

// email returns a unique lowercase ASCII address on a reserved example domain.
func (g *generator) email(first, last string, id int64) (string, error) {
	f := asciiLocal(first)
	l := asciiLocal(last)
	var local string
	switch g.rng.IntN(3) {
	case 0:
		local = fmt.Sprintf("%s.%s.%d", f, l, id)
	case 1:
		local = fmt.Sprintf("%s%s%d", f, l, id)
	default:
		local = fmt.Sprintf("%s.%s%d", f[:1], l, id)
	}
	e := local + "@" + pick(g.rng, emailDomains)
	if _, dup := g.emails[e]; dup {
		// first.last.id is unique by construction (names have no dots, ids are unique).
		e = fmt.Sprintf("%s.%s.%d@%s", f, l, id, emailDomains[0])
		if _, dup := g.emails[e]; dup {
			return "", fmt.Errorf("seed: duplicate email %q", e)
		}
	}
	g.emails[e] = struct{}{}
	return e, nil
}

// activity generates the user's cart sessions (each abandoned or recovered by
// a purchase) and standalone purchases within the 90-day window, never before
// signup and never after the reference time. It returns the latest event time
// (zero if none).
func (g *generator) activity(userID int64, signupAt time.Time) time.Time {
	start := maxTime(signupAt, g.ref.Add(-activityWindow))
	var last time.Time

	if g.rng.IntN(100) < 45 {
		sessions := make([]time.Time, 1+g.rng.IntN(4))
		for i := range sessions {
			sessions[i] = g.between(start, g.ref)
		}
		slices.SortFunc(sessions, func(a, b time.Time) int { return a.Compare(b) })

		for _, at := range sessions {
			items := 1 + g.rng.IntN(3)
			var total int64
			var mainCategory string
			t := maxTime(at, last) // keep each user's cart events chronological
			for j := 0; j < items; j++ {
				c := categories[g.rng.IntN(len(categories))]
				if j == 0 {
					mainCategory = c.name
				}
				qty := int32(1 + g.rng.IntN(3))
				price := c.minCents + g.rng.Int64N(c.maxCents-c.minCents+1)
				if j > 0 {
					t = minTime(t.Add(g.duration(0, 15*time.Minute)), g.ref)
				}
				g.ds.CartEvents = append(g.ds.CartEvents, CartEvent{
					ID:             int64(len(g.ds.CartEvents) + 1),
					UserID:         userID,
					ProductID:      fmt.Sprintf("%s-%04d", c.sku, g.rng.IntN(10000)),
					Category:       c.name,
					Quantity:       qty,
					UnitPriceCents: price,
					OccurredAt:     t,
				})
				total += int64(qty) * price
			}
			last = maxTime(last, t)

			if g.rng.IntN(100) < 50 { // recovered unless the purchase would fall after the reference time
				pt := t.Add(g.duration(5*time.Minute, 48*time.Hour))
				if !pt.After(g.ref) {
					var cat *string
					if g.rng.IntN(100) >= 10 {
						cat = &mainCategory
					}
					g.purchase(userID, total, cat, pt)
					last = maxTime(last, pt)
				}
			}
		}
	}

	if g.rng.IntN(100) < 15 { // standalone purchases without a cart event
		n := 1 + g.rng.IntN(2)
		times := make([]time.Time, n)
		for i := range times {
			times[i] = g.between(start, g.ref)
		}
		slices.SortFunc(times, func(a, b time.Time) int { return a.Compare(b) })
		for _, pt := range times {
			total := 5000 + g.rng.Int64N(200000-5000+1)
			var cat *string
			if g.rng.IntN(100) < 70 {
				name := categories[g.rng.IntN(len(categories))].name
				cat = &name
			}
			g.purchase(userID, total, cat, pt)
			last = maxTime(last, pt)
		}
	}
	return last
}

func (g *generator) purchase(userID, totalCents int64, cat *string, at time.Time) {
	g.ds.Purchases = append(g.ds.Purchases, Purchase{
		ID:              int64(len(g.ds.Purchases) + 1),
		UserID:          userID,
		OrderTotalCents: totalCents,
		Category:        cat,
		PurchasedAt:     at,
	})
}

// duration draws a whole number of microseconds uniformly from [lo, hi].
func (g *generator) duration(lo, hi time.Duration) time.Duration {
	loUS, hiUS := lo.Microseconds(), hi.Microseconds()
	return time.Duration(loUS+g.rng.Int64N(hiUS-loUS+1)) * time.Microsecond
}

// before returns t minus a uniform offset in [lo, hi].
func (g *generator) before(t time.Time, lo, hi time.Duration) time.Time {
	return t.Add(-g.duration(lo, hi))
}

// between returns a uniform instant in [a, b] (a when b is not after a).
func (g *generator) between(a, b time.Time) time.Time {
	if !b.After(a) {
		return a
	}
	return a.Add(g.duration(0, b.Sub(a)))
}

func pick[T any](r *rand.Rand, xs []T) T { return xs[r.IntN(len(xs))] }

func pickWeighted(r *rand.Rand, ws []weighted) string {
	n := r.IntN(ws[len(ws)-1].cum)
	for _, w := range ws {
		if n < w.cum {
			return w.value
		}
	}
	return ws[len(ws)-1].value // unreachable
}

// asciiLocal folds a Turkish name to lowercase ASCII letters for an email local part.
func asciiLocal(name string) string {
	return strings.ReplaceAll(segment.FoldKey(name), " ", "")
}

func maxTime(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}

func minTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}

// ---- Canonical serialization -------------------------------------------------
//
// The dataset hash is SHA-256 over this text form, which is reproducible from
// the database alone (the integration test rebuilds it in SQL):
//
//	"#users\n", then per user in id order, tab-separated:
//	  id first_name last_name email phone city city_normalized country gender
//	  birth_date language signup_at last_active_at
//	"#channel_consents\n", then per row in (user_id, channel) order with sms first:
//	  user_id channel status source updated_at
//	"#cart_events\n", then per row in id order:
//	  id user_id product_id category quantity unit_price occurred_at
//	"#purchases\n", then per row in id order:
//	  id user_id order_total category purchased_at
//
// Every line ends with "\n". NULL is `\N`. Timestamps are UTC with exactly six
// fractional digits ("2006-01-02T15:04:05.000000Z"), dates "2006-01-02", and
// money has exactly two decimals ("123.45").

const (
	canonicalTimestamp = "2006-01-02T15:04:05.000000Z"
	canonicalDate      = "2006-01-02"
	canonicalNull      = `\N`
)

// Hash returns the hex SHA-256 of the canonical serialization.
func (d *Dataset) Hash() string {
	h := sha256.New()
	if err := d.WriteCanonical(h); err != nil {
		panic(err) // hash.Hash.Write never fails
	}
	return hex.EncodeToString(h.Sum(nil))
}

// WriteCanonical writes the canonical serialization of d to w.
func (d *Dataset) WriteCanonical(w io.Writer) error {
	var b strings.Builder
	line := func(fields ...string) {
		b.WriteString(strings.Join(fields, "\t"))
		b.WriteByte('\n')
	}
	b.WriteString("#users\n")
	for _, u := range d.Users {
		bd := canonicalNull
		if u.BirthDate != nil {
			bd = u.BirthDate.UTC().Format(canonicalDate)
		}
		line(itoa(u.ID), u.FirstName, u.LastName, optString(u.Email), optString(u.Phone), u.City,
			u.CityNormalized, u.Country, u.Gender, bd, u.Language, ts(u.SignupAt), optTime(u.LastActiveAt))
	}
	b.WriteString("#channel_consents\n")
	for _, c := range d.Consents {
		line(itoa(c.UserID), c.Channel, c.Status, c.Source, ts(c.UpdatedAt))
	}
	b.WriteString("#cart_events\n")
	for _, e := range d.CartEvents {
		line(itoa(e.ID), itoa(e.UserID), e.ProductID, e.Category, itoa(int64(e.Quantity)),
			money(e.UnitPriceCents), ts(e.OccurredAt))
	}
	b.WriteString("#purchases\n")
	for _, p := range d.Purchases {
		line(itoa(p.ID), itoa(p.UserID), money(p.OrderTotalCents), optString(p.Category), ts(p.PurchasedAt))
	}
	_, err := io.WriteString(w, b.String())
	return err
}

func itoa(n int64) string { return fmt.Sprintf("%d", n) }

func ts(t time.Time) string { return t.UTC().Format(canonicalTimestamp) }

func money(cents int64) string { return fmt.Sprintf("%d.%02d", cents/100, cents%100) }

func optString(s *string) string {
	if s == nil {
		return canonicalNull
	}
	return *s
}

func optTime(t *time.Time) string {
	if t == nil {
		return canonicalNull
	}
	return ts(*t)
}
