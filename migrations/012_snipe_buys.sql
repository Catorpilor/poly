-- 012_snipe_buys.sql
-- Durable snipe-buy log (issue #84): close the restart-amnesia gap in the
-- Comeback Snipe. In-memory state (the per-recipient bought record, the
-- watcher's token-level bought latch that suppresses re-alerts, and the two
-- daily spend ledgers) is lost on restart — a token that already auto-bought can
-- re-enter its crash band after a reboot and auto-buy AGAIN, and the daily cap
-- resets to zero. This table is the single durable record that boot re-reads to
-- rebuild all three (internal/telegram.RestoreSnipeBuys), mirroring ADR 0008's
-- LiveWatchStore write-through pattern.
--
-- One row per ACCEPTED buy at every buy tier: in-band auto $10, manual tap
-- $10/$25, boxed rung $5 (all pool='main'), and Deep Crash fire $5
-- (pool='deep'). The pool column keeps the two spend budgets ($50 main / $20
-- deep) separable on restore. Amounts are the reserved stake so summing a pool's
-- current-UTC-day rows reconstructs that ledger's spend exactly.
--
-- Rows are AUDIT-KEPT (no pruning job, ~10 rows/day — the tap trail is useful);
-- boot restore scans a 24h window, so the index is on bought_at.
CREATE TABLE IF NOT EXISTS snipe_buys (
    id BIGSERIAL PRIMARY KEY,
    chat_id BIGINT NOT NULL,
    token_id TEXT NOT NULL,
    amount_usd NUMERIC(20,6) NOT NULL,
    pool TEXT NOT NULL CHECK (pool IN ('main', 'deep')),
    bought_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- Restore scans "rows since now-24h", so index the scan key.
CREATE INDEX IF NOT EXISTS idx_snipe_buys_bought_at ON snipe_buys (bought_at);
