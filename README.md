# Smart Campaign Composer

An AI-assisted marketing campaign composer (seven-day take-home). A marketer
describes a campaign in plain language; Claude proposes a **typed audience
rule** and **SMS/email content**; the Go backend validates everything; the
marketer edits, reviews a **frozen audience snapshot** and approves; a worker
then sends through a **mock** SMS/email provider at ≤ 100 requests per second
per channel, with retries, 429 handling and idempotency, while a live
dashboard shows progress. **No real messages are ever sent.**

Design documents: [`docs/ARCHITECTURE.md`](docs/ARCHITECTURE.md) (design and
contracts), [`DECISIONS.md`](DECISIONS.md) (ADR-001…026),
[`docs/ROADMAP.md`](docs/ROADMAP.md) (slices and submission status),
[`docs/FINAL_CHECKLIST.md`](docs/FINAL_CHECKLIST.md) (acceptance evidence),
[`CLAUDE.md`](CLAUDE.md) (implementation rules and invariants).

## Contents

1. [Workflow](#workflow) · 2. [Stack and services](#stack-and-services) ·
3. [Quick start (demo mode, no API key)](#quick-start-demo-mode-no-api-key) ·
4. [Demo walkthrough](#demo-walkthrough-35-minutes) ·
5. [Tests](#tests) · 6. [Smoke tests](#smoke-tests) ·
7. [Real Claude (optional, billable)](#real-claude-optional-billable) ·
8. [How approval, consent and sending work](#how-approval-consent-and-sending-work) ·
9. [Operations: restart, isolate, reset](#operations-restart-isolate-reset) ·
10. [API and metrics](#api-and-metrics) · 11. [Decisions and trade-offs](#decisions-and-trade-offs) ·
12. [Known limitations](#known-limitations) · 13. [Where the code lives](#where-the-code-lives) ·
14. [AI-assisted development](#ai-assisted-development)

## Workflow

Create campaign → **Interpret** the brief (Claude or the deterministic demo
interpreter) → review the validated rule and the live audience preview
(`eligible / no_consent / no_contact` per channel) → choose SMS and/or email →
**Generate** content (streamed) and edit it → **Prepare approval** (freezes
recipients + content version) → **Confirm** → **Start sending** (explicit) →
watch the live dashboard → *Completed*, *Completed with failures* or
*Cancelled* (cancel is possible at any non-final step).

## Stack and services

| Layer | Technology |
|---|---|
| Backend | Go 1.25, stdlib `net/http`, `pgx/v5`, `golang-migrate`, `log/slog`, Anthropic Go SDK v2.5.0 |
| Database / queue | PostgreSQL 16 (the only infrastructure; also the job queue — ADR-002) |
| Frontend | Vue 3 + TypeScript (strict), Vite, Vue Router, Pinia; Vitest + Vue Test Utils |
| Runtime | Docker Compose, nginx (serves the SPA, proxies `/api/` and `/readyz`, SSE-safe) |

Compose services (one backend image with five entrypoints — ADR-001):

| Service | Role | Published |
|---|---|---|
| `postgres` | data + queue (volume `pgdata`) | no |
| `migrate` | one-shot, applies embedded migrations | — |
| `seed` | one-shot, loads 5,000 deterministic users only if `users` is empty | — |
| `api` | REST + SSE API; healthy once `/readyz` sees a migrated DB | no |
| `mockprovider` | fake SMS/email provider with a durable idempotency ledger (volume `mockledger`) and fault injection | no |
| `worker` | the **single** delivery worker (also expiry sweeper, reaper, heartbeat) | no |
| `frontend` | nginx + built Vue app | `127.0.0.1:8000` only |

Start order: `postgres` (healthy) → `migrate` → `seed` → `api` (healthy) →
`mockprovider` (healthy) → `worker`; `frontend` after `api`. A failed
migration or seed stops the chain (`docker compose logs migrate seed`).
Backend containers run as uid 10001 with a read-only root filesystem, all
capabilities dropped and `no-new-privileges`; nginx runs unprivileged and
read-only; `postgres` uses stock image settings.

## Quick start (demo mode, no API key)

**Prerequisites:** Docker Desktop or Docker Engine with Compose v2.20+.
Nothing else — no Go, Node, Anthropic account or API key. Verified on macOS
(Apple Silicon, Docker Desktop, Compose v5.1); Linux hosts are expected to work
but were not separately verified.

```bash
git clone <repository-url> && cd insider-campaign-composer-case
```

```bash
LLM_PROVIDER=demo ANTHROPIC_API_KEY= docker compose up --build -d --wait
```

Open **http://localhost:8000** (another port: `COMPOSER_HTTP_PORT=18000`). The
first build takes a few minutes. The UI shows a **Demo AI** badge; nothing is
sent to Claude. `LLM_PROVIDER=demo ANTHROPIC_API_KEY=` forces demo mode even if
your shell or a local `.env` holds a key (`docker compose up --build` without
them also selects demo when no key is configured).

```bash
curl -s http://localhost:8000/readyz
```

→ `{"status":"ready","database":"ok","migration_version":2,"migration_dirty":false,"llm_provider":"demo","worker":{"status":"fresh",…},"sending_campaigns":0}`

Stop with `docker compose down` (keeps all data; see
[Operations](#operations-restart-isolate-reset)).

**Seed and dates.** `SEED_USERS=5000`, `SEED_RANDOM_SEED=42`,
`SEED_REFERENCE_TIME=2026-11-01T00:00:00Z` always produce the same dataset
(`dataset_hash c553fe36…` in the `seed` log). "Last N days" rules are evaluated
against the database's `now()`, so counts shrink as real time moves past the
reference date (the example brief matches 184 users on 2026-11-01 and 178 on
2026-11-06). For a demo much later, start a **fresh** volume with
`SEED_REFERENCE_TIME="$(date -u +%Y-%m-%dT00:00:00Z)"` (not reproducible across days).

## Demo walkthrough (3–5 minutes)

A condensed evaluator script with timings is in
[`docs/FINAL_CHECKLIST.md` → Recommended 3–5 minute live demo](docs/FINAL_CHECKLIST.md#recommended-35-minute-live-demo).

1. **New campaign** → **Use example brief** (or paste a Turkish one, e.g.
   *Son 30 gün içinde sepetine ürün ekleyip satın almayan İstanbul'daki
   kullanıcılara %30 indirim hatırlatması gönder.*) → **Create campaign**.
2. **Interpret brief.** The demo interpreter (a deterministic keyword matcher,
   clearly labelled) proposes *City is İstanbul · Abandoned cart in the last 30
   days*; the backend validates it. Show the rule chips, assumptions, and the
   audience card: per channel `eligible + no_consent + no_contact = matched`.
   Untick a channel to show that messages change and matched users do not.
3. **Generate content.** Phases stream (writing → validating → saving); the
   validated version replaces the unvalidated live draft. Edit the SMS: type
   `{{email}}` (→ `unknown_variable`) or exceed 160 worst-case characters
   (→ blocking error). **Save changes** creates version 2.
4. **Prepare approval.** The dialog shows the frozen per-channel counts, the
   exclusions, the content version, the snapshot digest, a 15-minute
   countdown and the rendered content. Tick the acknowledgement →
   **Confirm approval** (valid 24 h). Nothing is sent yet.
5. **Start sending…** → confirm. The dashboard goes *Live*: per-channel
   progress, sent/failed/skipped/cancelled, provider attempts, retries, 429s,
   uncertain outcomes. A few SMS fail on purpose (mock destinations ending
   `0000`), so the expected end state is **Completed with failures**. Reload:
   the same final numbers come from PostgreSQL.
6. *(Optional)* Prepare another campaign with a larger audience (*"…added
   products to their cart in the last 90 days but did not complete a purchase"*,
   ≈ 1,200 deliveries) and **Cancel sending…** mid-flight: queued rows are
   cancelled at once and the in-flight count is reported honestly.

## Tests

Unit tests need no Docker, no API key and no network (the Anthropic adapter is
tested against an offline recording transport):

```bash
make test
```

That runs `go vet` and `go test ./...` (backend) and `npm test` +
`npm run typecheck` (frontend; requires `cd frontend && npm ci` once). Separately:

```bash
cd backend && go test -race ./...
```

```bash
cd frontend && npm ci && npm test && npm run typecheck && npm run build
```

**PostgreSQL integration tests** (testcontainers; needs Docker; offline, no key;
≈ 3 min). They run the real worker against the mock provider over TCP: crash
and replay, lease reaping, two concurrent workers, the provider-observed rate
limit (`max_in_any_rolling_1s ≤ 100`), 429 isolation, cancellation races, the
lock-wait expiry test, statistics reconciliation and a privacy scan:

```bash
make test-integration
```

**Lint** (gofmt, `go vet` for all build tags, staticcheck v0.8.2):

```bash
make lint
```

`make lint` downloads staticcheck through the Go module proxy. Behind a
TLS-intercepting proxy that download fails; do not disable TLS or checksum
verification. If staticcheck v0.8.2 is already in your module cache, run it
offline instead:

```bash
cd backend && GOPROXY="file://$(go env GOMODCACHE)/cache/download" go run honnef.co/go/tools/cmd/staticcheck@v0.8.2 ./...
```

## Smoke tests

Against a running stack (need `curl` and `python3`; each creates its own
campaigns). `smoke-send` and `smoke-cancel` require demo mode:

```bash
make smoke         # create → interpret → audience partition → persistence
```

```bash
make smoke-send    # full send; checks DB rows, ledger, ≤100/s, no duplicates, no PII in logs
```

```bash
make smoke-cancel  # cancel before and during sending
```

For an isolated stack set the same variables you started it with, e.g.
`COMPOSE_PROJECT_NAME=composer-review COMPOSER_HTTP_PORT=18000 make smoke-send`.

## Real Claude (optional, billable)

⚠️ Every interpretation and every content generation is a **billable**
Anthropic API request (up to three requests per action: the answer, at most one
repair turn, at most one retry after truncation, each with up to
`CLAUDE_MAX_RETRIES` SDK retries). Keep the key out of tracked files:

```bash
cp .env.example .env   # then set ANTHROPIC_API_KEY=… in .env (git-ignored)
```

```bash
LLM_PROVIDER=anthropic docker compose up --build -d --wait
```

| `LLM_PROVIDER` | `ANTHROPIC_API_KEY` | Result |
|---|---|---|
| `auto` (default) | empty | `demo` |
| `auto` | set | `anthropic` |
| `anthropic` | empty | API refuses to start |
| `demo` | any | `demo` |

The provider is chosen once at startup. An Anthropic failure (auth, rate
limit, outage, timeout, refusal, invalid output) is returned as an API error —
**never** replaced by demo output. Models come only from configuration:
`CLAUDE_SEGMENT_MODEL` (default `claude-haiku-5-5`) and `CLAUDE_CONTENT_MODEL`
(default `claude-opus-5-5`), plus `_EFFORT`, `_TIMEOUT`, `_MAX_TOKENS` (all in
`.env.example`). Only the brief and the content brief are sent to Claude —
never customer names, contacts or purchase history. Live *integration* verification
(does the adapter work against the real API) is manual and budget-capped in
`backend/test/live`, never part of `make test`; it was run once for
interpretation (CP10). A *comparative model-quality evaluation* was planned but
not performed (ADR-015).

## How approval, consent and sending work

- **The LLM never writes SQL.** It returns a JSON rule in a small typed DSL;
  the backend strictly decodes it (unknown/duplicate keys, wrong types and
  out-of-range values are field errors), validates it against a field/operator
  registry and compiles it to parameterized SQL (`segment.Compile`). Consent
  and contact are not expressible in the DSL; the backend applies them.
- **Content** uses a custom `{{first_name|fallback}}` grammar (no template
  engine), NFC normalization and **worst-case** limits after personalization:
  SMS ≤ 160, subject ≤ 80, body ≤ 5,000 characters. Nothing is truncated.
- **Approval** (ADR-005/006/023): *Prepare* inserts one `held` delivery row per
  eligible recipient and channel with frozen personalization values, bound to
  the segment version, content version and channels. *Confirm* within 15 min
  (pending TTL) → approved for 24 h. Every transition locks the campaign row
  and checks expiry with `clock_timestamp()`; an expired approval can never be
  confirmed or sent (`409 approval_expired`). Editing content or channels
  revokes the approval and deletes its held rows. Users who start matching
  later are never added.
- **Send** is an explicit, idempotent, version-guarded call that consumes the
  approval and releases exactly its frozen rows to the worker. A repeat
  returns `200 already_started` and writes nothing.
- **Worker, per attempt** (ADR-007…012): acquire the channel's rate slot →
  re-check **current** consent and contact (missing → `skipped_*`, never sent)
  → persist the destination on the first attempt → send to the mock provider
  with the idempotency key `campaign:user:channel` and the same body on every
  retry. Transient errors retry with full-jitter exponential backoff (≤ 60 s,
  5 attempts); 429 pauses only that channel for `Retry-After` (1–30 s,
  persisted); permanent 4xx never retry. No transaction is open across HTTP.
- **Mock provider** (ADR-020): fsynced idempotency ledger on the `mockledger`
  volume — same key + same payload replays the original result, same key +
  different payload is `422`. Default faults: 3 % 5xx, 1 % 429, 0.5 % timeouts
  that commit before stalling, destinations ending `0000` rejected. Disable
  them with `MOCK_FAULT_5XX_RATE=0 MOCK_FAULT_429_RATE=0
  MOCK_FAULT_TIMEOUT_RATE=0 MOCK_PERMANENT_FAIL_PATTERN=-`.
- **Guarantees, honestly:** requests are at-least-once. With the mock provider
  there is at most one accepted message per recipient and channel **while its
  durable ledger is retained** (`down -v` or the opt-in admin reset forgets
  it). Ambiguous outcomes are flagged `outcome_uncertain`, never counted as
  sent. We never claim exactly-once. The 100 requests/s limit is enforced at
  the dispatch gate per channel; arrival at the provider additionally assumes
  ≤ 30 ms transport jitter (measured, not proven).

## Operations: restart, isolate, reset

**Restart, keep data.** `docker compose down` then `docker compose up -d`
(also `restart`, Ctrl+C, `--build`): `migrate` finds the schema current, `seed`
finds users present, and campaigns, delivery history and the mock ledger
survive.

**Isolated evaluation environment** (does not touch an existing stack's
containers or volumes):

```bash
export COMPOSE_PROJECT_NAME=composer-review COMPOSER_HTTP_PORT=18000 LLM_PROVIDER=demo ANTHROPIC_API_KEY=
```

```bash
docker compose up --build -d --wait
```

Remove only that project, including its two volumes:

```bash
docker compose -p composer-review down -v
```

**Start over in the default project** (⚠️ deletes every campaign, the seed and
the mock ledger of the `campaign-composer` project):

```bash
make reset-volumes CONFIRM=yes
```

Both stacks tag the same local images (`campaign-composer/backend:local`,
`campaign-composer/frontend:local`); rebuilding one re-tags them for both.

## API and metrics

All routes are under `/api/v1` (through nginx on the same origin; no CORS).
Request bodies are strict JSON (≤ 64 KiB, `application/json`, no unknown or
duplicate properties); every mutation carries `expected_version`. Errors use
one envelope: `{"error":{"code","message","details":[{path,code,message}],"retryable"}}`.

| Route | Purpose |
|---|---|
| `POST /campaigns`, `GET /campaigns/{id}` | create / read |
| `POST /campaigns/{id}/interpret`, `GET …/audience`, `PUT …/channels` | rule and audience |
| `GET …/content`, `POST …/content/validate`, `PUT …/content`, `POST …/content/generations` (SSE) | content |
| `POST …/approvals`, `GET …/approvals/{aid}`, `POST …/{aid}/confirm`, `POST …/{aid}/withdraw` | approval |
| `POST …/send`, `POST …/cancel` | delivery control |
| `GET …/stats`, `GET …/deliveries`, `GET …/events` (SSE) | progress (no recipient data; destinations masked) |
| `GET /healthz`, `GET /readyz`, `GET /metrics` | operations (`/metrics` and `/healthz` are not proxied by nginx) |

Full contracts: ARCHITECTURE §11–§13. Metrics (Prometheus text format, read
inside the network):

```bash
docker compose exec api wget -qO- http://127.0.0.1:8080/metrics
```

```bash
docker compose exec worker wget -qO- http://127.0.0.1:9091/metrics
```

`deliveries_total{channel,outcome}`, `provider_request_duration_seconds{channel}`,
`provider_rate_limited_total{channel}`, `delivery_queue_depth{channel,status}`,
`llm_requests_total{task,outcome}`, `llm_request_duration_seconds{task}` — closed
label sets, no campaign/user labels, per-process counters (ARCHITECTURE §15).
Mock provider counters:
`docker compose exec mockprovider wget -qO- http://127.0.0.1:8090/admin/stats`.

## Decisions and trade-offs

The full rationale is in `DECISIONS.md`; the most consequential choices:

- **PostgreSQL as the queue** (ADR-002): the delivery row *is* the job; claim
  with `FOR UPDATE SKIP LOCKED`, leases and a reaper. No Redis.
- **Typed DSL, never LLM SQL** (ADR-003/004) and strict validation of every
  model output, with one repair turn (ADR-014).
- **Prepare → confirm with a frozen snapshot** (ADR-005/007): what the
  marketer reviews is exactly what is sent; consent and contact are still
  re-checked at every attempt (ADR-008).
- **One dispatching worker** with an in-process per-channel sliding-window
  limiter (ADR-011/025), enforced by a PostgreSQL advisory lock — simple and
  provably ≤ 100/s at the gate, but not horizontally scalable.
- **Business idempotency key + recorded uncertainty** (ADR-009) instead of an
  exactly-once claim.
- **Separate mock provider service** with a durable ledger and fault
  injection (ADR-020) so retries, replays and rate limits are tested over real HTTP.
- **Snapshot SSE events from committed rows** (ADR-017/026): reconnects need no
  replay; the dashboard never shows a row as sent before its commit.
- **Standard-library Prometheus exposition** (ADR-026 amendment):
  `prometheus/client_golang` could not be fetched through the local
  TLS-intercepting proxy and verification is never disabled.

## Known limitations

Accepted trade-offs (documented, not bugs):

- Not exactly-once; at most one accepted message per recipient and channel
  only while the mock ledger is retained. Cancellation cannot recall a request
  already on the wire (`in_flight` in the cancel response).
- Consent revoked in the milliseconds between the precheck and the request is
  not honoured for that one request (ARCHITECTURE §10.10).
- One dispatching worker; FIFO across campaigns (a large campaign delays a small one).
- Snapshot staleness up to 24 h: users who stopped matching still receive the message.
- The demo interpreter is a keyword matcher; it declines briefs it cannot
  express instead of guessing a broader audience.
- No authentication/RBAC, scheduling, quiet hours, frequency capping or real
  unsubscribe handling; segment rules change only by re-interpreting.
- No GSM-7/UCS-2 or SMS-segment warnings; the hard rule is the worst-case
  character count.
- `/metrics` is a minimal standard-library implementation (no runtime collectors).
- Base images are pinned by tag, not digest.
- Not performed / not built (roadmap): the 12-brief comparative model
  evaluation (by decision; paid API calls), and the optional B-tier items —
  GSM/segment warnings, Playwright E2E, an `llm_calls` audit table, masked
  sample users, refusal fallback. See `docs/ROADMAP.md`.

ARCHITECTURE §18 has the complete list.

## Where the code lives

| Path | Contents |
|---|---|
| `backend/cmd/{api,worker,mockprovider,migrate,seed}` | process entrypoints |
| `backend/migrations/` | SQL schema (embedded) |
| `backend/internal/segment` | DSL, registry, validator, compiler, Turkish city normalization, audience partition |
| `backend/internal/ai`, `internal/llm` | prompts, schemas, interpreter/generator, repair, stream extractor; `llm.Client` with Anthropic, demo and scripted implementations |
| `backend/internal/content` | placeholder parser/renderer, variable registry, content validation |
| `backend/internal/campaign`, `internal/approval` | campaign aggregate and state machine; prepare/confirm/withdraw/expire/send/cancel (the only code inserting delivery rows) |
| `backend/internal/delivery`, `internal/provider` | worker, gated sender, limiter, retry policy, reaper, stats; provider HTTP client |
| `backend/internal/mockprovider` | fake provider and its ledger (never imported by production binaries) |
| `backend/internal/httpapi` | routing, strict decoding, error envelope, SSE |
| `backend/internal/platform` | config, DB pool, logging (PII masking), clock, metrics |
| `backend/test/integration`, `backend/test/live` | PostgreSQL integration tests; manual budget-capped live checks |
| `frontend/src/pages`, `components/{composer,dashboard,ui}` | composer page, delivery dashboard, dialogs |
| `frontend/src/api`, `composables`, `stores` | typed client + strict decoders + SSE parser; EventSource composable; Pinia store |
| `frontend/tests` | Vitest suites |
| `frontend/nginx`, `docker-compose.yml`, `scripts/` | proxy config, stack, smoke scripts |

## AI-assisted development

This project was built with Claude Code as an engineering partner. The
exported working sessions are in [`ai-sessions/`](ai-sessions) (sessions 1–18,
as recorded); design decisions, reviews and corrections made in those
sessions are reflected in `DECISIONS.md`. No AI output is trusted at runtime
without backend validation.
