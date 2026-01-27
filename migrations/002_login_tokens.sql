-- Login tokens for web authentication via Telegram
CREATE TABLE login_tokens (
    token UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    status VARCHAR(20) NOT NULL DEFAULT 'pending'
        CHECK (status IN ('pending', 'authenticated', 'used', 'expired')),
    telegram_id BIGINT REFERENCES users(telegram_id) ON DELETE SET NULL,
    wallet_address VARCHAR(42),
    proxy_address VARCHAR(42),
    created_at TIMESTAMPTZ DEFAULT NOW(),
    authenticated_at TIMESTAMPTZ,
    expires_at TIMESTAMPTZ NOT NULL,
    used_at TIMESTAMPTZ
);

-- Index for efficient token lookup by status
CREATE INDEX idx_login_tokens_status ON login_tokens(status);

-- Index for cleaning up expired tokens
CREATE INDEX idx_login_tokens_expires_at ON login_tokens(expires_at);

-- Comment for documentation
COMMENT ON TABLE login_tokens IS 'Tokens for web authentication via Telegram bot';
