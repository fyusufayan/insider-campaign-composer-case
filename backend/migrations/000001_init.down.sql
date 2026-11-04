-- 000001_init down: reverses 000001_init.up.sql in dependency order.
-- No CASCADE: if anything outside this migration depends on these objects,
-- the down migration fails instead of silently dropping it.

BEGIN;

-- Break the campaigns <-> campaign_contents / campaign_approvals cycles first.
ALTER TABLE campaigns DROP CONSTRAINT campaigns_active_approval_id_fkey;
ALTER TABLE campaigns DROP CONSTRAINT campaigns_current_content_id_fkey;

DROP TABLE worker_heartbeats;
DROP TABLE delivery_attempts;
DROP TABLE campaign_deliveries;
DROP TABLE campaign_approvals;
DROP TABLE campaign_contents;
DROP TABLE campaigns;

DROP TABLE purchases;
DROP TABLE cart_events;
DROP TABLE channel_consents;
DROP TABLE users;

DROP TYPE channel;

COMMIT;
