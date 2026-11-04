//go:build integration

package integration

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/fyusufayan/insider-campaign-composer-case/backend/internal/segment"
	"github.com/fyusufayan/insider-campaign-composer-case/backend/test/integration/testsupport"
)

// TestSegmentFutureEvents: historical windows are [now() − N days, now()].
// Rows and queries share one transaction, so now() is a single database
// evaluation time and "exactly now()" / "exactly N days ago" are exact.
func TestSegmentFutureEvents(t *testing.T) {
	pool := testsupport.Pool(t, pg.NewMigratedDatabase(t))
	ctx := context.Background()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	setup := []string{
		// 201: only a future cart.
		// 202: cart 5 days ago, purchase in the future (does not recover it).
		// 203: cart 5 days ago, purchase exactly at now() (recovers it; matches purchase exists).
		// 204: cart exactly 30 days ago (lower edge), no purchase.
		// 205: carts 2 and 3 days ago, plus two future carts.
		// 206: cart exactly at now(), no purchase.
		// 207: signup and last activity in the future.
		// 208: signup and last activity exactly at now().
		`INSERT INTO users (id, first_name, last_name, city, city_normalized, gender, signup_at, last_active_at) VALUES
		 (201, 'A', 'B', 'Ankara', 'ankara', 'female', now() - interval '400 days', NULL),
		 (202, 'A', 'B', 'Ankara', 'ankara', 'female', now() - interval '400 days', NULL),
		 (203, 'A', 'B', 'Ankara', 'ankara', 'female', now() - interval '400 days', NULL),
		 (204, 'A', 'B', 'Ankara', 'ankara', 'female', now() - interval '400 days', NULL),
		 (205, 'A', 'B', 'Ankara', 'ankara', 'female', now() - interval '400 days', NULL),
		 (206, 'A', 'B', 'Ankara', 'ankara', 'female', now() - interval '400 days', NULL),
		 (207, 'A', 'B', 'Ankara', 'ankara', 'female', now() + interval '1 day', now() + interval '1 day'),
		 (208, 'A', 'B', 'Ankara', 'ankara', 'female', now(), now())`,
		`INSERT INTO cart_events (user_id, product_id, category, quantity, unit_price, occurred_at) VALUES
		 (201, 'P', 'toys', 1, 1, now() + interval '1 day'),
		 (202, 'P', 'toys', 1, 1, now() - interval '5 days'),
		 (203, 'P', 'toys', 1, 1, now() - interval '5 days'),
		 (204, 'P', 'toys', 1, 1, now() - make_interval(days => 30)),
		 (205, 'P', 'toys', 1, 1, now() - interval '2 days'),
		 (205, 'P', 'toys', 1, 1, now() - interval '3 days'),
		 (205, 'P', 'toys', 1, 1, now() + interval '1 hour'),
		 (205, 'P', 'toys', 1, 1, now() + interval '2 days'),
		 (206, 'P', 'toys', 1, 1, now())`,
		`INSERT INTO purchases (user_id, order_total, category, purchased_at) VALUES
		 (202, 1, 'toys', now() + interval '1 day'),
		 (203, 1, 'toys', now())`,
	}
	for _, s := range setup {
		if _, err := tx.Exec(ctx, s); err != nil {
			t.Fatalf("%v\n%s", err, s)
		}
	}
	everyone := ids(201, 202, 203, 204, 205, 206, 207, 208)

	tests := []struct {
		name, rule string
		want       []int64
	}{
		// 1, 6, 7: future carts never match; exact edges (now() and now() − 30 days) do.
		{"cart exists 30", cond(`{"type":"event","event":"cart_add","op":"exists","within_days":30}`), ids(202, 203, 204, 205, 206)},
		// 3: future events do not affect counts or not_exists.
		{"cart count_gte 1 ≡ exists", cond(`{"type":"event","event":"cart_add","op":"count_gte","count":1,"within_days":30}`), ids(202, 203, 204, 205, 206)},
		{"cart not_exists 30", cond(`{"type":"event","event":"cart_add","op":"not_exists","within_days":30}`), ids(201, 207, 208)},
		{"cart count_lte 0 ≡ not_exists", cond(`{"type":"event","event":"cart_add","op":"count_lte","count":0,"within_days":30}`), ids(201, 207, 208)},
		{"cart count_gte 2: future carts not counted", cond(`{"type":"event","event":"cart_add","op":"count_gte","count":2,"within_days":30}`), ids(205)},
		{"cart count_gte 3: future carts not counted", cond(`{"type":"event","event":"cart_add","op":"count_gte","count":3,"within_days":30}`), nil},
		{"cart count_lte 1: 205 has 2 historical carts", cond(`{"type":"event","event":"cart_add","op":"count_lte","count":1,"within_days":30}`), ids(201, 202, 203, 204, 206, 207, 208)},
		{"cart count_lte 2: future carts do not exclude 205", cond(`{"type":"event","event":"cart_add","op":"count_lte","count":2,"within_days":30}`), everyone},
		{"cart count_gte 0", cond(`{"type":"event","event":"cart_add","op":"count_gte","count":0,"within_days":30}`), everyone},
		// 2, 6: a future purchase never matches; one exactly at now() does.
		{"purchase exists 30", cond(`{"type":"event","event":"purchase","op":"exists","within_days":30}`), ids(203)},
		{"purchase count_gte 1", cond(`{"type":"event","event":"purchase","op":"count_gte","count":1,"within_days":30}`), ids(203)},
		{"purchase not_exists 30", cond(`{"type":"event","event":"purchase","op":"not_exists","within_days":30}`), ids(201, 202, 204, 205, 206, 207, 208)},
		{"purchase count_lte 0", cond(`{"type":"event","event":"purchase","op":"count_lte","count":0,"within_days":30}`), ids(201, 202, 204, 205, 206, 207, 208)},
		// 4, 5, 6, 7: future carts are ignored, a future purchase does not
		// recover, a purchase at now() does, edge carts count.
		{"abandoned 30", cond(abandoned30), ids(202, 204, 205, 206)},
		// 8: future signup / last activity satisfy no days-ago condition; now() is 0 days ago.
		{"signup lte 0", cond(`{"type":"attribute","field":"signup_days_ago","op":"lte","value":0}`), ids(208)},
		{"signup lte 3650", cond(`{"type":"attribute","field":"signup_days_ago","op":"lte","value":3650}`), slices.DeleteFunc(slices.Clone(everyone), func(i int64) bool { return i == 207 })},
		{"signup gte 0", cond(`{"type":"attribute","field":"signup_days_ago","op":"gte","value":0}`), slices.DeleteFunc(slices.Clone(everyone), func(i int64) bool { return i == 207 })},
		{"signup between 0..10", cond(`{"type":"attribute","field":"signup_days_ago","op":"between","value":{"min":0,"max":10}}`), ids(208)},
		{"last active lte 0", cond(`{"type":"attribute","field":"last_active_days_ago","op":"lte","value":0}`), ids(208)},
		{"last active lte 3650", cond(`{"type":"attribute","field":"last_active_days_ago","op":"lte","value":3650}`), ids(208)},
		{"last active gte 0", cond(`{"type":"attribute","field":"last_active_days_ago","op":"gte","value":0}`), ids(208)},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := matchedIDs(t, tx, parseRule(t, tc.rule)); !slices.Equal(got, tc.want) {
				t.Errorf("matched %v, want %v", got, tc.want)
			}
		})
	}
}

// TestAudienceStatsCancelledMidQueryReleasesConnection: Stats is cancelled
// while its query waits on a lock; afterwards its connection is back in the
// pool (or discarded) and no server session is left inside a transaction —
// the deferred rollback ran despite the cancelled request context.
func TestAudienceStatsCancelledMidQueryReleasesConnection(t *testing.T) {
	dbURL := pg.NewMigratedDatabase(t)
	admin := testsupport.Pool(t, dbURL)
	repoPool := testsupport.Pool(t, dbURL)
	loadFixture(t, admin)
	repo := segment.NewAudienceRepository(repoPool)
	rule := parseRule(t, cond(abandoned30))
	ctx := context.Background()

	locker, err := admin.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer locker.Rollback(ctx) //nolint:errcheck
	if _, err := locker.Exec(ctx, "LOCK TABLE users IN ACCESS EXCLUSIVE MODE"); err != nil {
		t.Fatal(err)
	}
	var lockerPID int32
	if err := locker.QueryRow(ctx, "SELECT pg_backend_pid()").Scan(&lockerPID); err != nil {
		t.Fatal(err)
	}

	reqCtx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() {
		_, err := repo.Stats(reqCtx, rule, []segment.Channel{segment.ChannelSMS})
		done <- err
	}()
	waitForLockWait(t, admin, "WITH matched AS (")
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v, want context.Canceled", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("Stats did not return after cancellation")
	}

	// Deadline-bounded polling: the repository pool holds no connection and
	// no other session of this database is in a transaction.
	deadline := time.Now().Add(30 * time.Second)
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for {
		var inTx int64
		err := admin.QueryRow(ctx, `
			SELECT count(*) FROM pg_stat_activity
			 WHERE datname = current_database() AND xact_start IS NOT NULL
			   AND pid NOT IN ($1, pg_backend_pid())`, lockerPID).Scan(&inTx)
		acquired := repoPool.Stat().AcquiredConns()
		if err == nil && inTx == 0 && acquired == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("after cancellation: sessions in a transaction = %d, acquired repo connections = %d (err %v)", inTx, acquired, err)
		}
		<-tick.C
	}

	if err := locker.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.Stats(ctx, rule, []segment.Channel{segment.ChannelSMS}); err != nil {
		t.Errorf("after cancellation the pool is unusable: %v", err)
	}
}
