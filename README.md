# Smart Campaign Composer

AI-assisted campaign composer: a marketer writes a brief, the AI proposes a
typed audience rule, the backend validates it and shows a live,
database-backed audience preview. No real messages are sent.

Design and rationale: `docs/ARCHITECTURE.md`, `DECISIONS.md`, `docs/ROADMAP.md`,
`CLAUDE.md`. Current scope: **Slice 1a** plus the start of **Slice 1b** — brief →
interpretation (demo, or real Claude when a key is configured) → validated
segment → audience preview. Content generation, approval and sending
come in later slices.

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
3. Untick SMS and email: zero messages, matched users unchanged.
4. Reload or share `/campaigns/{id}` — the campaign is stored in PostgreSQL.

### Restart, keep data

```bash
docker compose down
```

```bash
docker compose up
```

`down` (without `-v`) keeps the `pgdata` volume. On the next start `migrate`
finds the schema current and `seed` finds users present; both exit without
changes, so campaigns and customer data are preserved. `docker compose restart`
or Ctrl+C followed by `docker compose up` behave the same.

### Start over (destroys all local data)

```bash
make reset-volumes CONFIRM=yes
```

This runs `docker compose down -v`, removing the database volume with every
campaign. The next `docker compose up --build` migrates and seeds from scratch.
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

**Use real Claude locally.** ⚠️ Every interpretation is a billable Anthropic API
request (up to three requests: the first answer, at most one repair turn and at
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
`claude-opus-5-5`, plus `_EFFORT`, `_TIMEOUT`, `_MAX_TOKENS`, and
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
