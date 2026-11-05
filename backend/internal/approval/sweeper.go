package approval

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/fyusufayan/insider-campaign-composer-case/backend/internal/campaign"
)

// The background approval-expiry sweeper (ARCHITECTURE §5.3 "Expiry
// mechanism" (2a), §10.1). Correctness never depends on it: every consuming
// transaction (confirm, send) re-checks expiry under the campaign lock with
// clock_timestamp(), and the API runs lazy expiry first. The sweeper only
// makes expiry visible (held rows deleted, campaign back to content_ready,
// version + 1) without waiting for the next request.
//
// Each pass selects due candidates with a cheap unlocked query, then expires
// each one with ExpireDue — its own transaction that locks the campaign row
// first and re-checks with clock_timestamp() (§5.1). Several sweepers (or a
// sweeper and the API) may race safely: they serialize on the campaign row,
// and whoever comes second finds nothing live and due, so an approval is
// expired — and the version advanced — exactly once. Consumed approvals
// (sending / finished campaigns) are never candidates and never expire.

// sqlDueApprovals lists campaigns whose live approval is due (index
// approvals_expiry). Advisory only: ExpireDue decides under the lock.
const sqlDueApprovals = `SELECT campaign_id FROM campaign_approvals
WHERE status IN ('pending', 'approved') AND expires_at <= clock_timestamp()
ORDER BY expires_at, id
LIMIT $1`

// DueCampaigns lists up to n campaigns whose live approval looks due.
func (r *Repository) DueCampaigns(ctx context.Context, n int) ([]campaign.ID, error) {
	rows, err := r.pool.Query(ctx, sqlDueApprovals, n)
	if err != nil {
		return nil, fmt.Errorf("approval: due approvals: %w", err)
	}
	ids, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (campaign.ID, error) {
		var id [16]byte
		err := row.Scan(&id)
		return campaign.ID(id), err
	})
	if err != nil {
		return nil, fmt.Errorf("approval: due approvals: %w", err)
	}
	return ids, nil
}

// SweeperConfig configures the sweeper.
type SweeperConfig struct {
	// Interval between passes (APPROVAL_SWEEP_INTERVAL, default 30 s).
	Interval time.Duration
	// Batch bounds one candidate query; a full batch is followed by another
	// query in the same pass.
	Batch int
	// MaxPerPass bounds the expiries attempted in one pass.
	MaxPerPass int
}

// Sweeper defaults.
const (
	DefaultSweepInterval   = 30 * time.Second
	DefaultSweepBatch      = 100
	DefaultSweepMaxPerPass = 1000
)

// Sweeper periodically expires due approvals.
type Sweeper struct {
	repo *Repository
	cfg  SweeperConfig
	log  *slog.Logger
}

// NewSweeper returns a sweeper.
func NewSweeper(repo *Repository, cfg SweeperConfig, log *slog.Logger) (*Sweeper, error) {
	if repo == nil || log == nil {
		return nil, errors.New("approval: nil sweeper dependency")
	}
	if cfg.Interval == 0 {
		cfg.Interval = DefaultSweepInterval
	}
	if cfg.Batch == 0 {
		cfg.Batch = DefaultSweepBatch
	}
	if cfg.MaxPerPass == 0 {
		cfg.MaxPerPass = DefaultSweepMaxPerPass
	}
	if cfg.Interval < 10*time.Millisecond || cfg.Batch < 1 || cfg.MaxPerPass < cfg.Batch {
		return nil, errors.New("approval: invalid sweeper configuration")
	}
	return &Sweeper{repo: repo, cfg: cfg, log: log}, nil
}

// SweepStats counts one pass.
type SweepStats struct {
	// Expired approvals (each one transition: approval expired, held rows
	// deleted, campaign content_ready, version + 1).
	Expired int
	// Skipped candidates that were no longer due or live under the lock
	// (expired, confirmed into a fresh TTL, consumed, withdrawn by someone
	// else meanwhile).
	Skipped int
	// Failed candidates (database errors; retried next pass).
	Failed int
}

// Sweep runs one bounded pass.
func (s *Sweeper) Sweep(ctx context.Context) SweepStats {
	var st SweepStats
	seen := map[campaign.ID]bool{}
	for st.Expired+st.Skipped+st.Failed < s.cfg.MaxPerPass && ctx.Err() == nil {
		ids, err := s.repo.DueCampaigns(ctx, s.cfg.Batch)
		if err != nil {
			if ctx.Err() == nil {
				s.log.Warn("approval sweep: candidate query failed", "err", err)
			}
			return st
		}
		fresh := 0
		for _, id := range ids {
			if seen[id] { // failed earlier in this pass: not again until the next pass
				continue
			}
			seen[id] = true
			fresh++
			expired, err := s.repo.ExpireDue(ctx, id)
			switch {
			case err != nil:
				st.Failed++
				if ctx.Err() == nil {
					s.log.Warn("approval sweep: expiry failed", "campaign_id", id.String(), "err", err)
				}
			case expired == nil:
				st.Skipped++
			default:
				st.Expired++
				s.log.Info("approval expired", "campaign_id", id.String(), "approval_id", expired.String(), "by", "sweeper")
			}
		}
		if len(ids) < s.cfg.Batch || fresh == 0 {
			break
		}
	}
	return st
}

// Run sweeps every Interval until ctx ends.
func (s *Sweeper) Run(ctx context.Context) {
	t := time.NewTicker(s.cfg.Interval)
	defer t.Stop()
	for {
		st := s.Sweep(ctx)
		if st.Expired+st.Failed > 0 {
			s.log.Info("approval sweep pass", "expired", st.Expired, "skipped", st.Skipped, "failed", st.Failed)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}
