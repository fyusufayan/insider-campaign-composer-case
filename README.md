# Smart Campaign Composer

AI-assisted campaign composer: a marketer writes a brief, the AI proposes a
typed audience rule, the backend validates it and shows a live,
database-backed audience preview; the AI then writes SMS/email content that
the backend validates and stores as immutable versions the marketer can edit;
the marketer then prepares an approval (a frozen recipient snapshot), reviews
it and confirms or withdraws it, and explicitly starts sending; a
PostgreSQL-backed worker delivers through a **mock** SMS/email provider. No
real messages are sent.

Design and rationale: `docs/ARCHITECTURE.md`, `DECISIONS.md`, `docs/ROADMAP.md`,
`CLAUDE.md`. Current scope: **Slices 1a, 1b and 2** — brief →
interpretation (demo, or real Claude when a key is configured) → validated
segment → audience preview → streamed content generation, editing,
validation and immutable content versions, plus Slice 3's approval
preparation, review, confirmation, withdrawal and expiry (CP14–CP15), and
Slice 4's backend (CP16–CP17A): the send API, the delivery worker and the
mock provider, the per-channel rate limiter (≤ 100 requests per channel in
any rolling second, retries included), exponential backoff with jitter, 429
channel pauses, the cancel endpoint and the background approval-expiry
sweeper. Not yet: delivery statistics, metrics, live progress and any
delivery UI (no Send/Cancel button, no dashboard) — those follow in CP17B.

## Quick start (Docker only)

Prerequisites: Docker Desktop (or Docker Engine) with Docker Compose v2.20+.
No Go, Node or Anthropic API key is needed.

```bash
docker compose up --build
```

Open **http://localhost:8000** (change the host port with `COMPOSER_HTTP_PORT=…`).

The web UI is published on the loopback interface only (`127.0.0.1`), so it is
not reachable from other machines; PostgreSQL and the Go API are not published
at all. The `api`, `migrate`, `seed` and `frontend` containers run as non-root
with a read-only root filesystem, all Linux capabilities dropped and
`no-new-privileges`; `postgres` uses the stock `postgres:16-alpine` settings.

On a fresh volume the stack starts in this order; each step must succeed before
the next one starts:

| Service    | Role |
|------------|------|
| `postgres` | PostgreSQL 16, data in the named volume `pgdata` |
| `migrate`  | one-shot: applies the embedded migrations (no-op when up to date) |
| `seed`     | one-shot: loads the deterministic 5,000-user dataset only if the `users` table is empty |
| `api`      | Go API; healthy once `/readyz` reports the database and a clean schema |
| `frontend` | nginx: the built Vue app, proxies `/api/` and `/readyz` to `api` (same origin) |

If `migrate` or `seed` fails, Compose prints `service "…" didn't complete
successfully` and the API is not started. Inspect with `docker compose logs migrate seed`.

### Health checks

```bash
docker compose ps
```

```bash
curl -s http://localhost:8000/readyz
```

Expected: `{"status":"ready","database":"ok","migration_version":1,"migration_dirty":false,"llm_provider":"demo"}`.

An automated HTTP check of the whole Slice 1a flow against the running stack
(needs `curl` and `python3`; creates one campaign named `smoke …`):

```bash
make smoke
```

### Try it

1. Click **Use example brief** (or paste it), then **Create campaign**:

   > Send a short and friendly 30% discount reminder to users in Istanbul who
   > added products to their cart in the last 30 days but did not complete a purchase.

2. Click **Interpret brief**. The demo interpreter proposes *City is İstanbul ·
   Abandoned cart in the last 30 days*; the backend validates it and the
   audience card shows matched users with the per-channel
   `eligible / no_consent / no_contact` split (184 matched users with the
   default seed while "now" is within 30 days of 2026-11-01; see below).
3. Untick SMS and email: zero messages, matched users unchanged. Tick them again.
4. In **Message content**, click **Generate content**. The progress panel shows
   the phases (writing → validating → saving) and a live draft labelled *not
   validated, not saved*; when the backend has validated and committed the
   content, version 1 replaces it in the editors (badge *Demo generator · not a
   live model* in demo mode), with worst-case character counters and SMS/email
   previews rendered with fictional example values.
5. Edit the SMS: the counter and preview update after the server validates the
   draft (e.g. type `{{email}}` to see an `unknown_variable` error, or make it
   longer than 160 worst-case characters). **Save changes** creates version 2
   (*Edited manually*); saving unchanged text reports *No changes* and creates
   no version.
6. Reload or share `/campaigns/{id}` — campaign and content are stored in PostgreSQL.
7. In **Review & approval**, click **Prepare approval**. The backend freezes
   the eligible recipients (rule match + contact + consent, per selected
   channel) and the content version into a pending approval; the review dialog
   shows the frozen per-channel counts, the excluded `no_consent / no_contact`
   counts, reach, the content version and origin, the snapshot digest, a
   countdown to the 15-minute confirmation deadline, the rendered content and
   every advisory offer/price finding. Tick the acknowledgement (a browser-side
   confirmation step; the server does not record or enforce it — it enforces
   the version and the reviewed binding) and click
   **Confirm approval**: the campaign becomes *Approved* (valid 24 h) and the
   read-only summary is shown after a reload too. Confirming sends nothing;
   sending is a separate, explicit API call (see *Send* below; no button yet).
8. **Withdraw approval** (pending: one click in the dialog or panel; approved:
   a second confirmation step) releases the snapshot and returns the campaign to
   *Content ready*. To see expiry, start the stack with short TTLs, e.g.
   `APPROVAL_PENDING_TTL=30s APPROVAL_APPROVED_TTL=1m docker compose up -d`;
   an expired approval cannot be confirmed (`409 approval_expired`) and the UI
   shows *Approval expired — review again*.

### Send (API only in this version)

There is no Send button yet. A confirmed campaign is sent with one explicit API
call (through the frontend proxy); confirming never sends:

```bash
curl -sS -X POST -H 'Content-Type: application/json' \
  -d '{"approval_id":"<approval id>","expected_version":<campaign version>}' \
  http://localhost:8000/api/v1/campaigns/<campaign id>/send
```

`202 {status: "sending", released: {sms, email}, …}` consumes the approval and
releases its frozen rows to the worker; repeating the call returns `200` with
`already_started: true` and writes nothing. The worker then, per row: renders
the approved content version with the **frozen** personalization values,
re-checks the **current** consent and contact (missing consent or contact →
skipped, never sent), persists the destination, and sends to the mock
provider, recording every attempt. The campaign becomes `completed` (or
`completed_with_failures` if any row failed) once every row is terminal — the
campaign page shows the status after a reload. `scripts/smoke-send.sh`
(`make smoke-send`) runs this whole flow against the stack and checks the
database and the mock provider's counters:

```bash
LLM_PROVIDER=demo ANTHROPIC_API_KEY= docker compose up --build -d --wait && make smoke-send
```

The mock provider (internal only, never published) injects faults by default
(3 % 5xx, 1 % 429, 0.5 % timeouts that commit before stalling, destinations
ending `0000` rejected) to exercise retries and idempotency; set
`MOCK_FAULT_5XX_RATE=0 MOCK_FAULT_429_RATE=0 MOCK_FAULT_TIMEOUT_RATE=0
MOCK_PERMANENT_FAIL_PATTERN=-` for a clean run. Its counters (per-process
counters restart at 0; `messages` counts the durable ledger, and
`?key_prefix=<campaign id>:` counts one campaign):
`docker compose exec mockprovider wget -qO- http://127.0.0.1:8090/admin/stats`.

**Rate limit and retries (CP17A).** Per channel, at most
`RATE_LIMIT_PER_SECOND` (100) provider requests in any `1 s +
RATE_SAFETY_MARGIN` window — a sliding-window log in the worker, counting
first attempts and retries; SMS and email are independent. Every request is
issued within `DISPATCH_MAX_SLOT_TO_REQUEST` (20 ms) of its slot or not at
all. Only one worker process dispatches (it holds a PostgreSQL advisory
lock); a second one stands by, so the limit holds for the deployment. A 429
pauses only its channel for `Retry-After` (1 s – 30 s), durably across
worker restarts; transient failures and timeouts retry after a full-jitter
exponential backoff (≤ 60 s, 5 attempts); permanent 4xx are never retried.
The mock provider measures the result: `max_in_any_rolling_1s` and
`limit_exceeded` (its own strict limiter) per channel in `/admin/stats`.

**Cancel** (no button yet; the frontend gets one in CP17B):

```bash
curl -sS -X POST -H 'Content-Type: application/json' -d '{"expected_version":<campaign version>}' \
  http://localhost:8000/api/v1/campaigns/<campaign id>/cancel
```

Before sending it revokes the approval and deletes the frozen rows. While
sending, queued rows are cancelled at once; rows already in flight finish
truthfully — the response's `in_flight` is the number of requests that may
still reach the provider (at most one per row; a request on the wire cannot
be recalled). A repeat is `409 invalid_state`. `make smoke-cancel` exercises
both cases against the stack. Due approvals are also expired in the
background every `APPROVAL_SWEEP_INTERVAL` (30 s); correctness never depends
on it (confirm and send re-check expiry under the campaign lock).

**Guarantees, honestly:** provider requests are at-least-once; every retry of a
delivery sends the same idempotency key (`campaign:user:channel`), destination
and body, and the mock provider replays instead of sending again, so with the
mock there is at most one delivered message per recipient and channel **while
its durable idempotency ledger is retained and intact**. The ledger is an
fsynced journal on the `mockledger` volume: it survives provider crashes,
`docker compose restart`, `down`/`up` and container recreation; only
`docker compose down -v` (which also deletes the database) or the opt-in
admin reset forget earlier keys. A corrupt ledger stops the mock provider
(and therefore the worker) instead of starting empty. This is
a property of the provider's idempotency, not of our database locks; we never
claim exactly-once. Rows whose last attempt was ambiguous and that end unsent
are flagged `outcome_uncertain`. Consent is re-checked immediately before each
request, so a revocation committed before that check is always honoured; one
committed during the few milliseconds between the check and the request is
not (§10.10).

### Restart, keep data

```bash
docker compose down
```

```bash
docker compose up
```

`down` (without `-v`) keeps the `pgdata` volume and the mock provider's
idempotency ledger (`mockledger`), so a retry after a restart is still
recognised as a replay. On the next start `migrate`
finds the schema current and `seed` finds users present; both exit without
changes, so campaigns and customer data are preserved. `docker compose restart`
or Ctrl+C followed by `docker compose up` behave the same.

### Start over (destroys all local data)

```bash
make reset-volumes CONFIRM=yes
```

This runs `docker compose down -v`, removing the database volume with every
campaign and the mock provider's idempotency ledger (`mockledger`). The next `docker compose up --build` migrates and seeds from scratch.
The stack never resets an existing database on its own.

## AI provider and demo mode

The provider is chosen once at API startup from `LLM_PROVIDER`:

| `LLM_PROVIDER` | `ANTHROPIC_API_KEY` | Result |
|---|---|---|
| `auto` (default) | empty | `demo` (warning logged; the UI shows a "Demo AI" badge) |
| `auto` | set | `anthropic` |
| `anthropic` | empty | startup fails with a configuration error |
| `demo` | any | `demo` |

There is never a runtime fallback from Anthropic to demo: once `anthropic` is
selected, every provider failure (authentication, rate limit, outage, timeout,
refusal, invalid output) is returned as an API error. Startup makes no model
call, so an invalid key only shows up on the first interpretation (as
`llm_rejected`). `/readyz` reports the selected `llm_provider`.

**Keep running demo mode** (no key, nothing sent to Claude): leave
`ANTHROPIC_API_KEY` empty, or force it even if your shell exports a key:

```bash
LLM_PROVIDER=demo docker compose up --build
```

**Use real Claude locally.** ⚠️ Every interpretation and every content
generation is a billable Anthropic API request (up to three requests: the first answer, at most one repair turn and at
most one retry after truncation, each with up to `CLAUDE_MAX_RETRIES` automatic
SDK retries). Put the key in the uncommitted `.env` (git-ignored) or export it
only in your shell, never in a tracked file:

```bash
cp .env.example .env    # then set ANTHROPIC_API_KEY=… in .env (never commit it)
```

```bash
LLM_PROVIDER=anthropic docker compose up --build
```

With `LLM_PROVIDER=anthropic` and no key the API refuses to start.

Demo limitations: the demo interpreter is a deterministic keyword matcher, not
language understanding. It recognizes a fixed set of keywords and declines
briefs it cannot express with field-level errors instead of guessing a broader
audience. Nothing is sent
to Claude.

Model names come only from configuration (`CLAUDE_SEGMENT_MODEL`, default
`claude-haiku-5-5`; set `claude-sonnet-5-5` or `claude-opus-5-5` to use another
model, plus `_EFFORT`, `_TIMEOUT`, `_MAX_TOKENS`, and
`CLAUDE_MAX_RETRIES`, default 2). `_TIMEOUT` is the total budget of one
interpretation, SDK retries included. All variables are
listed in `.env.example`; Compose reads an uncommitted `.env` in the repository
root. Never commit `.env` or an API key.

### Seed and dates

The seed is reproducible: `SEED_USERS=5000`, `SEED_RANDOM_SEED=42` and
`SEED_REFERENCE_TIME=2026-11-01T00:00:00Z` always produce the same dataset
(the `dataset_hash` is logged by `seed`). "Last N days" rules are evaluated
against the database's `now()`, so counts shrink as real time moves past the
reference time. For a demo long after that date, start from a fresh volume
with today's date:

```bash
SEED_REFERENCE_TIME="$(date -u +%Y-%m-%dT00:00:00Z)" docker compose up --build
```

(This only takes effect on an empty volume; it is not reproducible across days.)

## Tests

Unit tests need no Docker, no API key and no network; the Anthropic adapter is
tested against an offline HTTP transport that records the exact request JSON
and returns scripted Messages API responses (no test calls Anthropic):

```bash
make test
```

Backend (Go 1.25) and frontend (Node ≥ 22.13) separately:

```bash
cd backend && go test ./... && go test -race ./...
```

```bash
cd frontend && npm ci && npm test && npm run typecheck && npm run build
```

Integration tests (PostgreSQL via testcontainers; requires Docker; also offline,
no API key):

```bash
make test-integration
```

Live Anthropic verification is manual and billable; it is never part of
`make test` or the smoke test. `backend/test/live/liveapi` (build tag `live`)
serves the real API with the Anthropic adapter's transport wrapped in a
persistent request counter (`backend/test/live/budget`) that refuses any request
beyond 3, and it only starts with `LLM_PROVIDER=anthropic`,
`CLAUDE_MAX_RETRIES=0`, an allowed model and an absolute `LIVE_BUDGET_FILE`
outside the repository. The counter fails closed: it must be created
deliberately (`echo 0 > "$LIVE_BUDGET_FILE"`); a missing or corrupt counter sends
nothing.

The delivery integration tests run the real worker against the mock provider
over TCP (crash recovery, consent/contact rechecks, two concurrent workers on
2,000 rows, injected database failures, graceful shutdown) and, since CP17A,
prove the rate guarantee from the provider's side (real time, 1,200
messages with injected 5xx/429: `max_in_any_rolling_1s ≤ 100`, arrivals =
recorded attempts), a dispatch handover between two workers, 429 channel
isolation and pause persistence, cancellation races and the expiry sweeper.

Lint (gofmt, go vet, staticcheck):

```bash
make lint
```

## Development without Docker for the app

Requires Go, Node ≥ 22.13 and a PostgreSQL 16 database. The Vite dev server
proxies `/api` and `/readyz` to the Go API (`http://localhost:8080` by default;
override with `VITE_API_PROXY_TARGET`).

```bash
# 1. Database (credentials as in .env.example), migrated and seeded
export DATABASE_URL='postgres://composer:composer@localhost:5432/composer?sslmode=disable'
make migrate
make seed            # or `make seed-today` so "last N days" rules match today's data

# 2. API in demo mode (no ANTHROPIC_API_KEY needed; nothing is sent to Claude)
cd backend && LLM_PROVIDER=demo go run ./cmd/api

# 3. Frontend, in a second terminal
cd frontend && npm install && npm run dev   # http://localhost:5173
```

Routes: `/campaigns/new` (create) and `/campaigns/:id` (composer; the URL can be
reloaded or shared). There is no campaign-listing endpoint in Slice 1a; the
create page keeps a "recently opened" list in `localStorage` (IDs and names only)
and can open a campaign by ID.
