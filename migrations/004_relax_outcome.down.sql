-- Reverse 004_relax_outcome.sql.
--
-- WARNING: this will FAIL if any row already contains an outcome value
-- longer than 3 chars or not in ('YES','NO') — that's intentional. Don't
-- run this in production without first auditing and rewriting any
-- multi-outcome rows; the original schema cannot represent them.

ALTER TABLE sltp_arms ALTER COLUMN outcome TYPE VARCHAR(3);
ALTER TABLE sltp_arms ADD CONSTRAINT sltp_arms_outcome_check CHECK (outcome IN ('YES', 'NO'));

ALTER TABLE price_alerts ALTER COLUMN outcome TYPE VARCHAR(3);
ALTER TABLE price_alerts ADD CONSTRAINT price_alerts_outcome_check CHECK (outcome IN ('YES', 'NO'));

ALTER TABLE positions ALTER COLUMN outcome TYPE VARCHAR(3);
ALTER TABLE positions ADD CONSTRAINT positions_outcome_check CHECK (outcome IN ('YES', 'NO'));

ALTER TABLE orders ALTER COLUMN outcome TYPE VARCHAR(3);
ALTER TABLE orders ADD CONSTRAINT orders_outcome_check CHECK (outcome IN ('YES', 'NO'));
