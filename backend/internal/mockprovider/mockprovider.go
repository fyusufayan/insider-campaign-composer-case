// Package mockprovider is the fake SMS/email provider (cmd/mockprovider,
// ARCHITECTURE §11 "Mock provider", ADR-020). It sends nothing: an
// "accepted" message is an entry in an in-memory ledger. Production
// packages must not import it.
//
// Behaviour per request (POST /v1/sms, POST /v1/email):
//
//  1. the arrival is recorded at handler entry (also for requests that are
//     rejected, faulted or replayed);
//  2. the request is validated strictly: Idempotency-Key header (1–200
//     printable ASCII characters), JSON media type, a single JSON object
//     with exactly the channel's fields, a valid destination (E.164 phone /
//     email address) and bounded text → 400 invalid_request;
//  3. an injected fault may answer instead (5xx, 429 with Retry-After);
//  4. idempotency ledger, keyed by the Idempotency-Key: the same key with
//     the same payload (channel, destination, subject, text) replays the
//     original result — 200, the original message_id and
//     Idempotent-Replayed: true; nothing is sent again. The same key with a
//     different payload is rejected with 422 idempotency_key_reuse;
//  5. a destination matching the permanent-failure pattern → 400
//     destination_rejected (nothing recorded);
//  6. otherwise the message is accepted: a ledger entry with a new
//     message_id is committed, then (latency) 202. An injected timeout
//     commits the entry first and then stalls past the client's timeout,
//     so the client sees an ambiguous outcome while the message counts as
//     sent; a retry with the same key is reconciled by the replay.
//
// Durability: with Config.LedgerPath set (always, in cmd/mockprovider) every
// acceptance is committed to a framed, fsynced journal before 202 is
// returned (ledger.go), and the index is rebuilt from it at startup, so a
// replay works across process restarts and container recreation; an
// unreadable or corrupt journal stops the server from starting (fail
// closed); a storage failure answers 503 storage_unavailable and accepts
// nothing. The journal holds keys, channels, payload fingerprints and
// message ids only. Without a LedgerPath (unit tests) the index is in
// memory for the process lifetime. Message texts and destinations are kept
// only in memory, for the in-process test accessor Messages: they are never
// persisted, logged or returned by the admin endpoints.
package mockprovider

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"mime"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/fyusufayan/insider-campaign-composer-case/backend/internal/provider"
)

// Fault is an injected failure.
type Fault int

// Faults.
const (
	FaultNone Fault = iota
	// Fault5xx answers 503 before the ledger (nothing recorded).
	Fault5xx
	// Fault429 answers 429 with Retry-After before the ledger.
	Fault429
	// FaultTimeout commits the message, then stalls past the client timeout.
	FaultTimeout
	// FaultPermanent answers 400 destination_rejected (nothing recorded).
	FaultPermanent
)

// Request is what a fault function sees: the validated request.
type Request struct {
	Channel        provider.Channel
	IdempotencyKey string
	To             string
	// Arrival is the 1-based arrival number of this key (retries > 1).
	Arrival int
}

// FaultFunc decides the fault for one request; tests use it for
// deterministic scenarios. It runs after validation, before the ledger.
type FaultFunc func(Request) Fault

// Config configures the server. The zero value injects no faults and adds
// no latency.
type Config struct {
	Fault5xxRate     float64
	Fault429Rate     float64
	FaultTimeoutRate float64
	// LatencyMin/Max: uniform artificial latency before an accepted reply.
	LatencyMin, LatencyMax time.Duration
	// PermanentFailSuffix: destinations ending with it are rejected
	// permanently (e.g. "0000"; empty disables).
	PermanentFailSuffix string
	// TimeoutStall is how long an injected timeout stalls (must exceed the
	// client's request timeout); the stall ends early when the client
	// disconnects.
	TimeoutStall time.Duration
	// RetryAfter is the Retry-After of an injected 429 (whole seconds).
	RetryAfter time.Duration
	// Seed makes the random faults and latencies reproducible for a given
	// arrival order.
	Seed uint64
	// LedgerPath is the durable journal file (MOCK_LEDGER_PATH); empty keeps
	// the index in memory only (unit tests).
	LedgerPath string
	// AllowReset enables the destructive POST /admin/reset
	// (MOCK_ADMIN_RESET_ENABLED); otherwise it answers 403.
	AllowReset bool
}

// Validate checks the configuration.
func (c Config) Validate() error {
	for name, r := range map[string]float64{"5xx": c.Fault5xxRate, "429": c.Fault429Rate, "timeout": c.FaultTimeoutRate} {
		if r < 0 || r > 1 {
			return fmt.Errorf("mockprovider: %s fault rate must be in [0, 1]", name)
		}
	}
	if c.Fault5xxRate+c.Fault429Rate+c.FaultTimeoutRate > 1 {
		return errors.New("mockprovider: fault rates must sum to at most 1")
	}
	if c.LatencyMin < 0 || c.LatencyMax < c.LatencyMin {
		return errors.New("mockprovider: latency must satisfy 0 <= min <= max")
	}
	if c.TimeoutStall < 0 || c.RetryAfter < 0 {
		return errors.New("mockprovider: durations must not be negative")
	}
	return nil
}

// Request limits.
const (
	maxBodyBytes      = 64 << 10
	maxKeyLen         = 200
	maxSMSRunes       = 1600
	maxSubjectRunes   = 200
	maxEmailTextRunes = 20000
	maxEmailLen       = 254
)

var (
	e164Pattern  = regexp.MustCompile(`^\+[1-9][0-9]{7,14}$`)
	emailPattern = regexp.MustCompile(`^[^@\s]+@[^@\s]+\.[^@\s]+$`)
)

// Message is one accepted message (ledger entry). To, Subject and Text are
// known only for messages accepted by this process (they are not persisted).
type Message struct {
	Key       string
	Channel   provider.Channel
	To        string
	Subject   string
	Text      string
	MessageID string
}

type entry struct {
	msg  Message
	hash [32]byte
	seq  int64
}

// ChannelStats are the counters of one channel. They contain no keys,
// destinations or texts.
type ChannelStats struct {
	Arrivals         int64 `json:"arrivals"`
	Accepted         int64 `json:"accepted"`
	Replayed         int64 `json:"replayed"`
	RejectedReuse    int64 `json:"rejected_key_reuse"`
	RejectedInvalid  int64 `json:"rejected_invalid"`
	RejectedPerm     int64 `json:"rejected_permanent"`
	Injected5xx      int64 `json:"injected_5xx"`
	Injected429      int64 `json:"injected_429"`
	InjectedTimeouts int64 `json:"injected_timeouts"`
	// Messages is the number of ledger entries of the channel (distinct keys
	// ever accepted; durable when the journal is enabled).
	Messages int64 `json:"messages"`
	// StorageFailures counts acceptances refused because the journal could
	// not be written.
	StorageFailures int64 `json:"storage_failures"`
	// MaxArrivalsPerKey is the highest number of requests seen for one key.
	MaxArrivalsPerKey int `json:"max_arrivals_per_key"`
}

// Stats is the body of GET /admin/stats.
type Stats struct {
	Channels map[provider.Channel]ChannelStats `json:"channels"`
	// DuplicateAcceptedKeys counts keys that were accepted more than once as
	// a new message; it is 0 by construction of the ledger and reported so
	// that tests and the demo can assert it.
	DuplicateAcceptedKeys int64 `json:"duplicate_accepted_keys"`
	// Durable reports whether acceptances are journaled.
	Durable bool `json:"durable"`
	// KeyPrefix / PrefixMessages: ledger entries per channel whose key starts
	// with the requested prefix (e.g. "<campaign_id>:"), so one campaign can
	// be checked against a ledger that keeps earlier campaigns.
	KeyPrefix      string                     `json:"key_prefix,omitempty"`
	PrefixMessages map[provider.Channel]int64 `json:"prefix_messages,omitempty"`
}

// Server is the mock provider.
type Server struct {
	cfg    Config
	faults FaultFunc
	log    *slog.Logger

	mu       sync.Mutex
	rng      *rand.Rand
	ledger   map[string]entry // the index: committed acceptances
	journal  *ledger          // nil: in memory only
	arrivals map[string]int
	stats    map[provider.Channel]*ChannelStats
	seq      int64
	dupes    int64
}

// Option configures a Server.
type Option func(*Server)

// WithFaults replaces the random fault injection by fn (tests).
func WithFaults(fn FaultFunc) Option { return func(s *Server) { s.faults = fn } }

// New returns a server.
func New(cfg Config, log *slog.Logger, opts ...Option) (*Server, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if log == nil {
		return nil, errors.New("mockprovider: nil logger")
	}
	if cfg.RetryAfter == 0 {
		cfg.RetryAfter = time.Second
	}
	if cfg.TimeoutStall == 0 {
		cfg.TimeoutStall = 30 * time.Second
	}
	s := &Server{cfg: cfg, log: log}
	s.resetCounters()
	s.ledger = map[string]entry{}
	if cfg.LedgerPath != "" {
		j, records, rec, err := openLedger(cfg.LedgerPath)
		if err != nil {
			return nil, err
		}
		for _, r := range records {
			var h [32]byte
			if _, err := hex.Decode(h[:], []byte(r.Hash)); err != nil { // validated by parseLedger
				j.close()
				return nil, fmt.Errorf("%w: %v", ErrLedgerCorrupt, err)
			}
			s.ledger[r.Key] = entry{msg: Message{Key: r.Key, Channel: r.Channel, MessageID: r.MessageID}, hash: h, seq: r.Seq}
			s.stats[r.Channel].Messages++
		}
		s.journal, s.seq = j, int64(len(records))
		attrs := []any{"records", rec.Records, "created", rec.Created}
		if rec.TornTailBytes > 0 {
			log.Warn("idempotency ledger: truncated an unacknowledged partial record", append(attrs, "torn_tail_bytes", rec.TornTailBytes)...)
		} else {
			log.Info("idempotency ledger opened", attrs...)
		}
	}
	for _, o := range opts {
		o(s)
	}
	return s, nil
}

// Close releases the journal (and its lock). The handler must no longer be
// serving.
func (s *Server) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.journal == nil {
		return nil
	}
	err := s.journal.close()
	s.journal = nil
	s.ledger = map[string]entry{}
	return err
}

// resetCounters clears the per-process counters and the fault RNG.
func (s *Server) resetCounters() {
	s.rng = rand.New(rand.NewPCG(s.cfg.Seed, s.cfg.Seed^0x9e3779b97f4a7c15))
	s.arrivals = map[string]int{}
	s.stats = map[provider.Channel]*ChannelStats{provider.ChannelSMS: {}, provider.ChannelEmail: {}}
	s.dupes = 0
}

// resetAll destroys every accepted key (journal included) and the counters.
func (s *Server) resetAll() error {
	if s.journal != nil {
		if err := s.journal.reset(); err != nil {
			return err
		}
	}
	s.ledger = map[string]entry{}
	s.seq = 0
	s.resetCounters()
	return nil
}

// Handler returns the HTTP handler.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST "+provider.PathSMS, func(w http.ResponseWriter, r *http.Request) { s.handleSend(w, r, provider.ChannelSMS) })
	mux.HandleFunc("POST "+provider.PathEmail, func(w http.ResponseWriter, r *http.Request) { s.handleSend(w, r, provider.ChannelEmail) })
	mux.HandleFunc("GET /admin/stats", func(w http.ResponseWriter, r *http.Request) {
		prefix := r.URL.Query().Get("key_prefix")
		if prefix != "" && !prefixPattern.MatchString(prefix) {
			writeError(w, http.StatusBadRequest, "invalid_request", "key_prefix must be 1-64 characters of 0-9 a-f - :")
			return
		}
		writeJSON(w, http.StatusOK, s.StatsFor(prefix))
	})
	mux.HandleFunc("POST /admin/reset", func(w http.ResponseWriter, _ *http.Request) {
		if !s.cfg.AllowReset {
			writeError(w, http.StatusForbidden, "reset_disabled", "the destructive reset is disabled")
			return
		}
		s.mu.Lock()
		err := s.resetAll()
		s.mu.Unlock()
		if err != nil {
			s.log.Error("mock provider reset failed", "err", err)
			writeError(w, http.StatusServiceUnavailable, "storage_unavailable", "the idempotency ledger could not be reset")
			return
		}
		s.log.Warn("mock provider state reset: every accepted idempotency key was discarded")
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	return mux
}

// Stats returns a copy of the counters.
func (s *Server) Stats() Stats { return s.StatsFor("") }

// StatsFor returns the counters and, for a non-empty prefix, the number of
// ledger entries per channel whose key starts with it.
func (s *Server) StatsFor(prefix string) Stats {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := Stats{Channels: map[provider.Channel]ChannelStats{}, DuplicateAcceptedKeys: s.dupes, Durable: s.journal != nil}
	for ch, st := range s.stats {
		out.Channels[ch] = *st
	}
	if prefix != "" {
		out.KeyPrefix = prefix
		out.PrefixMessages = map[provider.Channel]int64{provider.ChannelSMS: 0, provider.ChannelEmail: 0}
		for k, e := range s.ledger {
			if strings.HasPrefix(k, prefix) {
				out.PrefixMessages[e.msg.Channel]++
			}
		}
	}
	return out
}

var prefixPattern = regexp.MustCompile(`^[0-9a-f:-]{1,64}$`)

// Messages returns the ledger (accepted messages) in acceptance order. It
// is an in-process accessor for tests; no HTTP endpoint exposes it.
func (s *Server) Messages() []Message {
	s.mu.Lock()
	defer s.mu.Unlock()
	es := make([]entry, 0, len(s.ledger))
	for _, e := range s.ledger {
		es = append(es, e)
	}
	slices.SortFunc(es, func(a, b entry) int { return int(a.seq - b.seq) })
	out := make([]Message, len(es))
	for i, e := range es {
		out[i] = e.msg
	}
	return out
}

// Arrivals returns how many requests arrived for key (tests).
func (s *Server) Arrivals(key string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.arrivals[key]
}

type parsed struct {
	to, subject, text string
}

func (s *Server) handleSend(w http.ResponseWriter, r *http.Request, ch provider.Channel) {
	key := r.Header.Get(provider.HeaderIdempotencyKey)
	s.mu.Lock()
	st := s.stats[ch]
	st.Arrivals++
	arrival := 0
	if validKey(key) {
		s.arrivals[key]++
		arrival = s.arrivals[key]
		if arrival > st.MaxArrivalsPerKey {
			st.MaxArrivalsPerKey = arrival
		}
	}
	s.mu.Unlock()

	p, problem := decodeRequest(r, ch, key)
	if problem != "" {
		s.count(ch, func(c *ChannelStats) { c.RejectedInvalid++ })
		s.log.Debug("mock request rejected", "channel", ch, "reason", problem)
		writeError(w, http.StatusBadRequest, "invalid_request", problem)
		return
	}

	fault := s.decideFault(Request{Channel: ch, IdempotencyKey: key, To: p.to, Arrival: arrival})
	switch fault {
	case Fault5xx:
		s.count(ch, func(c *ChannelStats) { c.Injected5xx++ })
		writeError(w, http.StatusServiceUnavailable, "unavailable", "injected failure")
		return
	case Fault429:
		s.count(ch, func(c *ChannelStats) { c.Injected429++ })
		w.Header().Set(provider.HeaderRetryAfter, strconv.Itoa(max(1, int(s.cfg.RetryAfter/time.Second))))
		writeError(w, http.StatusTooManyRequests, "rate_limited", "injected rate limit")
		return
	}

	hash := payloadHash(ch, p)
	s.mu.Lock()
	if e, ok := s.ledger[key]; ok {
		if e.hash == hash {
			st.Replayed++
			id := e.msg.MessageID
			s.mu.Unlock()
			w.Header().Set(provider.HeaderReplayed, "true")
			writeJSON(w, http.StatusOK, provider.AcceptedResponse{MessageID: id, Status: "accepted"})
			return
		}
		st.RejectedReuse++
		s.mu.Unlock()
		s.log.Warn("idempotency key reused with a different payload", "channel", ch)
		writeError(w, http.StatusUnprocessableEntity, provider.CodeIdempotencyKeyReuse,
			"the idempotency key was already used with a different payload")
		return
	}
	if fault == FaultPermanent || (s.cfg.PermanentFailSuffix != "" && strings.HasSuffix(p.to, s.cfg.PermanentFailSuffix)) {
		st.RejectedPerm++
		s.mu.Unlock()
		writeError(w, http.StatusBadRequest, "destination_rejected", "the destination is permanently rejected")
		return
	}
	seq := s.seq + 1
	id := fmt.Sprintf("mp_%s_%010d", ch, seq)
	if s.journal != nil {
		// Commit point: the journal frame is fsynced before the key enters
		// the index and before any success is returned (fail closed).
		if err := s.journal.append(ledgerRecord{Seq: seq, Key: key, Channel: ch, Hash: hashHex(hash), MessageID: id}); err != nil {
			st.StorageFailures++
			s.mu.Unlock()
			s.log.Error("acceptance refused: the idempotency ledger could not be written", "channel", ch, "err", err)
			writeError(w, http.StatusServiceUnavailable, "storage_unavailable", "the message could not be recorded")
			return
		}
	}
	s.seq = seq
	s.ledger[key] = entry{msg: Message{Key: key, Channel: ch, To: p.to, Subject: p.subject, Text: p.text, MessageID: id}, hash: hash, seq: seq}
	st.Accepted++
	st.Messages++
	latency := s.latencyLocked()
	if fault == FaultTimeout {
		st.InjectedTimeouts++
	}
	s.mu.Unlock()

	if fault == FaultTimeout {
		// Committed above; now stall so that the client gives up.
		sleep(r.Context(), s.cfg.TimeoutStall)
		return
	}
	if !sleep(r.Context(), latency) {
		return
	}
	writeJSON(w, http.StatusAccepted, provider.AcceptedResponse{MessageID: id, Status: "accepted"})
}

func (s *Server) count(ch provider.Channel, fn func(*ChannelStats)) {
	s.mu.Lock()
	fn(s.stats[ch])
	s.mu.Unlock()
}

func (s *Server) decideFault(req Request) Fault {
	if s.faults != nil {
		return s.faults(req)
	}
	s.mu.Lock()
	x := s.rng.Float64()
	s.mu.Unlock()
	switch {
	case x < s.cfg.Fault5xxRate:
		return Fault5xx
	case x < s.cfg.Fault5xxRate+s.cfg.Fault429Rate:
		return Fault429
	case x < s.cfg.Fault5xxRate+s.cfg.Fault429Rate+s.cfg.FaultTimeoutRate:
		return FaultTimeout
	}
	return FaultNone
}

func (s *Server) latencyLocked() time.Duration {
	if s.cfg.LatencyMax <= s.cfg.LatencyMin {
		return s.cfg.LatencyMin
	}
	return s.cfg.LatencyMin + time.Duration(s.rng.Int64N(int64(s.cfg.LatencyMax-s.cfg.LatencyMin)+1))
}

// sleep waits d or until ctx is done; it reports whether d elapsed.
func sleep(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-ctx.Done():
		return false
	}
}

func validKey(k string) bool {
	if k == "" || len(k) > maxKeyLen {
		return false
	}
	for i := 0; i < len(k); i++ {
		if k[i] < 0x21 || k[i] > 0x7e {
			return false
		}
	}
	return true
}

// decodeRequest validates one request; problem is a fixed text (never an
// echo of the input).
func decodeRequest(r *http.Request, ch provider.Channel, key string) (parsed, string) {
	if !validKey(key) {
		return parsed{}, "Idempotency-Key header must be 1-200 printable ASCII characters"
	}
	if mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err != nil || mt != "application/json" {
		return parsed{}, "Content-Type must be application/json"
	}
	data, err := io.ReadAll(io.LimitReader(r.Body, maxBodyBytes+1))
	if err != nil || len(data) > maxBodyBytes {
		return parsed{}, "request body is unreadable or too large"
	}
	if !utf8.Valid(data) {
		return parsed{}, "request body must be UTF-8"
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var p parsed
	switch ch {
	case provider.ChannelSMS:
		var req provider.SMSRequest
		if err := dec.Decode(&req); err != nil {
			return parsed{}, "request body must be a JSON object with to and text"
		}
		p = parsed{to: req.To, text: req.Text}
	default:
		var req provider.EmailRequest
		if err := dec.Decode(&req); err != nil {
			return parsed{}, "request body must be a JSON object with to, subject and text"
		}
		p = parsed{to: req.To, subject: req.Subject, text: req.Text}
	}
	if dec.More() {
		return parsed{}, "request body must contain exactly one JSON object"
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return parsed{}, "request body must contain exactly one JSON object"
	}
	switch ch {
	case provider.ChannelSMS:
		if !e164Pattern.MatchString(p.to) {
			return parsed{}, "to must be an E.164 phone number"
		}
		if n := utf8.RuneCountInString(p.text); n == 0 || n > maxSMSRunes {
			return parsed{}, "text must be 1-1600 characters"
		}
	default:
		if len(p.to) > maxEmailLen || !emailPattern.MatchString(p.to) {
			return parsed{}, "to must be an email address"
		}
		if n := utf8.RuneCountInString(p.subject); n == 0 || n > maxSubjectRunes || strings.ContainsAny(p.subject, "\r\n") {
			return parsed{}, "subject must be 1-200 characters on one line"
		}
		if n := utf8.RuneCountInString(p.text); n == 0 || n > maxEmailTextRunes {
			return parsed{}, "text must be 1-20000 characters"
		}
	}
	return p, ""
}

// payloadHash identifies the payload semantics of a request: channel,
// destination, subject and text (length-prefixed, so field boundaries are
// unambiguous).
func payloadHash(ch provider.Channel, p parsed) [32]byte {
	h := sha256.New()
	for _, f := range []string{string(ch), p.to, p.subject, p.text} {
		_, _ = fmt.Fprintf(h, "%d:%s;", len(f), f)
	}
	var out [32]byte
	copy(out[:], h.Sum(nil))
	return out
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, code, msg string) {
	var e provider.ErrorResponse
	e.Error.Code, e.Error.Message = code, msg
	writeJSON(w, status, e)
}
