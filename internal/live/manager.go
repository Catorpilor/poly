package live

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/shopspring/decimal"
)

const (
	rtdsURL      = "wss://ws-live-data.polymarket.com"
	pingInterval = 5 * time.Second
)

// TelegramSender interface for sending messages to Telegram
type TelegramSender interface {
	SendMessage(chatID int64, text string)
}

// RTDS message types
type rtdsSubscription struct {
	Topic   string `json:"topic"`
	Type    string `json:"type"`
	Filters string `json:"filters,omitempty"`
}

type rtdsMessage struct {
	Action        string             `json:"action,omitempty"`
	Subscriptions []rtdsSubscription `json:"subscriptions,omitempty"`
}

type rtdsTradePayload struct {
	Asset           string          `json:"asset"`
	Side            string          `json:"side"`
	Price           decimal.Decimal `json:"price"`
	Size            decimal.Decimal `json:"size"`
	Outcome         string          `json:"outcome"`
	Slug            string          `json:"slug"`
	ConditionID     string          `json:"conditionId"`
	ProxyWallet     string          `json:"proxyWallet"`
	TransactionHash string          `json:"transactionHash"`
	Timestamp       int64           `json:"timestamp"`
	Name            string          `json:"name"`
	EventSlug       string          `json:"event_slug"`
	EventTitle      string          `json:"event_title"`
}

type rtdsEvent struct {
	Topic     string           `json:"topic"`
	Type      string           `json:"type"`
	Timestamp int64            `json:"timestamp"`
	Payload   rtdsTradePayload `json:"payload"`
}

// SubscriptionRegistry tracks all active subscriptions
type SubscriptionRegistry struct {
	mu sync.RWMutex
	// telegramSubs / userEvents values carry the subscription's tape flag:
	// presence means subscribed (snipe watch + web tape armed), true means
	// the batched Telegram trade feed is delivered too. Membership must be
	// checked with the comma-ok form, never the value.
	telegramSubs map[string]map[int64]bool
	userEvents   map[int64]map[string]bool
	webSubs      map[string]map[*websocket.Conn]bool
	// Track "all markets" flag per subscription (default false = ML only)
	webSubsAllMarkets map[string]map[*websocket.Conn]bool
	// Mutex per connection to prevent concurrent writes
	connWriteMu map[*websocket.Conn]*sync.Mutex
}

func NewSubscriptionRegistry() *SubscriptionRegistry {
	return &SubscriptionRegistry{
		telegramSubs:      make(map[string]map[int64]bool),
		userEvents:        make(map[int64]map[string]bool),
		webSubs:           make(map[string]map[*websocket.Conn]bool),
		webSubsAllMarkets: make(map[string]map[*websocket.Conn]bool),
		connWriteMu:       make(map[*websocket.Conn]*sync.Mutex),
	}
}

// SubscribeTelegram records chatID's subscription to eventSlug and reports
// whether it is newly subscribed. The tape flag (deliver the batched Telegram
// trade feed) is applied unconditionally, so re-subscribing switches an
// existing subscription's mode in either direction.
func (r *SubscriptionRegistry) SubscribeTelegram(chatID int64, eventSlug string, tape bool) bool {
	r.mu.Lock()
	defer r.mu.Unlock()

	_, existed := r.userEvents[chatID][eventSlug]

	if r.telegramSubs[eventSlug] == nil {
		r.telegramSubs[eventSlug] = make(map[int64]bool)
	}
	r.telegramSubs[eventSlug][chatID] = tape

	if r.userEvents[chatID] == nil {
		r.userEvents[chatID] = make(map[string]bool)
	}
	r.userEvents[chatID][eventSlug] = tape

	return !existed
}

func (r *SubscriptionRegistry) UnsubscribeTelegram(chatID int64, eventSlug string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()

	if users, ok := r.telegramSubs[eventSlug]; ok {
		delete(users, chatID)
		if len(users) == 0 {
			delete(r.telegramSubs, eventSlug)
		}
	}

	if events, ok := r.userEvents[chatID]; ok {
		if _, subscribed := events[eventSlug]; !subscribed {
			return false
		}
		delete(events, eventSlug)
		if len(events) == 0 {
			delete(r.userEvents, chatID)
		}
	} else {
		return false
	}

	return true
}

func (r *SubscriptionRegistry) UnsubscribeAllTelegram(chatID int64) []string {
	r.mu.Lock()
	defer r.mu.Unlock()

	events, ok := r.userEvents[chatID]
	if !ok {
		return nil
	}

	unsubscribed := make([]string, 0, len(events))
	for eventSlug := range events {
		unsubscribed = append(unsubscribed, eventSlug)
		if users, ok := r.telegramSubs[eventSlug]; ok {
			delete(users, chatID)
			if len(users) == 0 {
				delete(r.telegramSubs, eventSlug)
			}
		}
	}

	delete(r.userEvents, chatID)
	return unsubscribed
}

func (r *SubscriptionRegistry) GetUserEvents(chatID int64) []string {
	r.mu.RLock()
	defer r.mu.RUnlock()

	events, ok := r.userEvents[chatID]
	if !ok {
		return nil
	}

	result := make([]string, 0, len(events))
	for eventSlug := range events {
		result = append(result, eventSlug)
	}
	return result
}

// GetTelegramSubscribers returns ALL telegram subscribers of eventSlug,
// whatever their tape mode — it routes snipe alerts, which every
// subscription receives. The batched trade feed uses TapeSubscribers.
func (r *SubscriptionRegistry) GetTelegramSubscribers(eventSlug string) []int64 {
	r.mu.RLock()
	defer r.mu.RUnlock()

	users, ok := r.telegramSubs[eventSlug]
	if !ok {
		return nil
	}

	result := make([]int64, 0, len(users))
	for chatID := range users {
		result = append(result, chatID)
	}
	return result
}

// TapeSubscribers returns the telegram subscribers of eventSlug that opted
// into the batched trade feed (`/live <slug> tape`).
func (r *SubscriptionRegistry) TapeSubscribers(eventSlug string) []int64 {
	r.mu.RLock()
	defer r.mu.RUnlock()

	users, ok := r.telegramSubs[eventSlug]
	if !ok {
		return nil
	}

	result := make([]int64, 0, len(users))
	for chatID, tape := range users {
		if tape {
			result = append(result, chatID)
		}
	}
	return result
}

// IsTapeSubscribed reports whether chatID's subscription to eventSlug has the
// tape flag. False for unknown subscriptions.
func (r *SubscriptionRegistry) IsTapeSubscribed(chatID int64, eventSlug string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.userEvents[chatID][eventSlug]
}

func (r *SubscriptionRegistry) HasTelegramSubscribers(eventSlug string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.telegramSubs[eventSlug]) > 0
}

// HasAnySubscribers reports whether any telegram chat or web connection is
// still subscribed to eventSlug. The snipe watcher keeps an event's tokens
// watched while this holds and releases them with the last subscriber.
func (r *SubscriptionRegistry) HasAnySubscribers(eventSlug string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.telegramSubs[eventSlug]) > 0 || len(r.webSubs[eventSlug]) > 0
}

func (r *SubscriptionRegistry) SubscribeWeb(conn *websocket.Conn, eventSlug string, allMarkets bool) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.webSubs[eventSlug] == nil {
		r.webSubs[eventSlug] = make(map[*websocket.Conn]bool)
	}
	r.webSubs[eventSlug][conn] = true

	// Track allMarkets preference
	if r.webSubsAllMarkets[eventSlug] == nil {
		r.webSubsAllMarkets[eventSlug] = make(map[*websocket.Conn]bool)
	}
	r.webSubsAllMarkets[eventSlug][conn] = allMarkets

	// Create write mutex for connection if not exists
	if r.connWriteMu[conn] == nil {
		r.connWriteMu[conn] = &sync.Mutex{}
	}
}

func (r *SubscriptionRegistry) UnsubscribeWeb(conn *websocket.Conn) {
	r.mu.Lock()
	defer r.mu.Unlock()

	for eventSlug, conns := range r.webSubs {
		delete(conns, conn)
		if len(conns) == 0 {
			delete(r.webSubs, eventSlug)
		}
	}
	for eventSlug, conns := range r.webSubsAllMarkets {
		delete(conns, conn)
		if len(conns) == 0 {
			delete(r.webSubsAllMarkets, eventSlug)
		}
	}
	// Clean up write mutex
	delete(r.connWriteMu, conn)
}

func (r *SubscriptionRegistry) UnsubscribeWebFromEvent(conn *websocket.Conn, eventSlug string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()

	conns, ok := r.webSubs[eventSlug]
	if !ok {
		return false
	}

	if !conns[conn] {
		return false
	}

	delete(conns, conn)
	if len(conns) == 0 {
		delete(r.webSubs, eventSlug)
	}

	// Also clean up allMarkets map
	if allConns, ok := r.webSubsAllMarkets[eventSlug]; ok {
		delete(allConns, conn)
		if len(allConns) == 0 {
			delete(r.webSubsAllMarkets, eventSlug)
		}
	}
	return true
}

func (r *SubscriptionRegistry) GetWebConnectionEvents(conn *websocket.Conn) []string {
	r.mu.RLock()
	defer r.mu.RUnlock()

	var events []string
	for eventSlug, conns := range r.webSubs {
		if conns[conn] {
			events = append(events, eventSlug)
		}
	}
	return events
}

func (r *SubscriptionRegistry) IsWebSubscribed(conn *websocket.Conn, eventSlug string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()

	conns, ok := r.webSubs[eventSlug]
	if !ok {
		return false
	}
	return conns[conn]
}

func (r *SubscriptionRegistry) GetWebSubscribers(eventSlug string) []*websocket.Conn {
	r.mu.RLock()
	defer r.mu.RUnlock()

	conns, ok := r.webSubs[eventSlug]
	if !ok {
		return nil
	}

	result := make([]*websocket.Conn, 0, len(conns))
	for conn := range conns {
		result = append(result, conn)
	}
	return result
}

func (r *SubscriptionRegistry) WantsAllMarkets(conn *websocket.Conn, eventSlug string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()

	if conns, ok := r.webSubsAllMarkets[eventSlug]; ok {
		return conns[conn]
	}
	return false
}

// RegisterConn creates the connection's write mutex. Called when the
// connection is accepted, before any write can happen; UnsubscribeWeb
// removes it on disconnect.
func (r *SubscriptionRegistry) RegisterConn(conn *websocket.Conn) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.connWriteMu[conn] == nil {
		r.connWriteMu[conn] = &sync.Mutex{}
	}
}

// webWriteTimeout bounds a single write to a web client so a wedged
// connection can only stall its caller briefly before erroring out and
// getting dropped.
const webWriteTimeout = 5 * time.Second

// WriteConn is the single write path to a web connection: it serializes
// writers (gorilla/websocket forbids concurrent writes to one conn — the
// subscribe-ack and broadcast goroutines share each conn) and applies
// webWriteTimeout. Fails if the connection was never registered or is
// already cleaned up.
func (r *SubscriptionRegistry) WriteConn(conn *websocket.Conn, data []byte) error {
	r.mu.RLock()
	mu := r.connWriteMu[conn]
	r.mu.RUnlock()
	if mu == nil {
		return fmt.Errorf("connection not registered")
	}

	mu.Lock()
	defer mu.Unlock()
	conn.SetWriteDeadline(time.Now().Add(webWriteTimeout))
	return conn.WriteMessage(websocket.TextMessage, data)
}

func (r *SubscriptionRegistry) GetAllSubscribedEvents() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()

	events := make(map[string]bool)
	for eventSlug := range r.telegramSubs {
		events[eventSlug] = true
	}
	for eventSlug := range r.webSubs {
		events[eventSlug] = true
	}

	result := make([]string, 0, len(events))
	for eventSlug := range events {
		result = append(result, eventSlug)
	}
	return result
}

// LiveTradeManager manages WebSocket connections and trade broadcasting
type LiveTradeManager struct {
	subscriptions *SubscriptionRegistry
	resolver      *EventSlugResolver
	formatter     *TradeFormatter
	// feedBatcher coalesces the Telegram trade feed (issue #31); nil until
	// SetTelegramBot wires a sender. Nil-safe: every hook checks first.
	feedBatcher *FeedBatcher

	mu              sync.RWMutex
	conn            *websocket.Conn
	connected       bool
	subscribed      bool // Whether we've sent the subscription message
	lastMessageTime time.Time
	ctx             context.Context
	cancel          context.CancelFunc

	// Serializes writes to the upstream RTDS conn: pingLoop and
	// subscribeToAllTrades (reachable from subscribe-handler goroutines)
	// would otherwise write concurrently, which gorilla/websocket forbids.
	rtdsWriteMu sync.Mutex

	// Map asset ID to event slug for trade matching
	assetToEvent map[string]string
	// Map asset ID to market short name (e.g., "WOL", "DRAW", "NEW" for 3-way)
	assetToMarketName map[string]string
	assetMu           sync.RWMutex

	// snipeWatcher, when set, watches subscribed events' tokens for the
	// comeback-snipe pattern. Nil-safe: every hook checks before calling.
	snipeWatcher *SnipeWatcher
}

// SetSnipeWatcher wires the comeback-snipe watcher into the subscription
// lifecycle: an event's tokens are watched while it has any subscriber
// (telegram or web) and released with the last one.
func (m *LiveTradeManager) SetSnipeWatcher(w *SnipeWatcher) {
	m.snipeWatcher = w
}

// snipeMarketsFor flattens resolved markets into per-token snipe metadata.
func snipeMarketsFor(markets []*MarketInfo) []SnipeMarket {
	var out []SnipeMarket
	for _, mkt := range markets {
		outcomes := mkt.GetOutcomes()
		start := mkt.GetGameStartTime()
		for i, tokenID := range mkt.GetClobTokenIds() {
			sm := SnipeMarket{
				TokenID:   tokenID,
				MarketID:  mkt.ID,
				Question:  mkt.Question,
				GameStart: start,
			}
			if i < len(outcomes) {
				sm.Outcome = outcomes[i]
			}
			out = append(out, sm)
		}
	}
	return out
}

// snipeWatchEvent registers the subscription's tokens with the snipe watcher,
// mirroring the trade feed's market resolution: the pinned market for pinned
// subscriptions, the Moneyline markets otherwise.
func (m *LiveTradeManager) snipeWatchEvent(eventSlug string, eventInfo *EventInfo) {
	if m.snipeWatcher == nil {
		return
	}
	var markets []*MarketInfo
	if pinned := pinnedMarket(m.resolver, eventInfo, eventSlug); pinned != nil {
		markets = []*MarketInfo{pinned}
	} else {
		markets = m.resolver.GetAllMLMarkets(eventInfo)
	}
	m.snipeWatcher.WatchEventMarkets(eventSlug, snipeMarketsFor(markets))
}

// snipeUnwatchIfUnsubscribed releases an event's snipe watch when its last
// subscriber (telegram or web) is gone.
func (m *LiveTradeManager) snipeUnwatchIfUnsubscribed(eventSlugs ...string) {
	if m.snipeWatcher == nil {
		return
	}
	for _, slug := range eventSlugs {
		if !m.subscriptions.HasAnySubscribers(slug) {
			m.snipeWatcher.UnwatchEventMarkets(slug)
		}
	}
}

func NewLiveTradeManager() *LiveTradeManager {
	ctx, cancel := context.WithCancel(context.Background())

	return &LiveTradeManager{
		subscriptions:     NewSubscriptionRegistry(),
		resolver:          NewEventSlugResolver(),
		formatter:         NewTradeFormatter(),
		ctx:               ctx,
		cancel:            cancel,
		assetToEvent:      make(map[string]string),
		assetToMarketName: make(map[string]string),
	}
}

func (m *LiveTradeManager) SetTelegramBot(bot TelegramSender) {
	m.feedBatcher = NewFeedBatcher(bot)
}

func (m *LiveTradeManager) Start() error {
	return m.connect()
}

func (m *LiveTradeManager) connect() error {
	m.mu.Lock()
	if m.connected {
		m.mu.Unlock()
		return nil
	}
	m.mu.Unlock()

	// Use custom dialer with larger buffer for big messages
	dialer := websocket.Dialer{
		ReadBufferSize:   65536, // 64KB
		WriteBufferSize:  4096,
		HandshakeTimeout: 30 * time.Second,
	}

	// Check for proxy from environment
	if proxyURL := os.Getenv("HTTPS_PROXY"); proxyURL != "" {
		if parsed, err := url.Parse(proxyURL); err == nil {
			dialer.Proxy = http.ProxyURL(parsed)
			log.Printf("LiveTradeManager: Using proxy %s", proxyURL)
		}
	} else if proxyURL := os.Getenv("HTTP_PROXY"); proxyURL != "" {
		if parsed, err := url.Parse(proxyURL); err == nil {
			dialer.Proxy = http.ProxyURL(parsed)
			log.Printf("LiveTradeManager: Using proxy %s", proxyURL)
		}
	}

	log.Printf("LiveTradeManager: Connecting to %s...", rtdsURL)
	conn, resp, err := dialer.Dial(rtdsURL, nil)
	if err != nil {
		if resp != nil {
			log.Printf("LiveTradeManager: Connection failed with status %d", resp.StatusCode)
		}
		log.Printf("LiveTradeManager: Connection error: %v", err)
		return fmt.Errorf("failed to connect to RTDS: %w", err)
	}

	m.mu.Lock()
	m.conn = conn
	m.connected = true
	m.mu.Unlock()

	log.Println("LiveTradeManager: Connected to RTDS")

	// Start ping routine
	go m.pingLoop()

	// Start read loop
	go m.readLoop()

	// Resubscribe to all tracked assets
	m.resubscribeAll()

	return nil
}

func (m *LiveTradeManager) pingLoop() {
	ticker := time.NewTicker(pingInterval)
	defer ticker.Stop()

	for {
		select {
		case <-m.ctx.Done():
			return
		case <-ticker.C:
			m.mu.RLock()
			conn := m.conn
			connected := m.connected
			lastMsg := m.lastMessageTime
			m.mu.RUnlock()

			if !connected || conn == nil {
				return
			}

			// Check for stale connection (no messages for 60 seconds)
			if !lastMsg.IsZero() && time.Since(lastMsg) > 60*time.Second {
				log.Printf("LiveTradeManager: No messages for 60s, forcing reconnect...")
				m.handleDisconnect()
				return
			}

			m.rtdsWriteMu.Lock()
			err := conn.WriteMessage(websocket.TextMessage, []byte("PING"))
			m.rtdsWriteMu.Unlock()
			if err != nil {
				log.Printf("LiveTradeManager: Ping failed: %v", err)
				m.handleDisconnect()
				return
			}
		}
	}
}

func (m *LiveTradeManager) readLoop() {
	messageCount := 0
	tradeCount := 0
	staleCount := 0
	matchedCount := 0
	lastLogTime := time.Now()
	sampleSlugs := make(map[string]int)         // Sample of incoming event slugs
	unmatchedSamples := make(map[string]string) // Sample of unmatched slugs with their full info

	for {
		m.mu.RLock()
		conn := m.conn
		connected := m.connected
		m.mu.RUnlock()

		if !connected || conn == nil {
			return
		}

		_, message, err := conn.ReadMessage()
		if err != nil {
			log.Printf("LiveTradeManager: Read error: %v", err)
			m.handleDisconnect()
			return
		}

		// Update last message time for stale connection detection
		m.mu.Lock()
		m.lastMessageTime = time.Now()
		m.mu.Unlock()

		messageCount++

		// Skip PONG messages
		if string(message) == "PONG" {
			continue
		}

		// Parse the event
		var event rtdsEvent
		if err := json.Unmarshal(message, &event); err != nil {
			continue
		}

		// Handle activity trades
		if event.Topic == "activity" && event.Type == "trades" {
			tradeCount++
			// Log first trade to see timestamp age
			if tradeCount == 1 {
				ts := event.Payload.Timestamp
				if ts > 0 {
					if ts < 10000000000 {
						ts *= 1000
					}
					ageMs := time.Now().UnixMilli() - ts
					log.Printf("LiveTradeManager: First trade age=%dms, payload: %s", ageMs, string(message))
				} else {
					log.Printf("LiveTradeManager: First trade payload (raw): %s", string(message))
				}
			}
			// Track sample of incoming event slugs (keep up to 10 unique)
			slug := event.Payload.EventSlug
			if slug == "" {
				slug = event.Payload.Slug // Try alternate field
			}
			if len(sampleSlugs) < 10 && slug != "" {
				sampleSlugs[slug]++
			}
			if m.handleTrade(&event.Payload) {
				matchedCount++
			} else {
				// Check if it was filtered due to staleness
				if event.Payload.Timestamp > 0 {
					ts := event.Payload.Timestamp
					if ts < 10000000000 {
						ts *= 1000
					}
					if time.Now().UnixMilli()-ts > 120_000 {
						staleCount++
					}
				}
				if slug != "" && len(unmatchedSamples) < 20 {
					// Track unmatched trades to help debug
					unmatchedSamples[slug] = fmt.Sprintf("event_slug=%s, slug=%s, asset=%s",
						event.Payload.EventSlug, event.Payload.Slug, event.Payload.Asset)
				}
			}
		}

		// Log stats every 60 seconds
		if time.Since(lastLogTime) > 60*time.Second {
			subscribedEvents := m.subscriptions.GetAllSubscribedEvents()
			log.Printf("LiveTradeManager: Stats - messages=%d, trades=%d, matched=%d, stale_skipped=%d", messageCount, tradeCount, matchedCount, staleCount)
			log.Printf("LiveTradeManager: Subscribed events: %v", subscribedEvents)
			log.Printf("LiveTradeManager: Sample incoming slugs: %v", sampleSlugs)
			if len(unmatchedSamples) > 0 {
				log.Printf("LiveTradeManager: Unmatched samples (first %d):", len(unmatchedSamples))
				for slug, info := range unmatchedSamples {
					log.Printf("  - %s: %s", slug, info)
				}
			}
			sampleSlugs = make(map[string]int)         // Reset
			unmatchedSamples = make(map[string]string) // Reset
			matchedCount = 0
			staleCount = 0
			lastLogTime = time.Now()
		}
	}
}

func (m *LiveTradeManager) handleDisconnect() {
	m.mu.Lock()
	// Guard against double reconnect
	if !m.connected {
		m.mu.Unlock()
		return
	}
	m.connected = false
	m.subscribed = false
	if m.conn != nil {
		m.conn.Close()
		m.conn = nil
	}
	m.mu.Unlock()

	log.Println("LiveTradeManager: Disconnected, reconnecting...")

	// Reconnect after a delay
	go func() {
		time.Sleep(2 * time.Second)
		if err := m.connect(); err != nil {
			log.Printf("LiveTradeManager: Reconnect failed: %v", err)
		}
	}()
}

func (m *LiveTradeManager) resubscribeAll() {
	m.assetMu.RLock()
	hasAssets := len(m.assetToEvent) > 0
	m.assetMu.RUnlock()

	if hasAssets {
		m.subscribeToAllTrades()
	}
}

func (m *LiveTradeManager) subscribeToAllTrades() error {
	m.mu.Lock()
	if m.subscribed {
		m.mu.Unlock()
		return nil
	}
	conn := m.conn
	connected := m.connected
	m.mu.Unlock()

	// If not connected, try to connect first
	if !connected || conn == nil {
		log.Println("LiveTradeManager: Not connected, attempting to connect...")
		if err := m.connect(); err != nil {
			return fmt.Errorf("not connected: %w", err)
		}
		// Re-check after connect
		m.mu.Lock()
		conn = m.conn
		connected = m.connected
		m.mu.Unlock()
		if !connected || conn == nil {
			return fmt.Errorf("failed to establish connection")
		}
	}

	// Subscribe to all trades, filter client-side by asset ID
	msg := map[string]interface{}{
		"action": "subscribe",
		"subscriptions": []map[string]interface{}{
			{
				"topic": "activity",
				"type":  "trades",
			},
		},
	}

	data, err := json.Marshal(msg)
	if err != nil {
		return err
	}

	m.rtdsWriteMu.Lock()
	err = conn.WriteMessage(websocket.TextMessage, data)
	m.rtdsWriteMu.Unlock()
	if err != nil {
		return err
	}
	log.Println("LiveTradeManager: Subscribed to activity trades")

	m.mu.Lock()
	m.subscribed = true
	m.mu.Unlock()

	return nil
}

func (m *LiveTradeManager) handleTrade(payload *rtdsTradePayload) bool {
	// Skip stale trades (RTDS replays backlog on subscription)
	if payload.Timestamp > 0 {
		tradeTime := payload.Timestamp
		if tradeTime < 10000000000 { // seconds → milliseconds
			tradeTime *= 1000
		}
		ageMs := time.Now().UnixMilli() - tradeTime
		if ageMs > 120_000 { // older than 2 minutes
			return false
		}
	}

	subscribedEvents := m.subscriptions.GetAllSubscribedEvents()

	var matchedSlug string
	var matchedByAsset bool

	// Primary: match by asset ID (most accurate — tracked assets are the
	// Moneyline's, or the pinned market's for pinned web subscriptions)
	if payload.Asset != "" {
		m.assetMu.RLock()
		if slug, found := m.assetToEvent[payload.Asset]; found {
			matchedSlug = slug
			matchedByAsset = true
		}
		m.assetMu.RUnlock()
	}

	// Fallback: match by event slug prefix
	// RTDS sends slugs like "epl-ast-eve-2026-01-18-eve" but we subscribe to "epl-ast-eve-2026-01-18"
	if matchedSlug == "" {
		eventSlug := payload.EventSlug
		if eventSlug == "" {
			eventSlug = payload.Slug
		}
		for _, slug := range subscribedEvents {
			if strings.HasPrefix(eventSlug, slug) {
				matchedSlug = slug
				break
			}
		}
	}

	if matchedSlug == "" {
		return false
	}

	// Use payload.Slug (market-specific slug) for sub-market detection
	// payload.EventSlug is the event-level slug (same for all markets in an event)
	// payload.Slug contains market indicators like "-first-set-winner-", "-over-2-5-", etc.
	marketSlug := payload.Slug
	if marketSlug == "" {
		marketSlug = payload.EventSlug
	}

	// Determine if this is a sub-market trade
	// If matched by asset ID, it's a directly tracked market (ML or pinned)
	// If matched by prefix, check the market slug for sub-market indicators
	var marketName string
	var isSubMarket bool
	if matchedByAsset {
		// Look up market name for 3-way markets (empty for 2-way/pinned)
		m.assetMu.RLock()
		marketName = m.assetToMarketName[payload.Asset]
		m.assetMu.RUnlock()
	} else if isSubMarketSlug(marketSlug) {
		// Prefix-matched and slug has sub-market indicators
		marketName = extractSubMarketName(marketSlug, matchedSlug)
		isSubMarket = true
	}

	tradeInfo := &TradeInfo{
		EventSlug:   matchedSlug,
		ProxyWallet: payload.ProxyWallet,
		Pseudonym:   payload.Name,
		Side:        payload.Side,
		Outcome:     payload.Outcome,
		MarketName:  marketName,
		IsSubMarket: isSubMarket,
		Size:        payload.Size,
		Price:       payload.Price,
		Timestamp:   payload.Timestamp,
	}

	m.broadcastToTelegram(matchedSlug, tradeInfo)
	m.broadcastToWeb(matchedSlug, tradeInfo, marketSlug, matchedByAsset)
	return true
}

func (m *LiveTradeManager) Stop() error {
	// Best-effort drain: pending feed batches go out before teardown.
	if m.feedBatcher != nil {
		m.feedBatcher.FlushAll()
	}

	m.cancel()

	m.mu.Lock()
	defer m.mu.Unlock()

	if m.conn != nil {
		m.conn.Close()
		m.conn = nil
	}

	m.connected = false
	return nil
}

func (m *LiveTradeManager) IsConnected() bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.connected
}

func (m *LiveTradeManager) IsSubscribed() bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.subscribed
}

func (m *LiveTradeManager) GetTrackedAssetCount() int {
	m.assetMu.RLock()
	defer m.assetMu.RUnlock()
	return len(m.assetToEvent)
}

// SubscribeTelegram subscribes chatID to eventSlug. tape opts into the
// batched Telegram trade feed; a quiet subscription still arms the snipe
// watch and the web tape. Re-subscribing switches an existing subscription's
// mode without re-tracking assets.
func (m *LiveTradeManager) SubscribeTelegram(ctx context.Context, chatID int64, eventSlug string, tape bool) (*EventInfo, error) {
	eventInfo, err := m.resolver.GetEventInfo(ctx, eventSlug)
	if err != nil {
		return nil, err
	}

	isNew := m.subscriptions.SubscribeTelegram(chatID, eventSlug, tape)

	if isNew {
		m.trackEventAssets(eventSlug, eventInfo)
		m.snipeWatchEvent(eventSlug, eventInfo)
	}

	return eventInfo, nil
}

// IsTapeSubscription reports whether chatID's subscription to eventSlug
// delivers the batched Telegram trade feed.
func (m *LiveTradeManager) IsTapeSubscription(chatID int64, eventSlug string) bool {
	return m.subscriptions.IsTapeSubscribed(chatID, eventSlug)
}

func (m *LiveTradeManager) UnsubscribeTelegram(chatID int64, eventSlug string) bool {
	ok := m.subscriptions.UnsubscribeTelegram(chatID, eventSlug)
	if ok {
		if m.feedBatcher != nil {
			m.feedBatcher.Flush(chatID, eventSlug)
		}
		m.snipeUnwatchIfUnsubscribed(eventSlug)
	}
	return ok
}

func (m *LiveTradeManager) UnsubscribeAllTelegram(chatID int64) []string {
	unsubscribed := m.subscriptions.UnsubscribeAllTelegram(chatID)
	if m.feedBatcher != nil {
		for _, eventSlug := range unsubscribed {
			m.feedBatcher.Flush(chatID, eventSlug)
		}
	}
	m.snipeUnwatchIfUnsubscribed(unsubscribed...)
	return unsubscribed
}

func (m *LiveTradeManager) GetUserSubscriptions(chatID int64) []string {
	return m.subscriptions.GetUserEvents(chatID)
}

func (m *LiveTradeManager) SubscribeWeb(conn *websocket.Conn, eventSlug string, allMarkets bool) error {
	eventInfo, err := m.resolver.GetEventInfo(context.Background(), eventSlug)
	if err != nil {
		return err
	}

	isNew := !m.subscriptions.IsWebSubscribed(conn, eventSlug)
	m.subscriptions.SubscribeWeb(conn, eventSlug, allMarkets)

	if isNew {
		if pinned := pinnedMarket(m.resolver, eventInfo, eventSlug); pinned != nil {
			// The subscriber addressed a specific sub-market: feed the
			// panel that market's trades, not the event Moneyline's.
			m.trackMarketAssets(eventSlug, pinned)
		} else {
			m.trackEventAssets(eventSlug, eventInfo)
		}
		m.snipeWatchEvent(eventSlug, eventInfo)
	}

	return nil
}

// isSubMarketSlug checks if a slug indicates a sub-market (over/under, btts, handicap, etc.)
func isSubMarketSlug(slug string) bool {
	subMarketIndicators := []string{
		// General
		"-over-", "-under-", "-btts", "-handicap", "-spread",
		"-total-", "-first-", "-score-", "-goals-",
		// Sports player props
		"-points-", "-rebounds-", "-assists-",
		// Half/quarter markets
		"-1h-", "-1q-", "-2h-", "-2q-", "-3q-", "-4q-",
		"-moneyline", // catches "1h-moneyline"
		// Esports
		"-kills-", "-map-", "-maps-", "-dragon-", "-baron-",
		"-blood-", "-tower-", "-inhibitor-", "-series-",
		// Tennis
		"-1st-", "-2nd-", "-3rd-", "-set-",
		// BO series individual games
		"-game-",
	}
	slugLower := strings.ToLower(slug)
	for _, indicator := range subMarketIndicators {
		if strings.Contains(slugLower, indicator) {
			return true
		}
	}
	return false
}

// extractSubMarketName extracts a human-readable sub-market name from the RTDS slug
// e.g., "epl-ast-eve-2026-01-18-over-2-5" with base "epl-ast-eve-2026-01-18" → "Over 2.5"
func extractSubMarketName(rtdsSlug, baseSlug string) string {
	if !strings.HasPrefix(rtdsSlug, baseSlug) {
		return ""
	}

	// Get the suffix after the base slug
	suffix := strings.TrimPrefix(rtdsSlug, baseSlug)
	suffix = strings.TrimPrefix(suffix, "-")

	if suffix == "" {
		return ""
	}

	// Format common patterns
	suffix = strings.ToLower(suffix)

	// Replace dashes with spaces and handle decimal numbers
	// e.g., "over-2-5" → "Over 2.5"
	parts := strings.Split(suffix, "-")
	var result []string
	for i := 0; i < len(parts); i++ {
		part := parts[i]
		// Check if this and next part form a decimal number (e.g., "2" "5" → "2.5")
		if i+1 < len(parts) && isNumeric(part) && isNumeric(parts[i+1]) {
			result = append(result, part+"."+parts[i+1])
			i++ // Skip next part
		} else {
			result = append(result, part)
		}
	}

	// Capitalize first letter of each word
	for i, word := range result {
		if len(word) > 0 {
			result[i] = strings.ToUpper(word[:1]) + word[1:]
		}
	}

	return strings.Join(result, " ")
}

func isNumeric(s string) bool {
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return len(s) > 0
}

func (m *LiveTradeManager) trackEventAssets(eventSlug string, eventInfo *EventInfo) {
	// Use GetAllMLMarketsAssetIDs to support both 2-way (NBA) and 3-way (Football) moneylines
	assetIDs := m.resolver.GetAllMLMarketsAssetIDs(eventInfo)
	if len(assetIDs) == 0 {
		return
	}

	// Get asset to market name mapping (for 3-way markets)
	marketNameMap := m.resolver.GetAssetToMarketNameMap(eventInfo)

	m.assetMu.Lock()
	for _, assetID := range assetIDs {
		m.assetToEvent[assetID] = eventSlug
		if marketName, ok := marketNameMap[assetID]; ok {
			m.assetToMarketName[assetID] = marketName
		}
	}
	m.assetMu.Unlock()

	// Subscribe to all trades (only once), filter client-side
	if err := m.subscribeToAllTrades(); err != nil {
		log.Printf("LiveTradeManager: Failed to subscribe to trades: %v", err)
	}
}

// trackMarketAssets maps a single (pinned) market's assets to the
// subscription slug, so only that market's trades match the subscription.
func (m *LiveTradeManager) trackMarketAssets(eventSlug string, market *MarketInfo) {
	assetIDs := market.GetClobTokenIds()
	if len(assetIDs) == 0 {
		return
	}

	m.assetMu.Lock()
	for _, assetID := range assetIDs {
		m.assetToEvent[assetID] = eventSlug
	}
	m.assetMu.Unlock()

	if err := m.subscribeToAllTrades(); err != nil {
		log.Printf("LiveTradeManager: Failed to subscribe to trades: %v", err)
	}
}

// RegisterWebConn must be called when a web connection is accepted, before
// anything writes to it — WriteWeb refuses unregistered connections.
func (m *LiveTradeManager) RegisterWebConn(conn *websocket.Conn) {
	m.subscriptions.RegisterConn(conn)
}

// WriteWeb sends one frame to a web client through the registry's single
// serialized write path.
func (m *LiveTradeManager) WriteWeb(conn *websocket.Conn, data []byte) error {
	return m.subscriptions.WriteConn(conn, data)
}

func (m *LiveTradeManager) UnsubscribeWeb(conn *websocket.Conn) {
	// Snapshot the connection's events before removal so the snipe watch can
	// be released for any event this disconnect leaves subscriber-less.
	events := m.subscriptions.GetWebConnectionEvents(conn)
	m.subscriptions.UnsubscribeWeb(conn)
	m.snipeUnwatchIfUnsubscribed(events...)
}

func (m *LiveTradeManager) UnsubscribeWebFromEvent(conn *websocket.Conn, eventSlug string) bool {
	ok := m.subscriptions.UnsubscribeWebFromEvent(conn, eventSlug)
	if ok {
		m.snipeUnwatchIfUnsubscribed(eventSlug)
	}
	return ok
}

func (m *LiveTradeManager) GetWebConnectionEvents(conn *websocket.Conn) []string {
	return m.subscriptions.GetWebConnectionEvents(conn)
}

func (m *LiveTradeManager) IsWebSubscribed(conn *websocket.Conn, eventSlug string) bool {
	return m.subscriptions.IsWebSubscribed(conn, eventSlug)
}

// broadcastToTelegram relays one matched trade into the feed batcher, for
// tape subscribers only — quiet subscriptions get snipe alerts, never trade
// prints. Sub-floor trades are dropped and the rest coalesce into one message
// per (chat, subscription) per feedBatchWindow, so the feed can never crowd
// SL/TP fires or snipe alerts (direct sends) out of the per-chat rate
// budget. The web path (broadcastToWeb) stays unfiltered and unbatched.
func (m *LiveTradeManager) broadcastToTelegram(eventSlug string, trade *TradeInfo) {
	if m.feedBatcher == nil {
		return
	}

	subscribers := m.subscriptions.TapeSubscribers(eventSlug)
	if len(subscribers) == 0 {
		return
	}

	// RTDS "size" is the trade's USDC value — both feeds display it as
	// dollars — so it is the figure the relay floor applies to.
	tradeUSD := trade.Size.InexactFloat64()
	line := m.formatter.FormatTelegramLine(trade)
	for _, chatID := range subscribers {
		m.feedBatcher.Add(chatID, eventSlug, tradeUSD, line)
	}
}

func (m *LiveTradeManager) broadcastToWeb(eventSlug string, trade *TradeInfo, rtdsSlug string, matchedByAsset bool) {
	subscribers := m.subscriptions.GetWebSubscribers(eventSlug)
	if len(subscribers) == 0 {
		return
	}

	// Asset-matched trades are on a market the subscription tracks
	// directly (Moneyline or pinned) — the allMarkets gate applies only
	// to prefix-matched spillover from the rest of the event.
	isSubMarket := !matchedByAsset && (trade.IsSubMarket || isSubMarketSlug(rtdsSlug))

	webFormat := m.formatter.FormatForWeb(trade)
	data, err := json.Marshal(webFormat)
	if err != nil {
		return
	}

	for _, conn := range subscribers {
		// Skip sub-market trades unless subscriber wants all markets
		if isSubMarket && !m.subscriptions.WantsAllMarkets(conn, eventSlug) {
			continue
		}
		if err := m.subscriptions.WriteConn(conn, data); err != nil {
			// A dead or wedged client must not stall the feed for the
			// rest. Drop it here; its handler goroutine finishes cleanup
			// when its ReadMessage fails.
			log.Printf("LiveTradeManager: Dropping web client, write failed: %v", err)
			m.subscriptions.UnsubscribeWeb(conn)
			conn.Close()
		}
	}
}

func (m *LiveTradeManager) GetResolver() *EventSlugResolver {
	return m.resolver
}
