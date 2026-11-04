package segment

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// AudienceStatementTimeout bounds every audience query (ARCHITECTURE §6.4).
// It is applied with SET LOCAL, so it ends with the transaction.
const AudienceStatementTimeout = 5 * time.Second

const (
	sqlSetStatementTimeout = "SET LOCAL statement_timeout = '5s'"

	// sqlAudienceStatsHead / Tail wrap the compiled matched-users statement.
	// matched has one row per user (Compile selects the primary key without
	// joins); each LEFT JOIN on channel_consents matches at most one row
	// (PRIMARY KEY (user_id, channel)), so no user is counted twice. A
	// missing consent row is classified exactly like opted_out.
	//
	// Per channel: 0 = no_contact, 1 = no_consent, 2 = eligible.
	sqlAudienceStatsHead = `WITH matched AS (`
	sqlAudienceStatsTail = `),
classified AS (
  SELECT
    CASE WHEN u.phone IS NULL THEN 0 WHEN sc.status = 'opted_in' THEN 2 ELSE 1 END AS sms,
    CASE WHEN u.email IS NULL THEN 0 WHEN ec.status = 'opted_in' THEN 2 ELSE 1 END AS email
  FROM matched AS m
  JOIN users AS u ON u.id = m.id
  LEFT JOIN channel_consents AS sc ON sc.user_id = u.id AND sc.channel = 'sms'
  LEFT JOIN channel_consents AS ec ON ec.user_id = u.id AND ec.channel = 'email'
)
SELECT
  count(*),
  count(*) FILTER (WHERE sms = 0),
  count(*) FILTER (WHERE sms = 1),
  count(*) FILTER (WHERE sms = 2),
  count(*) FILTER (WHERE email = 0),
  count(*) FILTER (WHERE email = 1),
  count(*) FILTER (WHERE email = 2),
  count(*) FILTER (WHERE sms = 2 AND email = 2),
  count(*) FILTER (WHERE sms = 2 AND email <> 2),
  count(*) FILTER (WHERE sms <> 2 AND email = 2),
  count(*) FILTER (WHERE sms <> 2 AND email <> 2),
  now()
FROM classified`
)

// rollbackTimeout bounds the transaction cleanup that runs after Stats returns.
const rollbackTimeout = 5 * time.Second

// cleanupContext returns a context for cleanup work after ctx may have been
// cancelled: it keeps ctx's values, ignores its cancellation and deadline, and
// expires after rollbackTimeout. The caller must call the CancelFunc.
func cleanupContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), rollbackTimeout)
}

// Querier is the subset of pgx.Tx / pgx.Conn / pgxpool.Pool used to run the
// audience query inside a caller's transaction.
type Querier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// AudienceRepository computes audience statistics over the customer tables.
type AudienceRepository struct {
	pool *pgxpool.Pool
}

// NewAudienceRepository returns a repository using pool.
func NewAudienceRepository(pool *pgxpool.Pool) *AudienceRepository {
	return &AudienceRepository{pool: pool}
}

// Stats computes the audience partition (ARCHITECTURE §6.5) of rule for the
// selected channels in its own short read-only transaction with a
// transaction-local statement timeout. No network I/O other than the
// database happens while the transaction is open.
func (r *AudienceRepository) Stats(ctx context.Context, rule Validated, channels []Channel) (AudienceStats, error) {
	// Fail on bad input before opening a transaction.
	if _, _, err := Compile(rule); err != nil {
		return AudienceStats{}, err
	}
	if _, err := NormalizeChannels(channels); err != nil {
		return AudienceStats{}, err
	}

	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly})
	if err != nil {
		return AudienceStats{}, fmt.Errorf("segment: begin audience transaction: %w", err)
	}
	defer func() {
		// No-op after a successful commit. Otherwise the rollback must still
		// run when ctx is already cancelled (to return a clean connection to
		// the pool), but never without a deadline.
		cleanupCtx, cancel := cleanupContext(ctx)
		defer cancel()
		_ = tx.Rollback(cleanupCtx)
	}()
	if _, err := tx.Exec(ctx, sqlSetStatementTimeout); err != nil {
		return AudienceStats{}, fmt.Errorf("segment: set statement timeout: %w", err)
	}
	stats, err := ComputeAudienceStats(ctx, tx, rule, channels)
	if err != nil {
		return AudienceStats{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return AudienceStats{}, fmt.Errorf("segment: commit audience transaction: %w", err)
	}
	return stats, nil
}

// ComputeAudienceStats runs the audience query with q, which is normally a
// transaction owned by the caller (e.g. approval preparation, which must see
// the same snapshot as the rows it inserts). The caller is responsible for
// the transaction's access mode and statement timeout.
func ComputeAudienceStats(ctx context.Context, q Querier, rule Validated, channels []Channel) (AudienceStats, error) {
	sel, err := NormalizeChannels(channels)
	if err != nil {
		return AudienceStats{}, err
	}
	matchedSQL, args, err := Compile(rule)
	if err != nil {
		return AudienceStats{}, err
	}
	var c audienceCells
	err = q.QueryRow(ctx, sqlAudienceStatsHead+matchedSQL+sqlAudienceStatsTail, args...).Scan(
		&c.matched,
		&c.smsNoContact, &c.smsNoConsent, &c.smsElig,
		&c.emailNoContact, &c.emailNoConsent, &c.emElig,
		&c.bothElig, &c.smsOnlyElig, &c.emailOnlyElig, &c.neitherElig,
		&c.computedAt,
	)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil && !errors.Is(err, ctxErr) {
			err = fmt.Errorf("%w (%w)", ctxErr, err)
		}
		return AudienceStats{}, fmt.Errorf("segment: audience query: %w", err)
	}
	c.computedAt = c.computedAt.UTC()
	return c.project(sel)
}
