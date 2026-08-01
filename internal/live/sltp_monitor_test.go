package live

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Catorpilor/poly/internal/database"
	"github.com/Catorpilor/poly/internal/database/repositories"
	"github.com/Catorpilor/poly/internal/polymarket"
)

// --- fakes ---

type fakeStore struct {
	mu      sync.Mutex
	byToken map[string][]*database.SLTPArm
	// clearTPCalls / disarmCalls count successful (non-idempotent) clears
	clearTPCalls int
	disarmCalls  int
	// updateHWMCalls records every UpdateHWM invocation's hwm argument.
	updateHWMCalls []float64
	// disarmFailN makes the next N Disarm calls fail with a generic error
	// (simulates a transient DB outage; not ErrSLTPArmNotFound).
	disarmFailN int
}

func newFakeStore() *fakeStore {
	return &fakeStore{byToken: make(map[string][]*database.SLTPArm)}
}

func (s *fakeStore) seed(arm *database.SLTPArm) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.byToken[arm.TokenID] = append(s.byToken[arm.TokenID], arm)
}

func (s *fakeStore) ListArmedTokenIDs(_ context.Context) ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, 0, len(s.byToken))
	for id, arms := range s.byToken {
		for _, a := range arms {
			if a.TPArmed || a.SLArmed {
				out = append(out, id)
				break
			}
		}
	}
	return out, nil
}

func (s *fakeStore) ListArmedByToken(_ context.Context, tokenID string) ([]*database.SLTPArm, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []*database.SLTPArm
	for _, a := range s.byToken[tokenID] {
		if a.TPArmed || a.SLArmed {
			// Return a copy to prevent tests mutating fake state.
			cp := *a
			out = append(out, &cp)
		}
	}
	return out, nil
}

func (s *fakeStore) ClearTP(_ context.Context, telegramID int64, tokenID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, a := range s.byToken[tokenID] {
		if a.TelegramID == telegramID && a.TPArmed {
			a.TPArmed = false
			s.clearTPCalls++
			return nil
		}
	}
	return repositories.ErrSLTPArmNotFound
}

func (s *fakeStore) Disarm(_ context.Context, telegramID int64, tokenID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.disarmFailN > 0 {
		s.disarmFailN--
		return errors.New("simulated disarm failure")
	}
	arms := s.byToken[tokenID]
	for i, a := range arms {
		if a.TelegramID == telegramID {
			s.byToken[tokenID] = append(arms[:i], arms[i+1:]...)
			s.disarmCalls++
			return nil
		}
	}
	return repositories.ErrSLTPArmNotFound
}

func (s *fakeStore) UpdateHWM(_ context.Context, telegramID int64, tokenID string, hwm float64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.updateHWMCalls = append(s.updateHWMCalls, hwm)
	for _, a := range s.byToken[tokenID] {
		// Mirrors the SQL monotonic guard: only ever raises, no-op otherwise.
		if a.TelegramID == telegramID && a.HighWaterMark < hwm {
			a.HighWaterMark = hwm
		}
	}
	return nil
}

// storedHWM returns the fake's high_water_mark for the first arm on tokenID.
func (s *fakeStore) storedHWM(tokenID string) float64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	if arms := s.byToken[tokenID]; len(arms) > 0 {
		return arms[0].HighWaterMark
	}
	return -1
}

// replace swaps the stored arm with the same ID (simulates a re-arm upsert
// that re-snapshots avg_price/shares and resets the HWM on the same row).
func (s *fakeStore) replace(arm *database.SLTPArm) {
	s.mu.Lock()
	defer s.mu.Unlock()
	arms := s.byToken[arm.TokenID]
	for i, a := range arms {
		if a.ID == arm.ID {
			arms[i] = arm
			return
		}
	}
	s.byToken[arm.TokenID] = append(arms, arm)
}

// armedCount returns how many armed rows remain for tokenID.
func (s *fakeStore) armedCount(tokenID string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, a := range s.byToken[tokenID] {
		if a.TPArmed || a.SLArmed {
			n++
		}
	}
	return n
}

type fakeFeed struct {
	mu   sync.Mutex
	bids map[string]float64
	asks map[string]float64
	// fallbackBids overrides the value returned by BidWithFallback when set.
	// The string is the source returned ("ws"/"http"); empty means "use bids".
	fallbackBids   map[string]float64
	fallbackSource map[string]string
	fallbackOK     map[string]bool
	fallbackCalls  []string
	subscribes     []string
	unsubscribes   []string
	listeners      []PriceUpdateListener
}

func newFakeFeed() *fakeFeed {
	return &fakeFeed{
		bids:           make(map[string]float64),
		asks:           make(map[string]float64),
		fallbackBids:   make(map[string]float64),
		fallbackSource: make(map[string]string),
		fallbackOK:     make(map[string]bool),
	}
}

func (f *fakeFeed) Subscribe(tokenID string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.subscribes = append(f.subscribes, tokenID)
}

func (f *fakeFeed) Unsubscribe(tokenID string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.unsubscribes = append(f.unsubscribes, tokenID)
}

func (f *fakeFeed) BestBid(tokenID string) (float64, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p, ok := f.bids[tokenID]
	return p, ok
}

func (f *fakeFeed) BestAsk(tokenID string) (float64, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p, ok := f.asks[tokenID]
	return p, ok
}

func (f *fakeFeed) BidWithFallback(tokenID string, _ time.Duration) (float64, string, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.fallbackCalls = append(f.fallbackCalls, tokenID)
	if p, isSet := f.fallbackBids[tokenID]; isSet {
		src := f.fallbackSource[tokenID]
		if src == "" {
			src = "http"
		}
		ok, hasOK := f.fallbackOK[tokenID]
		if !hasOK {
			ok = true
		}
		return p, src, ok
	}
	// Default: mirror the WS bid if available, source "ws".
	if p, ok := f.bids[tokenID]; ok {
		return p, "ws", true
	}
	return 0, "http", false
}

func (f *fakeFeed) OnUpdate(l PriceUpdateListener) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.listeners = append(f.listeners, l)
}

func (f *fakeFeed) setBid(tokenID string, price float64) {
	f.mu.Lock()
	f.bids[tokenID] = price
	f.mu.Unlock()
}

func (f *fakeFeed) setAsk(tokenID string, price float64) {
	f.mu.Lock()
	f.asks[tokenID] = price
	f.mu.Unlock()
}

// setFallbackBid forces BidWithFallback to return (price, source, ok) for
// tokenID regardless of the WS-cached bid. Used in ticker tests to simulate a
// silent WS where BestBid is stale but the HTTP fallback returns a fresh value.
func (f *fakeFeed) setFallbackBid(tokenID string, price float64, source string, ok bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.fallbackBids[tokenID] = price
	f.fallbackSource[tokenID] = source
	f.fallbackOK[tokenID] = ok
}

func (f *fakeFeed) emit(tokenID string) {
	f.mu.Lock()
	listeners := make([]PriceUpdateListener, len(f.listeners))
	copy(listeners, f.listeners)
	f.mu.Unlock()
	for _, l := range listeners {
		l(tokenID)
	}
}

type fakeExecutor struct {
	mu    sync.Mutex
	calls []executorCall
	ret   *polymarket.TradeResult
	// onSell, when set, is invoked (outside the lock) on every ExecuteSell —
	// used to observe ordering (e.g. disarm must not precede the sell).
	onSell func()

	// Lottery-related fields. When unset, ResolveOtherToken returns the
	// configured "<armToken>-other" stub and ExecuteLotteryBuy succeeds.
	otherTokenID string
	otherOutcome string
	resolveErr   error
	lotteryRet   *polymarket.TradeResult
	lotteryCalls []lotteryCall
}

type executorCall struct {
	armID      int
	sharesRaw  int64
	limitPrice float64
	orderType  polymarket.OrderType
}

type lotteryCall struct {
	armID        int
	otherTokenID string
	otherOutcome string
	maxSpend     float64
	maxPrice     float64
}

func (e *fakeExecutor) ExecuteSell(_ context.Context, arm *database.SLTPArm, sharesRaw int64,
	limitPrice float64, orderType polymarket.OrderType) *polymarket.TradeResult {
	e.mu.Lock()
	e.calls = append(e.calls, executorCall{
		armID: arm.ID, sharesRaw: sharesRaw, limitPrice: limitPrice, orderType: orderType,
	})
	ret := e.ret
	hook := e.onSell
	e.mu.Unlock()
	if hook != nil {
		hook()
	}
	if ret != nil {
		return ret
	}
	return &polymarket.TradeResult{Success: true, OrderID: "ord-stub"}
}

func (e *fakeExecutor) ResolveOtherToken(_ context.Context, arm *database.SLTPArm) (string, string, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.resolveErr != nil {
		return "", "", e.resolveErr
	}
	other := e.otherTokenID
	if other == "" {
		other = arm.TokenID + "-other"
	}
	outcome := e.otherOutcome
	if outcome == "" {
		outcome = "OTHER"
	}
	return other, outcome, nil
}

func (e *fakeExecutor) ExecuteLotteryBuy(_ context.Context, arm *database.SLTPArm,
	otherTokenID, otherOutcome string, maxSpend, maxPrice float64) *polymarket.TradeResult {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.lotteryCalls = append(e.lotteryCalls, lotteryCall{
		armID:        arm.ID,
		otherTokenID: otherTokenID,
		otherOutcome: otherOutcome,
		maxSpend:     maxSpend,
		maxPrice:     maxPrice,
	})
	if e.lotteryRet != nil {
		return e.lotteryRet
	}
	return &polymarket.TradeResult{Success: true, OrderID: "lot-stub", FilledSize: 100, AveragePrice: 0.04}
}

type fakeNotifier struct {
	mu        sync.Mutex
	fires     []fireNotice
	paused    []int64 // telegramIDs notified of pause
	lotteries []lotteryNotice
	pendings  []pendingNotice
	stales    []staleNotice
	swepts    []sweptNotice
}

type sweptNotice struct {
	telegramID int64
	outcomes   []string
}

type staleNotice struct {
	telegramID   int64
	armID        int
	availableRaw int64
}

type pendingNotice struct {
	telegramID int64
	armID      int
	bid        float64
	trigger    float64
	floor      float64
}

type lotteryNotice struct {
	telegramID    int64
	otherOutcome  string
	reason        string
	detail        string
	hasResult     bool
	resultSuccess bool
}

type fireNotice struct {
	telegramID int64
	kind       string
	bid        float64
	armID      int
}

func (n *fakeNotifier) NotifySLTPFired(telegramID int64, kind string, arm *database.SLTPArm, bid float64, _ *polymarket.TradeResult) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.fires = append(n.fires, fireNotice{telegramID, kind, bid, arm.ID})
}

func (n *fakeNotifier) NotifySLExitPending(telegramID int64, arm *database.SLTPArm, bid, trigger, floor float64) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.pendings = append(n.pendings, pendingNotice{telegramID, arm.ID, bid, trigger, floor})
}

func (n *fakeNotifier) NotifySLTPStaleSize(telegramID int64, arm *database.SLTPArm, availableRaw int64) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.stales = append(n.stales, staleNotice{telegramID, arm.ID, availableRaw})
}

func (n *fakeNotifier) NotifySLTPPaused(telegramID int64, _ *database.SLTPArm) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.paused = append(n.paused, telegramID)
}

func (n *fakeNotifier) NotifyArmsSwept(telegramID int64, outcomes []string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.swepts = append(n.swepts, sweptNotice{telegramID, append([]string(nil), outcomes...)})
}

func (n *fakeNotifier) NotifyLottery(telegramID int64, _ *database.SLTPArm, otherOutcome,
	reason, detail string, result *polymarket.TradeResult) {
	n.mu.Lock()
	defer n.mu.Unlock()
	notice := lotteryNotice{
		telegramID:   telegramID,
		otherOutcome: otherOutcome,
		reason:       reason,
		detail:       detail,
	}
	if result != nil {
		notice.hasResult = true
		notice.resultSuccess = result.Success
	}
	n.lotteries = append(n.lotteries, notice)
}

// fakeClock is a mutex-guarded manual clock wired into SLTPMonitor.now so
// debounce tests advance time deterministically instead of sleeping.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newFakeClock() *fakeClock {
	return &fakeClock{t: time.Unix(1_700_000_000, 0)}
}

func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

// breachStamped reports whether the monitor holds in-memory breach state for
// armID (white-box: same package).
func breachStamped(m *SLTPMonitor, armID int) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	st := m.slState[armID]
	return st != nil && !st.breachStart.IsZero()
}

// slStateCleared reports whether no in-memory SL state exists for armID.
func slStateCleared(m *SLTPMonitor, armID int) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.slState[armID] == nil
}

// waitFor polls cond until true or timeout; fails the test otherwise.
func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("condition never became true")
}

// --- tests ---

func TestSLTPMonitor_StartSeedsSubscriptions(t *testing.T) {
	t.Parallel()
	store := newFakeStore()
	store.seed(&database.SLTPArm{ID: 1, TelegramID: 1, TokenID: "A", AvgPrice: 0.2, SharesAtArm: 100, TPArmed: true, SLArmed: true})
	store.seed(&database.SLTPArm{ID: 2, TelegramID: 2, TokenID: "B", AvgPrice: 0.3, SharesAtArm: 50, TPArmed: true, SLArmed: true})

	feed := newFakeFeed()
	m := NewSLTPMonitor(store, feed, &fakeExecutor{}, &fakeNotifier{}, nil)
	if err := m.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if len(feed.subscribes) != 2 {
		t.Errorf("expected 2 subscribes, got %d: %v", len(feed.subscribes), feed.subscribes)
	}
	if len(feed.listeners) != 1 {
		t.Errorf("expected 1 listener registered, got %d", len(feed.listeners))
	}
}

func TestSLTPMonitor_TPFiresAt2x(t *testing.T) {
	t.Parallel()
	store := newFakeStore()
	arm := &database.SLTPArm{ID: 10, TelegramID: 5, TokenID: "T", AvgPrice: 0.20, SharesAtArm: 100, TPArmed: true, SLArmed: true}
	store.seed(arm)

	feed := newFakeFeed()
	exec := &fakeExecutor{}
	notif := &fakeNotifier{}
	m := NewSLTPMonitor(store, feed, exec, notif, nil)
	_ = m.Start()

	feed.setBid("T", 0.41) // >= 0.20*2 = 0.40
	feed.emit("T")

	waitFor(t, func() bool {
		notif.mu.Lock()
		defer notif.mu.Unlock()
		return len(notif.fires) == 1
	})

	exec.mu.Lock()
	if exec.calls[0].sharesRaw != int64(100*0.50*1e6) {
		t.Errorf("expected TP sell 50e6 shares, got %d", exec.calls[0].sharesRaw)
	}
	// TP execution is unchanged by the trailing-SL rework: market order (GTC).
	if exec.calls[0].limitPrice != 0 || exec.calls[0].orderType != polymarket.OrderTypeGTC {
		t.Errorf("expected TP sell as (0, GTC), got (%v, %v)", exec.calls[0].limitPrice, exec.calls[0].orderType)
	}
	exec.mu.Unlock()
	store.mu.Lock()
	clears := store.clearTPCalls
	store.mu.Unlock()
	if clears != 1 {
		t.Errorf("expected 1 clearTP, got %d", clears)
	}
	notif.mu.Lock()
	if len(notif.fires) != 1 || notif.fires[0].kind != "TP" {
		t.Errorf("expected 1 TP notification, got %+v", notif.fires)
	}
	notif.mu.Unlock()
	// SL should still be armed on the remainder
	store.mu.Lock()
	still := *store.byToken["T"][0] // copy to avoid holding the pointer across the unlock
	store.mu.Unlock()
	if still.TPArmed {
		t.Error("tp_armed should be false after fire")
	}
	if !still.SLArmed {
		t.Error("sl_armed should still be true")
	}
}

// TestSLTPMonitor_TPFiresOnTickGridBid is the issue #25 regression: entry
// 0.2355 doubles to 0.471, which a 0.01-tick book can never print. The bid
// peaked at exactly 0.4700 twice in production and the TP never fired. With
// the trigger floored to the arm's tick grid, bid 0.47 must fire.
func TestSLTPMonitor_TPFiresOnTickGridBid(t *testing.T) {
	t.Parallel()
	store := newFakeStore()
	store.seed(&database.SLTPArm{ID: 90, TelegramID: 71, TokenID: "G1", AvgPrice: 0.2355, SharesAtArm: 450,
		TickSize: 0.01, TPArmed: true, SLArmed: true})

	feed := newFakeFeed()
	exec := &fakeExecutor{}
	notif := &fakeNotifier{}
	m := NewSLTPMonitor(store, feed, exec, notif, nil)
	_ = m.Start()

	feed.setBid("G1", 0.47)
	feed.emit("G1")

	waitFor(t, func() bool {
		notif.mu.Lock()
		defer notif.mu.Unlock()
		return len(notif.fires) == 1
	})

	notif.mu.Lock()
	defer notif.mu.Unlock()
	if notif.fires[0].kind != "TP" {
		t.Errorf("expected TP fire at grid bid 0.47, got kind=%q", notif.fires[0].kind)
	}
}

func TestSLTPMonitor_SL_FiresOnConfirmedBreach(t *testing.T) {
	t.Parallel()
	store := newFakeStore()
	// Active trailing stop: avg=0.50, peak=0.65 → trigger = max(0.50, 0.52) = 0.52.
	arm := &database.SLTPArm{ID: 11, TelegramID: 7, TokenID: "U", AvgPrice: 0.50, SharesAtArm: 80,
		HighWaterMark: 0.65, TPArmed: true, SLArmed: true}
	store.seed(arm)

	feed := newFakeFeed()
	exec := &fakeExecutor{}
	notif := &fakeNotifier{}
	clock := newFakeClock()
	m := NewSLTPMonitor(store, feed, exec, notif, nil)
	m.now = clock.now
	_ = m.Start()

	feed.setBid("U", 0.45) // below trigger 0.52 — starts the confirmation window
	feed.emit("U")
	waitFor(t, func() bool { return breachStamped(m, 11) })

	clock.advance(31 * time.Second)
	feed.emit("U")

	waitFor(t, func() bool {
		notif.mu.Lock()
		defer notif.mu.Unlock()
		return len(notif.fires) == 1
	})

	exec.mu.Lock()
	call := exec.calls[0]
	exec.mu.Unlock()
	if call.sharesRaw != int64(80*1e6) {
		t.Errorf("expected SL sell 80e6 shares, got %d", call.sharesRaw)
	}
	wantFloor := arm.SLFloorPrice() // 0.52 * 0.90 = 0.468
	if diff := call.limitPrice - wantFloor; diff > 1e-9 || diff < -1e-9 {
		t.Errorf("expected FOK floor %v, got %v", wantFloor, call.limitPrice)
	}
	if call.orderType != polymarket.OrderTypeFOK {
		t.Errorf("expected FOK order, got %v", call.orderType)
	}
	store.mu.Lock()
	disarms := store.disarmCalls
	store.mu.Unlock()
	if disarms != 1 {
		t.Errorf("expected 1 disarm, got %d", disarms)
	}
	notif.mu.Lock()
	kind := notif.fires[0].kind
	notif.mu.Unlock()
	if kind != "SL" {
		t.Errorf("expected SL notification, got %s", kind)
	}
	// Feed should be unsubscribed since no other armed rows on token U
	waitFor(t, func() bool {
		feed.mu.Lock()
		defer feed.mu.Unlock()
		return len(feed.unsubscribes) == 1 && feed.unsubscribes[0] == "U"
	})
}

func TestSLTPMonitor_SLAfterTPSellsHalf(t *testing.T) {
	t.Parallel()
	store := newFakeStore()
	// TP already fired previously: TPArmed=false, SLArmed=true. Peak 0.30 makes
	// the trailing stop active: trigger = max(0.20, 0.24) = 0.24.
	arm := &database.SLTPArm{ID: 12, TelegramID: 9, TokenID: "V", AvgPrice: 0.20, SharesAtArm: 100,
		HighWaterMark: 0.30, TPArmed: false, SLArmed: true}
	store.seed(arm)

	feed := newFakeFeed()
	exec := &fakeExecutor{}
	notif := &fakeNotifier{}
	clock := newFakeClock()
	m := NewSLTPMonitor(store, feed, exec, notif, nil)
	m.now = clock.now
	_ = m.Start()

	feed.setBid("V", 0.20) // below trigger 0.24
	feed.emit("V")
	waitFor(t, func() bool { return breachStamped(m, 12) })

	clock.advance(31 * time.Second)
	feed.emit("V")

	waitFor(t, func() bool {
		exec.mu.Lock()
		defer exec.mu.Unlock()
		return len(exec.calls) == 1
	})

	exec.mu.Lock()
	shares := exec.calls[0].sharesRaw
	exec.mu.Unlock()
	// TP already fired: SL sells remaining 50% of original snapshot
	if shares != int64(100*0.50*1e6) {
		t.Errorf("expected SL sell 50e6 (half remainder), got %d", shares)
	}
}

func TestSLTPMonitor_DoesNotFireBelowThreshold(t *testing.T) {
	t.Parallel()
	store := newFakeStore()
	store.seed(&database.SLTPArm{ID: 1, TelegramID: 1, TokenID: "T", AvgPrice: 0.20, SharesAtArm: 100, TPArmed: true, SLArmed: true})
	feed := newFakeFeed()
	exec := &fakeExecutor{}
	m := NewSLTPMonitor(store, feed, exec, &fakeNotifier{}, nil)
	_ = m.Start()

	feed.setBid("T", 0.30) // between SL (0.14) and TP (0.40); should not fire
	feed.emit("T")

	time.Sleep(50 * time.Millisecond) // let any goroutines settle
	exec.mu.Lock()
	defer exec.mu.Unlock()
	if len(exec.calls) != 0 {
		t.Errorf("expected no fires, got %d", len(exec.calls))
	}
}

func TestSLTPMonitor_RespectsPauseWindow(t *testing.T) {
	t.Parallel()
	store := newFakeStore()
	store.seed(&database.SLTPArm{ID: 1, TelegramID: 1, TokenID: "T", AvgPrice: 0.20, SharesAtArm: 100, TPArmed: true, SLArmed: true})
	feed := newFakeFeed()
	exec := &fakeExecutor{}
	notif := &fakeNotifier{}
	pause := func(now time.Time) bool { return true }
	m := NewSLTPMonitor(store, feed, exec, notif, pause)
	_ = m.Start()

	feed.setBid("T", 0.50) // well above TP
	feed.emit("T")

	waitFor(t, func() bool {
		notif.mu.Lock()
		defer notif.mu.Unlock()
		return len(notif.paused) == 1
	})

	exec.mu.Lock()
	if len(exec.calls) != 0 {
		t.Errorf("pause window must block fires, got %d calls", len(exec.calls))
	}
	exec.mu.Unlock()
}

func TestSLTPMonitor_PauseNotifiesOncePerUser(t *testing.T) {
	t.Parallel()
	store := newFakeStore()
	store.seed(&database.SLTPArm{ID: 1, TelegramID: 42, TokenID: "T", AvgPrice: 0.20, SharesAtArm: 100, TPArmed: true, SLArmed: true})
	feed := newFakeFeed()
	notif := &fakeNotifier{}
	m := NewSLTPMonitor(store, feed, &fakeExecutor{}, notif, func(time.Time) bool { return true })
	_ = m.Start()

	feed.setBid("T", 0.99)

	for i := 0; i < 10; i++ {
		feed.emit("T")
	}

	waitFor(t, func() bool {
		notif.mu.Lock()
		defer notif.mu.Unlock()
		return len(notif.paused) >= 1
	})
	time.Sleep(100 * time.Millisecond)

	notif.mu.Lock()
	defer notif.mu.Unlock()
	if len(notif.paused) != 1 {
		t.Errorf("expected exactly one pause notice, got %d", len(notif.paused))
	}
	if notif.paused[0] != 42 {
		t.Errorf("expected notice for user 42, got %d", notif.paused[0])
	}
}

func TestV2CutoverPause(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		now    time.Time
		paused bool
	}{
		{"before window", time.Date(2026, 4, 28, 10, 29, 59, 0, time.UTC), false},
		{"exact start is paused", V2CutoverStart, true},
		{"middle of window", time.Date(2026, 4, 28, 11, 15, 0, 0, time.UTC), true},
		{"end is not paused", V2CutoverEnd, false},
		{"after window", time.Date(2026, 4, 28, 12, 1, 0, 0, time.UTC), false},
		{"next day", time.Date(2026, 4, 29, 11, 0, 0, 0, time.UTC), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := V2CutoverPause(tt.now); got != tt.paused {
				t.Errorf("V2CutoverPause(%v) = %v, want %v", tt.now, got, tt.paused)
			}
		})
	}
}

func TestSLTPMonitor_ConcurrentUpdatesFireOnce(t *testing.T) {
	t.Parallel()
	store := newFakeStore()
	store.seed(&database.SLTPArm{ID: 20, TelegramID: 3, TokenID: "C", AvgPrice: 0.20, SharesAtArm: 200, TPArmed: true, SLArmed: true})

	feed := newFakeFeed()
	exec := &fakeExecutor{}
	m := NewSLTPMonitor(store, feed, exec, &fakeNotifier{}, nil)
	_ = m.Start()

	feed.setBid("C", 0.42)

	// Emit 20 concurrent updates — the ClearTP race guard should serialize to a single fire
	var wg sync.WaitGroup
	var emitted int32
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			feed.emit("C")
			atomic.AddInt32(&emitted, 1)
		}()
	}
	wg.Wait()

	// Give evaluation goroutines time to finish
	waitFor(t, func() bool {
		exec.mu.Lock()
		defer exec.mu.Unlock()
		return len(exec.calls) >= 1
	})
	time.Sleep(100 * time.Millisecond)

	exec.mu.Lock()
	defer exec.mu.Unlock()
	if len(exec.calls) != 1 {
		t.Errorf("expected exactly 1 fire under concurrent updates, got %d", len(exec.calls))
	}
}

// --- ticker tests (sltp-silent-feed-fix) ---

// fastTickMonitor builds a monitor with a near-instant tick interval so tests
// don't have to wait. Tests must call Start() to launch the tick loop.
func fastTickMonitor(store SLTPArmStore, feed PriceFeedSubscriber, exec TradeExecutor, notif Notifier, paused PauseWindow) *SLTPMonitor {
	m := NewSLTPMonitor(store, feed, exec, notif, paused)
	m.tickInterval = 5 * time.Millisecond
	return m
}

func TestSLTPMonitor_Tick_FiresSLWhenWSBidIsStale(t *testing.T) {
	t.Parallel()
	store := newFakeStore()
	// Active trailing stop: avg=0.10, peak=0.15 → trigger = max(0.10, 0.12) = 0.12.
	store.seed(&database.SLTPArm{ID: 30, TelegramID: 9, TokenID: "L", AvgPrice: 0.10, SharesAtArm: 900,
		HighWaterMark: 0.15, TPArmed: true, SLArmed: true})

	feed := newFakeFeed()
	// Stale WS bid would not have triggered SL (above trigger), but the
	// fallback returns the live HTTP value below trigger. The periodic tick
	// both stamps the breach and (after the confirm window) fires it.
	feed.setBid("L", 0.13)
	feed.setFallbackBid("L", 0.06, "http", true)

	exec := &fakeExecutor{}
	notif := &fakeNotifier{}
	clock := newFakeClock()
	m := fastTickMonitor(store, feed, exec, notif, nil)
	m.now = clock.now
	defer m.Stop()
	_ = m.Start()

	waitFor(t, func() bool { return breachStamped(m, 30) })
	clock.advance(31 * time.Second)

	waitFor(t, func() bool {
		notif.mu.Lock()
		defer notif.mu.Unlock()
		return len(notif.fires) == 1
	})

	notif.mu.Lock()
	defer notif.mu.Unlock()
	if notif.fires[0].kind != "SL" {
		t.Errorf("expected SL fire, got kind=%s", notif.fires[0].kind)
	}
	if notif.fires[0].bid != 0.06 {
		t.Errorf("expected fire bid=0.06 (from HTTP), got %v", notif.fires[0].bid)
	}
}

func TestSLTPMonitor_Tick_FiresTPWhenWSBidIsStale(t *testing.T) {
	t.Parallel()
	store := newFakeStore()
	// TP threshold = 0.10 * 2.0 = 0.20
	store.seed(&database.SLTPArm{ID: 31, TelegramID: 11, TokenID: "M", AvgPrice: 0.10, SharesAtArm: 500, TPArmed: true, SLArmed: true})

	feed := newFakeFeed()
	feed.setBid("M", 0.15) // stale, below TP
	feed.setFallbackBid("M", 0.25, "http", true)

	exec := &fakeExecutor{}
	notif := &fakeNotifier{}
	m := fastTickMonitor(store, feed, exec, notif, nil)
	defer m.Stop()
	_ = m.Start()

	waitFor(t, func() bool {
		notif.mu.Lock()
		defer notif.mu.Unlock()
		return len(notif.fires) == 1
	})

	notif.mu.Lock()
	defer notif.mu.Unlock()
	if notif.fires[0].kind != "TP" {
		t.Errorf("expected TP fire, got kind=%s", notif.fires[0].kind)
	}
}

func TestSLTPMonitor_Tick_NoFireBetweenThresholds(t *testing.T) {
	t.Parallel()
	store := newFakeStore()
	// TP=0.40, SL=0.14
	store.seed(&database.SLTPArm{ID: 32, TelegramID: 12, TokenID: "N", AvgPrice: 0.20, SharesAtArm: 100, TPArmed: true, SLArmed: true})

	feed := newFakeFeed()
	feed.setFallbackBid("N", 0.25, "http", true) // safe band

	exec := &fakeExecutor{}
	notif := &fakeNotifier{}
	m := fastTickMonitor(store, feed, exec, notif, nil)
	defer m.Stop()
	_ = m.Start()

	// Allow several ticks to fire.
	time.Sleep(50 * time.Millisecond)

	exec.mu.Lock()
	defer exec.mu.Unlock()
	if len(exec.calls) != 0 {
		t.Errorf("expected no fires for bid in safe band, got %d", len(exec.calls))
	}
	notif.mu.Lock()
	defer notif.mu.Unlock()
	if len(notif.fires) != 0 {
		t.Errorf("expected no notifications, got %d", len(notif.fires))
	}
}

func TestSLTPMonitor_Tick_NoFireWhenFallbackNotOK(t *testing.T) {
	t.Parallel()
	store := newFakeStore()
	store.seed(&database.SLTPArm{ID: 33, TelegramID: 13, TokenID: "O", AvgPrice: 0.10, SharesAtArm: 100, TPArmed: false, SLArmed: true})

	feed := newFakeFeed()
	// HTTP fetcher errored: ok=false. Don't fire even though SL would otherwise trip.
	feed.setFallbackBid("O", 0, "http", false)

	exec := &fakeExecutor{}
	notif := &fakeNotifier{}
	m := fastTickMonitor(store, feed, exec, notif, nil)
	defer m.Stop()
	_ = m.Start()

	time.Sleep(50 * time.Millisecond)

	exec.mu.Lock()
	defer exec.mu.Unlock()
	if len(exec.calls) != 0 {
		t.Errorf("expected no fires when fallback returns ok=false, got %d", len(exec.calls))
	}
}

func TestSLTPMonitor_Tick_RespectsPauseWindow(t *testing.T) {
	t.Parallel()
	store := newFakeStore()
	store.seed(&database.SLTPArm{ID: 34, TelegramID: 14, TokenID: "P", AvgPrice: 0.10, SharesAtArm: 100, TPArmed: true, SLArmed: true})

	feed := newFakeFeed()
	feed.setFallbackBid("P", 0.06, "http", true) // would trip SL

	exec := &fakeExecutor{}
	notif := &fakeNotifier{}
	always := func(time.Time) bool { return true }
	m := fastTickMonitor(store, feed, exec, notif, always)
	defer m.Stop()
	_ = m.Start()

	// Pause notifier should be invoked at least once for this user.
	waitFor(t, func() bool {
		notif.mu.Lock()
		defer notif.mu.Unlock()
		return len(notif.paused) >= 1
	})

	// And the executor must NOT have been called.
	exec.mu.Lock()
	defer exec.mu.Unlock()
	if len(exec.calls) != 0 {
		t.Errorf("expected no fires while paused, got %d", len(exec.calls))
	}

	// The pause notification should be deduped per user (at most one).
	notif.mu.Lock()
	defer notif.mu.Unlock()
	count := 0
	for _, id := range notif.paused {
		if id == 14 {
			count++
		}
	}
	if count != 1 {
		t.Errorf("expected exactly 1 pause notice for user 14, got %d", count)
	}
}

func TestSLTPMonitor_Tick_StopsCleanlyOnContextCancel(t *testing.T) {
	t.Parallel()
	store := newFakeStore()
	store.seed(&database.SLTPArm{ID: 35, TelegramID: 15, TokenID: "Q", AvgPrice: 0.10, SharesAtArm: 100, TPArmed: true, SLArmed: true})

	feed := newFakeFeed()
	feed.setFallbackBid("Q", 0.15, "http", true) // safe band, no fire

	exec := &fakeExecutor{}
	m := fastTickMonitor(store, feed, exec, &fakeNotifier{}, nil)
	_ = m.Start()
	// Let a few ticks happen, then Stop. If tickLoop leaks, race detector / -count
	// runs would catch it.
	time.Sleep(20 * time.Millisecond)
	m.Stop()
	// Sleep past several would-be tick deadlines; nothing should panic or race.
	time.Sleep(20 * time.Millisecond)
}

// --- ceiling-TP tests ---

// TestSLTPMonitor_CeilingTP_FullSnapshotWhenTPNotYetFired verifies that when
// the bid hits CeilingTPPrice before standard TP has triggered, ALL of the
// snapshot shares are sold and the arm is fully disarmed.
func TestSLTPMonitor_CeilingTP_FullSnapshotWhenTPNotYetFired(t *testing.T) {
	t.Parallel()
	store := newFakeStore()
	// avg=0.30, so 2x TP would be 0.60 (well below ceiling).
	arm := &database.SLTPArm{
		ID: 40, TelegramID: 21, TokenID: "C1", AvgPrice: 0.30, SharesAtArm: 200,
		TPArmed: true, SLArmed: true,
	}
	store.seed(arm)

	feed := newFakeFeed()
	exec := &fakeExecutor{}
	notif := &fakeNotifier{}
	m := NewSLTPMonitor(store, feed, exec, notif, nil)
	_ = m.Start()

	feed.setBid("C1", database.CeilingTPPrice)
	feed.emit("C1")

	waitFor(t, func() bool {
		notif.mu.Lock()
		defer notif.mu.Unlock()
		return len(notif.fires) == 1
	})

	exec.mu.Lock()
	shares := exec.calls[0].sharesRaw
	exec.mu.Unlock()
	if shares != int64(200*1e6) {
		t.Errorf("expected ceiling sell 200e6 shares (full snapshot), got %d", shares)
	}

	notif.mu.Lock()
	kind := notif.fires[0].kind
	notif.mu.Unlock()
	if kind != "TP-ceiling" {
		t.Errorf("expected TP-ceiling notification kind, got %q", kind)
	}

	store.mu.Lock()
	disarms := store.disarmCalls
	clears := store.clearTPCalls
	store.mu.Unlock()
	if disarms != 1 {
		t.Errorf("expected 1 disarm (ceiling fires full Disarm), got %d", disarms)
	}
	if clears != 0 {
		t.Errorf("expected 0 ClearTP (ceiling supersedes 2x TP), got %d", clears)
	}
}

// TestSLTPMonitor_CeilingTP_RemainingHalfWhenTPAlreadyFired verifies that when
// standard TP already fired (TPArmed=false), the ceiling sells the remaining
// 50% of the original snapshot.
func TestSLTPMonitor_CeilingTP_RemainingHalfWhenTPAlreadyFired(t *testing.T) {
	t.Parallel()
	store := newFakeStore()
	// avg=0.20, so 2x TP would have already fired at 0.40 (well below ceiling).
	// TPArmed=false reflects post-TP state; SL still watching the remainder.
	arm := &database.SLTPArm{
		ID: 41, TelegramID: 22, TokenID: "C2", AvgPrice: 0.20, SharesAtArm: 300,
		TPArmed: false, SLArmed: true,
	}
	store.seed(arm)

	feed := newFakeFeed()
	exec := &fakeExecutor{}
	notif := &fakeNotifier{}
	m := NewSLTPMonitor(store, feed, exec, notif, nil)
	_ = m.Start()

	feed.setBid("C2", 0.97)
	feed.emit("C2")

	waitFor(t, func() bool {
		exec.mu.Lock()
		defer exec.mu.Unlock()
		return len(exec.calls) == 1
	})

	exec.mu.Lock()
	shares := exec.calls[0].sharesRaw
	exec.mu.Unlock()
	if shares != int64(300*0.50*1e6) {
		t.Errorf("expected ceiling sell 150e6 shares (remaining half), got %d", shares)
	}

	notif.mu.Lock()
	kind := notif.fires[0].kind
	notif.mu.Unlock()
	if kind != "TP-ceiling" {
		t.Errorf("expected TP-ceiling notification kind, got %q", kind)
	}
}

// TestSLTPMonitor_CeilingTP_DoesNotFireBelowThreshold verifies that bids
// strictly less than CeilingTPPrice don't trigger the ceiling — only the 2x
// rule applies in that range.
func TestSLTPMonitor_CeilingTP_DoesNotFireBelowThreshold(t *testing.T) {
	t.Parallel()
	store := newFakeStore()
	arm := &database.SLTPArm{
		ID: 42, TelegramID: 23, TokenID: "C3", AvgPrice: 0.30, SharesAtArm: 100,
		TPArmed: true, SLArmed: true,
	}
	store.seed(arm)

	feed := newFakeFeed()
	exec := &fakeExecutor{}
	notif := &fakeNotifier{}
	m := NewSLTPMonitor(store, feed, exec, notif, nil)
	_ = m.Start()

	// Just below ceiling (0.9499 < 0.95). 2x TP threshold is 0.60, so standard
	// TP fires here (50% sale), not the ceiling.
	feed.setBid("C3", 0.9499)
	feed.emit("C3")

	waitFor(t, func() bool {
		notif.mu.Lock()
		defer notif.mu.Unlock()
		return len(notif.fires) == 1
	})

	notif.mu.Lock()
	kind := notif.fires[0].kind
	notif.mu.Unlock()
	if kind != "TP" {
		t.Errorf("expected standard TP fire just below ceiling, got %q", kind)
	}

	exec.mu.Lock()
	shares := exec.calls[0].sharesRaw
	exec.mu.Unlock()
	if shares != int64(100*database.TPSellFraction*1e6) {
		t.Errorf("expected standard TP half-sell 50e6 shares, got %d", shares)
	}
}

// --- lottery-ticket tests ---

// TestSLTPMonitor_Lottery_FiresAfterCeilingWhenAskCheap verifies the happy path:
// ceiling-TP fires, lottery flag is on, ask is below cap → lottery BUY succeeds
// and the user is notified.
func TestSLTPMonitor_Lottery_FiresAfterCeilingWhenAskCheap(t *testing.T) {
	t.Parallel()
	store := newFakeStore()
	store.seed(&database.SLTPArm{
		ID: 50, TelegramID: 31, TokenID: "L1", AvgPrice: 0.30, SharesAtArm: 200,
		TPArmed: true, SLArmed: true, LotteryTicketArmed: true,
	})

	feed := newFakeFeed()
	feed.setAsk("L1-other", 0.04) // below LotteryMaxPrice of 0.05

	exec := &fakeExecutor{}
	notif := &fakeNotifier{}
	m := NewSLTPMonitor(store, feed, exec, notif, nil)
	_ = m.Start()

	feed.setBid("L1", database.CeilingTPPrice)
	feed.emit("L1")

	waitFor(t, func() bool {
		notif.mu.Lock()
		defer notif.mu.Unlock()
		return len(notif.lotteries) == 1
	})

	exec.mu.Lock()
	defer exec.mu.Unlock()
	if len(exec.lotteryCalls) != 1 {
		t.Fatalf("expected 1 lottery call, got %d", len(exec.lotteryCalls))
	}
	got := exec.lotteryCalls[0]
	if got.otherTokenID != "L1-other" {
		t.Errorf("expected otherTokenID=L1-other, got %s", got.otherTokenID)
	}
	if got.maxPrice != database.LotteryMaxPrice {
		t.Errorf("expected maxPrice=%v, got %v", database.LotteryMaxPrice, got.maxPrice)
	}
	if got.maxSpend != database.LotteryMaxSpend {
		t.Errorf("expected maxSpend=%v, got %v", database.LotteryMaxSpend, got.maxSpend)
	}

	notif.mu.Lock()
	defer notif.mu.Unlock()
	if notif.lotteries[0].reason != "filled" {
		t.Errorf("expected lottery notice reason=filled, got %q", notif.lotteries[0].reason)
	}
}

// TestSLTPMonitor_Lottery_NotArmedSkips verifies that when the user hasn't
// opted into lottery, the ceiling-TP fires without any lottery call.
func TestSLTPMonitor_Lottery_NotArmedSkips(t *testing.T) {
	t.Parallel()
	store := newFakeStore()
	store.seed(&database.SLTPArm{
		ID: 51, TelegramID: 32, TokenID: "L2", AvgPrice: 0.30, SharesAtArm: 100,
		TPArmed: true, SLArmed: true, LotteryTicketArmed: false,
	})

	feed := newFakeFeed()
	feed.setAsk("L2-other", 0.03) // would be cheap enough if lottery were armed

	exec := &fakeExecutor{}
	notif := &fakeNotifier{}
	m := NewSLTPMonitor(store, feed, exec, notif, nil)
	_ = m.Start()

	feed.setBid("L2", 0.96)
	feed.emit("L2")

	waitFor(t, func() bool {
		notif.mu.Lock()
		defer notif.mu.Unlock()
		return len(notif.fires) == 1 // ceiling-TP fired
	})

	// Sanity: give any errant lottery goroutine a moment to run.
	time.Sleep(50 * time.Millisecond)

	exec.mu.Lock()
	defer exec.mu.Unlock()
	if len(exec.lotteryCalls) != 0 {
		t.Errorf("expected no lottery call when not armed, got %d", len(exec.lotteryCalls))
	}
	notif.mu.Lock()
	defer notif.mu.Unlock()
	if len(notif.lotteries) != 0 {
		t.Errorf("expected no lottery notification, got %d", len(notif.lotteries))
	}
}

// TestSLTPMonitor_Lottery_AskTooHighSkips verifies that when the other side's
// ask is above LotteryMaxPrice, the lottery is skipped and the user is told
// why — no BUY is attempted.
func TestSLTPMonitor_Lottery_AskTooHighSkips(t *testing.T) {
	t.Parallel()
	store := newFakeStore()
	store.seed(&database.SLTPArm{
		ID: 52, TelegramID: 33, TokenID: "L3", AvgPrice: 0.30, SharesAtArm: 100,
		TPArmed: true, SLArmed: true, LotteryTicketArmed: true,
	})

	feed := newFakeFeed()
	feed.setAsk("L3-other", 0.07) // above 0.05 cap

	exec := &fakeExecutor{}
	notif := &fakeNotifier{}
	m := NewSLTPMonitor(store, feed, exec, notif, nil)
	_ = m.Start()

	feed.setBid("L3", 0.96)
	feed.emit("L3")

	waitFor(t, func() bool {
		notif.mu.Lock()
		defer notif.mu.Unlock()
		return len(notif.lotteries) == 1
	})

	exec.mu.Lock()
	defer exec.mu.Unlock()
	if len(exec.lotteryCalls) != 0 {
		t.Errorf("expected no lottery call when ask above cap, got %d", len(exec.lotteryCalls))
	}
	notif.mu.Lock()
	defer notif.mu.Unlock()
	if notif.lotteries[0].reason != "ask-too-high" {
		t.Errorf("expected lottery reason=ask-too-high, got %q", notif.lotteries[0].reason)
	}
}

// TestSLTPMonitor_Lottery_MultiOutcomeSkips verifies that when ResolveOtherToken
// returns ErrMultiOutcome, the lottery is skipped with a clear notice.
func TestSLTPMonitor_Lottery_MultiOutcomeSkips(t *testing.T) {
	t.Parallel()
	store := newFakeStore()
	store.seed(&database.SLTPArm{
		ID: 53, TelegramID: 34, TokenID: "L4", AvgPrice: 0.30, SharesAtArm: 100,
		TPArmed: true, SLArmed: true, LotteryTicketArmed: true,
	})

	feed := newFakeFeed()
	exec := &fakeExecutor{resolveErr: ErrMultiOutcome}
	notif := &fakeNotifier{}
	m := NewSLTPMonitor(store, feed, exec, notif, nil)
	_ = m.Start()

	feed.setBid("L4", 0.96)
	feed.emit("L4")

	waitFor(t, func() bool {
		notif.mu.Lock()
		defer notif.mu.Unlock()
		return len(notif.lotteries) == 1
	})

	exec.mu.Lock()
	defer exec.mu.Unlock()
	if len(exec.lotteryCalls) != 0 {
		t.Errorf("expected no lottery call for multi-outcome market, got %d", len(exec.lotteryCalls))
	}
	notif.mu.Lock()
	defer notif.mu.Unlock()
	if notif.lotteries[0].reason != "multi-outcome" {
		t.Errorf("expected lottery reason=multi-outcome, got %q", notif.lotteries[0].reason)
	}
}

// TestSLTPMonitor_Lottery_BuyFailureNotified verifies that a failed lottery
// BUY (e.g., FOK rejected) still produces a notification with the failure
// detail — never silent.
func TestSLTPMonitor_Lottery_BuyFailureNotified(t *testing.T) {
	t.Parallel()
	store := newFakeStore()
	store.seed(&database.SLTPArm{
		ID: 54, TelegramID: 35, TokenID: "L5", AvgPrice: 0.30, SharesAtArm: 100,
		TPArmed: true, SLArmed: true, LotteryTicketArmed: true,
	})

	feed := newFakeFeed()
	feed.setAsk("L5-other", 0.04)

	exec := &fakeExecutor{
		lotteryRet: &polymarket.TradeResult{
			Success:  false,
			ErrorMsg: "FOK rejected: insufficient depth",
		},
	}
	notif := &fakeNotifier{}
	m := NewSLTPMonitor(store, feed, exec, notif, nil)
	_ = m.Start()

	feed.setBid("L5", 0.96)
	feed.emit("L5")

	waitFor(t, func() bool {
		notif.mu.Lock()
		defer notif.mu.Unlock()
		return len(notif.lotteries) == 1
	})

	notif.mu.Lock()
	defer notif.mu.Unlock()
	if notif.lotteries[0].reason != "failed" {
		t.Errorf("expected lottery reason=failed, got %q", notif.lotteries[0].reason)
	}
	if !strings.Contains(notif.lotteries[0].detail, "FOK rejected") {
		t.Errorf("expected detail to mention FOK rejection, got %q", notif.lotteries[0].detail)
	}
}

// TestSLTPMonitor_Lottery_NotFiredIfSellFailed verifies that when the
// ceiling-TP SELL itself fails, we don't attempt the lottery — would be
// silly to add a position when we couldn't exit the original.
func TestSLTPMonitor_Lottery_NotFiredIfSellFailed(t *testing.T) {
	t.Parallel()
	store := newFakeStore()
	store.seed(&database.SLTPArm{
		ID: 55, TelegramID: 36, TokenID: "L6", AvgPrice: 0.30, SharesAtArm: 100,
		TPArmed: true, SLArmed: true, LotteryTicketArmed: true,
	})

	feed := newFakeFeed()
	feed.setAsk("L6-other", 0.04)

	exec := &fakeExecutor{
		ret: &polymarket.TradeResult{Success: false, ErrorMsg: "sell failed"},
	}
	notif := &fakeNotifier{}
	m := NewSLTPMonitor(store, feed, exec, notif, nil)
	_ = m.Start()

	feed.setBid("L6", 0.96)
	feed.emit("L6")

	// SELL was attempted (and reported as fired-with-failure via NotifySLTPFired).
	waitFor(t, func() bool {
		notif.mu.Lock()
		defer notif.mu.Unlock()
		return len(notif.fires) == 1
	})

	time.Sleep(50 * time.Millisecond)

	exec.mu.Lock()
	defer exec.mu.Unlock()
	if len(exec.lotteryCalls) != 0 {
		t.Errorf("expected no lottery call after failed SELL, got %d", len(exec.lotteryCalls))
	}
}

// --- trailing-SL: HWM ratchet ---

func TestSLTPMonitor_HWMRatchetsUpOnNewHigh(t *testing.T) {
	t.Parallel()
	store := newFakeStore()
	store.seed(&database.SLTPArm{
		ID: 60, TelegramID: 41, TokenID: "H1", AvgPrice: 0.20, SharesAtArm: 100,
		HighWaterMark: 0.20, TPArmed: true, SLArmed: true,
	})
	feed := newFakeFeed()
	exec := &fakeExecutor{}
	m := NewSLTPMonitor(store, feed, exec, &fakeNotifier{}, nil)
	_ = m.Start()

	feed.setBid("H1", 0.25)
	feed.emit("H1")
	waitFor(t, func() bool { return store.storedHWM("H1") == 0.25 })

	// A lower bid must not lower the stored HWM nor trigger an update call.
	feed.setBid("H1", 0.22)
	feed.emit("H1")
	time.Sleep(50 * time.Millisecond)
	store.mu.Lock()
	calls := len(store.updateHWMCalls)
	store.mu.Unlock()
	if calls != 1 {
		t.Errorf("expected 1 UpdateHWM call after lower bid, got %d", calls)
	}
	if got := store.storedHWM("H1"); got != 0.25 {
		t.Errorf("HWM lowered to %v, want 0.25", got)
	}

	feed.setBid("H1", 0.30)
	feed.emit("H1")
	waitFor(t, func() bool { return store.storedHWM("H1") == 0.30 })
}

func TestSLTPMonitor_HWMPersistCallsAreMonotonic(t *testing.T) {
	t.Parallel()
	store := newFakeStore()
	store.seed(&database.SLTPArm{
		ID: 61, TelegramID: 42, TokenID: "H2", AvgPrice: 0.30, SharesAtArm: 100,
		HighWaterMark: 0.30, TPArmed: true, SLArmed: true,
	})
	feed := newFakeFeed()
	m := NewSLTPMonitor(store, feed, &fakeExecutor{}, &fakeNotifier{}, nil)
	_ = m.Start()

	feed.setBid("H2", 0.42)
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			feed.emit("H2")
		}()
	}
	wg.Wait()
	waitFor(t, func() bool { return store.storedHWM("H2") == 0.42 })

	// Concurrent duplicate calls are fine; the guard must never lower it.
	feed.setBid("H2", 0.35)
	feed.emit("H2")
	time.Sleep(50 * time.Millisecond)
	if got := store.storedHWM("H2"); got != 0.42 {
		t.Errorf("HWM regressed to %v, want 0.42", got)
	}
}

// --- trailing-SL: state machine ---

// slBreachMonitor builds a monitor wired to a fake clock. Callers seed the
// store/feed first and Start() it themselves.
func slBreachMonitor(store *fakeStore, feed *fakeFeed, exec *fakeExecutor, notif *fakeNotifier) (*SLTPMonitor, *fakeClock) {
	clock := newFakeClock()
	m := NewSLTPMonitor(store, feed, exec, notif, nil)
	m.now = clock.now
	return m, clock
}

func TestSLTPMonitor_SL_DormantNeverFires(t *testing.T) {
	t.Parallel()
	store := newFakeStore()
	// HWM == avg: the stop never activated. A crash must NOT sell.
	store.seed(&database.SLTPArm{ID: 70, TelegramID: 51, TokenID: "D1", AvgPrice: 0.50, SharesAtArm: 100,
		HighWaterMark: 0.50, TPArmed: false, SLArmed: true})
	feed := newFakeFeed()
	exec := &fakeExecutor{}
	notif := &fakeNotifier{}
	m, clock := slBreachMonitor(store, feed, exec, notif)
	_ = m.Start()

	feed.setBid("D1", 0.05)
	feed.emit("D1")
	clock.advance(10 * time.Minute)
	feed.emit("D1")
	time.Sleep(50 * time.Millisecond)

	exec.mu.Lock()
	calls := len(exec.calls)
	exec.mu.Unlock()
	if calls != 0 {
		t.Errorf("dormant SL must never sell, got %d calls", calls)
	}
	store.mu.Lock()
	disarms := store.disarmCalls
	store.mu.Unlock()
	if disarms != 0 {
		t.Errorf("dormant SL must not disarm, got %d", disarms)
	}
	if !slStateCleared(m, 70) {
		t.Error("dormant branch should keep no breach state")
	}
}

func TestSLTPMonitor_SL_ActivationViaRatchetThenBreachFires(t *testing.T) {
	t.Parallel()
	store := newFakeStore()
	// Starts dormant (HWM == avg). A rally to 0.62 (>= 0.60 activation)
	// activates the stop via the live ratchet.
	store.seed(&database.SLTPArm{ID: 71, TelegramID: 52, TokenID: "D2", AvgPrice: 0.50, SharesAtArm: 100,
		HighWaterMark: 0.50, TPArmed: true, SLArmed: true})
	feed := newFakeFeed()
	exec := &fakeExecutor{}
	notif := &fakeNotifier{}
	m, clock := slBreachMonitor(store, feed, exec, notif)
	_ = m.Start()

	feed.setBid("D2", 0.62)
	feed.emit("D2")
	waitFor(t, func() bool { return store.storedHWM("D2") == 0.62 })

	// Breach: trigger = max(0.50, 0.62*0.80=0.496) = 0.50.
	feed.setBid("D2", 0.45)
	feed.emit("D2")
	waitFor(t, func() bool { return breachStamped(m, 71) })
	clock.advance(31 * time.Second)
	feed.emit("D2")

	waitFor(t, func() bool {
		notif.mu.Lock()
		defer notif.mu.Unlock()
		return len(notif.fires) == 1
	})
	exec.mu.Lock()
	call := exec.calls[0]
	exec.mu.Unlock()
	if call.orderType != polymarket.OrderTypeFOK {
		t.Errorf("expected FOK, got %v", call.orderType)
	}
	if diff := call.limitPrice - 0.45; diff > 1e-9 || diff < -1e-9 { // 0.50*0.90
		t.Errorf("expected floor 0.45, got %v", call.limitPrice)
	}
}

func TestSLTPMonitor_SL_NoFireBeforeConfirmWindow(t *testing.T) {
	t.Parallel()
	store := newFakeStore()
	store.seed(&database.SLTPArm{ID: 72, TelegramID: 53, TokenID: "D3", AvgPrice: 0.50, SharesAtArm: 100,
		HighWaterMark: 0.65, TPArmed: true, SLArmed: true})
	feed := newFakeFeed()
	exec := &fakeExecutor{}
	notif := &fakeNotifier{}
	m, clock := slBreachMonitor(store, feed, exec, notif)
	_ = m.Start()

	feed.setBid("D3", 0.45)
	feed.emit("D3")
	waitFor(t, func() bool { return breachStamped(m, 72) })

	clock.advance(29 * time.Second)
	feed.emit("D3")
	time.Sleep(50 * time.Millisecond)

	exec.mu.Lock()
	calls := len(exec.calls)
	exec.mu.Unlock()
	if calls != 0 {
		t.Errorf("expected no sell before the confirm window, got %d", calls)
	}
}

func TestSLTPMonitor_SL_RecoveryResetsDebounce(t *testing.T) {
	t.Parallel()
	store := newFakeStore()
	store.seed(&database.SLTPArm{ID: 73, TelegramID: 54, TokenID: "D4", AvgPrice: 0.50, SharesAtArm: 100,
		HighWaterMark: 0.65, TPArmed: true, SLArmed: true})
	feed := newFakeFeed()
	exec := &fakeExecutor{}
	notif := &fakeNotifier{}
	m, clock := slBreachMonitor(store, feed, exec, notif)
	_ = m.Start()

	// Episode 1: breach, then recover 10s in.
	feed.setBid("D4", 0.45)
	feed.emit("D4")
	waitFor(t, func() bool { return breachStamped(m, 73) })
	clock.advance(10 * time.Second)
	feed.setBid("D4", 0.60) // above trigger 0.52, below HWM 0.65
	feed.emit("D4")
	waitFor(t, func() bool { return slStateCleared(m, 73) })

	// Episode 2: breach again; the old 10s must not count.
	feed.setBid("D4", 0.45)
	feed.emit("D4")
	waitFor(t, func() bool { return breachStamped(m, 73) })
	clock.advance(25 * time.Second)
	feed.emit("D4")
	time.Sleep(50 * time.Millisecond)
	exec.mu.Lock()
	calls := len(exec.calls)
	exec.mu.Unlock()
	if calls != 0 {
		t.Errorf("expected no sell 25s into a fresh episode, got %d", calls)
	}

	clock.advance(6 * time.Second)
	feed.emit("D4")
	waitFor(t, func() bool {
		exec.mu.Lock()
		defer exec.mu.Unlock()
		return len(exec.calls) == 1
	})
}

func TestSLTPMonitor_SL_RestartLosesBreachStateSafely(t *testing.T) {
	t.Parallel()
	store := newFakeStore()
	store.seed(&database.SLTPArm{ID: 74, TelegramID: 55, TokenID: "D5", AvgPrice: 0.50, SharesAtArm: 100,
		HighWaterMark: 0.65, TPArmed: true, SLArmed: true})

	// Monitor #1 accumulates a confirmed-age breach...
	feed1 := newFakeFeed()
	m1, clock1 := slBreachMonitor(store, feed1, &fakeExecutor{}, &fakeNotifier{})
	_ = m1.Start()
	feed1.setBid("D5", 0.45)
	feed1.emit("D5")
	waitFor(t, func() bool { return breachStamped(m1, 74) })
	clock1.advance(31 * time.Second)
	m1.Stop() // "crash" before the next evaluation

	// ...restart: monitor #2 must require a full fresh window.
	feed2 := newFakeFeed()
	exec2 := &fakeExecutor{}
	notif2 := &fakeNotifier{}
	m2, clock2 := slBreachMonitor(store, feed2, exec2, notif2)
	_ = m2.Start()
	feed2.setBid("D5", 0.45)
	feed2.emit("D5")
	waitFor(t, func() bool { return breachStamped(m2, 74) })
	time.Sleep(50 * time.Millisecond)
	exec2.mu.Lock()
	calls := len(exec2.calls)
	exec2.mu.Unlock()
	if calls != 0 {
		t.Errorf("restart must not inherit the old breach age, got %d sells", calls)
	}

	clock2.advance(31 * time.Second)
	feed2.emit("D5")
	waitFor(t, func() bool {
		exec2.mu.Lock()
		defer exec2.mu.Unlock()
		return len(exec2.calls) == 1
	})
}

func TestSLTPMonitor_SL_FOKFailureKeepsArmAndNotifiesPendingOnce(t *testing.T) {
	t.Parallel()
	store := newFakeStore()
	store.seed(&database.SLTPArm{ID: 75, TelegramID: 56, TokenID: "D6", AvgPrice: 0.50, SharesAtArm: 100,
		HighWaterMark: 0.65, TPArmed: true, SLArmed: true})
	feed := newFakeFeed()
	exec := &fakeExecutor{ret: &polymarket.TradeResult{Success: false, ErrorMsg: "fok no fill"}}
	notif := &fakeNotifier{}
	m, clock := slBreachMonitor(store, feed, exec, notif)
	_ = m.Start()

	feed.setBid("D6", 0.45)
	feed.emit("D6")
	waitFor(t, func() bool { return breachStamped(m, 75) })
	clock.advance(31 * time.Second)
	feed.emit("D6")

	waitFor(t, func() bool {
		notif.mu.Lock()
		defer notif.mu.Unlock()
		return len(notif.pendings) == 1
	})

	// Arm must survive; no disarm, no "fired" notice.
	store.mu.Lock()
	disarms := store.disarmCalls
	store.mu.Unlock()
	if disarms != 0 {
		t.Errorf("failed FOK must keep the arm, got %d disarms", disarms)
	}
	if store.armedCount("D6") != 1 {
		t.Errorf("arm should still be listed, got %d", store.armedCount("D6"))
	}
	notif.mu.Lock()
	fires := len(notif.fires)
	pending := notif.pendings[0]
	notif.mu.Unlock()
	if fires != 0 {
		t.Errorf("no fired notice on failed exit, got %d", fires)
	}
	if pending.telegramID != 56 || pending.armID != 75 {
		t.Errorf("pending notice routed wrong: %+v", pending)
	}
	if diff := pending.floor - 0.468; diff > 1e-9 || diff < -1e-9 {
		t.Errorf("pending floor = %v, want 0.468", pending.floor)
	}
	// A plain FOK kill is NOT a stale-size case (issue #24 regression guard).
	notif.mu.Lock()
	stales := len(notif.stales)
	notif.mu.Unlock()
	if stales != 0 {
		t.Errorf("plain FOK kill must not send a stale-size notice, got %d", stales)
	}

	// More breach evaluations inside the retry interval: still exactly one
	// attempt and one pending notice.
	clock.advance(10 * time.Second)
	feed.emit("D6")
	time.Sleep(50 * time.Millisecond)
	exec.mu.Lock()
	calls := len(exec.calls)
	exec.mu.Unlock()
	notif.mu.Lock()
	pendings := len(notif.pendings)
	notif.mu.Unlock()
	if calls != 1 || pendings != 1 {
		t.Errorf("expected 1 attempt / 1 pending inside retry interval, got %d / %d", calls, pendings)
	}
}

func TestSLTPMonitor_SL_RetryRateLimited(t *testing.T) {
	t.Parallel()
	store := newFakeStore()
	store.seed(&database.SLTPArm{ID: 76, TelegramID: 57, TokenID: "D7", AvgPrice: 0.50, SharesAtArm: 100,
		HighWaterMark: 0.65, TPArmed: true, SLArmed: true})
	feed := newFakeFeed()
	exec := &fakeExecutor{ret: &polymarket.TradeResult{Success: false, ErrorMsg: "fok no fill"}}
	notif := &fakeNotifier{}
	m, clock := slBreachMonitor(store, feed, exec, notif)
	_ = m.Start()

	feed.setBid("D7", 0.45)
	feed.emit("D7")
	waitFor(t, func() bool { return breachStamped(m, 76) })
	clock.advance(31 * time.Second)
	feed.emit("D7")
	waitFor(t, func() bool {
		exec.mu.Lock()
		defer exec.mu.Unlock()
		return len(exec.calls) == 1
	})

	clock.advance(10 * time.Second)
	feed.emit("D7")
	clock.advance(10 * time.Second)
	feed.emit("D7")
	time.Sleep(50 * time.Millisecond)
	exec.mu.Lock()
	calls := len(exec.calls)
	exec.mu.Unlock()
	if calls != 1 {
		t.Errorf("expected retry to be rate-limited, got %d attempts", calls)
	}

	clock.advance(11 * time.Second) // 31s since attempt #1
	feed.emit("D7")
	waitFor(t, func() bool {
		exec.mu.Lock()
		defer exec.mu.Unlock()
		return len(exec.calls) == 2
	})

	// Second failure must not send a second pending notice (same episode).
	time.Sleep(50 * time.Millisecond)
	notif.mu.Lock()
	pendings := len(notif.pendings)
	notif.mu.Unlock()
	if pendings != 1 {
		t.Errorf("expected 1 pending notice per episode, got %d", pendings)
	}
}

func TestSLTPMonitor_SL_DisarmOnlyAfterSuccessfulSell(t *testing.T) {
	t.Parallel()
	store := newFakeStore()
	store.seed(&database.SLTPArm{ID: 77, TelegramID: 58, TokenID: "D8", AvgPrice: 0.50, SharesAtArm: 100,
		HighWaterMark: 0.65, TPArmed: true, SLArmed: true})
	feed := newFakeFeed()
	exec := &fakeExecutor{}
	var disarmsAtSellTime int32 = -1
	exec.onSell = func() {
		store.mu.Lock()
		defer store.mu.Unlock()
		atomic.StoreInt32(&disarmsAtSellTime, int32(store.disarmCalls))
	}
	notif := &fakeNotifier{}
	m, clock := slBreachMonitor(store, feed, exec, notif)
	_ = m.Start()

	feed.setBid("D8", 0.45)
	feed.emit("D8")
	waitFor(t, func() bool { return breachStamped(m, 77) })
	clock.advance(31 * time.Second)
	feed.emit("D8")
	waitFor(t, func() bool {
		store.mu.Lock()
		defer store.mu.Unlock()
		return store.disarmCalls == 1
	})

	if got := atomic.LoadInt32(&disarmsAtSellTime); got != 0 {
		t.Errorf("disarm must happen AFTER the sell; at sell time disarms=%d", got)
	}
}

func TestSLTPMonitor_SL_ConcurrentConfirmedBreachFiresOnce(t *testing.T) {
	t.Parallel()
	store := newFakeStore()
	store.seed(&database.SLTPArm{ID: 78, TelegramID: 59, TokenID: "D9", AvgPrice: 0.50, SharesAtArm: 100,
		HighWaterMark: 0.65, TPArmed: true, SLArmed: true})
	feed := newFakeFeed()
	exec := &fakeExecutor{}
	notif := &fakeNotifier{}
	m, clock := slBreachMonitor(store, feed, exec, notif)
	_ = m.Start()

	feed.setBid("D9", 0.45)
	feed.emit("D9")
	waitFor(t, func() bool { return breachStamped(m, 78) })
	clock.advance(31 * time.Second)

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			feed.emit("D9")
		}()
	}
	wg.Wait()

	waitFor(t, func() bool {
		exec.mu.Lock()
		defer exec.mu.Unlock()
		return len(exec.calls) >= 1
	})
	time.Sleep(100 * time.Millisecond)
	exec.mu.Lock()
	calls := len(exec.calls)
	exec.mu.Unlock()
	if calls != 1 {
		t.Errorf("expected exactly 1 sell under concurrent confirmed breach, got %d", calls)
	}
	store.mu.Lock()
	disarms := store.disarmCalls
	store.mu.Unlock()
	if disarms != 1 {
		t.Errorf("expected exactly 1 disarm, got %d", disarms)
	}
}

func TestSLTPMonitor_SL_SoldButDisarmFailedNeverResells(t *testing.T) {
	t.Parallel()
	store := newFakeStore()
	store.seed(&database.SLTPArm{ID: 79, TelegramID: 60, TokenID: "DA", AvgPrice: 0.50, SharesAtArm: 100,
		HighWaterMark: 0.65, TPArmed: true, SLArmed: true})
	store.mu.Lock()
	store.disarmFailN = 1
	store.mu.Unlock()
	feed := newFakeFeed()
	exec := &fakeExecutor{}
	notif := &fakeNotifier{}
	m, clock := slBreachMonitor(store, feed, exec, notif)
	_ = m.Start()

	feed.setBid("DA", 0.45)
	feed.emit("DA")
	waitFor(t, func() bool { return breachStamped(m, 79) })
	clock.advance(31 * time.Second)
	feed.emit("DA")

	// Sell succeeded, disarm failed once: the fired notice still goes out.
	waitFor(t, func() bool {
		notif.mu.Lock()
		defer notif.mu.Unlock()
		return len(notif.fires) == 1
	})

	// Next evaluation retries ONLY the disarm — no second sell.
	feed.emit("DA")
	waitFor(t, func() bool {
		store.mu.Lock()
		defer store.mu.Unlock()
		return store.disarmCalls == 1
	})
	time.Sleep(50 * time.Millisecond)
	exec.mu.Lock()
	calls := len(exec.calls)
	exec.mu.Unlock()
	if calls != 1 {
		t.Errorf("sold arm must never re-sell, got %d sells", calls)
	}
	notif.mu.Lock()
	fires := len(notif.fires)
	notif.mu.Unlock()
	if fires != 1 {
		t.Errorf("expected exactly 1 fired notice, got %d", fires)
	}
	if store.armedCount("DA") != 0 {
		t.Errorf("arm should be gone after the disarm retry, got %d", store.armedCount("DA"))
	}
}

func TestSLTPMonitor_SL_TPFireDuringBreachClearsDebounce(t *testing.T) {
	t.Parallel()
	store := newFakeStore()
	// Active: trigger = max(0.20, 0.30*0.80=0.24) = 0.24. TP trigger = 0.40.
	store.seed(&database.SLTPArm{ID: 80, TelegramID: 61, TokenID: "DB", AvgPrice: 0.20, SharesAtArm: 100,
		HighWaterMark: 0.30, TPArmed: true, SLArmed: true})
	feed := newFakeFeed()
	exec := &fakeExecutor{}
	notif := &fakeNotifier{}
	m, clock := slBreachMonitor(store, feed, exec, notif)
	_ = m.Start()

	feed.setBid("DB", 0.23)
	feed.emit("DB")
	waitFor(t, func() bool { return breachStamped(m, 80) })
	clock.advance(15 * time.Second)

	// TP fires mid-breach; the debounce state must be wiped.
	feed.setBid("DB", 0.41)
	feed.emit("DB")
	waitFor(t, func() bool {
		notif.mu.Lock()
		defer notif.mu.Unlock()
		return len(notif.fires) == 1 && notif.fires[0].kind == "TP"
	})
	waitFor(t, func() bool { return slStateCleared(m, 80) })

	// New breach (HWM ratcheted to 0.41 → trigger = max(0.20, 0.328) = 0.328):
	// needs a FULL fresh window, the old 15s must not count.
	feed.setBid("DB", 0.23)
	feed.emit("DB")
	waitFor(t, func() bool { return breachStamped(m, 80) })
	clock.advance(20 * time.Second)
	feed.emit("DB")
	time.Sleep(50 * time.Millisecond)
	exec.mu.Lock()
	calls := len(exec.calls)
	exec.mu.Unlock()
	if calls != 1 { // just the TP sell so far
		t.Errorf("expected no SL sell 20s into the fresh episode, got %d total sells", calls)
	}

	clock.advance(11 * time.Second)
	feed.emit("DB")
	waitFor(t, func() bool {
		notif.mu.Lock()
		defer notif.mu.Unlock()
		return len(notif.fires) == 2 && notif.fires[1].kind == "SL"
	})
	exec.mu.Lock()
	slCall := exec.calls[1]
	exec.mu.Unlock()
	// TP already fired → SL sells the remaining half.
	if slCall.sharesRaw != int64(100*0.50*1e6) {
		t.Errorf("expected SL sell of remaining half (50e6), got %d", slCall.sharesRaw)
	}
	if slCall.orderType != polymarket.OrderTypeFOK {
		t.Errorf("expected FOK SL sell, got %v", slCall.orderType)
	}
}

func TestSLTPMonitor_SL_CeilingDuringBreachClearsState(t *testing.T) {
	t.Parallel()
	store := newFakeStore()
	store.seed(&database.SLTPArm{ID: 81, TelegramID: 62, TokenID: "DC", AvgPrice: 0.50, SharesAtArm: 100,
		HighWaterMark: 0.65, TPArmed: true, SLArmed: true})
	feed := newFakeFeed()
	exec := &fakeExecutor{}
	notif := &fakeNotifier{}
	m, clock := slBreachMonitor(store, feed, exec, notif)
	_ = m.Start()

	feed.setBid("DC", 0.45)
	feed.emit("DC")
	waitFor(t, func() bool { return breachStamped(m, 81) })
	clock.advance(10 * time.Second)

	feed.setBid("DC", 0.96)
	feed.emit("DC")
	waitFor(t, func() bool {
		notif.mu.Lock()
		defer notif.mu.Unlock()
		return len(notif.fires) == 1 && notif.fires[0].kind == "TP-ceiling"
	})
	waitFor(t, func() bool { return slStateCleared(m, 81) })
	if store.armedCount("DC") != 0 {
		t.Errorf("ceiling fire should disarm fully, got %d armed", store.armedCount("DC"))
	}
}

func TestSLTPMonitor_SL_RearmMidBreachResets(t *testing.T) {
	t.Parallel()
	store := newFakeStore()
	store.seed(&database.SLTPArm{ID: 82, TelegramID: 63, TokenID: "DD", AvgPrice: 0.50, SharesAtArm: 100,
		HighWaterMark: 0.70, TPArmed: true, SLArmed: true}) // trigger 0.56
	feed := newFakeFeed()
	exec := &fakeExecutor{}
	notif := &fakeNotifier{}
	m, clock := slBreachMonitor(store, feed, exec, notif)
	_ = m.Start()

	feed.setBid("DD", 0.50)
	feed.emit("DD")
	waitFor(t, func() bool { return breachStamped(m, 82) })
	clock.advance(31 * time.Second)

	// Re-arm (upsert, same row ID): HWM reset to avg → dormant again.
	store.replace(&database.SLTPArm{ID: 82, TelegramID: 63, TokenID: "DD", AvgPrice: 0.50, SharesAtArm: 100,
		HighWaterMark: 0.50, TPArmed: true, SLArmed: true})

	feed.emit("DD")
	waitFor(t, func() bool { return slStateCleared(m, 82) })
	time.Sleep(50 * time.Millisecond)
	exec.mu.Lock()
	calls := len(exec.calls)
	exec.mu.Unlock()
	if calls != 0 {
		t.Errorf("re-armed (dormant) arm must not fire from stale breach state, got %d", calls)
	}
}

// --- stale share snapshot (issue #24) ---

// shortfallResult builds the failed TradeResult the trading client produces
// for the CLOB's "not enough balance" rejection.
func shortfallResult(availableRaw int64) *polymarket.TradeResult {
	return &polymarket.TradeResult{
		Success:             false,
		ErrorMsg:            "Order failed: not enough balance / allowance",
		InsufficientBalance: true,
		AvailableSharesRaw:  availableRaw,
	}
}

// TestSLTPMonitor_SL_ShortfallClampsRetryToBalance is the issue #24 core
// case: arm snapshot 450 shares, 225 manually sold outside the bot. The
// first SL attempt is rejected with the wallet's actual balance; the user
// gets ONE stale-size notice (never the misleading thin-book pending), and
// every later attempt sells the clamped size instead of the doomed 450.
func TestSLTPMonitor_SL_ShortfallClampsRetryToBalance(t *testing.T) {
	t.Parallel()
	store := newFakeStore()
	store.seed(&database.SLTPArm{ID: 100, TelegramID: 81, TokenID: "S1", AvgPrice: 0.50, SharesAtArm: 450,
		HighWaterMark: 0.65, TPArmed: true, SLArmed: true})
	feed := newFakeFeed()
	exec := &fakeExecutor{ret: shortfallResult(225_000_000)}
	notif := &fakeNotifier{}
	m, clock := slBreachMonitor(store, feed, exec, notif)
	_ = m.Start()

	feed.setBid("S1", 0.45)
	feed.emit("S1")
	waitFor(t, func() bool { return breachStamped(m, 100) })
	clock.advance(31 * time.Second)
	feed.emit("S1")

	waitFor(t, func() bool {
		notif.mu.Lock()
		defer notif.mu.Unlock()
		return len(notif.stales) == 1
	})

	notif.mu.Lock()
	stale := notif.stales[0]
	pendings := len(notif.pendings)
	notif.mu.Unlock()
	if stale.telegramID != 81 || stale.armID != 100 || stale.availableRaw != 225_000_000 {
		t.Errorf("stale notice = %+v, want user 81 arm 100 available 225000000", stale)
	}
	if pendings != 0 {
		t.Errorf("balance shortfall must not send the thin-book pending notice, got %d", pendings)
	}
	exec.mu.Lock()
	first := exec.calls[0].sharesRaw
	exec.mu.Unlock()
	if first != int64(450*1e6) {
		t.Errorf("first attempt sharesRaw = %d, want 450e6 (snapshot)", first)
	}

	// Arm survives; the next retry sells the clamped size.
	if store.armedCount("S1") != 1 {
		t.Errorf("arm should survive a shortfall, got %d armed", store.armedCount("S1"))
	}
	clock.advance(31 * time.Second)
	feed.emit("S1")
	waitFor(t, func() bool {
		exec.mu.Lock()
		defer exec.mu.Unlock()
		return len(exec.calls) == 2
	})
	exec.mu.Lock()
	second := exec.calls[1].sharesRaw
	exec.mu.Unlock()
	if second != 225_000_000 {
		t.Errorf("retry sharesRaw = %d, want clamped 225000000", second)
	}

	// Second shortfall in the same episode: still exactly one stale notice.
	time.Sleep(50 * time.Millisecond)
	notif.mu.Lock()
	stales := len(notif.stales)
	notif.mu.Unlock()
	if stales != 1 {
		t.Errorf("expected 1 stale notice per episode, got %d", stales)
	}
}

// TestSLTPMonitor_SL_ShortfallZeroDisarms: the wallet holds nothing — the
// position was fully closed outside the bot. The arm is auto-disarmed, the
// user told once, and no further sell attempts happen.
func TestSLTPMonitor_SL_ShortfallZeroDisarms(t *testing.T) {
	t.Parallel()
	store := newFakeStore()
	store.seed(&database.SLTPArm{ID: 101, TelegramID: 82, TokenID: "S2", AvgPrice: 0.50, SharesAtArm: 450,
		HighWaterMark: 0.65, TPArmed: true, SLArmed: true})
	feed := newFakeFeed()
	exec := &fakeExecutor{ret: shortfallResult(0)}
	notif := &fakeNotifier{}
	m, clock := slBreachMonitor(store, feed, exec, notif)
	_ = m.Start()

	feed.setBid("S2", 0.45)
	feed.emit("S2")
	waitFor(t, func() bool { return breachStamped(m, 101) })
	clock.advance(31 * time.Second)
	feed.emit("S2")

	waitFor(t, func() bool {
		store.mu.Lock()
		defer store.mu.Unlock()
		return store.disarmCalls == 1
	})
	waitFor(t, func() bool {
		notif.mu.Lock()
		defer notif.mu.Unlock()
		return len(notif.stales) == 1
	})

	notif.mu.Lock()
	stale := notif.stales[0]
	pendings := len(notif.pendings)
	fires := len(notif.fires)
	notif.mu.Unlock()
	if stale.availableRaw != 0 {
		t.Errorf("stale notice availableRaw = %d, want 0", stale.availableRaw)
	}
	if pendings != 0 || fires != 0 {
		t.Errorf("zero-balance shortfall must send only the stale notice, got %d pendings / %d fires", pendings, fires)
	}
	if store.armedCount("S2") != 0 {
		t.Errorf("arm should be auto-disarmed, got %d armed", store.armedCount("S2"))
	}
	waitFor(t, func() bool {
		feed.mu.Lock()
		defer feed.mu.Unlock()
		return len(feed.unsubscribes) == 1 && feed.unsubscribes[0] == "S2"
	})

	// Later evaluations must never sell again.
	clock.advance(31 * time.Second)
	feed.emit("S2")
	time.Sleep(50 * time.Millisecond)
	exec.mu.Lock()
	calls := len(exec.calls)
	exec.mu.Unlock()
	if calls != 1 {
		t.Errorf("gone position must never be re-sold, got %d attempts", calls)
	}
}

// TestSLTPMonitor_TP_ShortfallRetriesClamped: a TP half-sell rejected for
// balance shortfall retries exactly once, immediately, clamped to the
// wallet's actual balance.
func TestSLTPMonitor_TP_ShortfallRetriesClamped(t *testing.T) {
	t.Parallel()
	store := newFakeStore()
	// TP trigger = 0.40; intended TP sell = 50% of 450 = 225e6 raw.
	store.seed(&database.SLTPArm{ID: 102, TelegramID: 83, TokenID: "S3", AvgPrice: 0.20, SharesAtArm: 450,
		TPArmed: true, SLArmed: true})
	feed := newFakeFeed()
	exec := &fakeExecutor{ret: shortfallResult(100_000_000)}
	notif := &fakeNotifier{}
	m := NewSLTPMonitor(store, feed, exec, notif, nil)
	_ = m.Start()

	feed.setBid("S3", 0.41)
	feed.emit("S3")

	waitFor(t, func() bool {
		exec.mu.Lock()
		defer exec.mu.Unlock()
		return len(exec.calls) == 2
	})
	time.Sleep(50 * time.Millisecond)

	exec.mu.Lock()
	calls := make([]executorCall, len(exec.calls))
	copy(calls, exec.calls)
	exec.mu.Unlock()
	if len(calls) != 2 {
		t.Fatalf("expected exactly 2 sell attempts (original + one clamped retry), got %d", len(calls))
	}
	if calls[0].sharesRaw != int64(450*0.50*1e6) {
		t.Errorf("original TP sharesRaw = %d, want 225e6", calls[0].sharesRaw)
	}
	if calls[1].sharesRaw != 100_000_000 {
		t.Errorf("retry sharesRaw = %d, want clamped 100000000", calls[1].sharesRaw)
	}
	if calls[1].orderType != polymarket.OrderTypeGTC || calls[1].limitPrice != 0 {
		t.Errorf("retry should stay a market-style GTC, got (%v, %v)", calls[1].limitPrice, calls[1].orderType)
	}

	// The fire notice reflects the retry's outcome; the arm is not disarmed.
	notif.mu.Lock()
	fires := len(notif.fires)
	notif.mu.Unlock()
	if fires != 1 {
		t.Errorf("expected 1 TP fire notice, got %d", fires)
	}
	store.mu.Lock()
	disarms := store.disarmCalls
	store.mu.Unlock()
	if disarms != 0 {
		t.Errorf("TP shortfall > 0 must not disarm, got %d", disarms)
	}
}

// TestSLTPMonitor_TP_ShortfallZeroDisarms: a TP sell rejected with zero
// balance means the whole position is gone — disarm the arm, tell the user,
// and skip the fired notice.
func TestSLTPMonitor_TP_ShortfallZeroDisarms(t *testing.T) {
	t.Parallel()
	store := newFakeStore()
	store.seed(&database.SLTPArm{ID: 103, TelegramID: 84, TokenID: "S4", AvgPrice: 0.20, SharesAtArm: 450,
		TPArmed: true, SLArmed: true})
	feed := newFakeFeed()
	exec := &fakeExecutor{ret: shortfallResult(0)}
	notif := &fakeNotifier{}
	m := NewSLTPMonitor(store, feed, exec, notif, nil)
	_ = m.Start()

	feed.setBid("S4", 0.41)
	feed.emit("S4")

	waitFor(t, func() bool {
		store.mu.Lock()
		defer store.mu.Unlock()
		return store.disarmCalls == 1
	})
	waitFor(t, func() bool {
		notif.mu.Lock()
		defer notif.mu.Unlock()
		return len(notif.stales) == 1
	})
	time.Sleep(50 * time.Millisecond)

	notif.mu.Lock()
	stale := notif.stales[0]
	fires := len(notif.fires)
	notif.mu.Unlock()
	if stale.telegramID != 84 || stale.availableRaw != 0 {
		t.Errorf("stale notice = %+v, want user 84 available 0", stale)
	}
	if fires != 0 {
		t.Errorf("gone position must not produce a fired notice, got %d", fires)
	}
	exec.mu.Lock()
	calls := len(exec.calls)
	exec.mu.Unlock()
	if calls != 1 {
		t.Errorf("expected exactly 1 sell attempt (no retry at zero balance), got %d", calls)
	}
	if store.armedCount("S4") != 0 {
		t.Errorf("arm should be auto-disarmed, got %d armed", store.armedCount("S4"))
	}
}

// --- unsellable shortfall balances (issue #24 reopened) ---
//
// The CLOB truncates order sizes to 2 decimals, so fractional positions leave
// dust and availableRaw == 0 almost never happens (production saw 16922 raw =
// 0.0169 shares). A shortfall balance counts as GONE when the clamped sell
// could not be a valid CLOB order: below the 0.01-share size precision, or
// under the $1 minimum order value at the attempt's price (SL: FOK floor;
// TP/ceiling: current bid).

// TestShortfallGone pins the sellability rule itself.
func TestShortfallGone(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name         string
		availableRaw int64
		price        float64
		want         bool
	}{
		{"production dust 0.0169 shares", 16_922, 0.30, true},
		{"below size precision", 9_999, 0.99, true},
		{"zero balance", 0, 0.50, true},
		{"just under $1 minimum", 1_400_000, 0.70, true},   // $0.98
		{"just over $1 minimum", 1_500_000, 0.70, false},   // $1.05
		{"exactly $1 is sellable", 2_000_000, 0.50, false}, // $1.00
		{"issue #24 original clamp case", 225_000_000, 0.468, false},
	} {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := shortfallGone(tt.availableRaw, tt.price); got != tt.want {
				t.Errorf("shortfallGone(%d, %v) = %v, want %v", tt.availableRaw, tt.price, got, tt.want)
			}
		})
	}
}

// TestSLTPMonitor_SL_ShortfallDustAutoDisarms is the reopened issue #24 SL
// case: the wallet holds 16922 raw (0.0169 shares) of dust after outside
// sells. Clamping to it would be a doomed sub-$1 order, so the position is
// treated as gone: sold-latch + auto-disarm + one stale notice, and never
// another sell attempt.
func TestSLTPMonitor_SL_ShortfallDustAutoDisarms(t *testing.T) {
	t.Parallel()
	store := newFakeStore()
	store.seed(&database.SLTPArm{ID: 104, TelegramID: 85, TokenID: "S5", AvgPrice: 0.50, SharesAtArm: 99.9969,
		HighWaterMark: 0.65, TPArmed: true, SLArmed: true}) // trigger 0.52, floor 0.468
	feed := newFakeFeed()
	exec := &fakeExecutor{ret: shortfallResult(16_922)}
	notif := &fakeNotifier{}
	m, clock := slBreachMonitor(store, feed, exec, notif)
	_ = m.Start()

	feed.setBid("S5", 0.45)
	feed.emit("S5")
	waitFor(t, func() bool { return breachStamped(m, 104) })
	clock.advance(31 * time.Second)
	feed.emit("S5")

	waitFor(t, func() bool {
		store.mu.Lock()
		defer store.mu.Unlock()
		return store.disarmCalls == 1
	})
	waitFor(t, func() bool {
		notif.mu.Lock()
		defer notif.mu.Unlock()
		return len(notif.stales) == 1
	})

	notif.mu.Lock()
	stale := notif.stales[0]
	pendings := len(notif.pendings)
	fires := len(notif.fires)
	notif.mu.Unlock()
	if stale.telegramID != 85 || stale.armID != 104 || stale.availableRaw != 0 {
		t.Errorf("stale notice = %+v, want user 85 arm 104 available 0 (gone)", stale)
	}
	if pendings != 0 || fires != 0 {
		t.Errorf("dust shortfall must send only the stale notice, got %d pendings / %d fires", pendings, fires)
	}
	if store.armedCount("S5") != 0 {
		t.Errorf("arm should be auto-disarmed, got %d armed", store.armedCount("S5"))
	}

	// Later evaluations must never try to sell the dust.
	clock.advance(31 * time.Second)
	feed.emit("S5")
	time.Sleep(50 * time.Millisecond)
	exec.mu.Lock()
	calls := len(exec.calls)
	exec.mu.Unlock()
	if calls != 1 {
		t.Errorf("dust position must never be re-sold, got %d attempts", calls)
	}
}

// TestSLTPMonitor_SL_ShortfallBelowMinValueDisarms: the balance is above the
// size precision but the clamped sell would be worth less than the CLOB's $1
// minimum at the FOK floor (1.4 shares x $0.702 = $0.98) — gone, not clamp.
func TestSLTPMonitor_SL_ShortfallBelowMinValueDisarms(t *testing.T) {
	t.Parallel()
	store := newFakeStore()
	store.seed(&database.SLTPArm{ID: 105, TelegramID: 86, TokenID: "S6", AvgPrice: 0.78, SharesAtArm: 50,
		HighWaterMark: 0.975, TPArmed: true, SLArmed: true}) // trigger 0.78, floor 0.702
	feed := newFakeFeed()
	exec := &fakeExecutor{ret: shortfallResult(1_400_000)}
	notif := &fakeNotifier{}
	m, clock := slBreachMonitor(store, feed, exec, notif)
	_ = m.Start()

	feed.setBid("S6", 0.60)
	feed.emit("S6")
	waitFor(t, func() bool { return breachStamped(m, 105) })
	clock.advance(31 * time.Second)
	feed.emit("S6")

	waitFor(t, func() bool {
		store.mu.Lock()
		defer store.mu.Unlock()
		return store.disarmCalls == 1
	})
	notif.mu.Lock()
	staleZero := len(notif.stales) == 1 && notif.stales[0].availableRaw == 0
	pendings := len(notif.pendings)
	notif.mu.Unlock()
	if !staleZero {
		t.Errorf("want one stale notice with availableRaw 0 (gone), got %+v", notif.stales)
	}
	if pendings != 0 {
		t.Errorf("below-minimum shortfall must not send the thin-book pending notice, got %d", pendings)
	}
	if store.armedCount("S6") != 0 {
		t.Errorf("arm should be auto-disarmed, got %d armed", store.armedCount("S6"))
	}
}

// TestSLTPMonitor_SL_ShortfallClampsWhenSellable: the same arm/floor with 1.5
// shares ($1.05 at the floor) is a valid order — existing clamp behavior is
// unchanged and the next attempt sells exactly the reported balance.
func TestSLTPMonitor_SL_ShortfallClampsWhenSellable(t *testing.T) {
	t.Parallel()
	store := newFakeStore()
	store.seed(&database.SLTPArm{ID: 106, TelegramID: 87, TokenID: "S7", AvgPrice: 0.78, SharesAtArm: 50,
		HighWaterMark: 0.975, TPArmed: true, SLArmed: true}) // trigger 0.78, floor 0.702
	feed := newFakeFeed()
	exec := &fakeExecutor{ret: shortfallResult(1_500_000)}
	notif := &fakeNotifier{}
	m, clock := slBreachMonitor(store, feed, exec, notif)
	_ = m.Start()

	feed.setBid("S7", 0.60)
	feed.emit("S7")
	waitFor(t, func() bool { return breachStamped(m, 106) })
	clock.advance(31 * time.Second)
	feed.emit("S7")

	waitFor(t, func() bool {
		notif.mu.Lock()
		defer notif.mu.Unlock()
		return len(notif.stales) == 1
	})
	notif.mu.Lock()
	stale := notif.stales[0]
	notif.mu.Unlock()
	if stale.availableRaw != 1_500_000 {
		t.Errorf("stale notice availableRaw = %d, want 1500000 (clamp, not gone)", stale.availableRaw)
	}
	if store.armedCount("S7") != 1 {
		t.Errorf("sellable shortfall must keep the arm, got %d armed", store.armedCount("S7"))
	}

	clock.advance(31 * time.Second)
	feed.emit("S7")
	waitFor(t, func() bool {
		exec.mu.Lock()
		defer exec.mu.Unlock()
		return len(exec.calls) == 2
	})
	exec.mu.Lock()
	second := exec.calls[1].sharesRaw
	exec.mu.Unlock()
	if second != 1_500_000 {
		t.Errorf("retry sharesRaw = %d, want clamped 1500000", second)
	}
}

// TestSLTPMonitor_TP_ShortfallGoneProductionRegression is the exact
// 2026-07-26 production case: TP fires, the sell is rejected with the
// byte-exact escaped-arrow zero-balance body. The arm must be disarmed with a
// stale notice — and NO "TP failed" fired notice (in production the regex
// missed, InsufficientBalance stayed false, and the user got a failure DM
// for a position that was already gone).
func TestSLTPMonitor_TP_ShortfallGoneProductionRegression(t *testing.T) {
	t.Parallel()
	const tpShortfallZeroBody = `Order failed: {"error":"not enough balance / allowance: the balance is not enough -\u003e balance: 0, order amount: 24990000"}`
	store := newFakeStore()
	// TP trigger = 0.40; intended TP sell = 50% of 49.98 = 24.99 shares.
	store.seed(&database.SLTPArm{ID: 107, TelegramID: 88, TokenID: "S8", AvgPrice: 0.20, SharesAtArm: 49.98,
		TPArmed: true, SLArmed: true})
	feed := newFakeFeed()
	exec := &fakeExecutor{ret: &polymarket.TradeResult{
		Success:             false,
		ErrorMsg:            tpShortfallZeroBody,
		InsufficientBalance: true,
		AvailableSharesRaw:  0,
	}}
	notif := &fakeNotifier{}
	m := NewSLTPMonitor(store, feed, exec, notif, nil)
	_ = m.Start()

	feed.setBid("S8", 0.41)
	feed.emit("S8")

	waitFor(t, func() bool {
		store.mu.Lock()
		defer store.mu.Unlock()
		return store.disarmCalls == 1
	})
	waitFor(t, func() bool {
		notif.mu.Lock()
		defer notif.mu.Unlock()
		return len(notif.stales) == 1
	})
	time.Sleep(50 * time.Millisecond)

	notif.mu.Lock()
	stale := notif.stales[0]
	fires := len(notif.fires)
	notif.mu.Unlock()
	if stale.telegramID != 88 || stale.availableRaw != 0 {
		t.Errorf("stale notice = %+v, want user 88 available 0", stale)
	}
	if fires != 0 {
		t.Errorf("gone position must not produce a TP-failed fired notice, got %d", fires)
	}
	exec.mu.Lock()
	calls := len(exec.calls)
	exec.mu.Unlock()
	if calls != 1 {
		t.Errorf("expected exactly 1 sell attempt (no retry on a gone position), got %d", calls)
	}
	if store.armedCount("S8") != 0 {
		t.Errorf("arm should be auto-disarmed, got %d armed", store.armedCount("S8"))
	}
}

// TestSLTPMonitor_TP_ShortfallBelowMinValueDisarms: a TP shortfall balance of
// 2 shares at bid $0.41 ($0.82) is under the $1 minimum — gone, no clamped
// retry, no fired notice. (At a bid >= $0.50 the same balance would clamp.)
func TestSLTPMonitor_TP_ShortfallBelowMinValueDisarms(t *testing.T) {
	t.Parallel()
	store := newFakeStore()
	store.seed(&database.SLTPArm{ID: 108, TelegramID: 89, TokenID: "S9", AvgPrice: 0.20, SharesAtArm: 450,
		TPArmed: true, SLArmed: true})
	feed := newFakeFeed()
	exec := &fakeExecutor{ret: shortfallResult(2_000_000)}
	notif := &fakeNotifier{}
	m := NewSLTPMonitor(store, feed, exec, notif, nil)
	_ = m.Start()

	feed.setBid("S9", 0.41)
	feed.emit("S9")

	waitFor(t, func() bool {
		store.mu.Lock()
		defer store.mu.Unlock()
		return store.disarmCalls == 1
	})
	time.Sleep(50 * time.Millisecond)

	notif.mu.Lock()
	staleZero := len(notif.stales) == 1 && notif.stales[0].availableRaw == 0
	fires := len(notif.fires)
	notif.mu.Unlock()
	if !staleZero {
		t.Errorf("want one stale notice with availableRaw 0 (gone), got %+v", notif.stales)
	}
	if fires != 0 {
		t.Errorf("below-minimum TP shortfall must not produce a fired notice, got %d", fires)
	}
	exec.mu.Lock()
	calls := len(exec.calls)
	exec.mu.Unlock()
	if calls != 1 {
		t.Errorf("expected exactly 1 sell attempt (no clamped retry), got %d", calls)
	}
}
