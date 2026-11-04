-- 000001_init: full schema of docs/ARCHITECTURE.md §4.
-- Index and constraint names that §4 leaves implicit are spelled out with
-- PostgreSQL's default naming so the down migration can refer to them.

BEGIN;

-- ===== Customer data (seeded; read-only for the application except consents in tests) =====
CREATE TYPE channel AS ENUM ('sms', 'email');

CREATE TABLE users (
  id              BIGSERIAL PRIMARY KEY,
  first_name      TEXT NOT NULL CHECK (char_length(first_name) BETWEEN 1 AND 20),
  last_name       TEXT NOT NULL CHECK (char_length(last_name)  BETWEEN 1 AND 25),
  email           TEXT UNIQUE,                      -- NULL = no email contact
  phone           TEXT UNIQUE,                      -- E.164; NULL = no SMS contact
  city            TEXT NOT NULL,                    -- display form, e.g. 'İstanbul'
  city_normalized TEXT NOT NULL,                    -- Turkish-aware folded key, e.g. 'istanbul'
  country         CHAR(2) NOT NULL DEFAULT 'TR',
  gender          TEXT NOT NULL CHECK (gender IN ('female', 'male', 'other', 'unknown')),
  birth_date      DATE,
  language        TEXT NOT NULL DEFAULT 'tr',
  signup_at       TIMESTAMPTZ NOT NULL,
  last_active_at  TIMESTAMPTZ
);
CREATE INDEX users_city_normalized_idx ON users (city_normalized);

CREATE TABLE channel_consents (
  user_id    BIGINT  NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  channel    channel NOT NULL,
  status     TEXT    NOT NULL CHECK (status IN ('opted_in', 'opted_out')),
  source     TEXT    NOT NULL,
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  PRIMARY KEY (user_id, channel)                    -- missing row => not consented
);

CREATE TABLE cart_events (
  id          BIGSERIAL PRIMARY KEY,
  user_id     BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  product_id  TEXT NOT NULL,
  category    TEXT NOT NULL,
  quantity    INT NOT NULL CHECK (quantity > 0),
  unit_price  NUMERIC(12,2) NOT NULL,
  occurred_at TIMESTAMPTZ NOT NULL
);
CREATE INDEX cart_events_user_id_occurred_at_idx ON cart_events (user_id, occurred_at DESC);

CREATE TABLE purchases (
  id           BIGSERIAL PRIMARY KEY,
  user_id      BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  order_total  NUMERIC(12,2) NOT NULL,
  category     TEXT,
  purchased_at TIMESTAMPTZ NOT NULL
);
CREATE INDEX purchases_user_id_purchased_at_idx ON purchases (user_id, purchased_at DESC);

-- ===== Campaign authoring =====
CREATE TABLE campaigns (
  id                 UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  name               TEXT NOT NULL CHECK (char_length(name) BETWEEN 1 AND 120),
  brief              TEXT NOT NULL CHECK (char_length(brief) BETWEEN 10 AND 2000),
  status             TEXT NOT NULL CHECK (status IN ('draft', 'segmented', 'content_ready', 'pending_approval',
                                                     'approved', 'sending', 'completed', 'completed_with_failures',
                                                     'cancelled')),
  channels           channel[] NOT NULL DEFAULT '{}',
  segment_rule       JSONB,                         -- validated + normalized DSL only
  segment_version    INT NOT NULL DEFAULT 0,        -- +1 on every rule change
  interpretation     JSONB,                         -- {summary, assumptions[], unsupported[], model, prompt_version}
  content_brief      JSONB,                         -- {goal, tone, length, offer}
  current_content_id BIGINT,                        -- FK added below
  active_approval_id UUID,                          -- FK added below; set while an approval is pending/approved/consumed
  version            INT NOT NULL DEFAULT 1,        -- optimistic concurrency for marketer edits
  send_started_at    TIMESTAMPTZ,
  completed_at       TIMESTAMPTZ,
  cancelled_at       TIMESTAMPTZ,
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
  origin         TEXT NOT NULL CHECK (origin IN ('ai', 'edited')),
  model          TEXT,                              -- set when origin = 'ai'
  prompt_version TEXT,
  created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
  UNIQUE (campaign_id, version)
);
ALTER TABLE campaigns
  ADD CONSTRAINT campaigns_current_content_id_fkey
  FOREIGN KEY (current_content_id) REFERENCES campaign_contents(id);

-- ===== Approval (binds segment version + content version + channels + snapshot) =====
CREATE TABLE campaign_approvals (
  id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  campaign_id     UUID NOT NULL REFERENCES campaigns(id),
  status          TEXT NOT NULL CHECK (status IN ('pending', 'approved', 'consumed', 'revoked', 'expired')),
  revoke_reason   TEXT CHECK (revoke_reason IN ('superseded', 'edited', 'withdrawn', 'campaign_cancelled')),
  segment_version INT    NOT NULL,
  segment_rule    JSONB  NOT NULL,                  -- frozen copy
  content_id      BIGINT NOT NULL REFERENCES campaign_contents(id),
  channels        channel[] NOT NULL CHECK (cardinality(channels) > 0),
  audience_stats  JSONB  NOT NULL,                  -- exact partition shown to the marketer (§6.5)
  snapshot_count  INT    NOT NULL CHECK (snapshot_count > 0),
  snapshot_digest TEXT   NOT NULL,                  -- sha256 over sorted (user_id, channel)
  prepared_at     TIMESTAMPTZ NOT NULL,             -- clock_timestamp() after the campaign lock (no DEFAULT now())
  expires_at      TIMESTAMPTZ NOT NULL,             -- pending: prepared_at+15m; approved: approved_at+24h
  approved_at     TIMESTAMPTZ,
  approved_by     TEXT,
  consumed_at     TIMESTAMPTZ,
  closed_at       TIMESTAMPTZ,                      -- set on revoked/expired
  CHECK ((status = 'revoked') = (revoke_reason IS NOT NULL)),
  CHECK (status NOT IN ('approved', 'consumed') OR (approved_at IS NOT NULL AND approved_by IS NOT NULL)),
  CHECK (status <> 'consumed' OR consumed_at IS NOT NULL)
);
CREATE UNIQUE INDEX approvals_one_live     ON campaign_approvals (campaign_id) WHERE status IN ('pending', 'approved');
CREATE UNIQUE INDEX approvals_one_consumed ON campaign_approvals (campaign_id) WHERE status = 'consumed';
CREATE INDEX approvals_expiry ON campaign_approvals (expires_at) WHERE status IN ('pending', 'approved');
ALTER TABLE campaigns
  ADD CONSTRAINT campaigns_active_approval_id_fkey
  FOREIGN KEY (active_approval_id) REFERENCES campaign_approvals(id);

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
                          ('held', 'pending', 'in_flight', 'retry_scheduled', 'sent', 'failed',
                           'skipped_consent_revoked', 'skipped_no_contact', 'cancelled')),
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
  WHERE status IN ('pending', 'retry_scheduled');
CREATE INDEX deliveries_lease ON campaign_deliveries (lease_until) WHERE status = 'in_flight';
CREATE INDEX deliveries_stats ON campaign_deliveries (campaign_id, channel, status);

CREATE TABLE delivery_attempts (
  id             BIGSERIAL PRIMARY KEY,
  delivery_id    BIGINT NOT NULL REFERENCES campaign_deliveries(id),
  attempt_no     INT NOT NULL,                      -- 1-based across all outcomes, incl. 429
  destination    TEXT NOT NULL,
  outcome        TEXT NOT NULL CHECK (outcome IN ('accepted', 'replayed', 'rate_limited', 'transient_error',
                                                  'timeout', 'permanent_error', 'unknown')),
  http_status    INT,
  retry_after_ms INT,
  error          TEXT,
  started_at     TIMESTAMPTZ NOT NULL,              -- = dispatch_started_at
  finished_at    TIMESTAMPTZ NOT NULL,
  UNIQUE (delivery_id, attempt_no)
);

CREATE TABLE worker_heartbeats (
  worker_id    TEXT PRIMARY KEY,
  last_seen_at TIMESTAMPTZ NOT NULL
);

COMMIT;
