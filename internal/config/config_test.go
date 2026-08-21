package config

import (
	"reflect"
	"testing"
)

// TestParseLinkedChatIDs covers LINKED_CHAT_IDS parsing (issue #90). Recipiency
// is safety-critical — a qualifying alert on a household-subscribed event fires
// one auto-buy per member — so a single malformed entry must fail the WHOLE
// parse (caller then treats the household as empty/off) rather than silently
// dropping a member or fanning auto-buys to an unintended chat.
func TestParseLinkedChatIDs(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		raw     string
		want    []int64
		wantErr bool
	}{
		{name: "empty is off", raw: "", want: nil},
		{name: "blank is off", raw: "   ", want: nil},
		{name: "single id", raw: "123", want: []int64{123}},
		{name: "list", raw: "1,2,3", want: []int64{1, 2, 3}},
		{name: "whitespace tolerated", raw: " 1 , 2 ,3 ", want: []int64{1, 2, 3}},
		{name: "negative ids (Telegram group chats)", raw: "-100123,456", want: []int64{-100123, 456}},
		{name: "duplicates collapse", raw: "1,2,1,2", want: []int64{1, 2}},
		{name: "garbage fails whole parse", raw: "1,abc,3", wantErr: true},
		{name: "empty entry fails", raw: "1,,3", wantErr: true},
		{name: "trailing comma fails", raw: "1,2,", wantErr: true},
		{name: "float fails", raw: "1.5", wantErr: true},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := parseLinkedChatIDs(tt.raw)
			if (err != nil) != tt.wantErr {
				t.Fatalf("parseLinkedChatIDs(%q) err = %v, wantErr %v", tt.raw, err, tt.wantErr)
			}
			if tt.wantErr {
				return
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("parseLinkedChatIDs(%q) = %v, want %v", tt.raw, got, tt.want)
			}
		})
	}
}

// TestLoadLinkedChatIDs proves the fail-safe wiring: a valid list populates
// config; garbage disables the feature (empty list) rather than erroring boot.
func TestLoadLinkedChatIDs(t *testing.T) {
	tests := []struct {
		name string
		val  string // "" = unset
		want []int64
	}{
		{name: "unset is off", val: "", want: nil},
		{name: "valid list loads", val: "111,222,333", want: []int64{111, 222, 333}},
		{name: "garbage disables (fail-safe)", val: "111,oops", want: nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			extra := map[string]string{}
			if tt.val != "" {
				extra["LINKED_CHAT_IDS"] = tt.val
			}
			cfg := loadWithRequiredEnv(t, extra)
			if !reflect.DeepEqual(cfg.Telegram.LinkedChatIDs, tt.want) {
				t.Errorf("LinkedChatIDs = %v, want %v", cfg.Telegram.LinkedChatIDs, tt.want)
			}
		})
	}
}

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
