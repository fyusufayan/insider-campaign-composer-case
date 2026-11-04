package campaign

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/fyusufayan/insider-campaign-composer-case/backend/internal/ai"
	"github.com/fyusufayan/insider-campaign-composer-case/backend/internal/segment"
)

// Every statement is a constant; every value is a bind parameter. The enum
// array is exchanged as text[] so no custom type registration is needed.
const (
	campaignColumns = `id, name, brief, status, channels::text[], segment_rule, segment_version,
  interpretation, content_brief, current_content_id, active_approval_id, version, created_at, updated_at`

	sqlInsertCampaign = `INSERT INTO campaigns (name, brief, status) VALUES ($1, $2, 'draft')
RETURNING ` + campaignColumns

	sqlSelectCampaign          = `SELECT ` + campaignColumns + ` FROM campaigns WHERE id = $1`
	sqlSelectCampaignForUpdate = sqlSelectCampaign + ` FOR UPDATE`

	// The version predicate is redundant while the row lock is held; it
	// guards against a caller that forgot to lock.
	sqlUpdateCampaign = `UPDATE campaigns
SET status = $2, channels = $3::text[]::channel[], segment_rule = $4::jsonb, segment_version = $5,
    interpretation = $6::jsonb, content_brief = $7::jsonb, version = $8, updated_at = now()
WHERE id = $1 AND version = $9
RETURNING ` + campaignColumns

	// Authoring transactions are short and never wait long: a lock wait or
	// statement beyond these limits fails instead of piling up requests.
	sqlSetLocalTimeouts = `SET LOCAL lock_timeout = '5s'; SET LOCAL statement_timeout = '5s'`
)

// cleanupTimeout bounds the rollback that runs after the caller's context
// may already have been cancelled.
const cleanupTimeout = 5 * time.Second

// Repository persists campaigns in PostgreSQL.
type Repository struct {
	pool *pgxpool.Pool
}

// NewRepository returns a repository using pool.
func NewRepository(pool *pgxpool.Pool) *Repository { return &Repository{pool: pool} }

// Create inserts a draft campaign (status draft, version 1, segment_version
// 0, no channels).
func (r *Repository) Create(ctx context.Context, d Draft) (Campaign, error) {
	c, err := scanCampaign(r.pool.QueryRow(ctx, sqlInsertCampaign, d.Name, d.Brief))
	if err != nil {
		return Campaign{}, fmt.Errorf("campaign: insert: %w", err)
	}
	return c, nil
}

// Get reads a campaign without locking it.
func (r *Repository) Get(ctx context.Context, id ID) (Campaign, error) {
	c, err := scanCampaign(r.pool.QueryRow(ctx, sqlSelectCampaign, uuidArg(id)))
	if errors.Is(err, pgx.ErrNoRows) {
		return Campaign{}, ErrNotFound
	}
	if err != nil {
		return Campaign{}, fmt.Errorf("campaign %s: select: %w", id, err)
	}
	return c, nil
}

// Mutation computes the next state of a locked campaign. It must be pure: it
// runs while the campaign row lock is held, so it must not perform I/O
// (invariant 13). changed = false means nothing is written.
type Mutation func(current Campaign) (next Campaign, changed bool, err error)

// Update runs one short transaction: lock the campaign row (always first,
// §5.1), apply mutate, write the result and commit. An error from mutate
// rolls back and is returned unchanged.
func (r *Repository) Update(ctx context.Context, id ID, mutate Mutation) (Campaign, error) {
	var out Campaign
	err := r.withLockedCampaign(ctx, id, func(tx pgx.Tx, cur Campaign) error {
		next, changed, err := mutate(cur.clone())
		if err != nil {
			return err
		}
		if !changed {
			out = cur
			return nil
		}
		if next.ID != cur.ID {
			return fmt.Errorf("campaign %s: mutation changed the id", id)
		}
		args, err := updateArgs(cur.Version, next)
		if err != nil {
			return fmt.Errorf("campaign %s: %w", id, err)
		}
		saved, err := scanCampaign(tx.QueryRow(ctx, sqlUpdateCampaign, args...))
		if errors.Is(err, pgx.ErrNoRows) { // unreachable while the lock is held
			return &ConflictError{Expected: cur.Version, Current: -1}
		}
		if err != nil {
			return fmt.Errorf("campaign %s: update: %w", id, err)
		}
		out = saved
		return nil
	})
	if err != nil {
		return Campaign{}, err
	}
	return out, nil
}

// withLockedCampaign runs fn in one short transaction that first locks the
// campaign row (§5.1) and commits iff fn succeeds. The rollback of a failed
// or abandoned transaction runs even when ctx is already cancelled.
func (r *Repository) withLockedCampaign(ctx context.Context, id ID, fn func(tx pgx.Tx, cur Campaign) error) error {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("campaign %s: begin: %w", id, err)
	}
	defer func() {
		cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
		defer cancel()
		_ = tx.Rollback(cctx) // no-op after commit
	}()
	if _, err := tx.Exec(ctx, sqlSetLocalTimeouts); err != nil {
		return fmt.Errorf("campaign %s: set timeouts: %w", id, err)
	}
	cur, err := scanCampaign(tx.QueryRow(ctx, sqlSelectCampaignForUpdate, uuidArg(id)))
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("campaign %s: lock: %w", id, err)
	}
	if err := fn(tx, cur); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("campaign %s: commit: %w", id, err)
	}
	return nil
}

func updateArgs(lockedVersion int, c Campaign) ([]any, error) {
	if !c.Status.Valid() {
		return nil, fmt.Errorf("invalid status %q", c.Status)
	}
	channels := make([]string, len(c.Channels))
	for i, ch := range c.Channels {
		channels[i] = string(ch)
	}
	var rule, interp []byte
	var err error
	if !c.Segment.IsZero() {
		if rule, err = json.Marshal(c.Segment); err != nil {
			return nil, fmt.Errorf("encode segment rule: %w", err)
		}
	}
	if c.Interpretation != nil {
		if interp, err = json.Marshal(c.Interpretation); err != nil {
			return nil, fmt.Errorf("encode interpretation: %w", err)
		}
	}
	var brief []byte
	if c.ContentBrief != nil {
		brief = []byte(c.ContentBrief)
	}
	return []any{uuidArg(c.ID), string(c.Status), channels, rule, c.SegmentVersion,
		interp, brief, c.Version, lockedVersion}, nil
}

func uuidArg(id ID) pgtype.UUID { return pgtype.UUID{Bytes: id, Valid: true} }

// scanCampaign decodes one row. Stored rules are re-validated on load
// (segment.Parse), never trusted.
func scanCampaign(row pgx.Row) (Campaign, error) {
	var (
		c                       Campaign
		id, approval            pgtype.UUID
		status                  string
		channels                []string
		rule, interp, brief     []byte
		contentID               *int64
		createdAt, updatedAt    time.Time
		segmentVersion, version int32
	)
	if err := row.Scan(&id, &c.Name, &c.Brief, &status, &channels, &rule, &segmentVersion,
		&interp, &brief, &contentID, &approval, &version, &createdAt, &updatedAt); err != nil {
		return Campaign{}, err
	}
	c.ID = ID(id.Bytes)
	c.Status = Status(status)
	if !c.Status.Valid() {
		return Campaign{}, fmt.Errorf("stored status %q is unknown", status)
	}
	c.Channels = make([]segment.Channel, 0, len(channels))
	for _, ch := range channels {
		c.Channels = append(c.Channels, segment.Channel(ch))
	}
	if rule != nil {
		v, err := segment.Parse(rule)
		if err != nil {
			return Campaign{}, fmt.Errorf("stored segment rule is invalid: %w", err)
		}
		c.Segment = v
	}
	if interp != nil {
		var rec ai.Record
		dec := json.NewDecoder(bytes.NewReader(interp))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&rec); err != nil {
			return Campaign{}, fmt.Errorf("stored interpretation is invalid: %w", err)
		}
		c.Interpretation = &rec
	}
	if brief != nil {
		c.ContentBrief = json.RawMessage(brief)
	}
	c.SegmentVersion = int(segmentVersion)
	c.Version = int(version)
	c.CurrentContentID = contentID
	if approval.Valid {
		a := ID(approval.Bytes)
		c.ActiveApprovalID = &a
	}
	c.CreatedAt = createdAt.UTC()
	c.UpdatedAt = updatedAt.UTC()
	return c, nil
}
