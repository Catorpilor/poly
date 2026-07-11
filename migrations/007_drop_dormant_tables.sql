-- Drop the dormant tables (ADR 0002: Polymarket APIs are the source of
-- truth; no local trading state).
--
-- These six tables were designed for local order tracking, position/market
-- caching, sessions, audit logs, and price alerts, but no Go code ever read
-- or wrote them. Orders, positions, and market data are fetched live from
-- the Polymarket CLOB/Gamma/Data APIs. Local persistence is limited to
-- identity (users, login_tokens) and standing instructions (sltp_arms).
--
-- Dependents first (FKs to users and markets), then markets.
-- Indexes and triggers drop with their tables. The shared
-- update_updated_at_column() function stays — users and sltp_arms use it.

DROP TABLE IF EXISTS price_alerts;
DROP TABLE IF EXISTS orders;
DROP TABLE IF EXISTS positions;
DROP TABLE IF EXISTS sessions;
DROP TABLE IF EXISTS audit_logs;
DROP TABLE IF EXISTS markets;
