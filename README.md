# Smart Campaign Composer

See `docs/ARCHITECTURE.md`, `DECISIONS.md`, `docs/ROADMAP.md` and `CLAUDE.md`.

## Frontend development (Vue 3 + TypeScript + Vite)

Requires Node ≥ 22.13 and a PostgreSQL 16 database. The Vite dev server proxies
`/api` and `/readyz` to the Go API (`http://localhost:8080` by default; override
with `VITE_API_PROXY_TARGET`).

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

Frontend scripts: `npm test` (Vitest + jsdom, no network), `npm run typecheck`
(strict vue-tsc for app and tests), `npm run build`.

Routes: `/campaigns/new` (create) and `/campaigns/:id` (composer; the URL can be
reloaded or shared). There is no campaign-listing endpoint in Slice 1a; the
create page keeps a "recently opened" list in `localStorage` (IDs and names only)
and can open a campaign by ID.
