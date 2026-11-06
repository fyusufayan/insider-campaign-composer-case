# Seven-Day Implementation Roadmap (vertical slices)

Every slice ends with a working end-to-end result in the browser, its tests, and demonstrated acceptance criteria. Backend and frontend evolve together. Section numbers (§) refer to `docs/ARCHITECTURE.md`.

## Scope tiers

**A — Core (must ship)**
- Brief → interpretation (Anthropic SDK v2 + scripted + demo mocks), structured outputs, strict decode, semantic validation, one repair turn, refusal/truncation/timeout handling.
- Typed DSL, registry, validator, compiler, Turkish-aware city normalization, exclusive audience partition.
- Day 2 model evaluation script (12 briefs, Opus 5.5 vs Sonnet 5.5, per task).
- Streamed content generation with field extractor; content versions (ai/edited); worst-case rune validation (160/80/5,000) with NFC and variable allow-list.
- Prepare/confirm/withdraw approval with held snapshot; approval expiry (15 min / 24 h) incl. sweeper and lazy expiry; campaign cancellation (pre-send and mid-send).
- Postgres queue: claim, lease, fencing precheck (consent + contact + destination persistence), gated sender, sliding-window limiter per channel, 429 pause, classified retries, reaper, completion detection, graceful shutdown, `outcome_uncertain`.
- Mock provider: faults, 429 + `Retry-After`, idempotency ledger, strict limiter, arrival recorder and rolling-rate stats.
- Live dashboard via SSE snapshots; `/healthz`, `/readyz`, `/metrics` with the 6 core metrics.
- Unit + integration tests per `CLAUDE.md`; one Go API-level end-to-end test; Docker Compose; README; DECISIONS.md.

**B — Quality (in this order, if time allows)**
1. GSM-7/UCS-2 and segment-count warnings.
2. Playwright happy-path E2E against Compose with the demo LLM.
3. Slim `llm_calls` audit table (core keeps model/prompt version on results + structured logs).
4. Masked sample users in the audience card.
5. Server-side refusal fallback (beta API path) behind a flag.
6. Snapshot digest displayed in the approval dialog.

**C — Stretch (only after A and B)**
- Form-based segment editor (`PUT /segment`, stateless preview endpoint).
- Property-based compiler-vs-in-memory-evaluator test.
- Re-evaluation of the rule at dispatch (could only remove recipients).
- LISTEN/NOTIFY for status streams; prompt caching.

**Out of scope (documented only)**: distributed rate limiting / multiple worker replicas, authentication/RBAC, tracing, Grafana dashboards, frequency capping, quiet hours, scheduling, real unsubscribe handling.

---

## Day 1 — Slice 1a: Brief → mock interpretation → audience preview

**Objective.** Thinnest complete path: Vue brief input → Go API → demo LLM → segment validation → PostgreSQL → audience partition in Vue.

**Tasks**
- Compose: `postgres`, `migrate`, `seed`, `api`, `frontend` (nginx with SSE-safe proxy). Makefile targets `up`, `test`, `test-integration`, `seed`.
- Full schema migration (§4). Deterministic seed (5,000 users, ~30 % İstanbul with mixed spellings in source data, consent mix, missing contacts, Turkish names within CHECK limits, 90 days of carts/purchases).
- `platform` (config validation, pgx pool, slog, health/readiness).
- `segment`: DSL types + strict decode, registry, validator, normalizer, compiler, partition query.
- `llm` interface + `demo` implementation; `ai.Interpreter` (prompt v1, schema generated from registry, decode, validate, repair loop — exercised with `scripted`).
- `campaign` create/get/interpret; `GET /audience`; `PUT /channels`.
- Frontend: Vite scaffold, typed API client, `BriefInput`, `InterpretationCard`, `SegmentRuleView`, `AudienceCard` with channel checkboxes.

**Tests.** Validator tables, compiler golden files, fixture-based counts (30-user fixture with hand-computed expectations), partition identities, interpreter with `scripted` (ok / repair ok / repair fails), API test create → interpret, Vitest for `AudienceCard`.

**Acceptance criteria**
- `docker compose up` from a fresh clone with no API key; entering the example brief shows rule chips (City is İstanbul · Abandoned cart in last 30 days), assumptions, and per-channel `eligible / no_consent / no_contact` that sum to `matched_users`.
- A scripted invalid rule produces field-level errors visible in the UI; no SQL is executed for it.
- `go test ./...` and `npm test` pass without a key.

**Depends on.** Nothing.

---

## Day 2 — Slice 1b: Real Claude interpretation + model evaluation

**Objective.** Production-grade interpretation with the real model and a data-driven model decision.

**Tasks**
- `llm/anthropic.go`: SDK v2 client, `OutputConfig` JSON schema + effort, timeouts, `WithMaxRetries`, `*anthropic.Error` mapping, `stop_reason` handling (refusal, max_tokens), usage/latency logging, `LLM_PROVIDER=auto`.
- Error UX in Vue (retryable vs. not, refusal, invalid output with raw proposal).
- `make eval-segment MODEL=…`: 12 briefs (incl. ambiguous and unsupported ones) with expected normalized rules; reports exact-match, validation failures, repair rate, latency. Run for `claude-haiku-5-5` (interpretation default since CP10) and `claude-sonnet-5-5` **after confirming the spend**.
- Core metrics `llm_requests_total`, `llm_request_duration_seconds`.

**Tests.** Scripted refusal/truncation/timeout/cancellation; error mapping unit tests; provider-selection table (`auto`/`anthropic`/`demo` × key present/absent; `anthropic` without key fails startup); Anthropic failure returns an error and never demo output; optional `make smoke-claude`.

**Acceptance criteria**
- With a key, the real model yields a valid rule for the example brief and for 3 ambiguous briefs, with meaningful `assumptions`/`unsupported`.
- Refusal, timeout and invalid output each show an actionable message; the campaign remains editable; with a key configured, an Anthropic outage never produces demo output.
- Evaluation results recorded in `docs/model-evaluation.md`; `CLAUDE_SEGMENT_MODEL` default confirmed or changed via env (decision noted in ADR-015).

**Depends on.** Slice 1a.

---

## Day 3 — Slice 2: Content generation and streaming

**Objective.** Streamed, validated SMS and email content in the UI.

**Tasks**
- `content`: template parser/renderer with fallbacks, variable registry (declared maxima), NFC, validator (§8), rendered example.
- `ai.Generator`: prompt v1, schema, streaming via `llm.Stream`, `jsonstream` extractor, final validation, content version (`origin=ai`, model, prompt version).
- `POST /content/generations` (SSE: `meta`, `status`, `delta`, `validated`, `error`), `POST /content/validate`.
- Frontend: `useContentStream`, `ContentStreamPanel` (thinking state, abort), `SmsPreview` (worst-case counter), `EmailPreview` (sandboxed iframe + footer), `ValidationList`.
- Run the content half of the model evaluation (subjective rubric + limit compliance) after confirming spend.

**Tests.** Extractor fuzz test; content validator tables (limits, fallbacks, multi-byte, NFC, unknown variables); generator with scripted streams (split chunks, truncated stream, invalid final JSON → repair); SSE handler test; Vitest for the SSE parser and `SmsPreview`.

**Acceptance criteria**
- Generate streams text into SMS, subject and body live (demo and real model); the validated version then replaces the preview.
- An SMS whose worst-case length exceeds 160 shows a blocking error; Turkish text shows the worst-case count.
- Closing the tab during generation cancels the LLM call (visible in logs).

**Depends on.** Slice 1b.

---

## Day 4 — Slice 3: Editing, approval, snapshot, expiry, pre-send cancellation

**Objective.** The marketer edits content, prepares an approval showing the frozen snapshot, confirms it, and can withdraw, let it expire, or cancel.

**Tasks**
- `PUT /content` (new version, optimistic concurrency), channel/content invalidation rules.
- `approval` package: prepare (held rows, partition, digest, count assertion, empty-audience guard), confirm (binding CAS with expiry), withdraw, `ExpireDue`, revoke-on-edit; campaign state machine (§5.2).
- Worker skeleton (`cmd/worker`) with the expiry sweeper and heartbeat (dispatch comes in Slice 4).
- `POST /cancel` for pre-send states.
- Frontend: `SmsEditor`/`EmailEditor` with debounced `validate`, `ApprovalDialog` (prepare stats, countdown, confirm/withdraw, restore on reload), approved banner, expiry/revocation banners.

**Tests.** Every allowed/forbidden transition; binding mismatch → 409; confirm vs. expiry and edit vs. confirm races (concurrent goroutines on real PostgreSQL); **lock-wait expiry integration test** for confirm, send and the sweeper (ARCHITECTURE §5.3: lock held across expiry by another transaction, consumption started before expiry must be rejected; channels + deadline-bounded polling, no `time.Sleep`); held rows deleted on revoke/expire; invariant test (users added after prepare are not in the snapshot); statistics on approval = held row counts; Vitest for `ApprovalDialog` states.

**Acceptance criteria**
- Edit → prepare shows counts that exactly equal the held rows in the DB; confirm → approved with 24 h countdown.
- Editing content or channels after prepare/confirm visibly revokes the approval and removes held rows. *(As implemented: such edits are refused with `409 invalid_state` while the approval is live and the UI disables them; the marketer withdraws explicitly, which removes the held rows — ARCHITECTURE §5.2 ².)*
- Shortening TTLs via env demonstrates expiry: campaign returns to `content_ready`, held rows gone, confirm/send return `409 approval_expired`.
- The lock-wait expiry test passes: an approval that expires while a confirm/send transaction waits for the campaign lock is never confirmed or consumed.

**Depends on.** Slice 2.

---

## Day 5 — Slice 4: Queue and delivery

**Objective.** Reliable, rate-limited, idempotent sending from the approved snapshot.

**Tasks**
- `cmd/mockprovider`: endpoints, faults, idempotency ledger (replay / 422 reuse), strict limiter, arrival recorder, `/admin/stats`, `/admin/reset`.
- Send transaction (consume approval, release held rows), mid-send cancellation.
- Worker: dispatcher + sender pool per channel, claim, precheck (§10.3), `gatedSender.Attempt` (Acquire → precheck with budget → final slot-deadline gate → Do), provider `Capabilities` + retry-horizon computation + effective-guarantee log/`/readyz`, sliding-window limiter + `Pause`, outcome classification, retry policy, guarded completion + attempt rows, reaper, completion detection, graceful shutdown, `outcome_uncertain`.
- `GET /stats`, `GET /deliveries`; Send and Cancel buttons; basic stats panel (manual refresh).
- Core delivery metrics.

**Tests.** Retry policy table; limiter (fake clock, 50 goroutines, `-race`, pause isolation); slot-deadline tests (precheck over budget, delay after precheck, 3 aborts → release without counting an attempt; never a request after the deadline); mock-provider idempotency (same key + same payload → replay, same key + different payload → 422, retry after a committed timeout → reconciled with one ledger entry); concurrent claim (each of 2,000 rows claimed once); lease expiry → reaper → `unknown` attempt; crash after provider accept → replay → exactly one provider message, row `sent`; consent revoked after approval → `skipped_consent_revoked`; contact removed before first attempt → `skipped_no_contact`; destination persisted before request and identical across retries; cancellation mid-send; **rate integration test** (provider-observed `max_in_any_rolling_1s ≤ 100` per channel, arrivals = attempts, 429 pauses one channel only).

**Acceptance criteria**
- Sending ~2,000 messages with faults enabled reaches `completed` or `completed_with_failures`.
- Provider stats: ≤100 requests in any rolling second per channel; zero duplicate accepted keys; arrivals equal recorded attempts.
- Every channel's status counts sum to `approved_total`.

**Depends on.** Slice 3.

---

## Day 6 — Slice 5: Live dashboard and observability

**Objective.** Live, resilient progress view and operational endpoints.

**Tasks**
- `GET /events` (1 s snapshots, heartbeat, `retry:`, close on terminal), nginx SSE config verified.
- Frontend: `useCampaignEvents`, `CampaignDashboardPage` (`ChannelProgress`, `StatTiles`, uncertain count, `FailureTable` with paging, `ConnectionBadge`), cancel while sending.
- `/readyz` with worker heartbeat age; remaining core metrics (`delivery_queue_depth`, `provider_rate_limited_total`).
- Go API-level end-to-end test: create → interpret → generate → edit → prepare → confirm → send → completion (scripted LLM, real worker, mock provider).

**Tests.** SSE handler (snapshot, heartbeat, close on terminal); composable reconnect test; dashboard component tests; the end-to-end API test.

**Acceptance criteria**
- The dashboard updates live during a send, survives `docker compose restart api`, and stops at a terminal status with final numbers equal to `/stats`.
- `/metrics` shows delivery, provider, queue and LLM metrics moving during a send.
- Cancel during sending stops new requests within one second; final partition still sums to `approved_total`.

**Depends on.** Slice 4.

---

## Day 7 — Hardening, documentation, B-tier

**Objective.** Submission quality.

**Tasks**
- Fresh-clone verification; README (setup, env, demo script, "how we prove: no duplicates, rate limit, snapshot invariant", limitations); final pass on `DECISIONS.md` and `docs/ARCHITECTURE.md`.
- B-tier items in order as time allows (GSM/UCS-2 warnings, Playwright, …).
- Log/metric review; remove dead code; `go vet`, `staticcheck`, `vue-tsc`, lint.

**Acceptance criteria**
- `docker compose up` + README demo script works end-to-end from a clean machine with and without an API key.
- All unit and integration tests pass; documented limitations match behaviour.

**Depends on.** All slices.

---

## Schedule risk controls
- Slices 1–3 must be complete by end of Day 4; if not, B-tier is dropped entirely and Day 7 absorbs the overrun.
- If Slice 4 overruns, Slice 5 reduces to SSE snapshots rendered as plain tiles (no failure table paging).
- The rate integration test and the crash/replay test are never cut: they prove the core reliability claims.

---

## Status at submission (CP18)

Delivered (A tier): Slices 1a–5 end to end — interpretation (demo + Anthropic), DSL/validator/compiler, audience partition, streamed content generation and editing, prepare/confirm/withdraw/expiry with frozen snapshots, send and cancel, the PostgreSQL worker (claim, lease, precheck, gated sender, per-channel sliding-window limiter, 429 pauses, classified retries, reaper, sweeper), the durable mock provider, the live SSE dashboard, `/healthz`, `/readyz`, `/metrics` (six core metrics), Docker Compose, README and ADRs. Verification evidence: `docs/FINAL_CHECKLIST.md`.

Not delivered:
- **Day 2 model evaluation (A tier):** not performed, by decision (internal goal, not an assignment requirement; it needs paid API calls). The 12-brief comparison and `docs/model-evaluation.md` do not exist. Live integration was verified with one interpretation call (CP10); that is not a comparative quality evaluation. The content default rests on reasoning only (ADR-015 status).
- **B tier:** GSM-7/UCS-2 and segment warnings (1), Playwright (2), `llm_calls` audit table (3), masked sample users (4) and the refusal fallback (5) were not built; the snapshot digest in the approval dialog (6) was. Browser acceptance was performed manually against Compose (CP17B, CP18).
- **C tier:** none.
