package telegram

import (
	"strings"
	"testing"
)

// TestNotifySnipeAlertRestingOrderGate: a user with a resting buy order on
// the alerted token gets alert-only (auto-snipe skipped, tap buttons live).
// The resting order is the user's expressed entry strategy — the bot should
// not second-guess it.
func TestNotifySnipeAlertRestingOrderGate(t *testing.T) {
	t.Parallel()
	m := testSnipeMarket()
	h := newSnipeAutoBuyHarness(t, snipeHarnessConfig{ask: 0.17, askOK: true, user: snipeWalletUser()})

	// Simulate the user having placed a limit buy order on this token
	h.bot.trackRestingOrder(7, m.TokenID)

	h.bot.NotifySnipeAlert(7, m, 0.45, 0.17)

	// Auto-buy should NOT have fired
	if got := h.buys.count(); got != 0 {
		t.Fatalf("buy calls = %d, want 0 (resting order gate)", got)
	}
	if got := h.watch.boughtCount(); got != 0 {
		t.Errorf("MarkBought calls = %d, want 0", got)
	}

	// Should have sent manual alert with skip note
	sent := h.tg.sentAt(t, 0)
	if !strings.Contains(sent.text, "Comeback Snipe") || strings.Contains(sent.text, "Auto-sniped") {
		t.Errorf("resting-order-gated alert wrong:\n%s", sent.text)
	}
	if !strings.Contains(sent.text, "Auto-buy skipped") || !strings.Contains(sent.text, "resting buy order") {
		t.Errorf("resting-order-gated alert must explain the skip:\n%s", sent.text)
	}

	// Tap buttons should still be live
	callbackData(t, sent.markup, "⚡ Snipe $10")
	callbackData(t, sent.markup, "⚡ Snipe $25")

	// Cap should not have been reserved
	if _, ok := h.bot.snipeSpend.reserve(7, snipeAutoBuyDailyCapUSD); !ok {
		t.Error("cap consumed by a resting-order-gated alert")
	}
}

// TestNotifySnipeAlertNoRestingOrderProceeds: without a resting order, the
// auto-buy proceeds normally.
func TestNotifySnipeAlertNoRestingOrderProceeds(t *testing.T) {
	t.Parallel()
	h := newSnipeAutoBuyHarness(t, snipeHarnessConfig{ask: 0.17, askOK: true, user: snipeWalletUser()})

	// No resting order tracked — auto-buy should proceed
	h.bot.NotifySnipeAlert(7, testSnipeMarket(), 0.45, 0.17)

	if got := h.buys.count(); got != 1 {
		t.Fatalf("buy calls = %d, want 1 (no resting order)", got)
	}
}

// TestNotifySnipeAlertRestingOrderDifferentToken: a resting order on a
// different token does not gate the alert.
func TestNotifySnipeAlertRestingOrderDifferentToken(t *testing.T) {
	t.Parallel()
	h := newSnipeAutoBuyHarness(t, snipeHarnessConfig{ask: 0.17, askOK: true, user: snipeWalletUser()})

	// Resting order on a different token
	h.bot.trackRestingOrder(7, "different-token-id")

	h.bot.NotifySnipeAlert(7, testSnipeMarket(), 0.45, 0.17)

	// Should still auto-buy since the resting order is on a different token
	if got := h.buys.count(); got != 1 {
		t.Fatalf("buy calls = %d, want 1 (resting order on different token)", got)
	}
}

// TestNotifySnipeAlertRestingOrderDifferentChat: a resting order from a
// different chat does not gate this chat's alert.
func TestNotifySnipeAlertRestingOrderDifferentChat(t *testing.T) {
	t.Parallel()
	m := testSnipeMarket()
	h := newSnipeAutoBuyHarness(t, snipeHarnessConfig{ask: 0.17, askOK: true, user: snipeWalletUser()})

	// Resting order from a different chat
	h.bot.trackRestingOrder(999, m.TokenID)

	h.bot.NotifySnipeAlert(7, m, 0.45, 0.17)

	// Should still auto-buy since the resting order is from a different chat
	if got := h.buys.count(); got != 1 {
		t.Fatalf("buy calls = %d, want 1 (resting order from different chat)", got)
	}
}

// TestRestingOrderClearedOnCancel: clearing a resting order allows auto-snipe
// to proceed again.
func TestRestingOrderClearedOnCancel(t *testing.T) {
	t.Parallel()
	m := testSnipeMarket()
	h := newSnipeAutoBuyHarness(t, snipeHarnessConfig{ask: 0.17, askOK: true, user: snipeWalletUser()})

	// Track then clear
	h.bot.trackRestingOrder(7, m.TokenID)
	h.bot.clearRestingOrder(7, m.TokenID)

	h.bot.NotifySnipeAlert(7, m, 0.45, 0.17)

	// Should auto-buy since the resting order was cleared
	if got := h.buys.count(); got != 1 {
		t.Fatalf("buy calls = %d, want 1 (resting order cleared)", got)
	}
}

// TestRestingOrderGateAppliesToBoxedTranches: the resting order gate applies
// to boxed ladder tranches too, not just the in-band $10.
func TestRestingOrderGateAppliesToBoxedTranches(t *testing.T) {
	t.Parallel()
	m := testSnipeMarket()
	h := newSnipeAutoBuyHarness(t, snipeHarnessConfig{ask: 0.08, askOK: true, user: snipeWalletUser()})

	// Set up case-3 scenario: user holds the sibling (other side)
	h.watch.siblings = []string{"sibling-token-id"}
	h.bot.snipeBought.mark(7, "sibling-token-id", 10)

	// User also has a resting buy order on the alerted token
	h.bot.trackRestingOrder(7, m.TokenID)

	// Arm the boxed latch (simulating what happens when case-3 is detected)
	h.bot.snipeBoxedLatch.arm(7, m.TokenID, true, true)

	// Fire boxed tranche 1
	h.bot.NotifySnipeBoxed(7, m, 0.45, 0.08, 1)

	// Should NOT buy due to resting order gate
	if got := h.buys.count(); got != 0 {
		t.Fatalf("boxed buy calls = %d, want 0 (resting order gate)", got)
	}
}

// testSnipeMarket is defined in snipe_test.go
