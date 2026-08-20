package telegram

import (
	"context"
	"errors"
	"math/big"
	"strings"
	"testing"
	"time"

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
		b.snipeBought.mark(7, "sibB", snipeAutoBuyUSD)
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
		b.snipeBought.mark(7, "sibB", snipeAutoBuyUSD)
		if b.snipeHoldsSibling(context.Background(), user, 7, market) {
			t.Error("no watched siblings must short-circuit to false")
		}
	})
}

// TestNotifySnipeAlertCase3BoxedWait: a case-3 recipient at ask 0.18 gets no
// in-band buy — the alert says the auto-buy ladders the flip ($5 at ≤ $0.10 +
// $5 at ≤ $0.05), taps stay live, and the recipient is latched boxed-eligible.
func TestNotifySnipeAlertCase3BoxedWait(t *testing.T) {
	t.Parallel()
	h := newSnipeAutoBuyHarness(t, snipeHarnessConfig{ask: 0.18, askOK: true, user: snipeWalletUser()})
	h.watch.siblings = []string{"sibB"}
	h.bot.snipeBought.mark(7, "sibB", snipeAutoBuyUSD) // holds the other side
	m := testSnipeMarket()

	h.bot.NotifySnipeAlert(7, m, 0.45, 0.18)

	if got := h.buys.count(); got != 0 {
		t.Fatalf("buy calls = %d, want 0 (boxed-wait)", got)
	}
	sent := h.tg.sentAt(t, 0)
	if !strings.Contains(sent.text, "Comeback Snipe") || strings.Contains(sent.text, "Auto-sniped") {
		t.Errorf("boxed-wait alert wrong:\n%s", sent.text)
	}
	if !strings.Contains(sent.text, "other side") || !strings.Contains(sent.text, "0.10") || !strings.Contains(sent.text, "0.05") {
		t.Errorf("boxed-wait alert must explain the ladder:\n%s", sent.text)
	}
	// The recipient is now latched boxed-eligible for the episode.
	if !h.bot.snipeBoxedLatch.eligible(7, m.TokenID) {
		t.Error("case-3 boxed-wait alert did not latch the recipient boxed-eligible")
	}
	callbackData(t, sent.markup, "⚡ Snipe $10")
	callbackData(t, sent.markup, "⚡ Snipe $25")
	// Cap untouched.
	if _, ok := h.bot.snipeSpend.reserve(7, snipeAutoBuyDailyCapUSD); !ok {
		t.Error("cap consumed by a boxed-wait alert")
	}
}

// TestNotifySnipeAlertCase3ImmediateLadders (issue #78 F3): a case-3 recipient
// whose alert ask is ALREADY ≤ 0.10 buys tranche 1 ($5) at the alert — NOT a
// flat $10 — and stays latched for tranche 2, which the watcher's ≤0.05 fire
// adds. The watcher's tranche-1 fire must not re-buy. Total $10, never $15.
func TestNotifySnipeAlertCase3ImmediateLadders(t *testing.T) {
	t.Parallel()
	h := newSnipeAutoBuyHarness(t, snipeHarnessConfig{ask: 0.09, askOK: true, user: snipeWalletUser()})
	h.watch.siblings = []string{"sibB"}
	h.bot.snipeBought.mark(7, "sibB", snipeAutoBuyUSD)
	m := testSnipeMarket()

	h.bot.NotifySnipeAlert(7, m, 0.45, 0.09)

	// Immediate rung 1: exactly one $5 buy, flip token recorded, rung 2 latched.
	if got := h.buys.count(); got != 1 {
		t.Fatalf("immediate buys = %d, want 1", got)
	}
	if c := h.buys.call(t, 0); c.amount != snipeBoxedTrancheUSD {
		t.Errorf("immediate case-3 amount = %v, want $5 (not the flat $10)", c.amount)
	}
	if !h.bot.snipeBought.held(7, m.TokenID) {
		t.Error("immediate case-3 buy did not record the flip token")
	}
	if !h.bot.snipeBoxedLatch.eligible(7, m.TokenID) {
		t.Error("immediate case-3 must leave tranche 2 latched")
	}
	if sent := h.tg.sentAt(t, 0); !strings.Contains(sent.text, "Boxed flip tranche 1") || strings.Contains(sent.text, "$10") {
		t.Errorf("immediate case-3 alert must report the $5 tranche-1 buy, not $10:\n%s", sent.text)
	}

	// The watcher's tranche-1 fire must not re-buy (rung 1 consumed).
	h.bot.NotifySnipeBoxed(7, m, 0.45, 0.09, 1)
	if got := h.buys.count(); got != 1 {
		t.Fatalf("tranche-1 re-fire bought again: buys = %d, want 1 (no stacking to $15)", got)
	}
	// The watcher's tranche-2 fire adds the second $5 — total $10.
	h.bot.NotifySnipeBoxed(7, m, 0.45, 0.045, 2)
	if got := h.buys.count(); got != 2 {
		t.Fatalf("tranche-2 buys = %d, want 2 (total $10)", got)
	}
	if c := h.buys.call(t, 1); c.amount != snipeBoxedTrancheUSD {
		t.Errorf("tranche-2 amount = %v, want $5", c.amount)
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

// TestSnipeBoxedBoughtText: the tranche confirmation states the tranche number,
// the $5 stake, the fill price, order ID, and cap left. Pure — table-tested.
func TestSnipeBoxedBoughtText(t *testing.T) {
	t.Parallel()
	got := snipeBoxedBoughtText("LoL: T1 vs. Gen.G", "T1", 0.045, snipeBoxedTrancheUSD, 2, "ord-b2", 35)
	for _, want := range []string{"Boxed flip tranche 2", "$5", "T1", "$0.045", "ord-b2", "$35"} {
		if !strings.Contains(got, want) {
			t.Errorf("snipeBoxedBoughtText missing %q in:\n%s", want, got)
		}
	}
}

// TestNotifySnipeBoxedTrancheBuysLatched: a latched recipient's tranche buys $5
// from the main pool, records the flip token, and DMs a tranche confirmation.
func TestNotifySnipeBoxedTrancheBuysLatched(t *testing.T) {
	t.Parallel()
	h := newSnipeAutoBuyHarness(t, snipeHarnessConfig{ask: 0.09, askOK: true, user: snipeWalletUser()})
	m := testSnipeMarket()
	h.bot.snipeBoxedLatch.arm(7, m.TokenID, true, true) // latched case-3 at alert time

	h.bot.NotifySnipeBoxed(7, m, 0.45, 0.09, 1)

	if got := h.buys.count(); got != 1 {
		t.Fatalf("boxed buy calls = %d, want 1", got)
	}
	if c := h.buys.call(t, 0); c.amount != snipeBoxedTrancheUSD {
		t.Errorf("boxed tranche amount = %v, want %v", c.amount, snipeBoxedTrancheUSD)
	}
	if !h.bot.snipeBought.held(7, m.TokenID) {
		t.Error("boxed buy did not record the flip token")
	}
	sent := h.tg.sentAt(t, 0)
	if !strings.Contains(sent.text, "Boxed flip tranche 1") || !strings.Contains(sent.text, "ord-auto") {
		t.Errorf("boxed buy confirmation wrong:\n%s", sent.text)
	}
}

// TestNotifySnipeBoxedSkipsUnlatched: a recipient NOT latched at alert time never
// boxed-buys — even if sibling holdings appear later. The alert-time latch, not a
// fire-time re-check, is the sole decision (issue #78).
func TestNotifySnipeBoxedSkipsUnlatched(t *testing.T) {
	t.Parallel()
	h := newSnipeAutoBuyHarness(t, snipeHarnessConfig{ask: 0.09, askOK: true, user: snipeWalletUser()})
	m := testSnipeMarket()
	// Holdings that a fire-time snipeHoldsSibling re-check WOULD have accepted.
	h.watch.siblings = []string{"sibB"}
	h.bot.snipeBought.mark(7, "sibB", snipeAutoBuyUSD)
	// But the recipient was never latched (default false).

	h.bot.NotifySnipeBoxed(7, m, 0.45, 0.09, 1)

	if got := h.buys.count(); got != 0 {
		t.Fatalf("boxed buy calls = %d, want 0 (not latched)", got)
	}
	if got := h.tg.sendCount(); got != 0 {
		t.Errorf("boxed messaged an unlatched recipient (%d sends)", got)
	}
}

// TestNotifySnipeBoxedLadderR72Regression replays the r72 shape: hold A, the
// flip side B alerts case-3 (ask 0.18 ⇒ boxed-wait, latched). Tranche 1 buys $5,
// then A is ceiling-harvested mid-episode (the sibling holding disappears), and
// tranche 2 must STILL buy $5 — the latch, not a live sibling re-check, drives
// it. Two $5 tranches = the same $10 max exposure as the old single boxed buy.
func TestNotifySnipeBoxedLadderR72Regression(t *testing.T) {
	t.Parallel()
	h := newSnipeAutoBuyHarness(t, snipeHarnessConfig{ask: 0.18, askOK: true, user: snipeWalletUser()})
	m := testSnipeMarket()
	// Case-3 at alert: holds the flip side (sibB), ask 0.18 > 0.10 ⇒ boxed-wait.
	h.watch.siblings = []string{"sibB"}
	h.bot.snipeBought.mark(7, "sibB", snipeAutoBuyUSD)

	h.bot.NotifySnipeAlert(7, m, 0.45, 0.18)
	if got := h.buys.count(); got != 0 {
		t.Fatalf("in-band buys = %d, want 0 (boxed-wait)", got)
	}
	if !h.bot.snipeBoxedLatch.eligible(7, m.TokenID) {
		t.Fatal("case-3 alert did not latch boxed-eligibility")
	}

	// The held side is sold / ceiling-harvested mid-episode: no sibling remains.
	h.watch.siblings = nil
	h.bot.snipeBought = newSnipeBoughtRecord() // drop the sibB holding record too

	// Tranche 1 fires at ≤ 0.10.
	h.bot.NotifySnipeBoxed(7, m, 0.45, 0.09, 1)
	if got := h.buys.count(); got != 1 {
		t.Fatalf("after tranche 1 buys = %d, want 1", got)
	}
	// Tranche 1 just recorded the flip token as bought; tranche 2 must ignore that
	// and buy anyway (the ladder is two deliberate $5 rungs).
	h.bot.NotifySnipeBoxed(7, m, 0.45, 0.045, 2)
	if got := h.buys.count(); got != 2 {
		t.Fatalf("after tranche 2 buys = %d, want 2 (tranche 2 must still buy)", got)
	}
	for i, wantTranche := range []string{"tranche 1", "tranche 2"} {
		if c := h.buys.call(t, i); c.amount != snipeBoxedTrancheUSD {
			t.Errorf("tranche %d amount = %v, want %v", i+1, c.amount, snipeBoxedTrancheUSD)
		}
		// send 0 is the boxed-wait in-band alert; tranche confirmations follow.
		if sent := h.tg.sentAt(t, i+1); !strings.Contains(sent.text, wantTranche) {
			t.Errorf("send %d = %q, want mention of %q", i+1, sent.text, wantTranche)
		}
	}
}

// TestNotifySnipeBoxedNewEpisodeOverwritesLatch: the per-alert overwrite is the
// episode boundary. A recipient latched case-3 in episode 1 who is no longer
// case-3 at episode 2's in-band alert is un-latched, so episode 2's tranche is
// skipped.
func TestNotifySnipeBoxedNewEpisodeOverwritesLatch(t *testing.T) {
	t.Parallel()
	h := newSnipeAutoBuyHarness(t, snipeHarnessConfig{ask: 0.18, askOK: true, user: snipeWalletUser()})
	m := testSnipeMarket()

	// Episode 1 alert: case-3 ⇒ latched.
	h.watch.siblings = []string{"sibB"}
	h.bot.snipeBought.mark(7, "sibB", snipeAutoBuyUSD)
	h.bot.NotifySnipeAlert(7, m, 0.45, 0.18)
	if !h.bot.snipeBoxedLatch.eligible(7, m.TokenID) {
		t.Fatal("episode 1 did not latch boxed-eligibility")
	}

	// Episode 2 alert: no longer case-3 (sibling gone) ⇒ latch overwritten false.
	h.watch.siblings = nil
	h.bot.snipeBought = newSnipeBoughtRecord()
	h.bot.NotifySnipeAlert(7, m, 0.45, 0.18)
	if h.bot.snipeBoxedLatch.eligible(7, m.TokenID) {
		t.Fatal("episode 2's in-band alert must overwrite the latch to false")
	}

	buysBefore := h.buys.count()
	h.bot.NotifySnipeBoxed(7, m, 0.45, 0.09, 1)
	if got := h.buys.count(); got != buysBefore {
		t.Errorf("boxed buy after latch cleared = %d new, want 0", got-buysBefore)
	}
}

// TestNotifySnipeBoxedSportGate: the boxed ladder is esports-only too — a latched
// recipient on a non-esports market still gets no buy.
func TestNotifySnipeBoxedSportGate(t *testing.T) {
	t.Parallel()
	h := newSnipeAutoBuyHarness(t, snipeHarnessConfig{ask: 0.09, askOK: true, user: snipeWalletUser()})
	m := nonEsportsMarket()
	h.bot.snipeBoxedLatch.arm(7, m.TokenID, true, true)

	h.bot.NotifySnipeBoxed(7, m, 0.45, 0.09, 1)

	if got := h.buys.count(); got != 0 {
		t.Fatalf("boxed buy calls = %d, want 0 (sport gate)", got)
	}
}

// TestSnipeManualTapClearsBoxedLatch (issue #78 F2): a latched case-3 recipient
// who taps the advertised "buy now anyway" button must not then get the ladder
// stacked on top — the manual fill clears the latch, so later tranche fires are
// no-ops.
func TestSnipeManualTapClearsBoxedLatch(t *testing.T) {
	t.Parallel()
	h := newSnipeAutoBuyHarness(t, snipeHarnessConfig{ask: 0.18, askOK: true, user: snipeWalletUser()})
	h.watch.siblings = []string{"sibB"}
	h.bot.snipeBought.mark(7, "sibB", snipeAutoBuyUSD)
	m := testSnipeMarket()

	// Case-3 boxed-wait alert latches both rungs and advertises the manual tap.
	h.bot.NotifySnipeAlert(7, m, 0.45, 0.18)
	tapData := callbackData(t, h.tg.sentAt(t, 0).markup, "⚡ Snipe $25")

	// The user taps "buy now anyway" — a successful manual snipe.
	h.bot.handleSnipeCallback(context.Background(), snipeTapUpdate(7, tapData))
	if h.bot.snipeBoxedLatch.eligible(7, m.TokenID) {
		t.Fatal("a manual tap must clear the boxed latch (F2)")
	}

	buysAfterTap := h.buys.count()
	// The watcher's later tranche fires must NOT stack more buys.
	h.bot.NotifySnipeBoxed(7, m, 0.45, 0.09, 1)
	h.bot.NotifySnipeBoxed(7, m, 0.45, 0.045, 2)
	if got := h.buys.count(); got != buysAfterTap {
		t.Errorf("tranche fires after a manual tap bought %d more, want 0 (F2)", got-buysAfterTap)
	}
}

// TestNotifySnipeAlertCase3ImmediateFailureRetries (issue #78 F3): when the
// immediate case-3 $5 buy FAILS, BOTH rungs stay latched so the watcher's boxed
// re-offers retry — the recipient is not dropped for the episode.
func TestNotifySnipeAlertCase3ImmediateFailureRetries(t *testing.T) {
	t.Parallel()
	h := newSnipeAutoBuyHarness(t, snipeHarnessConfig{
		ask: 0.09, askOK: true, user: snipeWalletUser(),
		buyResult: &polymarket.TradeResult{Success: false, ErrorMsg: "rejected"},
	})
	h.watch.siblings = []string{"sibB"}
	h.bot.snipeBought.mark(7, "sibB", snipeAutoBuyUSD)
	m := testSnipeMarket()

	h.bot.NotifySnipeAlert(7, m, 0.45, 0.09)

	// The immediate $5 was attempted and failed; both rungs stay armed for retry.
	if got := h.buys.count(); got != 1 {
		t.Fatalf("immediate attempts = %d, want 1", got)
	}
	if !h.bot.snipeBoxedLatch.eligible(7, m.TokenID) {
		t.Fatal("a failed immediate case-3 buy must keep the recipient latched (retry)")
	}
	// Both tranche fires retry.
	h.bot.NotifySnipeBoxed(7, m, 0.45, 0.09, 1)
	h.bot.NotifySnipeBoxed(7, m, 0.45, 0.045, 2)
	if got := h.buys.count(); got != 3 {
		t.Errorf("retry attempts = %d, want 3 (immediate + both tranches)", got)
	}
}

// TestNotifySnipeBoxedStaleLatchSkipped (issue #78 F4): a latch older than its
// TTL belongs to an episode the recipient never saw a fresh alert for and must
// not fire; a fresh arm re-enables it.
func TestNotifySnipeBoxedStaleLatchSkipped(t *testing.T) {
	t.Parallel()
	h := newSnipeAutoBuyHarness(t, snipeHarnessConfig{ask: 0.09, askOK: true, user: snipeWalletUser()})
	m := testSnipeMarket()
	base := time.Date(2026, 8, 19, 12, 0, 0, 0, time.UTC)
	clock := base
	h.bot.snipeBoxedLatch.now = func() time.Time { return clock }

	h.bot.snipeBoxedLatch.arm(7, m.TokenID, true, true) // stamped at base
	clock = base.Add(snipeBoxedLatchTTL + time.Minute)  // latch now stale
	h.bot.NotifySnipeBoxed(7, m, 0.45, 0.09, 1)
	if got := h.buys.count(); got != 0 {
		t.Fatalf("stale-latch tranche bought (%d), want 0 (F4)", got)
	}

	// A fresh arm re-enables the buy.
	h.bot.snipeBoxedLatch.arm(7, m.TokenID, true, true)
	h.bot.NotifySnipeBoxed(7, m, 0.45, 0.09, 1)
	if got := h.buys.count(); got != 1 {
		t.Errorf("fresh-latch tranche buys = %d, want 1", got)
	}
}
