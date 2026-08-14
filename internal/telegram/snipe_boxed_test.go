package telegram

import (
	"context"
	"errors"
	"math/big"
	"strings"
	"testing"

	"github.com/Catorpilor/poly/internal/database"
	"github.com/Catorpilor/poly/internal/database/repositories"
	"github.com/Catorpilor/poly/internal/live"
	"github.com/Catorpilor/poly/internal/polymarket"
)

// siblingArmRepo returns a fixed arm per token ID for the boxed sibling check.
type siblingArmRepo struct {
	repositories.SLTPArmRepository
	arms map[string]*database.SLTPArm
}

func (r *siblingArmRepo) GetByUserAndToken(_ context.Context, _ int64, tokenID string) (*database.SLTPArm, error) {
	return r.arms[tokenID], nil
}

// TestSnipeHoldsSibling covers the three case-3 detection sources (first hit
// wins) and the conservative fallbacks.
func TestSnipeHoldsSibling(t *testing.T) {
	t.Parallel()
	user := &database.User{TelegramID: 7, ProxyAddress: "0x000000000000000000000000000000000000dEaD"}
	market := live.SnipeMarket{TokenID: "A", MarketID: "m1"}

	newBot := func() *Bot {
		return &Bot{
			snipeWatcher: &fakeSnipeWatch{siblings: []string{"sibB"}},
			snipeBought:  newSnipeBoughtRecord(),
		}
	}

	t.Run("record source", func(t *testing.T) {
		t.Parallel()
		b := newBot()
		b.snipeBought.mark(7, "sibB")
		if !b.snipeHoldsSibling(context.Background(), user, 7, market) {
			t.Error("record-held sibling not detected")
		}
	})

	t.Run("arm source", func(t *testing.T) {
		t.Parallel()
		b := newBot()
		b.sltpArmRepo = &siblingArmRepo{arms: map[string]*database.SLTPArm{"sibB": {ID: 1, TokenID: "sibB"}}}
		if !b.snipeHoldsSibling(context.Background(), user, 7, market) {
			t.Error("armed sibling not detected")
		}
	})

	t.Run("positions source", func(t *testing.T) {
		t.Parallel()
		b := newBot()
		b.snipePositions = &fakePositionSource{positions: []*polymarket.Position{
			{TokenID: "sibB", Shares: big.NewInt(50_000_000)},
		}}
		if !b.snipeHoldsSibling(context.Background(), user, 7, market) {
			t.Error("positions-held sibling not detected")
		}
	})

	t.Run("no sibling holding anywhere ⇒ false", func(t *testing.T) {
		t.Parallel()
		b := newBot()
		b.snipePositions = &fakePositionSource{positions: []*polymarket.Position{
			{TokenID: "unrelated", Shares: big.NewInt(50_000_000)},
		}}
		if b.snipeHoldsSibling(context.Background(), user, 7, market) {
			t.Error("false case-3 with no sibling holding")
		}
	})

	t.Run("positions API error ⇒ not case-3 (conservative)", func(t *testing.T) {
		t.Parallel()
		b := newBot()
		b.snipePositions = &fakePositionSource{err: errors.New("data api down")}
		if b.snipeHoldsSibling(context.Background(), user, 7, market) {
			t.Error("positions error must be treated as not case-3")
		}
	})

	t.Run("no watched siblings ⇒ false without any lookup", func(t *testing.T) {
		t.Parallel()
		b := &Bot{snipeWatcher: &fakeSnipeWatch{}, snipeBought: newSnipeBoughtRecord()}
		b.snipeBought.mark(7, "sibB")
		if b.snipeHoldsSibling(context.Background(), user, 7, market) {
			t.Error("no watched siblings must short-circuit to false")
		}
	})
}

// TestNotifySnipeAlertCase3BoxedWait: a case-3 recipient at ask 0.18 gets no
// in-band buy — the alert says the auto-buy waits for ≤ $0.10, taps stay live.
func TestNotifySnipeAlertCase3BoxedWait(t *testing.T) {
	t.Parallel()
	h := newSnipeAutoBuyHarness(t, snipeHarnessConfig{ask: 0.18, askOK: true, user: snipeWalletUser()})
	h.watch.siblings = []string{"sibB"}
	h.bot.snipeBought.mark(7, "sibB") // holds the other side

	h.bot.NotifySnipeAlert(7, testSnipeMarket(), 0.45, 0.18)

	if got := h.buys.count(); got != 0 {
		t.Fatalf("buy calls = %d, want 0 (boxed-wait)", got)
	}
	sent := h.tg.sentAt(t, 0)
	if !strings.Contains(sent.text, "Comeback Snipe") || strings.Contains(sent.text, "Auto-sniped") {
		t.Errorf("boxed-wait alert wrong:\n%s", sent.text)
	}
	if !strings.Contains(sent.text, "other side") || !strings.Contains(sent.text, "0.10") {
		t.Errorf("boxed-wait alert must explain the postponement:\n%s", sent.text)
	}
	callbackData(t, sent.markup, "⚡ Snipe $10")
	callbackData(t, sent.markup, "⚡ Snipe $25")
	// Cap untouched.
	if _, ok := h.bot.snipeSpend.reserve(7, snipeAutoBuyDailyCapUSD); !ok {
		t.Error("cap consumed by a boxed-wait alert")
	}
}

// TestNotifySnipeAlertCase3ImmediateWhenDeep: a case-3 recipient whose alert
// ask is ALREADY ≤ 0.10 buys now (no postponement) and the flip token is
// recorded (so the later boxed offer dedups).
func TestNotifySnipeAlertCase3ImmediateWhenDeep(t *testing.T) {
	t.Parallel()
	h := newSnipeAutoBuyHarness(t, snipeHarnessConfig{ask: 0.09, askOK: true, user: snipeWalletUser()})
	h.watch.siblings = []string{"sibB"}
	h.bot.snipeBought.mark(7, "sibB")
	m := testSnipeMarket()

	h.bot.NotifySnipeAlert(7, m, 0.45, 0.09)

	if got := h.buys.count(); got != 1 {
		t.Fatalf("buy calls = %d, want 1 (already ≤ 0.10 ⇒ buy now)", got)
	}
	if !h.bot.snipeBought.held(7, m.TokenID) {
		t.Error("immediate case-3 buy did not record the flip token")
	}
}

// TestNotifySnipeAlertCase2Unchanged: a non-case-3 recipient at ask 0.18 buys
// exactly as before — the boxed path never triggers.
func TestNotifySnipeAlertCase2Unchanged(t *testing.T) {
	t.Parallel()
	h := newSnipeAutoBuyHarness(t, snipeHarnessConfig{ask: 0.18, askOK: true, user: snipeWalletUser()})
	// No siblings watched ⇒ never case-3.

	h.bot.NotifySnipeAlert(7, testSnipeMarket(), 0.45, 0.18)

	if got := h.buys.count(); got != 1 {
		t.Fatalf("buy calls = %d, want 1 (case-2 unchanged)", got)
	}
}

// TestNotifySnipeBoxedBuysCase3: the boxed dispatch buys $10 for a case-3
// recipient who hasn't bought the flip token yet, and DMs a boxed confirmation.
func TestNotifySnipeBoxedBuysCase3(t *testing.T) {
	t.Parallel()
	h := newSnipeAutoBuyHarness(t, snipeHarnessConfig{ask: 0.09, askOK: true, user: snipeWalletUser()})
	h.watch.siblings = []string{"sibB"}
	h.bot.snipeBought.mark(7, "sibB") // case-3, but flip token A not yet bought
	m := testSnipeMarket()

	h.bot.NotifySnipeBoxed(7, m, 0.45, 0.09)

	if got := h.buys.count(); got != 1 {
		t.Fatalf("boxed buy calls = %d, want 1", got)
	}
	if !h.bot.snipeBought.held(7, m.TokenID) {
		t.Error("boxed buy did not record the flip token")
	}
	sent := h.tg.sentAt(t, 0)
	if !strings.Contains(sent.text, "Boxed") || !strings.Contains(sent.text, "ord-auto") {
		t.Errorf("boxed buy confirmation wrong:\n%s", sent.text)
	}
}

// TestNotifySnipeBoxedSkipsAlreadyBought: a recipient who already holds the flip
// token (bought it in-band/tap) gets no second buy and no message.
func TestNotifySnipeBoxedSkipsAlreadyBought(t *testing.T) {
	t.Parallel()
	h := newSnipeAutoBuyHarness(t, snipeHarnessConfig{ask: 0.09, askOK: true, user: snipeWalletUser()})
	h.watch.siblings = []string{"sibB"}
	m := testSnipeMarket()
	h.bot.snipeBought.mark(7, "sibB")
	h.bot.snipeBought.mark(7, m.TokenID) // already bought the flip token

	h.bot.NotifySnipeBoxed(7, m, 0.45, 0.09)

	if got := h.buys.count(); got != 0 {
		t.Fatalf("boxed buy calls = %d, want 0 (already bought the flip token)", got)
	}
	if got := h.tg.sendCount(); got != 0 {
		t.Errorf("boxed sent %d messages for an already-bought token, want 0", got)
	}
}

// TestNotifySnipeBoxedSkipsNonCase3: a recipient who doesn't hold the other side
// gets nothing (they had their chance at the in-band alert).
func TestNotifySnipeBoxedSkipsNonCase3(t *testing.T) {
	t.Parallel()
	h := newSnipeAutoBuyHarness(t, snipeHarnessConfig{ask: 0.09, askOK: true, user: snipeWalletUser()})
	// No siblings ⇒ not case-3.

	h.bot.NotifySnipeBoxed(7, testSnipeMarket(), 0.45, 0.09)

	if got := h.buys.count(); got != 0 {
		t.Fatalf("boxed buy calls = %d, want 0 (non-case-3)", got)
	}
	if got := h.tg.sendCount(); got != 0 {
		t.Errorf("boxed messaged a non-case-3 recipient (%d sends)", got)
	}
}

// TestNotifySnipeBoxedSportGate: the boxed tier is esports-only too.
func TestNotifySnipeBoxedSportGate(t *testing.T) {
	t.Parallel()
	h := newSnipeAutoBuyHarness(t, snipeHarnessConfig{ask: 0.09, askOK: true, user: snipeWalletUser()})
	h.watch.siblings = []string{"sibB"}
	h.bot.snipeBought.mark(7, "sibB")
	m := nonEsportsMarket()

	h.bot.NotifySnipeBoxed(7, m, 0.45, 0.09)

	if got := h.buys.count(); got != 0 {
		t.Fatalf("boxed buy calls = %d, want 0 (sport gate)", got)
	}
}
