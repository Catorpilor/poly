package live

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/Catorpilor/poly/internal/database"
	"github.com/Catorpilor/poly/internal/polymarket"
)

// fakeClosedChecker implements ClosedMarketChecker. Conditions without an
// entry in closed/errs behave like Gamma's live-verified negative: an empty
// response, surfaced as polymarket.ErrMarketNotFound.
type fakeClosedChecker struct {
	mu     sync.Mutex
	calls  []string
	closed map[string]*polymarket.GammaMarket
	errs   map[string]error
	// onLookup, when set, runs during a lookup (outside the lock) — used to
	// simulate a concurrent manual disarm between snapshot and sweep.
	onLookup func(conditionID string)
}

func newFakeClosedChecker() *fakeClosedChecker {
	return &fakeClosedChecker{
		closed: make(map[string]*polymarket.GammaMarket),
		errs:   make(map[string]error),
	}
}

func (c *fakeClosedChecker) GetClosedMarketByConditionID(_ context.Context, conditionID string) (*polymarket.GammaMarket, error) {
	c.mu.Lock()
	c.calls = append(c.calls, conditionID)
	err := c.errs[conditionID]
	market := c.closed[conditionID]
	hook := c.onLookup
	c.mu.Unlock()
	if hook != nil {
		hook(conditionID)
	}
	if err != nil {
		return nil, err
	}
	if market != nil {
		return market, nil
	}
	return nil, fmt.Errorf("%w for conditionId: %s", polymarket.ErrMarketNotFound, conditionID)
}

func (c *fakeClosedChecker) setClosed(conditionID string, closed bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closed[conditionID] = &polymarket.GammaMarket{ConditionID: conditionID, Closed: closed}
}

func (c *fakeClosedChecker) callCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.calls)
}

// sweeperMonitor builds a monitor with the checker wired, without Start —
// tests drive sweepClosedArms directly for determinism.
func sweeperMonitor(store *fakeStore, feed *fakeFeed, notif *fakeNotifier, checker ClosedMarketChecker) *SLTPMonitor {
	m := NewSLTPMonitor(store, feed, &fakeExecutor{}, notif, nil)
	m.SetClosedMarketChecker(checker)
	return m
}

func TestSLTPMonitor_Sweep_ClosedMarketDisarmsAllArms(t *testing.T) {
	t.Parallel()
	store := newFakeStore()
	// Two arms of one user on the same closed condition (both sides of a
	// binary market) — one lookup, both swept, ONE notification.
	store.seed(&database.SLTPArm{ID: 1, TelegramID: 5, TokenID: "A", ConditionID: "C1",
		Outcome: "YES", AvgPrice: 0.2, SharesAtArm: 100, TPArmed: true, SLArmed: true})
	store.seed(&database.SLTPArm{ID: 2, TelegramID: 5, TokenID: "B", ConditionID: "C1",
		Outcome: "NO", AvgPrice: 0.3, SharesAtArm: 50, TPArmed: true, SLArmed: true})

	feed := newFakeFeed()
	notif := &fakeNotifier{}
	checker := newFakeClosedChecker()
	checker.setClosed("C1", true)
	m := sweeperMonitor(store, feed, notif, checker)

	// Pre-stamp in-memory SL state for arm 1: the sweep must clear it.
	m.mu.Lock()
	m.slState[1] = &slArmState{breachStart: time.Unix(1, 0)}
	m.mu.Unlock()

	m.sweepClosedArms()

	if got := checker.callCount(); got != 1 {
		t.Errorf("condition looked up %d times, want exactly 1", got)
	}
	store.mu.Lock()
	disarms := store.disarmCalls
	store.mu.Unlock()
	if disarms != 2 {
		t.Errorf("expected exactly 2 disarms (one per arm), got %d", disarms)
	}
	if n := store.armedCount("A") + store.armedCount("B"); n != 0 {
		t.Errorf("expected all arms removed, %d remain", n)
	}
	if !slStateCleared(m, 1) {
		t.Error("in-memory SL state for arm 1 should be cleared")
	}

	notif.mu.Lock()
	swepts := append([]sweptNotice(nil), notif.swepts...)
	notif.mu.Unlock()
	if len(swepts) != 1 {
		t.Fatalf("expected ONE notification for the user, got %d: %+v", len(swepts), swepts)
	}
	if swepts[0].telegramID != 5 {
		t.Errorf("notified user %d, want 5", swepts[0].telegramID)
	}
	gotOutcomes := append([]string(nil), swepts[0].outcomes...)
	sort.Strings(gotOutcomes)
	if len(gotOutcomes) != 2 || gotOutcomes[0] != "NO" || gotOutcomes[1] != "YES" {
		t.Errorf("outcomes = %v, want [NO YES] grouped in one notice", gotOutcomes)
	}

	feed.mu.Lock()
	unsubs := append([]string(nil), feed.unsubscribes...)
	feed.mu.Unlock()
	sort.Strings(unsubs)
	if len(unsubs) != 2 || unsubs[0] != "A" || unsubs[1] != "B" {
		t.Errorf("unsubscribes = %v, want both A and B", unsubs)
	}
}

func TestSLTPMonitor_Sweep_OpenMarketKept(t *testing.T) {
	t.Parallel()
	store := newFakeStore()
	store.seed(&database.SLTPArm{ID: 10, TelegramID: 7, TokenID: "T", ConditionID: "C2",
		Outcome: "YES", AvgPrice: 0.4, SharesAtArm: 10, TPArmed: true, SLArmed: true})

	feed := newFakeFeed()
	notif := &fakeNotifier{}
	// No entry for C2: the checker answers not-found — the open/unresolved case.
	m := sweeperMonitor(store, feed, notif, newFakeClosedChecker())

	m.sweepClosedArms()

	store.mu.Lock()
	disarms := store.disarmCalls
	store.mu.Unlock()
	if disarms != 0 {
		t.Errorf("open market must not be disarmed, got %d disarms", disarms)
	}
	if store.armedCount("T") != 1 {
		t.Error("arm on open market should be untouched")
	}
	notif.mu.Lock()
	defer notif.mu.Unlock()
	if len(notif.swepts) != 0 {
		t.Errorf("no notification expected, got %+v", notif.swepts)
	}
	feed.mu.Lock()
	defer feed.mu.Unlock()
	if len(feed.unsubscribes) != 0 {
		t.Errorf("no unsubscribes expected, got %v", feed.unsubscribes)
	}
}

func TestSLTPMonitor_Sweep_CheckerErrorKeepsArm(t *testing.T) {
	t.Parallel()
	store := newFakeStore()
	store.seed(&database.SLTPArm{ID: 20, TelegramID: 8, TokenID: "E", ConditionID: "C3",
		Outcome: "NO", AvgPrice: 0.6, SharesAtArm: 5, TPArmed: true, SLArmed: true})

	notif := &fakeNotifier{}
	checker := newFakeClosedChecker()
	checker.mu.Lock()
	checker.errs["C3"] = errors.New("gamma 500")
	checker.mu.Unlock()
	m := sweeperMonitor(store, newFakeFeed(), notif, checker)

	m.sweepClosedArms()

	store.mu.Lock()
	disarms := store.disarmCalls
	store.mu.Unlock()
	if disarms != 0 || store.armedCount("E") != 1 {
		t.Errorf("lookup error must keep the arm (fail-safe): disarms=%d armed=%d",
			disarms, store.armedCount("E"))
	}
	notif.mu.Lock()
	defer notif.mu.Unlock()
	if len(notif.swepts) != 0 {
		t.Errorf("no notification expected on error, got %+v", notif.swepts)
	}
}

// TestSLTPMonitor_Sweep_MarketNotClosedKept guards the fail-safe belt: even if
// the closed-filtered lookup ever returns a market with Closed=false (filter
// regression), only positive closed:true evidence sweeps.
func TestSLTPMonitor_Sweep_MarketNotClosedKept(t *testing.T) {
	t.Parallel()
	store := newFakeStore()
	store.seed(&database.SLTPArm{ID: 30, TelegramID: 9, TokenID: "F", ConditionID: "C4",
		Outcome: "YES", AvgPrice: 0.5, SharesAtArm: 8, TPArmed: true, SLArmed: true})

	checker := newFakeClosedChecker()
	checker.setClosed("C4", false) // market returned but closed=false
	m := sweeperMonitor(store, newFakeFeed(), &fakeNotifier{}, checker)

	m.sweepClosedArms()

	if store.armedCount("F") != 1 {
		t.Error("market without closed=true must not be swept")
	}
}

func TestSLTPMonitor_Sweep_TwoUsersShareConditionEachNotified(t *testing.T) {
	t.Parallel()
	store := newFakeStore()
	// Two users armed on the same token/condition: one lookup, two disarms,
	// one notification per user.
	store.seed(&database.SLTPArm{ID: 41, TelegramID: 100, TokenID: "S", ConditionID: "C5",
		Outcome: "KNICKS", AvgPrice: 0.4, SharesAtArm: 10, TPArmed: true, SLArmed: true})
	store.seed(&database.SLTPArm{ID: 42, TelegramID: 200, TokenID: "S", ConditionID: "C5",
		Outcome: "KNICKS", AvgPrice: 0.5, SharesAtArm: 20, TPArmed: true, SLArmed: true})

	notif := &fakeNotifier{}
	checker := newFakeClosedChecker()
	checker.setClosed("C5", true)
	m := sweeperMonitor(store, newFakeFeed(), notif, checker)

	m.sweepClosedArms()

	if got := checker.callCount(); got != 1 {
		t.Errorf("condition looked up %d times, want exactly 1 despite two arms", got)
	}
	store.mu.Lock()
	disarms := store.disarmCalls
	store.mu.Unlock()
	if disarms != 2 {
		t.Errorf("expected 2 disarms, got %d", disarms)
	}

	notif.mu.Lock()
	swepts := append([]sweptNotice(nil), notif.swepts...)
	notif.mu.Unlock()
	if len(swepts) != 2 {
		t.Fatalf("expected one notification per user, got %d: %+v", len(swepts), swepts)
	}
	byUser := make(map[int64][]string)
	for _, s := range swepts {
		if _, dup := byUser[s.telegramID]; dup {
			t.Errorf("user %d notified more than once", s.telegramID)
		}
		byUser[s.telegramID] = s.outcomes
	}
	for _, user := range []int64{100, 200} {
		if outcomes := byUser[user]; len(outcomes) != 1 || outcomes[0] != "KNICKS" {
			t.Errorf("user %d outcomes = %v, want [KNICKS]", user, outcomes)
		}
	}
}

func TestSLTPMonitor_Sweep_MixedConditionsSweepsOnlyClosed(t *testing.T) {
	t.Parallel()
	store := newFakeStore()
	store.seed(&database.SLTPArm{ID: 51, TelegramID: 5, TokenID: "X", ConditionID: "CDONE",
		Outcome: "YES", AvgPrice: 0.2, SharesAtArm: 10, TPArmed: true, SLArmed: true})
	store.seed(&database.SLTPArm{ID: 52, TelegramID: 5, TokenID: "Y", ConditionID: "CLIVE",
		Outcome: "NO", AvgPrice: 0.3, SharesAtArm: 10, TPArmed: true, SLArmed: true})

	feed := newFakeFeed()
	notif := &fakeNotifier{}
	checker := newFakeClosedChecker()
	checker.setClosed("CDONE", true)
	m := sweeperMonitor(store, feed, notif, checker)

	m.sweepClosedArms()

	if store.armedCount("X") != 0 {
		t.Error("arm on closed condition should be swept")
	}
	if store.armedCount("Y") != 1 {
		t.Error("arm on live condition should be kept")
	}
	notif.mu.Lock()
	swepts := append([]sweptNotice(nil), notif.swepts...)
	notif.mu.Unlock()
	if len(swepts) != 1 || len(swepts[0].outcomes) != 1 || swepts[0].outcomes[0] != "YES" {
		t.Errorf("notification should cover only the swept arm, got %+v", swepts)
	}
	feed.mu.Lock()
	defer feed.mu.Unlock()
	for _, u := range feed.unsubscribes {
		if u == "Y" {
			t.Error("kept arm's token must not be unsubscribed")
		}
	}
}

// TestSLTPMonitor_Sweep_ToleratesArmAlreadyDisarmed simulates a manual disarm
// racing the sweep: the row vanishes between snapshot and Disarm, which then
// returns ErrSLTPArmNotFound — tolerated, the sweep still finishes its
// cleanup for that arm.
func TestSLTPMonitor_Sweep_ToleratesArmAlreadyDisarmed(t *testing.T) {
	t.Parallel()
	store := newFakeStore()
	store.seed(&database.SLTPArm{ID: 61, TelegramID: 6, TokenID: "G", ConditionID: "C6",
		Outcome: "YES", AvgPrice: 0.2, SharesAtArm: 10, TPArmed: true, SLArmed: true})

	notif := &fakeNotifier{}
	checker := newFakeClosedChecker()
	checker.setClosed("C6", true)
	checker.mu.Lock()
	checker.onLookup = func(string) {
		// The user taps Disarm while the sweep is mid-flight.
		_ = store.Disarm(context.Background(), 6, "G")
	}
	checker.mu.Unlock()
	m := sweeperMonitor(store, newFakeFeed(), notif, checker)

	m.sweepClosedArms()

	notif.mu.Lock()
	defer notif.mu.Unlock()
	if len(notif.swepts) != 1 {
		t.Errorf("not-found disarm should be tolerated as swept, got %+v", notif.swepts)
	}
}

// TestSLTPMonitor_Sweep_DisarmErrorKeepsArmForRetry: a transient DB failure on
// Disarm keeps the arm silent (no notification for it) so the next hourly
// sweep retries.
func TestSLTPMonitor_Sweep_DisarmErrorKeepsArmForRetry(t *testing.T) {
	t.Parallel()
	store := newFakeStore()
	store.seed(&database.SLTPArm{ID: 71, TelegramID: 7, TokenID: "H", ConditionID: "C7",
		Outcome: "NO", AvgPrice: 0.2, SharesAtArm: 10, TPArmed: true, SLArmed: true})
	store.mu.Lock()
	store.disarmFailN = 1
	store.mu.Unlock()

	notif := &fakeNotifier{}
	checker := newFakeClosedChecker()
	checker.setClosed("C7", true)
	m := sweeperMonitor(store, newFakeFeed(), notif, checker)

	m.sweepClosedArms()
	notif.mu.Lock()
	first := len(notif.swepts)
	notif.mu.Unlock()
	if first != 0 {
		t.Errorf("failed disarm must not be reported as swept, got %d notices", first)
	}
	if store.armedCount("H") != 1 {
		t.Error("arm should survive the failed disarm")
	}

	// Next sweep succeeds.
	m.sweepClosedArms()
	if store.armedCount("H") != 0 {
		t.Error("retry sweep should disarm the arm")
	}
	notif.mu.Lock()
	defer notif.mu.Unlock()
	if len(notif.swepts) != 1 {
		t.Errorf("retry sweep should notify once, got %d", len(notif.swepts))
	}
}

// TestSLTPMonitor_Sweep_LoopRunsFromStartAndStops wires the sweeper the way
// production does: Start launches it after sweepInitialDelay, it repeats on
// sweepInterval, and Stop ends it cleanly.
func TestSLTPMonitor_Sweep_LoopRunsFromStartAndStops(t *testing.T) {
	t.Parallel()
	store := newFakeStore()
	store.seed(&database.SLTPArm{ID: 81, TelegramID: 8, TokenID: "L", ConditionID: "C8",
		Outcome: "YES", AvgPrice: 0.2, SharesAtArm: 10, TPArmed: true, SLArmed: true})

	notif := &fakeNotifier{}
	checker := newFakeClosedChecker()
	checker.setClosed("C8", true)
	m := sweeperMonitor(store, newFakeFeed(), notif, checker)
	m.sweepInitialDelay = time.Millisecond
	m.sweepInterval = time.Millisecond
	if err := m.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer m.Stop()

	waitFor(t, func() bool { return store.armedCount("L") == 0 })
	waitFor(t, func() bool {
		notif.mu.Lock()
		defer notif.mu.Unlock()
		return len(notif.swepts) == 1
	})

	m.Stop()
	// After Stop, later intervals must not sweep again (nothing left anyway —
	// this mainly proves the goroutine exits without panicking or blocking).
	time.Sleep(10 * time.Millisecond)
	notif.mu.Lock()
	defer notif.mu.Unlock()
	if len(notif.swepts) != 1 {
		t.Errorf("expected exactly one sweep notice, got %d", len(notif.swepts))
	}
}

// TestSLTPMonitor_Sweep_RaceWithConcurrentEvals exercises the sweep against
// live evaluate() traffic under -race: both sides touch slState and the store.
func TestSLTPMonitor_Sweep_RaceWithConcurrentEvals(t *testing.T) {
	t.Parallel()
	store := newFakeStore()
	// Active trailing SL mid-breach so evaluations keep writing slState.
	store.seed(&database.SLTPArm{ID: 91, TelegramID: 9, TokenID: "R", ConditionID: "C9",
		Outcome: "YES", AvgPrice: 0.50, SharesAtArm: 10,
		HighWaterMark: 0.65, TPArmed: true, SLArmed: true})

	feed := newFakeFeed()
	notif := &fakeNotifier{}
	checker := newFakeClosedChecker()
	m := sweeperMonitor(store, feed, notif, checker)
	if err := m.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer m.Stop()

	feed.setBid("R", 0.45) // below trigger 0.52: stamps breach state, never confirms

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 100; i++ {
			feed.emit("R")
		}
	}()
	for i := 0; i < 20; i++ {
		m.sweepClosedArms() // not-found: keeps the arm while contending on state
	}
	checker.setClosed("C9", true)
	m.sweepClosedArms()
	wg.Wait()

	waitFor(t, func() bool { return store.armedCount("R") == 0 })
	notif.mu.Lock()
	defer notif.mu.Unlock()
	if len(notif.swepts) != 1 {
		t.Errorf("expected one sweep notice, got %d", len(notif.swepts))
	}
}
