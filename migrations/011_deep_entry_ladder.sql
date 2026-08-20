-- 011_deep_entry_ladder.sql
-- Deep-entry exit ladder (issue #81). An arm whose entry ≤ $0.05 sells
-- 25% @ 2×, 20% @ 3×, 15% @ 4×, 15% @ 5× of a COMMON BASE frozen at the first
-- rung fire; the remaining ~25% rides to the $0.95 ceiling. Ratified from the
-- 2026-08-20 counterfactual study (class A, n=30, 0 winners: laddering
-- monotonically better, P4 −$84.5 vs current −$133.0).
--
-- These two columns persist the ladder's progress so a restart resumes mid-ladder:
--   ladder_rungs_fired  — count of rungs already sold (0..4), a monotonic prefix.
--   ladder_base_shares  — the whole-position size frozen at the FIRST rung fire.
--
-- Ladder MEMBERSHIP is NOT stored: it is derived from avg_price at load time
-- (SLTPArm.IsDeepEntry). The rule is purely by entry, so existing qualifying
-- arms adopt the ladder with no backfill, and every non-qualifying arm keeps
-- ladder_rungs_fired = 0 forever (never enters the ladder path) — byte-identical
-- to pre-#81 behavior. See internal/database/models.go (DeepEntryLadder).
ALTER TABLE sltp_arms
    ADD COLUMN IF NOT EXISTS ladder_rungs_fired INTEGER NOT NULL DEFAULT 0
    CHECK (ladder_rungs_fired >= 0 AND ladder_rungs_fired <= 4);

ALTER TABLE sltp_arms
    ADD COLUMN IF NOT EXISTS ladder_base_shares DECIMAL(20,6) NOT NULL DEFAULT 0
    CHECK (ladder_base_shares >= 0);
