//go:build integration

package integration

import (
	"context"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/fyusufayan/insider-campaign-composer-case/backend/internal/segment"
	"github.com/fyusufayan/insider-campaign-composer-case/backend/test/integration/testsupport"
)

// TestSegmentWindowBoundaries places rows exactly on the window edges. Rows
// and queries share one transaction, so now() is the same instant for both
// and the edges are exact (ARCHITECTURE §6.4: t ≥ now() − N days).
func TestSegmentWindowBoundaries(t *testing.T) {
	pool := testsupport.Pool(t, pg.NewMigratedDatabase(t))
	ctx := context.Background()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	const today = "(now() AT TIME ZONE 'UTC')::date"
	setup := []string{
		// 101: cart exactly 30 days ago, never purchased → in the window, abandoned.
		// 102: cart 30 days + 1 µs ago → outside.
		// 103: cart exactly 30 days ago and a purchase at the same instant → recovered.
		// 104: purchase exactly 7 days ago → in a 7-day window; 105: 1 µs earlier → outside.
		// 106: signup/last activity exactly 10 days ago (10 whole days);
		// 107: 10 days − 1 µs ago (9 whole days).
		// 108: 30th birthday today (age 30); 109: 30th birthday tomorrow (age 29).
		`INSERT INTO users (id, first_name, last_name, city, city_normalized, gender, signup_at, last_active_at, birth_date) VALUES
		 (101, 'A', 'B', 'Ankara', 'ankara', 'female', now() - interval '400 days', NULL, NULL),
		 (102, 'A', 'B', 'Ankara', 'ankara', 'female', now() - interval '400 days', NULL, NULL),
		 (103, 'A', 'B', 'Ankara', 'ankara', 'female', now() - interval '400 days', NULL, NULL),
		 (104, 'A', 'B', 'Ankara', 'ankara', 'female', now() - interval '400 days', NULL, NULL),
		 (105, 'A', 'B', 'Ankara', 'ankara', 'female', now() - interval '400 days', NULL, NULL),
		 (106, 'A', 'B', 'Ankara', 'ankara', 'female', now() - interval '10 days', now() - interval '10 days', NULL),
		 (107, 'A', 'B', 'Ankara', 'ankara', 'female', now() - interval '10 days' + interval '1 microsecond',
		                                                now() - interval '10 days' + interval '1 microsecond', NULL),
		 (108, 'A', 'B', 'Ankara', 'ankara', 'female', now() - interval '400 days', NULL, (` + today + ` - interval '30 years')::date),
		 (109, 'A', 'B', 'Ankara', 'ankara', 'female', now() - interval '400 days', NULL, (` + today + ` - interval '30 years' + interval '1 day')::date)`,
		`INSERT INTO cart_events (user_id, product_id, category, quantity, unit_price, occurred_at) VALUES
		 (101, 'P', 'toys', 1, 1, now() - make_interval(days => 30)),
		 (102, 'P', 'toys', 1, 1, now() - make_interval(days => 30) - interval '1 microsecond'),
		 (103, 'P', 'toys', 1, 1, now() - make_interval(days => 30))`,
		`INSERT INTO purchases (user_id, order_total, category, purchased_at) VALUES
		 (103, 1, 'books', now() - make_interval(days => 30)),
		 (104, 1, 'toys',  now() - make_interval(days => 7)),
		 (105, 1, 'toys',  now() - make_interval(days => 7) - interval '1 microsecond')`,
	}
	for _, s := range setup {
		if _, err := tx.Exec(ctx, s); err != nil {
			t.Fatalf("%v\n%s", err, s)
		}
	}

	tests := []struct {
		name, rule string
		want       []int64
	}{
		{"cart exists 30 includes the edge", cond(`{"type":"event","event":"cart_add","op":"exists","within_days":30}`), ids(101, 103)},
		{"cart count_gte 1 includes the edge", cond(`{"type":"event","event":"cart_add","op":"count_gte","count":1,"within_days":30}`), ids(101, 103)},
		{"abandoned 30: edge cart counts, same-instant purchase recovers", cond(abandoned30), ids(101)},
		{"abandoned 31 reaches 102", cond(`{"type":"behavior","predicate":"abandoned_cart","within_days":31}`), ids(101, 102)},
		{"purchase exists 7 includes the edge", cond(`{"type":"event","event":"purchase","op":"exists","within_days":7}`), ids(104)},
		{"signup_days_ago gte 10", cond(`{"type":"attribute","field":"signup_days_ago","op":"gte","value":10}`), ids(101, 102, 103, 104, 105, 106, 108, 109)},
		{"signup_days_ago lte 9", cond(`{"type":"attribute","field":"signup_days_ago","op":"lte","value":9}`), ids(107)},
		{"signup_days_ago between 10..10", cond(`{"type":"attribute","field":"signup_days_ago","op":"between","value":{"min":10,"max":10}}`), ids(106)},
		{"last_active_days_ago gte 10", cond(`{"type":"attribute","field":"last_active_days_ago","op":"gte","value":10}`), ids(106)},
		{"last_active_days_ago lte 9", cond(`{"type":"attribute","field":"last_active_days_ago","op":"lte","value":9}`), ids(107)},
		{"age gte 30 (birthday today)", cond(`{"type":"attribute","field":"age","op":"gte","value":30}`), ids(108)},
		{"age lte 29", cond(`{"type":"attribute","field":"age","op":"lte","value":29}`), ids(109)},
		{"age between 30..30", cond(`{"type":"attribute","field":"age","op":"between","value":{"min":30,"max":30}}`), ids(108)},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := matchedIDs(t, tx, parseRule(t, tc.rule)); !slices.Equal(got, tc.want) {
				t.Errorf("matched %v, want %v", got, tc.want)
			}
		})
	}

	// ComputeAudienceStats runs inside the caller's transaction (it sees the
	// uncommitted rows). None of these users has contact data.
	s, err := segment.ComputeAudienceStats(ctx, tx, parseRule(t, cond(abandoned30)), []segment.Channel{segment.ChannelSMS, segment.ChannelEmail})
	if err != nil {
		t.Fatal(err)
	}
	if s.MatchedUsers != 1 || s.ByChannel[segment.ChannelSMS].NoContact != 1 || s.ReachUsers.Unreachable != 1 || s.Messages != 0 {
		t.Errorf("stats in caller transaction = %+v", s)
	}
}

// ---- deterministic 5,000-user seed ----

func seededPool(t *testing.T, env map[string]string) *pgxpool.Pool {
	t.Helper()
	dbURL := pg.NewMigratedDatabase(t)
	mustSeed(t, dbURL, env)
	return testsupport.Pool(t, dbURL)
}

func scalar(t *testing.T, db *pgxpool.Pool, sql string, args ...any) int64 {
	t.Helper()
	var n int64
	if err := db.QueryRow(context.Background(), sql, args...).Scan(&n); err != nil {
		t.Fatalf("%v\n%s", err, sql)
	}
	return n
}

var seedRules = []string{
	`{"match":"all","conditions":[` + cityIstanbul + `,` + abandoned30 + `]}`,
	cond(`{"type":"event","event":"cart_add","op":"count_gte","count":0,"within_days":30}`),
	cond(`{"type":"event","event":"purchase","op":"count_gte","count":2,"within_days":90}`),
	cond(`{"type":"event","event":"cart_add","op":"not_exists","within_days":14,"category_in":["electronics","toys"]}`),
	cond(`{"type":"attribute","field":"age","op":"between","value":{"min":18,"max":30}}`),
	cond(`{"type":"attribute","field":"last_active_days_ago","op":"lte","value":7}`),
	`{"match":"any","conditions":[{"type":"attribute","field":"city","op":"in","value":["ankara","izmir"]},` + abandoned30 + `],` +
		`"groups":[{"match":"all","conditions":[{"type":"attribute","field":"language","op":"eq","value":"en"},{"type":"attribute","field":"gender","op":"eq","value":"female"}]}]}`,
	cond(`{"type":"attribute","field":"city","op":"eq","value":"Van"}`),
}

// TestAudienceStatsSeedIdentities checks the partition identities over the
// default seed for every channel selection, and cross-checks matched users
// and the whole-population partition with independently written SQL.
func TestAudienceStatsSeedIdentities(t *testing.T) {
	pool := seededPool(t, nil)
	repo := segment.NewAudienceRepository(pool)
	ctx := context.Background()
	selections := [][]segment.Channel{
		{segment.ChannelSMS, segment.ChannelEmail}, {segment.ChannelSMS}, {segment.ChannelEmail}, {},
	}
	for i, js := range seedRules {
		rule := parseRule(t, js)
		matched := int64(len(matchedIDs(t, pool, rule)))
		var both segment.AudienceStats
		for _, sel := range selections {
			s, err := repo.Stats(ctx, rule, sel)
			if err != nil {
				t.Fatalf("rule %d %v: %v", i, sel, err)
			}
			if err := s.Check(); err != nil {
				t.Errorf("rule %d %v: %v", i, sel, err)
			}
			if s.MatchedUsers != matched {
				t.Errorf("rule %d %v: matched_users %d, matched ids %d", i, sel, s.MatchedUsers, matched)
			}
			if len(s.ByChannel) != len(sel) {
				t.Errorf("rule %d %v: by_channel has %d channels", i, sel, len(s.ByChannel))
			}
			if len(sel) == 2 {
				both = s
			}
		}
		// Single-channel selections agree with the per-channel partition of the
		// two-channel selection.
		smsOnly, _ := repo.Stats(ctx, rule, []segment.Channel{segment.ChannelSMS})
		if smsOnly.ByChannel[segment.ChannelSMS] != both.ByChannel[segment.ChannelSMS] ||
			smsOnly.ReachUsers.SMSOnly != both.ReachUsers.Both+both.ReachUsers.SMSOnly {
			t.Errorf("rule %d: sms-only selection disagrees with both-channel selection", i)
		}
		t.Logf("rule %d: matched=%d sms=%+v email=%+v reach=%+v messages=%d", i, both.MatchedUsers,
			both.ByChannel[segment.ChannelSMS], both.ByChannel[segment.ChannelEmail], both.ReachUsers, both.Messages)
	}

	// Whole population, against hand-written SQL that does not use the compiler.
	s, err := repo.Stats(ctx, parseRule(t, seedRules[1]), []segment.Channel{segment.ChannelSMS, segment.ChannelEmail})
	if err != nil {
		t.Fatal(err)
	}
	if n := scalar(t, pool, `SELECT count(*) FROM users`); s.MatchedUsers != n || n != 5000 {
		t.Errorf("count_gte 0 matched %d of %d users", s.MatchedUsers, n)
	}
	const eligible = `EXISTS (SELECT 1 FROM channel_consents c WHERE c.user_id = u.id AND c.channel = $1::channel AND c.status = 'opted_in')`
	for _, ch := range []segment.Channel{segment.ChannelSMS, segment.ChannelEmail} {
		col := map[segment.Channel]string{segment.ChannelSMS: "phone", segment.ChannelEmail: "email"}[ch]
		want := segment.ChannelStats{
			NoContact: scalar(t, pool, fmt.Sprintf(`SELECT count(*) FROM users u WHERE u.%s IS NULL`, col)),
			Eligible:  scalar(t, pool, fmt.Sprintf(`SELECT count(*) FROM users u WHERE u.%s IS NOT NULL AND `+eligible, col), string(ch)),
			NoConsent: scalar(t, pool, fmt.Sprintf(`SELECT count(*) FROM users u WHERE u.%s IS NOT NULL AND NOT `+eligible, col), string(ch)),
		}
		if got := s.ByChannel[ch]; got != want {
			t.Errorf("%s partition = %+v, independent SQL = %+v", ch, got, want)
		}
	}
	wantBoth := scalar(t, pool, `SELECT count(*) FROM users u WHERE u.phone IS NOT NULL AND u.email IS NOT NULL
		AND (SELECT count(*) FROM channel_consents c WHERE c.user_id = u.id AND c.status = 'opted_in') = 2`)
	if s.ReachUsers.Both != wantBoth {
		t.Errorf("both = %d, independent SQL = %d", s.ReachUsers.Both, wantBoth)
	}
	// The seed must exercise every partition cell, or the identities above are vacuous.
	for name, v := range map[string]int64{
		"sms no_contact": s.ByChannel[segment.ChannelSMS].NoContact, "sms no_consent": s.ByChannel[segment.ChannelSMS].NoConsent,
		"email no_contact": s.ByChannel[segment.ChannelEmail].NoContact, "email no_consent": s.ByChannel[segment.ChannelEmail].NoConsent,
		"both": s.ReachUsers.Both, "sms_only": s.ReachUsers.SMSOnly, "email_only": s.ReachUsers.EmailOnly, "unreachable": s.ReachUsers.Unreachable,
	} {
		if v == 0 {
			t.Errorf("seed has no users in %s", name)
		}
	}

	// abandoned_cart against an independent formulation: the latest cart in
	// the window is later than the latest purchase (or there is no purchase).
	abandonedIDs := matchedIDs(t, pool, parseRule(t, cond(abandoned30)))
	rows, err := pool.Query(ctx, `
		SELECT u.id FROM users u
		JOIN LATERAL (SELECT max(occurred_at) AS last_cart FROM cart_events WHERE user_id = u.id AND occurred_at >= now() - interval '30 days' AND occurred_at <= now()) c ON true
		JOIN LATERAL (SELECT max(purchased_at) AS last_purchase FROM purchases WHERE user_id = u.id AND purchased_at <= now()) p ON true
		WHERE c.last_cart IS NOT NULL AND (p.last_purchase IS NULL OR p.last_purchase < c.last_cart)
		ORDER BY u.id`)
	if err != nil {
		t.Fatal(err)
	}
	independent, err := pgx.CollectRows(rows, pgx.RowTo[int64])
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(abandonedIDs, independent) {
		t.Errorf("abandoned_cart(30): compiler %d users, independent SQL %d users", len(abandonedIDs), len(independent))
	}
	if len(abandonedIDs) == 0 || len(abandonedIDs) == 5000 {
		t.Errorf("abandoned_cart(30) matched %d users; the seed should make it selective", len(abandonedIDs))
	}

	// Event equivalences hold on the seed as well.
	assertEventEquivalences(t, pool, 5000)
}

// TestAudienceStatsSeedToday reproduces `make seed-today` (reference time =
// today's UTC midnight) and reports the example İstanbul abandoned-cart
// audience. The numbers depend on the day the test runs; they are logged,
// and the identities and the hand-written cross-check are asserted.
func TestAudienceStatsSeedToday(t *testing.T) {
	ref := time.Now().UTC().Truncate(24 * time.Hour).Format(time.RFC3339)
	pool := seededPool(t, map[string]string{"SEED_REFERENCE_TIME": ref})
	repo := segment.NewAudienceRepository(pool)
	rule := parseRule(t, seedRules[0])

	s, err := repo.Stats(context.Background(), rule, []segment.Channel{segment.ChannelSMS, segment.ChannelEmail})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("seed-today (reference %s) — İstanbul ∧ abandoned_cart(30): matched_users=%d sms=%+v email=%+v reach_users=%+v messages=%d",
		ref, s.MatchedUsers, s.ByChannel[segment.ChannelSMS], s.ByChannel[segment.ChannelEmail], s.ReachUsers, s.Messages)
	for _, sel := range [][]segment.Channel{{segment.ChannelSMS}, {segment.ChannelEmail}, {}} {
		x, err := repo.Stats(context.Background(), rule, sel)
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("  channels=%v: reach_users=%+v messages=%d", sel, x.ReachUsers, x.Messages)
	}

	istanbul := scalar(t, pool, `SELECT count(*) FROM users WHERE city_normalized = 'istanbul'`)
	want := scalar(t, pool, `
		SELECT count(*) FROM users u
		WHERE u.city_normalized = 'istanbul'
		  AND (SELECT max(occurred_at) FROM cart_events WHERE user_id = u.id AND occurred_at >= now() - interval '30 days' AND occurred_at <= now())
		      > coalesce((SELECT max(purchased_at) FROM purchases WHERE user_id = u.id AND purchased_at <= now()), '-infinity')`)
	t.Logf("  İstanbul users=%d", istanbul)
	if s.MatchedUsers != want {
		t.Errorf("matched_users = %d, independent SQL = %d", s.MatchedUsers, want)
	}
	if s.MatchedUsers == 0 {
		t.Error("example segment is empty on seed-today data")
	}
}
