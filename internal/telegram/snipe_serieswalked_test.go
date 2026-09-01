package telegram

import (
	"context"
	"strings"
	"testing"
)

// Issue #102 (ledger r114/r115): a recipient whose ONLY claim on the crashed
// market came from the series walk gets the alert with tap buttons but NO auto
// money — both the in-band $10 and the deep $5 tiers skip with the
// `series-walked` class. The walk auto-bought TL–SR G2 (r114) and the BO3 ML
// (r115) on markets the holder never personally traded; this gate stops that.

// In-band tier: walked-only recipient + qualifying in-band crash ⇒ alert-only.
func TestNotifySnipeAlertSeriesWalkedSkips(t *testing.T) {
	t.Parallel()
	h := newSnipeAutoBuyHarness(t, snipeHarnessConfig{ask: 0.17, askOK: true, user: snipeWalletUser()})
	m := testSnipeMarket()
	h.watch.markWalkedOnly(m.TokenID) // watched ONLY via the series walk

	h.bot.NotifySnipeAlert(7, m, 0.45, 0.17)

	if got := h.buys.count(); got != 0 {
		t.Fatalf("buy calls = %d, want 0 — a series-walked market must never auto-buy", got)
	}
	if got := h.watch.boughtCount(); got != 0 {
		t.Errorf("MarkBought calls = %d, want 0", got)
	}
	sent := h.tg.sentAt(t, 0)
	if strings.Contains(sent.text, "Auto-sniped") {
		t.Errorf("series-walked alert must not claim an auto-buy:\n%s", sent.text)
	}
	for _, want := range []string{
		"Auto-buy skipped",
		"series you traded",
		"continuations are alert-only",
		"tap below if you still want it",
	} {
		if !strings.Contains(sent.text, want) {
			t.Errorf("series-walked skip note missing %q in:\n%s", want, sent.text)
		}
	}
	// Tap buttons stay live — the user still judges the game.
	callbackData(t, sent.markup, "⚡ Snipe $10")
	callbackData(t, sent.markup, "⚡ Snipe $25")
	// The daily cap was never touched (no reserve on a class skip).
	if _, ok := h.bot.snipeSpend.reserve(7, snipeAutoBuyDailyCapUSD); !ok {
		t.Error("series-walked skip consumed the daily cap")
	}
}

// Tapping a walked market upgrades it to full semantics (ratified): the ⚡ tap
// is never gated, and a SUCCESSFUL tap registers the bought market DIRECT via the
// house bought-token registration — so WalkedOnlyHolder flips false and a later
// qualifying crash auto-buys instead of skipping series-walked.
func TestSnipeTapUpgradesWalkedMarket(t *testing.T) {
	t.Parallel()
	h := newSnipeAutoBuyHarness(t, snipeHarnessConfig{ask: 0.17, askOK: true, user: snipeWalletUser()})
	m := testSnipeMarket()
	h.watch.markWalkedOnly(m.TokenID)

	// The walked market alerts alert-only (series-walked skip, no auto money).
	h.bot.NotifySnipeAlert(7, m, 0.45, 0.17)
	if got := h.buys.count(); got != 0 {
		t.Fatalf("pre-tap buys = %d, want 0 (series-walked skip)", got)
	}
	tapData := callbackData(t, h.tg.sentAt(t, 0).markup, "⚡ Snipe $10")

	// The tap itself buys (taps are never gated) and upgrades the market.
	h.bot.handleSnipeCallback(context.Background(), snipeTapUpdate(7, tapData))
	if got := h.buys.count(); got != 1 {
		t.Fatalf("post-tap buys = %d, want 1 (the tap buy itself)", got)
	}
	if h.watch.WalkedOnlyHolder(7, m.TokenID) {
		t.Error("tap-bought market still walked-only — a tap must upgrade it to direct")
	}
	// The bought market's tokens are now registered direct (sibling watch too).
	if held := h.watch.heldTokens(); len(held) == 0 {
		t.Error("tap success did not register the bought market as held")
	}

	// A subsequent qualifying crash now auto-buys — the gate no longer fires.
	h.bot.NotifySnipeAlert(7, m, 0.45, 0.17)
	if got := h.buys.count(); got != 2 {
		t.Fatalf("post-upgrade buys = %d, want 2 (upgraded market auto-buys, no series-walked skip)", got)
	}
}

// A DIRECT recipient (WalkedOnlyHolder=false) is unaffected — the gate is scoped
// to walked-only markets, so a real hold/arm/position/web claim still auto-buys.
func TestNotifySnipeAlertDirectRecipientUnaffected(t *testing.T) {
	t.Parallel()
	h := newSnipeAutoBuyHarness(t, snipeHarnessConfig{ask: 0.17, askOK: true, user: snipeWalletUser()})
	// Token is NOT marked walked-only ⇒ direct recipient.

	h.bot.NotifySnipeAlert(7, testSnipeMarket(), 0.45, 0.17)

	if got := h.buys.count(); got != 1 {
		t.Fatalf("direct-recipient buy calls = %d, want 1 (gate must not touch direct claims)", got)
	}
	if sent := h.tg.sentAt(t, 0); !strings.Contains(sent.text, "Auto-sniped") {
		t.Errorf("direct recipient should auto-buy:\n%s", sent.text)
	}
}
