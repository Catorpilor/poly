package config

import "testing"

// loadWithRequiredEnv sets the minimum required env vars and runs Load.
func loadWithRequiredEnv(t *testing.T, extra map[string]string) *Config {
	t.Helper()
	t.Setenv("TELEGRAM_BOT_TOKEN", "test-token")
	t.Setenv("DATABASE_URL", "postgresql://u:p@localhost:5432/db")
	t.Setenv("POLYGON_RPC_URL", "https://rpc.example.test")
	t.Setenv("ENCRYPTION_KEY", "0123456789abcdef0123456789abcdef")
	for k, v := range extra {
		t.Setenv(k, v)
	}
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	return cfg
}

// TestTelegramPollTimeout covers TELEGRAM_POLL_TIMEOUT_SECONDS: the getUpdates
// long-poll hold. Proxies that kill idle connections make long holds fail with
// "context deadline exceeded", so deployments behind them shorten this;
// out-of-range values fall back to the Telegram-safe default of 60.
func TestTelegramPollTimeout(t *testing.T) {
	tests := []struct {
		name string
		val  string // "" = unset
		want int
	}{
		{"default 60 when unset", "", 60},
		{"honors short override", "15", 15},
		{"honors max 90", "90", 90},
		{"zero falls back to default", "0", 60},
		{"negative falls back to default", "-5", 60},
		{"above 90 falls back to default", "600", 60},
		{"garbage falls back to default", "abc", 60},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			extra := map[string]string{}
			if tt.val != "" {
				extra["TELEGRAM_POLL_TIMEOUT_SECONDS"] = tt.val
			}
			cfg := loadWithRequiredEnv(t, extra)
			if cfg.Telegram.PollTimeoutSeconds != tt.want {
				t.Errorf("PollTimeoutSeconds = %d, want %d", cfg.Telegram.PollTimeoutSeconds, tt.want)
			}
		})
	}
}
