package telegram

import (
	"testing"
	"time"
)

// TestPollHTTPTimeout: the API client's HTTP timeout must track the
// configured long-poll hold plus a grace for transit — a client timeout
// sized for 60s polls makes every proxy-killed 15s poll stall 75s before
// the retry loop can act (observed live 2026-08-16).
func TestPollHTTPTimeout(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		poll int
		want time.Duration
	}{
		{"default 60s poll gets 70s client timeout", 60, 70 * time.Second},
		{"short 15s poll fails fast", 15, 25 * time.Second},
		{"minimum poll", 1, 11 * time.Second},
		{"max poll", 90, 100 * time.Second},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := pollHTTPTimeout(tt.poll); got != tt.want {
				t.Errorf("pollHTTPTimeout(%d) = %v, want %v", tt.poll, got, tt.want)
			}
		})
	}
}
