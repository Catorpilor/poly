-- 008_sl_trailing_stop.sql
-- Breakeven-trailing stop: per-arm high-water mark of the best bid.
-- Seeded to avg_price on arm/re-arm (dormant); ratcheted monotonically by the
-- SL/TP monitor. The stop activates only once high_water_mark reaches
-- avg_price * activation multiplier (see internal/database/models.go).

ALTER TABLE sltp_arms
    ADD COLUMN IF NOT EXISTS high_water_mark DECIMAL(10,6) NOT NULL DEFAULT 0
    CHECK (high_water_mark >= 0 AND high_water_mark <= 1);

-- Backfill: existing armed rows restart as DORMANT trailing stops anchored
-- at their entry price.
UPDATE sltp_arms SET high_water_mark = avg_price WHERE high_water_mark < avg_price;
