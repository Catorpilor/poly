package telegram

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Catorpilor/poly/internal/live"
	"github.com/Catorpilor/poly/internal/polymarket"
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
			ask:         0.20,
			want:        []string{"Lakers", "was $0.40", "now $0.20", "5.0×"},
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
			// v1 copy claimed the bot never buys on its own — false since the
			// v2 auto-buy, and it landed 2s before an "Auto-sniped" confirm on
			// 2026-08-05 (issue #50).
			if strings.Contains(got, "never buys on its own") {
				t.Errorf("snipeAlertText still carries stale v1 copy:\n%s", got)
			}
		})
	}
}

// TestSnipeSkipNote: the degraded manual alert must say why the auto-buy
// didn't run (issue #50: contradictory copy without a reason).
func TestSnipeSkipNote(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		res  snipeBuyResult
		want string
	}{
		{"repriced", snipeBuyResult{outcome: snipeBuyRepriced, ask: 0.34, askOK: true}, "moved past the snipe guard"},
		{"no ask", snipeBuyResult{outcome: snipeBuyRepriced, askOK: false}, "moved past the snipe guard"},
		{"market error", snipeBuyResult{outcome: snipeBuyMarketErr}, "market lookup failed"},
		{"mismatch", snipeBuyResult{outcome: snipeBuyMismatch}, "market data mismatch"},
		{"rejected", snipeBuyResult{outcome: snipeBuyRejected, errorMsg: "not enough balance"}, "order was rejected"},
		{"no wallet", snipeBuyResult{outcome: snipeBuyNoWallet}, "no trading wallet"},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := snipeSkipNote(tt.res)
			if !strings.Contains(got, "Auto-buy skipped") || !strings.Contains(got, tt.want) {
				t.Errorf("snipeSkipNote(%v) = %q, want it to contain %q and 'Auto-buy skipped'", tt.res.outcome, got, tt.want)
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
		{name: "ask just above the guard refuses", ask: 0.31, ok: true, refuse: true},
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
		Question: "LoL: T1 vs. Gen.G", // esports so the sport gate lets the auto-buy through
		Outcome:  "T1",
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

// TestFetchSnipeMarketRoutesByIDForm reproduces issue #33: the Data API's
// position "market ID" is a 0x-prefixed condition ID, which must go through
// Gamma's ?condition_id= query — the /markets/{id} path form 422s on it.
func TestFetchSnipeMarketRoutesByIDForm(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/markets" && r.URL.Query().Get("condition_ids") != "":
			fmt.Fprint(w, `[{"id":"3080779","question":"Game 1 Winner","conditionId":"0x1cf14d0add6dfc90f2e3de1250cce7775cb5f5c909e9c81111f47c9ba5ce49a5"}]`)
		case r.URL.Path == "/markets":
			// Real Gamma behavior: unknown params are IGNORED and the default
			// market list comes back — an unrelated market that must never
			// pass as a lookup result.
			fmt.Fprint(w, `[{"id":"9999","question":"Unrelated politics market","conditionId":"0xdead","slug":"xi-jinping-out"}]`)
		case r.URL.Path == "/markets/3080779":
			fmt.Fprint(w, `{"id":"3080779","question":"Game 1 Winner"}`)
		default:
			// The path form with a 0x id — Gamma's real behavior is 422.
			w.WriteHeader(http.StatusUnprocessableEntity)
		}
	}))
	defer srv.Close()
	mc := polymarket.NewMarketClientWithURL(srv.URL)

	tests := []struct {
		name string
		id   string
	}{
		{name: "condition ID routes via query", id: "0x1cf14d0add6dfc90f2e3de1250cce7775cb5f5c909e9c81111f47c9ba5ce49a5"},
		{name: "numeric gamma ID routes via path", id: "3080779"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m, err := fetchSnipeMarket(context.Background(), mc, tt.id)
			if err != nil {
				t.Fatalf("fetchSnipeMarket(%q): %v", tt.id, err)
			}
			if m == nil || m.Question != "Game 1 Winner" {
				t.Fatalf("fetchSnipeMarket(%q) = %+v, want Game 1 Winner", tt.id, m)
			}
		})
	}
}

// TestFetchSnipeMarketEnrichesGameStart reproduces the v0.12.3 gap: Gamma's
// ?condition_id= responses omit gameStartTime entirely (the by-slug form has
// it), which left armed tokens permanently outside the in-play gate. The
// router must refetch by slug when the condition-ID response lacks a game
// start but carries a slug.
func TestFetchSnipeMarketEnrichesGameStart(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/markets" && r.URL.Query().Get("condition_ids") != "":
			// Sports markets can still omit gameStartTime on this form in
			// odd cases; the router then enriches via slug.
			fmt.Fprint(w, `[{"id":"3080779","question":"Game 2 Winner","conditionId":"0xf17e3c60c7ca0094aec6f7db5bcf058d8b8da68d7e01e9675fc2493e451237ac","slug":"lol-ns-fox1-game2"}]`)
		case r.URL.Path == "/markets":
			fmt.Fprint(w, `[{"id":"9999","question":"Unrelated politics market","conditionId":"0xdead","slug":"xi-jinping-out"}]`)
		case r.URL.Path == "/markets/slug/lol-ns-fox1-game2":
			// The by-slug form returns a single object and includes the field.
			fmt.Fprint(w, `{"id":"3080779","question":"Game 2 Winner","conditionId":"0xf17e","slug":"lol-ns-fox1-game2","gameStartTime":"2026-08-01 08:00:00+00"}`)
		default:
			w.WriteHeader(http.StatusUnprocessableEntity)
		}
	}))
	defer srv.Close()
	mc := polymarket.NewMarketClientWithURL(srv.URL)

	m, err := fetchSnipeMarket(context.Background(), mc, "0xf17e3c60c7ca0094aec6f7db5bcf058d8b8da68d7e01e9675fc2493e451237ac")
	if err != nil {
		t.Fatalf("fetchSnipeMarket: %v", err)
	}
	if m.GetGameStartTime().IsZero() {
		t.Fatalf("gameStartTime not enriched: %+v", m)
	}
	if got := m.GetGameStartTime().UTC().Hour(); got != 8 {
		t.Errorf("gameStartTime hour = %d, want 8", got)
	}
}
