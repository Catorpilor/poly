-- Create users table for storing user wallet information
CREATE TABLE IF NOT EXISTS users (
    telegram_id BIGINT PRIMARY KEY,
    username VARCHAR(255),
    eoa_address VARCHAR(42) UNIQUE,
    proxy_address VARCHAR(42) UNIQUE,
    encrypted_key TEXT NOT NULL,  -- AES-256-GCM encrypted private key
    settings JSONB DEFAULT '{}',
    is_active BOOLEAN DEFAULT true,
    created_at TIMESTAMP WITH TIME ZONE DEFAULT NOW(),
    updated_at TIMESTAMP WITH TIME ZONE DEFAULT NOW()
);

-- Create markets cache table
CREATE TABLE IF NOT EXISTS markets (
    market_id VARCHAR(66) PRIMARY KEY,
    quick_access_uuid UUID UNIQUE DEFAULT gen_random_uuid(),
    title TEXT NOT NULL,
    condition_id VARCHAR(66) NOT NULL,
    token_ids JSONB NOT NULL,  -- {yes: "0x...", no: "0x..."}
    cached_data JSONB DEFAULT '{}',  -- Store additional market metadata
    is_active BOOLEAN DEFAULT true,
    ends_at TIMESTAMP WITH TIME ZONE,
    created_at TIMESTAMP WITH TIME ZONE DEFAULT NOW(),
    updated_at TIMESTAMP WITH TIME ZONE DEFAULT NOW()
);

-- Create orders tracking table
CREATE TABLE IF NOT EXISTS orders (
    order_id VARCHAR(66) PRIMARY KEY,
    telegram_id BIGINT NOT NULL REFERENCES users(telegram_id) ON DELETE CASCADE,
    market_id VARCHAR(66) NOT NULL REFERENCES markets(market_id),
    side VARCHAR(4) NOT NULL CHECK (side IN ('BUY', 'SELL')),
    outcome VARCHAR(3) NOT NULL CHECK (outcome IN ('YES', 'NO')),
    size DECIMAL(20,6) NOT NULL CHECK (size > 0),
    price DECIMAL(10,6) NOT NULL CHECK (price >= 0 AND price <= 1),
    status VARCHAR(20) NOT NULL DEFAULT 'PENDING',
    filled DECIMAL(20,6) DEFAULT 0 CHECK (filled >= 0),
    tx_hash VARCHAR(66),
    error_message TEXT,
    created_at TIMESTAMP WITH TIME ZONE DEFAULT NOW(),
    updated_at TIMESTAMP WITH TIME ZONE DEFAULT NOW(),
    executed_at TIMESTAMP WITH TIME ZONE
);

-- Create positions cache table
CREATE TABLE IF NOT EXISTS positions (
    telegram_id BIGINT NOT NULL REFERENCES users(telegram_id) ON DELETE CASCADE,
    market_id VARCHAR(66) NOT NULL REFERENCES markets(market_id),
    position_id VARCHAR(66) NOT NULL,
    outcome VARCHAR(3) NOT NULL CHECK (outcome IN ('YES', 'NO')),
    shares DECIMAL(20,6) NOT NULL CHECK (shares >= 0),
    avg_price DECIMAL(10,6) CHECK (avg_price >= 0 AND avg_price <= 1),
    last_price DECIMAL(10,6) CHECK (last_price >= 0 AND last_price <= 1),
    unrealized_pnl DECIMAL(20,6),
    created_at TIMESTAMP WITH TIME ZONE DEFAULT NOW(),
    updated_at TIMESTAMP WITH TIME ZONE DEFAULT NOW(),
    PRIMARY KEY (telegram_id, position_id)
);

-- Create sessions table for user session management
CREATE TABLE IF NOT EXISTS sessions (
    session_id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    telegram_id BIGINT NOT NULL REFERENCES users(telegram_id) ON DELETE CASCADE,
    is_active BOOLEAN DEFAULT true,
    last_activity TIMESTAMP WITH TIME ZONE DEFAULT NOW(),
    expires_at TIMESTAMP WITH TIME ZONE NOT NULL,
    created_at TIMESTAMP WITH TIME ZONE DEFAULT NOW()
);

-- Create audit_logs table for tracking all important operations
CREATE TABLE IF NOT EXISTS audit_logs (
    id SERIAL PRIMARY KEY,
    telegram_id BIGINT REFERENCES users(telegram_id) ON DELETE SET NULL,
    action VARCHAR(100) NOT NULL,
    details JSONB DEFAULT '{}',
    ip_address INET,
    user_agent TEXT,
    created_at TIMESTAMP WITH TIME ZONE DEFAULT NOW()
);

-- Create price_alerts table
CREATE TABLE IF NOT EXISTS price_alerts (
    id SERIAL PRIMARY KEY,
    telegram_id BIGINT NOT NULL REFERENCES users(telegram_id) ON DELETE CASCADE,
    market_id VARCHAR(66) NOT NULL REFERENCES markets(market_id),
    outcome VARCHAR(3) NOT NULL CHECK (outcome IN ('YES', 'NO')),
    alert_type VARCHAR(10) NOT NULL CHECK (alert_type IN ('ABOVE', 'BELOW')),
    price_threshold DECIMAL(10,6) NOT NULL CHECK (price_threshold >= 0 AND price_threshold <= 1),
    is_active BOOLEAN DEFAULT true,
    triggered_at TIMESTAMP WITH TIME ZONE,
    created_at TIMESTAMP WITH TIME ZONE DEFAULT NOW()
);

-- Create indexes for better query performance
CREATE INDEX idx_users_eoa_address ON users(eoa_address);
CREATE INDEX idx_users_proxy_address ON users(proxy_address);
CREATE INDEX idx_users_created_at ON users(created_at);

CREATE INDEX idx_markets_quick_access_uuid ON markets(quick_access_uuid);
CREATE INDEX idx_markets_condition_id ON markets(condition_id);
CREATE INDEX idx_markets_is_active ON markets(is_active);

CREATE INDEX idx_orders_telegram_id ON orders(telegram_id);
CREATE INDEX idx_orders_market_id ON orders(market_id);
CREATE INDEX idx_orders_status ON orders(status);
CREATE INDEX idx_orders_created_at ON orders(created_at);
CREATE INDEX idx_orders_telegram_market ON orders(telegram_id, market_id);

CREATE INDEX idx_positions_telegram_id ON positions(telegram_id);
CREATE INDEX idx_positions_market_id ON positions(market_id);
CREATE INDEX idx_positions_updated_at ON positions(updated_at);

CREATE INDEX idx_sessions_telegram_id ON sessions(telegram_id);
CREATE INDEX idx_sessions_expires_at ON sessions(expires_at);
CREATE INDEX idx_sessions_is_active ON sessions(is_active);

CREATE INDEX idx_audit_logs_telegram_id ON audit_logs(telegram_id);
CREATE INDEX idx_audit_logs_action ON audit_logs(action);
CREATE INDEX idx_audit_logs_created_at ON audit_logs(created_at);

CREATE INDEX idx_price_alerts_telegram_id ON price_alerts(telegram_id);
CREATE INDEX idx_price_alerts_market_id ON price_alerts(market_id);
CREATE INDEX idx_price_alerts_is_active ON price_alerts(is_active);

-- Create update trigger for updated_at columns
CREATE OR REPLACE FUNCTION update_updated_at_column()
RETURNS TRIGGER AS $$
BEGIN
    NEW.updated_at = NOW();
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

-- Apply update trigger to relevant tables
CREATE TRIGGER update_users_updated_at BEFORE UPDATE ON users
    FOR EACH ROW EXECUTE FUNCTION update_updated_at_column();

CREATE TRIGGER update_markets_updated_at BEFORE UPDATE ON markets
    FOR EACH ROW EXECUTE FUNCTION update_updated_at_column();

CREATE TRIGGER update_orders_updated_at BEFORE UPDATE ON orders
    FOR EACH ROW EXECUTE FUNCTION update_updated_at_column();

CREATE TRIGGER update_positions_updated_at BEFORE UPDATE ON positions
    FOR EACH ROW EXECUTE FUNCTION update_updated_at_column();