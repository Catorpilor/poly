-- 009_sltp_tick_size.sql
-- Per-arm market tick size, captured at arm time from the CLOB. The TP
-- trigger is floored to this grid so it lands on a price the best bid can
-- actually print (issue #25: entry 0.2355 doubled to 0.471 on a 0.01-tick
-- book where only 0.47 or 0.48 exist — the effective threshold was silently
-- 0.48 and the 2x exit was missed by one tick). See TPTriggerPrice in
-- internal/database/models.go.

ALTER TABLE sltp_arms
    ADD COLUMN IF NOT EXISTS tick_size DECIMAL(6,4) NOT NULL DEFAULT 0.01
    CHECK (tick_size > 0 AND tick_size <= 0.1);
