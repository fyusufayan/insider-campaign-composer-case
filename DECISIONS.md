# Architectural Decisions

Each decision uses: **Context · Alternatives · Decision · Reasoning · Trade-offs**.
Details of every mechanism live in `docs/ARCHITECTURE.md` (section numbers referenced as §n).

---

## ADR-001 — Modular monolith with separate `api` and `worker` processes

**Context.** Seven-day deadline, one developer, but two very different runtime profiles: interactive HTTP/SSE with long LLM streams, and background delivery with strict rate and reliability requirements.

**Alternatives.** (a) Single process doing both. (b) Microservices (campaign, segmentation, AI, delivery). (c) Modular monolith with several entrypoints.

**Decision.** One Go module and one Docker image with entrypoints `api`, `worker`, `mockprovider`, `seed` and the one-shot `migrate` (applies the embedded migrations; added in CP8 so Compose needs no second image). Code is organized by feature package (`segment`, `content`, `ai`, `llm`, `approval`, `delivery`, …); `httpapi` holds only transport concerns.

**Reasoning.** Separate processes isolate failures (an LLM stall cannot starve dispatch; a worker crash does not take down the UI) without distributed-system costs. Feature packages give clear boundaries and testability.

**Trade-offs.** Boundaries are enforced by convention and import rules, not by deployment. Components cannot be scaled independently at a finer grain than process.

---

## ADR-002 — PostgreSQL as the only infrastructure; the delivery row is the job

**Context.** Delivery must be asynchronous, reliable, crash-tolerant and observable. The brief asks to evaluate PostgreSQL before adding Redis.

**Alternatives.** (a) Redis + asynq/streams. (b) River (Go job library on PostgreSQL). (c) Generic jobs table + separate deliveries table. (d) `campaign_deliveries` itself as the queue, claimed with `FOR UPDATE SKIP LOCKED`.

**Decision.** (d). Each row is both the snapshot entry and the job; workers claim batches with `SKIP LOCKED`, own them via leases, and complete them with guarded updates.

**Reasoning.** Releasing a snapshot to the queue is a single `UPDATE` inside the same transaction that consumes the approval — no dual write, no outbox. One table is the single source of truth for status and statistics. Required throughput (≤200 req/s) is trivial for PostgreSQL. No new infrastructure to operate.

**Trade-offs.** Polling latency (sub-second); we own ~300 lines of queue code; at much larger scale write amplification on one table would matter. River is the natural migration path.

---

## ADR-003 — Typed, shallow segmentation DSL compiled through a registry; the LLM never writes SQL

**Context.** LLM output is untrusted. The requirement forbids executing LLM-generated SQL and demands an allow-list of fields and operators.

**Alternatives.** (a) LLM generates SQL validated by a parser/sandbox. (b) Generic `{field, op, value}` triples. (c) Typed predicates (attribute / event / behavior) with a registry.

**Decision.** (c), shallow (top-level match + non-nesting groups). Each registry entry owns its allowed operators, value validator and SQL fragment builder; identifiers come from Go constants, values are bind parameters. The prompt's field catalog and the JSON schema are generated from the same registry.

**Reasoning.** The attack surface is exactly the allow-list. Behavioural conditions (events in a window) need different shapes than attributes; typing them makes validation and compilation simple. Structured-outputs schemas cannot be recursive, so shallowness is also a platform constraint. Generating prompt and schema from the registry prevents drift.

**Trade-offs.** Less expressive; every new segmentation capability requires code (intentionally).

---

## ADR-004 — Consent and contact are enforced by the backend, outside the DSL, with an ordered exclusive partition

**Context.** Consent is a legal/business invariant and must not depend on LLM output. Exclusion statistics in the earlier draft double-counted users lacking both contact and consent.

**Alternatives.** (a) Consent as a DSL field. (b) Independent overlapping metrics (`no_consent`, `no_contact`). (c) Backend-applied eligibility with an ordered, mutually exclusive classification.

**Decision.** (c). Per selected channel each matched user is exactly one of `no_contact` → `no_consent` → `eligible` (evaluated in that order). Cross-channel reach is a second partition (`both`, `sms_only`, `email_only`, `unreachable`). Users and messages are reported in separate units. DSL attempts to reference consent are rejected.

**Reasoning.** Categories sum to the matched total by construction, so nothing can be double-counted. Contact comes first because consent is irrelevant without an address.

**Trade-offs.** The order hides how many contact-less users also lack consent; that number is not needed for any decision.

---

## ADR-005 — Approval is bound to a prepared snapshot (prepare → confirm); no separate snapshot table

**Context.** An audience computed at send time can differ from the one the marketer reviewed. The approval must bind segment version, content version, channels and the audience snapshot.

**Alternatives.** (a) Snapshot at send time. (b) Snapshot created in the confirm transaction (after the click). (c) `campaign_audience_snapshots` + member table, copied into deliveries at send. (d) Prepare creates the approval (pending) and the snapshot as `held` delivery rows; confirm binds to that approval; send flips `held → pending`.

**Decision.** (d), with `campaign_approvals` holding the binding (segment version + frozen rule, content id, channels, partition statistics, count, digest, expiry).

**Reasoning.** Only a snapshot that exists *before* the confirm click is exactly what the marketer saw; (b) just moves the race by seconds. Snapshot entries and delivery jobs are 1:1 with the same key, so a separate table would duplicate every row and add a copy step that can diverge. Prepare is the only code path that inserts delivery rows, which makes the invariant "never send outside the approved snapshot" structural.

**Trade-offs.** The queue table contains not-yet-authorized rows (mitigated: claim filters by status and joins to a consumed approval of a sending campaign). Held rows are deleted when an approval is revoked or expires; the approval row keeps counts and digest for audit.

---

## ADR-006 — Approval expiry: pending 15 min, approved 24 h, enforced in the consuming transaction

**Context.** A stale approval must never be confirmed, consumed or dispatched; campaign state and held rows must stay consistent on expiry.

**Alternatives.** (a) Sweeper only. (b) Check on read only. (c) Expiry condition inside every consuming CAS, plus a sweeper and lazy expiry for state hygiene. For the time source of the comparison: (i) `now()`/`transaction_timestamp()`, (ii) a timestamp computed in Go, (iii) `clock_timestamp()` evaluated after the lock.

**Decision.** (c) with (iii). Every campaign/approval transition locks the campaign row first; expiry is checked only after the lock is acquired, using `clock_timestamp()`. `confirm` and `send` include `expires_at > clock_timestamp()` in their CAS. `approval.ExpireDue` (worker sweeper every 30 s, and lazily at the start of API operations) locks the campaign row, then moves the approval to `expired` if `expires_at <= clock_timestamp()`, deletes its held rows and resets the campaign to `content_ready`, all in one transaction. Expiry-defining timestamps (`prepared_at`, `approved_at`, `expires_at`) are also taken from `clock_timestamp()` after the lock.

**Reasoning.** `now()` is fixed at transaction start: a transaction that began before expiry, waited on the campaign lock and acquired it after expiry would still see a pre-expiry time and could consume an expired approval. `clock_timestamp()` returns the actual database time at the comparison point; combined with the lock (which serializes consume and expire for one campaign), exactly one of them can win, and it is decided by real time. A Go-side timestamp (ii) would reintroduce cross-container clock skew and is equally stale after a lock wait. Correctness never depends on sweeper timing; the sweeper only keeps visible state tidy. Consumed approvals never expire, so a running send is unaffected. A PostgreSQL integration test reproduces the lock-wait scenario (lock held across expiry, consumption must be rejected).

**Trade-offs.** Different time functions coexist in the codebase (`clock_timestamp()` for expiry, `now()` elsewhere); this is enforced by a CLAUDE.md invariant and code review. The remaining boundary is sub-millisecond: an approval is valid if `expires_at` is later than the database clock at the moment the CAS statement evaluates it.

---

## ADR-007 — No audience re-evaluation during sending; personalization frozen, destination resolved at first dispatch

**Context.** After approval, user data changes: rules may stop matching, names and contacts may change.

**Alternatives.** (a) Re-run the rule at dispatch. (b) Freeze everything at prepare. (c) Freeze membership and personalization; resolve the destination from current contact at the first attempt and persist it.

**Decision.** (c). Membership is never re-evaluated (snapshot may only shrink via consent/contact checks). `vars` are frozen at prepare (they are part of what was approved). The destination is taken from current contact at the first precheck, persisted before the HTTP request, and reused verbatim by all retries.

**Reasoning.** Freezing membership is the approval contract. Routing must respect the user's latest contact data at the moment we first send. Reusing the persisted destination keeps every retry byte-identical under the same idempotency key.

**Trade-offs.** Up to 24 h staleness: users who purchased after approval may still receive a cart reminder. A retry goes to the persisted address even if the user changed it mid-retry.

---

## ADR-008 — Consent re-checked on every attempt in a short precheck; no locks across HTTP

**Context.** A revocation can race with a dispatch. Holding locks or transactions across network calls is undesirable.

**Alternatives.** (a) Check at claim time. (b) Hold a `FOR SHARE` lock on the consent row across the HTTP request. (c) Single-statement precheck immediately before the request, after acquiring the rate-limit slot, with a bounded slot-to-request gap.

**Decision.** (c). Sequence: `Acquire slot → precheck (fence lease, campaign/approval status, consent, contact, persist destination) → final deadline gate → request`. The precheck has its own budget (`DISPATCH_MAX_PRECHECK`, 15 ms, enforced as a context deadline); the final gate, the last statement before `http.Client.Do`, aborts if `DISPATCH_MAX_SLOT_TO_REQUEST` (20 ms) since the slot was granted has elapsed. No I/O between precheck and request.

**Reasoning.** (a) would widen the window to include limiter waits and 429 pauses. (b) only serializes the revocation *after* the send — the message still goes out — while pinning connections and blocking the consent write path. (c) gives the strongest practical guarantee: any revocation committed before the precheck starts is honoured, and no further attempts follow. The final gate also bounds the window when the process is delayed after the precheck.

**Trade-offs.** One in-flight attempt can still be sent if the revocation commits inside a window of ≤ 20 ms (precheck snapshot to `Do`) plus transit (bounded by the 5 s request timeout). Documented explicitly.

---

## ADR-009 — At-least-once requests with a business-level idempotency key; uncertainty is recorded, not hidden

**Context.** "Never send the same message twice" must hold across worker crashes and provider timeouts. Exactly-once against an external provider is not achievable by us alone.

**Alternatives.** (a) At-most-once (mark sent before calling — loses messages on crash). (b) Claim exactly-once. (c) At-least-once with layered dedupe and provider idempotency keys.

**Decision.** (c). Layers: DB uniqueness per (campaign, user, channel); one consumable approval per campaign; `SKIP LOCKED` + lease + fencing; guarded completion; idempotency key `campaign_id:user_id:channel` with byte-identical retries. Rows that end non-`sent` after an ambiguous attempt are flagged `outcome_uncertain`.

Each provider adapter explicitly declares its idempotency capability and key retention (`provider.Capabilities{IdempotencyKeys, KeyRetention}`); the worker computes the retry horizon `H` from configuration (≈ 12 min with defaults) and logs/reports the effective guarantee.

**Reasoning.** The key mirrors the business uniqueness rule, so even a hypothetical duplicate row would collide at the provider. **At most one delivered message per recipient and channel is claimed only for a provider that implements idempotency keys and retains them for at least `H`** — the mock provider does, and its tests cover replay, key-reuse rejection and reconciliation after ambiguous responses. For any other provider the claim is at-least-once. Honest reporting of ambiguous outcomes is preferable to a false "exactly-once" or unconditional "at-most-once" claim.

**Trade-offs.** The strongest property depends on provider behaviour we do not control in production; a real adapter must document it. Worker downtime longer than the provider's key retention while a retry is pending can still cause a duplicate. Our recorded status can lag provider truth until the next attempt reconciles it.

---

## ADR-010 — Leases, fencing precheck and reaper for crash recovery

**Context.** Workers can crash or pause mid-dispatch.

**Alternatives.** (a) Long transactions holding row locks during sends. (b) Heartbeating leases. (c) Fixed leases renewed once at precheck, much longer than the request timeout, plus a reaper.

**Decision.** (c). Claim lease 60 s; precheck renews it and verifies ownership (`leased_by`); request timeout 5 s; reaper every 10 s returns expired rows to the queue (recording an `unknown` attempt if a request may have been sent).

**Reasoning.** No connection is held during network I/O; ownership is re-verified immediately before each request; a worker would need to stall for ≈55 s after precheck to overlap with another owner, and even then the idempotency key deduplicates.

**Trade-offs.** Recovery after a crash takes up to one lease period plus the reaper interval.

---

## ADR-011 — Per-channel sliding-window-log rate limiter with a configurable safety margin; one worker replica

**Context.** Requirement: at most 100 provider requests per second per channel, counting retries; 429 must pause the affected channel. A token bucket with burst 10 allows 110 in a rolling second.

**Alternatives.** (a) Token bucket (burst > 1 violates; burst 1 is borderline and hard to reason about). (b) Fixed window (allows 200 across a boundary). (c) Sliding-window log. (d) Distributed limiter in PostgreSQL.

**Decision.** (c): per channel a mutex-protected ring of the last N=100 acquisition times; a slot is granted only if the 100th-previous acquisition is at least `W = 1 s + RATE_SAFETY_MARGIN` old. `Pause(until)` affects only that channel. Acquisition happens inside the only function that sends provider requests. One worker replica.

A request is never issued after its slot deadline: `gatedSender.Attempt` aborts (slot wasted, never refunded) if the precheck exceeds `DISPATCH_MAX_PRECHECK` or if, at the final gate immediately before `http.Client.Do`, more than `DISPATCH_MAX_SLOT_TO_REQUEST` has elapsed since the slot was granted (this also catches process delays after the precheck). Startup requires `DISPATCH_MAX_PRECHECK < DISPATCH_MAX_SLOT_TO_REQUEST < RATE_SAFETY_MARGIN` (defaults 15 ms < 20 ms < 50 ms).

**Reasoning.** Exact rolling-window semantics, atomic under concurrency, ~40 lines, deterministic with a fake clock. Because the provider observes *arrivals*, the margin must cover the enforced slot-to-request bound (20 ms) plus transport jitter (assumed ≤ 30 ms on the local Docker network); the integration test measures provider-side arrival times. Wasting a slot is cheaper than risking the rate guarantee.

**Trade-offs.** ~5 % throughput given up to the margin, plus occasional wasted slots. Application code cannot detect stalls after the final gate (inside the HTTP transport, runtime or kernel); those are covered only by the margin under the documented network assumption — **not a universal guarantee** for arbitrary environments. Horizontal worker scaling would require a shared limiter (out of scope).

---

## ADR-012 — Classified retry policy with exponential backoff, full jitter and a separate 429 budget

**Context.** Provider failures differ: throttling, transient errors, ambiguous timeouts, permanent rejections.

**Alternatives.** (a) Uniform retry count. (b) Classified outcomes with separate budgets.

**Decision.** 2xx/replay → sent; 429 → pause channel, retry at `Retry-After` (≤10 rate-limited attempts); 5xx/connection/timeout → backoff `min(60 s, 1 s·2^(n−1))` with full jitter (≤5 attempts); other 4xx → failed. Implemented as a pure function.

**Reasoning.** Throttling storms should not burn the failure budget; permanent errors should not be retried; jitter avoids synchronized retries. A pure function is trivial to test exhaustively.

**Trade-offs.** Worst-case time to a terminal state is several minutes.

---

## ADR-013 — SMS validation by worst-case rune count after NFC; encoding as warnings

**Context.** The assignment requires SMS ≤ 160 characters with personalization. Real SMS encoding (GSM-7/UCS-2, segments) is more complex.

**Alternatives.** (a) Count template characters only. (b) Full telecom encoding model as the hard rule. (c) Hard rule on worst-case Unicode code points; encoding and segments as advisory warnings.

**Decision.** (c). NFC-normalize; worst case = literal runes + per-variable declared maximum (or fallback length if longer); hard limits SMS 160, subject 80, body 5,000. Declared maxima are enforced by DB CHECK constraints. GSM-7/UCS-2 detection and segment counts are warnings. The renderer asserts actual length.

**Reasoning.** Meets the stated requirement exactly and deterministically before approval, while informing the marketer about cost-relevant encoding without building a telecom stack.

**Trade-offs.** Conservative (rejects some messages that would fit for real names); no Turkish national shift table.

---

## ADR-014 — Official Anthropic Go SDK v2 with stable structured outputs and one repair turn

**Context.** Need reliable structured JSON, streaming, retries and API-key configuration from `ANTHROPIC_API_KEY`.

**Alternatives.** (a) Raw HTTP. (b) SDK v1 (older API version). (c) SDK v2 (`github.com/anthropics/anthropic-sdk-go/v2`, API `2026-10-21`). For JSON: prompt-only JSON, forced tool use, or `output_config.format`.

**Decision.** SDK v2 pinned at v2.5.0 (Go 1.25+). JSON via `OutputConfig{Format: JSONOutputFormatParam{Schema: …}}` on the stable Messages API, strict local decode, semantic validation, and at most one repair turn. SDK retries (2) handle transport errors.

**Reasoning.** Maintained, typed streaming (`NewStreaming` + `Accumulate`), typed errors (`*anthropic.Error`), built-in retries. Forced tool use is rejected by the default model; structured outputs remove most syntax failures; the schema cannot express ranges/lengths, so local validation remains mandatory. One repair turn bounds cost and latency.

**Trade-offs.** Coupling to SDK types is confined to `llm/anthropic.go`. v2.5.0 is very recent; fallback to v2.4.0 if issues arise. Server-side refusal fallbacks (beta API) are not used. The adapter disables the SDK's environment credential autoload (`option.WithoutEnvironmentDefaults`) so `ANTHROPIC_API_KEY` from our config is the only credential, sets no per-attempt request timeout (a timed-out attempt would be retried and re-billed; the interpreter's single deadline bounds all attempts) and sends adaptive thinking explicitly (implemented in CP9; details in ARCHITECTURE §7.1).

---

## ADR-015 — Model selection per task via environment; `claude-opus-5-5` by default; Day 2 evaluation

**Context.** Interpretation and content generation have different quality/latency profiles. The model must not be hardcoded.

**Alternatives.** (a) One global model constant. (b) Per-task model and effort via environment variables.

**Decision.** (b): `CLAUDE_SEGMENT_MODEL`, `CLAUDE_CONTENT_MODEL` (both default `claude-opus-5-5`), effort `low`/`medium` respectively. A Day 2 evaluation (12 briefs) compares `claude-sonnet-5-5` per task; switching is an env change.

**Reasoning.** Quality and predictable structured output are prioritized; costs per campaign are cents. Measurement, not assumption, decides any switch.

**Trade-offs.** Opus 5.5 thinking cannot be disabled, adding latency before the first streamed token (mitigated by a "thinking" status in the UI). The evaluation spends a small amount of real API budget.

---

## ADR-016 — LLM abstraction at the client level with scripted and demo mocks

**Context.** Tests must run without an API key; the app should be demonstrable without one.

**Alternatives.** (a) Mock at the service level (`Interpreter`, `Generator`). (b) Mock `llm.Client` (Complete/Stream).

**Decision.** (b). `scripted` returns queued responses, errors and chunked streams for tests; `demo` returns deterministic keyword-based results with simulated streaming for running the app. `LLM_PROVIDER` (`auto` default | `anthropic` | `demo`) is resolved **once at startup**: `auto` → `anthropic` if `ANTHROPIC_API_KEY` is non-empty, otherwise `demo` (with a warning and a visible "Mock AI" badge); `anthropic` without a key → startup error; `demo` → always demo. When Anthropic is selected, failures are returned as actionable errors and **never** replaced by demo output. Unit tests construct `scripted` explicitly and never read `LLM_PROVIDER`.

**Reasoning.** Tests exercise the real prompt assembly, decoding, validation, repair loop and stream extractor — the code most likely to break. A startup-time, immutable selection makes the provider in use observable and predictable; a silent runtime fallback would present fabricated "AI" results as real ones and hide outages.

**Trade-offs.** Mocks do not measure real model quality; the Day 2 evaluation and an optional live smoke test cover that. An Anthropic outage makes AI features unavailable instead of degrading to demo output (intended).

---

## ADR-017 — SSE for streaming: fetch-stream for generation, snapshot events with DB polling for status

**Context.** Two one-way streams: LLM content generation and live delivery statistics.

**Alternatives.** (a) WebSockets. (b) Client polling. (c) SSE; for status either LISTEN/NOTIFY or periodic DB snapshots.

**Decision.** Generation: `POST` returning `text/event-stream`, read via `fetch` + `ReadableStream`, not resumable. Status: `EventSource` receiving full statistics snapshots every second from a DB query.

**Reasoning.** One-way traffic fits SSE; snapshot events make reconnection trivial and work with multiple API instances; polling cannot miss events and is cheap at this scale.

**Trade-offs.** Up to 1 s latency; one aggregate query per open stream per second; a dropped generation must be restarted.

---

## ADR-018 — Custom `{{var}}` templating

**Context.** Personalization of AI-written text.

**Alternatives.** Go `text/template`; Mustache libraries; custom parser.

**Decision.** Minimal custom parser/renderer with a variable allow-list, optional fallback, and length metadata.

**Reasoning.** `text/template` would let LLM-written text execute template actions; we need an allow-list and worst-case length computation anyway.

**Trade-offs.** No conditionals or loops (not required).

---

## ADR-019 — pgx with handwritten SQL and the standard-library router

**Context.** Data access and HTTP routing.

**Alternatives.** ORM (GORM), sqlc, chi/gin.

**Decision.** `pgx/v5` + `pgxpool`, handwritten SQL in repository files, `net/http` `ServeMux` (method patterns), `golang-migrate` for migrations.

**Reasoning.** The critical code is SQL (queue claims, CAS transitions, compiled segments); explicit SQL keeps it reviewable. Minimal dependencies.

**Trade-offs.** More boilerplate in repositories.

---

## ADR-020 — Mock provider as a separate HTTP service with fault injection, idempotency ledger and arrival recorder

**Context.** 429 handling, timeouts, idempotency and rate limits are only meaningful across a real network boundary and must be verifiable.

**Alternatives.** In-process fake only; separate HTTP service.

**Decision.** `cmd/mockprovider`: `POST /v1/sms`, `/v1/email`; env-configured faults (5xx, 429 with `Retry-After`, timeouts that commit before sleeping, permanent failures by number pattern, latency); idempotency ledger (replay with original id, 422 on key reuse with different payload); its own strict 100/s sliding window; arrival-time recording with `max_in_any_rolling_1s` per channel. Its adapter declares `IdempotencyKeys = true`, retention = process lifetime (ADR-009). An in-process fake remains for unit tests. Tests must cover: same key + same payload → replay; same key + different payload → rejection; retry after an ambiguous response → reconciled via the original key.

**Reasoning.** Lets tests and the demo prove the delivery and rate guarantees from the provider's point of view.

**Trade-offs.** One extra container; the ledger is in memory.

---

## ADR-021 — Campaign cancellation is core

**Context.** Defining consistent transitions for approvals and deliveries requires a cancellation path; a send system needs a stop button.

**Alternatives.** Defer cancellation (B); implement minimal cancellation now.

**Decision.** Core. Before sending: approval revoked (`campaign_cancelled`) + held rows deleted. During sending: pending/retry rows → `cancelled`; in-flight rows finish truthfully; precheck refuses to send for a non-sending campaign.

**Reasoning.** One endpoint and two statements; it closes otherwise undefined transitions and uses the same race boundary as consent revocation.

**Trade-offs.** Slight scope increase; an attempt already past precheck is still sent.

---

## ADR-022 — Scope tiers

**Context.** A strong but not overengineered result within seven days.

**Decision.** Core (A), quality (B) and stretch (C) tiers as listed in `docs/ROADMAP.md`. Notable placements: approval expiry, cancellation, consent precheck, `outcome_uncertain`, fault-injecting mock provider, repair turn, Day 2 model evaluation → A. GSM/UCS-2 warnings, Playwright, slim LLM audit table → B. Form-based segment editor, property-based compiler test, LISTEN/NOTIFY → C. Distributed rate limiting, auth, tracing → out of scope.

**Reasoning.** Core contains everything the requirements demand plus the mechanisms needed to *prove* reliability claims. Items that improve polish or catch rarer bugs follow only when core is complete.

**Trade-offs.** Some valuable test techniques (property-based testing) are deferred in favour of fixture-based tests with hand-computed expectations.
