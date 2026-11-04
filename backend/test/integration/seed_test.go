//go:build integration

package integration

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/fyusufayan/insider-campaign-composer-case/backend/test/integration/testsupport"
)

// defaultDatasetHash is the hash of the default inputs (5000 users, seed 42,
// 2026-11-01T00:00:00Z); it equals the golden hash in cmd/seed's unit tests.
const defaultDatasetHash = "c553fe3638a6664a1c0de01035237a64d0dc4de72cde58c771cf9ed64fe2da25"

var (
	seedBuild struct {
		once sync.Once
		dir  string
		path string
		err  error
	}
)

// seedBinary builds cmd/seed once per test run.
func seedBinary(t *testing.T) string {
	t.Helper()
	seedBuild.once.Do(func() {
		_, file, _, _ := runtime.Caller(0)
		backend := filepath.Join(filepath.Dir(file), "..", "..")
		seedBuild.dir, seedBuild.err = os.MkdirTemp("", "composer-seed-")
		if seedBuild.err != nil {
			return
		}
		seedBuild.path = filepath.Join(seedBuild.dir, "seed")
		cmd := exec.Command("go", "build", "-o", seedBuild.path, "./cmd/seed")
		cmd.Dir = backend
		if out, err := cmd.CombinedOutput(); err != nil {
			seedBuild.err = errors.New("go build ./cmd/seed: " + err.Error() + "\n" + string(out))
		}
	})
	if seedBuild.err != nil {
		t.Fatal(seedBuild.err)
	}
	return seedBuild.path
}

func cleanupSeedBinary() {
	if seedBuild.dir != "" {
		_ = os.RemoveAll(seedBuild.dir)
	}
}

type seedRun struct {
	err  error
	logs []map[string]any
	out  string
}

// message returns the first log record with the given msg, or nil.
func (r seedRun) message(msg string) map[string]any {
	for _, l := range r.logs {
		if l["msg"] == msg {
			return l
		}
	}
	return nil
}

// runSeed runs the seed binary with an explicit environment (nothing leaks
// from the developer's shell).
func runSeed(t *testing.T, dbURL string, env map[string]string, args ...string) seedRun {
	t.Helper()
	return <-startSeed(t, dbURL, env, args...)
}

// startSeed starts the seed binary in the background; the result arrives on
// the returned channel when the process exits.
func startSeed(t *testing.T, dbURL string, env map[string]string, args ...string) <-chan seedRun {
	t.Helper()
	cmd := exec.Command(seedBinary(t), args...)
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + os.Getenv("HOME"), "DATABASE_URL=" + dbURL}
	for k, v := range env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	ch := make(chan seedRun, 1)
	if err := cmd.Start(); err != nil {
		ch <- seedRun{err: err}
		return ch
	}
	go func() {
		err := cmd.Wait()
		r := seedRun{err: err, out: out.String()}
		sc := bufio.NewScanner(strings.NewReader(r.out))
		for sc.Scan() {
			var rec map[string]any
			if json.Unmarshal(sc.Bytes(), &rec) == nil {
				r.logs = append(r.logs, rec)
			}
		}
		ch <- r
	}()
	return ch
}

func mustSeed(t *testing.T, dbURL string, env map[string]string, args ...string) seedRun {
	t.Helper()
	r := runSeed(t, dbURL, env, args...)
	if r.err != nil {
		t.Fatalf("seed %v failed: %v\n%s", args, r.err, r.out)
	}
	return r
}

func loggedHash(t *testing.T, r seedRun, msg string) string {
	t.Helper()
	rec := r.message(msg)
	if rec == nil {
		t.Fatalf("no %q log record in:\n%s", msg, r.out)
	}
	h, _ := rec["dataset_hash"].(string)
	if len(h) != 64 {
		t.Fatalf("%q record has dataset_hash %v", msg, rec["dataset_hash"])
	}
	return h
}

// Canonical serialization rebuilt in SQL, independently of the Go serializer
// (format documented in cmd/seed/generate.go).
var canonicalQueries = []struct{ header, query string }{
	{"#users", `SELECT concat_ws(E'\t', id::text, first_name, last_name, coalesce(email, '\N'), coalesce(phone, '\N'),
	       city, city_normalized, country, gender, coalesce(to_char(birth_date, 'YYYY-MM-DD'), '\N'), language,
	       to_char(signup_at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS.US"Z"'),
	       coalesce(to_char(last_active_at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS.US"Z"'), '\N'))
	  FROM users ORDER BY id`},
	{"#channel_consents", `SELECT concat_ws(E'\t', user_id::text, channel::text, status, source,
	       to_char(updated_at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS.US"Z"'))
	  FROM channel_consents ORDER BY user_id, channel`},
	{"#cart_events", `SELECT concat_ws(E'\t', id::text, user_id::text, product_id, category, quantity::text, unit_price::text,
	       to_char(occurred_at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS.US"Z"'))
	  FROM cart_events ORDER BY id`},
	{"#purchases", `SELECT concat_ws(E'\t', id::text, user_id::text, order_total::text, coalesce(category, '\N'),
	       to_char(purchased_at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS.US"Z"'))
	  FROM purchases ORDER BY id`},
}

func databaseHash(t *testing.T, pool *pgxpool.Pool) string {
	t.Helper()
	ctx := context.Background()
	h := sha256.New()
	for _, q := range canonicalQueries {
		h.Write([]byte(q.header + "\n"))
		rows, err := pool.Query(ctx, q.query)
		if err != nil {
			t.Fatalf("%s: %v", q.header, err)
		}
		for rows.Next() {
			var line string
			if err := rows.Scan(&line); err != nil {
				t.Fatal(err)
			}
			h.Write([]byte(line + "\n"))
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
	}
	return hex.EncodeToString(h.Sum(nil))
}

func count(t *testing.T, pool *pgxpool.Pool, table string) int64 {
	t.Helper()
	var n int64
	// table names come from test constants only
	if err := pool.QueryRow(context.Background(), "SELECT count(*) FROM "+table).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func sequenceLastValue(t *testing.T, pool *pgxpool.Pool, seq string) int64 {
	t.Helper()
	var n int64
	if err := pool.QueryRow(context.Background(), "SELECT last_value FROM "+seq).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestSeedLoadsCanonicalDataset(t *testing.T) {
	dbURL := pg.NewMigratedDatabase(t)
	pool := testsupport.Pool(t, dbURL)

	r := mustSeed(t, dbURL, nil)
	inputs := r.message("seed inputs")
	if inputs == nil || inputs["seed_users"] != float64(5000) || inputs["seed_random_seed"] != float64(42) ||
		inputs["seed_reference_time"] != "2026-11-01T00:00:00Z" || inputs["reset"] != false {
		t.Errorf("seed inputs log = %v", inputs)
	}
	generated := loggedHash(t, r, "dataset generated")
	loaded := loggedHash(t, r, "seed loaded")
	if generated != defaultDatasetHash || loaded != defaultDatasetHash {
		t.Fatalf("logged hashes generated=%s loaded=%s, want %s", generated, loaded, defaultDatasetHash)
	}
	if got := databaseHash(t, pool); got != defaultDatasetHash {
		t.Fatalf("database hash = %s, want generator hash %s", got, defaultDatasetHash)
	}

	if n := count(t, pool, "users"); n != 5000 {
		t.Errorf("users = %d, want 5000", n)
	}
	// IDs come from freshly restarted sequences: 1..N, and the sequence continues after N.
	for table, seq := range map[string]string{"users": "users_id_seq", "cart_events": "cart_events_id_seq", "purchases": "purchases_id_seq"} {
		if got, want := sequenceLastValue(t, pool, seq), count(t, pool, table); got != want {
			t.Errorf("%s last_value = %d, want %d", seq, got, want)
		}
	}
	// Mixed source spellings share one key and no generated timestamp is after the reference time.
	var spellings int
	var future bool
	err := pool.QueryRow(context.Background(), `
		SELECT (SELECT count(DISTINCT city) FROM users WHERE city_normalized = 'istanbul'),
		       EXISTS (SELECT 1 FROM users WHERE signup_at > '2026-11-01T00:00:00Z' OR last_active_at > '2026-11-01T00:00:00Z')
		    OR EXISTS (SELECT 1 FROM cart_events WHERE occurred_at > '2026-11-01T00:00:00Z')
		    OR EXISTS (SELECT 1 FROM purchases WHERE purchased_at > '2026-11-01T00:00:00Z')
		    OR EXISTS (SELECT 1 FROM channel_consents WHERE updated_at > '2026-11-01T00:00:00Z')`).Scan(&spellings, &future)
	if err != nil {
		t.Fatal(err)
	}
	if spellings != 4 || future {
		t.Errorf("istanbul spellings = %d (want 4), timestamps after reference = %v", spellings, future)
	}
}

func TestSeedSecondRunDoesNothing(t *testing.T) {
	dbURL := pg.NewMigratedDatabase(t)
	pool := testsupport.Pool(t, dbURL)
	env := map[string]string{"SEED_USERS": "200", "SEED_RANDOM_SEED": "7"}

	first := loggedHash(t, mustSeed(t, dbURL, env), "seed loaded")
	before := databaseHash(t, pool)
	if before != first {
		t.Fatalf("database hash %s != loaded %s", before, first)
	}

	// Different inputs, no -reset: must not touch the existing data.
	r := mustSeed(t, dbURL, map[string]string{"SEED_USERS": "300", "SEED_RANDOM_SEED": "8"})
	if r.message("seed loaded") != nil {
		t.Fatalf("second plain run loaded data:\n%s", r.out)
	}
	if r.message("users table already populated; nothing to do (run with -reset to replace the customer data)") == nil {
		t.Errorf("second run did not report that it did nothing:\n%s", r.out)
	}
	if after := databaseHash(t, pool); after != before {
		t.Errorf("database changed by a plain second run: %s -> %s", before, after)
	}
	if got := sequenceLastValue(t, pool, "users_id_seq"); got != 200 {
		t.Errorf("users_id_seq last_value = %d, want 200", got)
	}
}

const (
	refusedCampaigns  = "refusing to reset: campaigns contains rows"
	refusedDeliveries = "refusing to reset: campaign_deliveries contains rows"
	insertCampaignSQL = `INSERT INTO campaigns (name, brief, status) VALUES ('c', 'a campaign brief', 'draft')`
	resetEnvUsers     = "50"
)

// resetEnv differs from the initial seed in every input, so an accidental
// reset would always change the database hash.
var resetEnv = map[string]string{"SEED_USERS": resetEnvUsers, "SEED_RANDOM_SEED": "9", "SEED_REFERENCE_TIME": "2026-12-15T00:00:00Z"}

// dbState is everything a refused reset must leave untouched.
type dbState struct {
	hash                                     string
	userSeq, cartSeq, purchaseSeq            int64
	campaigns, contents, approvals, delivers int64
}

func captureState(t *testing.T, pool *pgxpool.Pool) dbState {
	t.Helper()
	return dbState{
		hash:        databaseHash(t, pool),
		userSeq:     sequenceLastValue(t, pool, "users_id_seq"),
		cartSeq:     sequenceLastValue(t, pool, "cart_events_id_seq"),
		purchaseSeq: sequenceLastValue(t, pool, "purchases_id_seq"),
		campaigns:   count(t, pool, "campaigns"),
		contents:    count(t, pool, "campaign_contents"),
		approvals:   count(t, pool, "campaign_approvals"),
		delivers:    count(t, pool, "campaign_deliveries"),
	}
}

// assertRefused checks that a reset run failed with wantMsg and changed nothing.
func assertRefused(t *testing.T, pool *pgxpool.Pool, r seedRun, wantMsg string, before dbState) {
	t.Helper()
	var exitErr *exec.ExitError
	if !errors.As(r.err, &exitErr) || exitErr.ExitCode() != 1 {
		t.Fatalf("reset: err = %v, want exit status 1\n%s", r.err, r.out)
	}
	failed := r.message("seed failed")
	if msg, _ := failed["err"].(string); !strings.Contains(msg, wantMsg) {
		t.Errorf("failure log = %v, want %q\n%s", failed, wantMsg, r.out)
	}
	if r.message("seed loaded") != nil {
		t.Error("refused reset still logged a load")
	}
	if after := captureState(t, pool); after != before {
		t.Errorf("refused reset changed the database:\n before %+v\n  after %+v", before, after)
	}
}

// insertDeliveryFixture inserts a campaign with content, a pending approval and
// one held delivery row (approval.Prepare does not exist yet in CP2; the rows
// satisfy every schema constraint).
func insertDeliveryFixture(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	_, err := pool.Exec(context.Background(), `
		WITH c AS (
		  INSERT INTO campaigns (name, brief, status) VALUES ('c', 'a campaign brief', 'pending_approval') RETURNING id
		), ct AS (
		  INSERT INTO campaign_contents (campaign_id, version, sms_body, origin)
		  SELECT id, 1, 'Merhaba', 'edited' FROM c RETURNING id, campaign_id
		), a AS (
		  INSERT INTO campaign_approvals (campaign_id, status, segment_version, segment_rule, content_id, channels,
		                                  audience_stats, snapshot_count, snapshot_digest, prepared_at, expires_at)
		  SELECT campaign_id, 'pending', 1, '{}', id, '{sms}', '{}', 1, 'digest',
		         clock_timestamp(), clock_timestamp() + interval '15 minutes' FROM ct
		  RETURNING id, campaign_id
		)
		INSERT INTO campaign_deliveries (campaign_id, approval_id, user_id, channel, vars, idempotency_key)
		SELECT campaign_id, id, 1, 'sms', '{}', campaign_id::text || ':1:sms' FROM a`)
	if err != nil {
		t.Fatalf("insert delivery fixture: %v", err)
	}
}

// waitForLockWait polls pg_stat_activity until a session of this database is
// waiting on a heavyweight lock while running a statement starting with
// queryPrefix. Bounded by a deadline; no fixed sleeps.
func waitForLockWait(t *testing.T, pool *pgxpool.Pool, queryPrefix string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for {
		var waiting bool
		err := pool.QueryRow(ctx, `
			SELECT EXISTS (SELECT 1 FROM pg_stat_activity
			                WHERE datname = current_database() AND wait_event_type = 'Lock'
			                  AND starts_with(ltrim(query), $1))`, queryPrefix).Scan(&waiting)
		if err == nil && waiting {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatalf("no session blocked on a lock while running %q (last error: %v)", queryPrefix, err)
		case <-tick.C:
		}
	}
}

// awaitSeed waits for a background seed run, bounded by a deadline.
func awaitSeed(t *testing.T, ch <-chan seedRun) seedRun {
	t.Helper()
	select {
	case r := <-ch:
		return r
	case <-time.After(2 * time.Minute):
		t.Fatal("seed did not finish")
		return seedRun{}
	}
}

// 1. A reset on a database without campaigns or deliveries succeeds and is reproducible.
func TestSeedResetReplacesCustomerData(t *testing.T) {
	dbURL := pg.NewMigratedDatabase(t)
	pool := testsupport.Pool(t, dbURL)
	mustSeed(t, dbURL, map[string]string{"SEED_USERS": "300", "SEED_RANDOM_SEED": "1"})

	env := map[string]string{"SEED_USERS": "120", "SEED_RANDOM_SEED": "2", "SEED_REFERENCE_TIME": "2026-12-15T00:00:00Z"}
	r := mustSeed(t, dbURL, env, "-reset")
	want := loggedHash(t, r, "seed loaded")
	if got := databaseHash(t, pool); got != want {
		t.Fatalf("database hash after reset = %s, want %s", got, want)
	}
	if n := count(t, pool, "users"); n != 120 {
		t.Errorf("users after reset = %d, want 120", n)
	}
	if got := sequenceLastValue(t, pool, "users_id_seq"); got != 120 {
		t.Errorf("users_id_seq last_value = %d, want 120 (sequence restarted)", got)
	}

	// Resetting again with the same inputs reproduces the identical dataset.
	again := loggedHash(t, mustSeed(t, dbURL, env, "-reset"), "seed loaded")
	if again != want || databaseHash(t, pool) != want {
		t.Errorf("reset with identical inputs produced %s, want %s", again, want)
	}
}

// 2 + 4. A campaign without deliveries blocks the reset; nothing changes.
func TestSeedResetRefusedWhenCampaignExists(t *testing.T) {
	dbURL := pg.NewMigratedDatabase(t)
	pool := testsupport.Pool(t, dbURL)
	mustSeed(t, dbURL, map[string]string{"SEED_USERS": "100"})
	if _, err := pool.Exec(context.Background(), insertCampaignSQL); err != nil {
		t.Fatal(err)
	}
	before := captureState(t, pool)

	r := runSeed(t, dbURL, resetEnv, "-reset")
	assertRefused(t, pool, r, refusedCampaigns, before)
}

// 3 + 4. Delivery rows block the reset; customer data, campaign data and sequences are preserved.
func TestSeedResetRefusedWhenDeliveriesExist(t *testing.T) {
	dbURL := pg.NewMigratedDatabase(t)
	pool := testsupport.Pool(t, dbURL)
	mustSeed(t, dbURL, map[string]string{"SEED_USERS": "100"})
	insertDeliveryFixture(t, pool)
	before := captureState(t, pool)

	r := runSeed(t, dbURL, resetEnv, "-reset")
	assertRefused(t, pool, r, refusedDeliveries, before)
	if before.delivers != 1 || before.approvals != 1 {
		t.Errorf("fixture state = %+v", before)
	}
}

// 5a. A campaign whose creation is still uncommitted when the reset starts: the
// reset waits on the campaigns lock and, once the creation commits, refuses.
func TestSeedResetWaitsForUncommittedCampaignThenRefuses(t *testing.T) {
	ctx := context.Background()
	dbURL := pg.NewMigratedDatabase(t)
	pool := testsupport.Pool(t, dbURL)
	mustSeed(t, dbURL, map[string]string{"SEED_USERS": "100"})
	before := captureState(t, pool)

	creator, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer creator.Rollback(ctx)
	if _, err := creator.Exec(ctx, insertCampaignSQL); err != nil {
		t.Fatal(err)
	}

	done := startSeed(t, dbURL, resetEnv, "-reset")
	waitForLockWait(t, pool, "LOCK TABLE campaigns")
	if err := creator.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	before.campaigns = 1 // the committed campaign is the only change allowed
	assertRefused(t, pool, awaitSeed(t, done), refusedCampaigns, before)
}

// 5b. Same race, but the campaign creation rolls back: the reset proceeds.
func TestSeedResetProceedsAfterUncommittedCampaignRollsBack(t *testing.T) {
	ctx := context.Background()
	dbURL := pg.NewMigratedDatabase(t)
	pool := testsupport.Pool(t, dbURL)
	mustSeed(t, dbURL, map[string]string{"SEED_USERS": "100"})

	creator, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer creator.Rollback(ctx) // releases the connection if the test fails early
	if _, err := creator.Exec(ctx, insertCampaignSQL); err != nil {
		t.Fatal(err)
	}

	done := startSeed(t, dbURL, resetEnv, "-reset")
	waitForLockWait(t, pool, "LOCK TABLE campaigns")
	if err := creator.Rollback(ctx); err != nil {
		t.Fatal(err)
	}

	r := awaitSeed(t, done)
	if r.err != nil {
		t.Fatalf("reset after rollback failed: %v\n%s", r.err, r.out)
	}
	if want := loggedHash(t, r, "seed loaded"); databaseHash(t, pool) != want {
		t.Errorf("database hash differs from logged %s", want)
	}
	if n := count(t, pool, "campaigns"); n != 0 {
		t.Errorf("campaigns = %d, want 0", n)
	}
}

// 5c. Campaign creation that starts while a reset holds the campaigns lock waits
// until the reset has committed, so it can never observe (or be referenced by)
// half-replaced customer data. The reset is paused deterministically after it
// has taken the campaigns lock by holding a conflicting lock on purchases,
// which it deletes next.
func TestCampaignCreationWaitsForRunningReset(t *testing.T) {
	ctx := context.Background()
	dbURL := pg.NewMigratedDatabase(t)
	pool := testsupport.Pool(t, dbURL)
	mustSeed(t, dbURL, map[string]string{"SEED_USERS": "100"})

	gate, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer gate.Rollback(ctx)
	if _, err := gate.Exec(ctx, `LOCK TABLE purchases IN SHARE MODE`); err != nil {
		t.Fatal(err)
	}

	seedDone := startSeed(t, dbURL, resetEnv, "-reset")
	waitForLockWait(t, pool, "DELETE FROM purchases") // reset now holds the campaigns lock

	created := make(chan error, 1)
	go func() {
		_, err := pool.Exec(ctx, insertCampaignSQL)
		created <- err
	}()
	waitForLockWait(t, pool, "INSERT INTO campaigns")
	select {
	case err := <-created:
		t.Fatalf("campaign insert finished while the reset held the lock: %v", err)
	default:
	}

	if err := gate.Rollback(ctx); err != nil { // let the reset continue
		t.Fatal(err)
	}
	r := awaitSeed(t, seedDone)
	if r.err != nil {
		t.Fatalf("reset failed: %v\n%s", r.err, r.out)
	}
	select {
	case err := <-created:
		if err != nil {
			t.Fatalf("campaign insert after reset: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("campaign insert did not complete after the reset committed")
	}
	if want := loggedHash(t, r, "seed loaded"); databaseHash(t, pool) != want {
		t.Errorf("database hash differs from logged %s", want)
	}
	if n := count(t, pool, "users"); n != 50 {
		t.Errorf("users = %d, want 50 (reset dataset)", n)
	}
	if n := count(t, pool, "campaigns"); n != 1 {
		t.Errorf("campaigns = %d, want 1", n)
	}
}

func TestSeedRequiresMigratedSchema(t *testing.T) {
	dbURL := pg.NewDatabase(t)
	r := runSeed(t, dbURL, map[string]string{"SEED_USERS": "10"})
	if r.err == nil {
		t.Fatalf("seed on an unmigrated database succeeded:\n%s", r.out)
	}
	if !strings.Contains(r.out, "is the database migrated?") {
		t.Errorf("error does not point at migrations:\n%s", r.out)
	}
}
