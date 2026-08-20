-- 011_deep_entry_ladder.down.sql
-- Reverse 011_deep_entry_ladder.sql.
ALTER TABLE sltp_arms DROP COLUMN IF EXISTS ladder_rungs_fired;
ALTER TABLE sltp_arms DROP COLUMN IF EXISTS ladder_base_shares;
