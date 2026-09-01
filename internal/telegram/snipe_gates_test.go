package telegram

import (
	"strings"
	"testing"
	"time"

	"github.com/Catorpilor/poly/internal/database"
	"github.com/Catorpilor/poly/internal/live"
)

// snipeWalletUserWithProxy is snipeWalletUser plus a Trading Wallet address, so
// the Gate 3 positions check has a proxy to scan.
func snipeWalletUserWithProxy() *database.User {
	u := snipeWalletUser()
	u.ProxyAddress = "0x000000000000000000000000000000000000dEaD"
	return u
}

// nonEsportsMarket is a tennis market — the sport gate must refuse it.
func nonEsportsMarket() live.SnipeMarket {
	m := testSnipeMarket()
	m.Question = "National Bank Open: Cobolli vs. Hanfmann"
	return m
}

// --- Gate 1 classifier (pure) ---

func TestSnipeIsEsports(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		texts []string
		want  bool
	}{
		{"counter-strike question", []string{"Counter-Strike: NIP vs. Legacy"}, true},
		{"cs2 marker", []string{"NIP vs Legacy CS2 BO1"}, true},
		{"dota question", []string{"Dota 2: Team Liquid vs. 1win"}, true},
		{"lol question", []string{"LoL: HANJIN BRION vs. Nongshim"}, true},
		{"league of legends spelled out", []string{"League of Legends: T1 vs Gen.G"}, true},
		{"lec league slug", []string{"", "lec-fnatic-g2-2026-08-01"}, true},
		{"valorant question", []string{"Valorant: A Team vs. Wolves"}, true},
		{"overwatch", []string{"Overwatch: Team A vs Team B"}, true},
		{"slug classifies when question is blank", []string{"", "cs2-faze-navi-2026-02-16"}, true},
		{"dota2 slug", []string{"", "dota2-flc-lgd-2026-08-12-game1"}, true},
		{"tennis is not esports", []string{"National Bank Open: Cobolli vs. Hanfmann"}, false},
		{"football club containing lec is not esports", []string{"US Lecce vs. AC Milan"}, false},
		{"name containing lol fragment is not esports", []string{"Serie A: Lollini United vs. Roma"}, false},
		{"name containing alec is not esports", []string{"Citi Open: Alec Deckers vs. Ann Li"}, false},
		{"football moneyline is not esports", []string{"SBV Excelsior"}, false},
		{"wnba is not esports", []string{"Toronto Tempo vs. Golden State Valkyries"}, false},
		{"nba is not esports", []string{"Lakers vs. Trail Blazers"}, false},
		{"empty is not esports", []string{""}, false},
		{"no args is not esports", nil, false},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := snipeIsEsports(tt.texts...); got != tt.want {
				t.Errorf("snipeIsEsports(%q) = %v, want %v", tt.texts, got, tt.want)
			}
		})
	}
}

// --- Gate 2 corpse geometry (pure) ---

func TestSnipeCorpseGeometry(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		bid   float64
		bidOK bool
		ask   float64
		want  bool
	}{
		{"healthy tight book is not a corpse", 0.15, true, 0.17, false},
		{"corpse signature bid far below ask", 0.022, true, 0.100, true},
		{"boundary bid == ask/3 is not a corpse", 0.10 / 3, true, 0.10, false},
		{"just below the boundary is a corpse", 0.10/3 - 0.001, true, 0.10, true},
		{"missing bid is corpse geometry", 0, false, 0.17, true},
		{"zero bid is corpse geometry", 0, true, 0.17, true},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := snipeCorpseGeometry(tt.bid, tt.bidOK, tt.ask); got != tt.want {
				t.Errorf("snipeCorpseGeometry(%v, %v, %v) = %v, want %v", tt.bid, tt.bidOK, tt.ask, got, tt.want)
			}
		})
	}
}

// --- Gate 1 (sport gate) wiring ---

// TestNotifySnipeAlertSportGate: a non-esports market skips the in-band auto-buy
// and falls back to the manual alert; the cap reservation is never consumed.
func TestNotifySnipeAlertSportGate(t *testing.T) {
	t.Parallel()
	h := newSnipeAutoBuyHarness(t, snipeHarnessConfig{ask: 0.17, askOK: true, user: snipeWalletUser()})

	h.bot.NotifySnipeAlert(7, nonEsportsMarket(), 0.45, 0.17)

	if got := h.buys.count(); got != 0 {
		t.Fatalf("buy calls = %d, want 0 (sport gate)", got)
	}
	if got := h.watch.boughtCount(); got != 0 {
		t.Errorf("MarkBought calls = %d, want 0", got)
	}
	sent := h.tg.sentAt(t, 0)
	if !strings.Contains(sent.text, "Comeback Snipe") || strings.Contains(sent.text, "Auto-sniped") {
		t.Errorf("sport-gated alert wrong:\n%s", sent.text)
	}
	if !strings.Contains(sent.text, "Auto-buy skipped") || !strings.Contains(sent.text, "esports") {
		t.Errorf("sport-gated alert must say the auto-buy was skipped for a non-esports market:\n%s", sent.text)
	}
	callbackData(t, sent.markup, "⚡ Snipe $10")
	callbackData(t, sent.markup, "⚡ Snipe $25")
	// The gate must not have reserved the cap.
	if _, ok := h.bot.snipeSpend.reserve(7, snipeAutoBuyDailyCapUSD); !ok {
		t.Error("cap consumed by a sport-gated alert")
	}
}

// TestNotifySnipeAlertEsportsProceeds: an esports market still auto-buys — the
// gate only blocks non-esports.
func TestNotifySnipeAlertEsportsProceeds(t *testing.T) {
	t.Parallel()
	h := newSnipeAutoBuyHarness(t, snipeHarnessConfig{ask: 0.17, askOK: true, user: snipeWalletUser()})

	h.bot.NotifySnipeAlert(7, testSnipeMarket(), 0.45, 0.17)

	if got := h.buys.count(); got != 1 {
		t.Fatalf("buy calls = %d, want 1 (esports proceeds)", got)
	}
}

// --- Gate 2 (corpse-spread gate, in-band $10 only) ---

// TestNotifySnipeAlertCorpseSpreadSkips: a fresh bid far below the ask is the
// decided-game corpse signature — skip the auto-buy, still alert, refund cap.
func TestNotifySnipeAlertCorpseSpreadSkips(t *testing.T) {
	t.Parallel()
	// ask 0.10, bid 0.022 ≈ 0.22× — the SBV Excelsior corpse geometry.
	h := newSnipeAutoBuyHarness(t, snipeHarnessConfig{
		ask: 0.10, askOK: true, bidSet: true, bid: 0.022, bidOK: true, user: snipeWalletUser(),
	})

	h.bot.NotifySnipeAlert(7, testSnipeMarket(), 0.45, 0.10)

	if got := h.buys.count(); got != 0 {
		t.Fatalf("buy calls = %d, want 0 (corpse spread)", got)
	}
	if got := h.watch.boughtCount(); got != 0 {
		t.Errorf("MarkBought calls = %d, want 0", got)
	}
	sent := h.tg.sentAt(t, 0)
	if !strings.Contains(sent.text, "Comeback Snipe") || strings.Contains(sent.text, "Auto-sniped") {
		t.Errorf("corpse-gated alert wrong:\n%s", sent.text)
	}
	if !strings.Contains(sent.text, "Auto-buy skipped") || !strings.Contains(sent.text, "spread") {
		t.Errorf("corpse-gated alert must cite the spread signature:\n%s", sent.text)
	}
	if _, ok := h.bot.snipeSpend.reserve(7, snipeAutoBuyDailyCapUSD); !ok {
		t.Error("cap not refunded after a corpse-spread skip")
	}
}

// TestNotifySnipeAlertCorpseSpreadBidError: a failed/zero bid fetch is treated
// as corpse geometry and skips the buy.
func TestNotifySnipeAlertCorpseSpreadBidError(t *testing.T) {
	t.Parallel()
	h := newSnipeAutoBuyHarness(t, snipeHarnessConfig{
		ask: 0.17, askOK: true, bidSet: true, bid: 0, bidOK: false, user: snipeWalletUser(),
	})

	h.bot.NotifySnipeAlert(7, testSnipeMarket(), 0.45, 0.17)

	if got := h.buys.count(); got != 0 {
		t.Fatalf("buy calls = %d, want 0 (bid fetch failed ⇒ corpse)", got)
	}
	sent := h.tg.sentAt(t, 0)
	if !strings.Contains(sent.text, "Auto-buy skipped") {
		t.Errorf("bid-error alert must fall back to manual:\n%s", sent.text)
	}
}

// TestNotifySnipeAlertCorpseSpreadBoundary: bid == ask/3 exactly clears the gate
// (strictly less-than) and the auto-buy proceeds.
func TestNotifySnipeAlertCorpseSpreadBoundary(t *testing.T) {
	t.Parallel()
	h := newSnipeAutoBuyHarness(t, snipeHarnessConfig{
		ask: 0.18, askOK: true, bidSet: true, bid: 0.06, bidOK: true, user: snipeWalletUser(),
	})

	h.bot.NotifySnipeAlert(7, testSnipeMarket(), 0.45, 0.18)

	if got := h.buys.count(); got != 1 {
		t.Fatalf("buy calls = %d, want 1 (bid == ask/3 proceeds)", got)
	}
}

// TestSnipeInBandBuyRecordsHolding: a successful in-band auto-buy records the
// holding — the lag-free half of the boxed case-3 sibling gate, and the record
// the durable buy-log write-through rebuilds. Only the in-band auto-buy, a
// one-tap, and boxed tranches feed it.
func TestSnipeInBandBuyRecordsHolding(t *testing.T) {
	t.Parallel()
	m := testSnipeMarket()
	h := newSnipeAutoBuyHarness(t, snipeHarnessConfig{ask: 0.17, askOK: true, user: snipeWalletUserWithProxy()})

	h.bot.NotifySnipeAlert(7, m, 0.45, 0.17) // in-band auto-buy fills and records
	if got := h.buys.count(); got != 1 {
		t.Fatalf("in-band buy calls = %d, want 1", got)
	}
	if !h.bot.snipeBought.held(7, m.TokenID) {
		t.Fatal("in-band auto-buy did not record the holding")
	}
}

// TestNotifySnipeDeepCrashAlertOnly (issue #105): the Deep Crash tier is
// alert-only — a fully-qualified deep fire (esports market, recipient holds
// nothing, fresh day) attempts ZERO buys and DMs the alert-only Deep Crash
// notice with ⚡ tap buttons. Regression for the September retirement of the
// 0-for-13, −$64.73 $5 auto-pool.
func TestNotifySnipeDeepCrashAlertOnly(t *testing.T) {
	t.Parallel()
	h := newSnipeAutoBuyHarness(t, snipeHarnessConfig{ask: 0.02, askOK: true, user: snipeWalletUser()})

	h.bot.NotifySnipeDeepCrash(7, testSnipeMarket(), 0.45, 0.02, 0.09, time.Minute)

	if got := h.buys.count(); got != 0 {
		t.Fatalf("deep buy calls = %d, want 0 — the deep tier is alert-only", got)
	}
	if got := h.watch.boughtCount(); got != 0 {
		t.Errorf("MarkBought calls = %d, want 0 (no auto-buy at this depth)", got)
	}
	sent := h.tg.sentAt(t, 0)
	if !strings.Contains(sent.text, "Deep Crash") || !strings.Contains(sent.text, "Corpse territory") {
		t.Errorf("deep alert missing the crash summary + corpse warning:\n%s", sent.text)
	}
	// Tap buttons stay live — the user still judges the game.
	callbackData(t, sent.markup, "⚡ Snipe $10")
	callbackData(t, sent.markup, "⚡ Snipe $25")
}
