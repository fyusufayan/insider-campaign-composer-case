package main

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

// seedLockKey serializes concurrent seed runs (transaction-scoped advisory lock).
const seedLockKey int64 = 0x5eed_0000_0001

// Reset refusals. Customer IDs restart at 1 on reset, so any campaign (its
// audience statistics, approvals, snapshots or deliveries) would silently start
// referring to different people. A reset is therefore only allowed on a
// database without campaigns; delivery rows are checked explicitly as well.
var (
	ErrDeliveriesExist = errors.New("refusing to reset: campaign_deliveries contains rows; customer data referenced by deliveries cannot be replaced")
	ErrCampaignsExist  = errors.New("refusing to reset: campaigns contains rows; customer IDs are reused after a reset, so it is only allowed on a database without campaigns")
)

// Statements. Every value is a bind parameter; identifiers are constants.
const (
	sqlAdvisoryLock = `SELECT pg_advisory_xact_lock($1)`
	sqlUsersExist   = `SELECT EXISTS (SELECT 1 FROM users)`

	// SHARE mode waits for every transaction that has inserted, updated or deleted
	// campaigns and blocks new ones until this transaction ends. After it is granted
	// no campaign can appear (or be mid-creation), and therefore no approval or
	// delivery either (both reference campaigns), so the checks below stay true
	// until commit. Only campaigns is locked: also locking campaign_deliveries could
	// deadlock with an approval that holds delivery row locks and waits to update
	// its campaign. Lock order campaigns → … matches the application's.
	sqlLockCampaigns    = `LOCK TABLE campaigns IN SHARE MODE`
	sqlCampaignsExist   = `SELECT EXISTS (SELECT 1 FROM campaigns)`
	sqlDeliveriesExist  = `SELECT EXISTS (SELECT 1 FROM campaign_deliveries)`
	sqlDeletePurchases  = `DELETE FROM purchases`
	sqlDeleteCartEvents = `DELETE FROM cart_events`
	sqlDeleteConsents   = `DELETE FROM channel_consents`
	sqlDeleteUsers      = `DELETE FROM users`

	// ALTER SEQUENCE ... RESTART is transactional: a rolled-back load leaves the sequences untouched.
	sqlRestartUserSeq     = `ALTER SEQUENCE users_id_seq RESTART WITH 1`
	sqlRestartCartSeq     = `ALTER SEQUENCE cart_events_id_seq RESTART WITH 1`
	sqlRestartPurchaseSeq = `ALTER SEQUENCE purchases_id_seq RESTART WITH 1`

	sqlReadUsers = `SELECT id, first_name, last_name, email, phone, city, city_normalized, country, gender,
       birth_date, language, signup_at, last_active_at
  FROM users ORDER BY id`
	// ORDER BY the qualified column: a bare "channel" would bind to the ::text output
	// column and sort alphabetically instead of in enum order (sms, email).
	sqlReadConsents = `SELECT user_id, channel::text, status, source, updated_at
  FROM channel_consents ORDER BY channel_consents.user_id, channel_consents.channel`
	sqlReadCartEvents = `SELECT id, user_id, product_id, category, quantity, (unit_price * 100)::bigint, occurred_at
  FROM cart_events ORDER BY id`
	sqlReadPurchases = `SELECT id, user_id, (order_total * 100)::bigint, category, purchased_at
  FROM purchases ORDER BY id`
)

var (
	userColumns     = []string{"first_name", "last_name", "email", "phone", "city", "city_normalized", "country", "gender", "birth_date", "language", "signup_at", "last_active_at"}
	consentColumns  = []string{"user_id", "channel", "status", "source", "updated_at"}
	cartColumns     = []string{"user_id", "product_id", "category", "quantity", "unit_price", "occurred_at"}
	purchaseColumns = []string{"user_id", "order_total", "category", "purchased_at"}
)

// load writes ds in one transaction. Without reset it does nothing (and
// returns false) when users is already populated. With reset it refuses if any
// campaign or delivery row exists, otherwise deletes the customer tables
// (explicitly, never TRUNCATE ... CASCADE) and reloads; campaign data is never
// touched. IDs are assigned by the database from
// freshly restarted sequences in insertion order; before committing, the rows
// are read back and must reproduce wantHash.
func load(ctx context.Context, pool *pgxpool.Pool, ds *Dataset, wantHash string, reset bool) (loaded bool, err error) {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return false, fmt.Errorf("begin: %w", err)
	}
	defer func() {
		if rbErr := tx.Rollback(ctx); rbErr != nil && !errors.Is(rbErr, pgx.ErrTxClosed) && err == nil {
			err = fmt.Errorf("rollback: %w", rbErr)
		}
	}()

	if _, err := tx.Exec(ctx, sqlAdvisoryLock, seedLockKey); err != nil {
		return false, fmt.Errorf("acquire seed lock: %w", err)
	}

	if reset {
		if err := deleteCustomerData(ctx, tx); err != nil {
			return false, err
		}
	} else {
		var populated bool
		if err := tx.QueryRow(ctx, sqlUsersExist).Scan(&populated); err != nil {
			return false, fmt.Errorf("check users (is the database migrated?): %w", err)
		}
		if populated {
			return false, nil
		}
	}

	for _, stmt := range []string{sqlRestartUserSeq, sqlRestartCartSeq, sqlRestartPurchaseSeq} {
		if _, err := tx.Exec(ctx, stmt); err != nil {
			return false, fmt.Errorf("restart sequence: %w", err)
		}
	}

	// COPY uses the binary format, which needs the enum type registered on this connection.
	channelType, err := tx.Conn().LoadType(ctx, "channel")
	if err != nil {
		return false, fmt.Errorf("load channel type: %w", err)
	}
	tx.Conn().TypeMap().RegisterType(channelType)

	if err := copyDataset(ctx, tx, ds); err != nil {
		return false, err
	}

	got, err := readDataset(ctx, tx)
	if err != nil {
		return false, err
	}
	if h := got.Hash(); h != wantHash {
		return false, fmt.Errorf("loaded rows do not match the generated dataset: database hash %s, generated %s", h, wantHash)
	}

	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("commit: %w", err)
	}
	return true, nil
}

// deleteCustomerData refuses unless the database has no campaigns and no
// deliveries, then deletes the customer tables. It never waits on a lock after
// acquiring the campaigns lock, so it cannot take part in a deadlock.
func deleteCustomerData(ctx context.Context, tx pgx.Tx) error {
	if _, err := tx.Exec(ctx, sqlLockCampaigns); err != nil {
		return fmt.Errorf("lock campaigns: %w", err)
	}
	var deliveries, campaigns bool
	if err := tx.QueryRow(ctx, sqlDeliveriesExist).Scan(&deliveries); err != nil {
		return fmt.Errorf("check campaign_deliveries: %w", err)
	}
	if deliveries {
		return ErrDeliveriesExist
	}
	if err := tx.QueryRow(ctx, sqlCampaignsExist).Scan(&campaigns); err != nil {
		return fmt.Errorf("check campaigns: %w", err)
	}
	if campaigns {
		return ErrCampaignsExist
	}
	// Children before parents, so nothing depends on ON DELETE CASCADE.
	for _, stmt := range []string{sqlDeletePurchases, sqlDeleteCartEvents, sqlDeleteConsents, sqlDeleteUsers} {
		if _, err := tx.Exec(ctx, stmt); err != nil {
			return fmt.Errorf("delete customer data: %w", err)
		}
	}
	return nil
}

func copyDataset(ctx context.Context, tx pgx.Tx, ds *Dataset) error {
	steps := []struct {
		table string
		cols  []string
		n     int
		row   func(i int) []any
	}{
		{"users", userColumns, len(ds.Users), func(i int) []any {
			u := ds.Users[i]
			return []any{u.FirstName, u.LastName, u.Email, u.Phone, u.City, u.CityNormalized, u.Country,
				u.Gender, date(u.BirthDate), u.Language, u.SignupAt, u.LastActiveAt}
		}},
		{"channel_consents", consentColumns, len(ds.Consents), func(i int) []any {
			c := ds.Consents[i]
			return []any{c.UserID, c.Channel, c.Status, c.Source, c.UpdatedAt}
		}},
		{"cart_events", cartColumns, len(ds.CartEvents), func(i int) []any {
			e := ds.CartEvents[i]
			return []any{e.UserID, e.ProductID, e.Category, e.Quantity, numeric(e.UnitPriceCents), e.OccurredAt}
		}},
		{"purchases", purchaseColumns, len(ds.Purchases), func(i int) []any {
			p := ds.Purchases[i]
			return []any{p.UserID, numeric(p.OrderTotalCents), p.Category, p.PurchasedAt}
		}},
	}
	for _, s := range steps {
		n, err := tx.CopyFrom(ctx, pgx.Identifier{s.table}, s.cols, pgx.CopyFromSlice(s.n, func(i int) ([]any, error) {
			return s.row(i), nil
		}))
		if err != nil {
			return fmt.Errorf("copy %s: %w", s.table, err)
		}
		if n != int64(s.n) {
			return fmt.Errorf("copy %s: wrote %d rows, want %d", s.table, n, s.n)
		}
	}
	return nil
}

// readDataset reads the customer tables back in canonical order.
func readDataset(ctx context.Context, tx pgx.Tx) (*Dataset, error) {
	ds := &Dataset{}
	var err error
	ds.Users, err = readRows(ctx, tx, sqlReadUsers, func(r pgx.CollectableRow) (User, error) {
		var u User
		var bd pgtype.Date
		err := r.Scan(&u.ID, &u.FirstName, &u.LastName, &u.Email, &u.Phone, &u.City, &u.CityNormalized,
			&u.Country, &u.Gender, &bd, &u.Language, &u.SignupAt, &u.LastActiveAt)
		if bd.Valid {
			t := bd.Time
			u.BirthDate = &t
		}
		return u, err
	})
	if err != nil {
		return nil, err
	}
	ds.Consents, err = readRows(ctx, tx, sqlReadConsents, func(r pgx.CollectableRow) (Consent, error) {
		var c Consent
		return c, r.Scan(&c.UserID, &c.Channel, &c.Status, &c.Source, &c.UpdatedAt)
	})
	if err != nil {
		return nil, err
	}
	ds.CartEvents, err = readRows(ctx, tx, sqlReadCartEvents, func(r pgx.CollectableRow) (CartEvent, error) {
		var e CartEvent
		return e, r.Scan(&e.ID, &e.UserID, &e.ProductID, &e.Category, &e.Quantity, &e.UnitPriceCents, &e.OccurredAt)
	})
	if err != nil {
		return nil, err
	}
	ds.Purchases, err = readRows(ctx, tx, sqlReadPurchases, func(r pgx.CollectableRow) (Purchase, error) {
		var p Purchase
		return p, r.Scan(&p.ID, &p.UserID, &p.OrderTotalCents, &p.Category, &p.PurchasedAt)
	})
	if err != nil {
		return nil, err
	}
	return ds, nil
}

func readRows[T any](ctx context.Context, tx pgx.Tx, query string, scan func(pgx.CollectableRow) (T, error)) ([]T, error) {
	rows, err := tx.Query(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("read back: %w", err)
	}
	out, err := pgx.CollectRows(rows, scan)
	if err != nil {
		return nil, fmt.Errorf("read back: %w", err)
	}
	return out, nil
}

func date(t *time.Time) pgtype.Date {
	if t == nil {
		return pgtype.Date{}
	}
	return pgtype.Date{Time: *t, Valid: true}
}

func numeric(cents int64) pgtype.Numeric {
	return pgtype.Numeric{Int: big.NewInt(cents), Exp: -2, Valid: true}
}
