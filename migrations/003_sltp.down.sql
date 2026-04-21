DROP TRIGGER IF EXISTS update_sltp_arms_updated_at ON sltp_arms;
DROP INDEX IF EXISTS idx_sltp_arms_any_armed;
DROP INDEX IF EXISTS idx_sltp_arms_token_id;
DROP INDEX IF EXISTS idx_sltp_arms_telegram_id;
DROP TABLE IF EXISTS sltp_arms;
