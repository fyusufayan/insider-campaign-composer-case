-- 000002_channel_pauses: durable per-channel provider pause (CP17, ADR-025).
-- A provider 429 pauses dispatch of ITS channel until paused_until (database
-- time = now() + Retry-After). The worker keeps the authoritative pause in
-- its in-process limiter (§10.4); this row makes the pause survive a worker
-- restart or a dispatch takeover, and lets claim skip a paused channel.
-- One row per channel at most; paused_until only ever moves forward.

BEGIN;

CREATE TABLE channel_pauses (
  channel      channel     PRIMARY KEY,
  paused_until TIMESTAMPTZ NOT NULL,
  updated_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);

COMMIT;
