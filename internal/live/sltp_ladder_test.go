package live

import (
	"context"
	"testing"
	"time"

	"github.com/Catorpilor/poly/internal/database"
	"github.com/Catorpilor/poly/internal/polymarket"
)

// deepArm builds a deep-entry (entry ≤ $0.05) arm on tokenID with a 0.01 tick,
// so its rungs land on 2×/3×/4×/5× = 0.10/0.15/0.20/0.25. slArmed picks the
// source shape: false = snipe/auto-arm (TP-only), true = manual TP+SL.
func deepArm(id int, telegramID int64, tokenID string, shares float64, slArmed bool) *database.SLTPArm {
	return &database.SLTPArm{
		ID: id, TelegramID: telegramID, TokenID: tokenID, AvgPrice: 0.05,
		SharesAtArm: shares, HighWaterMark: 0.05, TickSize: 0.01,
		TPArmed: true, SLArmed: slArmed,
	}
}

// TestSLTPMonitor_Ladder_SequentialRungsOnCommonBase walks 2×→3×→4×→5× one rung
// per tick and asserts each sells its own fraction of the SAME base frozen at
// the first fire (25/20/15/15 of 100), the fired count advances monotonically,
// and once all four have fired no further rung sells — the remainder rides.
func TestSLTPMonitor_Ladder_SequentialRungsOnCommonBase(t *testing.T) {
	t.Parallel()
	store := newFakeStore()
	store.seed(deepArm(1, 7, "D", 100, false))
	feed := newFakeFeed()
	exec := &fakeExecutor{}
	notif := &fakeNotifier{}
	m := NewSLTPMonitor(store, feed, exec, notif, nil)
	m.SetHoldingReader(&fakeHoldings{raw: map[string]int64{"D": 100_000_000}})
	_ = m.Start()

	// rung price -> expected combined sell (fraction of base 100)
	steps := []struct {
		bid   float64
		sells int64
		rungs int
	}{
		{0.10, 25_000_000, 1}, // 2×: 25%
		{0.15, 20_000_000, 2}, // 3×: 20%
		{0.20, 15_000_000, 3}, // 4×: 15%
		{0.25, 15_000_000, 4}, // 5×: 15%
	}
	for i, st := range steps {
		feed.setBid("D", st.bid)
		feed.emit("D")
		want := i + 1
		waitFor(t, func() bool { exec.mu.Lock(); defer exec.mu.Unlock(); return len(exec.calls) == want })
		exec.mu.Lock()
		got := exec.calls[i].sharesRaw
		exec.mu.Unlock()
		if got != st.sells {
			t.Errorf("rung %d (bid %.2f): sold %d, want %d", st.rungs, st.bid, got, st.sells)
		}
		if rungs, base := store.storedLadder("D"); rungs != st.rungs || base != 100 {
			t.Errorf("after rung %d: stored (%d,%v), want (%d,100)", st.rungs, rungs, base, st.rungs)
		}
	}

	// All four fired: a higher bid crosses no NEW rung, so nothing more sells —
	// the ~25% remainder rides to the ceiling.
	feed.setBid("D", 0.30)
	feed.emit("D")
	time.Sleep(50 * time.Millisecond)
	if n := exec.callCountTotal(); n != 4 {
		t.Errorf("post-ladder bid must not sell (remainder rides), got %d total sells", n)
	}
	// Total banked = 75% of base; every fire was the TP-ladder kind.
	notif.mu.Lock()
	defer notif.mu.Unlock()
	if len(notif.fires) != 4 {
		t.Fatalf("want 4 ladder fire notices, got %d", len(notif.fires))
	}
	for _, f := range notif.fires {
		if f.kind != "TP-ladder" {
			t.Errorf("fire kind = %q, want TP-ladder", f.kind)
		}
	}
}

// TestSLTPMonitor_Ladder_BaseIsFireTimeWholePosition proves the common base is
// the WHOLE position at the first fire (max of snapshot and current holding),
// and that a blended tranche added AFTER the first rung does NOT re-inflate the
// later rung fractions — they stay bound to the frozen base.
func TestSLTPMonitor_Ladder_BaseIsFireTimeWholePosition(t *testing.T) {
	t.Parallel()
	store := newFakeStore()
	// Fill snapshot 50, but the wallet already holds 100 (manual tranche blended
	// in): the base must be the whole 100, not the 50 snapshot.
	store.seed(deepArm(2, 7, "D", 50, false))
	feed := newFakeFeed()
	exec := &fakeExecutor{}
	notif := &fakeNotifier{}
	holds := &fakeHoldings{raw: map[string]int64{"D": 100_000_000}}
	m := NewSLTPMonitor(store, feed, exec, notif, nil)
	m.SetHoldingReader(holds)
	_ = m.Start()

	feed.setBid("D", 0.10) // rung 0
	feed.emit("D")
	waitFor(t, func() bool { exec.mu.Lock(); defer exec.mu.Unlock(); return len(exec.calls) == 1 })
	exec.mu.Lock()
	first := exec.calls[0].sharesRaw
	exec.mu.Unlock()
	if first != 25_000_000 { // 25% of the whole 100, not of the 50 snapshot
		t.Errorf("first rung sold %d, want 25000000 (25%% of whole 100)", first)
	}
	if _, base := store.storedLadder("D"); base != 100 {
		t.Errorf("frozen base = %v, want 100 (whole position at first fire)", base)
	}

	// Blend in another 50 shares AFTER the first rung; the second rung must still
	// size off the frozen 100 (20% = 20), never off the new 150.
	holds.mu.Lock()
	holds.raw["D"] = 150_000_000
	holds.mu.Unlock()
	feed.setBid("D", 0.15) // rung 1
	feed.emit("D")
	waitFor(t, func() bool { exec.mu.Lock(); defer exec.mu.Unlock(); return len(exec.calls) == 2 })
	exec.mu.Lock()
	second := exec.calls[1].sharesRaw
	exec.mu.Unlock()
	if second != 20_000_000 { // 20% of frozen 100, NOT 20% of 150
		t.Errorf("second rung sold %d, want 20000000 (frozen base 100, no re-inflation)", second)
	}
}

// TestSLTPMonitor_Ladder_GapTickCrossesMultipleRungs proves a single eval whose
// bid clears several rungs at once fires them in ONE combined sell (sum of the
// fractions) at the current book, not one sell per rung, and advances the count
// straight to the top crossed rung.
func TestSLTPMonitor_Ladder_GapTickCrossesMultipleRungs(t *testing.T) {
	t.Parallel()
	store := newFakeStore()
	store.seed(deepArm(3, 7, "D", 100, false))
	feed := newFakeFeed()
	exec := &fakeExecutor{}
	notif := &fakeNotifier{}
	m := NewSLTPMonitor(store, feed, exec, notif, nil)
	m.SetHoldingReader(&fakeHoldings{raw: map[string]int64{"D": 100_000_000}})
	_ = m.Start()

	// Gap straight to 0.20 crosses rungs 0,1,2 (2×/3×/4×): 25+20+15 = 60% in ONE
	// sell.
	feed.setBid("D", 0.20)
	feed.emit("D")
	waitFor(t, func() bool { exec.mu.Lock(); defer exec.mu.Unlock(); return len(exec.calls) == 1 })
	exec.mu.Lock()
	first := exec.calls[0].sharesRaw
	exec.mu.Unlock()
	if first != 60_000_000 {
		t.Errorf("gap sell = %d, want 60000000 (25+20+15%% of base 100 in one sell)", first)
	}
	if rungs, base := store.storedLadder("D"); rungs != 3 || base != 100 {
		t.Errorf("after gap: stored (%d,%v), want (3,100)", rungs, base)
	}

	// The final rung (5×) still fires on its own for the remaining 15%.
	feed.setBid("D", 0.25)
	feed.emit("D")
	waitFor(t, func() bool { exec.mu.Lock(); defer exec.mu.Unlock(); return len(exec.calls) == 2 })
	exec.mu.Lock()
	second := exec.calls[1].sharesRaw
	exec.mu.Unlock()
	if second != 15_000_000 {
		t.Errorf("final rung = %d, want 15000000", second)
	}
}

// TestSLTPMonitor_Ladder_NonDeepUsesStandardTP is the regression guard: an arm a
// hair above the 0.05 boundary keeps today's single 25% @ 2× partial + ceiling
// — fireTP clears tp_armed, and the ladder store method is never touched.
func TestSLTPMonitor_Ladder_NonDeepUsesStandardTP(t *testing.T) {
	t.Parallel()
	store := newFakeStore()
	// entry 0.0501 > 0.05 ⇒ not deep. 2× = 0.1002 → tick-floored to 0.10.
	store.seed(&database.SLTPArm{ID: 4, TelegramID: 7, TokenID: "N", AvgPrice: 0.0501,
		SharesAtArm: 100, HighWaterMark: 0.0501, TickSize: 0.01, TPArmed: true, SLArmed: false})
	feed := newFakeFeed()
	exec := &fakeExecutor{}
	notif := &fakeNotifier{}
	m := NewSLTPMonitor(store, feed, exec, notif, nil)
	_ = m.Start()

	feed.setBid("N", 0.20) // well past 2×
	feed.emit("N")
	waitFor(t, func() bool {
		notif.mu.Lock()
		defer notif.mu.Unlock()
		return len(notif.fires) == 1
	})
	notif.mu.Lock()
	kind := notif.fires[0].kind
	notif.mu.Unlock()
	if kind != "TP" {
		t.Errorf("non-deep arm fire kind = %q, want plain TP", kind)
	}
	// Standard path: exactly one 25% partial, tp_armed cleared, no ladder writes.
	if n := exec.callCountTotal(); n != 1 {
		t.Errorf("non-deep arm sells = %d, want 1 (single partial)", n)
	}
	store.mu.Lock()
	clears := store.clearTPCalls
	advances := len(store.advanceLadderCalls)
	store.mu.Unlock()
	if clears != 1 {
		t.Errorf("non-deep arm ClearTP calls = %d, want 1", clears)
	}
	if advances != 0 {
		t.Errorf("non-deep arm must never touch the ladder, got %d AdvanceLadder calls", advances)
	}
}

// TestSLTPMonitor_Ladder_SurvivesRestart proves the fired-rung count and frozen
// base persist across a monitor restart: monitor #2, built on the same store,
// resumes mid-ladder — it does NOT re-fire rungs #1 already sold, and its next
// rung sizes off the persisted base.
func TestSLTPMonitor_Ladder_SurvivesRestart(t *testing.T) {
	t.Parallel()
	store := newFakeStore()
	store.seed(deepArm(5, 7, "D", 100, false))

	// Monitor #1 fires rungs 0 and 1 (2× then 3×), then "crashes".
	feed1 := newFakeFeed()
	exec1 := &fakeExecutor{}
	m1 := NewSLTPMonitor(store, feed1, exec1, &fakeNotifier{}, nil)
	m1.SetHoldingReader(&fakeHoldings{raw: map[string]int64{"D": 100_000_000}})
	_ = m1.Start()
	feed1.setBid("D", 0.10)
	feed1.emit("D")
	waitFor(t, func() bool { exec1.mu.Lock(); defer exec1.mu.Unlock(); return len(exec1.calls) == 1 })
	feed1.setBid("D", 0.15)
	feed1.emit("D")
	waitFor(t, func() bool { exec1.mu.Lock(); defer exec1.mu.Unlock(); return len(exec1.calls) == 2 })
	m1.Stop()
	if rungs, base := store.storedLadder("D"); rungs != 2 || base != 100 {
		t.Fatalf("pre-restart stored (%d,%v), want (2,100)", rungs, base)
	}

	// Monitor #2 on the same store: a bid still at the 3× rung must NOT re-fire
	// rungs 0/1, and the 4× rung sizes off the persisted base.
	feed2 := newFakeFeed()
	exec2 := &fakeExecutor{}
	m2 := NewSLTPMonitor(store, feed2, exec2, &fakeNotifier{}, nil)
	m2.SetHoldingReader(&fakeHoldings{raw: map[string]int64{"D": 100_000_000}})
	_ = m2.Start()

	feed2.setBid("D", 0.15) // 3× again — already fired
	feed2.emit("D")
	time.Sleep(50 * time.Millisecond)
	if n := exec2.callCountTotal(); n != 0 {
		t.Errorf("restart must not re-fire already-sold rungs, got %d sells", n)
	}

	feed2.setBid("D", 0.20) // 4× — the next unfired rung
	feed2.emit("D")
	waitFor(t, func() bool { exec2.mu.Lock(); defer exec2.mu.Unlock(); return len(exec2.calls) == 1 })
	exec2.mu.Lock()
	got := exec2.calls[0].sharesRaw
	exec2.mu.Unlock()
	if got != 15_000_000 { // 15% of the persisted base 100
		t.Errorf("post-restart rung sold %d, want 15000000 (15%% of persisted base)", got)
	}
	if rungs, _ := store.storedLadder("D"); rungs != 3 {
		t.Errorf("post-restart fired count = %d, want 3", rungs)
	}
}

// TestSLTPMonitor_Ladder_ShortfallPreservesAccounting proves a balance shortfall
// on one rung's sell (clamped-retry, issue #24) does NOT corrupt the cumulative
// base accounting: the fired count still advances, the base stays frozen, and
// the next rung sizes off that same base.
func TestSLTPMonitor_Ladder_ShortfallPreservesAccounting(t *testing.T) {
	t.Parallel()
	store := newFakeStore()
	store.seed(deepArm(6, 7, "D", 100, false))
	feed := newFakeFeed()
	// The first rung intends 25e6; the wallet only holds 20e6 → shortfall, then
	// the clamped retry (and every other size) succeeds.
	exec := &fakeExecutor{retFor: func(c executorCall) *polymarket.TradeResult {
		if c.sharesRaw == 25_000_000 {
			return shortfallResult(20_000_000)
		}
		return &polymarket.TradeResult{Success: true, OrderID: "ok"}
	}}
	notif := &fakeNotifier{}
	m := NewSLTPMonitor(store, feed, exec, notif, nil)
	m.SetHoldingReader(&fakeHoldings{raw: map[string]int64{"D": 100_000_000}})
	_ = m.Start()

	feed.setBid("D", 0.10) // rung 0: 25e6 intended → shortfall → clamp to 20e6
	feed.emit("D")
	waitFor(t, func() bool { exec.mu.Lock(); defer exec.mu.Unlock(); return len(exec.calls) == 2 })
	exec.mu.Lock()
	intended, retry := exec.calls[0].sharesRaw, exec.calls[1].sharesRaw
	exec.mu.Unlock()
	if intended != 25_000_000 || retry != 20_000_000 {
		t.Errorf("rung 0 attempts = (%d,%d), want (25000000, 20000000 clamped)", intended, retry)
	}
	// Accounting intact despite the shortfall: fired advanced, base frozen at 100.
	if rungs, base := store.storedLadder("D"); rungs != 1 || base != 100 {
		t.Errorf("after shortfall: stored (%d,%v), want (1,100) — base uncorrupted", rungs, base)
	}
	// The arm must survive (a sellable shortfall clamps, never disarms).
	if store.armedCount("D") != 1 {
		t.Errorf("arm must survive a sellable shortfall, got %d armed", store.armedCount("D"))
	}

	// The NEXT rung sizes off the same frozen base (20% of 100 = 20e6).
	feed.setBid("D", 0.15)
	feed.emit("D")
	waitFor(t, func() bool { exec.mu.Lock(); defer exec.mu.Unlock(); return len(exec.calls) == 3 })
	exec.mu.Lock()
	third := exec.calls[2].sharesRaw
	exec.mu.Unlock()
	if third != 20_000_000 {
		t.Errorf("rung 1 after a rung-0 shortfall sold %d, want 20000000 (base still 100)", third)
	}
}

// TestSLTPMonitor_Ladder_DepthConfirmWrapsRungs proves the #80 depth confirm
// composes with every rung fire: a phantom high print (VWAP below the rung
// price) is refused (no sell, rung not consumed), a retry inside the cooldown is
// suppressed before any book/holdings I/O, and after the cooldown a genuine book
// fires the rung.
func TestSLTPMonitor_Ladder_DepthConfirmWrapsRungs(t *testing.T) {
	t.Parallel()
	store := newFakeStore()
	store.seed(deepArm(7, 7, "D", 100, false))
	feed := newFakeFeed()
	exec := &fakeExecutor{}
	notif := &fakeNotifier{}
	reader := newFakeBookReader()
	holdings := &fakeHoldings{raw: map[string]int64{"D": 100_000_000}}
	clock := newFakeClock()
	m := NewSLTPMonitor(store, feed, exec, notif, nil)
	m.now = clock.now
	m.SetHoldingReader(holdings)
	m.SetBookReader(reader)
	_ = m.Start()

	// Phantom: bid 0.10 crosses rung 0, but the real book VWAP 0.03 is far below
	// the 0.10 rung price → refuse.
	reader.setVWAP("D", 0.03, 1000, true)
	feed.setBid("D", 0.10)
	feed.emit("D")
	waitFor(t, func() bool { return depthCooldownActive(m, 7) })

	if exec.callCountTotal() != 0 {
		t.Fatalf("phantom rung must not sell, got %d", exec.callCountTotal())
	}
	if rungs, _ := store.storedLadder("D"); rungs != 0 {
		t.Errorf("refused rung must not advance the count, got %d", rungs)
	}
	if reader.callCount() != 1 || holdings.callCount() != 1 {
		t.Fatalf("first attempt reader/holdings = %d/%d, want 1/1", reader.callCount(), holdings.callCount())
	}

	// Retry INSIDE the cooldown: suppressed before the book fetch and holdings read.
	feed.emit("D")
	time.Sleep(50 * time.Millisecond)
	if reader.callCount() != 1 || holdings.callCount() != 1 {
		t.Errorf("cooldown must suppress I/O; reader/holdings = %d/%d, want 1/1", reader.callCount(), holdings.callCount())
	}
	if exec.callCountTotal() != 0 {
		t.Fatalf("still no sell during cooldown, got %d", exec.callCountTotal())
	}

	// After the cooldown a genuine book (VWAP ≥ rung price) fires the rung.
	clock.advance(5 * time.Second)
	reader.setVWAP("D", 0.11, 1000, true)
	feed.emit("D")
	waitFor(t, func() bool { exec.mu.Lock(); defer exec.mu.Unlock(); return len(exec.calls) == 1 })
	exec.mu.Lock()
	got := exec.calls[0].sharesRaw
	exec.mu.Unlock()
	if got != 25_000_000 {
		t.Errorf("post-cooldown rung sold %d, want 25000000", got)
	}
	if rungs, _ := store.storedLadder("D"); rungs != 1 {
		t.Errorf("genuine rung must advance the count to 1, got %d", rungs)
	}
}

// TestSLTPMonitor_Ladder_CrossedRungsFromExecutableVWAP is the F1 guard: the bid
// print only NOMINATES the candidate slice; the rungs ACTUALLY sold are derived
// from the executable VWAP the depth confirm computes, so a mid-crash overprint
// can never front-load the ladder past the study's model. entry 0.04 ⇒ rungs
// 0.08/0.12/0.16/0.20.
func TestSLTPMonitor_Ladder_CrossedRungsFromExecutableVWAP(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name      string
		bid       float64 // the (possibly phantom) print
		vwap      float64 // executable sell VWAP for the candidate slice
		wantSell  int64   // raw shares sold (fraction of base 100)
		wantRungs int     // fired count after the eval
	}{
		// Verifier's exhibit: print 0.16 nominates rungs 1-3, but the book only
		// clears rung 1 (VWAP 0.085 between rung 1 @0.08 and rung 2 @0.12) — sell
		// only rung 1's 25%.
		{"phantom gap sells only the genuinely-crossed rung", 0.16, 0.085, 25_000_000, 1},
		// Real gap: VWAP 0.17 clears rungs 1-3 → all three fire combined (60%).
		{"real gap fires all crossed rungs combined", 0.16, 0.17, 60_000_000, 3},
		// VWAP clears rungs 1-2 but not 3 → sell 45% (25+20).
		{"partial gap fires the cleared rungs only", 0.16, 0.13, 45_000_000, 2},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			store := newFakeStore()
			store.seed(&database.SLTPArm{ID: 1, TelegramID: 7, TokenID: "D", AvgPrice: 0.04,
				SharesAtArm: 100, HighWaterMark: 0.04, TickSize: 0.01, TPArmed: true, SLArmed: false})
			feed := newFakeFeed()
			exec := &fakeExecutor{}
			notif := &fakeNotifier{}
			reader := newFakeBookReader()
			reader.setVWAP("D", tc.vwap, 1_000_000, true)
			m := NewSLTPMonitor(store, feed, exec, notif, nil)
			m.SetBookReader(reader)
			_ = m.Start()

			feed.setBid("D", tc.bid)
			feed.emit("D")
			waitFor(t, func() bool { exec.mu.Lock(); defer exec.mu.Unlock(); return len(exec.calls) == 1 })
			exec.mu.Lock()
			got := exec.calls[0].sharesRaw
			exec.mu.Unlock()
			if got != tc.wantSell {
				t.Errorf("sold %d, want %d (rungs from executable VWAP, not the print)", got, tc.wantSell)
			}
			if rungs, _ := store.storedLadder("D"); rungs != tc.wantRungs {
				t.Errorf("advanced to %d rungs, want %d", rungs, tc.wantRungs)
			}
			// Exactly ONE fresh-book fetch, sized to the nominated slice.
			if reader.callCount() != 1 {
				t.Errorf("book fetches = %d, want 1", reader.callCount())
			}
		})
	}
}

// TestSLTPMonitor_Ladder_StaleCopyCASNoDoubleSell is the F2 guard: two evals with
// stale (pre-advance) row copies must not double-sell an overlapping slice. The
// compare-and-set advance lets the first eval win; the second aborts its fire,
// and the overlapping rung is sold exactly once.
func TestSLTPMonitor_Ladder_StaleCopyCASNoDoubleSell(t *testing.T) {
	t.Parallel()
	store := newFakeStore()
	store.seed(&database.SLTPArm{ID: 1, TelegramID: 7, TokenID: "D", AvgPrice: 0.04,
		SharesAtArm: 100, HighWaterMark: 0.04, TickSize: 0.01, TPArmed: true, SLArmed: false})
	feed := newFakeFeed()
	exec := &fakeExecutor{}
	notif := &fakeNotifier{}
	m := NewSLTPMonitor(store, feed, exec, notif, nil)
	// No Start: drive fireLadder directly with two independently-loaded (stale)
	// copies, both showing fired=0 as they would if dispatched before either wrote.
	copyA := &database.SLTPArm{ID: 1, TelegramID: 7, TokenID: "D", AvgPrice: 0.04,
		SharesAtArm: 100, HighWaterMark: 0.04, TickSize: 0.01, TPArmed: true, SLArmed: false}
	copyB := *copyA

	// Eval A crosses rung 1 (bid 0.08) and wins: advances 0→1, sells 25%.
	if !m.fireLadder(copyA, 0.08) {
		t.Fatal("eval A should consume the tick")
	}
	// Eval B still holds fired=0 and crosses rungs 1-2 (bid 0.12); its CAS on
	// fired=0 now misses (row is at 1), so it must sell NOTHING — not the wider
	// 45% slice that would re-cover rung 1.
	if !m.fireLadder(&copyB, 0.12) {
		t.Fatal("eval B should still consume the tick")
	}
	if n := exec.callCountTotal(); n != 1 {
		t.Fatalf("stale-copy CAS must sell rung 1 exactly once, got %d sells", n)
	}
	exec.mu.Lock()
	first := exec.calls[0].sharesRaw
	exec.mu.Unlock()
	if first != 25_000_000 {
		t.Errorf("only sell = %d, want 25000000 (rung 1 once)", first)
	}
	if rungs, _ := store.storedLadder("D"); rungs != 1 {
		t.Errorf("fired count = %d, want 1 (B's stale advance rejected)", rungs)
	}

	// A fresh eval (fired=1) fires rung 2 for the remaining 20% — total 45%, never 70%.
	fresh, _ := store.ListArmedByToken(context.Background(), "D")
	if !m.fireLadder(fresh[0], 0.12) {
		t.Fatal("fresh eval should consume the tick")
	}
	waitFor(t, func() bool { exec.mu.Lock(); defer exec.mu.Unlock(); return len(exec.calls) == 2 })
	exec.mu.Lock()
	second := exec.calls[1].sharesRaw
	exec.mu.Unlock()
	if second != 20_000_000 {
		t.Errorf("fresh rung 2 sell = %d, want 20000000", second)
	}
}

// TestFakeStore_AdvanceLadderCAS pins the compare-and-set semantics the SQL
// mirrors: an advance whose expected count no longer matches the row is rejected.
func TestFakeStore_AdvanceLadderCAS(t *testing.T) {
	t.Parallel()
	store := newFakeStore()
	store.seed(&database.SLTPArm{ID: 1, TelegramID: 7, TokenID: "D", AvgPrice: 0.04,
		SharesAtArm: 100, TPArmed: true})
	ctx := context.Background()
	if err := store.AdvanceLadder(ctx, 7, "D", 0, 1, 100); err != nil {
		t.Fatalf("first advance 0→1: %v", err)
	}
	// Stale expected=0 now misses (row is at 1).
	if err := store.AdvanceLadder(ctx, 7, "D", 0, 2, 100); err == nil {
		t.Error("stale-copy advance (expected 0, row at 1) must be rejected")
	}
	// Correct expected=1 succeeds.
	if err := store.AdvanceLadder(ctx, 7, "D", 1, 2, 100); err != nil {
		t.Fatalf("advance 1→2: %v", err)
	}
	if rungs, _ := store.storedLadder("D"); rungs != 2 {
		t.Errorf("final fired count = %d, want 2", rungs)
	}
}

// TestSLTPMonitor_Ladder_ManualArmKeepsTrailingStop proves a MANUAL deep arm
// (SLArmed=true) runs the ladder on the way up (a gap fires all four rungs at
// once) AND still lets the trailing stop protect the ~25% remainder when the bid
// later collapses — the two mechanisms compose.
func TestSLTPMonitor_Ladder_ManualArmKeepsTrailingStop(t *testing.T) {
	t.Parallel()
	store := newFakeStore()
	store.seed(deepArm(8, 7, "D", 100, true)) // manual TP+SL, entry 0.05
	feed := newFakeFeed()
	exec := &fakeExecutor{}
	notif := &fakeNotifier{}
	clock := newFakeClock()
	m := NewSLTPMonitor(store, feed, exec, notif, nil)
	m.now = clock.now
	_ = m.Start()

	// Rally to 0.30 (6× entry) fires all four rungs in one combined sell (75% of
	// 100) and ratchets the HWM → the trailing stop is now active.
	feed.setBid("D", 0.30)
	feed.emit("D")
	waitFor(t, func() bool { exec.mu.Lock(); defer exec.mu.Unlock(); return len(exec.calls) == 1 })
	exec.mu.Lock()
	ladderSell := exec.calls[0].sharesRaw
	exec.mu.Unlock()
	if ladderSell != 75_000_000 {
		t.Errorf("rally sell = %d, want 75000000 (all four rungs of base 100)", ladderSell)
	}
	if rungs, _ := store.storedLadder("D"); rungs != 4 {
		t.Errorf("all rungs should be fired, got %d", rungs)
	}

	// Collapse to 0.10: no NEW rung (all fired), so the ladder yields and the
	// trailing stop takes over. Trigger = max(0.05, 0.30×0.80=0.24) = 0.24; bid
	// 0.10 breaches. After the confirm window the SL sells the 25% remainder.
	feed.setBid("D", 0.10)
	feed.emit("D")
	waitFor(t, func() bool { return breachStamped(m, 8) })
	clock.advance(31 * time.Second)
	feed.emit("D")
	waitFor(t, func() bool {
		notif.mu.Lock()
		defer notif.mu.Unlock()
		return len(notif.fires) == 2
	})
	exec.mu.Lock()
	slCall := exec.calls[1]
	exec.mu.Unlock()
	if slCall.sharesRaw != 25_000_000 { // base 100 minus the 75% ladder = 25
		t.Errorf("SL sold %d, want 25000000 (the ladder remainder)", slCall.sharesRaw)
	}
	if slCall.orderType != polymarket.OrderTypeFOK {
		t.Errorf("SL exit order type = %v, want FOK", slCall.orderType)
	}
	notif.mu.Lock()
	kinds := []string{notif.fires[0].kind, notif.fires[1].kind}
	notif.mu.Unlock()
	if kinds[0] != "TP-ladder" || kinds[1] != "SL" {
		t.Errorf("fire kinds = %v, want [TP-ladder SL]", kinds)
	}
}
