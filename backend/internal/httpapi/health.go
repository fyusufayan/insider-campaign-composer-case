package httpapi

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/fyusufayan/insider-campaign-composer-case/backend/internal/delivery"
)

// readinessTimeout bounds the readiness probe's database checks.
const readinessTimeout = 2 * time.Second

// ReadinessReport is the body of GET /readyz.
type ReadinessReport struct {
	// Status is "ready" or "not_ready".
	Status string `json:"status"`
	// Database is "ok" or "unavailable" (never the error text).
	Database         string `json:"database"`
	MigrationVersion uint   `json:"migration_version"`
	MigrationDirty   bool   `json:"migration_dirty"`
	// LLMProvider is the provider selected at startup: "anthropic" or "demo".
	LLMProvider string `json:"llm_provider"`
	// Worker is the freshest delivery-worker heartbeat (CP17B, §15):
	// reported only — readiness of the API never fails on it, so it does
	// not flap while no campaign is sending. "unknown" when the database
	// is unavailable.
	Worker delivery.WorkerHealth `json:"worker"`
	// SendingCampaigns counts campaigns in status sending (null when the
	// database is unavailable).
	SendingCampaigns *int64 `json:"sending_campaigns"`
}

// ReadinessFunc reports readiness. It returns the report and whether the
// process is ready; err (if any) is logged and never sent to the client.
type ReadinessFunc func(ctx context.Context) (report ReadinessReport, err error)

func handleHealthz(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func handleReadyz(check ReadinessFunc, log *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), readinessTimeout)
		defer cancel()
		report, err := check(ctx)
		if err != nil {
			log.WarnContext(r.Context(), "not ready", "err", err)
			report.Status = "not_ready"
			writeJSON(w, http.StatusServiceUnavailable, report)
			return
		}
		report.Status = "ready"
		writeJSON(w, http.StatusOK, report)
	}
}
