package live

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Catorpilor/poly/internal/polymarket"
	"github.com/gorilla/websocket"
)

const (
	clobMarketWSURL            = "wss://ws-subscriptions-clob.polymarket.com/ws/market"
	priceFeedPingInterval      = 10 * time.Second
	priceFeedStaleTimeout      = 60 * time.Second
	priceFeedReconnectBackoff  = 5 * time.Second
	priceFeedFallbackThreshold = 30 * time.Second
)

// orderBookFetcher fetches book snapshots via HTTP for seeding and fallback.
type orderBookFetcher interface {
	GetOrderBook(ctx context.Context, tokenID string) (*polymarket.OrderBook, error)
}

// PriceUpdateListener is invoked whenever the best-bid for a tokenID may have changed.
type PriceUpdateListener func(tokenID string)

// PriceFeedManager maintains real-time best-bid state for subscribed tokenIDs via the
// Polymarket CLOB market WebSocket, with HTTP fallback when the WS is disconnected too long.
type PriceFeedManager struct {
	ctx     context.Context
	cancel  context.CancelFunc
	fetcher orderBookFetcher
	wsURL   string

	mu             sync.RWMutex
	conn           *websocket.Conn
	connected      bool
	subCount       map[string]int // tokenID -> ref count
	books          map[string]*bookState
	listeners      []PriceUpdateListener
	lastMsgAt      time.Time
	disconnectedAt time.Time
	// lastUpdateAt tracks the wall time of the most recent WS event applied for
	// each subscribed tokenID (book / price_change / last_trade_price). Used to
	// detect a per-token silent feed — the connection-level lastMsgAt is kept
	// fresh by PONGs even when no events flow for a specific token.
	lastUpdateAt map[string]time.Time
	// tradeBids holds the most recent SELL-side trade price observed via
	// last_trade_price events. A SELL trade hits the best bid, so this is a
	// fresh proxy for the live best-bid level — typically more accurate than
	// the static HTTP seed in book once trading begins.
	tradeBids map[string]float64
	// subscribeSignal wakes connectionLoop the moment the first subscribe
	// arrives. Polymarket's market WS forcibly closes connections that don't
	// send a subscribe within a few seconds, so we don't dial until we have
	// something to send.
	subscribeSignal chan struct{}
}

// NewPriceFeedManager creates a manager using the production CLOB market WS URL.
func NewPriceFeedManager(fetcher orderBookFetcher) *PriceFeedManager {
	return newPriceFeedManagerWithURL(fetcher, clobMarketWSURL)
}

// newPriceFeedManagerWithURL is an internal constructor for tests to inject a mock WS URL.
func newPriceFeedManagerWithURL(fetcher orderBookFetcher, wsURL string) *PriceFeedManager {
	ctx, cancel := context.WithCancel(context.Background())
	return &PriceFeedManager{
		ctx:             ctx,
		cancel:          cancel,
		fetcher:         fetcher,
		wsURL:           wsURL,
		subCount:        make(map[string]int),
		books:           make(map[string]*bookState),
		lastUpdateAt:    make(map[string]time.Time),
		tradeBids:       make(map[string]float64),
		subscribeSignal: make(chan struct{}, 1),
	}
}

// Start launches the connect/reconnect loop.
func (m *PriceFeedManager) Start() {
	go m.connectionLoop()
}

// Stop closes the connection and cancels all goroutines.
func (m *PriceFeedManager) Stop() {
	m.cancel()
	m.mu.Lock()
	if m.conn != nil {
		_ = m.conn.Close()
		m.conn = nil
	}
	m.connected = false
	m.mu.Unlock()
}

// Subscribe increments the ref count for tokenID. First subscribe triggers an HTTP seed
// and updates the WS subscription. Safe to call before Start.
func (m *PriceFeedManager) Subscribe(tokenID string) {
	if tokenID == "" {
		return
	}
	m.mu.Lock()
	m.subCount[tokenID]++
	first := m.subCount[tokenID] == 1
	m.mu.Unlock()

	if first {
		if err := m.seedBook(tokenID); err != nil {
			log.Printf("PriceFeedManager: HTTP seed failed for %s: %v", tokenID, err)
		}
		// Wake the connection loop in case it's idle waiting for the first
		// subscription. Non-blocking — buffered chan with cap 1.
		select {
		case m.subscribeSignal <- struct{}{}:
		default:
		}
		m.resubscribeAll()
	}
}

// Unsubscribe decrements the ref count. At 0, drops the token from local state
// and resends the subscription list to the WS.
func (m *PriceFeedManager) Unsubscribe(tokenID string) {
	m.mu.Lock()
	if m.subCount[tokenID] > 0 {
		m.subCount[tokenID]--
	}
	zero := m.subCount[tokenID] == 0
	if zero {
		delete(m.subCount, tokenID)
		delete(m.books, tokenID)
		delete(m.lastUpdateAt, tokenID)
		delete(m.tradeBids, tokenID)
	}
	m.mu.Unlock()

	if zero {
		m.resubscribeAll()
	}
}

// BestBid returns the current best bid from local state. If the WS has been
// disconnected longer than priceFeedFallbackThreshold, falls back to an HTTP fetch.
//
// Precedence (within local state): a SELL last_trade_price observation wins
// over the static HTTP-seeded book. The market WS pushes last_trade_price
// frames continuously; our book snapshot is only refreshed at subscribe time
// (Polymarket rarely sends `book`/`price_change` on the market channel), so
// the trade-derived bid is almost always the freshest signal.
func (m *PriceFeedManager) BestBid(tokenID string) (float64, bool) {
	m.mu.RLock()
	book := m.books[tokenID]
	tradeBid, hasTrade := m.tradeBids[tokenID]
	connected := m.connected
	discAt := m.disconnectedAt
	m.mu.RUnlock()

	if !connected && !discAt.IsZero() && time.Since(discAt) > priceFeedFallbackThreshold {
		if price, ok := m.httpBestBid(tokenID); ok {
			return price, true
		}
	}
	if hasTrade && tradeBid > 0 {
		return tradeBid, true
	}
	if book == nil {
		return 0, false
	}
	return book.BestBid()
}

// BidWithFallback returns the best bid for tokenID with an HTTP backstop:
//   - if the WS-cached book has a positive bid AND the per-token last update is
//     within maxAge, returns that bid with source "ws"
//   - otherwise issues an HTTP fetch and returns its bid with source "http"
//
// This is the read path for the SL/TP monitor's periodic tick: a per-token
// staleness check is needed because connection-level health (lastMsgAt) is
// kept fresh by PONGs even when one specific token's WS subscription goes
// silent.
func (m *PriceFeedManager) BidWithFallback(tokenID string, maxAge time.Duration) (float64, string, bool) {
	m.mu.RLock()
	book := m.books[tokenID]
	tradeBid, hasTrade := m.tradeBids[tokenID]
	lastUpd := m.lastUpdateAt[tokenID]
	m.mu.RUnlock()

	if !lastUpd.IsZero() && time.Since(lastUpd) <= maxAge {
		// Trade-derived bid wins (see BestBid for rationale).
		if hasTrade && tradeBid > 0 {
			return tradeBid, "ws", true
		}
		if book != nil {
			if bid, ok := book.BestBid(); ok && bid > 0 {
				return bid, "ws", true
			}
		}
	}
	if bid, ok := m.httpBestBid(tokenID); ok {
		return bid, "http", true
	}
	return 0, "http", false
}

// OnUpdate registers a listener invoked after each book snapshot or price_change
// applied for a subscribed tokenID. Listeners are called synchronously in the read loop.
func (m *PriceFeedManager) OnUpdate(l PriceUpdateListener) {
	m.mu.Lock()
	m.listeners = append(m.listeners, l)
	m.mu.Unlock()
}

// seedBook pulls a snapshot via HTTP and applies it to local state.
func (m *PriceFeedManager) seedBook(tokenID string) error {
	ctx, cancel := context.WithTimeout(m.ctx, 10*time.Second)
	defer cancel()
	ob, err := m.fetcher.GetOrderBook(ctx, tokenID)
	if err != nil {
		return err
	}
	m.applyBookSnapshot(tokenID, orderBookEntriesToLevels(ob.Bids), orderBookEntriesToLevels(ob.Asks))
	return nil
}

func orderBookEntriesToLevels(entries []polymarket.OrderBookEntry) []BookLevel {
	out := make([]BookLevel, 0, len(entries))
	for _, e := range entries {
		out = append(out, BookLevel{Price: e.Price, Size: e.Size})
	}
	return out
}

func (m *PriceFeedManager) httpBestBid(tokenID string) (float64, bool) {
	ctx, cancel := context.WithTimeout(m.ctx, 5*time.Second)
	defer cancel()
	ob, err := m.fetcher.GetOrderBook(ctx, tokenID)
	if err != nil {
		log.Printf("PriceFeedManager: HTTP fallback fetch for %s: %v", tokenID, err)
		return 0, false
	}
	var best float64
	found := false
	for _, lvl := range ob.Bids {
		if lvl.Size <= 0 {
			continue
		}
		if !found || lvl.Price > best {
			best = lvl.Price
			found = true
		}
	}
	return best, found
}

func (m *PriceFeedManager) connectionLoop() {
	for {
		select {
		case <-m.ctx.Done():
			return
		default:
		}

		// Don't dial until we have at least one subscription. Polymarket's
		// market WS forcibly closes connections that don't send a subscribe
		// frame within a few seconds — without this gate, an empty bot enters
		// a tight reconnect loop.
		m.mu.RLock()
		hasSubs := len(m.subCount) > 0
		m.mu.RUnlock()
		if !hasSubs {
			select {
			case <-m.ctx.Done():
				return
			case <-m.subscribeSignal:
			}
			continue
		}

		if err := m.connect(); err != nil {
			log.Printf("PriceFeedManager: connect error: %v; retry in %v", err, priceFeedReconnectBackoff)
			select {
			case <-m.ctx.Done():
				return
			case <-time.After(priceFeedReconnectBackoff):
			}
			continue
		}

		m.readLoop()

		m.mu.Lock()
		m.connected = false
		m.disconnectedAt = time.Now()
		if m.conn != nil {
			_ = m.conn.Close()
			m.conn = nil
		}
		m.mu.Unlock()

		select {
		case <-m.ctx.Done():
			return
		case <-time.After(priceFeedReconnectBackoff):
		}
	}
}

func (m *PriceFeedManager) connect() error {
	dialer := websocket.Dialer{
		ReadBufferSize:   65536,
		WriteBufferSize:  4096,
		HandshakeTimeout: 30 * time.Second,
	}
	if proxyURL := os.Getenv("HTTPS_PROXY"); proxyURL != "" {
		if parsed, err := url.Parse(proxyURL); err == nil {
			dialer.Proxy = http.ProxyURL(parsed)
		}
	} else if proxyURL := os.Getenv("HTTP_PROXY"); proxyURL != "" {
		if parsed, err := url.Parse(proxyURL); err == nil {
			dialer.Proxy = http.ProxyURL(parsed)
		}
	}

	log.Printf("PriceFeedManager: Connecting to %s...", m.wsURL)
	conn, _, err := dialer.Dial(m.wsURL, nil)
	if err != nil {
		return fmt.Errorf("ws dial: %w", err)
	}

	m.mu.Lock()
	m.conn = conn
	m.connected = true
	m.lastMsgAt = time.Now()
	m.disconnectedAt = time.Time{}
	m.mu.Unlock()

	log.Println("PriceFeedManager: Connected")
	go m.pingLoop()

	m.resubscribeAll()
	return nil
}

// resubscribeAll sends the current full asset list to the WS. No-op if disconnected or empty.
func (m *PriceFeedManager) resubscribeAll() {
	m.mu.RLock()
	conn := m.conn
	connected := m.connected
	ids := make([]string, 0, len(m.subCount))
	for id := range m.subCount {
		ids = append(ids, id)
	}
	m.mu.RUnlock()

	if !connected || conn == nil || len(ids) == 0 {
		return
	}
	payload := map[string]any{
		"type":       "market",
		"assets_ids": ids,
	}
	b, err := json.Marshal(payload)
	if err != nil {
		log.Printf("PriceFeedManager: marshal subscribe: %v", err)
		return
	}
	log.Printf("[WS-DIAG] subscribe send (%d ids): %s", len(ids), string(b))
	if err := conn.WriteMessage(websocket.TextMessage, b); err != nil {
		log.Printf("PriceFeedManager: write subscribe: %v", err)
	}
}

func (m *PriceFeedManager) pingLoop() {
	ticker := time.NewTicker(priceFeedPingInterval)
	defer ticker.Stop()
	for {
		select {
		case <-m.ctx.Done():
			return
		case <-ticker.C:
			m.mu.RLock()
			conn := m.conn
			connected := m.connected
			lastMsg := m.lastMsgAt
			m.mu.RUnlock()
			if !connected || conn == nil {
				return
			}
			if !lastMsg.IsZero() && time.Since(lastMsg) > priceFeedStaleTimeout {
				log.Printf("PriceFeedManager: stale %v, forcing reconnect", time.Since(lastMsg))
				_ = conn.Close()
				return
			}
			if err := conn.WriteMessage(websocket.TextMessage, []byte("PING")); err != nil {
				log.Printf("PriceFeedManager: ping failed: %v", err)
				_ = conn.Close()
				return
			}
		}
	}
}

func (m *PriceFeedManager) readLoop() {
	for {
		m.mu.RLock()
		conn := m.conn
		connected := m.connected
		m.mu.RUnlock()
		if !connected || conn == nil {
			return
		}

		_, data, err := conn.ReadMessage()
		if err != nil {
			log.Printf("PriceFeedManager: read error: %v", err)
			return
		}

		m.mu.Lock()
		m.lastMsgAt = time.Now()
		m.mu.Unlock()

		m.dispatchMessage(data)
	}
}

// wireBookLevel / wirePriceChange match Polymarket's JSON strings-as-numbers format.
type wireBookLevel struct {
	Price string `json:"price"`
	Size  string `json:"size"`
}

type wirePriceChange struct {
	Price string `json:"price"`
	Size  string `json:"size"`
	Side  string `json:"side"`
}

// dispatchMessage parses a raw WS frame. Polymarket sends either a JSON array of events
// or a single event object. Non-JSON frames (PONG) are ignored.
func (m *PriceFeedManager) dispatchMessage(data []byte) {
	if len(data) == 0 {
		return
	}
	switch data[0] {
	case '[':
		var events []json.RawMessage
		if err := json.Unmarshal(data, &events); err != nil {
			log.Printf("PriceFeedManager: parse array: %v", err)
			return
		}
		for _, e := range events {
			m.dispatchEvent(e)
		}
	case '{':
		m.dispatchEvent(data)
	default:
		// PONG or other non-JSON. Log non-PONG frames so we can spot
		// subscription rejection / ack messages from the server.
		s := strings.TrimSpace(string(data))
		if !strings.EqualFold(s, "PONG") {
			if len(s) > 256 {
				s = s[:256] + "...(truncated)"
			}
			log.Printf("[WS-DIAG] non-JSON frame (%d bytes): %q", len(data), s)
		}
	}
}

func (m *PriceFeedManager) dispatchEvent(raw json.RawMessage) {
	var peek struct {
		EventType string `json:"event_type"`
		AssetID   string `json:"asset_id"`
	}
	if err := json.Unmarshal(raw, &peek); err != nil {
		log.Printf("PriceFeedManager: parse event: %v", err)
		return
	}
	if peek.AssetID == "" {
		return
	}
	switch peek.EventType {
	case "book":
		var msg struct {
			Bids []wireBookLevel `json:"bids"`
			Asks []wireBookLevel `json:"asks"`
		}
		if err := json.Unmarshal(raw, &msg); err != nil {
			log.Printf("PriceFeedManager: parse book: %v", err)
			return
		}
		m.applyBookSnapshot(peek.AssetID, parseWireLevels(msg.Bids), parseWireLevels(msg.Asks))
		m.notify(peek.AssetID)
	case "price_change":
		var msg struct {
			Changes []wirePriceChange `json:"changes"`
		}
		if err := json.Unmarshal(raw, &msg); err != nil {
			log.Printf("PriceFeedManager: parse price_change: %v", err)
			return
		}
		m.applyPriceChanges(peek.AssetID, parseWireChanges(msg.Changes))
		m.notify(peek.AssetID)
	case "last_trade_price":
		// A SELL trade hit the bid; the trade price is a fresh observation of
		// the best-bid level. A BUY trade hit the ask and tells us nothing
		// directly about the bid — stamp freshness only.
		var msg struct {
			Price string `json:"price"`
			Side  string `json:"side"`
		}
		if err := json.Unmarshal(raw, &msg); err != nil {
			log.Printf("PriceFeedManager: parse last_trade_price: %v", err)
			return
		}
		m.applyLastTradePrice(peek.AssetID, msg.Price, msg.Side)
		m.notify(peek.AssetID)
	default:
		// tick_size_change, unknown — ignore in v1.
		// Log so we can see what event types the server actually pushes.
		raw := string(raw)
		if len(raw) > 256 {
			raw = raw[:256] + "...(truncated)"
		}
		log.Printf("[WS-DIAG] unhandled event_type=%q asset=%s raw=%s",
			peek.EventType, peek.AssetID, raw)
	}
}

// applyLastTradePrice updates per-token state from a last_trade_price event.
// Both sides stamp lastUpdateAt (heartbeat for the staleness check). Only SELL
// trades update tradeBids — those are direct observations of the best bid.
func (m *PriceFeedManager) applyLastTradePrice(tokenID, priceStr, side string) {
	price, err := strconv.ParseFloat(priceStr, 64)
	if err != nil || price <= 0 {
		// Garbage price — still stamp freshness so the staleness check
		// passes, but don't pollute tradeBids.
		m.stampUpdate(tokenID)
		return
	}
	m.mu.Lock()
	if strings.EqualFold(side, "SELL") {
		m.tradeBids[tokenID] = price
	}
	m.lastUpdateAt[tokenID] = time.Now()
	m.mu.Unlock()
}

func parseWireLevels(levels []wireBookLevel) []BookLevel {
	out := make([]BookLevel, 0, len(levels))
	for _, l := range levels {
		p, _ := strconv.ParseFloat(l.Price, 64)
		s, _ := strconv.ParseFloat(l.Size, 64)
		out = append(out, BookLevel{Price: p, Size: s})
	}
	return out
}

func parseWireChanges(changes []wirePriceChange) []PriceChange {
	out := make([]PriceChange, 0, len(changes))
	for _, c := range changes {
		p, _ := strconv.ParseFloat(c.Price, 64)
		s, _ := strconv.ParseFloat(c.Size, 64)
		out = append(out, PriceChange{Price: p, Size: s, Side: strings.ToUpper(c.Side)})
	}
	return out
}

func (m *PriceFeedManager) applyBookSnapshot(tokenID string, bids, asks []BookLevel) {
	b := m.ensureBook(tokenID)
	b.ApplyBook(bids, asks)
	m.stampUpdate(tokenID)
}

func (m *PriceFeedManager) applyPriceChanges(tokenID string, changes []PriceChange) {
	b := m.ensureBook(tokenID)
	b.ApplyPriceChange(changes)
	m.stampUpdate(tokenID)
}

func (m *PriceFeedManager) stampUpdate(tokenID string) {
	m.mu.Lock()
	m.lastUpdateAt[tokenID] = time.Now()
	m.mu.Unlock()
}

func (m *PriceFeedManager) ensureBook(tokenID string) *bookState {
	m.mu.Lock()
	defer m.mu.Unlock()
	b, ok := m.books[tokenID]
	if !ok {
		b = newBookState()
		m.books[tokenID] = b
	}
	return b
}

func (m *PriceFeedManager) notify(tokenID string) {
	m.mu.RLock()
	listeners := make([]PriceUpdateListener, len(m.listeners))
	copy(listeners, m.listeners)
	m.mu.RUnlock()
	for _, l := range listeners {
		l(tokenID)
	}
}
