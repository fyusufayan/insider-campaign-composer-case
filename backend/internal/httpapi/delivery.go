package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"time"

	"github.com/fyusufayan/insider-campaign-composer-case/backend/internal/delivery"
	"github.com/fyusufayan/insider-campaign-composer-case/backend/internal/provider"
)

// Delivery progress endpoints (CP17B; ARCHITECTURE §11, §12, §13):
//
//	GET /api/v1/campaigns/{id}/stats       one statistics snapshot (§13)
//	GET /api/v1/campaigns/{id}/deliveries  ?status=&channel=&uncertain=&cursor=&limit= (safe rows, masked destinations)
//	GET /api/v1/campaigns/{id}/events      SSE: "stats" snapshots, "end" once terminal
//
// Everything is read from committed PostgreSQL rows: a delivery is shown as
// sent only after the worker's completion transaction committed. Responses
// carry counts, sanitized reason codes and masked destinations only — never
// user ids, names, contacts, frozen vars, message bodies or idempotency keys.

// DeliveryStats reads delivery statistics (implemented by
// delivery.StatsReader; an interface so the SSE handler can be tested with
// a scripted source).
type DeliveryStats interface {
	Stats(ctx context.Context, campaignID string) (delivery.CampaignStats, error)
	List(ctx context.Context, campaignID string, q delivery.ListQuery) (delivery.Page, error)
}

// EventsConfig tunes the progress stream. Zero values mean the defaults.
type EventsConfig struct {
	// Poll is the database poll period per open stream (default 1 s, §12).
	Poll time.Duration
	// Heartbeat is the keep-alive comment period (default 15 s).
	Heartbeat time.Duration
	// MaxLifetime closes a stream (without "end") after this long; the
	// client reconnects and gets a fresh snapshot (default 10 min).
	MaxLifetime time.Duration
	// MaxStreams bounds concurrently open streams per API process
	// (default 64); beyond it → 503 too_many_streams.
	MaxStreams int
	// Retry is the reconnection delay advertised to EventSource (default 3 s).
	Retry time.Duration
	// MaxReadFailures closes the stream after this many consecutive failed
	// reads (default 3); the client reconnects.
	MaxReadFailures int
	// NewTicker is the time source (tests inject a manual one).
	NewTicker func(d time.Duration) (<-chan time.Time, func())
}

const (
	defaultEventsPoll        = time.Second
	defaultEventsHeartbeat   = sseHeartbeat
	defaultEventsLifetime    = 10 * time.Minute
	defaultEventsMaxStreams  = 64
	defaultEventsRetry       = 3 * time.Second
	defaultEventsMaxFailures = 3
	statsReadTimeout         = 5 * time.Second
)

func (c EventsConfig) withDefaults() EventsConfig {
	if c.Poll <= 0 {
		c.Poll = defaultEventsPoll
	}
	if c.Heartbeat <= 0 {
		c.Heartbeat = defaultEventsHeartbeat
	}
	if c.MaxLifetime <= 0 {
		c.MaxLifetime = defaultEventsLifetime
	}
	if c.MaxStreams <= 0 {
		c.MaxStreams = defaultEventsMaxStreams
	}
	if c.Retry <= 0 {
		c.Retry = defaultEventsRetry
	}
	if c.MaxReadFailures <= 0 {
		c.MaxReadFailures = defaultEventsMaxFailures
	}
	if c.NewTicker == nil {
		c.NewTicker = func(d time.Duration) (<-chan time.Time, func()) {
			t := time.NewTicker(d)
			return t.C, t.Stop
		}
	}
	return c
}

type deliveryHandlers struct {
	*campaignHandlers
	stats   DeliveryStats
	cfg     EventsConfig
	streams chan struct{}
}

func newDeliveryHandlers(ch *campaignHandlers, stats DeliveryStats, cfg EventsConfig) *deliveryHandlers {
	cfg = cfg.withDefaults()
	return &deliveryHandlers{campaignHandlers: ch, stats: stats, cfg: cfg, streams: make(chan struct{}, cfg.MaxStreams)}
}

func (h *deliveryHandlers) register(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/v1/campaigns/{id}/stats", h.getStats)
	mux.HandleFunc("GET /api/v1/campaigns/{id}/deliveries", h.list)
	mux.HandleFunc("GET /api/v1/campaigns/{id}/events", h.events)
}

func (h *deliveryHandlers) deliveryError(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(err, delivery.ErrCampaignNotFound) {
		writeError(w, &APIError{Status: http.StatusNotFound, Code: CodeNotFound, Message: "campaign not found"})
		return
	}
	if errors.Is(err, delivery.ErrPartitionMismatch) {
		h.log.ErrorContext(r.Context(), "delivery statistics do not reconcile with the approved snapshot", "err", err,
			"campaign_id", r.PathValue("id"))
	}
	h.fail(w, r, err)
}

// GET /api/v1/campaigns/{id}/stats → 200 statistics snapshot (any status).
func (h *deliveryHandlers) getStats(w http.ResponseWriter, r *http.Request) {
	id, ok := h.pathID(w, r)
	if !ok || !noQuery(w, r) {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), statsReadTimeout)
	defer cancel()
	st, err := h.stats.Stats(ctx, id.String())
	if err != nil {
		h.deliveryError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, st)
}

// noQuery rejects any query parameter on routes that take none.
func noQuery(w http.ResponseWriter, r *http.Request) bool {
	if r.URL.RawQuery == "" {
		return true
	}
	q, err := url.ParseQuery(r.URL.RawQuery)
	details := []FieldError{}
	if err != nil {
		details = append(details, FieldError{Path: "query", Code: fieldInvalidValue, Message: "malformed query string"})
	}
	for k := range q {
		details = append(details, FieldError{Path: "query." + k, Code: "unknown_parameter", Message: "unknown query parameter"})
	}
	writeError(w, &APIError{Status: http.StatusUnprocessableEntity, Code: CodeValidationFailed, Message: "request is invalid", Details: details})
	return false
}

var cursorRE = regexp.MustCompile(`^[1-9][0-9]{0,17}$`)

// parseListQuery validates GET /deliveries parameters strictly: only
// status, channel, uncertain, cursor and limit, each at most once.
func parseListQuery(raw string) (delivery.ListQuery, []FieldError) {
	q := delivery.ListQuery{Limit: delivery.DefaultPageSize}
	vals, err := url.ParseQuery(raw)
	if err != nil {
		return q, []FieldError{{Path: "query", Code: fieldInvalidValue, Message: "malformed query string"}}
	}
	var errs []FieldError
	add := func(k, code, msg string, allowed ...string) {
		errs = append(errs, FieldError{Path: "query." + k, Code: code, Message: msg, Allowed: allowed})
	}
	for k, vs := range vals {
		if len(vs) != 1 {
			add(k, "duplicate_parameter", "must be given at most once")
			continue
		}
		v := vs[0]
		switch k {
		case "status":
			if !delivery.ValidStatus(v) {
				allowed := make([]string, len(delivery.PartitionStatuses))
				for i, s := range delivery.PartitionStatuses {
					allowed[i] = string(s)
				}
				add(k, "not_allowed", "unknown delivery status", allowed...)
				continue
			}
			s := delivery.Status(v)
			q.Status = &s
		case "channel":
			if !delivery.ValidChannel(v) {
				add(k, "not_allowed", "unknown channel", "sms", "email")
				continue
			}
			c := provider.Channel(v)
			q.Channel = &c
		case "uncertain":
			switch v {
			case "true", "false":
				b := v == "true"
				q.Uncertain = &b
			default:
				add(k, fieldInvalidValue, "must be true or false")
			}
		case "cursor":
			if !cursorRE.MatchString(v) {
				add(k, fieldInvalidValue, "must be a cursor returned as next_cursor")
				continue
			}
			n, err := strconv.ParseInt(v, 10, 64)
			if err != nil {
				add(k, fieldInvalidValue, "must be a cursor returned as next_cursor")
				continue
			}
			q.After = n
		case "limit":
			n, err := strconv.Atoi(v)
			if err != nil || n < 1 || n > delivery.MaxPageSize || strconv.Itoa(n) != v {
				add(k, fieldInvalidValue, "must be an integer between 1 and "+strconv.Itoa(delivery.MaxPageSize))
				continue
			}
			q.Limit = n
		default:
			add(k, "unknown_parameter", "unknown query parameter")
		}
	}
	return q, errs
}

// GET /api/v1/campaigns/{id}/deliveries → 200 {campaign_id, approval_id,
// items, next_cursor, limit}. Rows of the consumed approval only, ordered
// by id; items is [] before a send.
func (h *deliveryHandlers) list(w http.ResponseWriter, r *http.Request) {
	id, ok := h.pathID(w, r)
	if !ok {
		return
	}
	q, errs := parseListQuery(r.URL.RawQuery)
	if len(errs) > 0 {
		writeError(w, &APIError{Status: http.StatusUnprocessableEntity, Code: CodeValidationFailed, Message: "request is invalid", Details: errs})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), statsReadTimeout)
	defer cancel()
	page, err := h.stats.List(ctx, id.String(), q)
	if err != nil {
		h.deliveryError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, page)
}

// endEventDTO is the data of the terminal "end" event.
type endEventDTO struct {
	CampaignID      string `json:"campaign_id"`
	CampaignStatus  string `json:"campaign_status"`
	CampaignVersion int    `json:"campaign_version"`
	Outcome         string `json:"outcome"`
}

// changeKey is the part of a snapshot whose change is worth an event: all
// of it except the snapshot time and the heartbeat age (which grow every
// tick; the heartbeat timestamp itself is included).
func changeKey(st delivery.CampaignStats) ([]byte, error) {
	st.UpdatedAt = time.Time{}
	st.Worker.AgeSeconds = nil
	return json.Marshal(st)
}

// GET /api/v1/campaigns/{id}/events → text/event-stream (§12):
//
//	retry: <ms>                 once, first
//	event: stats   data: <GET /stats snapshot>   first, then whenever it changes (polled every Poll)
//	: keep-alive                every Heartbeat
//	event: end     data: {campaign_id, campaign_status, campaign_version, outcome}
//	                            exactly once, after the snapshot that is terminal (campaign completed /
//	                            completed_with_failures / cancelled and no row pending or in flight); then the stream closes
//
// Each snapshot is complete, so a reconnect needs no replay (no event ids).
// No transaction stays open between polls; a client disconnect or write
// failure ends the handler. Errors before the stream starts (invalid id,
// unknown campaign, stream limit) are JSON envelopes.
func (h *deliveryHandlers) events(w http.ResponseWriter, r *http.Request) {
	id, ok := h.pathID(w, r)
	if !ok || !noQuery(w, r) {
		return
	}
	select {
	case h.streams <- struct{}{}:
		defer func() { <-h.streams }()
	default:
		writeError(w, &APIError{Status: http.StatusServiceUnavailable, Code: CodeTooManyStreams,
			Message: "too many open progress streams; try again shortly", Retryable: true})
		return
	}
	ctx := r.Context()
	read := func() (delivery.CampaignStats, error) {
		rctx, cancel := context.WithTimeout(ctx, statsReadTimeout)
		defer cancel()
		return h.stats.Stats(rctx, id.String())
	}
	first, err := read()
	if err != nil {
		h.deliveryError(w, r, err)
		return
	}
	logAttrs := []any{"campaign_id", id.String()}
	sw := newSSEWriter(w)
	defer sw.finish()
	if err := sw.retry(h.cfg.Retry); err != nil {
		return
	}
	var lastKey []byte
	send := func(st delivery.CampaignStats) (terminal bool, err error) {
		key, err := changeKey(st)
		if err != nil {
			return false, err
		}
		if lastKey == nil || !bytes.Equal(key, lastKey) {
			if err := sw.event("stats", st); err != nil {
				return false, err
			}
			lastKey = key
		}
		if !st.Terminal {
			return false, nil
		}
		return true, sw.event("end", endEventDTO{CampaignID: st.CampaignID, CampaignStatus: st.CampaignStatus,
			CampaignVersion: st.CampaignVersion, Outcome: st.Outcome})
	}
	if done, err := send(first); done || err != nil {
		h.streamClosed(ctx, err, logAttrs)
		return
	}
	poll, stopPoll := h.cfg.NewTicker(h.cfg.Poll)
	defer stopPoll()
	beat, stopBeat := h.cfg.NewTicker(h.cfg.Heartbeat)
	defer stopBeat()
	life, stopLife := h.cfg.NewTicker(h.cfg.MaxLifetime)
	defer stopLife()
	failures := 0
	for {
		select {
		case <-ctx.Done():
			return
		case <-life:
			h.log.DebugContext(ctx, "progress stream reached its lifetime", logAttrs...)
			return
		case <-beat:
			if err := sw.comment("keep-alive"); err != nil {
				return
			}
		case <-poll:
			st, err := read()
			if err != nil {
				if ctx.Err() != nil {
					return
				}
				failures++
				h.log.WarnContext(ctx, "progress stream read failed", append(logAttrs, "err", err, "consecutive", failures)...)
				if failures >= h.cfg.MaxReadFailures {
					return
				}
				continue
			}
			failures = 0
			if done, err := send(st); done || err != nil {
				h.streamClosed(ctx, err, logAttrs)
				return
			}
		}
	}
}

func (h *deliveryHandlers) streamClosed(ctx context.Context, err error, attrs []any) {
	if err != nil && ctx.Err() == nil {
		h.log.WarnContext(ctx, "progress stream write failed", append(attrs, "err", err)...)
	}
}
