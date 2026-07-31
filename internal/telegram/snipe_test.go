package telegram

import (
	"strings"
	"testing"
	"time"

	"github.com/Catorpilor/poly/internal/live"
)

func TestSnipeAlertText(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		question    string
		outcome     string
		sessionHigh float64
		ask         float64
		want        []string
	}{
		{
			name:        "typical crash",
			question:    "Will X win?",
			outcome:     "X",
			sessionHigh: 0.45,
			ask:         0.17,
			want:        []string{"Will X win?", "X", "was $0.45", "now $0.17", "5.9×"},
		},
		{
			name:        "crash exactly at the bar",
			question:    "Lakers vs. Trail Blazers",
			outcome:     "Lakers",
			sessionHigh: 0.40,
			ask:         0.18,
			want:        []string{"Lakers", "was $0.40", "now $0.18", "5.6×"},
		},
		{
			name:        "non-ascii title survives",
			question:    "Кудерметова wins? " + strings.Repeat("好", 80),
			outcome:     "Да",
			sessionHigh: 0.52,
			ask:         0.05,
			want:        []string{"Кудерметова", "Да", "was $0.52", "now $0.05", "20.0×"},
		},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := snipeAlertText(tt.question, tt.outcome, tt.sessionHigh, tt.ask)
			for _, want := range tt.want {
				if !strings.Contains(got, want) {
					t.Errorf("snipeAlertText missing %q in:\n%s", want, got)
				}
			}
		})
	}
}

func TestSnipeRefuseBuy(t *testing.T) {
	t.Parallel()
	guard := live.SnipeCrashAsk * 1.5
	tests := []struct {
		name   string
		ask    float64
		ok     bool
		refuse bool
	}{
		{name: "crash-level ask buys", ask: 0.17, ok: true, refuse: false},
		{name: "ask exactly at the guard buys", ask: guard, ok: true, refuse: false},
		{name: "ask just above the guard refuses", ask: 0.28, ok: true, refuse: true},
		{name: "fully repriced refuses", ask: 0.55, ok: true, refuse: true},
		{name: "unavailable ask refuses", ask: 0, ok: false, refuse: true},
		{name: "zero ask refuses even when ok", ask: 0, ok: true, refuse: true},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := snipeRefuseBuy(tt.ask, tt.ok); got != tt.refuse {
				t.Errorf("snipeRefuseBuy(%v, %v) = %v, want %v", tt.ask, tt.ok, got, tt.refuse)
			}
		})
	}
}

func TestSnipeRepricedText(t *testing.T) {
	t.Parallel()
	withAsk := snipeRepricedText("X", 0.31, true)
	for _, want := range []string{"Repriced", "not buying", "$0.31"} {
		if !strings.Contains(withAsk, want) {
			t.Errorf("snipeRepricedText missing %q in:\n%s", want, withAsk)
		}
	}
	noAsk := snipeRepricedText("X", 0, false)
	if !strings.Contains(noAsk, "not buying") {
		t.Errorf("snipeRepricedText (no ask) missing refusal in:\n%s", noAsk)
	}
}

func testSnipeMarket() live.SnipeMarket {
	return live.SnipeMarket{
		TokenID:  strings.Repeat("7", 78), // real CLOB token IDs are ~78 digits
		MarketID: "157417",
		Question: "Lakers vs. Trail Blazers",
		Outcome:  "Lakers",
	}
}

func TestSnipeAlertRegistryClaimLifecycle(t *testing.T) {
	t.Parallel()
	r := newSnipeAlertRegistry()

	id := r.add(testSnipeMarket())
	if id == "" {
		t.Fatal("add returned empty id")
	}
	if data := "snipe:" + id + ":25"; len(data) > 64 {
		t.Fatalf("callback data %q is %d bytes, exceeds Telegram's 64", data, len(data))
	}

	entry, status := r.claim(id)
	if status != snipeAlertOK {
		t.Fatalf("first claim status = %v, want OK", status)
	}
	if entry.tokenID != testSnipeMarket().TokenID || entry.marketID != "157417" {
		t.Errorf("claimed entry = %+v, want the registered market", entry)
	}

	// Double-tap: the second claim must report used and never yield a buy.
	if _, status := r.claim(id); status != snipeAlertUsed {
		t.Errorf("second claim status = %v, want used", status)
	}

	if _, status := r.claim("nope"); status != snipeAlertExpired {
		t.Errorf("unknown id status = %v, want expired", status)
	}
}

func TestSnipeAlertRegistryExpiry(t *testing.T) {
	t.Parallel()
	r := newSnipeAlertRegistry()
	now := time.Unix(1_700_000_000, 0)
	r.now = func() time.Time { return now }

	id := r.add(testSnipeMarket())
	now = now.Add(13 * time.Hour) // past the ~12h TTL

	if _, status := r.claim(id); status != snipeAlertExpired {
		t.Errorf("claim after TTL = %v, want expired", status)
	}

	// Lazy cleanup: a later add prunes the stale entry.
	stale := r.add(testSnipeMarket())
	now = now.Add(13 * time.Hour)
	fresh := r.add(testSnipeMarket())
	r.mu.Lock()
	_, staleAlive := r.entries[stale]
	_, freshAlive := r.entries[fresh]
	n := len(r.entries)
	r.mu.Unlock()
	if staleAlive || !freshAlive || n != 1 {
		t.Errorf("lazy cleanup left %d entries (stale=%v fresh=%v), want only the fresh one", n, staleAlive, freshAlive)
	}
}

func TestSnipeAlertRegistryIDsAreUnique(t *testing.T) {
	t.Parallel()
	r := newSnipeAlertRegistry()
	seen := make(map[string]bool)
	for i := 0; i < 1000; i++ {
		id := r.add(testSnipeMarket())
		if seen[id] {
			t.Fatalf("duplicate alert id %q", id)
		}
		seen[id] = true
	}
}

func TestSnipeFilledText(t *testing.T) {
	t.Parallel()
	got := snipeFilledText("Lakers vs. Trail Blazers", "Lakers", 25, "ord-1")
	for _, want := range []string{"Lakers", "$25.00", "ord-1", "SL/TP"} {
		if !strings.Contains(got, want) {
			t.Errorf("snipeFilledText missing %q in:\n%s", want, got)
		}
	}
}
