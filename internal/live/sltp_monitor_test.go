package live

import (
	"context"
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

type fakeFeed struct {
	mu            sync.Mutex
	bids          map[string]float64
	asks          map[string]float64
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

	// Lottery-related fields. When unset, ResolveOtherToken returns the
	// configured "<armToken>-other" stub and ExecuteLotteryBuy succeeds.
	otherTokenID    string
	otherOutcome    string
	resolveErr      error
	lotteryRet      *polymarket.TradeResult
	lotteryCalls    []lotteryCall
}

type executorCall struct {
	armID     int
	sharesRaw int64
}

type lotteryCall struct {
	armID        int
	otherTokenID string
	otherOutcome string
	maxSpend     float64
	maxPrice     float64
}

func (e *fakeExecutor) ExecuteSell(_ context.Context, arm *database.SLTPArm, sharesRaw int64) *polymarket.TradeResult {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.calls = append(e.calls, executorCall{armID: arm.ID, sharesRaw: sharesRaw})
	if e.ret != nil {
		return e.ret
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
	mu       sync.Mutex
	fires    []fireNotice
	paused   []int64 // telegramIDs notified of pause
	lotteries []lotteryNotice
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

func (n *fakeNotifier) NotifySLTPPaused(telegramID int64, _ *database.SLTPArm) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.paused = append(n.paused, telegramID)
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

func TestSLTPMonitor_SLFiresAtMinus30(t *testing.T) {
	t.Parallel()
	store := newFakeStore()
	arm := &database.SLTPArm{ID: 11, TelegramID: 7, TokenID: "U", AvgPrice: 0.50, SharesAtArm: 80, TPArmed: true, SLArmed: true}
	store.seed(arm)

	feed := newFakeFeed()
	exec := &fakeExecutor{}
	notif := &fakeNotifier{}
	m := NewSLTPMonitor(store, feed, exec, notif, nil)
	_ = m.Start()

	feed.setBid("U", 0.34) // <= 0.50*0.70 = 0.35
	feed.emit("U")

	waitFor(t, func() bool {
		notif.mu.Lock()
		defer notif.mu.Unlock()
		return len(notif.fires) == 1
	})

	exec.mu.Lock()
	shares := exec.calls[0].sharesRaw
	exec.mu.Unlock()
	if shares != int64(80*1e6) {
		t.Errorf("expected SL sell 80e6 shares, got %d", shares)
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
	// TP already fired previously: TPArmed=false, SLArmed=true
	arm := &database.SLTPArm{ID: 12, TelegramID: 9, TokenID: "V", AvgPrice: 0.20, SharesAtArm: 100, TPArmed: false, SLArmed: true}
	store.seed(arm)

	feed := newFakeFeed()
	exec := &fakeExecutor{}
	notif := &fakeNotifier{}
	m := NewSLTPMonitor(store, feed, exec, notif, nil)
	_ = m.Start()

	feed.setBid("V", 0.13) // <= 0.20*0.70 = 0.14
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
	// SL threshold = 0.10 * 0.70 = 0.07
	store.seed(&database.SLTPArm{ID: 30, TelegramID: 9, TokenID: "L", AvgPrice: 0.10, SharesAtArm: 900, TPArmed: true, SLArmed: true})

	feed := newFakeFeed()
	// Stale WS bid would not have triggered SL (above threshold), but the
	// fallback returns the live HTTP value below threshold.
	feed.setBid("L", 0.10)
	feed.setFallbackBid("L", 0.06, "http", true)

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
