//go:build integration

package integration

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/fyusufayan/insider-campaign-composer-case/backend/internal/platform/db"
	"github.com/fyusufayan/insider-campaign-composer-case/backend/test/integration/testsupport"
)

var schemaTables = []string{
	"campaign_approvals", "campaign_contents", "campaign_deliveries", "campaigns", "cart_events",
	"channel_consents", "channel_pauses", "delivery_attempts", "purchases", "users", "worker_heartbeats",
}

// latestMigration is the newest embedded migration (000002_channel_pauses, CP17).
const latestMigration = 2

func publicTables(t *testing.T, pool *pgxpool.Pool) []string {
	t.Helper()
	rows, err := pool.Query(context.Background(),
		`SELECT table_name::text FROM information_schema.tables
		  WHERE table_schema = 'public' AND table_name <> 'schema_migrations' ORDER BY 1`)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatal(err)
		}
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func channelTypeExists(t *testing.T, pool *pgxpool.Pool) bool {
	t.Helper()
	var ok bool
	if err := pool.QueryRow(context.Background(), `SELECT EXISTS (SELECT 1 FROM pg_type WHERE typname = 'channel')`).Scan(&ok); err != nil {
		t.Fatal(err)
	}
	return ok
}

func assertVersion(t *testing.T, dbURL string, want uint) {
	t.Helper()
	st, err := db.CurrentMigration(context.Background(), dbURL)
	if err != nil {
		t.Fatalf("CurrentMigration: %v", err)
	}
	if st.Version != want || st.Dirty {
		t.Fatalf("migration state = %+v, want version %d clean", st, want)
	}
}

func TestMigrateUpDownUp(t *testing.T) {
	ctx := context.Background()
	dbURL := pg.NewDatabase(t)
	pool := testsupport.Pool(t, dbURL)
	assertVersion(t, dbURL, 0)

	if err := db.MigrateUp(ctx, dbURL); err != nil {
		t.Fatalf("up: %v", err)
	}
	assertVersion(t, dbURL, latestMigration)
	if got := publicTables(t, pool); !slices.Equal(got, schemaTables) {
		t.Fatalf("tables after up = %v, want %v", got, schemaTables)
	}
	if !channelTypeExists(t, pool) {
		t.Fatal("channel enum missing after up")
	}
	if err := db.MigrateUp(ctx, dbURL); err != nil {
		t.Fatalf("second up (no change) = %v, want nil", err)
	}

	if err := db.MigrateDown(ctx, dbURL); err != nil {
		t.Fatalf("down: %v", err)
	}
	assertVersion(t, dbURL, 0)
	if got := publicTables(t, pool); len(got) != 0 {
		t.Fatalf("tables after down = %v, want none", got)
	}
	if channelTypeExists(t, pool) {
		t.Fatal("channel enum still present after down")
	}

	if err := db.MigrateUp(ctx, dbURL); err != nil {
		t.Fatalf("up again: %v", err)
	}
	assertVersion(t, dbURL, latestMigration)
	if got := publicTables(t, pool); !slices.Equal(got, schemaTables) {
		t.Fatalf("tables after up again = %v, want %v", got, schemaTables)
	}
}

// Indexes, foreign keys and column details that §4 relies on.
func TestSchemaStructure(t *testing.T) {
	ctx := context.Background()
	pool := testsupport.Pool(t, pg.NewMigratedDatabase(t))

	indexes := map[string]string{
		"approvals_one_live":                  "CREATE UNIQUE INDEX approvals_one_live ON public.campaign_approvals USING btree (campaign_id) WHERE (status = ANY (ARRAY['pending'::text, 'approved'::text]))",
		"approvals_one_consumed":              "CREATE UNIQUE INDEX approvals_one_consumed ON public.campaign_approvals USING btree (campaign_id) WHERE (status = 'consumed'::text)",
		"approvals_expiry":                    "CREATE INDEX approvals_expiry ON public.campaign_approvals USING btree (expires_at) WHERE (status = ANY (ARRAY['pending'::text, 'approved'::text]))",
		"deliveries_ready":                    "CREATE INDEX deliveries_ready ON public.campaign_deliveries USING btree (channel, next_attempt_at) WHERE (status = ANY (ARRAY['pending'::text, 'retry_scheduled'::text]))",
		"deliveries_lease":                    "CREATE INDEX deliveries_lease ON public.campaign_deliveries USING btree (lease_until) WHERE (status = 'in_flight'::text)",
		"deliveries_stats":                    "CREATE INDEX deliveries_stats ON public.campaign_deliveries USING btree (campaign_id, channel, status)",
		"users_city_normalized_idx":           "CREATE INDEX users_city_normalized_idx ON public.users USING btree (city_normalized)",
		"cart_events_user_id_occurred_at_idx": "CREATE INDEX cart_events_user_id_occurred_at_idx ON public.cart_events USING btree (user_id, occurred_at DESC)",
		"purchases_user_id_purchased_at_idx":  "CREATE INDEX purchases_user_id_purchased_at_idx ON public.purchases USING btree (user_id, purchased_at DESC)",
	}
	for name, want := range indexes {
		var got string
		if err := pool.QueryRow(ctx, `SELECT indexdef FROM pg_indexes WHERE indexname = $1`, name).Scan(&got); err != nil {
			t.Errorf("index %s: %v", name, err)
			continue
		}
		if got != want {
			t.Errorf("index %s:\n got %s\nwant %s", name, got, want)
		}
	}

	fks := map[string]string{ // constraint -> definition
		"campaigns_current_content_id_fkey":    "FOREIGN KEY (current_content_id) REFERENCES campaign_contents(id)",
		"campaigns_active_approval_id_fkey":    "FOREIGN KEY (active_approval_id) REFERENCES campaign_approvals(id)",
		"channel_consents_user_id_fkey":        "FOREIGN KEY (user_id) REFERENCES users(id) ON DELETE CASCADE",
		"cart_events_user_id_fkey":             "FOREIGN KEY (user_id) REFERENCES users(id) ON DELETE CASCADE",
		"purchases_user_id_fkey":               "FOREIGN KEY (user_id) REFERENCES users(id) ON DELETE CASCADE",
		"campaign_contents_campaign_id_fkey":   "FOREIGN KEY (campaign_id) REFERENCES campaigns(id) ON DELETE CASCADE",
		"campaign_approvals_campaign_id_fkey":  "FOREIGN KEY (campaign_id) REFERENCES campaigns(id)",
		"campaign_approvals_content_id_fkey":   "FOREIGN KEY (content_id) REFERENCES campaign_contents(id)",
		"campaign_deliveries_campaign_id_fkey": "FOREIGN KEY (campaign_id) REFERENCES campaigns(id)",
		"campaign_deliveries_approval_id_fkey": "FOREIGN KEY (approval_id) REFERENCES campaign_approvals(id)",
		"campaign_deliveries_user_id_fkey":     "FOREIGN KEY (user_id) REFERENCES users(id)",
		"delivery_attempts_delivery_id_fkey":   "FOREIGN KEY (delivery_id) REFERENCES campaign_deliveries(id)",
	}
	for name, want := range fks {
		var got string
		if err := pool.QueryRow(ctx, `SELECT pg_get_constraintdef(oid) FROM pg_constraint WHERE conname = $1`, name).Scan(&got); err != nil {
			t.Errorf("constraint %s: %v", name, err)
			continue
		}
		if got != want {
			t.Errorf("constraint %s = %q, want %q", name, got, want)
		}
	}

	type col struct{ table, column, typ, nullable, def string }
	cols := []col{
		{"cart_events", "unit_price", "numeric(12,2)", "NO", ""},
		{"purchases", "order_total", "numeric(12,2)", "NO", ""},
		{"purchases", "category", "text", "YES", ""},
		{"users", "country", "character(2)", "NO", "'TR'::bpchar"},
		{"users", "language", "text", "NO", "'tr'::text"},
		{"users", "birth_date", "date", "YES", ""},
		{"users", "last_active_at", "timestamp with time zone", "YES", ""},
		{"channel_consents", "updated_at", "timestamp with time zone", "NO", "now()"},
		{"campaigns", "id", "uuid", "NO", "gen_random_uuid()"},
		{"campaigns", "channels", "channel[]", "NO", "'{}'::channel[]"},
		{"campaigns", "segment_version", "integer", "NO", "0"},
		{"campaigns", "version", "integer", "NO", "1"},
		{"campaign_approvals", "prepared_at", "timestamp with time zone", "NO", ""},
		{"campaign_approvals", "expires_at", "timestamp with time zone", "NO", ""},
		{"campaign_deliveries", "status", "text", "NO", "'held'::text"},
		{"campaign_deliveries", "outcome_uncertain", "boolean", "NO", "false"},
		{"campaign_deliveries", "destination", "text", "YES", ""},
		{"campaign_deliveries", "next_attempt_at", "timestamp with time zone", "NO", "now()"},
	}
	for _, c := range cols {
		var typ, nullable string
		var def *string
		err := pool.QueryRow(ctx, `
			SELECT format_type(a.atttypid, a.atttypmod),
			       CASE WHEN a.attnotnull THEN 'NO' ELSE 'YES' END,
			       pg_get_expr(d.adbin, d.adrelid)
			  FROM pg_attribute a
			  LEFT JOIN pg_attrdef d ON d.adrelid = a.attrelid AND d.adnum = a.attnum
			 WHERE a.attrelid = $1::regclass AND a.attname = $2`, c.table, c.column).Scan(&typ, &nullable, &def)
		if err != nil {
			t.Errorf("%s.%s: %v", c.table, c.column, err)
			continue
		}
		gotDef := ""
		if def != nil {
			gotDef = *def
		}
		if typ != c.typ || nullable != c.nullable || gotDef != c.def {
			t.Errorf("%s.%s = (%s, nullable=%s, default=%q), want (%s, nullable=%s, default=%q)",
				c.table, c.column, typ, nullable, gotDef, c.typ, c.nullable, c.def)
		}
	}
}

// CHECK, UNIQUE and partial-index constraints reject invalid rows.
func TestSchemaConstraints(t *testing.T) {
	ctx := context.Background()
	pool := testsupport.Pool(t, pg.NewMigratedDatabase(t))

	exec := func(sql string, args ...any) error {
		_, err := pool.Exec(ctx, sql, args...)
		return err
	}
	must := func(sql string, args ...any) {
		t.Helper()
		if err := exec(sql, args...); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	wantCode := func(name, code, sql string, args ...any) {
		t.Helper()
		err := exec(sql, args...)
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != code {
			t.Errorf("%s: err = %v, want SQLSTATE %s", name, err, code)
		}
	}
	const (
		checkViolation  = "23514"
		uniqueViolation = "23505"
		fkViolation     = "23503"
		notNull         = "23502"
		numericOverflow = "22003"
		badEnum         = "22P02"
	)
	const insUser = `INSERT INTO users (first_name, last_name, email, phone, city, city_normalized, gender, signup_at)
	                 VALUES ($1, $2, $3, $4, 'İstanbul', 'istanbul', $5, now())`

	must(insUser, "Ayşe", "Yılmaz", "a@example.com", "+905321234567", "female")
	wantCode("first_name 21 runes", checkViolation, insUser, "ŞŞŞŞŞŞŞŞŞŞŞŞŞŞŞŞŞŞŞŞŞ", "X", nil, nil, "male")
	wantCode("first_name empty", checkViolation, insUser, "", "X", nil, nil, "male")
	wantCode("last_name 26 runes", checkViolation, insUser, "X", "ĞĞĞĞĞĞĞĞĞĞĞĞĞĞĞĞĞĞĞĞĞĞĞĞĞĞ", nil, nil, "male")
	must(insUser, "ŞŞŞŞŞŞŞŞŞŞŞŞŞŞŞŞŞŞŞŞ", "ĞĞĞĞĞĞĞĞĞĞĞĞĞĞĞĞĞĞĞĞĞĞĞĞĞ", nil, nil, "other") // exactly 20 / 25 runes
	wantCode("bad gender", checkViolation, insUser, "X", "Y", nil, nil, "robot")
	wantCode("duplicate email", uniqueViolation, insUser, "X", "Y", "a@example.com", nil, "male")
	wantCode("duplicate phone", uniqueViolation, insUser, "X", "Y", nil, "+905321234567", "male")
	must(insUser, "X", "Y", nil, nil, "unknown") // NULL contacts do not collide
	must(insUser, "Z", "Y", nil, nil, "unknown")

	wantCode("consent bad status", checkViolation, `INSERT INTO channel_consents (user_id, channel, status, source) VALUES (1, 'sms', 'maybe', 'x')`)
	wantCode("consent bad channel", badEnum, `INSERT INTO channel_consents (user_id, channel, status, source) VALUES (1, 'push', 'opted_in', 'x')`)
	wantCode("consent unknown user", fkViolation, `INSERT INTO channel_consents (user_id, channel, status, source) VALUES (999, 'sms', 'opted_in', 'x')`)
	must(`INSERT INTO channel_consents (user_id, channel, status, source) VALUES (1, 'sms', 'opted_in', 'x')`)
	wantCode("consent duplicate", uniqueViolation, `INSERT INTO channel_consents (user_id, channel, status, source) VALUES (1, 'sms', 'opted_out', 'x')`)

	const insCart = `INSERT INTO cart_events (user_id, product_id, category, quantity, unit_price, occurred_at) VALUES (1, 'p', 'c', $1, $2, now())`
	wantCode("cart quantity 0", checkViolation, insCart, 0, "1.00")
	wantCode("cart price overflow", numericOverflow, insCart, 1, "10000000000.00")
	must(insCart, 1, "9999999999.99")
	var rounded string
	if err := pool.QueryRow(ctx, `SELECT $1::numeric(12,2)::text`, "1.005").Scan(&rounded); err != nil || rounded != "1.01" {
		t.Errorf("numeric(12,2) rounding = %q, %v", rounded, err)
	}

	wantCode("campaign short brief", checkViolation, `INSERT INTO campaigns (name, brief, status) VALUES ('n', 'short', 'draft')`)
	wantCode("campaign bad status", checkViolation, `INSERT INTO campaigns (name, brief, status) VALUES ('n', 'long enough brief', 'paused')`)
	var campaignID string
	if err := pool.QueryRow(ctx, `INSERT INTO campaigns (name, brief, status) VALUES ('n', 'long enough brief', 'draft') RETURNING id`).Scan(&campaignID); err != nil {
		t.Fatal(err)
	}
	wantCode("content bad origin", checkViolation, `INSERT INTO campaign_contents (campaign_id, version, origin) VALUES ($1, 1, 'robot')`, campaignID)
	var contentID int64
	if err := pool.QueryRow(ctx, `INSERT INTO campaign_contents (campaign_id, version, sms_body, origin) VALUES ($1, 1, 'hi', 'edited') RETURNING id`, campaignID).Scan(&contentID); err != nil {
		t.Fatal(err)
	}
	wantCode("content duplicate version", uniqueViolation, `INSERT INTO campaign_contents (campaign_id, version, origin) VALUES ($1, 1, 'edited')`, campaignID)
	wantCode("campaign unknown content", fkViolation, `UPDATE campaigns SET current_content_id = 999 WHERE id = $1`, campaignID)
	must(`UPDATE campaigns SET current_content_id = $2 WHERE id = $1`, campaignID, contentID)

	const insApproval = `INSERT INTO campaign_approvals (campaign_id, status, revoke_reason, segment_version, segment_rule,
	      content_id, channels, audience_stats, snapshot_count, snapshot_digest, prepared_at, expires_at,
	      approved_at, approved_by, consumed_at)
	    VALUES ($1, $2, $3, 1, '{}', $4, $5::channel[], '{}', $6, 'digest', clock_timestamp(), clock_timestamp() + interval '15 minutes',
	      $7::timestamptz, $8, $9::timestamptz)`
	ins := func(status string, reason any, channels string, count int, approvedAt, approvedBy, consumedAt any) error {
		return exec(insApproval, campaignID, status, reason, contentID, channels, count, approvedAt, approvedBy, consumedAt)
	}
	check := func(name string, err error, code string) {
		t.Helper()
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != code {
			t.Errorf("%s: err = %v, want SQLSTATE %s", name, err, code)
		}
	}
	check("revoked without reason", ins("revoked", nil, "{sms}", 1, nil, nil, nil), checkViolation)
	check("pending with reason", ins("pending", "edited", "{sms}", 1, nil, nil, nil), checkViolation)
	check("bad reason", ins("revoked", "bored", "{sms}", 1, nil, nil, nil), checkViolation)
	check("no channels", ins("pending", nil, "{}", 1, nil, nil, nil), checkViolation)
	check("empty snapshot", ins("pending", nil, "{sms}", 0, nil, nil, nil), checkViolation)
	check("approved without approver", ins("approved", nil, "{sms}", 1, "2026-11-01T00:00:00Z", nil, nil), checkViolation)
	check("consumed without consumed_at", ins("consumed", nil, "{sms}", 1, "2026-11-01T00:00:00Z", "m", nil), checkViolation)
	if err := ins("pending", nil, "{sms,email}", 2, nil, nil, nil); err != nil {
		t.Fatalf("valid pending approval: %v", err)
	}
	check("second live approval", ins("approved", nil, "{sms}", 1, "2026-11-01T00:00:00Z", "m", nil), uniqueViolation)
	if err := ins("revoked", "superseded", "{sms}", 1, nil, nil, nil); err != nil {
		t.Errorf("revoked approval next to a live one: %v", err)
	}
	if err := ins("revoked", "withdrawn", "{sms}", 1, nil, nil, nil); err != nil {
		t.Errorf("second revoked approval: %v", err)
	}
	if err := ins("consumed", nil, "{sms}", 1, "2026-11-01T00:00:00Z", "m", "2026-11-01T00:01:00Z"); err != nil {
		t.Fatalf("consumed approval: %v", err)
	}
	check("second consumed approval", ins("consumed", nil, "{sms}", 1, "2026-11-01T00:00:00Z", "m", "2026-11-01T00:01:00Z"), uniqueViolation)
	wantCode("approval prepared_at required", notNull, `INSERT INTO campaign_approvals (campaign_id, status, segment_version, segment_rule, content_id,
	      channels, audience_stats, snapshot_count, snapshot_digest, expires_at)
	    VALUES ($1, 'expired', 1, '{}', $2, '{sms}', '{}', 1, 'd', now())`, campaignID, contentID)

	var approvalID string
	if err := pool.QueryRow(ctx, `SELECT id FROM campaign_approvals WHERE status = 'pending'`).Scan(&approvalID); err != nil {
		t.Fatal(err)
	}
	must(`UPDATE campaigns SET active_approval_id = $2 WHERE id = $1`, campaignID, approvalID)
	wantCode("campaign unknown approval", fkViolation, `UPDATE campaigns SET active_approval_id = gen_random_uuid() WHERE id = $1`, campaignID)

	const insDelivery = `INSERT INTO campaign_deliveries (campaign_id, approval_id, user_id, channel, vars, idempotency_key, status,
	      destination, provider_message_id, lease_until, leased_by)
	    VALUES ($1, $2, 1, $3, '{}', $4, $5, $6, $7, $8::timestamptz, $9)`
	del := func(ch, key, status string, dest, msgID, lease, leasedBy any) error {
		return exec(insDelivery, campaignID, approvalID, ch, key, status, dest, msgID, lease, leasedBy)
	}
	check("sent without destination", del("sms", "k1", "sent", nil, "m1", nil, nil), checkViolation)
	check("sent without message id", del("sms", "k1", "sent", "+905321234567", nil, nil, nil), checkViolation)
	check("in_flight without lease", del("sms", "k1", "in_flight", nil, nil, nil, "w"), checkViolation)
	check("bad delivery status", del("sms", "k1", "queued", nil, nil, nil, nil), checkViolation)
	if err := del("sms", "k1", "held", nil, nil, nil, nil); err != nil {
		t.Fatalf("held delivery: %v", err)
	}
	var status string
	if err := pool.QueryRow(ctx, `SELECT status FROM campaign_deliveries WHERE idempotency_key = 'k1'`).Scan(&status); err != nil || status != "held" {
		t.Errorf("default delivery status = %q, %v", status, err)
	}
	check("duplicate (campaign, user, channel)", del("sms", "k2", "held", nil, nil, nil, nil), uniqueViolation)
	check("duplicate idempotency key", del("email", "k1", "held", nil, nil, nil, nil), uniqueViolation)
	wantCode("delete referenced user", fkViolation, `DELETE FROM users WHERE id = 1`)

	const insAttempt = `INSERT INTO delivery_attempts (delivery_id, attempt_no, destination, outcome, started_at, finished_at)
	    SELECT id, $1, '+905321234567', $2, now(), now() FROM campaign_deliveries WHERE idempotency_key = 'k1'`
	wantCode("bad attempt outcome", checkViolation, insAttempt, 1, "lost")
	must(insAttempt, 1, "timeout")
	wantCode("duplicate attempt_no", uniqueViolation, insAttempt, 1, "accepted")
}
