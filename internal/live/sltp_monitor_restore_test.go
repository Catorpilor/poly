package live

import (
	"testing"
	"time"

	"github.com/Catorpilor/poly/internal/database"
	"github.com/Catorpilor/poly/internal/polymarket"
)

// Issue #92 residual (ledger r106): a committed fire whose sell definitively
// sold nothing (e.g. "no liquidity available" — a phantom print into an empty
// book) must restore what it committed — tp_armed, ladder rungs, or the
// ceiling-deleted row — arm a retry backoff, and notify once per streak.

func noLiquidityResult() *polymarket.TradeResult {
	return &polymarket.TradeResult{Success: false, ErrorMsg: "Failed to get price: no liquidity available"}
}

func restoreHarness(ret *polymarket.TradeResult) (*SLTPMonitor, *fakeStore, *fakeNotifier, *fakeExecutor, *fakeClock) {
	store := newFakeStore()
	feed := newFakeFeed()
	exec := &fakeExecutor{ret: ret}
	notif := &fakeNotifier{}
	m, clock := slBreachMonitor(store, feed, exec, notif)
	return m, store, notif, exec, clock
}

func firedCount(notif *fakeNotifier) int {
	notif.mu.Lock()
	defer notif.mu.Unlock()
	return len(notif.fires)
}

// The plain TP path: ClearTP committed, sell sold nothing → tp_armed restored,
// one DM, and the backoff blocks an immediate re-fire.
func TestTPFailedSell_RestoresTPArm(t *testing.T) {
	t.Parallel()
	m, store, notif, exec, clock := restoreHarness(noLiquidityResult())
	arm := &database.SLTPArm{ID: 601, TelegramID: 95, TokenID: "RST1",
		AvgPrice: 0.16, SharesAtArm: 91.6, HighWaterMark: 0.16, TPArmed: true}
	store.seed(arm)

	m.fireTP(arm, 0.33)

	store.mu.Lock()
	rearmed := store.rearmTPCalls
	tpArmed := store.byToken["RST1"][0].TPArmed
	store.mu.Unlock()
	if rearmed != 1 || !tpArmed {
		t.Fatalf("rearmTP calls = %d, tp_armed = %v — want the flag restored after a sold-nothing sell", rearmed, tpArmed)
	}
	if got := firedCount(notif); got != 1 {
		t.Errorf("fired notices = %d, want 1", got)
	}

	// Within the backoff the fire path must not even claim the slot.
	before := len(exec.calls)
	m.fireTP(arm, 0.33)
	if got := len(exec.calls); got != before {
		t.Errorf("sell attempted during failed-sell backoff (calls %d→%d)", before, got)
	}

	// After the backoff the TP retries.
	clock.advance(failedSellBackoff + time.Second)
	exec.mu.Lock()
	exec.ret = &polymarket.TradeResult{Success: true, OrderID: "ord-r"}
	exec.mu.Unlock()
	m.fireTP(arm, 0.33)
	exec.mu.Lock()
	after := len(exec.calls)
	exec.mu.Unlock()
	if after != before+1 {
		t.Errorf("sell attempts after backoff = %d, want %d (retry allowed)", after, before+1)
	}
}

// The r106 regression: the deep-entry ladder fires all rungs on a phantom
// print, the sell sells nothing → the rungs AND the frozen base are restored,
// so the mid-range harvest can retry.
func TestLadderFailedSell_RestoresRungs(t *testing.T) {
	t.Parallel()
	m, store, notif, _, _ := restoreHarness(noLiquidityResult())
	arm := &database.SLTPArm{ID: 602, TelegramID: 95, TokenID: "RST2",
		AvgPrice: 0.01, SharesAtArm: 995.06, HighWaterMark: 0.01, TPArmed: true}
	store.seed(arm)

	if fired := m.fireLadder(arm, 0.27); !fired {
		t.Fatal("fireLadder did not consume the tick")
	}

	store.mu.Lock()
	restored := store.restoreLadderCalls
	rungs := store.byToken["RST2"][0].LadderRungsFired
	base := store.byToken["RST2"][0].LadderBaseShares
	store.mu.Unlock()
	if restored != 1 || rungs != 0 || base != 0 {
		t.Fatalf("restore calls = %d, rungs = %d, base = %.1f — want rungs and base fully restored (r106)", restored, rungs, base)
	}
	if arm.LadderRungsFired != 0 || arm.LadderBaseShares != 0 {
		t.Errorf("local arm copy rungs=%d base=%.1f, want reset", arm.LadderRungsFired, arm.LadderBaseShares)
	}
	if got := firedCount(notif); got != 1 {
		t.Errorf("fired notices = %d, want 1", got)
	}
}

// The ceiling path: the row was deleted before the sell — a sold-nothing sell
// reinserts it verbatim and keeps the feed subscription.
func TestCeilingFailedSell_ReinsertsArm(t *testing.T) {
	t.Parallel()
	m, store, notif, _, _ := restoreHarness(noLiquidityResult())
	arm := &database.SLTPArm{ID: 603, TelegramID: 95, TokenID: "RST3",
		AvgPrice: 0.16, SharesAtArm: 91.6, HighWaterMark: 0.87, TPArmed: true}
	store.seed(arm)

	m.fireCeilingTP(arm, 0.96)

	store.mu.Lock()
	reinserts := store.reinsertCalls
	rows := len(store.byToken["RST3"])
	var hwm float64
	if rows > 0 {
		hwm = store.byToken["RST3"][0].HighWaterMark
	}
	store.mu.Unlock()
	if reinserts != 1 || rows != 1 {
		t.Fatalf("reinsert calls = %d, rows = %d — want the ceiling-deleted row restored", reinserts, rows)
	}
	if hwm != 0.87 {
		t.Errorf("restored HWM = %.2f, want the pre-delete 0.87 (verbatim reinsert)", hwm)
	}
	if got := firedCount(notif); got != 1 {
		t.Errorf("fired notices = %d, want 1", got)
	}
}

// A concurrent manual re-arm between the ceiling's Disarm and the reinsert
// wins: ON CONFLICT DO NOTHING leaves the user's fresh arm untouched.
func TestCeilingFailedSell_YieldsToConcurrentRearm(t *testing.T) {
	t.Parallel()
	m, store, _, exec, _ := restoreHarness(nil)
	arm := &database.SLTPArm{ID: 604, TelegramID: 95, TokenID: "RST4",
		AvgPrice: 0.16, SharesAtArm: 91.6, HighWaterMark: 0.87, TPArmed: true}
	store.seed(arm)
	// The user re-arms between the Disarm and the failed sell's reinsert.
	exec.retFor = func(executorCall) *polymarket.TradeResult {
		store.seed(&database.SLTPArm{ID: 700, TelegramID: 95, TokenID: "RST4",
			AvgPrice: 0.50, SharesAtArm: 10, HighWaterMark: 0.50, TPArmed: true, SLArmed: true})
		return noLiquidityResult()
	}

	m.fireCeilingTP(arm, 0.96)

	store.mu.Lock()
	defer store.mu.Unlock()
	rows := store.byToken["RST4"]
	if len(rows) != 1 || rows[0].ID != 700 || rows[0].AvgPrice != 0.50 {
		t.Fatalf("rows = %+v — the concurrent manual re-arm must win over the reinsert", rows)
	}
}

// Repeated failures within one streak DM once; a successful sell resets the
// streak so the next failure notifies again.
func TestFailedSell_NotifiesOncePerStreak(t *testing.T) {
	t.Parallel()
	m, store, notif, exec, clock := restoreHarness(noLiquidityResult())
	arm := &database.SLTPArm{ID: 605, TelegramID: 95, TokenID: "RST5",
		AvgPrice: 0.16, SharesAtArm: 91.6, HighWaterMark: 0.16, TPArmed: true}
	store.seed(arm)

	m.fireTP(arm, 0.33)
	clock.advance(failedSellBackoff + time.Second)
	m.fireTP(arm, 0.33)
	if got := firedCount(notif); got != 1 {
		t.Fatalf("fired notices after two failures = %d, want 1 (once per streak)", got)
	}

	clock.advance(failedSellBackoff + time.Second)
	exec.mu.Lock()
	exec.ret = &polymarket.TradeResult{Success: true, OrderID: "ord-ok"}
	exec.mu.Unlock()
	m.fireTP(arm, 0.33)
	if got := firedCount(notif); got != 2 {
		t.Fatalf("fired notices after success = %d, want 2", got)
	}

	// New failure after the successful sell: a fresh streak notifies again.
	store.mu.Lock()
	store.byToken["RST5"][0].TPArmed = true
	store.mu.Unlock()
	exec.mu.Lock()
	exec.ret = noLiquidityResult()
	exec.mu.Unlock()
	m.fireTP(arm, 0.33)
	if got := firedCount(notif); got != 3 {
		t.Errorf("fired notices after fresh failure = %d, want 3 (streak reset by success)", got)
	}
}

// The failed-sell backoff must NOT gate the trailing stop (review F2): after a
// TP sold-nothing failure on a manual TP+SL arm, an SL exit attempt proceeds
// immediately.
func TestFailedSellBackoff_DoesNotBlockSL(t *testing.T) {
	t.Parallel()
	m, store, _, exec, _ := restoreHarness(noLiquidityResult())
	arm := &database.SLTPArm{ID: 606, TelegramID: 95, TokenID: "RST6",
		AvgPrice: 0.30, SharesAtArm: 100, HighWaterMark: 0.60, TPArmed: true, SLArmed: true}
	store.seed(arm)

	m.fireTP(arm, 0.60) // sold nothing → backoff armed
	exec.mu.Lock()
	tpAttempts := len(exec.calls)
	exec.ret = &polymarket.TradeResult{Success: true, OrderID: "ord-sl"}
	exec.mu.Unlock()

	// SL breach inside the backoff window: the stop must still sell.
	feedArm := store.byToken["RST6"][0]
	m.attemptSLExit(feedArm, 0.40, feedArm.SLTriggerPrice())
	waitFor(t, func() bool {
		exec.mu.Lock()
		defer exec.mu.Unlock()
		return len(exec.calls) > tpAttempts
	})
}

// The ceiling restore keeps the SAME arm identity across the reinsert (review
// F1: an id-keyed backoff would be structurally dead when the reinserted row
// changes id — the failure state is keyed by (chat, token), so a re-fire on
// the restored row is still inside the backoff regardless of its id).
func TestCeilingFailedSell_BackoffSurvivesReinsertId(t *testing.T) {
	t.Parallel()
	m, store, notif, exec, _ := restoreHarness(noLiquidityResult())
	arm := &database.SLTPArm{ID: 607, TelegramID: 95, TokenID: "RST7",
		AvgPrice: 0.16, SharesAtArm: 91.6, HighWaterMark: 0.87, TPArmed: true}
	store.seed(arm)

	m.fireCeilingTP(arm, 0.96)
	exec.mu.Lock()
	attempts := len(exec.calls)
	exec.mu.Unlock()

	// Simulate the production id churn: the restored row carries a NEW id.
	store.mu.Lock()
	store.byToken["RST7"][0].ID = 999
	restored := *store.byToken["RST7"][0]
	store.mu.Unlock()

	m.fireCeilingTP(&restored, 0.96)
	exec.mu.Lock()
	after := len(exec.calls)
	exec.mu.Unlock()
	if after != attempts {
		t.Fatalf("sell attempts %d→%d — the (chat,token)-keyed backoff must hold across an id change", attempts, after)
	}
	if got := firedCount(notif); got != 1 {
		t.Errorf("fired notices = %d, want 1 (no DM churn across the reinsert)", got)
	}
}
