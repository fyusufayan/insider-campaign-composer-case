//go:build integration

package integration

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/fyusufayan/insider-campaign-composer-case/backend/internal/segment"
	"github.com/fyusufayan/insider-campaign-composer-case/backend/test/integration/testsupport"
)

// ---- 30-user fixture with hand-computed expectations ----
//
// Every time is relative to now() of the fixture transaction; the queries run
// in later transactions, so an event "N days" ago is a little more than N
// days old when evaluated. Window edges are therefore never placed exactly
// on N days here (TestSegmentWindowBoundaries tests exact edges inside one
// transaction). Signup/last-activity offsets carry an extra hour so that the
// whole number of elapsed days is unambiguous.
//
// Channel state codes:  "-"  no contact, no consent row
//                       "-I" no contact, opted_in row (still no_contact)
//                       "C"  contact, no consent row (no_consent)
//                       "CO" contact, opted_out      (no_consent)
//                       "CI" contact, opted_in       (eligible)

type fxEvent struct {
	ago      string // PostgreSQL interval before now()
	category string // "" = NULL (purchases only)
}

type fxUser struct {
	id            int64
	city, cityKey string
	sms, email    string
	gender        string
	age           int // 0 = NULL birth_date
	lang, country string
	signup        string // interval before now()
	lastActive    string // "" = NULL
	carts         []fxEvent
	purchases     []fxEvent
}

func ev(ago, cat string) fxEvent { return fxEvent{ago, cat} }

var fixtureUsers = []fxUser{
	// İstanbul (1–10), mixed display spellings, one key.
	{1, "İstanbul", "istanbul", "CI", "CI", "female", 25, "tr", "TR", "100 days 1 hour", "1 day 1 hour",
		[]fxEvent{ev("5 days", "electronics")}, nil},
	{2, "ISTANBUL", "istanbul", "CI", "CO", "male", 34, "tr", "TR", "200 days 1 hour", "3 days 1 hour",
		[]fxEvent{ev("10 days", "fashion")}, []fxEvent{ev("8 days", "fashion")}}, // recovered
	{3, "Istanbul", "istanbul", "CO", "CI", "female", 41, "en", "TR", "50 days 1 hour", "10 days 1 hour",
		[]fxEvent{ev("10 days", "books")}, []fxEvent{ev("12 days", "books")}}, // purchase before cart
	{4, "istanbul", "istanbul", "C", "C", "other", 0, "tr", "TR", "400 days 1 hour", "",
		[]fxEvent{ev("20 days", "home")}, []fxEvent{ev("15 days", "toys")}}, // recovered by another category
	{5, "İSTANBUL", "istanbul", "-I", "CI", "male", 19, "tr", "TR", "30 days 1 hour", "2 days 1 hour",
		[]fxEvent{ev("3 days", "toys"), ev("25 days", "toys")}, []fxEvent{ev("10 days", "toys")}}, // later cart unrecovered
	{6, "İstanbul", "istanbul", "CI", "-I", "female", 60, "tr", "TR", "1000 days 1 hour", "40 days 1 hour",
		[]fxEvent{ev("40 days", "beauty")}, nil}, // outside 30, inside 60
	{7, "İstanbul", "istanbul", "-", "-", "unknown", 30, "en", "TR", "5 days 1 hour", "1 hour", nil, nil},
	{8, "İstanbul", "istanbul", "CI", "C", "male", 22, "tr", "TR", "15 days 1 hour", "5 days 1 hour",
		[]fxEvent{ev("2 days", "sports"), ev("4 days", "sports"), ev("6 days", "sports")}, nil},
	{9, "İstanbul", "istanbul", "C", "CO", "female", 45, "tr", "TR", "700 days 1 hour", "7 days 1 hour",
		[]fxEvent{ev("7 days", "grocery")}, []fxEvent{ev("7 days", "grocery")}}, // same timestamp: recovered
	{10, "İstanbul", "istanbul", "CI", "CI", "male", 0, "en", "TR", "3000 days 1 hour", "",
		nil, []fxEvent{ev("1 day", "electronics"), ev("50 days", "")}},
	// Ankara (11–15)
	{11, "Ankara", "ankara", "CI", "CI", "female", 28, "tr", "TR", "20 days 1 hour", "1 day 1 hour",
		[]fxEvent{ev("1 day", "electronics")}, nil},
	{12, "ANKARA", "ankara", "-", "CI", "male", 35, "tr", "TR", "365 days 1 hour", "30 days 1 hour",
		[]fxEvent{ev("100 days", "fashion")}, nil},
	{13, "Ankara", "ankara", "CO", "CO", "female", 50, "tr", "TR", "60 days 1 hour", "60 days 1 hour",
		nil, []fxEvent{ev("5 days", "home"), ev("6 days", "home")}},
	{14, "Ankara", "ankara", "CI", "-", "male", 17, "en", "TR", "10 days 1 hour", "",
		[]fxEvent{ev("9 days", "toys")}, []fxEvent{ev("8 days", "books")}},
	{15, "Ankara", "ankara", "C", "-I", "other", 70, "tr", "TR", "2 days 1 hour", "1 day 1 hour", nil, nil},
	// İzmir (16–20)
	{16, "İzmir", "izmir", "CI", "CI", "female", 33, "tr", "TR", "45 days 1 hour", "4 days 1 hour",
		[]fxEvent{ev("29 days", "beauty")}, nil},
	{17, "IZMIR", "izmir", "-", "-", "male", 40, "en", "DE", "80 days 1 hour", "20 days 1 hour",
		[]fxEvent{ev("31 days", "books")}, nil},
	{18, "izmir", "izmir", "CI", "CO", "female", 26, "tr", "TR", "120 days 1 hour", "6 days 1 hour",
		[]fxEvent{ev("14 days", "electronics"), ev("13 days", "fashion")}, []fxEvent{ev("12 days", "fashion")}},
	{19, "İzmir", "izmir", "CO", "CI", "male", 55, "tr", "TR", "90 days 1 hour", "15 days 1 hour",
		[]fxEvent{ev("2 days", "home")}, []fxEvent{ev("3 days", "home")}},
	{20, "İzmir", "izmir", "C", "C", "unknown", 13, "tr", "TR", "12 hours", "12 hours", nil, nil},
	// Çanakkale (21–23)
	{21, "Çanakkale", "canakkale", "CI", "CI", "female", 38, "tr", "TR", "250 days 1 hour", "2 days 1 hour",
		[]fxEvent{ev("61 days", "books")}, []fxEvent{ev("62 days", "books")}},
	{22, "ÇANAKKALE", "canakkale", "-I", "-I", "male", 44, "en", "TR", "300 days 1 hour", "9 days 1 hour",
		[]fxEvent{ev("1 day", "books")}, nil},
	{23, "canakkale", "canakkale", "CI", "C", "female", 120, "tr", "TR", "3650 days 1 hour", "100 days 1 hour", nil, nil},
	// Iğdır (24–25)
	{24, "Iğdır", "igdir", "CI", "CI", "male", 29, "tr", "TR", "33 days 1 hour", "3 days 1 hour",
		[]fxEvent{ev("8 days", "grocery"), ev("18 days", "grocery")}, []fxEvent{ev("12 days", "grocery")}},
	{25, "IĞDIR", "igdir", "CO", "CO", "female", 31, "en", "TR", "70 days 1 hour", "25 days 1 hour",
		[]fxEvent{ev("45 days", "toys")}, nil},
	// Şanlıurfa (26–27)
	{26, "Şanlıurfa", "sanliurfa", "CI", "CI", "male", 48, "tr", "TR", "150 days 1 hour", "8 days 1 hour",
		nil, []fxEvent{ev("20 days", "sports"), ev("40 days", "sports"), ev("70 days", "sports")}},
	{27, "ŞANLIURFA", "sanliurfa", "C", "C", "female", 52, "tr", "TR", "500 days 1 hour", "50 days 1 hour",
		[]fxEvent{ev("15 days", "home")}, []fxEvent{ev("14 days", "home")}},
	// Eskişehir (28–30)
	{28, "Eskişehir", "eskisehir", "CI", "CO", "other", 23, "tr", "TR", "25 days 1 hour", "1 day 1 hour",
		[]fxEvent{ev("4 days", "fashion")}, nil},
	{29, "ESKİŞEHİR", "eskisehir", "-", "CI", "female", 0, "tr", "TR", "1 day 1 hour", "", nil, nil},
	{30, "eskisehir", "eskisehir", "CI", "CI", "male", 36, "en", "GB", "600 days 1 hour", "200 days 1 hour",
		[]fxEvent{ev("3 days", "electronics")}, []fxEvent{ev("2 days", "electronics")}},
}

const (
	fxInsertUser = `
INSERT INTO users (id, first_name, last_name, email, phone, city, city_normalized, country, gender,
                   birth_date, language, signup_at, last_active_at)
VALUES ($1, 'Test', 'User', $2, $3, $4, $5, $6, $7,
        ((now() AT TIME ZONE 'UTC')::date - make_interval(years => $8::integer) - interval '30 days')::date,
        $9, now() - $10::interval, now() - $11::interval)`
	fxInsertConsent  = `INSERT INTO channel_consents (user_id, channel, status, source) VALUES ($1, $2, $3, 'fixture')`
	fxInsertCart     = `INSERT INTO cart_events (user_id, product_id, category, quantity, unit_price, occurred_at) VALUES ($1, 'P-1', $2, 1, 10.00, now() - $3::interval)`
	fxInsertPurchase = `INSERT INTO purchases (user_id, order_total, category, purchased_at) VALUES ($1, 10.00, $2, now() - $3::interval)`
)

func loadFixture(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	ctx := context.Background()
	err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		for _, u := range fixtureUsers {
			var email, phone, last *string
			var age *int
			if u.email != "-" && u.email != "-I" {
				s := fmt.Sprintf("user%d@example.test", u.id)
				email = &s
			}
			if u.sms != "-" && u.sms != "-I" {
				s := fmt.Sprintf("+90555000%04d", u.id)
				phone = &s
			}
			if u.age != 0 {
				age = &u.age
			}
			if u.lastActive != "" {
				last = &u.lastActive
			}
			if _, err := tx.Exec(ctx, fxInsertUser, u.id, email, phone, u.city, u.cityKey, u.country, u.gender,
				age, u.lang, u.signup, last); err != nil {
				return fmt.Errorf("user %d: %w", u.id, err)
			}
			for ch, state := range map[string]string{"sms": u.sms, "email": u.email} {
				var status string
				switch state {
				case "-I", "CI":
					status = "opted_in"
				case "CO":
					status = "opted_out"
				default:
					continue
				}
				if _, err := tx.Exec(ctx, fxInsertConsent, u.id, ch, status); err != nil {
					return fmt.Errorf("consent %d: %w", u.id, err)
				}
			}
			for _, e := range u.carts {
				if _, err := tx.Exec(ctx, fxInsertCart, u.id, e.category, e.ago); err != nil {
					return fmt.Errorf("cart %d: %w", u.id, err)
				}
			}
			for _, e := range u.purchases {
				var cat *string
				if e.category != "" {
					cat = &e.category
				}
				if _, err := tx.Exec(ctx, fxInsertPurchase, u.id, cat, e.ago); err != nil {
					return fmt.Errorf("purchase %d: %w", u.id, err)
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("load fixture: %v", err)
	}
}

func parseRule(t *testing.T, js string) segment.Validated {
	t.Helper()
	v, err := segment.Parse([]byte(js))
	if err != nil {
		t.Fatalf("Parse(%s): %v", js, err)
	}
	return v
}

// q is a querier: a pool or a transaction.
type q interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// matchedIDs runs the compiled statement and returns the sorted user ids.
func matchedIDs(t *testing.T, db q, v segment.Validated) []int64 {
	t.Helper()
	sql, args, err := segment.Compile(v)
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	rows, err := db.Query(context.Background(), sql, args...)
	if err != nil {
		t.Fatalf("query: %v\n%s", err, sql)
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[int64])
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	slices.Sort(ids)
	if len(slices.Compact(slices.Clone(ids))) != len(ids) {
		t.Fatalf("duplicate user ids in %v", ids)
	}
	return ids
}

func ids(xs ...int64) []int64 { return xs }

func idRange(from, to int64) []int64 {
	var out []int64
	for i := from; i <= to; i++ {
		out = append(out, i)
	}
	return out
}

func allExcept(ex ...int64) []int64 {
	var out []int64
	for i := int64(1); i <= 30; i++ {
		if !slices.Contains(ex, i) {
			out = append(out, i)
		}
	}
	return out
}

func union(a, b []int64) []int64 {
	out := slices.Concat(a, b)
	slices.Sort(out)
	return slices.Compact(out)
}

func cond(c string) string { return `{"match":"all","conditions":[` + c + `]}` }

const (
	cityIstanbul = `{"type":"attribute","field":"city","op":"eq","value":"İstanbul"}`
	abandoned30  = `{"type":"behavior","predicate":"abandoned_cart","within_days":30}`
)

func TestSegmentFixtureMatches(t *testing.T) {
	pool := testsupport.Pool(t, pg.NewMigratedDatabase(t))
	loadFixture(t, pool)

	var (
		istanbul      = idRange(1, 10)
		cartIn30      = ids(1, 2, 3, 4, 5, 8, 9, 11, 14, 16, 18, 19, 22, 24, 27, 28, 30)
		noCartIn30    = ids(6, 7, 10, 12, 13, 15, 17, 20, 21, 23, 25, 26, 29)
		abandonedIn30 = ids(1, 3, 5, 8, 11, 16, 19, 22, 24, 28)
		abandonedIn60 = union(abandonedIn30, ids(6, 17, 25))
	)

	tests := []struct {
		name string
		rule string
		want []int64
	}{
		// ---- city (Turkish-aware normalization) ----
		{"city İstanbul", cond(cityIstanbul), istanbul},
		{"city ISTANBUL", cond(`{"type":"attribute","field":"city","op":"eq","value":"ISTANBUL"}`), istanbul},
		{"city ıstanbul (dotless)", cond(`{"type":"attribute","field":"city","op":"eq","value":"ıstanbul"}`), istanbul},
		{"city İSTANBUL", cond(`{"type":"attribute","field":"city","op":"eq","value":"  İSTANBUL "}`), istanbul},
		{"city in Ankara/IZMIR", cond(`{"type":"attribute","field":"city","op":"in","value":["Ankara","IZMIR"]}`), idRange(11, 20)},
		{"city neq istanbul", cond(`{"type":"attribute","field":"city","op":"neq","value":"istanbul"}`), idRange(11, 30)},
		{"city not_in", cond(`{"type":"attribute","field":"city","op":"not_in","value":["İstanbul","ANKARA"]}`), idRange(16, 30)},
		{"city ÇANAKKALE", cond(`{"type":"attribute","field":"city","op":"eq","value":"ÇANAKKALE"}`), ids(21, 22, 23)},
		{"city IGDIR (ASCII)", cond(`{"type":"attribute","field":"city","op":"eq","value":"IGDIR"}`), ids(24, 25)},
		{"city sanliurfa (ASCII)", cond(`{"type":"attribute","field":"city","op":"eq","value":"sanliurfa"}`), ids(26, 27)},
		{"city eskişehir", cond(`{"type":"attribute","field":"city","op":"eq","value":"eskişehir"}`), ids(28, 29, 30)},
		{"city with nobody", cond(`{"type":"attribute","field":"city","op":"eq","value":"Van"}`), nil},

		// ---- other attributes ----
		{"country tr", cond(`{"type":"attribute","field":"country","op":"eq","value":"tr"}`), allExcept(17, 30)},
		{"country in DE/GB", cond(`{"type":"attribute","field":"country","op":"in","value":["DE","gb"]}`), ids(17, 30)},
		{"gender female", cond(`{"type":"attribute","field":"gender","op":"eq","value":"female"}`), ids(1, 3, 6, 9, 11, 13, 16, 18, 21, 23, 25, 27, 29)},
		{"gender other/unknown", cond(`{"type":"attribute","field":"gender","op":"in","value":["other","unknown"]}`), ids(4, 7, 15, 20, 28)},
		{"language en", cond(`{"type":"attribute","field":"language","op":"eq","value":"en"}`), ids(3, 7, 10, 14, 17, 22, 25, 30)},
		{"age gte 40", cond(`{"type":"attribute","field":"age","op":"gte","value":40}`), ids(3, 6, 9, 13, 15, 17, 19, 22, 23, 26, 27)},
		{"age lte 25", cond(`{"type":"attribute","field":"age","op":"lte","value":25}`), ids(1, 5, 8, 14, 20, 28)},
		{"age between 25..35", cond(`{"type":"attribute","field":"age","op":"between","value":{"min":25,"max":35}}`), ids(1, 2, 7, 11, 12, 16, 18, 24, 25)},
		{"age lte 120 excludes NULL birth dates", cond(`{"type":"attribute","field":"age","op":"lte","value":120}`), allExcept(4, 10, 29)},
		{"age gte 13 excludes NULL birth dates", cond(`{"type":"attribute","field":"age","op":"gte","value":13}`), allExcept(4, 10, 29)},
		{"signup lte 30", cond(`{"type":"attribute","field":"signup_days_ago","op":"lte","value":30}`), ids(5, 7, 8, 11, 14, 15, 20, 28, 29)},
		{"signup gte 365", cond(`{"type":"attribute","field":"signup_days_ago","op":"gte","value":365}`), ids(4, 6, 9, 10, 12, 23, 27, 30)},
		{"signup between 30..60", cond(`{"type":"attribute","field":"signup_days_ago","op":"between","value":{"min":30,"max":60}}`), ids(3, 5, 13, 16, 24)},
		{"signup lte 0 (today)", cond(`{"type":"attribute","field":"signup_days_ago","op":"lte","value":0}`), ids(20)},
		{"signup gte 0", cond(`{"type":"attribute","field":"signup_days_ago","op":"gte","value":0}`), idRange(1, 30)},
		{"last active lte 7", cond(`{"type":"attribute","field":"last_active_days_ago","op":"lte","value":7}`), ids(1, 2, 5, 7, 8, 9, 11, 15, 16, 18, 20, 21, 24, 28)},
		{"last active gte 30", cond(`{"type":"attribute","field":"last_active_days_ago","op":"gte","value":30}`), ids(6, 12, 13, 23, 27, 30)},
		{"last active lte 3650 excludes NULL", cond(`{"type":"attribute","field":"last_active_days_ago","op":"lte","value":3650}`), allExcept(4, 10, 14, 29)},

		// ---- events ----
		{"cart exists 30", cond(`{"type":"event","event":"cart_add","op":"exists","within_days":30}`), cartIn30},
		{"cart count_gte 1 ≡ exists", cond(`{"type":"event","event":"cart_add","op":"count_gte","count":1,"within_days":30}`), cartIn30},
		{"cart not_exists 30", cond(`{"type":"event","event":"cart_add","op":"not_exists","within_days":30}`), noCartIn30},
		{"cart count_lte 0 ≡ not_exists", cond(`{"type":"event","event":"cart_add","op":"count_lte","count":0,"within_days":30}`), noCartIn30},
		{"cart count_gte 0 matches everyone", cond(`{"type":"event","event":"cart_add","op":"count_gte","count":0,"within_days":30}`), idRange(1, 30)},
		{"purchase count_gte 0 matches everyone", cond(`{"type":"event","event":"purchase","op":"count_gte","count":0,"within_days":1,"category_in":["toys"]}`), idRange(1, 30)},
		{"cart count_gte 2", cond(`{"type":"event","event":"cart_add","op":"count_gte","count":2,"within_days":30}`), ids(5, 8, 18, 24)},
		{"cart count_gte 3", cond(`{"type":"event","event":"cart_add","op":"count_gte","count":3,"within_days":30}`), ids(8)},
		{"cart count_lte 1", cond(`{"type":"event","event":"cart_add","op":"count_lte","count":1,"within_days":30}`), allExcept(5, 8, 18, 24)},
		{"cart count_gte 2 sports", cond(`{"type":"event","event":"cart_add","op":"count_gte","count":2,"within_days":30,"category_in":["sports"]}`), ids(8)},
		{"cart count_gte 2 toys", cond(`{"type":"event","event":"cart_add","op":"count_gte","count":2,"within_days":30,"category_in":["toys"]}`), ids(5)},
		{"cart exists books/electronics", cond(`{"type":"event","event":"cart_add","op":"exists","within_days":30,"category_in":["books","electronics"]}`), ids(1, 3, 11, 18, 22, 30)},
		{"purchase exists 30", cond(`{"type":"event","event":"purchase","op":"exists","within_days":30}`), ids(2, 3, 4, 5, 9, 10, 13, 14, 18, 19, 24, 26, 27, 30)},
		{"purchase exists 30 toys", cond(`{"type":"event","event":"purchase","op":"exists","within_days":30,"category_in":["toys"]}`), ids(4, 5)},
		{"purchase count_gte 2 in 30", cond(`{"type":"event","event":"purchase","op":"count_gte","count":2,"within_days":30}`), ids(13)},
		{"purchase count_gte 2 in 90", cond(`{"type":"event","event":"purchase","op":"count_gte","count":2,"within_days":90}`), ids(10, 13, 26)},
		{"purchase count_gte 2 in 90, any category: NULL category never matches a filter",
			cond(`{"type":"event","event":"purchase","op":"count_gte","count":2,"within_days":90,"category_in":["beauty","books","electronics","fashion","grocery","home","sports","toys"]}`),
			ids(13, 26)},

		// ---- abandoned cart ----
		{"abandoned 30", cond(abandoned30), abandonedIn30},
		{"abandoned 60", cond(`{"type":"behavior","predicate":"abandoned_cart","within_days":60}`), abandonedIn60},
		{"abandoned 90", cond(`{"type":"behavior","predicate":"abandoned_cart","within_days":90}`), union(abandonedIn60, ids(21))},

		// ---- composition ----
		{"example brief", `{"match":"all","conditions":[` + cityIstanbul + `,` + abandoned30 + `]}`, ids(1, 3, 5, 8)},
		{"any", `{"match":"any","conditions":[{"type":"attribute","field":"city","op":"eq","value":"ankara"},{"type":"event","event":"cart_add","op":"count_gte","count":3,"within_days":30}]}`,
			ids(8, 11, 12, 13, 14, 15)},
		{"all with groups",
			`{"match":"all","conditions":[{"type":"attribute","field":"country","op":"eq","value":"TR"}],"groups":[` +
				`{"match":"any","conditions":[{"type":"attribute","field":"city","op":"eq","value":"izmir"},{"type":"attribute","field":"city","op":"eq","value":"eskisehir"}]},` +
				`{"match":"any","conditions":[{"type":"attribute","field":"gender","op":"eq","value":"female"},{"type":"attribute","field":"language","op":"eq","value":"en"}]}]}`,
			ids(16, 18, 29)},
		{"any with an all-group",
			`{"match":"any","conditions":[{"type":"attribute","field":"language","op":"eq","value":"en"}],"groups":[` +
				`{"match":"all","conditions":[{"type":"attribute","field":"city","op":"eq","value":"ankara"},{"type":"attribute","field":"gender","op":"eq","value":"male"}]}]}`,
			ids(3, 7, 10, 12, 14, 17, 22, 25, 30)},
		{"groups only", `{"match":"all","conditions":[],"groups":[{"match":"any","conditions":[{"type":"attribute","field":"city","op":"eq","value":"IĞDIR"}]}]}`, ids(24, 25)},
		{"any over NULL-able attributes",
			`{"match":"any","conditions":[{"type":"attribute","field":"age","op":"lte","value":20},{"type":"attribute","field":"last_active_days_ago","op":"lte","value":0}]}`,
			ids(5, 7, 14, 20)},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := matchedIDs(t, pool, parseRule(t, tc.rule))
			if !reflect.DeepEqual(got, tc.want) && !(len(got) == 0 && len(tc.want) == 0) {
				t.Errorf("matched %v\n          want %v", got, tc.want)
			}
		})
	}
}

// count_lte 0 ≡ not_exists and count_gte 1 ≡ exists for identical filters,
// and count_gte 0 matches everyone, across events, windows and categories.
func TestSegmentFixtureEventEquivalences(t *testing.T) {
	pool := testsupport.Pool(t, pg.NewMigratedDatabase(t))
	loadFixture(t, pool)
	assertEventEquivalences(t, pool, 30)
}

func assertEventEquivalences(t *testing.T, pool *pgxpool.Pool, totalUsers int) {
	t.Helper()
	for _, event := range []string{"cart_add", "purchase"} {
		for _, window := range []int{1, 7, 30, 90, 365} {
			for _, cats := range []string{"", `,"category_in":["toys"]`, `,"category_in":["books","electronics","home"]`} {
				e := func(op string) []int64 {
					return matchedIDs(t, pool, parseRule(t, cond(fmt.Sprintf(`{"type":"event","event":%q,%s"within_days":%d%s}`, event, op, window, cats))))
				}
				name := fmt.Sprintf("%s/%d%s", event, window, cats)
				exists, notExists := e(`"op":"exists",`), e(`"op":"not_exists",`)
				if got := e(`"op":"count_gte","count":1,`); !slices.Equal(got, exists) {
					t.Errorf("%s: count_gte 1 (%d users) ≠ exists (%d users)", name, len(got), len(exists))
				}
				if got := e(`"op":"count_lte","count":0,`); !slices.Equal(got, notExists) {
					t.Errorf("%s: count_lte 0 (%d users) ≠ not_exists (%d users)", name, len(got), len(notExists))
				}
				if got := e(`"op":"count_gte","count":0,`); len(got) != totalUsers {
					t.Errorf("%s: count_gte 0 matched %d users, want %d", name, len(got), totalUsers)
				}
				if len(exists)+len(notExists) != totalUsers || len(union(exists, notExists)) != totalUsers {
					t.Errorf("%s: exists (%d) and not_exists (%d) do not partition %d users", name, len(exists), len(notExists), totalUsers)
				}
			}
		}
	}
}

func TestAudienceStatsFixture(t *testing.T) {
	pool := testsupport.Pool(t, pg.NewMigratedDatabase(t))
	loadFixture(t, pool)
	repo := segment.NewAudienceRepository(pool)

	sms := segment.ChannelSMS
	email := segment.ChannelEmail
	example := `{"match":"all","conditions":[` + cityIstanbul + `,` + abandoned30 + `]}`
	everyone := cond(`{"type":"event","event":"cart_add","op":"count_gte","count":0,"within_days":30}`)

	// Example rule → users 1, 3, 5, 8:
	//   1: sms CI, email CI   3: sms CO, email CI   5: sms -I, email CI   8: sms CI, email C
	exSMS := segment.ChannelStats{Eligible: 2, NoConsent: 1, NoContact: 1}
	exEmail := segment.ChannelStats{Eligible: 3, NoConsent: 1, NoContact: 0}
	// Everyone (30 users), see the channel codes in fixtureUsers:
	//   sms eligible   {1,2,6,8,10,11,14,16,18,21,23,24,26,28,30} = 15
	//   sms no_contact {5,7,12,17,22,29} = 6 (5 and 22 are opted in without a phone)
	//   sms no_consent {3,4,9,13,15,19,20,25,27} = 9
	//   email eligible   {1,3,5,10,11,12,16,19,21,24,26,29,30} = 13
	//   email no_contact {6,7,14,15,17,22} = 6 (6, 15 and 22 are opted in without an email)
	//   email no_consent {2,4,8,9,13,18,20,23,25,27,28} = 11
	//   both {1,10,11,16,21,24,26,30} = 8; sms_only {2,6,8,14,18,23,28} = 7;
	//   email_only {3,5,12,19,29} = 5; unreachable {4,7,9,13,15,17,20,22,25,27} = 10
	allSMS := segment.ChannelStats{Eligible: 15, NoConsent: 9, NoContact: 6}
	allEmail := segment.ChannelStats{Eligible: 13, NoConsent: 11, NoContact: 6}

	tests := []struct {
		name     string
		rule     string
		channels []segment.Channel
		matched  int64
		by       map[segment.Channel]segment.ChannelStats
		reach    segment.ReachStats
		messages int64
	}{
		{"example, both channels", example, []segment.Channel{sms, email}, 4,
			map[segment.Channel]segment.ChannelStats{sms: exSMS, email: exEmail},
			segment.ReachStats{Both: 1, SMSOnly: 1, EmailOnly: 2, Unreachable: 0}, 5},
		{"example, sms only", example, []segment.Channel{sms}, 4,
			map[segment.Channel]segment.ChannelStats{sms: exSMS},
			segment.ReachStats{SMSOnly: 2, Unreachable: 2}, 2},
		{"example, email only (selection order and duplicates ignored)", example, []segment.Channel{email, email}, 4,
			map[segment.Channel]segment.ChannelStats{email: exEmail},
			segment.ReachStats{EmailOnly: 3, Unreachable: 1}, 3},
		{"example, no channels", example, nil, 4,
			map[segment.Channel]segment.ChannelStats{},
			segment.ReachStats{Unreachable: 4}, 0},
		{"everyone, both channels", everyone, []segment.Channel{email, sms}, 30,
			map[segment.Channel]segment.ChannelStats{sms: allSMS, email: allEmail},
			segment.ReachStats{Both: 8, SMSOnly: 7, EmailOnly: 5, Unreachable: 10}, 28},
		{"everyone, sms only", everyone, []segment.Channel{sms}, 30,
			map[segment.Channel]segment.ChannelStats{sms: allSMS},
			segment.ReachStats{SMSOnly: 15, Unreachable: 15}, 15},
		{"everyone, email only", everyone, []segment.Channel{email}, 30,
			map[segment.Channel]segment.ChannelStats{email: allEmail},
			segment.ReachStats{EmailOnly: 13, Unreachable: 17}, 13},
		// Users with several carts and purchases (5, 8, 18, 24) are counted once.
		{"multi-event users, both channels", cond(`{"type":"event","event":"cart_add","op":"count_gte","count":2,"within_days":30}`),
			[]segment.Channel{sms, email}, 4,
			map[segment.Channel]segment.ChannelStats{
				sms:   {Eligible: 3, NoConsent: 0, NoContact: 1}, // 8, 18, 24 eligible; 5 no phone
				email: {Eligible: 2, NoConsent: 2, NoContact: 0}, // 5, 24 eligible; 8 no row, 18 opted out
			},
			segment.ReachStats{Both: 1, SMSOnly: 2, EmailOnly: 1, Unreachable: 0}, 5},
		{"nobody", cond(`{"type":"attribute","field":"city","op":"eq","value":"Van"}`), []segment.Channel{sms, email}, 0,
			map[segment.Channel]segment.ChannelStats{sms: {}, email: {}}, segment.ReachStats{}, 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s, err := repo.Stats(context.Background(), parseRule(t, tc.rule), tc.channels)
			if err != nil {
				t.Fatal(err)
			}
			if s.MatchedUsers != tc.matched || !reflect.DeepEqual(s.ByChannel, tc.by) || s.ReachUsers != tc.reach || s.Messages != tc.messages {
				t.Errorf("got  matched=%d by=%v reach=%+v messages=%d\nwant matched=%d by=%v reach=%+v messages=%d",
					s.MatchedUsers, s.ByChannel, s.ReachUsers, s.Messages, tc.matched, tc.by, tc.reach, tc.messages)
			}
			if s.ComputedAt.IsZero() || s.ComputedAt.Location().String() != "UTC" {
				t.Errorf("computed_at = %v", s.ComputedAt)
			}
			if err := s.Check(); err != nil {
				t.Error(err)
			}
		})
	}
}

func TestAudienceStatsErrors(t *testing.T) {
	pool := testsupport.Pool(t, pg.NewMigratedDatabase(t))
	loadFixture(t, pool)
	repo := segment.NewAudienceRepository(pool)
	rule := parseRule(t, cond(abandoned30))

	if _, err := repo.Stats(context.Background(), segment.Validated{}, []segment.Channel{segment.ChannelSMS}); !errors.Is(err, segment.ErrNotValidated) {
		t.Errorf("zero rule: err = %v, want ErrNotValidated", err)
	}
	if _, err := repo.Stats(context.Background(), rule, []segment.Channel{"push"}); !errors.Is(err, segment.ErrUnknownChannel) {
		t.Errorf("unknown channel: err = %v, want ErrUnknownChannel", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := repo.Stats(ctx, rule, []segment.Channel{segment.ChannelSMS}); !errors.Is(err, context.Canceled) {
		t.Errorf("cancelled context: err = %v, want context.Canceled", err)
	}
	// The pool is still usable afterwards (no leaked transaction).
	if _, err := repo.Stats(context.Background(), rule, []segment.Channel{segment.ChannelSMS}); err != nil {
		t.Errorf("after cancellation: %v", err)
	}
}

// Stats runs under the transaction-local 5 s statement timeout: while another
// transaction holds an exclusive lock on users, the audience query is
// cancelled by the server (SQLSTATE 57014) after ~5 s instead of waiting.
// The wait is the timeout itself, not a sleep.
func TestAudienceStatsStatementTimeout(t *testing.T) {
	pool := testsupport.Pool(t, pg.NewMigratedDatabase(t))
	loadFixture(t, pool)
	repo := segment.NewAudienceRepository(pool)
	rule := parseRule(t, cond(abandoned30))
	ctx := context.Background()

	locker, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer locker.Rollback(ctx) //nolint:errcheck
	if _, err := locker.Exec(ctx, "LOCK TABLE users IN ACCESS EXCLUSIVE MODE"); err != nil {
		t.Fatal(err)
	}

	start := time.Now()
	_, err = repo.Stats(ctx, rule, []segment.Channel{segment.ChannelSMS})
	elapsed := time.Since(start)
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "57014" {
		t.Fatalf("err = %v, want query_canceled (57014) from statement_timeout", err)
	}
	if elapsed < segment.AudienceStatementTimeout-500*time.Millisecond || elapsed > 30*time.Second {
		t.Errorf("cancelled after %v, want ≈ %v", elapsed, segment.AudienceStatementTimeout)
	}
	if err := locker.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	// The setting was transaction-local: a plain session statement has none.
	var timeout string
	if err := pool.QueryRow(ctx, "SELECT current_setting('statement_timeout')").Scan(&timeout); err != nil {
		t.Fatal(err)
	}
	if timeout != "0" {
		t.Errorf("session statement_timeout = %s, want 0 (SET LOCAL must not leak)", timeout)
	}
	if _, err := repo.Stats(ctx, rule, []segment.Channel{segment.ChannelSMS}); err != nil {
		t.Errorf("after unlock: %v", err)
	}
}
