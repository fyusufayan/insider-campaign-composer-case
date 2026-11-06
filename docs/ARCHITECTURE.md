# Smart Campaign Composer — Architecture

Status: **Final (architecture phase complete, implementation not started).**
This document is the single source of truth for the design. `DECISIONS.md` explains *why*; `docs/ROADMAP.md` explains *when*; `CLAUDE.md` explains *how we work*.

---

## 1. Overview

A marketer writes a campaign brief in natural language. The system:

1. Interprets the brief with Claude into a **typed segmentation rule** (never SQL) plus a content brief.
2. Validates the rule on the backend and shows an **audience preview** with an unambiguous eligibility breakdown.
3. Generates SMS and email content with Claude, **streamed** to the UI; the marketer edits it.
4. **Prepares an approval**: freezes the audience snapshot (as `held` delivery rows) bound to segment version, content version and channels.
5. The marketer **confirms** the approval after seeing exactly that snapshot.
6. **Send** releases the snapshot to a PostgreSQL-backed queue.
7. A worker dispatches through a **mock provider** at ≤100 req/s per channel, with retries, 429 handling, idempotency keys and a consent/contact re-check immediately before each request.
8. A **live dashboard** streams delivery statistics.

No real messages are sent. Seed data: ~5,000 deterministic fake users.

---

## 2. Processes and infrastructure

```
 Browser ──▶ frontend (nginx: Vue SPA, proxies /api → api, proxy_buffering off)
                 │
                 ▼
 ┌─────────────────────────── api (cmd/api) ───────────────────────────┐
 │ httpapi ─▶ campaign ─▶ segment · content · ai(→ llm) · approval      │
 │ SSE: content generation stream, campaign status stream              │
 └──────────────────────────────┬──────────────────────────────────────┘
                                │ pgx
                         ┌──────▼──────┐
                         │ PostgreSQL  │  ← the only infrastructure dependency
                         └──────▲──────┘
                                │ claim / precheck / complete
 ┌──────────────────────── worker (cmd/worker, 1 replica) ─────────────┐
 │ per-channel dispatcher + sender pool · sliding-window limiter        │
 │ lease reaper · approval-expiry sweeper · completion detector         │
 │ queue-depth sampler · heartbeat                                      │
 └──────────────────────────────┬──────────────────────────────────────┘
                                │ HTTP + Idempotency-Key
                     ┌──────────▼───────────┐
                     │ mockprovider          │ fault injection, own strict 100/s limiter,
                     │ (cmd/mockprovider)    │ idempotency ledger, arrival-time recorder
                     └──────────────────────┘
 One-shot containers: migrate (cmd/migrate), seed (cmd/seed).
```

- **Modular monolith**: one Go module, one image, several entrypoints. `api` and `worker` are separate processes (different failure and load profiles) sharing domain packages and the database.
- **Local deployment (Compose, CP8)**: one backend image (`/app/api`, `/app/migrate`, `/app/seed`) and an nginx image with the built SPA. Start order is enforced with health/completion conditions: `postgres` healthy → `migrate` exits 0 (embedded migrations; no-op when current; a dirty schema is an error, never forced) → `seed` exits 0 (loads the deterministic dataset only when `users` is empty; never `-reset`) → `api` healthy (`/readyz`) → `frontend`. A failed step stops the chain, so the API never serves a partially initialized database. Data lives in the named volume `pgdata`; `docker compose down` keeps it, `down -v` discards it. nginx serves the SPA (history fallback for `/campaigns/{id}`) and proxies `/api/` and exactly `/readyz` same-origin (no CORS); the API name is re-resolved through Docker DNS, and only nginx-generated upstream failures are replaced (plain-text `502`), API responses pass through unchanged.
- **Delivery services (CP16)**: `mockprovider` (`/app/mockprovider`, internal port 8090, started once `api` is healthy because it runs the image the `api` service builds) and `worker` (`/app/worker`, started once `seed` completed and `mockprovider` is healthy; `/healthz` on internal port 9091). Neither publishes a host port, and neither receives any LLM setting or `ANTHROPIC_API_KEY`.
- **Exactly one worker replica.** The rate limiter is in-process; this is a documented constraint, not an accident. Since CP17A it is also **enforced**: only the process holding the PostgreSQL dispatch advisory lock dispatches; a second process stands by (ADR-025, §10.13).
- **Time sources**: all persisted timestamps, leases and `next_attempt_at` use database time (no cross-container clock skew). **Approval expiry is the exception to plain `now()`**: PostgreSQL `now()`/`transaction_timestamp()` is the *transaction start* time, which can predate a lock wait, so every expiry comparison and every expiry-defining timestamp (`prepared_at`, `approved_at`, `expires_at`) uses `clock_timestamp()` evaluated **after** the campaign row lock is acquired (§5.1). The rate limiter uses the worker's monotonic clock. The mock provider uses its own monotonic clock for arrival measurement.

---

## 3. Module boundaries

```
backend/internal/
  platform/   config, db pool + migrations, slog, metrics registry, SSE writer, clock
  httpapi/    router (stdlib ServeMux), middleware, DTOs, error mapping, handlers  — no business logic
  campaign/   Campaign aggregate, campaign state machine, service, repository
  segment/    DSL types + strict decode, field registry, validator, compiler (→ SQL + args), audience stats
  content/    {{var}} template parser/renderer, variable registry, content validator (§8)
  ai/         prompts (versioned), JSON schemas (generated from registries), Interpreter, Generator,
              repair loop, streaming JSON field extractor
  llm/        Client interface; anthropic (SDK v2), scripted (tests), demo (keyword mock)
  approval/   prepare / confirm / withdraw / expire / consume; snapshot creation; the ONLY inserter of deliveries
  delivery/   claim, precheck, complete, reaper, retry policy, sliding-window limiter, gated provider sender,
              dispatcher, completion detector, stats queries
  provider/   HTTP client to the provider, response classification
  mockprovider/ server, fault injection, idempotency ledger, arrival recorder
```

**Dependency rules**
- `httpapi` → services (`campaign`, `approval`, `segment`, `content`, `ai`, `delivery` stats). Never → repositories directly.
- `ai` → `llm`, `segment` (registry, validator), `content` (validator). `llm` depends on nothing internal.
- `approval` → `segment` (compiler), `content` (validator), `campaign` (state transitions within the same tx).
- `delivery` → `provider`, `content` (renderer). `delivery` never imports `ai` or `llm`.
- Interfaces exist only where a test double is needed: `llm.Client`, `delivery.Sender` (provider), `platform.Clock`. Repositories are concrete and tested against real PostgreSQL.

---

## 4. Data model

```sql
-- ===== Customer data (seeded; read-only for the application except consents in tests) =====
CREATE TYPE channel AS ENUM ('sms','email');

CREATE TABLE users (
  id              BIGSERIAL PRIMARY KEY,
  first_name      TEXT NOT NULL CHECK (char_length(first_name) BETWEEN 1 AND 20),
  last_name       TEXT NOT NULL CHECK (char_length(last_name)  BETWEEN 1 AND 25),
  email           TEXT UNIQUE,                      -- NULL = no email contact
  phone           TEXT UNIQUE,                      -- E.164; NULL = no SMS contact
  city            TEXT NOT NULL,                    -- display form, e.g. 'İstanbul'
  city_normalized TEXT NOT NULL,                    -- Turkish-aware folded key, e.g. 'istanbul'
  country         CHAR(2) NOT NULL DEFAULT 'TR',
  gender          TEXT NOT NULL CHECK (gender IN ('female','male','other','unknown')),
  birth_date      DATE,
  language        TEXT NOT NULL DEFAULT 'tr',
  signup_at       TIMESTAMPTZ NOT NULL,
  last_active_at  TIMESTAMPTZ
);
CREATE INDEX ON users (city_normalized);

CREATE TABLE channel_consents (
  user_id    BIGINT  NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  channel    channel NOT NULL,
  status     TEXT    NOT NULL CHECK (status IN ('opted_in','opted_out')),
  source     TEXT    NOT NULL,
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  PRIMARY KEY (user_id, channel)                    -- missing row ⇒ not consented
);

CREATE TABLE cart_events (
  id BIGSERIAL PRIMARY KEY,
  user_id BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  product_id TEXT NOT NULL, category TEXT NOT NULL,
  quantity INT NOT NULL CHECK (quantity > 0), unit_price NUMERIC(12,2) NOT NULL,
  occurred_at TIMESTAMPTZ NOT NULL
);
CREATE INDEX ON cart_events (user_id, occurred_at DESC);

CREATE TABLE purchases (
  id BIGSERIAL PRIMARY KEY,
  user_id BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  order_total NUMERIC(12,2) NOT NULL, category TEXT,
  purchased_at TIMESTAMPTZ NOT NULL
);
CREATE INDEX ON purchases (user_id, purchased_at DESC);

-- ===== Campaign authoring =====
CREATE TABLE campaigns (
  id                 UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  name               TEXT NOT NULL CHECK (char_length(name) BETWEEN 1 AND 120),
  brief              TEXT NOT NULL CHECK (char_length(brief) BETWEEN 10 AND 2000),
  status             TEXT NOT NULL CHECK (status IN ('draft','segmented','content_ready','pending_approval',
                                                     'approved','sending','completed','completed_with_failures','cancelled')),
  channels           channel[] NOT NULL DEFAULT '{}',
  segment_rule       JSONB,                         -- validated + normalized DSL only
  segment_version    INT NOT NULL DEFAULT 0,        -- +1 on every rule change
  interpretation     JSONB,                         -- {summary, assumptions[], unsupported[], model, prompt_version}
  content_brief      JSONB,                         -- {goal, tone, length, offer}
  current_content_id BIGINT,                        -- FK added below
  active_approval_id UUID,                          -- FK added below; set while an approval is pending/approved/consumed
  version            INT NOT NULL DEFAULT 1,        -- optimistic concurrency for marketer edits
  send_started_at    TIMESTAMPTZ, completed_at TIMESTAMPTZ, cancelled_at TIMESTAMPTZ,
  created_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at         TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE campaign_contents (                    -- immutable versions
  id             BIGSERIAL PRIMARY KEY,
  campaign_id    UUID NOT NULL REFERENCES campaigns(id) ON DELETE CASCADE,
  version        INT  NOT NULL,
  sms_body       TEXT,                              -- NULL if not produced
  email_subject  TEXT,
  email_body     TEXT,
  origin         TEXT NOT NULL CHECK (origin IN ('ai','edited')),
  model          TEXT,                              -- set when origin = 'ai'
  prompt_version TEXT,
  created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
  UNIQUE (campaign_id, version)
);
ALTER TABLE campaigns ADD FOREIGN KEY (current_content_id) REFERENCES campaign_contents(id);

-- ===== Approval (binds segment version + content version + channels + snapshot) =====
CREATE TABLE campaign_approvals (
  id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  campaign_id     UUID NOT NULL REFERENCES campaigns(id),
  status          TEXT NOT NULL CHECK (status IN ('pending','approved','consumed','revoked','expired')),
  revoke_reason   TEXT CHECK (revoke_reason IN ('superseded','edited','withdrawn','campaign_cancelled')),
  segment_version INT    NOT NULL,
  segment_rule    JSONB  NOT NULL,                  -- frozen copy
  content_id      BIGINT NOT NULL REFERENCES campaign_contents(id),
  channels        channel[] NOT NULL CHECK (cardinality(channels) > 0),
  audience_stats  JSONB  NOT NULL,                  -- exact partition shown to the marketer (§6.5)
  snapshot_count  INT    NOT NULL CHECK (snapshot_count > 0),
  snapshot_digest TEXT   NOT NULL,                  -- sha256 over sorted (user_id, channel)
  prepared_at     TIMESTAMPTZ NOT NULL,             -- clock_timestamp() after the campaign lock (no DEFAULT now())
  expires_at      TIMESTAMPTZ NOT NULL,             -- pending: prepared_at+15m; approved: approved_at+24h
  approved_at     TIMESTAMPTZ, approved_by TEXT,
  consumed_at     TIMESTAMPTZ,
  closed_at       TIMESTAMPTZ,                      -- set on revoked/expired
  CHECK ((status = 'revoked') = (revoke_reason IS NOT NULL)),
  CHECK (status NOT IN ('approved','consumed') OR (approved_at IS NOT NULL AND approved_by IS NOT NULL)),
  CHECK (status <> 'consumed' OR consumed_at IS NOT NULL)
);
CREATE UNIQUE INDEX approvals_one_live     ON campaign_approvals (campaign_id) WHERE status IN ('pending','approved');
CREATE UNIQUE INDEX approvals_one_consumed ON campaign_approvals (campaign_id) WHERE status = 'consumed';
CREATE INDEX approvals_expiry ON campaign_approvals (expires_at) WHERE status IN ('pending','approved');
ALTER TABLE campaigns ADD FOREIGN KEY (active_approval_id) REFERENCES campaign_approvals(id);

-- ===== Delivery: one row per (campaign, user, channel). Snapshot entry AND queue job. =====
CREATE TABLE campaign_deliveries (
  id                    BIGSERIAL PRIMARY KEY,
  campaign_id           UUID    NOT NULL REFERENCES campaigns(id),
  approval_id           UUID    NOT NULL REFERENCES campaign_approvals(id),
  user_id               BIGINT  NOT NULL REFERENCES users(id),
  channel               channel NOT NULL,
  vars                  JSONB   NOT NULL,           -- frozen personalization values at prepare
  idempotency_key       TEXT    NOT NULL UNIQUE,    -- '<campaign_id>:<user_id>:<channel>'
  destination           TEXT,                       -- NULL until first precheck; then immutable
  status                TEXT    NOT NULL DEFAULT 'held' CHECK (status IN
                          ('held','pending','in_flight','retry_scheduled','sent','failed',
                           'skipped_consent_revoked','skipped_no_contact','cancelled')),
  attempts              INT NOT NULL DEFAULT 0,     -- completed non-429 provider attempts (incl. unknown)
  rate_limited_attempts INT NOT NULL DEFAULT 0,     -- 429 responses
  outcome_uncertain     BOOLEAN NOT NULL DEFAULT false, -- an attempt may have been delivered without confirmation
  next_attempt_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
  lease_until           TIMESTAMPTZ,
  leased_by             TEXT,
  dispatch_started_at   TIMESTAMPTZ,                -- set by precheck of the current attempt; cleared on completion
  provider_message_id   TEXT,
  last_error            TEXT,
  sent_at               TIMESTAMPTZ,
  created_at            TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at            TIMESTAMPTZ NOT NULL DEFAULT now(),
  UNIQUE (campaign_id, user_id, channel),
  CHECK (status <> 'sent' OR (destination IS NOT NULL AND provider_message_id IS NOT NULL)),
  CHECK (status <> 'in_flight' OR (lease_until IS NOT NULL AND leased_by IS NOT NULL))
);
CREATE INDEX deliveries_ready ON campaign_deliveries (channel, next_attempt_at)
  WHERE status IN ('pending','retry_scheduled');
CREATE INDEX deliveries_lease ON campaign_deliveries (lease_until) WHERE status = 'in_flight';
CREATE INDEX deliveries_stats ON campaign_deliveries (campaign_id, channel, status);

CREATE TABLE delivery_attempts (
  id             BIGSERIAL PRIMARY KEY,
  delivery_id    BIGINT NOT NULL REFERENCES campaign_deliveries(id),
  attempt_no     INT NOT NULL,                      -- 1-based across all outcomes, incl. 429
  destination    TEXT NOT NULL,
  outcome        TEXT NOT NULL CHECK (outcome IN ('accepted','replayed','rate_limited','transient_error',
                                                  'timeout','permanent_error','unknown')),
  http_status    INT,
  retry_after_ms INT,
  error          TEXT,
  started_at     TIMESTAMPTZ NOT NULL,              -- = dispatch_started_at
  finished_at    TIMESTAMPTZ NOT NULL,
  UNIQUE (delivery_id, attempt_no)
);

CREATE TABLE worker_heartbeats (worker_id TEXT PRIMARY KEY, last_seen_at TIMESTAMPTZ NOT NULL);

-- CP17A (migration 000002, ADR-025): durable per-channel 429 pause; only ever extended.
CREATE TABLE channel_pauses (channel channel PRIMARY KEY, paused_until TIMESTAMPTZ NOT NULL,
                             updated_at TIMESTAMPTZ NOT NULL DEFAULT now());
```

**Structural invariants enforced by the schema**
- One delivery per (campaign, user, channel): `UNIQUE (campaign_id, user_id, channel)` and `UNIQUE (idempotency_key)`.
- At most one live approval and at most one consumed approval per campaign (partial unique indexes) ⇒ at most one snapshot can ever be dispatched per campaign.
- A `sent` row always has a destination and a provider message id.

**Rows that the application deletes**: only `held` delivery rows, and only in the same transaction that moves their approval to `revoked` or `expired`. No other delivery row is ever deleted.

**Development seed reset** (`cmd/seed -reset`, `make seed-reset`, `make seed-today`; not part of the application): replaces the customer tables (`users`, `channel_consents`, `cart_events`, `purchases`) in one transaction and restarts their ID sequences, so customer IDs are reused. It is therefore refused unless the database has **no campaigns and no delivery rows**; campaign, content and approval rows are never deleted, and `TRUNCATE … CASCADE` is never used. Concurrency: the transaction takes `LOCK TABLE campaigns IN SHARE MODE` (after the seed's advisory lock) before checking. This waits for any uncommitted campaign insert/update and blocks new ones until the reset commits; approvals and deliveries reference a campaign, so none can appear either. The reset never waits on a lock after that, so it cannot deadlock with approval preparation. To reset a database that has campaigns, recreate it (migrate down/up).

---

## 5. State machines and transactions

### 5.1 Lock ordering (all authoring/approval transactions)
`campaigns` row (`SELECT … FOR UPDATE`) → `campaign_approvals` row → `campaign_deliveries` rows.
Every transaction that changes campaign or approval status starts by locking the campaign row. This serializes prepare / confirm / withdraw / expire / send / cancel for one campaign. Worker per-delivery statements do **not** lock the campaign row; they read campaign/approval status inside their own statement (§10).

**Expiry time semantics.** PostgreSQL `now()` (= `transaction_timestamp()`) is fixed at transaction start. A transaction may start before an approval expires, wait on the campaign row lock, and acquire it after expiry; a comparison against `now()` would then wrongly treat the approval as valid. Therefore:
1. Every campaign/approval transition locks the campaign row **first**.
2. Expiry is checked only **after** the lock is acquired, in statements executed after the lock statement returns.
3. Every expiry comparison uses `clock_timestamp()` (actual database time at evaluation), never `now()`, `CURRENT_TIMESTAMP` or a timestamp computed in Go.
4. Timestamps that define expiry (`prepared_at`, `approved_at`, `expires_at`) are also set from `clock_timestamp()` after the lock, so a lock wait cannot shorten or lengthen a TTL.
5. The sweeper and lazy `ExpireDue` follow the same rules (lock first, then `expires_at <= clock_timestamp()`).

Because `clock_timestamp()` is monotonic within the database server and transitions on one campaign are serialized by the lock, "consume" and "expire" can never both succeed: whichever runs first under the lock decides, using the real time at that moment. Other timestamps (`created_at`, `updated_at`, leases, `next_attempt_at`) keep `now()`; they carry no correctness-critical comparison against a lock wait.

### 5.2 Campaign state machine

| From | Event | To | Side effects (same transaction) |
|---|---|---|---|
| — | create | `draft` | |
| draft, segmented, content_ready, pending_approval, approved | interpret succeeds | `segmented` (no content) / `content_ready` (has content) | `segment_version+1`; live approval → revoked(`edited`) + delete held rows |
| segmented, content_ready, pending_approval, approved | set channels | same base status as above | live approval → revoked(`edited`) + delete held rows |
| segmented, content_ready, pending_approval, approved | new content version (generate or edit) | `content_ready` | live approval → revoked(`edited`) + delete held rows |
| content_ready, pending_approval, approved¹ | prepare | `pending_approval` | live approval → revoked(`superseded`) + delete its held rows; insert new approval + held rows |
| pending_approval | confirm | `approved` | approval pending → approved |
| pending_approval, approved | withdraw | `content_ready` | approval → revoked(`withdrawn`) + delete held rows |
| pending_approval, approved | expiry | `content_ready` | approval → expired + delete held rows; `version+1` |
| approved | send | `sending` | approval approved → consumed; held → pending |
| sending | completion (worker) | `completed` / `completed_with_failures` | none |
| draft, segmented, content_ready | cancel | `cancelled` | none |
| pending_approval, approved | cancel | `cancelled` | approval → revoked(`campaign_cancelled`) + delete held rows |
| sending | cancel | `cancelled` | pending/retry_scheduled → cancelled; in_flight rows finish (§10.8) |
| completed*, cancelled | anything | — | `409 invalid_state` |

¹ **Deliberate product-safe deviation (CP14, approved in review):** prepare in `approved` is refused (`409 invalid_state`, "withdraw the approval first"), so a plain prepare can never silently invalidate a confirmed approval. CP15 keeps this: the explicit re-preparation contract **is** withdraw (version-guarded, explicit second step in the UI) followed by prepare; no combined "replace" route was added (§9.2). An *expired* approved approval is closed by lazy expiry (version + 1); after a reload the marketer prepares from `content_ready`.

"Base status" after an invalidating edit = `content_ready` if `current_content_id` is set, else `segmented` (or `draft` if no rule). `active_approval_id` is cleared whenever the approval becomes revoked/expired and set on prepare.

`completed_with_failures` ⇔ at least one `failed` row. Skipped and cancelled rows are not failures.

### 5.3 Approval state machine

```
           confirm (CAS: status=pending AND expires_at > clock_timestamp())
 pending ───────────────────────────────▶ approved ──send (CAS: status=approved AND expires_at > clock_timestamp())──▶ consumed (terminal)
    │  expires_at ≤ clock_timestamp()        │  expires_at ≤ clock_timestamp()
    ├──────────────▶ expired (terminal)  ◀───┤
    │  supersede / edit / withdraw / cancel  │
    └──────────────▶ revoked (terminal)  ◀───┘
```
- `pending.expires_at = prepared_at + 15 min`; on confirm `expires_at := approved_at + 24 h` — both base times are `clock_timestamp()` after the campaign lock (§5.1).
- **Leaving `pending`/`approved` for `revoked`/`expired` always deletes the approval's `held` rows and resets the campaign status in the same transaction.** Therefore a held row exists iff its approval is live.
- `consumed` never expires and is never revoked; cancelling a sending campaign acts on delivery rows, not on the approval.

**Expiry mechanism** (correctness does not depend on timing of the sweeper):
1. Every consuming CAS (`confirm`, `send`) runs after the campaign lock and includes `expires_at > clock_timestamp()`. An expired approval can never be confirmed or consumed even if the sweeper has not run yet, including when the approval expired while the transaction was waiting for the lock.
2. `approval.ExpireDue(tx, campaignID)` performs the expire transition if due (`expires_at <= clock_timestamp()`, evaluated after the lock). It is called (a) by the worker sweeper every 30 s — which selects candidates with a cheap unlocked query, then for each one locks the campaign row and re-checks with `clock_timestamp()` — and (b) at the start of every API operation that reads or mutates a campaign with a live approval, so the API never presents a stale state.
3. **Verification** (PostgreSQL integration test, table-driven over `confirm` and `send`):
   1. Create an approval whose `expires_at = clock_timestamp() + 300 ms` (controlled expiry; TTL injected through config for the test).
   2. Goroutine A opens a transaction and locks the campaign row (`SELECT … FOR UPDATE`), then signals `locked` on a channel.
   3. After `locked`, goroutine B calls the production `Confirm`/`Send` service method. The test waits until B is actually blocked by bounded polling of `pg_stat_activity` (`wait_event_type = 'Lock'`, 2 s deadline) and records B's `xact_start`, asserting `xact_start < expires_at` (B's transaction started before expiry).
   4. A waits until the database clock passes expiry by bounded polling of `SELECT clock_timestamp() > $expires_at` (2 s deadline), then commits, releasing the lock.
   5. B's result is received on a channel: it must be `409 approval_expired`; the approval must not be `approved`/`consumed`, no delivery row may be `pending`, and the campaign must be `content_ready` with its held rows deleted (B's lazy `ExpireDue` performed the expiry).
   A third case does the same for the sweeper: the sweeper blocks on the lock before expiry and must expire the approval once it gets the lock after expiry. No `time.Sleep` is used for coordination; only channels and deadline-bounded polling of database state.

### 5.4 Delivery state machine

```
held ──send tx──▶ pending ──claim──▶ in_flight ──2xx / replay──────────────────▶ sent
  │                  ▲   │              │ ──429 (rate_limited_attempts < 10)───▶ retry_scheduled ─┐
  │ approval         │   │              │ ──5xx/timeout/conn (attempts < 5)────▶ retry_scheduled ─┤ (claim again)
  │ revoked/expired  │   │              │ ──4xx permanent / attempts exhausted──▶ failed           │
  ▼                  │   │              │ ──precheck: consent revoked───────────▶ skipped_consent_revoked
(deleted)            │   │              │ ──precheck: no contact (first attempt)▶ skipped_no_contact
                     │   │              │ ──precheck or outcome: campaign cancelled ▶ cancelled
                     │   └─cancel──▶ cancelled
                     └──── graceful shutdown release (no precheck happened) ◀── in_flight
     in_flight with lease_until < now() ──reaper──▶ retry_scheduled | failed | cancelled (attempt 'unknown' if dispatch had started)
```
Claim selects only rows with `status IN ('pending','retry_scheduled') AND next_attempt_at <= now()` **joined to** `campaign_approvals.status = 'consumed'` and `campaigns.status = 'sending'`. A `held` row, a row of a non-consumed approval, or a row of a cancelled campaign can never be claimed.

### 5.5 Transaction catalogue

| Operation | Statements (one transaction unless noted) |
|---|---|
| **Prepare** | lock campaign; `ExpireDue`; require status ∈ {content_ready, pending_approval, approved}, segment present, ≥1 channel, current content **valid for the selected channels**; revoke live approval (`superseded`) + delete its held rows; run compiled audience query; `INSERT … SELECT` held rows (vars frozen, idempotency keys); compute stats + digest; assert row count = stats eligible sum, else rollback; `snapshot_count = 0` ⇒ rollback + `422 empty_audience`; insert approval (pending, `prepared_at = clock_timestamp()`, `expires_at = clock_timestamp() + $pending_ttl`, evaluated after the lock); campaign → pending_approval, `active_approval_id` set |
| **Confirm** | lock campaign; `ExpireDue`; `UPDATE campaign_approvals SET status='approved', approved_at=clock_timestamp(), approved_by=$u, expires_at=clock_timestamp()+$approved_ttl WHERE id=$a AND campaign_id=$c AND status='pending' AND expires_at > clock_timestamp() AND segment_version=$sv AND content_id=$cid AND channels=$ch` → 0 rows ⇒ 409 (`approval_expired` / `approval_mismatch` / `invalid_state`, determined by re-read); campaign → approved |
| **Withdraw** | lock campaign; approval → revoked(`withdrawn`); delete held rows; campaign → content_ready |
| **Expire** | lock campaign; approval → expired WHERE status IN (pending, approved) AND expires_at ≤ clock_timestamp(); delete held rows; campaign → content_ready; clear `active_approval_id`; `version+1` (nothing written when nothing is due) |
| **Send** | lock campaign; `ExpireDue`; require campaign approved and `active_approval_id = $a`; `UPDATE campaign_approvals SET status='consumed', consumed_at=clock_timestamp() WHERE id=$a AND status='approved' AND expires_at > clock_timestamp()` → 0 rows ⇒ 409; `UPDATE campaign_deliveries SET status='pending' WHERE approval_id=$a AND status='held'` → count must equal `snapshot_count`, else rollback; campaign → sending, `send_started_at=now()`. Repeat call on a sending campaign with the same approval id ⇒ `200` current state (idempotent) |
| **Cancel** | lock campaign; per §5.2 |
| **Claim** (worker) | single statement: `UPDATE … SET status='in_flight', lease_until=now()+lease, leased_by=$w WHERE id IN (SELECT d.id … JOIN approvals/campaigns … FOR UPDATE OF d SKIP LOCKED LIMIT $n) RETURNING …` |
| **Precheck** (worker) | single statement, §10.3 |
| **Complete** (worker) | single tx: guarded `UPDATE campaign_deliveries … WHERE id=$d AND leased_by=$w AND status='in_flight'` + insert `delivery_attempts` row |
| **Reap** (worker) | per expired lease, single tx: §10.7 |
| **Completion detection** (worker) | `UPDATE campaigns SET status = CASE WHEN failed>0 … END, completed_at=now() WHERE id=$c AND status='sending' AND NOT EXISTS (… status IN ('pending','in_flight','retry_scheduled'))` |

No transaction is ever held open across an HTTP request (LLM or provider).

---

## 6. Audience segmentation

### 6.1 DSL v1 (typed, shallow)
```
Rule      := { "match": "all"|"any", "conditions": [Condition, …], "groups"?: [Group, …] }
Group     := { "match": "all"|"any", "conditions": [Condition, …] }          -- groups do not nest
Condition := Attribute | Event | Behavior
Attribute := { "type":"attribute", "field": F, "op": Op, "value": V }
Event     := { "type":"event", "event":"cart_add"|"purchase",
               "op":"exists"|"not_exists"|"count_gte"|"count_lte", "count"?: int,
               "within_days": int, "category_in"?: [string] }
Behavior  := { "type":"behavior", "predicate":"abandoned_cart", "within_days": int }
```
`abandoned_cart(N)` = the user has a cart event in the last N days with **no purchase at or after that cart event**.

**Event counts.** The count of an event condition is the number of the user's events of that kind within the window (and in `category_in`, when present); a user with no matching events has count **0**. Therefore, for identical event, window and category filters, `count_lte: 0` ≡ `not_exists`, `count_gte: 1` ≡ `exists`, and `count_gte: 0` matches every user. The compiler must honour this (e.g. a correlated `COUNT(*)` subquery, never an inner join that drops zero-count users).

**Field registry (allow-list)**

| field | operators | value |
|---|---|---|
| city | eq, neq, in, not_in | known city; matched on `city_normalized` (Turkish-aware folding) |
| country | eq, in | ISO-3166 alpha-2 |
| gender | eq, in | female, male, other, unknown |
| age | gte, lte, between | 13–120 |
| signup_days_ago | gte, lte, between | 0–3650 |
| last_active_days_ago | gte, lte | 0–3650 |
| language | eq, in | tr, en |

Limits: `within_days` 1–365; `count` 0–100; ≤10 conditions per group; ≤5 groups; `in` lists ≤50 items. Consent and contact fields are **not** part of the DSL; any attempt is rejected with `consent_is_backend_enforced`.

Value shapes (fixed per operator): `eq`/`neq` → string; `in`/`not_in` → non-empty array of strings; `gte`/`lte` → integer; `between` → `{"min": int, "max": int}` (inclusive, `min ≤ max`). A rule needs ≥1 condition in total; groups need ≥1 condition. `"version"` is optional on input (must be `1`) and always present in the normalized form. `category_in` values must be known product categories (`segment.Categories()`; the seed generator takes its category names from the same catalog). String values are canonicalized case-insensitively (city → folded key, country → upper-case alpha-2, gender/language/category → lower case); `in` lists and `category_in` are sorted and de-duplicated; condition and group order is preserved.

### 6.2 Example interpretation output (example brief)
```json
{
  "segment": { "match": "all", "conditions": [
    { "type": "attribute", "field": "city", "op": "eq", "value": "Istanbul" },
    { "type": "behavior", "predicate": "abandoned_cart", "within_days": 30 } ] },
  "suggested_channels": ["sms", "email"],
  "content_brief": { "goal": "cart_recovery", "tone": "friendly", "length": "short",
                     "offer": { "kind": "percent_discount", "value": 30 } },
  "interpretation": "Users in Istanbul who added a product to their cart in the last 30 days and have not purchased since.",
  "assumptions": ["'Did not complete a purchase' means no purchase after the cart addition."],
  "unsupported": []
}
```

### 6.3 Validation pipeline
1. **Generation-time shape** — structured outputs JSON schema (enums for type/field/op/predicate/event, `additionalProperties:false`, `anyOf` for condition variants). The schema cannot express lengths or numeric ranges (API limitation), so:
2. **Strict decode** — `json.Decoder.DisallowUnknownFields`, discriminator-based `UnmarshalJSON`.
3. **Semantic validation** — registry-driven: field exists, operator allowed for field, value type and range, `count` only with `count_*`, limits, city resolves, consent fields rejected. Errors: `{path, code, message, allowed?}`.
4. **Normalize** — canonical city keys, sorted `in` lists, stamp `"version": 1`. Only the normalized rule is persisted.
The same validator handles every source of rules.

### 6.4 Compilation
`segment.Compile(rule) (sql string, args []any, err error)`: each registry entry owns a fragment builder; identifiers come only from Go constants; every value is a bind parameter. Behavioural predicates compile to `EXISTS` / `NOT EXISTS` subqueries over `cart_events` / `purchases` with `now() - make_interval(days => $n)`. Queries run in a read-only transaction with `SET LOCAL statement_timeout = '5s'`.

Clarifications fixed by the compiler (CP4):
- **Input/output.** `Compile` accepts only a non-zero `segment.Validated` and returns `SELECT u.id FROM users AS u WHERE (…)` — one row per matched user (no joins; events are correlated subqueries). Placeholders `$1..$n` are numbered in order of appearance; the statement is a pure function of the normalized rule.
- **Composition.** The rule's `match` combines each top-level condition and each group (top-level conditions first, then groups in order); a group is one operand whose conditions are combined with the group's own `match`.
- **Windows.** "Last N days" is `now() - N days <= t <= now()` (both edges inclusive); future-dated events never match, for `cart_add`, `purchase`, counts and `not_exists` alike. The evaluation time is the statement's `now()` (one value per transaction). This is a historical audience query, so `now()` is correct here; approval expiry keeps using `clock_timestamp()` after the campaign lock (§5, ADR-006).
- **`*_days_ago`** is the number of whole elapsed days, `floor((now() - t) / 1 day)`; **age** is whole years between `birth_date` and the current UTC date. A NULL `birth_date` / `last_active_at`, or a future-dated `signup_at` / `last_active_at`, never satisfies a condition.
- **`abandoned_cart(N)`**: only cart events in the window `[now() - N days, now()]` are considered; a purchase of the user (any or no category, not limited to the window) with `cart.occurred_at <= purchased_at <= now()` recovers that cart, so a future-dated purchase does not; one unrecovered cart event in the window suffices.
- **Category filters** never match a purchase whose category is NULL.
- **Empty channel selection**: `by_channel` is empty, `messages = 0`, `reach_users.unreachable = matched_users`. With one selected channel, its eligible users are `sms_only`/`email_only` and everyone else is `unreachable`.

### 6.5 Audience statistics (unambiguous partition)
For each **selected** channel, every matched user is placed in exactly one category, evaluated in order:
1. `no_contact` — no phone (SMS) / no email (email).
2. `no_consent` — has contact, consent row missing or `opted_out`.
3. `eligible` — has contact and `opted_in`.

Identities (asserted in tests): per channel `matched_users = eligible + no_consent + no_contact`; across channels `matched_users = both + sms_only + email_only + unreachable`; `messages = Σ eligible`. Users and messages are never summed together. Overlapping "independent" metrics are deliberately not reported.

```json
"audience": {
  "matched_users": 412,
  "by_channel": { "sms":   { "eligible": 251, "no_consent": 140, "no_contact": 21 },
                  "email": { "eligible": 318, "no_consent":  82, "no_contact": 12 } },
  "reach_users": { "both": 202, "sms_only": 49, "email_only": 116, "unreachable": 45 },
  "messages": 569,
  "computed_at": "…"
}
```
The prepare step computes this partition from the same query that inserts the held rows; `snapshot_count = messages` is asserted inside the transaction.

---

## 7. AI integration

### 7.1 SDK
- Module `github.com/anthropics/anthropic-sdk-go/v2`, pinned **v2.5.0** in `go.mod` (checksum in `go.sum`). Requires **Go 1.25+**. Sends `anthropic-version: 2026-10-21` — **no** `temperature`/`top_p`/`top_k` are ever sent.
- Client (`llm/anthropic.go`, `llm.NewAnthropic`): `anthropic.NewClient(option.WithoutEnvironmentDefaults(), option.WithAPIKey(key), option.WithHTTPClient(hc), option.WithMaxRetries(CLAUDE_MAX_RETRIES))`. `WithoutEnvironmentDefaults` disables the SDK's own credential chain and env autoload (`ANTHROPIC_AUTH_TOKEN`, profiles, federation, `ANTHROPIC_BASE_URL`, `ANTHROPIC_CUSTOM_HEADERS`), so the key from our config is the only credential (the SDK sends it as `Authorization: Bearer`). The key is never logged. Construction performs no I/O; there is no startup model call.
- Non-streaming: `client.Messages.New(ctx, anthropic.MessageNewParams{…})`.
- Streaming: `client.Messages.NewStreaming(ctx, params)`; for each event `message.Accumulate(event)` (check error) and switch on `event.AsAny()`; `ContentBlockDeltaEvent` → `TextDelta` feeds the field extractor; a thinking content-block start emits SSE `status: thinking`; finally check `stream.Err()`.
- Structured output (stable API, no beta header): `OutputConfig: anthropic.OutputConfigParam{Effort: …, Format: anthropic.JSONOutputFormatParam{Schema: <map[string]any>}}`, serialized as `"output_config":{"effort":"low","format":{"type":"json_schema","schema":{…}}}` (verified by a transport-level test that compares the captured body with `InterpretationSchemaJSON`). Thinking is sent explicitly as `{"type":"adaptive"}` so every model gets the same reasoning configuration (on `claude-opus-5-5` this equals omitting it); a model without adaptive thinking fails with a 400 instead of silently running without it. `CLAUDE_*_EFFORT` maps 1:1 (`low|medium|high|max`); an empty or unknown effort is rejected locally rather than letting the model default apply. The interpretation schema stays within the structured-output subset and limits (all objects closed, non-recursive `$defs`, 3 optional properties of 24, 3 unions of 16). The JSON arrives as a text block and is decoded by our strict decoder. Schemas are generated deterministically from the registries (stable key order) so the API's compiled-grammar cache (24 h) is reused.
- Retries: the SDK retries connection errors, 408, 409, 429, ≥500 (honouring `x-should-retry`) up to `CLAUDE_MAX_RETRIES` (default 2, range 0–5) with 0.5 s·2ⁿ backoff (≤ 8 s, jittered) or the server's `retry-after(-ms)`. The **only** time budget is the interpreter's `context.WithTimeout(CLAUDE_SEGMENT_TIMEOUT)`, which spans all SDK attempts and is never exceeded (a long `retry-after` simply ends in a timeout). **No** per-attempt `option.WithRequestTimeout` is set: the SDK retries an attempt that hit it, which would abandon and re-bill a slow but healthy generation. Worst case per interpretation: 3 logical requests (answer, repair, one truncation retry) × (1 + retries) HTTP attempts, all inside the one deadline.
- Errors: `errors.As(err, &apiErr *anthropic.Error)` → `llm.Error{StatusCode, RequestID}`; 408/409/429/5xx → `llm.ErrUnavailable` (retryable), other 4xx (400 incl. schema rejection, 401, 403, 404 unknown model, 413) → `llm.ErrRejected`; transport errors → `ErrUnavailable` (status 0); the caller's cancellation/deadline → the context error. The message is rebuilt from metadata (status, API error type, request id) because `*anthropic.Error.Error()` embeds the raw response body; transport failures get the fixed message "anthropic transport failure" (their text may carry URLs, proxy credentials or quoted bytes). 2xx responses with a non-text content block (anything but text/thinking) or `end_turn` without text are `ErrUnavailable`, never output. Stop reasons other than end_turn/max_tokens/refusal/stop_sequence pass through verbatim and are rejected by the interpreter. `ai` maps these to `ErrLLMUnavailable`, `ErrLLMRejected`, `ErrLLMRefused`, `ErrLLMTruncated`, `ErrOutputInvalid`. The adapter logs one metadata line per call (task, models, effort, stop reason, tokens, latency, provider request id) — never prompts, briefs, outputs or the key.
- `Stream` (implemented in CP13) uses `client.Messages.NewStreaming` with the same request as `Complete` plus `stream: true`; every event goes through `Message.Accumulate`, a thinking block start is reported once as `EventThinking`, each `text_delta` as `EventTextDelta`, and the accumulated message is converted exactly like a `Complete` response. The SDK's retries apply only to establishing the stream; an interrupted stream is never retried by the SDK. A stream that ends without `message_stop` → `ErrUnavailable` (partial text is never returned); an in-stream `error` event (HTTP status 200) is classified by its API error type (`overloaded_error`, `rate_limit_error`, `api_error`, `timeout_error` → unavailable, anything else → rejected), with the same metadata-only message. If the callback fails (client gone), the stream and its HTTP response are closed and the callback's error is returned unchanged. Covered by offline fake-HTTP tests (`anthropictest.SSE`: happy path with thinking, stop reasons, truncated stream, mid-stream error events, pre-stream HTTP errors with SDK retry, callback error, cancellation of a stalled stream, deadline).
- Model constraints honoured (Opus 5.5, Haiku 5.5, Sonnet 5.5): thinking always on (controlled by effort only), no forced `tool_choice`, no assistant prefill.
- Server-side refusal fallback (beta Messages API) is **not** used in core; refusals are surfaced to the marketer.

### 7.2 Abstraction
```go
// llm: provider-neutral
type Client interface {
    Complete(ctx context.Context, req Request) (Response, error)
    Stream(ctx context.Context, req Request, onEvent func(StreamEvent) error) (Response, error)
}
// Request{Model, System, Messages, JSONSchema, MaxTokens, Effort}; Response{Text, StopReason, Usage, Model}
```
Implementations: `anthropic` (real), `scripted` (tests: queued responses/errors, chunked streams, delays), `demo` (deterministic keyword-based results with simulated streaming; selected per §7.3; UI shows a "Mock AI" badge). The demo is a heuristic keyword matcher, not language understanding: it never emits a rule broader than the brief — when nothing is recognized, or a recognized targeting requirement cannot be expressed (unsupported concept, invalid or out-of-range value, category on an abandoned cart, only unsupported channels), it declines with field-level problems, surfaced as a non-retryable `invalid_brief` without a repair turn. Unsupported offers are content preferences and are reported in `unsupported`/`assumptions` without blocking the audience. `ai.Interpreter` and `ai.Generator` own prompts, schemas, decoding, validation and repair, so tests with `scripted` exercise all of it.

### 7.3 Configuration (model never hardcoded)
```
LLM_PROVIDER=auto|anthropic|demo        ANTHROPIC_API_KEY=…
CLAUDE_SEGMENT_MODEL=claude-haiku-5-5   CLAUDE_SEGMENT_EFFORT=low     CLAUDE_SEGMENT_TIMEOUT=45s  CLAUDE_SEGMENT_MAX_TOKENS=4000
CLAUDE_CONTENT_MODEL=claude-opus-5-5    CLAUDE_CONTENT_EFFORT=medium  CLAUDE_CONTENT_TIMEOUT=90s  CLAUDE_CONTENT_MAX_TOKENS=8000
CLAUDE_MAX_RETRIES=2                    # 0–5, SDK retries per model request
```
**Provider selection** (once, at startup; immutable for the process lifetime):

| `LLM_PROVIDER` | `ANTHROPIC_API_KEY` (non-empty after trimming) | Result |
|---|---|---|
| `auto` (default) | absent | `demo`; warning logged; `/readyz` and the UI report `llm_provider=demo` |
| `auto` | present | `anthropic` |
| `anthropic` | absent | startup fails with a configuration error |
| `anthropic` | present | `anthropic` |
| `demo` | any | `demo` (the key is ignored) |

- Only `ANTHROPIC_API_KEY` is consulted; the key is passed explicitly with `option.WithAPIKey`, so other SDK credential sources are never picked up implicitly.
- **No fallback at runtime**: when the Anthropic provider is selected, every failure (transport, 4xx/5xx after SDK retries, refusal, truncation, invalid output after the repair turn, timeout) is returned to the caller as the mapped domain error and surfaced as an actionable API error (§7.5). Demo output is never substituted.
- **Unit tests** never read `LLM_PROVIDER`; they construct `llm.Scripted` explicitly. `demo` is used only for running the app (Compose, smoke scripts; Playwright was planned as B-tier and not built) and in its own unit tests.

The model string flows config → service → `llm.Request.Model` and is persisted with every AI result. Interpretation defaults to `claude-haiku-5-5` (ADR-015; one live integration call in CP10 verified compatibility, not comparative quality); content generation keeps `claude-opus-5-5` without a recorded evaluation. The planned 12-brief comparative evaluation was not performed (ADR-015 status); switching is an env change, never a runtime fallback.

### 7.4 Prompts
Versioned templates (`segment_v1`, `content_v1`). The interpretation system prompt contains the **field catalog generated from the registry**, operator semantics, rules (never invent fields; put inexpressible requests in `unsupported`; state interpretation choices in `assumptions`) and few-shot examples. The brief is wrapped in `<brief>` tags and declared as data. The content prompt receives the content brief, selected channels, allowed variables and limits (target SMS ≤150, subject ≤70 to leave headroom), plain-text only. **No customer data is ever sent to the LLM.**

### 7.5 Failure handling
| Case | Handling |
|---|---|
| Transport 429/5xx/connection | SDK retries; then `503 llm_unavailable` (retryable); never a fallback to demo output |
| `max_tokens` | incomplete → never used; one retry with doubled `max_tokens` |
| `refusal` | `422 llm_refused` with category; marketer edits brief or writes content manually |
| JSON decode failure / semantic errors | one **repair turn**: append the assistant output and a user message listing the structured errors (append-only history); revalidate; second failure → `422 ai_output_invalid` with errors and the raw proposal |
| Stream interrupted | partial output discarded; SSE `error` |
| Client disconnect | context cancelled → SDK stream closed |

### 7.6 Streaming content
The model streams one flat JSON object with the content field names (`sms_body`, `email_subject`, `email_body`; only the selected channels' fields, §7.8). `ai/jsonstream` incrementally extracts string field values (escape- and UTF-8-safe across chunk boundaries) and emits `FieldDelta{field, text}`. On completion the full text goes through strict decode + `content.Validate`; only the validated result becomes a content version and replaces the streamed preview.

### 7.7 AI-proposed vs backend-controlled
| AI proposes (untrusted, displayed, editable) | Backend decides (deterministic) |
|---|---|
| rule draft, interpretation, assumptions, unsupported, suggested channels, content brief, copy | allowed fields/ops/ranges, SQL, consent, contact, audience partition, snapshot, limits, variables, footer, approval, rate limits, retries, idempotency |

### 7.8 Content generation as implemented (CP12)

CP12 is the backend half of Slice 2: `ai.Generator`, the content-version repository and `campaign.Service.GenerateContent`. CP13 adds streaming, the HTTP routes and the editor (§7.9, §11.2, §12).

- **Input** (`ai.GenerationInput`): the re-validated stored content brief (`ai.ParseContentBrief`, strict), the normalized segment rule (context only) and the selected channels. The raw marketer brief, customer records, contacts and delivery rows are not part of the type and never reach the prompt.
- **Prompt `content_v1`** (`ai.ContentPromptVersion`, persisted as `campaign_contents.prompt_version`): system prompt built from `content.Variables()` (names, declared maxima), the placeholder/fallback grammar, the hard limits (160/80/5,000) with the soft targets (150/70), the worst-case rule with a worked example, plain-text rules, "no invented facts", "the backend adds the unsubscribe footer", and the offer rules. The user message carries one JSON request document (`channels`, `fields`, `content_brief`, `audience_rule`) inside `<content_request>` tags — `encoding/json` escapes `<`, `>` and `&`, so no value can close the tag — plus a deterministic restatement of the required fields and the authorized offer. Output language is English (the content brief has no language field).
- **Schema** (`ai.ContentSchema`): a closed flat object whose properties are exactly the selected channels' fields (`sms_body`; `email_subject` + `email_body`), all required strings. Three deterministic schemas exist.
- **Decoding** (`ai.ParseGeneratedContent`): ≤ 64 KiB, one JSON value, valid UTF-8, no duplicate or unknown properties (a field of an unselected channel is unknown), every field a string (null/other types rejected); then `content.Parse` for the selected channels (the single content validator) and the **offer policy**; all problems of both are reported together.
- **Offer policy** (brief-dependent, so it lives in `ai`, not `content`): Terms fall into three groups: discount wording (`discount`, `indirim`, `% off`, `percent off`), separate incentives (coupon/kupon, voucher, promo code/promosyon, cashback, buy one get one/BOGO, half price/yarı fiyat, free gift/hediye) and free shipping. `none` forbids percentages and all three groups; `percent_discount N` allows only `N%` (also `%N`, `N percent`, `yüzde N`) with discount wording ("20% off", "Save 20%", "%20 indirim") and forbids separate incentives and free shipping; `free_shipping` forbids percentages, discount wording and separate incentives. Monetary amounts (`₺50`, `500 TL`, `$5`) are always rejected (`unauthorized_claim`). Matching is case-insensitive over English and common Turkish phrasings — conservative (it can reject "100% cotton" when no discount is authorized) and not language understanding. An authorized offer that a message does not mention yields the warning `offer_not_mentioned`.
- **Model-call policy** is shared with the interpreter (`ai/call.go`): only `end_turn` is output; `refusal` → `ErrLLMRefused` (no repair); `max_tokens` → one retry with a doubled budget, then `ErrLLMTruncated`; other stop reasons → `ErrLLMTruncated`; one deadline (`CLAUDE_CONTENT_TIMEOUT`) spans all calls. Invalid output gets **one** repair turn (append-only history, structured field errors, same schema); a second failure → `ErrOutputInvalid` with details and the bounded raw proposal. At most 3 logical calls.
- **Configuration**: `CLAUDE_CONTENT_MODEL` (default `claude-opus-5-5`), `_EFFORT` (`medium`), `_TIMEOUT` (`90s`), `_MAX_TOKENS` (`8000`), validated like the segment task; logged at startup with `content_prompt_version`.
- **Demo** (`TaskContentGeneration` responder): fixed English templates chosen by goal keyword (cart recovery / win back / generic), tone (friendly / urgent / formal) and length (email paragraphs); the offer is rendered only from its validated kind and integer value; no brief text is ever copied into the output. Every combination is within the soft targets and passes the same pipeline in one call. Demo versions are stored with `model = demo`, and `Generation.Notes` carries `ai.DemoContentNotice`.
- **State machine**: `CheckGenerateContent` allows `segmented` and `content_ready`; `draft` → `409 invalid_state` ("interpret the brief first"); `pending_approval`/`approved` → `409 invalid_state` until Slice 3 can revoke the approval in the same transaction (as for interpret and channels, §11.1); sending and terminal states → `409`. An empty channel selection is `422 validation_failed` (`channels`, `required`) and no model call is made. Success → `content_ready`.
- **Lifecycle**: read the campaign without a lock and record `version` and `channels` → call the generator with no transaction open → `Repository.SaveContent`: one short transaction (`lock_timeout`/`statement_timeout` 5 s) that locks the campaign row first, reads and re-validates the current version and checks that it is the newest (`max(version)`), applies the pure `Campaign.WithAIContent` (version and channel CAS, state check, content covers exactly the selected channels, `content.NextVersion`), inserts the immutable `campaign_contents` row and writes `status`, `current_content_id` and `version` in one `UPDATE`, then commits. A campaign changed while the model worked → `*ConflictError`, nothing inserted. The rollback runs even if the caller's context is cancelled, so a failure after the insert leaves no orphaned version.
- **Persistence**: the repository lives in `campaign` because the version row and the pointer must share the campaign-locked transaction; `content` stays pure. Only `INSERT` and `SELECT` statements exist for `campaign_contents`. Reads (`GetContent`, `Service.CurrentContent`) re-validate the row through `content.Parse` for the channels it has text for, require canonical (NFC, trimmed) text and a valid provenance. No migration was needed: the CP1 schema (`UNIQUE (campaign_id, version)`, FK `current_content_id`) fits.

### 7.9 Streamed generation and manual edits as implemented (CP13)

- **Real provider streaming.** `ai.Generator` always calls `llm.Client.Stream` (Anthropic: §7.1; demo: deterministic 24-rune chunks; scripted: queued chunks). Each model request's text deltas pass through `ai/jsonstream`, an incremental extractor of the selected channels' string fields (escape-, surrogate- and UTF-8-safe across chunk boundaries, bounded at 64 KiB, never panics; a fuzz test checks it against `encoding/json` for arbitrary split points). The extracted fragments are reported as progress **previews only**; the result is still computed from the *complete* text of a finished request by `ParseGeneratedContent` (strict decode → `content.Parse` → offer policy), with the unchanged single repair turn and truncation retry. **The SSE stream therefore contains real model-token deltas, explicitly labelled as unvalidated previews, plus workflow status events; only the `validated` event carries content, and only after the commit.**
- **Progress** (`ai.Progress`): `attempt` (request n, reason `initial` | `truncation_retry` | `repair`; a new attempt discards the previous attempt's preview), `thinking`, `delta` (field, text), `validating`, `repair` (number of field problems). The progress callback runs synchronously; if it fails (client gone) generation stops, no further request is made and the error is returned unchanged (not classified as a provider failure).
- **Service** (`GenerateContentStream`): `started` (after the state checks, before any model call) → progress … → `persisting` → the CP12 campaign-locked transaction. Errors before `started` (not found, invalid state, no channels) are plain request errors. The returned validation is `Campaign.ValidateStored` of the committed version, identical to `GET /content`.
- **Manual edits** (`Service.EditContent`, `Campaign.WithEditedContent`, pure, under the campaign row lock in the CP12 `SaveContent` transaction): version CAS first (`409 version_conflict`), then the state check (`segmented`/`content_ready`; `pending_approval`/`approved` → `409 invalid_state` until Slice 3 can revoke the approval in the same transaction; at least one channel), then `Campaign.ValidateDraft` (`content.Parse` for the selected channels + `channel_not_selected` for text of an unselected channel + the offer policy as **advisory warnings**) → `422 validation_failed` with field details, then `content.NextVersion(origin edited)`: canonical text equal to the current version is a **no-op** (nothing written, campaign version unchanged, `created: false`), any change inserts version n+1. Every version therefore covers exactly the channels selected when it was created.
- **Offer policy for marketer text (intentional limitation).** `ai.OfferPolicyAdvisories` runs the CP12 policy on edited/validated text and returns every finding (`unauthorized_offer`, `unauthorized_claim`, `offer_not_mentioned`) as a **warning**: the policy still **blocks model output** (§7.8, unchanged), but a human edit is the marketer's decision and the keyword check is conservative ("100% cotton"). This is consistent with the design: §8's rules (`content.Parse`) apply identically to AI output and edits, while the blocking offer policy is defined only for the generator (§7.8). The warnings are returned by `validate`, `PUT /content` and `GET /content` (re-computed from the stored text and the current content brief, so they survive save and reload), shown inline per field and as a "Review offers and claims before approval" notice; the editor saves only a draft revision the server has validated, so warnings are always displayed before a save. They are **not** evidence that copy is commercially or legally safe: the checker is a keyword/pattern match over English and common Turkish phrasings and cannot detect every unsupported claim (paraphrases, implied promises, other languages). Saving approves nothing. **Slice 3 requirement:** prepare must re-run `content.Parse` for the selected channels and the offer policy against the *current* content brief, and the approval dialog must show any `unauthorized_offer` / `unauthorized_claim` findings to the marketer before confirm.
- **Stored version vs. current selection** (`Campaign.ValidateStored`): missing text for a newly selected channel is an error (prepare will require it); text for a channel that is no longer selected is a warning (`channel_not_selected`: it is never sent).

---

## 8. Content validation

Applied identically to AI output, edits, the debounced editor validation, and prepare.

1. **Normalize** to Unicode NFC on save.
2. **Variables**: `{{first_name}}`, `{{last_name}}`, `{{city}}`, optional fallback `{{first_name|there}}`. Unknown or malformed placeholders are errors. Custom parser/renderer (not `text/template`).
3. **Declared maximum lengths** (variable registry): `first_name` 20, `last_name` 25, `city` = longest known city name (computed at startup). The database CHECK constraints guarantee the data never exceeds these.
4. **Worst-case length** = literal runes + Σ for each placeholder `max(declared_max, rune_len(fallback))`.
5. **Hard limits (blocking errors)** on the worst-case rune count: SMS ≤ 160, email subject ≤ 80, email body ≤ 5,000. Required fields for each selected channel must be present and non-empty.
6. **Warnings (non-blocking)** *(design; as implemented only `first_name_not_used` exists — encoding/segment warnings are B-tier and were not built, §8.1)*: encoding — GSM-7 if every rune is in the GSM 03.38 basic set or extension table (extension characters count 2 septets), otherwise UCS-2; worst-case segment count (GSM-7: 1 if ≤160 septets else ⌈n/153⌉; UCS-2: 1 if ≤70 UTF-16 units else ⌈n/67⌉); offer not mentioned; email body without the first name.
7. **Render-time assertion**: the renderer checks the actual rendered length; violation (impossible given 3 and 5) fails the delivery permanently with `content_too_long` and increments a metric. Never silently truncated.
8. The backend appends the fixed email footer (unsubscribe text) outside the 5,000-rune body budget; it is shown in the preview. No SMS opt-out suffix by default.

Result shape (design): `{errors[], warnings[], sms:{chars_worst_case, encoding, segments_worst_case}, subject:{chars_worst_case}, rendered_example}`; as implemented `sms`/`subject`/`body` are `{chars_worst_case, limit}` (§8.1, §11.2).

### 8.1 Content contract as implemented (CP11)

`internal/content` is pure (no I/O, no SDK) and depends only on `segment` (channels, field-error shape, city catalog). `content.Parse(Draft, channels)` is the single entry point for AI output, edits, `validate`, prepare and stored rows read back; a `Content` exists only as Parse's output, so the renderer never sees unvalidated text.
- **Normalization**: invalid UTF-8 → `invalid_encoding`; otherwise NFC, then trim leading/trailing whitespace. Positions in messages are 1-based characters of the normalized text.
- **Placeholders**: exactly `{{name}}` or `{{name|fallback}}`, name ∈ registry (case-sensitive). `{` and `}` are reserved (no escape): a stray brace, `}}` or unterminated `{{` → `unbalanced_delimiter`; empty, spaced, dotted, filtered, called, nested or multi-pipe placeholders → `malformed_placeholder`; a well-formed but unregistered name → `unknown_variable` (with `allowed`). Fallbacks are single-line, without surrounding whitespace, and contain at least one visible character, i.e. a letter, number, punctuation or symbol (`invalid_fallback`). Rendering is one literal substitution pass; values are never re-parsed.
- **Characters**: `\n` allowed in SMS and email body, not in the subject; all other C0/C1 controls (incl. `\t`, `\r`), U+2028/2029, bidirectional controls, BOM and noncharacters → `invalid_character`. Text without a visible character (letter, number, punctuation, symbol) → `empty`. Markup-like `<tag`, `</`, `<!`, `<?` → `markup_not_allowed` (plain-text policy, not a sanitizer; `<3`, `a < b` are fine).
- **Channels**: every provided field is validated whether or not its channel is selected; a selected channel's fields are required (`required` if absent, `empty` if blank); subject and body form a pair; unselected blank fields count as absent; content must contain at least one channel (path `""`, `empty`).
- **Errors** use the shared `{path, code, message, allowed?}` shape with paths `sms_body`, `email_subject`, `email_body`, `channels`; order is field order, then presence → encoding → characters → markup → placeholders → length; at most 10 per field plus one `too_many`. Messages never echo input. Length statistics are returned for every field that parsed (`{chars_worst_case, limit}`, also for `body`); `rendered_example` uses fixed fictional values and only when there are no errors. Warning implemented: `first_name_not_used` (email body). Encoding/segment warnings are B-tier; "offer not mentioned" needs the content brief and belongs to the generator.
- **Rendering** (`RenderSMS`, `RenderEmail`) is deterministic. Values are NFC-normalized and trimmed. Malformed UTF-8 or a forbidden character fails first (`invalid_value`, never replaced by the fallback); a value without a visible character (empty, or only e.g. U+200B/U+200D) is missing: it uses the fallback, and without one fails with `missing_value` (never rendered as empty). Values over the declared maximum (`value_too_long`) or with control characters (`invalid_value`) fail; nothing is truncated. The rendered length is asserted (`content_too_long`). The email footer (`content.EmailFooter`) is appended after `"\n\n-- \n"`, outside the body budget.
- **Versions**: AI generation always creates a new version (`origin=ai`, model + prompt version required); an edit whose canonical text equals the current version is a no-op; any other change creates version `n+1` (`origin=edited`, no model). Approval binds the immutable version id.

---

## 9. Approval and audience snapshot

- The snapshot **is** the set of `held` delivery rows of a pending/approved approval; the approval row carries the binding metadata (segment version + frozen rule, content id, channels, partition stats, count, digest, expiry).
- **The marketer approves exactly what they saw**: the approval dialog displays the statistics returned by *prepare* (computed from the same transaction that wrote the held rows). Confirm binds to that approval id and re-asserts segment version, content version and channels.
- **No audience re-evaluation during sending.** New matching users are never added. Users who stop matching the rule after prepare (e.g. purchased) are still sent to; staleness is bounded by the 24 h expiry.
- **Personalization** values are frozen at prepare (`vars`). **Destination** is taken from the user's current contact at the **first** dispatch precheck, persisted before the HTTP request, and reused verbatim by every retry.

### 9.1 Preparation as implemented (CP14)

CP14 is the backend half of the prepare step: `approval.Service.Prepare` / `ExpireDue`, `approval.Repository`, the campaign-side transitions (`campaign/approval.go`) and `segment.CompileEligible`. CP15 (§9.2) adds confirm, withdraw, the approval read, the HTTP routes and the UI. Send and the worker sweeper loop follow in Slice 4. The "until Slice 3" refusals of interpret / channels / content edits in `pending_approval`/`approved` (§7.8/§7.9/§11.1) stay in place in CP15 with the reason "withdraw it first", which is now an available action; revoke-on-edit (revoking the live approval inside the edit transaction) is not implemented yet.

- **Lazy expiry first, in its own transaction.** `Service.Prepare` runs `ExpireDue` (the §5.5 Expire transaction) and commits it before the preparation transaction. Expiry advances the version, so a request based on the pre-expiry view then fails the version CAS with `409 version_conflict` — and because the expiry is already committed, that rollback cannot undo it; the client reloads (content_ready, version + 1) and prepares again.
- **Transaction** (§5.5, READ COMMITTED, `lock_timeout` 5 s, `statement_timeout` 10 s, 30 s context deadline for the whole transaction; no I/O but the database): lock campaign → expiry re-check after the lock (an approval that became due since the lazy expiry is expired, never superseded; the version advance makes the CAS fail and the transaction roll back) → version CAS (`expected_version`) + state check → re-read the current content version (re-validated, must be the newest) → `Campaign.ReviewForApproval` → revoke a still-live **pending** approval (`superseded`) and delete its held rows (count must equal its `snapshot_count`) → `segment.ComputeSnapshotAudience`: partition **and** expected snapshot (per-channel eligible counts + digest) in one statement → insert approval → `INSERT … SELECT` held rows (same `eligible` definition) → read them back in digest order and compare counts + digest → campaign `pending_approval`, `active_approval_id`, `version+1`. Any failure rolls back everything, so a superseded approval stays live when the new preparation fails, and no approval ever points at an incomplete snapshot.
- **Consistency under concurrent customer changes** (no customer row is locked; no isolation upgrade). The partition shown to the marketer and the expected recipient set come from **one statement**, i.e. one database snapshot, so the stored `audience_stats` is exactly the partition whose `eligible` cell is the expected set. The held rows are inserted by a second statement; if any consent, contact or rule-relevant data committed in between changes the eligible set (even with equal counts), the read-back digest differs and preparation rolls back with `ErrAudienceChanged` (retryable). Hence a committed approval's held rows are exactly the eligible cell of its stored partition at one instant. Changes committed **after** that instant are not reflected (snapshot semantics, ADR-007); they are handled at send time: consent is re-checked in the precheck of **every** attempt, the destination is resolved from the **current** contact at the first precheck (no contact → `skipped_no_contact`), membership is never re-evaluated, and `vars` stay frozen (§10.3).
- **Approved campaigns** (CP14 decision, review of §5.2): a plain prepare never invalidates a confirmed approval. §5.2 allows `approved → prepare`, but the prepare request carries only `expected_version`, so it cannot express "replace my confirmed approval" — a stale "Review & approve" would silently revoke it. Prepare in `approved` is therefore `409 invalid_state` (and the supersede step refuses anything but a pending approval, defence in depth). This is the deliberate product-safe deviation from the §5.2 table (approved in the CP14 review). The marketer withdraws first (CP15), or the approval expires (its own `expires_at`, 24 h after confirm — the pending 15 min TTL never applies to it), in which case lazy expiry closes it as `expired` (version + 1) and, after a reload, preparation proceeds from `content_ready`. Superseding a **pending** approval stays as documented: it requires the current version (a double submit is `409 version_conflict`) and is recorded as `revoke_reason = superseded`. An explicit re-preparation of an approved campaign, if wanted, needs its own contract in CP15.
- **Campaigns without a live approval**: only `pending_approval` / `approved` campaigns have a live approval to expire or supersede. `sending` and finished campaigns keep `active_approval_id` on their consumed approval (§4), which lazy expiry leaves untouched even when its old `expires_at` has passed (no write, no version change); prepare then fails the state check.
- **Eligibility** is defined once: `segment.CompileEligible` = `WITH matched AS (<Compile(rule)>), classified AS (…), eligible(user_id, channel)`, where `classified` is the same CTE the audience statistics count (no_contact 0 / no_consent 1 / eligible 2) and `eligible` keeps code 2 for the selected channels (bound as a parameter). One row per (user, channel) by construction.
- **Uniqueness and idempotency** (actual schema): `UNIQUE (campaign_id, user_id, channel)` and `UNIQUE (idempotency_key)` with key `<campaign_id>:<user_id>:<channel>` span **all approvals of a campaign**, not one approval. A later approval may freeze the same recipient with the same key because the earlier approval's held rows are deleted in the same transaction that closes it (invariant 6), before the new rows are inserted; the new rows carry the new `approval_id`, so they cannot be mistaken for the earlier snapshot. At most one approval per campaign is ever consumed (`approvals_one_consumed`) and a sending/finished campaign cannot be prepared, so a released row is never deleted or re-frozen and its key is never reused by another snapshot; every retry of a delivery reuses its one key (§10.6). Revoked/expired approval rows are kept unchanged after closing (binding, stats, count, digest — no statement reopens them); their held rows are not history and are deleted by design (ADR-005).
- **Frozen values**: `vars = {"first_name", "last_name", "city"}` (the `content.Values` shape) copied verbatim from the user row inside the `INSERT … SELECT` — no contact data, no normalization, no truncation, no fallback substitution; Go never reads them except in the read-back. **Unusable values do not change eligibility** (the snapshot must equal the partition): the read-back trial-renders every row with the deterministic renderer and reports per channel the number of rows that would fail (`render_issues` by code: `missing_value`, `invalid_value`, `value_too_long`, `content_too_long`). Such a delivery fails permanently at send time (§8.7). Fallbacks apply as in §8.1 (a blank value uses the placeholder's fallback). Counts only; never values.
- **Snapshot digest**: lower-case hex SHA-256 of lines `<user_id>:<channel>` sorted by user id, then channel in enum order (sms, email), joined by `\n` (no trailing newline). Computed in SQL before the insert (stored) and in Go over the read-back rows; they must match.
- **Content review** (§7.9 requirement; returned in `Summary.Review`): `content.Parse` for the selected channels via `ValidateStored` (missing text for a selected channel → `422 validation_failed`; text for an unselected channel → warning `channel_not_selected`, it is never frozen or sent) and the offer policy against the **current** content brief. `unauthorized_offer` / `unauthorized_claim` findings never block preparation (also not for AI versions, which could not contain them); they are returned separately as `OfferFindings` for the approval dialog. **CP15 requirement:** the prepare response and the approval dialog must show these findings before the Confirm action (§7.9); whether confirm additionally needs an explicit acknowledgement is a CP15 product decision, not implied by preparation. A stored content brief that does not parse is an internal error (the offer check must not be silently skipped). The rendered example uses `content.ExampleValues` only.
- **Versions**: every persisted campaign state change advances `campaigns.version` exactly once (§11.1): prepare (+1, including a supersede) and expiry (+1, in the same transaction as the approval status change and the held-row deletion). An expiry check that finds nothing due writes nothing. A request based on the pre-expiry version is therefore `409 version_conflict` for every version-guarded mutation (prepare, edit, channels); confirm/send (CP15) additionally bind the approval id and CAS on `status` and `expires_at > clock_timestamp()` (§5.5). No statement ever reopens an expired or revoked approval; a new preparation always inserts a new id with new rows. A repeated prepare with the same `expected_version` (double submit) is `409 version_conflict`; it never supersedes the approval the first request created. Superseding requires the current version — an explicit re-preparation, recorded as `revoke_reason = superseded`.
- **Expiry**: `expires_at = prepared_at + APPROVAL_PENDING_TTL` from one `clock_timestamp()` value taken after the lock (TTL is a service parameter, 1 s – 24 h, default 15 min; env wiring with CP15). A due approval is closed as `expired` (never `superseded`), even when it expired while the expiry transaction waited for the campaign lock (integration test: the waiting transaction started before expiry, expires it after the lock, and the stale preparation is `409`). `ExpireDue` performs the §5.5 Expire transaction. Expired and revoked approvals keep their row (binding, stats, count, digest); only their held rows are deleted.
- **Summary** (`approval.Summary`): approval id, campaign version, segment version, content id + number, channels, the stored partition, count, digest, prepared/expires times, per-channel held counts with render issues, the content review, and the superseded/expired approval ids. It contains no user ids, names, cities or contacts. Logs carry ids and counts only.

### 9.2 Confirmation, withdrawal and review as implemented (CP15)

`approval.Service.Get` / `Confirm` / `Withdraw`, `approval.Repository` (`decision.go`), the campaign transitions `CheckConfirmAt` / `WithConfirmedApproval` / `CheckWithdrawAt` (`campaign/approval.go`), the HTTP routes (§11.3) and the Vue approval step. Nothing here calls the LLM or a provider; no transaction is open across any I/O but the database.

- **Lazy expiry everywhere.** Every approval operation first runs `ExpireDue` in its own committed transaction (as Prepare does, §9.1). The campaign service has an optional `ApprovalExpirer` (wired in `cmd/api`): `Get`, `Audience`, `Content`, `ValidateContent`, `Interpret`, `GenerateContentStream`, `SetChannels` and `EditContent` load the campaign and, **only** when it is `pending_approval`/`approved`, run `ExpireDue` first and re-read. A request based on the pre-expiry view is therefore a `409 version_conflict` for every version-guarded mutation, and reads never present a due approval as live. Sending/finished campaigns are never touched (their consumed approval never expires).
- **Decision transactions** (READ COMMITTED, `lock_timeout` 5 s, `statement_timeout` 10 s, 30 s deadline): lock campaign → lock the **target** approval row (`id` + `campaign_id`; else `404`) → `expireIfDue` under the lock with `clock_timestamp()`. If the campaign's live approval is due *now* (it became due after the service's lazy expiry, e.g. while this request waited for the lock), the expiry is written and the transaction **commits** with the request ending as `409 approval_expired` (target) or `409 version_conflict` (another approval expired under the client) — a final transition is never rolled back with the rejected request. A target that is already `expired` → `409 approval_expired` (before the version check, so a pre-expiry client learns why).
- **Confirm** (after the prelude): target `approved` → `409 invalid_state` ("already confirmed"), `consumed`/`revoked` → `409 invalid_state`; version CAS (`expected_version`) and campaign state (`pending_approval` with this approval active) → `409 version_conflict` / `invalid_state`; binding: the request's `segment_version`, `content_version`, `channels` must equal the approval's, and the approval's must equal the campaign's current ones → `409 approval_mismatch` with the differing fields as `details` (`campaign.*` paths for drift between approval and campaign); the bound content version is re-read and re-reviewed for the bound channels against the **current** content brief (`ReviewForApproval`; offer findings recomputed, never taken from client state or a cached preparation); the held rows are re-read and their per-channel counts and digest must equal the stored partition, `snapshot_count` and digest (else `ErrInconsistent` → 500, rollback); then the §5.5 CAS `UPDATE … SET status='approved', approved_at=t, approved_by=$APPROVER_NAME, expires_at=t + APPROVAL_APPROVED_TTL FROM (SELECT clock_timestamp() t) WHERE status='pending' AND expires_at > t AND segment_version/content_id/channels = binding` — `t` is one value, so `expires_at − approved_at` is exactly the approved TTL. If the CAS matches no row the only remaining cause is expiry between the prelude and the CAS: the approval is expired (committed) → `409 approval_expired`. Finally campaign → `approved`, version + 1. Held rows, `vars`, content and the audience are untouched; nothing is re-evaluated.
- **Withdraw** (after the prelude): target `revoked` → `409 invalid_state` ("already withdrawn or replaced"), `consumed` → `409 invalid_state`; version CAS and campaign state (`pending_approval`/`approved` with this approval active; `sending`/finished → `409 invalid_state`); approval → `revoked(withdrawn)`, `closed_at = clock_timestamp()`; its held rows deleted (count must equal `snapshot_count`, no non-held row may exist); campaign → `content_ready`, `active_approval_id = NULL`, version + 1. The approval row keeps its binding, partition, count, digest and `approved_at`.
- **Concurrency.** All decisions of one campaign serialize on the campaign row lock; every mutation CASes the version. Two simultaneous confirms: one commits, the other sees `approved` → `409 invalid_state`. Confirm vs. withdraw: exactly one applies. Repeated withdraw: `409 invalid_state`. A failure at any step (including the final campaign update) rolls everything back; integration tests inject one with a trigger.
- **Read** (`GET …/approvals/{aid}`): lazy expiry, then one read-only REPEATABLE READ transaction: campaign, approval, bound content version and — for a live approval — the review and the held-row verification above (so the dialog shows what is actually frozen, with render issues). A closed approval (expired/revoked/consumed) returns its stored binding and partition, `review: null`, `render_issues: null`.
- **TTLs.** Pending: `prepared_at + APPROVAL_PENDING_TTL` (default 15 min, 1 s – 24 h). Approved: `approved_at + APPROVAL_APPROVED_TTL` (default 24 h, 1 s – 7 d), i.e. 24 h from confirmation, not from preparation; the pending TTL never applies to an approved approval. Both are configured once at startup (`platform/config`, bounds identical to `approval`'s, asserted by a test). `APPROVER_NAME` (default `marketer@demo`, ≤ 100 printable characters) is stored as `approved_by`.
- **Offer findings and acknowledgement (CP15 decision).** The documented confirm contract (§11) has no acknowledgement field, so none was invented. The findings are returned by prepare, read and confirm (`review.offer_findings`, recomputed each time), listed individually in the dialog (never as a bare count) with the authorized offer and an explicit statement that the check is keyword-based and advisory; Confirm requires ticking "I reviewed the frozen content and recipient counts" and, when there are findings, "I reviewed all N offer/price findings". This is a UI control, not a server-side guarantee; the server-side guarantee is the binding (approval id + version + segment/content version + channels) to exactly the reviewed state. See ADR-023.
- **Privacy.** Responses contain counts, binding metadata, the review (rendered with the fictional `content.ExampleValues`) and the digest — never user ids, names, cities, contacts, `vars`, destinations or idempotency keys. The frontend decoder rejects unknown properties in approval responses. Logs carry ids, versions and counts.

---

## 10. Delivery engine

### 10.1 Worker components (single process)
- Per channel: one **dispatcher** (claims batches of up to 50 when the sender pool has capacity) and a bounded **sender pool** (default 20 goroutines per channel).
- One **sliding-window limiter** per channel (§10.4).
- **Reaper** (every 10 s), **approval-expiry sweeper** (every 30 s), **completion detector** (after each completed batch and every 5 s), **queue-depth sampler** (every 5 s), **heartbeat** (every 5 s).
- Retry delays and leases are computed in SQL (`now() + interval`).

### 10.2 Attempt sequence (the only code path that calls the provider)
`delivery.gatedSender.Attempt(ctx, d)`:
1. Build everything that needs no I/O: render body from immutable content + frozen `vars`, marshal payload template.
2. `t0 := limiter[d.channel].Acquire(ctx)` — reserves a slot. `slotDeadline := t0 + DISPATCH_MAX_SLOT_TO_REQUEST` (default 20 ms; monotonic clock).
3. **Precheck** (one SQL statement, §10.3), run with a context deadline of `min(now + DISPATCH_MAX_PRECHECK, slotDeadline)` (default precheck budget 15 ms) → destination or a skip reason.
4. **Final deadline gate**, the last statement before handing the request to the HTTP client: fill in the destination (CPU only), then `if clock.Now() > slotDeadline → abort`. Nothing but `http.Client.Do` follows the gate.
5. `http.Client.Do` (request timeout 5 s). The transport keeps `MaxIdleConnsPerHost ≥` pool size so requests normally reuse warm keep-alive connections.
6. **Complete** (one transaction): guarded status update + attempt row, per §10.5.

**Deadline handling** — preserving the rate guarantee beats throughput; a slot is never refunded:

| Situation | Detection | Behaviour |
|---|---|---|
| Precheck exceeds `DISPATCH_MAX_PRECHECK` (slow DB, lock wait) | precheck context deadline fires | no request; slot wasted; restart at step 2 |
| Total slot-to-request interval exceeds `DISPATCH_MAX_SLOT_TO_REQUEST` (e.g. precheck fast but the goroutine was descheduled or GC-paused afterwards) | final gate in step 4 | no request; slot wasted; restart at step 2 |
| Process delayed between precheck completion and the request | the same final gate — it runs after everything else, immediately before `Do` | no request; slot wasted; restart at step 2 |

After 3 consecutive aborts the lease is released (guarded by `leased_by`), `dispatch_started_at` is cleared and the row is rescheduled 1 s later without counting an attempt — no request was sent. A persisted destination stays persisted. If a precheck's context expired during its commit (outcome unknown), the release statement clears `dispatch_started_at` anyway; at worst the destination was persisted, which is allowed.

**Residual (documented, not eliminated):** application code cannot prevent a stall that occurs *after* the final gate — inside the HTTP transport, the Go runtime or the kernel (connection acquisition, write syscall, network). Time spent there is part of the transport jitter `J` covered by the safety margin (§10.4) under the documented network assumption. It is not a universal guarantee.

Because the limiter is acquired inside the only function that performs provider requests, every request — first attempt or retry — consumes a slot. There is no other code path to the provider.

### 10.3 Precheck statement
```sql
UPDATE campaign_deliveries d
SET destination         = COALESCE(d.destination, CASE d.channel WHEN 'sms' THEN u.phone ELSE u.email END),
    dispatch_started_at = now(),
    lease_until         = now() + $lease,              -- renew: lease ≫ request timeout
    updated_at          = now()
FROM users u, campaigns c, campaign_approvals a
WHERE d.id = $id AND d.leased_by = $worker AND d.status = 'in_flight'         -- fencing: we still own it
  AND u.id = d.user_id
  AND c.id = d.campaign_id AND c.status = 'sending'
  AND a.id = d.approval_id AND a.status = 'consumed'
  AND EXISTS (SELECT 1 FROM channel_consents cc
              WHERE cc.user_id = d.user_id AND cc.channel = d.channel AND cc.status = 'opted_in')
  AND COALESCE(d.destination, CASE d.channel WHEN 'sms' THEN u.phone ELSE u.email END) IS NOT NULL
RETURNING d.destination;
```
Zero rows ⇒ a follow-up read classifies the reason and completes the row: lease lost → do nothing (another worker owns it); campaign cancelled → `cancelled`; consent not opted-in → `skipped_consent_revoked`; no destination (only possible before the first attempt) → `skipped_no_contact`. On retries the persisted destination is reused even if the user's contact has since changed or been removed; consent is checked on **every** attempt.

### 10.4 Rate limiter (sliding-window log)
```
state per channel: mu, limit N (=100), window W (= 1s + RATE_SAFETY_MARGIN),
                   ring[N] of monotonic timestamps (zero = unused), next (index of oldest), pausedUntil
Acquire(ctx):
  loop:
    lock
    now     := clock.Now()
    readyAt := max(pausedUntil, ring[next] + W)      -- ring[next] = the N-th most recent acquisition
    if now ≥ readyAt: ring[next] = now; next = (next+1) mod N; unlock; return now
    unlock
    wait until readyAt (timer) or ctx.Done()          -- then re-check under the lock
Pause(until): lock; pausedUntil = max(pausedUntil, until); unlock
```
- **Correctness**: a timestamp is recorded only if it is ≥ W after the acquisition N positions earlier, so any N+1 consecutive acquisitions span ≥ W ⇒ at most N acquisitions in any window shorter than W. Check-and-record is atomic under the mutex, so concurrent goroutines cannot bypass it; waking goroutines always re-check. Go's monotonic clock makes it immune to wall-clock jumps.
- **From local slots to provider arrivals**: for requests i and i+N, `arrival_{i+N} − arrival_i ≥ W − G − J`, where `G = DISPATCH_MAX_SLOT_TO_REQUEST` bounds slot-to-`Do` (enforced by the final gate, §10.2) and `J` is the variation in time from `Do` to arrival at the provider (transport, runtime, kernel, network). With `W = 1 s + margin` and `margin ≥ G + J`, at most N requests arrive at the provider in any rolling 1-second interval. Defaults: `RATE_LIMIT_PER_SECOND=100`, `RATE_SAFETY_MARGIN=50ms`, `DISPATCH_MAX_SLOT_TO_REQUEST=20ms`, `DISPATCH_MAX_PRECHECK=15ms` (startup fails unless `DISPATCH_MAX_PRECHECK < DISPATCH_MAX_SLOT_TO_REQUEST < RATE_SAFETY_MARGIN`). Effective throughput ≈ 95/s. `J ≤ 30 ms` is an **assumption** for the local Docker network; it is verified empirically by the provider-observed integration test (§10.11), not proven, and the guarantee does not extend to environments that violate it.
- **Independence**: separate limiter instances for SMS and email; nothing shared.
- **429**: `Pause(now + RetryAfter)` on **that channel's limiter only**; the delivery is rescheduled to `now() + RetryAfter` (SQL). `Retry-After` parsed as seconds or HTTP-date; missing → 1 s; capped at 30 s. In-flight requests finish; other channels are unaffected.

### 10.5 Outcome classification and retry policy
| Provider result | Outcome | Next status |
|---|---|---|
| 2xx | `accepted` | `sent` |
| 2xx with `Idempotent-Replayed: true` | `replayed` | `sent` (a previous attempt was delivered) |
| 429 | `rate_limited` | `retry_scheduled` at `max(Retry-After, 1s)`; `failed` after 10 rate-limited attempts |
| 5xx, connection error | `transient_error` | `retry_scheduled` with backoff; `failed` after 5 attempts |
| timeout | `timeout` (ambiguous) | as transient; sets `outcome_uncertain` if the row ends non-`sent` |
| 4xx other than 429 (incl. 422 idempotency-key reuse) | `permanent_error` | `failed` (422 reuse also logs an error: it indicates a bug) |

Backoff: `min(60 s, 1 s · 2^(attempts−1))` with full jitter (pure function `retry.Next(attempts, rateLimitedAttempts, outcome, retryAfter)`). If the campaign is no longer `sending` when a non-terminal outcome is recorded, the row becomes `cancelled` instead of `retry_scheduled`. `outcome_uncertain` is set whenever a row reaches a terminal status other than `sent` after any `timeout` or `unknown` attempt.

### 10.6 Idempotency and destination consistency
- Key = `<campaign_id>:<user_id>:<channel>`: identical to the business uniqueness rule, so even a hypothetical second row for the same recipient would collide at the provider.
- Every attempt for a delivery sends **byte-identical** content: same key, same persisted destination, same body (immutable content version + frozen vars + deterministic renderer).
- Mock provider semantics (mirrors common real-provider behaviour): same key + same payload → `200` with the original `provider_message_id` and `Idempotent-Replayed: true`, nothing re-sent; same key + different payload → `422 idempotency_key_reuse`. Ledger: a durable, fsynced journal on a named volume, retained across process restarts and container recreation until explicitly destroyed (ADR-020 amendment, §10.12).
- **Idempotency is a provider capability, not something we can enforce.** Each provider adapter declares it explicitly: `provider.Capabilities{IdempotencyKeys bool, KeyRetention time.Duration}` (the mock adapter declares `true` and "until explicit reset" of its durable ledger; a real adapter must state what its provider documents, or `false` if it does not document it). No real provider adapter is in scope.
- **Retry horizon `H`**: an upper bound, computed at worker startup from configuration, on the time between the first and the last provider request for one delivery while the worker is able to dispatch (transient attempts × max(request timeout, lease + reaper interval) + Σ backoff caps + rate-limited attempts × `RETRY_AFTER_CAP`; ≈ 12 min with defaults).
- **Effective guarantee** is derived, logged at worker startup and reported by `/readyz`: if `IdempotencyKeys && KeyRetention ≥ H` → *at most one delivered message per (campaign, user, channel), provided the provider implements its documented semantics*; otherwise → *at-least-once; a duplicate is possible after an ambiguous attempt*. Worker downtime longer than `KeyRetention` while a retry is pending can still produce a duplicate even for an idempotent provider (§18).

### 10.7 Leases, crashes, reaper
- Claim lease 60 s; precheck renews to 60 s right before the request; request timeout 5 s.
- Reaper, per row with `status='in_flight' AND lease_until < now()`: if `dispatch_started_at IS NOT NULL` (a request may have been sent) → insert attempt `unknown`, `attempts+1`, then `retry_scheduled` (or `failed` if exhausted; `cancelled` if campaign cancelled), mark uncertain if terminal non-sent; if `dispatch_started_at IS NULL` → back to `pending` without counting an attempt.
- Graceful shutdown (SIGTERM): stop claiming; wait up to 10 s for in-progress HTTP requests; release claimed-but-not-prechecked rows to `pending`.

**Crash after the provider accepted, before we recorded success**
1. Row remains `in_flight`, `dispatch_started_at` set, destination persisted.
2. Lease expires → reaper records attempt `unknown`, schedules retry.
3. Next attempt: precheck (consent still opted-in) → same key, same destination, same body.
4. The mock provider (idempotent, retention ≥ `H`) returns the original message id with `Idempotent-Replayed` → row `sent`. One message was delivered; our record is reconciled. With a provider that does not honour keys for the whole retry horizon, step 3 can produce a second message; that is the documented at-least-once case.
5. If consent was revoked between the crash and the retry, the retry is not sent; the row ends `skipped_consent_revoked` with `outcome_uncertain = true` (the first request may have been delivered). This is reported on the dashboard, not hidden.

### 10.8 Cancellation during sending
Cancel transaction: campaign → `cancelled`; `pending`/`retry_scheduled` rows → `cancelled`. Rows already `in_flight`: if their precheck has not run, the precheck fails (`c.status='sending'` no longer holds) → `cancelled`; if the request is already on the wire, the outcome is recorded truthfully (`sent` or `cancelled` instead of retry). The campaign stays `cancelled`; final counts settle once in-flight rows complete.

### 10.9 Delivery guarantees (exact)
**Guaranteed**
- G1 — At most one delivery row per (campaign, user, channel) (DB constraint).
- G2 — Only rows of the **consumed** approval of a **sending** campaign are ever claimed or sent; a pending, expired, revoked or superseded approval can never produce a provider request (claim join + precheck join + held-row deletion).
- G3 — Every provider request belongs to a recipient in the approved snapshot (rows are only created by prepare).
- G4 — All attempts of a delivery carry the same idempotency key, destination and body.
- G5 — No concurrent duplicate sends from our side: the lease plus fencing precheck ensure a single owner at request time unless a worker is paused for longer than the remaining lease (≈55 s) after its precheck; even then, G4 lets an idempotent provider deduplicate (§10.6).
- G6 — Liveness: while the worker runs, every released row reaches a terminal status (bounded attempts, reaper).

**Delivery semantics**: **at-least-once requests** to the provider. Whether that becomes **at most one delivered message per (campaign, user, channel)** depends entirely on the provider's actual idempotency implementation and key retention (§10.6): it holds only for a provider that declares idempotency keys with `KeyRetention ≥ H` (the retry horizon, ≈ 12 min with defaults) and actually implements them as documented — the mock provider does. For any other provider we claim at-least-once only: a duplicate is possible after an ambiguous attempt (timeout, crash after send).

**Mock-provider idempotency behaviour verified by tests**: same key + same payload → replay of the original result; same key + different payload → `422` rejection; retry after an ambiguous response (timeout that committed, crash after accept) → reconciled to `sent` via the original key with exactly one message in the ledger.

**Not guaranteed**: exactly-once end-to-end delivery; our recorded status always matching the provider's truth (ambiguous cases are flagged with `outcome_uncertain`).

### 10.10 Consent revocation guarantee (exact)
- A revocation **committed before an attempt's precheck statement starts** is always honoured: that attempt and all later attempts for the row are not sent.
- Race boundary: a revocation committed after the precheck's snapshot but before the provider receives the request does not stop **that single attempt**. The window from the precheck's snapshot to `Do` is bounded by `DISPATCH_MAX_SLOT_TO_REQUEST` (20 ms, enforced by the final gate), plus request transit (local network ≈ ms; at most the 5 s request timeout in pathological cases). No further attempts follow once the revocation is visible.
- We deliberately do not hold a row lock on the consent across the HTTP request: it would only serialize the revocation *after* the send (the message still goes out), while pinning connections and blocking the consent write path for up to the request timeout. It improves nothing observable.
- Cancellation of a sending campaign has the identical boundary.

### 10.11 Verification of the rate guarantee
- Unit (fake clock): 1,000 acquisitions from 50 goroutines; over recorded timestamps the maximum count in any window W is ≤ N; `Pause` blocks only its own channel; run with `-race`.
- Unit (fake clock, fake sender that records calls): (a) precheck stub that advances the clock beyond `DISPATCH_MAX_PRECHECK` → no request, slot consumed, retried with a new slot; (b) fast precheck, then a test hook between precheck and the final gate advances the clock past `slotDeadline` (simulated process delay) → no request; (c) three consecutive aborts → lease released, `dispatch_started_at` cleared, attempts unchanged; (d) a request is never issued with `clock.Now() > slotDeadline` across a randomized sweep of delays.
- Integration (real time, testcontainers PostgreSQL + real worker + mock provider over real TCP): ~1,500 messages per channel, 5 % injected 5xx, 2 % injected 429 (with `Retry-After`).
  - The provider records **arrival time at handler entry** for every request (including those it answers with 429, 5xx, or a replay) and exposes `max_in_any_rolling_1s` per channel (sorted arrivals, two-pointer scan over half-open 1 s windows).
  - Assertions: `max_in_any_rolling_1s ≤ 100` for each channel; provider-side arrival count per channel = `Σ delivery_attempts` rows for that channel (retries counted); provider's own strict limiter reports zero "limit exceeded" 429s (injected 429s are counted separately); after an injected 429 on one channel, no request arrives on that channel before `Retry-After` while the other channel keeps flowing.

### 10.12 Delivery as implemented (CP16)

CP16 is the first half of **Slice 4** (ROADMAP Day 5): the send transaction (§5.5 "Send", §11.4), `internal/delivery` (claim, precheck, attempt, completion, reaper, completion detection, graceful shutdown), `internal/provider` (client + classification + declared capabilities), `internal/mockprovider` + `cmd/mockprovider`, and `cmd/worker`. CP17A (§10.13) adds the per-channel sliding-window limiter with `Pause` and the slot-deadline gate (§10.2–§10.4), exponential backoff with full jitter, cancellation (`POST /cancel`), the expiry sweeper and heartbeat and the rate integration test (§10.11); CP17B follows with `/stats`, `/deliveries`, metrics, progress SSE and the Send/Cancel UI. **Superseded by CP17A** (kept for the record) — in CP16:

- **No rate limiting.** `gatedSender.Attempt` (`Worker.attempt`) is still the only code path to the provider and keeps the documented order — precheck → destination (CPU) → `Do` → complete — with the limiter slot to be acquired at its first step; requests are bounded only by `SENDER_POOL_SIZE` per channel. A 429 reschedules the row (`Retry-After`, ≥ 1 s, ≤ `RETRY_AFTER_CAP`) but does not pause the channel yet.
- **Retries** use a fixed delay `DELIVERY_RETRY_DELAY` (default 5 s) instead of backoff, with the documented budgets (`MAX_TRANSIENT_ATTEMPTS` 5, `MAX_RATE_LIMITED_ATTEMPTS` 10) and classification (§10.5, `delivery.Policy.Decide`, pure). Every non-terminal outcome is persisted as `retry_scheduled` with `next_attempt_at`, so no row is abandoned and no campaign completes early; the retry horizon is computed with this delay (≈ 11 min with defaults) and the effective guarantee is logged at startup (§10.6).
- **Precheck budget**: the precheck runs with a 2 s statement deadline (CP17 tightens it to `DISPATCH_MAX_PRECHECK`); there is no I/O between the precheck and `Do`, but the 20 ms slot-to-request gate is not enforced yet, so §10.10's window is "precheck commit → request on the wire" without that hard bound.

Behaviour (all statements in `delivery/repository.go`):

- **Claim** (§5.5): `FOR UPDATE OF d SKIP LOCKED` over rows `pending`/`retry_scheduled` with `next_attempt_at <= now()`, joined to a **consumed** approval that is the **active** approval of a **sending** campaign, ordered `(next_attempt_at, id)`, at most `min(CLAIM_BATCH_SIZE, free pool slots)` rows per channel; lease `LEASE_DURATION` (60 s), `leased_by` = worker id. The worker id is `WORKER_ID` plus a random suffix per process start, so a restarted worker never treats its predecessor's leases as its own. Claiming is safe with several worker processes (tested with two on 2,000 rows); the deployment still runs one replica because the CP17 limiter is in-process.
- **Render** before the precheck, from the approval's content version (read once per approval, re-validated with `content.Parse` for the approval's channels and required to be canonical — `delivery.ParseApprovedContent`) and the row's frozen `vars` (strict decode). `RenderSMS` / `RenderEmail` (footer always attached). A render failure (`missing_value`, `invalid_value`, `value_too_long`, `content_too_long`) or invalid `vars` makes the row `failed` with that code in `last_error`, **without** a provider request or attempt row (§8.7, §9.1). Nothing is truncated or replaced. The LLM is never involved.
- **Precheck** (§10.3, one statement): fencing (`leased_by`, `in_flight`, no earlier precheck in this claim), campaign `sending`, approval `consumed`, a current `opted_in` consent row (a missing row is not consent), and a destination: the persisted one on retries, otherwise the user's **current** contact, which must be **valid** (SMS: E.164 `^\+[1-9][0-9]{7,14}$`; email: one `@`, a dot in the domain, no whitespace, ≤ 254 characters) and is persisted **before** the request and never changed. It sets `dispatch_started_at` and renews the lease. On a miss a follow-up read classifies, in this order: lease lost (nothing written) → campaign not sending / approval not consumed → `cancelled` → no consent → `skipped_consent_revoked` → no valid contact → `skipped_no_contact`; if nothing is wrong any more, the row is released without an attempt. (Consent is evaluated before contact here, as §10.3 lists it; the audience partition's no_contact-first order (§6.5) applies to preparation.)
- **Request**: `provider.Client.Send` with `Idempotency-Key = campaign_id:user_id:channel`, the persisted destination and the deterministically rendered body (same bytes on every retry); timeout `PROVIDER_REQUEST_TIMEOUT` (5 s). Classification: 2xx → `accepted` (`Idempotent-Replayed: true` → `replayed`); 429 → `rate_limited`; 5xx and failures to connect → `transient_error`; no response in time → `timeout`; other transport errors after the request may have been written, unreadable 2xx bodies and requests aborted by shutdown → `unknown` (ambiguous like `timeout`); other 4xx (incl. 422 `idempotency_key_reuse`) → `permanent_error`. Provider error texts are never stored; only a sanitized code.
- **Complete** (one transaction): lock the row guarded by `leased_by`, `in_flight` **and** the precheck's `dispatch_started_at` (a fencing token: a stale attempt can never complete a re-claimed row); compute the decision from the locked counters; a retry of a campaign that is no longer sending becomes `cancelled`; `sent` sets `provider_message_id` and `sent_at`; `outcome_uncertain` is set when a row ends terminal non-`sent` after any `timeout`/`unknown` attempt (cleared on `sent`); insert the `delivery_attempts` row (`attempt_no` = attempts + rate-limited attempts + 1, `started_at = dispatch_started_at`, persisted destination, HTTP status, `retry_after_ms`, error code). Delivery **status** (row) and attempt **outcome** (attempt rows) stay distinct; skips and render failures have no attempt row.
- **Reaper** (every 10 s, `SKIP LOCKED`, one transaction per batch of 100): expired lease without `dispatch_started_at` → `pending`, no attempt; with it → an `unknown` attempt (`lease_expired`), attempts + 1, then `retry_scheduled` / `failed` (uncertain) / `cancelled` (uncertain) per policy and campaign state. A worker that crashed after the provider accepted is therefore reconciled by the retry's replay (tested: one ledger entry, attempts `unknown, replayed`, row `sent`); a failure while recording an outcome (tested with an injected trigger) is recovered the same way.
- **Completion detection** (after terminal outcomes and every 5 s): one `UPDATE campaigns` per sending campaign that sets `completed` / `completed_with_failures` (any `failed` row), `completed_at`, `version + 1`, only if the consumed approval has exactly `snapshot_count` rows and none is `held`/`pending`/`in_flight`/`retry_scheduled`. Concurrent detectors serialize on the campaign row; the second no longer matches `status = 'sending'` (tested with 8 concurrent calls: one transition). Final per-channel counts are logged. Skipped and cancelled rows are not failures.
- **Graceful shutdown** (SIGTERM; Compose `stop_grace_period: 20s`): stop claiming; claimed rows whose precheck has not run are released to `pending`; provider requests in flight finish (≤ 10 s) and are recorded; after the grace period they are aborted and recorded as `unknown` (the retry reconciles). Database writes after a request use a context that survives the shutdown signal. Anything that could not be written is recovered by the reaper after the lease.
- **Privacy**: rows' `vars` and rendered text exist only in memory during an attempt; worker logs carry campaign/approval/delivery ids, channel, outcome, status and codes — never destinations, names or text. The mock provider's durable journal stores keys, channels, payload fingerprints and message ids only; destinations and texts stay in process memory (test accessor); `/admin/stats` returns counters only.

**Mock provider** (`internal/mockprovider`, ADR-020): `POST /v1/sms {to, text}` and `POST /v1/email {to, subject, text}` with `Idempotency-Key` (1–200 printable ASCII); strict JSON (exactly these fields, one object), destination validation (E.164 / email), bounded text → `400 invalid_request`. Every arrival is counted at handler entry. Injected faults (seeded random rates, or a deterministic fault function in tests): `503` and `429 + Retry-After` before the ledger (nothing recorded); a destination with the permanent-failure suffix → `400 destination_rejected`; a timeout fault **commits** the message, then stalls past the client timeout. Ledger: same key + same payload (channel, to, subject, text) → `200`, original `message_id`, `Idempotent-Replayed: true`, nothing sent again; same key + different payload → `422 idempotency_key_reuse`; otherwise accepted → `202 {message_id, status}`. "Accepted" means only that the mock recorded the message; nothing leaves the process. **Durable ledger** (`internal/mockprovider/ledger.go`, ADR-020 amendment): one file (`MOCK_LEDGER_PATH`, Compose volume `mockledger`) = header `MPLEDGER` + version, then frames `"MPR1" | uint32 length | JSON {seq, key, channel, payload_sha256, message_id} | CRC-32C`. No destination or text is stored (the payload is a SHA-256 fingerprint of channel, destination, subject and text). *Commit point*: the frame is written after the last committed one and `fsync` returned; only then is the key added to the in-memory index and 202 sent — under the same mutex as the lookup, so concurrent identical requests produce one acceptance. *Startup*: an exclusive `flock`; every complete frame must have the magic, a bounded length, a matching CRC, a strictly decoded record, `seq = previous + 1` and a new key; the index (and the message-id sequence) is rebuilt from them. A torn tail — < 8 bytes, or a correct-magic frame extending past EOF — can only be an append whose fsync never returned (never acknowledged), so it is truncated (fsynced) and reported. Anything else (checksum, magic, length, sequence, duplicate key, short header, garbage or zeros after the last frame) is corruption: the server refuses to start (sanitized error, file untouched), so it never starts with a fresh ledger and the worker (`depends_on` healthy) does not start either. *Disk failure*: a failed write/fsync answers `503 storage_unavailable` (transient for the client), and every further acceptance is refused until restart; recovery then decides from the file whether that record committed — if it did, the client's retry is a replay, never a second acceptance. Not covered: media corruption the CRC cannot detect, filesystems that do not honour fsync or locks (e.g. some network filesystems), loss of the volume. *Reset*: `docker compose down -v` deletes the volume; `POST /admin/reset` truncates the journal only with `MOCK_ADMIN_RESET_ENABLED=true` (off in Compose); either forgets every earlier key. Per-process counters (arrivals, accepted, replayed, faults) restart at 0; `messages` counts ledger entries (durable), and `?key_prefix=<campaign_id>:` returns per-campaign ledger counts. Without `MOCK_LEDGER_PATH` (unit tests only; required by `cmd/mockprovider`) the index is in memory.

The adapter declares `IdempotencyKeys = true`, retention = until explicit reset (`provider.RetentionUntilReset`), and the worker logs the conditional guarantee: at most one delivered message per (campaign, user, channel) while the mock's durable ledger is retained and intact and the provider implements its declared semantics; requests are at-least-once; not exactly-once. `TestMockProviderRestartReplaysAfterLostResponse` covers accept → lost response → provider restart → worker retry → replay of the original id. `GET /admin/stats` (per channel: arrivals, accepted, replayed, rejections, injected faults, messages, max arrivals per key; duplicate accepted keys), `POST /admin/reset` (tests), `GET /healthz`.

**TOCTOU, stated plainly**: the precheck sees consent and contact as committed when its statement runs. A change committed after that (consent revoked, contact changed) does not stop the request already being made; it is honoured by every later attempt (consent) — the persisted destination is deliberately kept for retries (ADR-007). No lock is held across the request (§10.10, ADR-008).

### 10.13 Reliability as implemented (CP17A)

CP17A completes the reliability half of Slice 4 (ADR-025). Delivery statistics, `/stats`, `/deliveries`, metrics, progress SSE and the dashboard are CP17B.

**Rate limit — algorithm and guarantee.** `delivery.Limiter` (one per channel, nothing shared) is the §10.4 sliding-window log with `N = RATE_LIMIT_PER_SECOND` (100) and `W = 1 s + RATE_SAFETY_MARGIN` (1.05 s): a slot is granted at t only if the N-th previous grant is at least W before t and t is not inside a pause; check-and-record is atomic under a mutex; time is the injected monotonic `platform/clock` (fake in tests). Waiters are served **FIFO** (only the queue head may take the next slot; a cancelled waiter leaves the queue without consuming a slot), so no row starves and claim order is slot order. Granted slots are never refunded. Guarantee on slots: any N+1 consecutive grants span ≥ W. `gatedSender.Attempt` (`delivery/gate.go`) is the only path to the provider: slot → precheck with a context deadline of `min(DISPATCH_MAX_PRECHECK, DISPATCH_MAX_SLOT_TO_REQUEST)` (and a measured over-budget check) → destination filled in (CPU) → the **final gate inside `provider.Client.SendGated`, after the request is built, immediately before `http.Client.Do`** (`clock.Now() ≤ slot + G`). An over-budget precheck or a missed gate wastes the slot, undoes the precheck (`dispatch_started_at = NULL`, the row stays ours) and takes a new slot; after 3 consecutive aborts the row is released 1 s later without counting an attempt. A precheck that timed out on our side but committed after its undo is recognized on the next miss (`MissStaleDispatch`: our lease, `dispatch_started_at` set) and released without an attempt — never reaped into a phantom `unknown`. Hence requests i and i+N leave the process ≥ W − G apart and, with `margin ≥ G + J`, at most N arrive at the provider in any rolling second — **under the assumption J ≤ 30 ms** (measured, not proven). Startup requires `DISPATCH_MAX_PRECHECK < DISPATCH_MAX_SLOT_TO_REQUEST < RATE_SAFETY_MARGIN`. Effective throughput ≈ 95/s per channel.

**One dispatcher across processes (ADR-025).** The limiter bounds one process, so only one process may dispatch: the holder of `pg_try_advisory_lock(DispatchLockKey)` on a dedicated session (taken out of the pool). Every other worker process stands by (reaper, completion detector, sweeper and heartbeat still run) and retries every second. The holder checks its session every `I` = 1 s (timeout `T` = 1 s); a failed check stops granting slots. **Leadership fence in the final gate:** the gate admits a request only if (a) the slot deadline holds, (b) the dispatch context is live (not shut down, lock not known lost) and (c) `now ≤ leaseOK + I + T`, where `leaseOK` is the monotonic reading taken just *before* the latest successful statement on the lock session (acquisition or check). (c) does not depend on the watcher being scheduled on time: a process that stalls after losing its lock cannot issue a request once the fence lapses. A new holder waits `Q = I + T + W` (3.05 s) before its first slot; a failed read of the durable pauses at that point gives the lock up (fail closed).

**What is proven and what is assumed.** Let L be the moment the old holder's lock is lost, a ≥ L the new holder's acquisition. *Enforced by the code (monotonic clocks, mutex, final gate):* (1) slot grants of one process satisfy g_{i+N} − g_i ≥ W; (2) every request is issued — the gate passes, immediately before `http.Client.Do` — within G of its slot, so at most N gate passes per process in any interval shorter than W − G (≥ 1 s with the validated ordering); (3) the old holder's last gate pass is ≤ leaseOK + I + T ≤ L + I + T, because the successful statement that set leaseOK began before L; the new holder's first gate pass is ≥ a + Q ≥ L + I + T + W. So no interval shorter than W contains gate passes of two processes, and **at the gate, at most N requests per channel are issued in any rolling second across the deployment.** This assumes only that both hosts' monotonic clocks advance at the real rate (no assumption about scheduling or clock offsets). *Assumed, not proven:* the step from "issued at the gate" to "arrived at the provider" — time inside `http.Client.Do` (connection reuse/dial, write syscall, kernel, network, including a request that sits in a TCP retransmission for up to the 5 s request timeout, or an old holder that lost the database but still has a working provider connection) — is bounded by J ≤ margin − G (30 ms here). Under that assumption at most N arrive in any rolling second; it is verified by provider-side measurement, not proven. Requests already on the wire during a handover are not recalled; the old holder's responses may still arrive after the new holder starts, which does not affect arrivals.

**Retries (ADR-012).** `Policy.Decide(outcome, attempts, rateLimited, retryAfter, u)` is pure; `u ∈ [0, 1)` is injected (`Config.Rand`, default `math/rand/v2`). Transient (`transient_error`, 5xx, connection failures), `timeout` and `unknown` outcomes retry after **full jitter** `u · min(BACKOFF_CAP, BACKOFF_BASE · 2^(n−1))` for the n-th attempt (defaults 1 s, 60 s), failing after `MAX_TRANSIENT_ATTEMPTS` (5); `permanent_error` (4xx incl. 422 key reuse) is never retried; 429s use their own budget (`MAX_RATE_LIMITED_ATTEMPTS`, 10). Delays are persisted as `next_attempt_at = now() + delay` in the completion transaction, so a scheduled retry survives any restart and is never sent early (test: retry after a restart starts ≥ 1.98 s after the failed attempt finished, database time). Every retry re-runs the precheck (current consent) and sends the same key, persisted destination and frozen rendering. Retry horizon with defaults: 5 × 70 s + (1+2+4+8) s + 10 × 30 s = 665 s (≈ 11 min), plus any time a ready row waits behind a backlog for a slot.

**429 and Retry-After.** `provider.ParseRetryAfter`: delay-seconds must be all digits; HTTP-date relative to now; missing, malformed (`+5`, `1.5`, `5s`), non-positive or past → 1 s; larger than `RETRY_AFTER_CAP` (30 s) — including values that overflow int64 — → the cap. The policy bounds it again to `[1 s, RETRY_AFTER_CAP]`. On a 429 the worker pauses **that channel's limiter only** until now + Retry-After (in memory, immediately after the response), and the completion transaction records one `rate_limited` attempt (`rate_limited_attempts + 1`, `attempts` unchanged), reschedules the row at the same delay and upserts `channel_pauses` (`paused_until = GREATEST(existing, now() + Retry-After)`). The next dispatch on the channel is therefore at the later of the pause end and limiter availability. Requests already granted a slot before the pause still arrive (≤ G later). A new lock holder loads the remaining pauses before its first slot (measured with the database clock, so container clock offsets cancel out) and **does not dispatch if that read fails**; while dispatching it re-reads them after every successful lock check (≈ 1 s), so a pause recorded by another process — e.g. a former holder's late 429 — takes effect within about one check interval (requests already granted a slot still go out within G); claim skips a channel whose durable pause has not ended; expired rows are ignored. If the completion transaction carrying the pause fails, the pause is persisted again on its own; if that also fails it holds in this process's memory only (logged as an error) and a restart within the pause could resume the channel early — the residual of a database outage. The 10th 429 of a row fails it but still pauses the channel.

**Cancellation (§5.2, §10.8; §11.5).** The fence against new requests is the per-attempt precheck (`campaigns.status = 'sending'`), not the final gate: the gate cannot observe a database change without I/O (invariant 11). A precheck that committed before the cancel commit may still lead to one request within G (the §10.10 race boundary). `approval.Repository.Cancel` — one transaction, campaign lock first: lazy expiry under the lock (a due approval is expired and committed; the request is then a version conflict); `cancelled`/`completed*` → `409 invalid_state` *before* the version check; stale version → `409 version_conflict`; then by status: draft/segmented/content_ready → nothing else; pending_approval/approved → approval `revoked(campaign_cancelled)`, held rows deleted (count = `snapshot_count`), `active_approval_id` cleared; sending → `pending`/`retry_scheduled` rows → `cancelled` (`last_error = campaign_cancelled`, uncertain if an earlier attempt was ambiguous), `in_flight` rows untouched; campaign → `cancelled`, `cancelled_at = now()`, version + 1. **In-flight boundary:** claim requires a sending campaign; a claim racing the cancel either committed first (the row is in flight and counted) or skips the row the cancel locked; every in-flight row makes **at most one** further request (its precheck, retry, release and reap all refuse a non-sending campaign), so `in_flight` in the response is the honest upper bound — zero further requests is never claimed while it is non-zero, and an accepted request is recorded as `sent` with its message id, never erased. Release and reaping of rows of a cancelled campaign now end `cancelled` (previously they returned to `pending` and could never be claimed again). Completion detection only completes `sending` campaigns, so a cancelled campaign is never completed; concurrent cancels serialize on the campaign lock (one transition).

**Expiry sweeper (§5.3 (2a)).** `approval.Sweeper` runs in every worker process every `APPROVAL_SWEEP_INTERVAL` (30 s): an unlocked candidate query (`status IN (pending, approved) AND expires_at <= clock_timestamp()`, batch 100, ≤ 1,000 per pass), then `ExpireDue` per campaign (campaign lock first, `clock_timestamp()` after it). Concurrent sweepers, the API's lazy expiry and live workers serialize on the campaign row; the loser finds nothing live and due, so an approval expires — and the version advances — exactly once. Consumed approvals (sending/finished) are never candidates, whatever their old `expires_at`. Pending (15 min) and approved (24 h from confirmation) TTLs are distinguished by the stored `expires_at`. Tested: two sweepers in parallel with a live worker; a sweeper that blocks on the campaign lock before expiry and acquires it after expiry expires the approval.

**Heartbeat.** Every worker process upserts `worker_heartbeats(worker_id, now())` every 5 s and deletes rows older than a day. CP17B reports the freshest one in `/readyz` and in every statistics snapshot (§13.1, §15).

**Mock provider (§10.11).** Per channel it keeps the arrival times of the last second (handler entry, monotonic) and reports `max_in_any_rolling_1s` (half-open windows) and its own strict limiter (`MOCK_RATE_LIMIT_PER_SECOND`, default 100; an arrival beyond it gets `429 rate_limit_exceeded`, counted as `limit_exceeded`, apart from injected 429s).

**Recovery invariants (re-audited).** Expired leases are reaped (`unknown` attempt iff a precheck committed); stale holders cannot complete (fencing by `leased_by`, `in_flight` and the precheck's `dispatch_started_at`); no transaction is open while a provider request is served (integration test checks `pg_stat_activity` from inside the provider handler); all attempts of a delivery reuse key, destination and body, and the mock's durable ledger replays (no duplicate accepted key in any test or smoke run). Requests are at-least-once; at most one delivered message per (campaign, user, channel) only under the mock's declared, retained ledger; never exactly-once.

**Verification.** Unit (fake clock, `-race`): limiter boundary, 1,000 grants from 50 goroutines never > N per W, FIFO, cancel-while-waiting, pause isolation; gate cases (a)–(d) of §10.11 plus deadline-cancelled prechecks; backoff progression, cap, overflow, jitter spread; Retry-After parsing. Integration (testcontainers, real worker, mock over TCP): provider-observed rate at N = 100 with 5 % 5xx and 1 % 429 over 1,200 messages (`max_in_any_rolling_1s` = 100, `limit_exceeded` = 0, arrivals = attempt rows); two workers with failover at N = 20; simultaneous 429s pause SMS only; durable pause across restart; cancellation before/while sending and racing completions; release/reap after cancel; sweeper concurrency and lock-wait expiry; retry across restart.

---

## 11. API

Base `/api/v1`. Errors: `{"error": {"code", "message", "details": [...], "retryable": bool}}`.
Codes: `validation_failed`, `ai_output_invalid`, `llm_unavailable`, `llm_refused`, `llm_truncated`, `invalid_state` (409), `version_conflict` (409), `approval_expired` (409), `approval_mismatch` (409), `empty_audience` (422), `not_found`.

| Method & path | Allowed in status | Purpose |
|---|---|---|
| `POST /campaigns` | — | create draft `{name, brief}` |
| `GET /campaigns` · `GET /campaigns/{id}` | any | list / detail (rule, interpretation, channels, current content + validation, active approval incl. `expires_at`, delivery summary) |
| `POST /campaigns/{id}/interpret` | draft … approved | Claude → validated rule + audience stats |
| `GET /campaigns/{id}/audience` | segmented … approved | recompute live preview statistics |
| `PUT /campaigns/{id}/channels` | segmented … approved | `{channels, expected_version}` |
| `POST /campaigns/{id}/content/generations` | segmented … approved | **SSE** response: `meta`, `status`, `delta`*, `validated` \| `error` |
| `POST /campaigns/{id}/content/validate` | segmented … approved | validate without saving (debounced editor) |
| `PUT /campaigns/{id}/content` | segmented … approved | save edited version `{sms_body, email_subject, email_body, expected_version}` |
| `POST /campaigns/{id}/approvals` | content_ready, pending_approval (approved: see §5.2 note ¹) | **prepare**: returns `{approval_id, expires_at, segment_version, content_version, channels, audience_stats, rendered_content}` |
| `GET /campaigns/{id}/approvals/{aid}` | any | approval summary (CP15, §11.3; restores the dialog on reload) |
| `POST /campaigns/{id}/approvals/{aid}/confirm` | pending_approval | `{expected_version, segment_version, content_version, channels}` |
| `POST /campaigns/{id}/approvals/{aid}/withdraw` | pending_approval, approved | `{expected_version}`: discard approval |
| `POST /campaigns/{id}/send` | approved (idempotent in sending) | `{approval_id}` → `202 {status, released: {sms, email}}` |
| `POST /campaigns/{id}/cancel` | not terminal | cancel |
| `GET /campaigns/{id}/stats` | any | delivery statistics snapshot (§13) |
| `GET /campaigns/{id}/events` | any | **SSE** `stats` snapshots every 1 s; closes after a terminal status |
| `GET /campaigns/{id}/deliveries?status=&cursor=&limit=` | any | paged rows (masked destinations) for the failure table |
| `GET /healthz` · `GET /readyz` · `GET /metrics` | — | liveness · readiness (DB, migrations, worker heartbeat in body) · Prometheus |

Mock provider: `POST /v1/sms`, `POST /v1/email` (header `Idempotency-Key`), `GET /admin/stats`, `POST /admin/reset` (tests only). Fault rates via environment.

### 11.1 Slice 1a contracts as implemented (CP6)

Only these campaign routes exist in Slice 1a: there is **no** `GET /campaigns` (list) and **no** endpoint to edit `name`/`brief`. The frontend opens campaigns by URL (`/campaigns/{id}`) and revises a brief by creating a new campaign. Request bodies are strict JSON objects (`Content-Type: application/json`, unknown/duplicate properties → `422 validation_failed` with per-field `details`).

| Request | Success | Notes |
|---|---|---|
| `POST /campaigns` `{name, brief}` | `201` campaign | name 1–120, brief 10–2000 runes after NFC + trim; no control characters (brief allows `\n`, `\t`) |
| `GET /campaigns/{id}` | `200` campaign | id must be a canonical UUID (else `422`); unknown → `404 not_found` |
| `POST /campaigns/{id}/interpret` (body empty or `{}`) | `200 {campaign, audience, audience_error?}` | allowed in draft/segmented/content_ready (`pending_approval`/`approved` → `409 invalid_state` until Slice 3 can revoke the approval) |
| `GET /campaigns/{id}/audience` | `200 {campaign_id, campaign_version, segment_version, channels, audience}` | draft → `409 invalid_state` |
| `PUT /campaigns/{id}/channels` `{channels, expected_version}` | `200` campaign | segmented/content_ready only |

- **Campaign DTO**: `{id, name, brief, status, channels, segment_rule, segment_version, interpretation{summary, assumptions, unsupported, model, prompt_version}, content_brief{goal, tone, length, offer}, version, created_at, updated_at}`; `segment_rule`, `interpretation`, `content_brief` are `null` before the first successful interpretation. `interpretation.model` is `"demo"` for the demo interpreter. `suggested_channels` is not persisted separately (see channels below).
- **Interpretation saved, audience failed**: the interpretation is committed before the audience is computed. If the statistics then fail, the response is still `200` with `audience: null` and `audience_error` (same shape as the error envelope body, e.g. `audience_unavailable`, retryable). Clients retry `GET /audience`, never the interpretation.
- **Interpretation errors** (nothing is saved): `422 invalid_brief` (demo declines; field `details`, not retryable), `422 ai_output_invalid` (`details` + `raw_proposal`), `422 llm_refused`, `503 llm_unavailable` (retryable), `502 llm_truncated` / `llm_rejected`; `409 version_conflict` if the campaign changed while the model was working (the result is discarded).
- **Version conflicts**: every write bumps `version` — every persisted campaign state mutation increments it exactly once, including system transitions such as approval expiry (§9.1). `PUT /channels` is a compare-and-set on `expected_version` → `409 version_conflict` on mismatch; nothing is merged or retried server-side. A selection equal to the stored one is a no-op (version unchanged). Audience responses carry `campaign_version` so clients can discard statistics computed for another version.
- **Channels**: canonical order `sms, email`, de-duplicated; unknown values → `422` (`channels[i]`, `not_allowed`). On the **first** successful interpretation (`segment_version = 0` and no channels) the channels are set from the AI's `suggested_channels`; afterwards only `PUT /channels` changes them.
- **Empty selection** (`[]`) is valid and explicit: `by_channel = {}`, `messages = 0`, `reach_users = {both: 0, sms_only: 0, email_only: 0, unreachable: matched_users}`; `matched_users` is unaffected.

### 11.2 Slice 2 content contracts as implemented (CP13)

| Request | Success | Notes |
|---|---|---|
| `GET /campaigns/{id}/content` | `200 {campaign_id, campaign_version, channels, content, validation, example_values, limits}` | any status; `content` and `validation` are `null` without a version (no draft is fabricated); `validation` is for the **current** channel selection, bound to `campaign_version` |
| `POST /campaigns/{id}/content/validate` `{sms_body?, email_subject?, email_body?}` | `200 {campaign_id, campaign_version, channels, validation, example_values, limits}` | read-only (no write, no version bump); segmented … approved; malformed JSON / wrong types / unknown properties → `422 validation_failed`; well-formed JSON with invalid content → `200` with `validation.valid = false` and field errors |
| `PUT /campaigns/{id}/content` `{sms_body?, email_subject?, email_body?, expected_version}` | `200 {campaign, content, created, notes}` | segmented/content_ready; stale `expected_version` → `409 version_conflict`; invalid content → `422 validation_failed` (`details` = field errors); unchanged canonical text → `created: false`, nothing written |
| `POST /campaigns/{id}/content/generations` (body empty or `{}`) | `200 text/event-stream` (§12) | errors before the stream starts are JSON envelopes (`404`, `409 invalid_state`, `422 validation_failed` incl. no channels) |

- **Draft DTO** (`content.Draft`): each field is a string or `null`/absent ("not provided"). The editor sends the selected channels' fields and `null` for the others.
- **Content version DTO**: `{id, version, origin (ai|edited), model, prompt_version, created_at, sms_body, email_subject, email_body}`; AI versions have model and prompt version, edited versions neither; text is canonical (NFC, trimmed). Versions are immutable (only `INSERT`/`SELECT` exist for `campaign_contents`).
- **Validation DTO** (`content.Result`): `{valid, errors[], warnings[], sms, subject, body, rendered_example}`; `sms`/`subject`/`body` are `{chars_worst_case, limit}` or `null`; `rendered_example` = `{sms, email: {subject, body, footer}}` rendered with the fixed **fictional** `example_values` (`content.ExampleValues`, never customer data), `null` when there are errors. `limits` = `{sms_body: 160, email_subject: 80, email_body: 5000}`.
- **Campaign DTO** gains `current_content_id` (the immutable version the campaign points at, `null` without content). The content itself is served by `GET /content` (not embedded in every campaign response), so campaign responses stay cheap; the client fetches it on load and after every own change of the campaign version.
- **Errors**: generation failures use the AI codes of §11.1 with content-specific messages (e.g. `llm_refused`: "declined to write this content"); `ai_output_invalid` carries `details` and the bounded `raw_proposal` (untrusted, displayed escaped and collapsed).
- **Size limits**: request bodies 64 KiB (strict JSON); one SSE event ≤ 256 KiB, one delta ≤ 2 KiB (longer deltas are split at rune boundaries); the client parser bounds an event at 1 MiB and the live preview at 20,000 characters per field.

---

### 11.3 Slice 3 approval contracts as implemented (CP15)

Every response is `{campaign, approval, server_time}` (`campaign` = the campaign DTO at the version the approval was read or written at; `server_time` lets the client align its countdown). Request bodies are strict JSON (64 KiB, no unknown or duplicate properties, integers only, `422 validation_failed` with field `details`; non-JSON media type → `415`).

| Request | Success | Errors |
|---|---|---|
| `POST /campaigns/{id}/approvals` `{expected_version}` | `201` + `Location` | `409 version_conflict` / `invalid_state` (incl. `approved`, §5.2 ¹), `422 validation_failed` (content invalid for the selection, no channel), `422 empty_audience`, `409 audience_changed` (retryable), `503 campaign_busy` (retryable) |
| `GET /campaigns/{id}/approvals/{aid}` | `200` | `404 not_found` (unknown, or another campaign's approval) |
| `POST …/{aid}/confirm` `{expected_version, segment_version, content_version, channels}` | `200` | `409 approval_expired`, `409 approval_mismatch` (`details` = differing fields), `409 version_conflict`, `409 invalid_state`, `404` |
| `POST …/{aid}/withdraw` `{expected_version}` | `200` | `409 approval_expired`, `409 invalid_state` (already withdrawn, consumed, sending/finished), `409 version_conflict`, `404` |

- `expected_version` is required on every mutation (an addition to the §11 confirm payload: it makes a confirm from a stale tab a deterministic `409` even when the binding is unchanged). `channels` must be non-empty, known and without duplicates.
- **Approval DTO**: `{approval_id, campaign_id, status, revoke_reason, campaign_version, segment_version, content_id, content_version, content_origin, content_model, content_prompt_version, channels, audience_stats, snapshot_count, snapshot_digest, snapshot: {<channel>: {recipients, render_issues}}, prepared_at, expires_at, approved_at, approved_by, closed_at, review: {validation, offer_findings, offer} | null, rendered_content, superseded_approval_id}`. It contains the documented prepare fields (`approval_id, expires_at, segment_version, content_version, channels, audience_stats, rendered_content`). `review` and `render_issues` are `null` for a closed approval.
- The **campaign DTO** gains `active_approval_id` (pending/approved approval, or the consumed one once sending; `null` otherwise).

### 11.4 Slice 4 send contract as implemented (CP16)

`POST /campaigns/{id}/send` `{approval_id, expected_version}` (strict JSON, 64 KiB, both required; `expected_version` is an addition to the §11 payload, as for every other mutation, ADR-024).

| Result | Response |
|---|---|
| This request consumed the approval | `202 {status: "sending", approval_id, released: {<channel>: rows}, already_started: false, campaign, server_time}` |
| Repeat for the same consumed approval while the campaign is `sending`, `completed` or `completed_with_failures` | `200` same shape, `already_started: true`, current campaign; nothing written (whatever `expected_version` says) |
| Approval due (expired before or while waiting for the lock) | `409 approval_expired` (the expiry is committed: approval `expired`, held rows deleted, campaign `content_ready`, version + 1) |
| Approval pending, revoked, consumed by a cancelled campaign; campaign not `approved` | `409 invalid_state` |
| Stale `expected_version` | `409 version_conflict` |
| Approval binding ≠ campaign (segment version, content, channels) | `409 approval_mismatch` |
| Unknown campaign / approval, or another campaign's approval | `404 not_found` |
| Held rows ≠ stored partition/count/digest | `500 internal` (rollback; a bug) |
| Lock or statement timeout | `503 campaign_busy` (retryable) |

Transaction: lazy expiry in its own committed transaction; then lock campaign → lock approval → expiry re-check under the lock (`clock_timestamp()`) → state/version checks → binding → re-review the bound content and verify the held rows against the stored partition, count and digest → `UPDATE campaign_approvals SET status='consumed', consumed_at=clock_timestamp() WHERE … status='approved' AND expires_at > clock_timestamp() AND <binding>` → `UPDATE campaign_deliveries SET status='pending', next_attempt_at=now() WHERE approval_id=$a AND status='held'` (per-channel counts must equal the stored eligible counts, total = `snapshot_count`) → campaign `sending`, `send_started_at = now()`, version + 1. Any failure rolls back everything. Concurrent sends serialize on the campaign lock: one starts, the others are idempotent repeats. A consumed approval never expires: its old `expires_at` is ignored by lazy expiry, so a send started within the TTL is never interrupted. The response carries counts and ids only. The API never calls the provider; the frontend has no Send button yet.

### 11.5 Cancel contract as implemented (CP17A)

`POST /campaigns/{id}/cancel` `{expected_version}` (strict JSON, 64 KiB; `expected_version` added as for every mutation).

| Result | Response |
|---|---|
| Cancelled (any non-terminal status) | `200 {status: "cancelled", previous_status, revoked_approval_id, released_held_rows, cancelled_deliveries: {<channel>: n}, in_flight: {<channel>: n}, campaign, server_time}` |
| Already cancelled, completed or completed_with_failures (any `expected_version`) | `409 invalid_state` |
| Stale `expected_version` | `409 version_conflict` (also when lazy expiry just closed a due approval) |
| Unknown campaign | `404 not_found` |
| Lock or statement timeout | `503 campaign_busy` (retryable) |

`in_flight` counts rows in flight at the commit: each may still produce at most one provider request (already on the wire or past its precheck); it cannot be recalled. Counts only — no recipient data.

### 11.6 Delivery statistics, list and progress contracts as implemented (CP17B)

Read-only, any campaign status, served by `delivery.StatsReader` (one short `REPEATABLE READ READ ONLY` transaction per read, `statement_timeout` 5 s, never open between reads). A due live approval is expired lazily first (§5.3 (b)), as for every campaign read.

| Request | Success | Errors |
|---|---|---|
| `GET /campaigns/{id}/stats` (no query parameters) | `200` statistics snapshot (§13.1) | `422 validation_failed` (bad id, any query parameter), `404 not_found`, `500 internal` if the rows do not reconcile with the approved totals (a bug; logged) |
| `GET /campaigns/{id}/deliveries?status=&channel=&uncertain=&cursor=&limit=` | `200 {campaign_id, approval_id, items, next_cursor, limit}` | `422 validation_failed` (unknown or repeated parameter; `status` ∉ the eight partition statuses — `held` is never listed; `channel` ∉ {sms, email}; `uncertain` ∉ {true, false}; `cursor` not a positive decimal id; `limit` ∉ 1–100, canonical decimal), `404` |
| `GET /campaigns/{id}/events` | `200 text/event-stream` (§12.1) | before the stream: `422`, `404`, `503 too_many_streams` (retryable; 64 open streams per API process) |

- **Delivery list.** Rows of the consumed approval only, ordered by `id` ascending; `cursor` = the last id of the previous page (`next_cursor`, set when more rows may follow; default page 50, max 100). Ids never change, so pages never duplicate or skip a row under concurrent worker updates; a row may leave or enter a `status` filter between pages. `items` is `[]` (and `approval_id` null) before a send. **Item** = `{id, channel, status, attempts, rate_limited_attempts, outcome_uncertain, reason, destination_masked, sent_at, updated_at}`: `reason` is the row's `last_error` passed through `delivery.SafeCode` (`[a-z0-9_]{1,64}`, otherwise `other`; empty → `unspecified`); `destination_masked` uses the log masking (`+90********67`, `a***@example.com`; null before the first precheck). Never returned: user id, names, raw contacts, frozen `vars`, message text, idempotency key, provider message id. The dashboard does not display `destination_masked`.

## 12. Streaming (SSE)

- **Content generation**: the `POST` response itself is `text/event-stream`; the frontend reads it with `fetch` + `ReadableStream`. Not resumable by design; disconnect cancels the LLM call.
- **Campaign status**: `GET /events` via `EventSource`. Each event is a **full snapshot**, so reconnection needs no replay or `Last-Event-ID`. Heartbeat comment every 15 s, `retry: 3000`. Server polls the stats query once per second per open stream.
- Headers: `Content-Type: text/event-stream`, `Cache-Control: no-cache`, `X-Accel-Buffering: no`; nginx `proxy_buffering off` for `/api/`.

**Content generation stream as implemented (CP13).** Every event has an explicit name and exactly one `data:` line of JSON:

| Event | Data | When |
|---|---|---|
| `meta` | `{campaign_id, campaign_version, channels, fields, prompt_version, limits}` | first, after the state checks; `campaign_version` is the state the generation is based on |
| `status` | `{phase, attempt?, reason?, problems?}`, `phase` ∈ `generating` (attempt n starts; reason `initial` \| `truncation_retry` \| `repair`), `thinking`, `validating`, `repairing` (problems = field errors of the rejected attempt), `persisting` | workflow progress |
| `delta` | `{attempt, field, text, preview: true}` | a fragment of the model's raw output for one field of the current attempt — **untrusted, unvalidated, not saved** |
| `validated` | `{campaign, content, created: true, notes}` (same shape as `PUT /content`) | terminal success, written **only after the transaction committed** |
| `error` | `{status, error: {code, message, details, retryable, raw_proposal?}}` | terminal failure (nothing was saved) |

Exactly one terminal event ends a stream that started. The handler runs the service in its own goroutine and is the only writer (events arrive over an unbuffered channel, so their order is the service's order); each write is flushed and bounded by a 30 s write deadline; a `: keep-alive` comment is sent every 15 s (Opus thinking can take a while; nginx `proxy_read_timeout 300s` ≥ `CLAUDE_CONTENT_TIMEOUT`). A client disconnect or a failed write cancels the request context, which closes the provider stream; nothing is persisted unless the commit had already happened (then the client sees the version after a reload). No transaction is open while the model streams. The stream is not resumable (no `id`, no `retry`). The browser reads it with `fetch` + `ReadableStream` and an incremental WHATWG-conformant parser (`src/api/sse.ts`; LF/CRLF/CR, split chunks and split UTF-8, comments, a truncated trailing event is discarded), decodes every event strictly and rejects unknown names or shapes.

### 12.1 Delivery progress stream as implemented (CP17B)

`GET /campaigns/{id}/events` (`EventSource`). Every event has a name and one `data:` line of JSON:

| Line | When |
|---|---|
| `retry: 3000` | first (EventSource reconnection delay) |
| `event: stats` / `data: <§13.1 snapshot>` | immediately on connection (fresh from the database), then after each 1 s poll **whose snapshot changed** (everything except `updated_at` and `worker.heartbeat_age_seconds` is compared, so an idle stream sends one event per worker heartbeat, ≈ 5 s) |
| `: keep-alive` | every 15 s |
| `event: end` / `data: {campaign_id, campaign_status, campaign_version, outcome}` | exactly once, right after the first snapshot with `terminal = true` (campaign `completed`, `completed_with_failures` or `cancelled` **and** no row `pending`/`in_flight`/`retry_scheduled`); the server then closes the stream |

- A cancelled campaign whose in-flight rows have not finished is `outcome: settling` and the stream stays open until they settle, so `end` always carries the final numbers. A campaign cancelled before sending (no consumed approval) is terminal at once: one snapshot, `end`.
- Snapshots are full and read from committed rows, so a reconnect needs no event ids or replay, and a delivery is never shown as sent before the worker's completion transaction committed. Reconnecting to a finished campaign yields one snapshot and `end`.
- Without `end` the stream closes after 10 min (lifetime cap), after 3 consecutive failed reads, on a write failure (30 s write deadline) or on client disconnect (request context); each poll runs with a 5 s timeout. The client reconnects in all of these cases.
- nginx: `/api/` already has `proxy_buffering off`, `proxy_read_timeout 300s` (≫ the 15 s keep-alive) and the API sends `X-Accel-Buffering: no`.
- Polling (1 s per open stream, three to four short queries) was chosen over `LISTEN/NOTIFY` (C tier) per ADR-017; ADR-026.

---

## 13. Delivery statistics

Per channel, every row of the consumed approval is in exactly one status, so
`approved_total = pending + in_flight + retry_scheduled + sent + failed + skipped_consent_revoked + skipped_no_contact + cancelled`
where `approved_total` equals the approval's `by_channel[ch].eligible` (asserted). `uncertain` is an **overlay attribute** (subset of terminal non-sent rows), shown separately and never added to the partition. Rate = provider attempts in the last 10 s / 10.

```json
{ "campaign_status": "sending", "approval_id": "…",
  "channels": { "sms": { "approved_total": 251, "pending": 80, "in_flight": 12, "retry_scheduled": 4, "sent": 150,
                         "failed": 3, "skipped_consent_revoked": 1, "skipped_no_contact": 1, "cancelled": 0,
                         "uncertain": 0, "attempts_per_sec": 94.8 }, "email": { "…": "…" } },
  "updated_at": "…" }
```

### 13.1 Statistics snapshot as implemented (CP17B)

Additive to the documented shape (ADR-026). Partition counts come from `campaign_deliveries` of the campaign's **consumed** approval (one row per delivery, whatever its number of attempts); attempt counts from `delivery_attempts`; `approved_total` per channel from the approval's stored `audience_stats.by_channel[ch].eligible`, and the partition must sum to it (else `500`, never a silently wrong number).

```json
{ "campaign_id": "…", "campaign_status": "sending", "campaign_version": 7,
  "approval_id": "…", "segment_version": 1, "content_id": 12, "content_version": 1,
  "channels": { "sms": { "approved_total": 101, "pending": 40, "in_flight": 8, "retry_scheduled": 2, "sent": 50,
                         "failed": 1, "skipped_consent_revoked": 0, "skipped_no_contact": 0, "cancelled": 0, "uncertain": 0,
                         "attempts": { "total": 56, "by_outcome": { "accepted": 50, "replayed": 0, "rate_limited": 2,
                                       "transient_error": 3, "timeout": 0, "permanent_error": 1, "unknown": 0 },
                                       "retries": 5, "retried_deliveries": 4, "recent": 480, "per_second": 48.0 },
                         "reasons": [ { "status": "retry_scheduled", "code": "http_503", "count": 2 } ] } },
  "totals": { "approved_total": 101, "…": "Σ channels" }, "attempts": { "…": "Σ channels" },
  "processed": 51, "remaining": 50, "progress": 0.505,
  "campaign_terminal": false, "terminal": false, "outcome": "sending",
  "send_started_at": "…", "completed_at": null, "cancelled_at": null,
  "worker": { "status": "fresh", "last_seen_at": "…", "heartbeat_age_seconds": 1.4, "stale_after_seconds": 15, "fresh_processes": 1 },
  "updated_at": "…" }
```

- **Identities** (checked by the server and again by the client decoder): per channel `approved_total = pending + in_flight + retry_scheduled + sent + failed + skipped_consent_revoked + skipped_no_contact + cancelled`; `uncertain ≤ approved_total − sent` (overlay, never added); `totals` = Σ channels; `processed` = terminal rows, `remaining = pending + in_flight + retry_scheduled`, `processed + remaining = approved_total`; `progress = processed / approved_total`, `0` for an empty snapshot (no division by zero); `attempts.total = Σ by_outcome`; `retries = total − deliveries with ≥ 1 attempt`; `retried_deliveries` = rows with `attempts + rate_limited_attempts > 1`; `per_second = recent / 10` (attempts finished in the last 10 s, database time); `accepted + replayed ≥ sent`.
- **Flags.** `campaign_terminal` ⇔ status ∈ {completed, completed_with_failures, cancelled}; `terminal` ⇔ `campaign_terminal` ∧ `remaining = 0`. `outcome` = the status when terminal, `settling` (cancelled, rows still in flight), `sending`, or `not_started` (before a send: `approval_id`/versions null, `channels {}`).
- **Three different counts:** `sent` = deliveries the provider **accepted** (not real-world delivery to a handset or inbox — not tracked); `attempts.total` = provider requests recorded (retries, 429s, replays included; a reaped `unknown` attempt may not have reached the provider); the mock provider's ledger `messages` = distinct accepted keys (a replay reuses the original message). With the mock, `sent` = ledger messages for the campaign (smoke-send checks it).
- **Reasons**: `(status, code, count)` over rows not sent (`retry_scheduled`, `failed`, skipped, `cancelled`), codes from the worker (`no_consent`, `no_contact`, `campaign_cancelled`, `campaign_not_sending`, `lease_expired`, provider codes such as `destination_rejected`, `http_<status>`), sanitized, at most 20 per channel (smaller cells folded into `other` per status).
- **Worker**: freshest `worker_heartbeats` row, database time. `fresh` ⇔ age ≤ `delivery.HeartbeatStaleAfter` = 15 s (3 × the 5 s heartbeat period), else `stale`; `absent` without any row. `fresh_processes` counts processes with a fresh heartbeat (at most one dispatches, ADR-025).

---

## 14. Frontend

Vue 3 + TypeScript (strict) + Vite + Vue Router + Pinia (one store for the active campaign); Vitest + Vue Test Utils; Playwright (B, not built).

| Campaign status | UI |
|---|---|
| draft | Brief step: `BriefInput` → Interpret |
| segmented | Audience step: `InterpretationCard`, `SegmentRuleView` (read-only chips), `AudienceCard` (partition + channel checkboxes) |
| content_ready | Content step: `ContentStreamPanel` (Generate/Regenerate, live fields, "thinking" state), `SmsEditor` + `SmsPreview`, `EmailEditor` + `EmailPreview` (sandboxed iframe, escaped text, footer), `ValidationList`; "Review & approve" → prepare |
| pending_approval | `ApprovalDialog`: statistics **from the prepare response**, rendered content, channels, countdown to `expires_at`; Confirm / Withdraw. On reload the dialog is restored from the active approval |
| approved | Approved banner with countdown (24 h), **Send**, Withdraw; editing is allowed but warns that it revokes the approval |
| sending / completed* / cancelled | `CampaignDashboardPage`: `ChannelProgress` (stacked bar per channel over `approved_total`), `StatTiles`, uncertain count, `FailureTable`, `ConnectionBadge`; Cancel while sending |

- Expiry/withdraw/supersede detected on any response (`409 approval_expired` or status change) → banner "Approval expired — review again".
- Composables: `useContentStream` (fetch-stream SSE parser, abort on unmount/regenerate), `useCampaignEvents` (EventSource, backoff, close on terminal), `useCampaign`.
- No `v-html` for any AI or user content; variables highlighted via tokenized spans. Client-side counters are UX only; the server is authoritative.
- Slice 2 (CP13): `ContentPanel` (step 5) with `GenerationProgress` (phases, attempt, live preview labelled "not validated, not saved" in an escaped `<pre>`, Cancel), `SmsEditor`/`EmailEditor` (only for the selected channels; server-computed worst-case counters next to the typed length; inline errors and warnings), `SmsPreview`/`EmailPreview` (rendered example with the fictional values; the email in an `<iframe sandbox="">` whose `srcdoc` is built from escaped text), `ValidationList`, `ContentVersionBadge` (version, AI model/prompt vs. "Edited manually", demo generator called out). The store keeps the draft, validates it on the server (debounced 350 ms; results bound to the draft revision and campaign version, re-validated when the campaign version changes), saves only a revision the server has validated (so its warnings were shown) with `expected_version` (a `409` keeps the draft for an explicit save after reload; never retried automatically), blocks generation while the draft has unsaved edits, drops stream events after navigation (epoch), aborts the stream on unmount/navigation/Cancel and reloads campaign + content after any failure, interruption or cancellation (a commit may have happened just before).
- Slice 3 (CP15): `ApprovalPanel` (step 6: *Prepare approval* with the reason it is unavailable — unsaved or invalid draft, no channel; pending: *Review & confirm* / *Withdraw* with countdown; approved: read-only summary with a 24 h countdown and a two-step *Withdraw approval…*; banners for confirmed / withdrawn / expired / replaced), `ApprovalDialog` (explainer of what confirming authorizes and that consent/contact are re-checked at delivery, `ApprovalSummary` — channels, content version + origin, segment version, digest, per-channel frozen recipients with `no_consent`/`no_contact`, reach, render issues, non-offer warnings, rendered previews — the offer/price findings listed individually with the authorized offer, explicit acknowledgement checkboxes, Confirm / Withdraw / *Close without confirming*). No send button. The store applies an approval only together with the campaign of the same response (same version), runs one approval mutation at a time, never retries a `409` (shown with *Reload latest campaign*), re-reads the approval (server-side lazy expiry) when the display countdown elapses and disables Confirm meanwhile, restores the dialog on reload of a pending campaign, drops late responses after navigation (epoch), and decodes approval responses strictly (closed property sets, cross-field consistency).
- Slice 4/5 delivery UI (CP17B): the dashboard is a section of the campaign page (`components/dashboard/DeliveryPanel.vue`, step 7), not a separate route: one stable URL, one store. It appears for `approved` (Start sending) and leads the page for `sending`/finished campaigns. **Start sending…** opens a confirmation (`ConfirmDialog`) with the frozen per-channel counts, content version, irreversibility, "the worker may start before this returns" and what cancel cannot do; the store sends `{approval_id, expected_version}` once (a second click while pending is ignored), treats `already_started` as success, never retries a `409`, refreshes the approval on `approval_expired`, and on a lost response (network / invalid response) or `invalid_state` re-reads the campaign with a fresh GET instead of guessing. **Cancel sending…** (sending only) explains the in-flight limit and reports `cancelled_deliveries` / `in_flight`; a cancel racing completion (`409 invalid_state`) or a lost cancel response also reconciles with a GET. Live data: `useCampaignEvents` (EventSource; strict decoding of every event; a malformed or oversized event is dropped, the source closed and reopened for a fresh snapshot; events of superseded connections and of other campaigns are ignored; snapshots older than the one shown (`updated_at`) are never applied; own capped exponential reconnect 1 s → 15 s instead of the browser's; `end` closes for good; polling `GET /stats` every 3 s if EventSource is unavailable; manual Refresh). A snapshot reporting a newer `campaign_version` triggers a campaign re-read, so the status badge follows completion or a cancel elsewhere. Views: connection badge, worker-offline warning while sending with a non-fresh heartbeat, overall and per-channel stacked progress (`ChannelProgress`), tiles (sent/failed/skipped/cancelled/pending, attempts, retries, attempts/s), uncertain overlay note, reason summary, version identity, final summary distinguishing *Completed* from *Completed with failures* and *Cancelled*, and `FailureTable` (`GET /deliveries`, filters by status or uncertain, *Load more*; ids, reasons and attempt counts only — no recipient data, no message text). Responsive: auto-fit grids and a table that scrolls inside its own container (no page overflow at 375 px).
- Slice 1a (CP7): routes `/campaigns/new` and `/campaigns/:id` (reloadable). The store applies a campaign only if its `version` is not older than the one shown, shows audience statistics only for the matching `campaign_version`, aborts superseded requests, and never retries a `409` automatically (the rejected channel selection is offered for an explicit re-apply after reload). The shell reads `llm_provider` from `GET /readyz` to label demo mode, so the CP8 proxy must forward `/readyz` as well as `/api/`.

---

## 15. Observability

- **api**: `/healthz` (process alive), `/readyz` (DB ping + migration version; worker heartbeat age reported in the body, readiness does not fail on it), `/metrics`.
- **As implemented (CP17B)**: `/readyz` = `{status, database, migration_version, migration_dirty, llm_provider, worker: {status, last_seen_at, heartbeat_age_seconds, stale_after_seconds: 15, fresh_processes}, sending_campaigns}`. `status`/HTTP code depend only on the database and migrations (`503 not_ready` otherwise); the worker block distinguishes `fresh`/`stale`/`absent` (and `unknown` with `sending_campaigns: null` when the heartbeat cannot be read) and never fails readiness, so it does not flap while nothing is sending; an absent or stale heartbeat is never reported as healthy. API liveness stays `/healthz`.
- **`/metrics` as implemented (CP17B, ADR-026)**: Prometheus text exposition 0.0.4 (`text/plain; version=0.0.4; charset=utf-8`) from `platform/metrics`, a minimal standard-library registry (counters, gauges, histograms with closed label sets; `HELP`/`TYPE` per family, escaped help and label values, families sorted by name), on the API router (`GET /metrics`, not proxied by nginx) and the worker's health port (`:9091/metrics`). The handler renders memory only: no database call, bounded output, unaffected by a database outage. Definitions (types from the names' Prometheus conventions and §10.1):
  - `deliveries_total{channel,outcome}` — counter; `outcome` ∈ terminal statuses. Incremented after the committing transaction of a terminal transition: worker completion (`Complete` returning a terminal status), finish-without-request (skips, render failures, campaign not sending), release/reap ending `cancelled`, reap ending `failed`; the API's cancel transaction adds its cancelled queued rows. A row is counted once (every path is guarded by lease/status, so a transition commits once); retries, 429s and replays never add to it.
  - `provider_request_duration_seconds{channel}` — histogram (buckets 5 ms … 10 s), observed in the worker after `SendGated` returned, for every request actually issued; the clock is read in `provider.Client.SendGated` before the request is built, so nothing is added between the final gate and `http.Client.Do`. Its count equals the attempt rows (reaped `unknown` attempts made by a crashed process are not observed by anyone) and the provider's arrivals.
  - `provider_rate_limited_total{channel}` — counter of 429 responses (injected or the mock's own limiter).
  - `delivery_queue_depth{channel,status}` — gauge, `status` ∈ {pending, in_flight, retry_scheduled}, sampled from committed rows by every worker process every 5 s (one `GROUP BY` statement); all six series are set (missing combinations = 0) after a successful sample and removed after a failed one, never served stale; absent before the first sample.
  - `llm_requests_total{task,outcome}` — counter per LLM provider request via `llm.Observe` (decorator of the selected client): `task` ∈ {segment_interpretation, content_generation, other}, `outcome` ∈ {ok, max_tokens, refused, unavailable, rejected, timeout, cancelled, aborted, error}.
  - `llm_request_duration_seconds{task}` — histogram (100 ms … 300 s) of the same requests.
  Counters/histograms are process-local and start at 0 (restart = counter reset); no value is derived from or reconstructed out of the database except the queue gauge. Not provided: Go runtime/process collectors, summaries, exemplars, OpenMetrics/protobuf.
- **worker**: `/healthz`, `/metrics` on its own port.
- Core metrics: `deliveries_total{channel,outcome}`, `provider_request_duration_seconds{channel}`, `provider_rate_limited_total{channel}`, `delivery_queue_depth{channel,status}`, `llm_requests_total{task,outcome}`, `llm_request_duration_seconds{task}`.
- Structured `slog` JSON logs with `campaign_id`, `approval_id`, `delivery_id`, `request_id`; phone numbers and emails masked; LLM calls logged with model, prompt version, tokens, latency, stop reason.

---

## 16. Configuration (complete)

| Variable | Default |
|---|---|
| `DATABASE_URL` | compose value (built from `POSTGRES_USER`/`POSTGRES_PASSWORD`/`POSTGRES_DB`, local-only default `composer`) |
| `COMPOSER_HTTP_PORT` (Compose only) | `8000` (frontend host port, bound to `127.0.0.1` only) |
| `HTTP_ADDR`, `WORKER_METRICS_ADDR` | `:8080`, `:9091` |
| `LLM_PROVIDER`, `ANTHROPIC_API_KEY`, `CLAUDE_*` | §7.3 |
| `PROVIDER_BASE_URL`, `PROVIDER_REQUEST_TIMEOUT` | `http://mockprovider:8090`, `5s` |
| `RATE_LIMIT_PER_SECOND`, `RATE_SAFETY_MARGIN`, `DISPATCH_MAX_SLOT_TO_REQUEST`, `DISPATCH_MAX_PRECHECK` | `100`, `50ms`, `20ms`, `15ms` |
| `SENDER_POOL_SIZE`, `CLAIM_BATCH_SIZE`, `LEASE_DURATION` | `20`, `50`, `60s` |
| `MAX_TRANSIENT_ATTEMPTS`, `MAX_RATE_LIMITED_ATTEMPTS`, `BACKOFF_BASE`, `BACKOFF_CAP`, `RETRY_AFTER_CAP` | `5`, `10`, `1s`, `60s`, `30s` |
| `APPROVAL_SWEEP_INTERVAL` (CP17A, worker) | `30s` (1 s – 10 min) |
| `APPROVAL_PENDING_TTL`, `APPROVAL_APPROVED_TTL` | `15m`, `24h` |
| `APPROVER_NAME` | `marketer@demo` (recorded as `approved_by`; no auth) |
| `MOCK_FAULT_5XX_RATE`, `MOCK_FAULT_429_RATE`, `MOCK_FAULT_TIMEOUT_RATE`, `MOCK_LATENCY_MS`, `MOCK_PERMANENT_FAIL_PATTERN` | `0.03`, `0.01`, `0.005`, `50–200`, numbers ending `0000` |
| `SEED_USERS`, `SEED_RANDOM_SEED` | `5000`, `42` |
| `WORKER_ID` (CP16; a random suffix is appended per process start), `WORKER_POLL_INTERVAL` (CP16) | `worker`, `500ms` |
| ~~`DELIVERY_RETRY_DELAY`~~ (CP16 interim fixed delay; removed in CP17A — setting it is a startup error) | — |
| `MOCK_PROVIDER_ADDR`, `MOCK_TIMEOUT_STALL`, `MOCK_RETRY_AFTER`, `MOCK_RANDOM_SEED` (CP16) | `:8090`, `30s`, `1s`, `1` |
| `MOCK_LEDGER_PATH` (required, absolute), `MOCK_ADMIN_RESET_ENABLED` (CP16) | Compose: `/var/lib/mockprovider/ledger.journal` on volume `mockledger`, `false` |
| `MOCK_RATE_LIMIT_PER_SECOND` (CP17A; mock's strict limiter, 0 disables) | `100` |

CP16 implements and validates `PROVIDER_*`, `SENDER_POOL_SIZE`, `CLAIM_BATCH_SIZE`, `LEASE_DURATION` (≥ 10 × request timeout), `MAX_TRANSIENT_ATTEMPTS`, `MAX_RATE_LIMITED_ATTEMPTS`, `RETRY_AFTER_CAP`, `WORKER_METRICS_ADDR` (serves `/healthz` only so far) and the `MOCK_*` fault settings (`MOCK_LATENCY_MS` is `N` or `MIN-MAX`; `MOCK_PERMANENT_FAIL_PATTERN` is a destination suffix, `-` disables). CP17A implements and validates `RATE_LIMIT_PER_SECOND` (1–1000), `RATE_SAFETY_MARGIN` (1 ms – 1 s), `DISPATCH_MAX_SLOT_TO_REQUEST`, `DISPATCH_MAX_PRECHECK` (ordering enforced), `BACKOFF_BASE` ≤ `BACKOFF_CAP`, `APPROVAL_SWEEP_INTERVAL` and `MOCK_RATE_LIMIT_PER_SECOND`. The dispatch-lock check interval/timeout (1 s / 1 s) and the heartbeat period (5 s) are code defaults.

Startup validates every value (e.g. `DISPATCH_MAX_PRECHECK < DISPATCH_MAX_SLOT_TO_REQUEST < RATE_SAFETY_MARGIN`, `LEASE_DURATION ≥ 10 × PROVIDER_REQUEST_TIMEOUT`, `LLM_PROVIDER=anthropic` requires `ANTHROPIC_API_KEY`).

---

## 17. Final consistency review

| # | Finding | Resolution |
|---|---|---|
| 1 | Round-2 idempotency key used `approval_id`; a bug producing rows under two approvals would not collide at the provider | Key is `campaign:user:channel` (§10.6) |
| 2 | Destination was "snapshot at enqueue" (v1) vs "current at dispatch" (v2) | NULL until first precheck, persisted before the request, immutable afterwards (§10.3) |
| 3 | `attempts` was incremented at claim, so lease recovery without a send burned retries | Incremented only when an attempt completes or is reaped with `dispatch_started_at` set |
| 4 | Precheck before the limiter wait would widen the consent window to the wait (up to a 429 pause) | Order is Acquire → Precheck → Do, with a bounded gap (§10.2) |
| 5 | DB latency or process delay between slot and request would add arrival jitter | Precheck budget (`DISPATCH_MAX_PRECHECK`) plus a final slot-to-request gate (`DISPATCH_MAX_SLOT_TO_REQUEST`) immediately before `Do`; included in the margin derivation (§10.2, §10.4) |
| 6 | Approval statuses `superseded`/`cancelled` vs v2 list | Unified as `revoked` + `revoke_reason` |
| 7 | No way to discard a pending approval without editing | `withdraw` endpoint |
| 8 | Cancellation was "B" but needed to define consistent transitions | Core (small: one endpoint, two statements); see ROADMAP scope |
| 9 | `PUT /segment` and `POST /segments/preview` only served the form editor (C) | Removed from core API; interpret replaces the rule; `GET /audience` refreshes |
| 10 | Channel change after content generation could leave content missing for a channel | Content validity is evaluated against selected channels; prepare blocked with field errors |
| 11 | Retry outcome after cancellation could leave rows `retry_scheduled` forever | Completion/reaper convert non-terminal outcomes to `cancelled` when campaign ≠ sending |
| 12 | Expired approval could be confirmed/consumed before the sweeper ran | Consuming CAS checks `expires_at > clock_timestamp()` after the lock; lazy `ExpireDue` in the API |
| 13 | Dashboard denominator vs prepare statistics | `approved_total` = approval stats, asserted equal to row counts |
| 14 | `uncertain` could be mistaken for a partition bucket | Defined as overlay attribute (§13) |
| 15 | Timestamps from three containers | DB time for persisted times; monotonic clocks only for rate limiting and arrival measurement |
| 16 | Consent revocation not testable without an endpoint | Integration tests update `channel_consents` directly; no consent API in scope |
| 17 | Expiry compared with `now()` (transaction start) could accept an approval that expired while the transaction waited for the campaign lock | Lock first, then compare with `clock_timestamp()`; expiry-defining timestamps also from `clock_timestamp()`; lock-wait integration test (§5.1, §5.3) |
| 18 | At-most-one delivery was stated as if it were unconditional | Conditional on declared provider idempotency capability and retention ≥ retry horizon; effective guarantee logged and in `/readyz` (§10.6, §10.9) |
| 19 | Slot-to-request gap was checked only after the precheck; a delay after that check was unbounded | Separate precheck budget plus a final gate immediately before `Do`; residual inside transport documented (§10.2) |
| 20 | `LLM_PROVIDER=auto` behaviour on Anthropic failures was undefined | Provider chosen once at startup; Anthropic failures return errors, never demo output (§7.3) |

---

## 18. Assumptions and known limitations

**Assumptions**
- Single marketer identity, no authentication; `approved_by` is a configured name.
- One worker replica; one API replica is sufficient (the API is stateless and could scale; the worker could not without a shared limiter).
- Local-network transport jitter ≤ 30 ms (validated by the rate test, not proven).
- "Last N days" is relative to evaluation time in UTC.
- The mock provider's idempotency ledger is a durable journal on a local volume (ADR-020 amendment): retained across restarts and recreation, lost only by an explicit reset or volume deletion; it assumes a local filesystem that honours fsync and flock. Real providers differ: a real adapter must declare its idempotency capability and retention, and the at-most-one property is claimed only when the retention covers the retry horizon `H` (≈ 12 min with defaults).
- Seed data respects the declared variable maximums (DB CHECK constraints).

**Known limitations**
- Not exactly-once: ambiguous attempts can end with `outcome_uncertain`. At-most-one delivery is conditional on provider idempotency (§10.6); worker downtime longer than the provider's key retention while a retry is pending voids it, and for the mock so does destroying its ledger (`down -v`, admin reset) while a retry is pending.
- The slot-to-request deadline is enforced up to the final gate before `Do`; stalls inside the HTTP transport, runtime or kernel are covered only by the safety margin under the local-network assumption.
- Consent revocation race window: one in-flight attempt (§10.10).
- Snapshot staleness up to 24 h: users who stopped matching the rule (e.g. purchased) still receive the message.
- In-process rate limiter: correct only with one *dispatching* process. CP17A enforces that with a PostgreSQL advisory lock (a second process stands by, ADR-025); it does not scale throughput horizontally. Every start or takeover waits ≈ 3 s before the first request; a holder whose database connection vanishes without a TCP close may keep the lock until PostgreSQL's keepalive drops the session (liveness only).
- Cancellation cannot recall a request already on the wire: up to one request per row in flight at the commit (reported as `in_flight`).
- FIFO across campaigns: a large campaign delays a small one.
- No quiet hours, frequency capping, localization beyond tr/en, A/B testing, scheduling, or real unsubscribe handling.
- Segment rules can only be changed by re-interpreting the brief (no form editor in core).
- No GSM-7/UCS-2 encoding or SMS segment-count warnings (B-tier, not built); the hard rule is the worst-case character count (ADR-013).
- The offer policy is a keyword check: it blocks model output but is only advisory for marketer-written text, and it cannot detect every unauthorized offer or claim; the marketer's review (and the Slice 3 approval re-check, §7.9) remains the control.
