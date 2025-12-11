-- Drop triggers
DROP TRIGGER IF EXISTS update_positions_updated_at ON positions;
DROP TRIGGER IF EXISTS update_orders_updated_at ON orders;
DROP TRIGGER IF EXISTS update_markets_updated_at ON markets;
DROP TRIGGER IF EXISTS update_users_updated_at ON users;

-- Drop function
DROP FUNCTION IF EXISTS update_updated_at_column();

-- Drop indexes
DROP INDEX IF EXISTS idx_price_alerts_is_active;
DROP INDEX IF EXISTS idx_price_alerts_market_id;
DROP INDEX IF EXISTS idx_price_alerts_telegram_id;

DROP INDEX IF EXISTS idx_audit_logs_created_at;
DROP INDEX IF EXISTS idx_audit_logs_action;
DROP INDEX IF EXISTS idx_audit_logs_telegram_id;

DROP INDEX IF EXISTS idx_sessions_is_active;
DROP INDEX IF EXISTS idx_sessions_expires_at;
DROP INDEX IF EXISTS idx_sessions_telegram_id;

DROP INDEX IF EXISTS idx_positions_updated_at;
DROP INDEX IF EXISTS idx_positions_market_id;
DROP INDEX IF EXISTS idx_positions_telegram_id;

DROP INDEX IF EXISTS idx_orders_telegram_market;
DROP INDEX IF EXISTS idx_orders_created_at;
DROP INDEX IF EXISTS idx_orders_status;
DROP INDEX IF EXISTS idx_orders_market_id;
DROP INDEX IF EXISTS idx_orders_telegram_id;

DROP INDEX IF EXISTS idx_markets_is_active;
DROP INDEX IF EXISTS idx_markets_condition_id;
DROP INDEX IF EXISTS idx_markets_quick_access_uuid;

DROP INDEX IF EXISTS idx_users_created_at;
DROP INDEX IF EXISTS idx_users_proxy_address;
DROP INDEX IF EXISTS idx_users_eoa_address;

-- Drop tables in reverse order of dependencies
DROP TABLE IF EXISTS price_alerts;
DROP TABLE IF EXISTS audit_logs;
DROP TABLE IF EXISTS sessions;
DROP TABLE IF EXISTS positions;
DROP TABLE IF EXISTS orders;
DROP TABLE IF EXISTS markets;
DROP TABLE IF EXISTS users;