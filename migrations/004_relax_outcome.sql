-- Relax outcome columns to accept multi-outcome market display names.
--
-- Polymarket markets are binary at the contract level (two CTF tokens per
-- condition), but the *display* outcome is the user-facing name. For
-- prediction questions that's "YES"/"NO", but for sports/esports/elections
-- it's a team or candidate name like "WEIBO GAMING", "KNICKS", or
-- "DN SOOPERS". The original VARCHAR(3) + CHECK (outcome IN ('YES','NO'))
-- crashed inserts for those markets with:
--   ERROR: value too long for type character varying(3) (SQLSTATE 22001)
--
-- token_id is the canonical key for orders, positions, alerts, and SL/TP
-- arms — outcome is display metadata only. Validation now happens in Go
-- (must be non-empty) so the DB only enforces width.

ALTER TABLE orders DROP CONSTRAINT IF EXISTS orders_outcome_check;
ALTER TABLE orders ALTER COLUMN outcome TYPE VARCHAR(64);

ALTER TABLE positions DROP CONSTRAINT IF EXISTS positions_outcome_check;
ALTER TABLE positions ALTER COLUMN outcome TYPE VARCHAR(64);

ALTER TABLE price_alerts DROP CONSTRAINT IF EXISTS price_alerts_outcome_check;
ALTER TABLE price_alerts ALTER COLUMN outcome TYPE VARCHAR(64);

ALTER TABLE sltp_arms DROP CONSTRAINT IF EXISTS sltp_arms_outcome_check;
ALTER TABLE sltp_arms ALTER COLUMN outcome TYPE VARCHAR(64);
