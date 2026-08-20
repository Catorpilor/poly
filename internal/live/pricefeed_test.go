package live

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Catorpilor/poly/internal/polymarket"
	"github.com/gorilla/websocket"
)

// stubFetcher implements orderBookFetcher for tests.
type stubFetcher struct {
	mu       sync.Mutex
	byToken  map[string]*polymarket.OrderBook
	callCount int32
	err      error
}

func (s *stubFetcher) GetOrderBook(_ context.Context, tokenID string) (*polymarket.OrderBook, error) {
	atomic.AddInt32(&s.callCount, 1)
	if s.err != nil {
		return nil, s.err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if ob, ok := s.byToken[tokenID]; ok {
		return ob, nil
	}
	return &polymarket.OrderBook{}, nil
}

func (s *stubFetcher) set(tokenID string, ob *polymarket.OrderBook) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.byToken == nil {
		s.byToken = make(map[string]*polymarket.OrderBook)
	}
	s.byToken[tokenID] = ob
}

// newStubFetcher constructs a stub with optional preloaded books.
func newStubFetcher() *stubFetcher {
	return &stubFetcher{byToken: make(map[string]*polymarket.OrderBook)}
}

// TestPriceFeed_SubscribeSeedsFromHTTP verifies that Subscribe triggers an HTTP seed
// and populates local book state even without a WS connection.
func TestPriceFeed_SubscribeSeedsFromHTTP(t *testing.T) {
	t.Parallel()
	f := newStubFetcher()
	f.set("token1", &polymarket.OrderBook{
		Bids: []polymarket.OrderBookEntry{{Price: 0.30, Size: 100}, {Price: 0.28, Size: 200}},
		Asks: []polymarket.OrderBookEntry{{Price: 0.32, Size: 150}},
	})
	m := newPriceFeedManagerWithURL(f, "ws://unused")
	defer m.Stop()

	m.Subscribe("token1")

	got, ok := m.BestBid("token1")
	if !ok || got != 0.30 {
		t.Errorf("BestBid after seed = %v ok=%v, want 0.30 true", got, ok)
	}
	if atomic.LoadInt32(&f.callCount) != 1 {
		t.Errorf("expected 1 HTTP fetch, got %d", f.callCount)
	}
}

// TestPriceFeed_SubscribeRefCount verifies that repeated Subscribe calls don't re-seed,
// and that Unsubscribe only clears state when the ref count reaches zero.
func TestPriceFeed_SubscribeRefCount(t *testing.T) {
	t.Parallel()
	f := newStubFetcher()
	f.set("token1", &polymarket.OrderBook{
		Bids: []polymarket.OrderBookEntry{{Price: 0.40, Size: 100}},
	})
	m := newPriceFeedManagerWithURL(f, "ws://unused")
	defer m.Stop()

	m.Subscribe("token1")
	m.Subscribe("token1")
	m.Subscribe("token1")
	if c := atomic.LoadInt32(&f.callCount); c != 1 {
		t.Errorf("expected 1 seed fetch for 3 subs, got %d", c)
	}

	// First two unsubscribes shouldn't drop state
	m.Unsubscribe("token1")
	m.Unsubscribe("token1")
	if got, ok := m.BestBid("token1"); !ok || got != 0.40 {
		t.Errorf("state should persist at ref=1, got %v ok=%v", got, ok)
	}

	// Final unsubscribe drops state
	m.Unsubscribe("token1")
	if _, ok := m.BestBid("token1"); ok {
		t.Error("state should be dropped at ref=0")
	}
}

// TestPriceFeed_BookAndPriceChange runs against an in-memory WS server, verifying
// that book snapshots and price_change deltas are parsed and applied, and that
// listeners fire.
func TestPriceFeed_BookAndPriceChange(t *testing.T) {
	t.Parallel()
	f := newStubFetcher()
	// Seed returns an empty book; WS delivers the real data.
	f.set("tokenA", &polymarket.OrderBook{})

	srv, wsURL := startMockWSServer(t, func(c *websocket.Conn) {
		// Wait for subscription message
		_, _, err := c.ReadMessage()
		if err != nil {
			return
		}
		// Send a book snapshot
		book := []map[string]any{{
			"event_type": "book",
			"asset_id":   "tokenA",
			"bids": []map[string]string{
				{"price": "0.50", "size": "100"},
				{"price": "0.48", "size": "200"},
			},
			"asks": []map[string]string{
				{"price": "0.52", "size": "80"},
			},
		}}
		raw, _ := json.Marshal(book)
		_ = c.WriteMessage(websocket.TextMessage, raw)

		time.Sleep(30 * time.Millisecond)

		// Send a price_change adding a higher bid
		change := []map[string]any{{
			"event_type": "price_change",
			"asset_id":   "tokenA",
			"changes": []map[string]string{
				{"price": "0.55", "size": "50", "side": "BUY"},
			},
		}}
		raw, _ = json.Marshal(change)
		_ = c.WriteMessage(websocket.TextMessage, raw)

		// Keep the connection open by blocking on further reads (ping messages from the client).
		for {
			if _, _, err := c.ReadMessage(); err != nil {
				return
			}
		}
	})
	defer srv.Close()

	m := newPriceFeedManagerWithURL(f, wsURL)
	defer m.Stop()

	var updates int32
	m.OnUpdate(func(tokenID string) {
		if tokenID == "tokenA" {
			atomic.AddInt32(&updates, 1)
		}
	})

	m.Start()
	m.Subscribe("tokenA")

	// Wait up to 2s for both updates
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if atomic.LoadInt32(&updates) >= 2 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	if got := atomic.LoadInt32(&updates); got < 2 {
		t.Fatalf("expected >= 2 updates, got %d", got)
	}
	if got, ok := m.BestBid("tokenA"); !ok || got != 0.55 {
		t.Errorf("BestBid = %v ok=%v, want 0.55 true", got, ok)
	}
}

// TestPriceFeed_HTTPFallbackAfterDisconnect verifies that BestBid uses HTTP when the
// WS has been disconnected longer than priceFeedFallbackThreshold.
func TestPriceFeed_HTTPFallbackAfterDisconnect(t *testing.T) {
	t.Parallel()
	f := newStubFetcher()
	f.set("tokenX", &polymarket.OrderBook{
		Bids: []polymarket.OrderBookEntry{{Price: 0.22, Size: 100}},
	})
	m := newPriceFeedManagerWithURL(f, "ws://unused")
	defer m.Stop()

	m.Subscribe("tokenX") // initial HTTP seed

	// Simulate an old disconnect
	m.mu.Lock()
	m.connected = false
	m.disconnectedAt = time.Now().Add(-2 * priceFeedFallbackThreshold)
	delete(m.books, "tokenX") // local state wiped to force HTTP path
	m.mu.Unlock()

	got, ok := m.BestBid("tokenX")
	if !ok || got != 0.22 {
		t.Errorf("fallback BestBid = %v ok=%v, want 0.22 true", got, ok)
	}
	// 1 call for seed + 1 call for fallback
	if c := atomic.LoadInt32(&f.callCount); c < 2 {
		t.Errorf("expected at least 2 fetches (seed+fallback), got %d", c)
	}
}

// TestPriceFeed_BidWithFallback_UsesWSWhenFresh verifies that a recent WS book
// is used and no HTTP fetch is made.
func TestPriceFeed_BidWithFallback_UsesWSWhenFresh(t *testing.T) {
	t.Parallel()
	f := newStubFetcher()
	f.set("tokenF", &polymarket.OrderBook{
		Bids: []polymarket.OrderBookEntry{{Price: 0.30, Size: 100}},
	})
	m := newPriceFeedManagerWithURL(f, "ws://unused")
	defer m.Stop()

	m.Subscribe("tokenF") // seeds book + stamps lastUpdateAt

	// 1 fetch for the seed.
	seedCalls := atomic.LoadInt32(&f.callCount)
	bid, src, ok := m.BidWithFallback("tokenF", 1*time.Second)
	if !ok || bid != 0.30 || src != "ws" {
		t.Errorf("got bid=%v src=%s ok=%v, want 0.30 ws true", bid, src, ok)
	}
	if got := atomic.LoadInt32(&f.callCount); got != seedCalls {
		t.Errorf("expected no extra HTTP fetch, got %d (seed=%d)", got, seedCalls)
	}
}

// TestPriceFeed_BidWithFallback_FallsBackToHTTPWhenStale verifies that an
// older-than-maxAge update triggers an HTTP fetch and returns its bid.
func TestPriceFeed_BidWithFallback_FallsBackToHTTPWhenStale(t *testing.T) {
	t.Parallel()
	f := newStubFetcher()
	f.set("tokenG", &polymarket.OrderBook{
		Bids: []polymarket.OrderBookEntry{{Price: 0.07, Size: 50}},
	})
	m := newPriceFeedManagerWithURL(f, "ws://unused")
	defer m.Stop()

	m.Subscribe("tokenG") // initial seed at t=now

	// Backdate the per-token timestamp to force stale path.
	m.mu.Lock()
	m.lastUpdateAt["tokenG"] = time.Now().Add(-1 * time.Hour)
	m.mu.Unlock()

	bid, src, ok := m.BidWithFallback("tokenG", 30*time.Second)
	if !ok || bid != 0.07 || src != "http" {
		t.Errorf("got bid=%v src=%s ok=%v, want 0.07 http true", bid, src, ok)
	}
	// 1 seed + 1 fallback.
	if got := atomic.LoadInt32(&f.callCount); got < 2 {
		t.Errorf("expected >=2 fetches (seed + fallback), got %d", got)
	}
}

// TestPriceFeed_BidWithFallback_HTTPErrorReturnsNotOK verifies the not-ok
// branch when the HTTP fetcher fails.
func TestPriceFeed_BidWithFallback_HTTPErrorReturnsNotOK(t *testing.T) {
	t.Parallel()
	f := newStubFetcher()
	m := newPriceFeedManagerWithURL(f, "ws://unused")
	defer m.Stop()

	// Don't seed; lastUpdateAt is zero so stale path runs immediately.
	// Now make the fetcher error.
	f.err = http.ErrServerClosed

	bid, _, ok := m.BidWithFallback("tokenH", 30*time.Second)
	if ok || bid != 0 {
		t.Errorf("expected not-ok zero bid on HTTP error, got bid=%v ok=%v", bid, ok)
	}
}

// TestPriceFeed_LastUpdateAt_StampedOnPriceChange verifies that the per-token
// freshness timer is updated when a price_change applies, even with no book event.
func TestPriceFeed_LastUpdateAt_StampedOnPriceChange(t *testing.T) {
	t.Parallel()
	f := newStubFetcher()
	m := newPriceFeedManagerWithURL(f, "ws://unused")
	defer m.Stop()

	// Seed via subscribe.
	f.set("tokenI", &polymarket.OrderBook{
		Bids: []polymarket.OrderBookEntry{{Price: 0.10, Size: 100}},
	})
	m.Subscribe("tokenI")

	// Backdate the seed stamp, then apply a price_change.
	m.mu.Lock()
	old := time.Now().Add(-1 * time.Hour)
	m.lastUpdateAt["tokenI"] = old
	m.mu.Unlock()

	m.applyPriceChanges("tokenI", []PriceChange{{Price: 0.12, Size: 200, Side: "BUY"}})

	m.mu.RLock()
	stamped := m.lastUpdateAt["tokenI"]
	m.mu.RUnlock()
	if !stamped.After(old) {
		t.Errorf("price_change should refresh lastUpdateAt; got %v vs %v", stamped, old)
	}
}

// TestPriceFeed_LastUpdateAt_DroppedOnUnsubscribe verifies that token state is
// fully dropped when the ref count reaches zero.
func TestPriceFeed_LastUpdateAt_DroppedOnUnsubscribe(t *testing.T) {
	t.Parallel()
	f := newStubFetcher()
	f.set("tokenJ", &polymarket.OrderBook{
		Bids: []polymarket.OrderBookEntry{{Price: 0.20, Size: 50}},
	})
	m := newPriceFeedManagerWithURL(f, "ws://unused")
	defer m.Stop()

	m.Subscribe("tokenJ")
	m.mu.RLock()
	_, hadStamp := m.lastUpdateAt["tokenJ"]
	m.mu.RUnlock()
	if !hadStamp {
		t.Fatal("expected lastUpdateAt to be stamped after Subscribe")
	}

	m.Unsubscribe("tokenJ")
	m.mu.RLock()
	_, stillThere := m.lastUpdateAt["tokenJ"]
	m.mu.RUnlock()
	if stillThere {
		t.Error("expected lastUpdateAt to be cleared after final Unsubscribe")
	}
}

// TestPriceFeed_LastTradePrice_SELL_UpdatesBid verifies that a SELL-side
// last_trade_price overrides the HTTP-seeded book bid (since SELL trades hit
// the best bid, the trade price is a direct bid observation).
func TestPriceFeed_LastTradePrice_SELL_UpdatesBid(t *testing.T) {
	t.Parallel()
	f := newStubFetcher()
	f.set("tokenLT1", &polymarket.OrderBook{
		Bids: []polymarket.OrderBookEntry{{Price: 0.36, Size: 100}},
	})
	m := newPriceFeedManagerWithURL(f, "ws://unused")
	defer m.Stop()

	m.Subscribe("tokenLT1") // seeds book at 0.36

	if got, ok := m.BestBid("tokenLT1"); !ok || got != 0.36 {
		t.Fatalf("post-seed BestBid = %v ok=%v, want 0.36 true", got, ok)
	}

	m.applyLastTradePrice("tokenLT1", "0.20", "SELL")
	if got, ok := m.BestBid("tokenLT1"); !ok || got != 0.20 {
		t.Errorf("after SELL trade: BestBid = %v ok=%v, want 0.20 true", got, ok)
	}
}

// TestPriceFeed_LastTradePrice_BUY_DoesNotChangeBid verifies that a BUY-side
// last_trade_price (which hits the ask, not the bid) does NOT change the
// returned bid value.
func TestPriceFeed_LastTradePrice_BUY_DoesNotChangeBid(t *testing.T) {
	t.Parallel()
	f := newStubFetcher()
	f.set("tokenLT2", &polymarket.OrderBook{
		Bids: []polymarket.OrderBookEntry{{Price: 0.36, Size: 100}},
	})
	m := newPriceFeedManagerWithURL(f, "ws://unused")
	defer m.Stop()

	m.Subscribe("tokenLT2")

	m.applyLastTradePrice("tokenLT2", "0.40", "BUY")
	if got, ok := m.BestBid("tokenLT2"); !ok || got != 0.36 {
		t.Errorf("after BUY trade: BestBid = %v ok=%v, want 0.36 true (BUY hits ask, not bid)", got, ok)
	}
}

// TestPriceFeed_LastTradePrice_StampsLastUpdate verifies that both BUY and
// SELL last_trade_price events advance the per-token freshness timestamp so
// BidWithFallback's staleness check is satisfied.
func TestPriceFeed_LastTradePrice_StampsLastUpdate(t *testing.T) {
	t.Parallel()
	for _, side := range []string{"SELL", "BUY"} {
		side := side
		t.Run(side, func(t *testing.T) {
			t.Parallel()
			f := newStubFetcher()
			m := newPriceFeedManagerWithURL(f, "ws://unused")
			defer m.Stop()

			m.Subscribe("tok-" + side)
			// Backdate to simulate an aged seed.
			m.mu.Lock()
			m.lastUpdateAt["tok-"+side] = time.Now().Add(-1 * time.Hour)
			m.mu.Unlock()

			m.applyLastTradePrice("tok-"+side, "0.50", side)

			m.mu.RLock()
			stamp := m.lastUpdateAt["tok-"+side]
			m.mu.RUnlock()
			if time.Since(stamp) > 5*time.Second {
				t.Errorf("%s side did not refresh lastUpdateAt; got %v ago", side, time.Since(stamp))
			}
		})
	}
}

// TestPriceFeed_LastTradePrice_NotifiesListeners verifies that listeners fire
// on a last_trade_price event so the SLTPMonitor evaluates immediately.
func TestPriceFeed_LastTradePrice_NotifiesListeners(t *testing.T) {
	t.Parallel()
	f := newStubFetcher()
	m := newPriceFeedManagerWithURL(f, "ws://unused")
	defer m.Stop()

	m.Subscribe("tokenLT3")
	var calls int32
	m.OnUpdate(func(id string) {
		if id == "tokenLT3" {
			atomic.AddInt32(&calls, 1)
		}
	})

	// Use the wire-format dispatch path to exercise the parser too.
	raw := []byte(`{"event_type":"last_trade_price","asset_id":"tokenLT3","price":"0.30","side":"SELL"}`)
	m.dispatchEvent(raw)

	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Errorf("expected 1 listener call, got %d", got)
	}
	if got, ok := m.BestBid("tokenLT3"); !ok || got != 0.30 {
		t.Errorf("expected BestBid=0.30 after parsing SELL trade; got %v ok=%v", got, ok)
	}
}

// TestPriceFeed_BidWithFallback_PrefersTradeOverSeed confirms that when both
// trade-derived and seed bids are present and fresh, BidWithFallback returns
// the trade-derived value with source "ws".
func TestPriceFeed_BidWithFallback_PrefersTradeOverSeed(t *testing.T) {
	t.Parallel()
	f := newStubFetcher()
	f.set("tokenLT4", &polymarket.OrderBook{
		Bids: []polymarket.OrderBookEntry{{Price: 0.36, Size: 100}},
	})
	m := newPriceFeedManagerWithURL(f, "ws://unused")
	defer m.Stop()

	m.Subscribe("tokenLT4")
	m.applyLastTradePrice("tokenLT4", "0.20", "SELL")

	bid, src, ok := m.BidWithFallback("tokenLT4", 30*time.Second)
	if !ok || bid != 0.20 || src != "ws" {
		t.Errorf("got bid=%v src=%s ok=%v, want 0.20 ws true", bid, src, ok)
	}
}

// TestPriceFeed_SellVWAP verifies the depth-aware accessor reads the local WS
// book: a seeded token walks its bids best-first, an unsubscribed token has no
// book and fails open (ok=false).
func TestPriceFeed_SellVWAP(t *testing.T) {
	t.Parallel()
	f := newStubFetcher()
	f.set("tokenVW", &polymarket.OrderBook{
		Bids: []polymarket.OrderBookEntry{{Price: 0.52, Size: 20}, {Price: 0.30, Size: 100}, {Price: 0.20, Size: 500}},
	})
	m := newPriceFeedManagerWithURL(f, "ws://unused")
	defer m.Stop()

	// No subscription yet: no local book → fail-open signal.
	if _, _, ok := m.SellVWAP("tokenVW", 100); ok {
		t.Fatalf("SellVWAP before subscribe should be ok=false")
	}

	m.Subscribe("tokenVW") // HTTP-seeds the book

	// Selling 100 spans the thin 0.52 top (a phantom print) into real depth:
	// 20@0.52 + 80@0.30 = 34.4 / 100 = 0.344, well under the 0.52 print.
	vwap, depth, ok := m.SellVWAP("tokenVW", 100)
	if !ok {
		t.Fatalf("SellVWAP after seed should be ok=true")
	}
	if want := (0.52*20 + 0.30*80) / 100; math.Abs(vwap-want) > 1e-9 {
		t.Errorf("vwap = %v, want %v", vwap, want)
	}
	if math.Abs(depth-620) > 1e-9 {
		t.Errorf("depth = %v, want 620", depth)
	}
}

// TestPriceFeed_DeferredConnect_NoSubsNoDial verifies that connectionLoop does
// not dial the WS while subCount is empty (which would otherwise produce a
// reconnect loop because Polymarket closes empty connections).
func TestPriceFeed_DeferredConnect_NoSubsNoDial(t *testing.T) {
	t.Parallel()
	f := newStubFetcher()
	// Point at an httptest server that records dial attempts but never
	// upgrades — any dial would hang or fail and we'd see calls.
	var dialAttempts int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&dialAttempts, 1)
	}))
	defer srv.Close()
	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http")

	m := newPriceFeedManagerWithURL(f, wsURL)
	defer m.Stop()
	m.Start()

	// Wait long enough that, if the loop were dialing, we'd see attempts.
	time.Sleep(150 * time.Millisecond)

	if got := atomic.LoadInt32(&dialAttempts); got != 0 {
		t.Errorf("expected 0 dial attempts with no subs, got %d", got)
	}
}

// TestPriceFeed_DeferredConnect_ConnectsAfterSubscribe verifies that the
// subscribe signal wakes connectionLoop and triggers a dial.
func TestPriceFeed_DeferredConnect_ConnectsAfterSubscribe(t *testing.T) {
	t.Parallel()
	f := newStubFetcher()
	f.set("tokenDC", &polymarket.OrderBook{})

	srv, wsURL := startMockWSServer(t, func(c *websocket.Conn) {
		// Echo subscribe receipt and hold the connection open.
		_, _, _ = c.ReadMessage()
		for {
			if _, _, err := c.ReadMessage(); err != nil {
				return
			}
		}
	})
	defer srv.Close()

	m := newPriceFeedManagerWithURL(f, wsURL)
	defer m.Stop()
	m.Start()

	// No dial yet.
	time.Sleep(50 * time.Millisecond)
	m.mu.RLock()
	connectedBefore := m.connected
	m.mu.RUnlock()
	if connectedBefore {
		t.Fatal("connection should not be established before any subscribe")
	}

	// Subscribe wakes the loop.
	m.Subscribe("tokenDC")

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		m.mu.RLock()
		c := m.connected
		m.mu.RUnlock()
		if c {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("expected connection after Subscribe, never got connected")
}

// TestPriceFeed_ConcurrentWSWrites_NoPanic reproduces issue #48: pingLoop and
// resubscribeAll wrote to the shared WS conn without serialization, and
// gorilla/websocket panics with "concurrent write to websocket connection"
// when two writers collide. Subscribe/Unsubscribe call resubscribeAll on every
// watch-list change, so mid-session churn raced the periodic PING in prod.
func TestPriceFeed_ConcurrentWSWrites_NoPanic(t *testing.T) {
	t.Parallel()
	_, wsURL := startMockWSServer(t, func(c *websocket.Conn) {
		for {
			if _, _, err := c.ReadMessage(); err != nil {
				return
			}
		}
	})

	f := newStubFetcher()
	m := newPriceFeedManagerWithURL(f, wsURL)
	defer m.Stop()

	m.Subscribe("token1") // non-empty sub list so resubscribeAll actually writes
	m.Start()

	deadline := time.Now().Add(2 * time.Second)
	for {
		m.mu.RLock()
		connected := m.connected
		m.mu.RUnlock()
		if connected {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("never connected to mock WS server")
		}
		time.Sleep(10 * time.Millisecond)
	}

	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 300; i++ {
				m.resubscribeAll()
			}
		}()
	}
	wg.Wait()
}

// --- issue #42: reconnect-on-membership-change ---

// recordedConn captures the subscribe frames seen on one WS connection.
type recordedConn struct {
	subFrames [][]string // each subscribe frame's asset list, in arrival order
}

// wsRecorder records every WS connection and the subscribe frames it received.
// A mid-session subscribe (the rejected path we're removing) shows up as a
// second frame on an already-established connection; the reconnect fix instead
// produces a brand-new connection whose connect-time frame carries the full
// list.
type wsRecorder struct {
	mu    sync.Mutex
	conns []*recordedConn
}

func (r *wsRecorder) newConn() *recordedConn {
	r.mu.Lock()
	defer r.mu.Unlock()
	c := &recordedConn{}
	r.conns = append(r.conns, c)
	return c
}

func (r *wsRecorder) addSub(c *recordedConn, assets []string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	c.subFrames = append(c.subFrames, assets)
}

func (r *wsRecorder) connCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.conns)
}

// connSubs returns a copy of connection i's recorded subscribe frames.
func (r *wsRecorder) connSubs(i int) [][]string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if i < 0 || i >= len(r.conns) {
		return nil
	}
	return append([][]string(nil), r.conns[i].subFrames...)
}

// startRecordingWSServer upgrades each connection and records its subscribe
// frames, holding the connection open until the client closes it.
func startRecordingWSServer(t *testing.T) (*wsRecorder, string) {
	t.Helper()
	rec := &wsRecorder{}
	_, wsURL := startMockWSServer(t, func(c *websocket.Conn) {
		rc := rec.newConn()
		for {
			mt, data, err := c.ReadMessage()
			if err != nil {
				return
			}
			if mt != websocket.TextMessage {
				continue
			}
			if strings.EqualFold(strings.TrimSpace(string(data)), "PING") {
				continue
			}
			var payload struct {
				Type      string   `json:"type"`
				AssetsIDs []string `json:"assets_ids"`
			}
			if err := json.Unmarshal(data, &payload); err == nil && payload.Type == "market" {
				rec.addSub(rc, payload.AssetsIDs)
			}
		}
	})
	return rec, wsURL
}

// waitFor polls cond until it holds or the deadline passes.
func waitUntil(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for: %s", what)
}

func waitConnected(t *testing.T, m *PriceFeedManager) {
	t.Helper()
	waitUntil(t, "ws connected", func() bool {
		m.mu.RLock()
		defer m.mu.RUnlock()
		return m.connected
	})
}

func containsAll(list []string, want ...string) bool {
	set := make(map[string]bool, len(list))
	for _, s := range list {
		set[s] = true
	}
	for _, w := range want {
		if !set[w] {
			return false
		}
	}
	return true
}

func contains(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

// TestPriceFeed_SubscribeAfterConnect_Reconnects: a mid-session Subscribe must
// NOT push a subscribe frame onto the live connection (the server rejects that
// with "INVALID OPERATION", issue #42). Instead it forces a fresh connection
// whose connect-time subscribe carries the full list including the new token.
func TestPriceFeed_SubscribeAfterConnect_Reconnects(t *testing.T) {
	t.Parallel()
	f := newStubFetcher()
	rec, wsURL := startRecordingWSServer(t)
	m := newPriceFeedManagerWithURL(f, wsURL)
	m.reconnectInterval = 100 * time.Millisecond
	defer m.Stop()

	m.Subscribe("t1") // before Start → rides the connect-time subscribe
	m.Start()
	waitUntil(t, "conn #1 with its connect-time subscribe", func() bool {
		return rec.connCount() == 1 && len(rec.connSubs(0)) == 1
	})
	waitConnected(t, m)

	// Mid-session add.
	m.Subscribe("t2")

	waitUntil(t, "conn #2 (reconnect) with its subscribe", func() bool {
		return rec.connCount() == 2 && len(rec.connSubs(1)) >= 1
	})
	subs2 := rec.connSubs(1)
	if len(subs2) == 0 || !containsAll(subs2[0], "t1", "t2") {
		t.Fatalf("conn #2 connect-time subscribe = %v, want it to include t1 and t2", subs2)
	}
	// The original connection must never have received a second subscribe frame.
	if got := len(rec.connSubs(0)); got != 1 {
		t.Errorf("conn #1 subscribe frames = %d, want 1 (no mid-session subscribe on a live conn)", got)
	}
}

// TestPriceFeed_SubscribeBurst_CoalescesToOneReconnect: a burst of membership
// changes must debounce into exactly ONE reconnect, not one per change.
func TestPriceFeed_SubscribeBurst_CoalescesToOneReconnect(t *testing.T) {
	t.Parallel()
	f := newStubFetcher()
	rec, wsURL := startRecordingWSServer(t)
	m := newPriceFeedManagerWithURL(f, wsURL)
	m.reconnectInterval = 250 * time.Millisecond
	defer m.Stop()

	m.Subscribe("t1")
	m.Start()
	waitUntil(t, "conn #1", func() bool { return rec.connCount() == 1 })
	waitConnected(t, m)

	// Rapid burst while connected — coalesces into one reconnect.
	for _, tok := range []string{"t2", "t3", "t4", "t5"} {
		m.Subscribe(tok)
	}

	waitUntil(t, "conn #2 (single coalesced reconnect) with its subscribe", func() bool {
		return rec.connCount() == 2 && len(rec.connSubs(1)) >= 1
	})
	// No THIRD connection may appear after another debounce window elapses.
	time.Sleep(3 * m.reconnectInterval)
	if got := rec.connCount(); got != 2 {
		t.Errorf("connections = %d, want exactly 2 (burst must coalesce to one reconnect)", got)
	}
	if subs := rec.connSubs(1); len(subs) == 0 || !containsAll(subs[0], "t1", "t2", "t3", "t4", "t5") {
		t.Errorf("conn #2 subscribe = %v, want the full five-token list", subs)
	}
}

// TestPriceFeed_Unsubscribe_ExcludedFromNextSubscribe: after Unsubscribe, the
// next connection's connect-time subscribe must omit the dropped token.
func TestPriceFeed_Unsubscribe_ExcludedFromNextSubscribe(t *testing.T) {
	t.Parallel()
	f := newStubFetcher()
	rec, wsURL := startRecordingWSServer(t)
	m := newPriceFeedManagerWithURL(f, wsURL)
	m.reconnectInterval = 100 * time.Millisecond
	defer m.Stop()

	m.Subscribe("t1")
	m.Subscribe("t2")
	m.Start()
	waitUntil(t, "conn #1 with both tokens", func() bool {
		return rec.connCount() == 1 && len(rec.connSubs(0)) == 1 && containsAll(rec.connSubs(0)[0], "t1", "t2")
	})
	waitConnected(t, m)

	m.Unsubscribe("t2")

	waitUntil(t, "conn #2 (reconnect) with its subscribe", func() bool {
		return rec.connCount() == 2 && len(rec.connSubs(1)) >= 1
	})
	got := rec.connSubs(1)
	if len(got) == 0 || !contains(got[0], "t1") || contains(got[0], "t2") {
		t.Errorf("conn #2 subscribe = %v, want [t1] without t2", got)
	}
}

// TestPriceFeed_Stop_PendingDebounce_NoReconnect: Stop() during a pending
// debounce must cancel the timer — no reconnect fires after Stop, and no
// goroutine leaks (best-effort).
func TestPriceFeed_Stop_PendingDebounce_NoReconnect(t *testing.T) {
	baseline := runtime.NumGoroutine()
	f := newStubFetcher()
	rec, wsURL := startRecordingWSServer(t)
	m := newPriceFeedManagerWithURL(f, wsURL)
	m.reconnectInterval = 500 * time.Millisecond // long enough to Stop mid-pending
	m.Subscribe("t1")
	m.Start()
	waitUntil(t, "conn #1", func() bool { return rec.connCount() == 1 })
	waitConnected(t, m)

	m.Subscribe("t2") // arms the debounce timer (connected)
	m.Stop()          // before the timer fires

	// Past the debounce window, no reconnect may have occurred.
	time.Sleep(3 * m.reconnectInterval)
	if got := rec.connCount(); got != 1 {
		t.Errorf("connections = %d after Stop with a pending debounce, want 1 (no reconnect)", got)
	}

	// Best-effort goroutine-leak check: the loop/ping/timer goroutines should
	// have wound down. Generous margin — the shared runtime carries other
	// parallel tests' goroutines.
	settled := baseline + 8
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && runtime.NumGoroutine() > settled {
		time.Sleep(20 * time.Millisecond)
	}
	if got := runtime.NumGoroutine(); got > settled {
		t.Logf("goroutines after Stop = %d (baseline %d) — possible leak", got, baseline)
	}
}

// startMockWSServer spins up an httptest server that upgrades to WebSocket and
// hands the connection to the provided handler.
func startMockWSServer(t *testing.T, handler func(*websocket.Conn)) (*httptest.Server, string) {
	t.Helper()
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Logf("upgrade: %v", err)
			return
		}
		defer c.Close()
		handler(c)
	}))
	t.Cleanup(srv.Close)
	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http")
	return srv, wsURL
}
