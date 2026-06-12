-- SL/TP (Stop-Loss / Take-Profit) auto-sell arms
-- One row per (telegram_id, token_id). Snapshots avg_price + shares at arm time
-- so threshold evaluation is deterministic and independent of Data API drift.
CREATE TABLE IF NOT EXISTS sltp_arms (
    id SERIAL PRIMARY KEY,
    telegram_id BIGINT NOT NULL REFERENCES users(telegram_id) ON DELETE CASCADE,
    token_id VARCHAR(80) NOT NULL,
    condition_id VARCHAR(66) NOT NULL,
    market_id VARCHAR(66),
    outcome VARCHAR(3) NOT NULL CHECK (outcome IN ('YES', 'NO')),
    avg_price DECIMAL(10,6) NOT NULL CHECK (avg_price > 0 AND avg_price <= 1),
    shares_at_arm DECIMAL(20,6) NOT NULL CHECK (shares_at_arm > 0),
    tp_armed BOOLEAN NOT NULL DEFAULT TRUE,
    sl_armed BOOLEAN NOT NULL DEFAULT TRUE,
    neg_risk BOOLEAN NOT NULL DEFAULT FALSE,
    created_at TIMESTAMP WITH TIME ZONE DEFAULT NOW(),
    updated_at TIMESTAMP WITH TIME ZONE DEFAULT NOW(),
    UNIQUE (telegram_id, token_id)
);

CREATE INDEX idx_sltp_arms_telegram_id ON sltp_arms(telegram_id);
CREATE INDEX idx_sltp_arms_token_id ON sltp_arms(token_id);
CREATE INDEX idx_sltp_arms_any_armed ON sltp_arms(token_id) WHERE tp_armed OR sl_armed;

CREATE TRIGGER update_sltp_arms_updated_at BEFORE UPDATE ON sltp_arms
    FOR EACH ROW EXECUTE FUNCTION update_updated_at_column();
