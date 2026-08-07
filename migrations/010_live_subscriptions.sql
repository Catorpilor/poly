-- 010_live_subscriptions.sql
-- Durable Live Watches (ADR 0008). A Live Watch is user intent, not a soft
-- rail: it drives snipe watching, alerts, and (when tape is set) the batched
-- Telegram trade feed, and it must survive a restart. Boot re-registers and
-- re-resolves every row (internal/live.RestoreWatches). The registry in
-- internal/live stays the runtime view; this table is the durable record.
-- Snipe caps and episode latches stay in-memory (bounds on harm, self-healing)
-- — the soft-rail doctrine is narrowed here, not abandoned.
--
-- One row per (chat_id, event_slug). chat_id is the owning Telegram identity;
-- web-created watches (Phase 3) are the same object and funnel through the same
-- writer. The PK dedupes both write paths.
CREATE TABLE IF NOT EXISTS live_subscriptions (
    chat_id BIGINT NOT NULL,
    event_slug TEXT NOT NULL,
    tape BOOLEAN NOT NULL DEFAULT FALSE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (chat_id, event_slug)
);
