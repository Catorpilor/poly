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
}

// NewPriceFeedManager creates a manager using the production CLOB market WS URL.
func NewPriceFeedManager(fetcher orderBookFetcher) *PriceFeedManager {
	return newPriceFeedManagerWithURL(fetcher, clobMarketWSURL)
}

// newPriceFeedManagerWithURL is an internal constructor for tests to inject a mock WS URL.
func newPriceFeedManagerWithURL(fetcher orderBookFetcher, wsURL string) *PriceFeedManager {
	ctx, cancel := context.WithCancel(context.Background())
	return &PriceFeedManager{
		ctx:      ctx,
		cancel:   cancel,
		fetcher:  fetcher,
		wsURL:    wsURL,
		subCount: make(map[string]int),
		books:    make(map[string]*bookState),
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
	}
	m.mu.Unlock()

	if zero {
		m.resubscribeAll()
	}
}

// BestBid returns the current best bid from local state. If the WS has been
// disconnected longer than priceFeedFallbackThreshold, falls back to an HTTP fetch.
func (m *PriceFeedManager) BestBid(tokenID string) (float64, bool) {
	m.mu.RLock()
	book := m.books[tokenID]
	connected := m.connected
	discAt := m.disconnectedAt
	m.mu.RUnlock()

	if !connected && !discAt.IsZero() && time.Since(discAt) > priceFeedFallbackThreshold {
		if price, ok := m.httpBestBid(tokenID); ok {
			return price, true
		}
	}
	if book == nil {
		return 0, false
	}
	return book.BestBid()
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
		// PONG or other non-JSON - ignore
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
	default:
		// tick_size_change, last_trade_price, unknown — ignore in v1
	}
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
}

func (m *PriceFeedManager) applyPriceChanges(tokenID string, changes []PriceChange) {
	b := m.ensureBook(tokenID)
	b.ApplyPriceChange(changes)
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
