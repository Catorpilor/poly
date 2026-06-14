-- Add account_type to users: classifies the trading account architecture so the
-- order signer can select the right EIP-712 signature type:
--   legacy_proxy   -> POLY_PROXY (1)
--   safe           -> POLY_GNOSIS_SAFE (2)
--   deposit_wallet -> POLY_1271 / ERC-7739 (3)
-- Existing rows default to legacy_proxy (the historical behavior).
ALTER TABLE users
    ADD COLUMN IF NOT EXISTS account_type VARCHAR(20) NOT NULL DEFAULT 'legacy_proxy';
