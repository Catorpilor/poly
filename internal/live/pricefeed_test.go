package live

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
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
