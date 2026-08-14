package live

import (
	"testing"

	"github.com/Catorpilor/poly/internal/database"
	"github.com/Catorpilor/poly/internal/polymarket"
)

// TestSLTPMonitor_TPOnlyCoverage covers feat/auto-arm-full-coverage: a TP-only
// auto-arm (SLArmed=false) sizes fires from the CURRENT holding, not the fill
// snapshot; a manual TP+SL arm is byte-identical to today; a failed holdings
// read falls back to the snapshot; the reactive shortfall clamp still caps a
// shrunken wallet.
func TestSLTPMonitor_TPOnlyCoverage(t *testing.T) {
	t.Parallel()

	t.Run("TP-only TP fire sells fraction of CURRENT holding", func(t *testing.T) {
		t.Parallel()
		store := newFakeStore()
		store.seed(&database.SLTPArm{ID: 1, TelegramID: 5, TokenID: "T", AvgPrice: 0.20,
			SharesAtArm: 50, TPArmed: true, SLArmed: false})
		feed := newFakeFeed()
		exec := &fakeExecutor{}
		notif := &fakeNotifier{}
		m := NewSLTPMonitor(store, feed, exec, notif, nil)
		m.SetHoldingReader(&fakeHoldings{raw: map[string]int64{"T": 93_000_000}})
		_ = m.Start()

		feed.setBid("T", 0.41) // >= 2x
		feed.emit("T")
		waitFor(t, func() bool { exec.mu.Lock(); defer exec.mu.Unlock(); return len(exec.calls) == 1 })

		exec.mu.Lock()
		got := exec.calls[0].sharesRaw
		exec.mu.Unlock()
		if want := int64(93 * database.TPSellFraction * 1e6); got != want {
			t.Errorf("TP sell = %d, want %d (fraction of CURRENT 93, not snapshot 50)", got, want)
		}
	})

	t.Run("TP-only ceiling sells the full CURRENT holding", func(t *testing.T) {
		t.Parallel()
		store := newFakeStore()
		store.seed(&database.SLTPArm{ID: 2, TelegramID: 5, TokenID: "C", AvgPrice: 0.20,
			SharesAtArm: 50, TPArmed: true, SLArmed: false})
		feed := newFakeFeed()
		exec := &fakeExecutor{}
		notif := &fakeNotifier{}
		m := NewSLTPMonitor(store, feed, exec, notif, nil)
		m.SetHoldingReader(&fakeHoldings{raw: map[string]int64{"C": 93_000_000}})
		_ = m.Start()

		feed.setBid("C", database.CeilingTPPrice)
		feed.emit("C")
		waitFor(t, func() bool { exec.mu.Lock(); defer exec.mu.Unlock(); return len(exec.calls) == 1 })

		exec.mu.Lock()
		got := exec.calls[0].sharesRaw
		exec.mu.Unlock()
		if want := int64(93 * 1e6); got != want {
			t.Errorf("ceiling sell = %d, want %d (full CURRENT 93, not snapshot 50)", got, want)
		}
	})

	t.Run("manual TP+SL arm is byte-identical: snapshot, holdings never read", func(t *testing.T) {
		t.Parallel()
		store := newFakeStore()
		store.seed(&database.SLTPArm{ID: 3, TelegramID: 5, TokenID: "M", AvgPrice: 0.20,
			SharesAtArm: 100, TPArmed: true, SLArmed: true})
		feed := newFakeFeed()
		exec := &fakeExecutor{}
		notif := &fakeNotifier{}
		holds := &fakeHoldings{raw: map[string]int64{"M": 93_000_000}}
		m := NewSLTPMonitor(store, feed, exec, notif, nil)
		m.SetHoldingReader(holds)
		_ = m.Start()

		feed.setBid("M", 0.41)
		feed.emit("M")
		waitFor(t, func() bool { exec.mu.Lock(); defer exec.mu.Unlock(); return len(exec.calls) == 1 })

		exec.mu.Lock()
		got := exec.calls[0].sharesRaw
		exec.mu.Unlock()
		if want := int64(100 * database.TPSellFraction * 1e6); got != want {
			t.Errorf("manual TP sell = %d, want %d (frozen snapshot 100)", got, want)
		}
		holds.mu.Lock()
		calls := holds.calls
		holds.mu.Unlock()
		if calls != 0 {
			t.Errorf("holdings read %d times for a manual arm, want 0 (frozen snapshot)", calls)
		}
	})

	t.Run("holdings read failure falls back to snapshot", func(t *testing.T) {
		t.Parallel()
		store := newFakeStore()
		store.seed(&database.SLTPArm{ID: 4, TelegramID: 5, TokenID: "F", AvgPrice: 0.20,
			SharesAtArm: 50, TPArmed: true, SLArmed: false})
		feed := newFakeFeed()
		exec := &fakeExecutor{}
		notif := &fakeNotifier{}
		m := NewSLTPMonitor(store, feed, exec, notif, nil)
		m.SetHoldingReader(&fakeHoldings{fail: true})
		_ = m.Start()

		feed.setBid("F", 0.41)
		feed.emit("F")
		waitFor(t, func() bool { exec.mu.Lock(); defer exec.mu.Unlock(); return len(exec.calls) == 1 })

		exec.mu.Lock()
		got := exec.calls[0].sharesRaw
		exec.mu.Unlock()
		if want := int64(50 * database.TPSellFraction * 1e6); got != want {
			t.Errorf("fallback TP sell = %d, want %d (snapshot 50)", got, want)
		}
	})

	t.Run("shrunken wallet: coverage never shrinks below snapshot, clamp still wins", func(t *testing.T) {
		t.Parallel()
		store := newFakeStore()
		store.seed(&database.SLTPArm{ID: 5, TelegramID: 5, TokenID: "S", AvgPrice: 0.20,
			SharesAtArm: 100, TPArmed: true, SLArmed: false})
		feed := newFakeFeed()
		exec := &fakeExecutor{
			// First (over-)request is rejected with the true wallet balance; the
			// clamped retry succeeds.
			retFor: func(c executorCall) *polymarket.TradeResult {
				if c.sharesRaw > 20_000_000 {
					return &polymarket.TradeResult{Success: false, InsufficientBalance: true, AvailableSharesRaw: 20_000_000}
				}
				return &polymarket.TradeResult{Success: true, OrderID: "ord"}
			},
		}
		notif := &fakeNotifier{}
		m := NewSLTPMonitor(store, feed, exec, notif, nil)
		// current 20 < snapshot 100 → basis stays 100 (never shrinks); TP intends
		// 25, the reactive clamp caps the retry to the wallet's 20.
		m.SetHoldingReader(&fakeHoldings{raw: map[string]int64{"S": 20_000_000}})
		_ = m.Start()

		feed.setBid("S", 0.41)
		feed.emit("S")
		waitFor(t, func() bool { exec.mu.Lock(); defer exec.mu.Unlock(); return len(exec.calls) == 2 })

		exec.mu.Lock()
		first, second := exec.calls[0].sharesRaw, exec.calls[1].sharesRaw
		exec.mu.Unlock()
		if first != int64(100*database.TPSellFraction*1e6) {
			t.Errorf("first attempt = %d, want %d (25%% of snapshot 100, coverage never below snapshot)", first, int64(100*database.TPSellFraction*1e6))
		}
		if second != 20_000_000 {
			t.Errorf("clamped retry = %d, want 20000000 (wallet balance)", second)
		}
	})
}

// TestSLTPMonitor_SweepReconcilesCoverage covers the sweep reconciliation: a
// TP-only auto-arm whose wallet now holds more than the snapshot has
// SharesAtArm persisted upward; AvgPrice/HWM are untouched; manual arms and
// already-covered arms are left alone.
func TestSLTPMonitor_SweepReconcilesCoverage(t *testing.T) {
	t.Parallel()

	t.Run("TP-only under-covered arm reconciles up, thresholds untouched", func(t *testing.T) {
		t.Parallel()
		store := newFakeStore()
		store.seed(&database.SLTPArm{ID: 1, TelegramID: 5, TokenID: "T", ConditionID: "C1",
			AvgPrice: 0.20, HighWaterMark: 0.55, SharesAtArm: 50, TPArmed: true, SLArmed: false})
		checker := newFakeClosedChecker() // C1 open ⇒ kept
		m := sweeperMonitor(store, newFakeFeed(), &fakeNotifier{}, checker)
		m.SetHoldingReader(&fakeHoldings{raw: map[string]int64{"T": 93_000_000}})

		m.sweepClosedArms()

		if got := store.storedSharesAtArm("T"); got != 93 {
			t.Errorf("SharesAtArm = %v, want 93 (reconciled up)", got)
		}
		store.mu.Lock()
		arm := store.byToken["T"][0]
		store.mu.Unlock()
		if arm.AvgPrice != 0.20 || arm.HighWaterMark != 0.55 {
			t.Errorf("AvgPrice/HWM changed: %v/%v, want 0.20/0.55", arm.AvgPrice, arm.HighWaterMark)
		}
	})

	t.Run("manual TP+SL arm is never reconciled", func(t *testing.T) {
		t.Parallel()
		store := newFakeStore()
		store.seed(&database.SLTPArm{ID: 2, TelegramID: 5, TokenID: "M", ConditionID: "C1",
			AvgPrice: 0.20, SharesAtArm: 50, TPArmed: true, SLArmed: true})
		checker := newFakeClosedChecker()
		m := sweeperMonitor(store, newFakeFeed(), &fakeNotifier{}, checker)
		m.SetHoldingReader(&fakeHoldings{raw: map[string]int64{"M": 93_000_000}})

		m.sweepClosedArms()

		if got := store.storedSharesAtArm("M"); got != 50 {
			t.Errorf("manual SharesAtArm = %v, want 50 (frozen)", got)
		}
		store.mu.Lock()
		n := len(store.updateSharesCalls)
		store.mu.Unlock()
		if n != 0 {
			t.Errorf("UpdateSharesAtArm called %d times for a manual arm, want 0", n)
		}
	})

	t.Run("already-covered TP-only arm is not bumped", func(t *testing.T) {
		t.Parallel()
		store := newFakeStore()
		store.seed(&database.SLTPArm{ID: 3, TelegramID: 5, TokenID: "K", ConditionID: "C1",
			AvgPrice: 0.20, SharesAtArm: 93, TPArmed: true, SLArmed: false})
		checker := newFakeClosedChecker()
		m := sweeperMonitor(store, newFakeFeed(), &fakeNotifier{}, checker)
		m.SetHoldingReader(&fakeHoldings{raw: map[string]int64{"K": 50_000_000}}) // wallet smaller

		m.sweepClosedArms()

		if got := store.storedSharesAtArm("K"); got != 93 {
			t.Errorf("SharesAtArm = %v, want 93 (never shrinks)", got)
		}
	})
}
