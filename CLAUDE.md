# CLAUDE.md — Smart Campaign Composer

Guidance for anyone (human or AI) implementing this repository. Read `docs/ARCHITECTURE.md` (design), `DECISIONS.md` (rationale) and `docs/ROADMAP.md` (slices) before changing anything. When code and these documents disagree, stop and resolve the disagreement explicitly — do not silently diverge.

## Project overview

AI-assisted marketing campaign composer (7-day take-home). A marketer writes a brief; Claude proposes a typed audience rule and SMS/email content; the backend validates everything; the marketer edits, reviews a frozen audience snapshot and approves; a worker sends through a mock provider at ≤100 req/s per channel with retries and idempotency; a dashboard shows live statistics. No real messages are sent.

## Technology stack

- **Backend**: Go 1.25, `pgx/v5` + `pgxpool`, stdlib `net/http` ServeMux, `golang-migrate`, `log/slog`, Prometheus text exposition (planned `prometheus/client_golang`; replaced by a standard-library implementation — ADR-026 amendment), `golang.org/x/text/unicode/norm`, Anthropic Go SDK `github.com/anthropics/anthropic-sdk-go/v2` (pinned v2.5.0).
- **Database**: PostgreSQL 16 — the only infrastructure dependency. Also the job queue.
- **Frontend**: Vue 3 + TypeScript (strict) + Vite + Vue Router + Pinia; Vitest + Vue Test Utils; Playwright (B tier, not built).
- **Tests**: `go test`, `testcontainers-go` (PostgreSQL) for integration tests.
- **Runtime**: Docker Compose — `postgres`, `migrate`, `seed`, `api`, `worker` (exactly 1 replica), `mockprovider`, `frontend` (nginx).

Do not add infrastructure (Redis, brokers, caches) or new major dependencies without a new ADR in `DECISIONS.md`.

## Repository layout

```
backend/cmd/{api,worker,mockprovider,seed,migrate}
backend/migrations/
backend/internal/{platform,httpapi,campaign,segment,content,ai,llm,approval,delivery,provider,mockprovider}
backend/test/integration/
frontend/src/{api,composables,pages,components/{composer,dashboard}}
docs/  DECISIONS.md  CLAUDE.md  README.md  docker-compose.yml  Makefile
```

## Module boundaries

| Package | Owns | Must not |
|---|---|---|
| `httpapi` | routing, DTO decode/validate, error mapping, SSE framing | contain business rules or SQL |
| `campaign` | campaign aggregate, state machine, repository | call the LLM or provider |
| `segment` | DSL, registry, validator, compiler, audience statistics | accept fields not in the registry |
| `content` | template parse/render, variable registry, content validation | use `text/template` |
| `ai` | prompts, schemas, Interpreter, Generator, repair, stream extractor | talk to the SDK directly (only via `llm.Client`) |
| `llm` | `Client` interface; `anthropic`, `scripted`, `demo` implementations | know about campaigns or SQL |
| `approval` | prepare / confirm / withdraw / expire / consume, snapshot creation | — it is the **only** code that inserts `campaign_deliveries` |
| `delivery` | claim, precheck, gated sender, limiter, retry policy, reaper, sweeper hooks, completion, stats | import `ai` or `llm`; call the provider outside `gatedSender.Attempt` |
| `provider` | HTTP client to the provider, response classification, declared `Capabilities` (idempotency keys, key retention) | rate limit (that is `delivery`'s job) |
| `mockprovider` | fake provider server | be imported by production packages |

Interfaces only where a test double is needed: `llm.Client`, the provider sender, `platform.Clock`. Repositories are concrete structs tested against real PostgreSQL.

## Coding conventions

**Go**
- `context.Context` is the first parameter of every function doing I/O; respect cancellation.
- Errors: wrap with `fmt.Errorf("…: %w", err)`; domain errors are sentinel/typed values in their package; `httpapi` maps them to the error envelope. Never `panic` for control flow.
- No package-level mutable state; dependencies are passed via constructors. Configuration is loaded and validated once in `platform/config`.
- SQL lives in repository files as constants; every value is a bind parameter. Multi-statement state changes run in one transaction that locks the campaign row first (`campaigns` → `campaign_approvals` → `campaign_deliveries`).
- Use database `now()` for ordinary persisted timestamps, leases and `next_attempt_at`. **Approval expiry is different**: lock the campaign row first, then compare and set expiry-related times (`prepared_at`, `approved_at`, `expires_at`, `consumed_at`) with `clock_timestamp()` — `now()` is the transaction start time and is stale after a lock wait. Use the injected `Clock` (monotonic) only for rate limiting and durations.
- Logging: `slog` JSON with `campaign_id`, `approval_id`, `delivery_id`, `request_id`; mask phone numbers and emails; never log `ANTHROPIC_API_KEY` or full prompts containing anything sensitive.
- Keep handlers thin: decode → call one service method → encode.
- `gofmt`, `go vet`, `staticcheck` clean.

**Frontend**
- `<script setup lang="ts">`, Composition API, strict TypeScript; API types in `src/api/types.ts` mirror backend DTOs.
- Server state is fetched per page; Pinia holds only the active composer campaign.
- Never render AI or user text with `v-html`; email preview uses a sandboxed iframe with escaped text.
- Client-side validation is UX only; always display server validation results.

## Architectural invariants (must always hold)

1. The LLM never produces executable SQL. Segment SQL comes only from `segment.Compile` over validated, normalized rules.
2. All LLM output is untrusted: strict decode + semantic validation on every path, including after the repair turn.
3. Consent and contact are enforced by the backend and are not expressible in the DSL.
4. A campaign is never sent without a **confirmed, unexpired** approval bound to segment version, content version, channels and snapshot.
5. Delivery rows are inserted **only** by `approval.Prepare`, as `held`. The system never sends to a user who is not in the approved snapshot. No audience re-evaluation after prepare.
6. Held rows exist iff their approval is `pending` or `approved`. Revoking/expiring an approval deletes its held rows and resets the campaign status in the same transaction. No other delivery rows are ever deleted.
7. Only rows of the `consumed` approval of a `sending` campaign can be claimed or sent.
8. Expired approvals can never be confirmed or consumed: every campaign/approval transition locks the campaign row first, and expiry is evaluated after the lock with `expires_at > clock_timestamp()` — never `now()`/`CURRENT_TIMESTAMP`/a Go timestamp. Sweeper and lazy expiry use the same rule.
9. One delivery per (campaign, user, channel); idempotency key `campaign_id:user_id:channel`; every attempt for a delivery sends the same key, destination and body.
10. Destination is resolved from current contact at the first precheck, persisted **before** the HTTP request, and never changed afterwards.
11. Consent is re-checked in the precheck of **every** attempt; the precheck happens after the rate-limit slot is acquired, and there is no I/O between precheck and the provider request.
12. Every provider request goes through `gatedSender.Attempt`, which acquires the channel's sliding-window slot. SMS and email limiters are independent; a 429 pauses only its channel. A request is never issued if the precheck exceeded `DISPATCH_MAX_PRECHECK` or if `DISPATCH_MAX_SLOT_TO_REQUEST` has elapsed since the slot was granted (final gate immediately before `http.Client.Do`); the slot is then wasted, never refunded.
13. No transaction or row lock is held across an HTTP request (LLM or provider).
14. Worker completions are guarded by `leased_by` and `status = 'in_flight'`.
15. Audience statistics are an exclusive partition (`no_contact` → `no_consent` → `eligible`); delivery statistics partition `approved_total`; `uncertain` is an overlay, never added.
16. SMS ≤160, subject ≤80, body ≤5,000 runes **worst case after personalization** (NFC); encoding and segments are warnings only.
17. Model names come only from configuration (`CLAUDE_SEGMENT_MODEL`, `CLAUDE_CONTENT_MODEL`). The LLM provider is chosen once at startup (`LLM_PROVIDER=auto|anthropic|demo`; `auto` = anthropic iff `ANTHROPIC_API_KEY` is set); Anthropic failures are returned as errors and never replaced by demo output.
18. We never claim exactly-once delivery. Ambiguous outcomes set `outcome_uncertain`. "At most one delivered message per recipient and channel" is claimed only for a provider adapter that declares idempotency keys with retention ≥ the retry horizon (the mock provider); otherwise the claim is at-least-once.

## Testing requirements

- `go test ./...` and `npm test` must pass **without** `ANTHROPIC_API_KEY` and without Docker (unit tests only).
- Integration tests (`go test -tags=integration ./...`) use testcontainers PostgreSQL and, for delivery, the real worker and mock provider over TCP. Run with `-race`.
- Every slice ships its tests in the same change. A slice is not done until its acceptance criteria in `docs/ROADMAP.md` are demonstrated.
- Required test categories:
  - **Segmentation**: validator table tests (every field × operator, wrong types, ranges, limits, unknown keys, consent fields, injection strings as values), compiler golden tests, fixture-based count tests with hand-computed expectations, partition identities.
  - **AI**: `llm.scripted` tests for happy path, malformed → repair → ok, repair failure, refusal, `max_tokens`, timeout, cancellation; fuzz test for the stream extractor (splits inside escapes and multi-byte runes).
  - **Content**: variable parsing, worst-case length incl. fallbacks and multi-byte text, NFC, limits, encoding/segment warnings.
  - **Approval/state machines**: every allowed and forbidden transition; binding mismatch; expiry vs confirm/send races (concurrent goroutines); **lock-wait expiry test** (transaction A holds the campaign lock; confirm/send started before expiry waits on it; the approval expires; A commits; the consumption must be rejected as expired — likewise the sweeper must expire it), coordinated with channels and deadline-bounded polling of `pg_stat_activity` / `clock_timestamp()`; held-row deletion; snapshot invariant (users added after prepare are never sent).
  - **Delivery**: retry policy (pure), limiter with fake clock + concurrency, concurrent claim (each row claimed once), lease expiry + reaper, crash after provider accept → replay → `sent` with one provider message, consent revoked after approval → skipped, cancellation mid-send, destination persisted before request and reused by retries; **slot deadline** (precheck over budget → no request; delay after precheck past the slot deadline → no request; 3 aborts → lease released without counting an attempt).
  - **Mock provider idempotency**: same key + same payload → replay of the original result; same key + different payload → rejection (`422`); retry after an ambiguous response (timeout that committed) → reconciled via the original key with exactly one ledger entry.
  - **LLM provider selection**: the `auto`/`anthropic`/`demo` × key-present/absent table; an Anthropic error is returned, never demo output.
  - **Rate (integration)**: provider-observed `max_in_any_rolling_1s ≤ 100` per channel; provider arrivals = recorded attempts; 429 pauses only the affected channel.
  - **API**: `httptest` against real DB + scripted LLM covering the full flow and 409/422 paths.
  - **Frontend**: component tests for previews/counters, validation display, approval dialog states, SSE parser, EventSource composable reconnect.
- Do not use `time.Sleep` to synchronize tests; use the fake clock, channels or polling with deadlines. Real-time waits are allowed only in the designated rate integration test.
- Do not mock PostgreSQL for queue, approval or segmentation tests.

## Implementation workflow

1. Work one vertical slice at a time (`docs/ROADMAP.md`); backend and frontend of a slice land together.
2. Before coding a slice, re-read the relevant ARCHITECTURE sections; if a change to the design is needed, update `docs/ARCHITECTURE.md` and add/modify an ADR first.
3. Write migrations before repositories; repositories before services; services before handlers; then UI.
4. Run `make test` (unit) continuously and `make test-integration` before declaring a slice done.
5. Demonstrate the slice's acceptance criteria (`docker compose up`, browser flow) and record any deviation.
6. Commit per coherent step with descriptive messages; do not commit secrets or `.env`.
7. B-tier items start only after all A-tier items of the current slice are done; C-tier only after all slices are done.
8. Any live Claude call (smoke test, Day 2 evaluation) spends real money: ask before running it.

## Explicitly prohibited shortcuts

- Executing or string-building SQL from LLM output or any user-provided value; interpolating values into SQL text.
- Skipping validation because output "came from the model" or "the schema guarantees it".
- Inserting delivery rows anywhere other than `approval.Prepare`; enqueueing at send time; re-running the audience query at send or dispatch.
- Calling the provider outside `gatedSender.Attempt`, or retrying without acquiring a new rate-limit slot.
- Any token bucket with burst > 1, fixed-window limiting, or a shared limiter across channels.
- Holding a DB transaction or lock across an HTTP call; doing I/O between precheck and the provider request; sending after the slot deadline or refunding a wasted slot.
- Comparing approval expiry with `now()`, `CURRENT_TIMESTAMP` or a Go-side timestamp, or checking expiry before acquiring the campaign row lock.
- Falling back to demo/mock LLM output when the Anthropic provider fails; selecting the LLM provider anywhere except startup configuration.
- Claiming at-most-one delivery for a provider whose adapter does not declare idempotency keys with sufficient retention.
- Changing a delivery's destination after it is persisted; generating a new idempotency key for a retry.
- Marking a delivery `sent` before the provider confirms; deleting non-held delivery rows.
- Treating a timeout as success or as a definite failure without recording ambiguity.
- Using `text/template`/`html/template` for AI-written content; rendering AI text with `v-html`.
- Hardcoding model IDs; sending `temperature`/`top_p`/`top_k`; forced `tool_choice`; assistant prefill.
- Sending customer data (names, contacts, purchase history) to the LLM.
- Claiming exactly-once delivery anywhere (code comments, README, UI).
- Adding Redis or any other infrastructure; adding more worker replicas.
- Disabling or weakening a failing test to make a slice pass.
- Committing `ANTHROPIC_API_KEY` or any `.env` file.
