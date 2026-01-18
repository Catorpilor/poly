package live

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
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
	mu           sync.RWMutex
	telegramSubs map[string]map[int64]bool
	userEvents   map[int64]map[string]bool
	webSubs      map[string]map[*websocket.Conn]bool
}

func NewSubscriptionRegistry() *SubscriptionRegistry {
	return &SubscriptionRegistry{
		telegramSubs: make(map[string]map[int64]bool),
		userEvents:   make(map[int64]map[string]bool),
		webSubs:      make(map[string]map[*websocket.Conn]bool),
	}
}

func (r *SubscriptionRegistry) SubscribeTelegram(chatID int64, eventSlug string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()

	if events, ok := r.userEvents[chatID]; ok {
		if events[eventSlug] {
			return false
		}
	}

	if r.telegramSubs[eventSlug] == nil {
		r.telegramSubs[eventSlug] = make(map[int64]bool)
	}
	r.telegramSubs[eventSlug][chatID] = true

	if r.userEvents[chatID] == nil {
		r.userEvents[chatID] = make(map[string]bool)
	}
	r.userEvents[chatID][eventSlug] = true

	return true
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
		if !events[eventSlug] {
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

func (r *SubscriptionRegistry) HasTelegramSubscribers(eventSlug string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.telegramSubs[eventSlug]) > 0
}

func (r *SubscriptionRegistry) SubscribeWeb(conn *websocket.Conn, eventSlug string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.webSubs[eventSlug] == nil {
		r.webSubs[eventSlug] = make(map[*websocket.Conn]bool)
	}
	r.webSubs[eventSlug][conn] = true
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
	telegramBot   TelegramSender

	mu         sync.RWMutex
	conn       *websocket.Conn
	connected  bool
	subscribed bool // Whether we've sent the subscription message
	ctx        context.Context
	cancel     context.CancelFunc

	// Map asset ID to event slug for trade matching
	assetToEvent map[string]string
	// Map asset ID to market short name (e.g., "WOL", "DRAW", "NEW" for 3-way)
	assetToMarketName map[string]string
	assetMu           sync.RWMutex
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
	m.telegramBot = bot
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

	conn, _, err := websocket.DefaultDialer.Dial(rtdsURL, nil)
	if err != nil {
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
			m.mu.RUnlock()

			if !connected || conn == nil {
				return
			}

			if err := conn.WriteMessage(websocket.TextMessage, []byte("PING")); err != nil {
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
	lastLogTime := time.Now()

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
			m.handleTrade(&event.Payload)
		}

		// Log stats every 60 seconds
		if time.Since(lastLogTime) > 60*time.Second {
			log.Printf("LiveTradeManager: Stats - messages=%d, trades=%d, subscribed_events=%v",
				messageCount, tradeCount, m.subscriptions.GetAllSubscribedEvents())
			lastLogTime = time.Now()
		}
	}
}

func (m *LiveTradeManager) handleDisconnect() {
	m.mu.Lock()
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

	if !connected || conn == nil {
		return fmt.Errorf("not connected")
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

	if err := conn.WriteMessage(websocket.TextMessage, data); err != nil {
		return err
	}
	log.Println("LiveTradeManager: Subscribed to activity trades")

	m.mu.Lock()
	m.subscribed = true
	m.mu.Unlock()

	return nil
}

func (m *LiveTradeManager) handleTrade(payload *rtdsTradePayload) {
	// Match by event slug from payload (primary method)
	subscribedEvents := m.subscriptions.GetAllSubscribedEvents()

	var matchedSlug string
	for _, slug := range subscribedEvents {
		if slug == payload.EventSlug {
			matchedSlug = slug
			break
		}
	}

	// Fallback: match by asset ID
	if matchedSlug == "" && payload.Asset != "" {
		m.assetMu.RLock()
		if slug, found := m.assetToEvent[payload.Asset]; found {
			matchedSlug = slug
		}
		m.assetMu.RUnlock()
	}

	if matchedSlug == "" {
		// Log first few unmatched trades for debugging
		if len(subscribedEvents) > 0 {
			log.Printf("LiveTradeManager: Unmatched trade - event_slug=%s, asset=%s, subscribed=%v",
				payload.EventSlug, payload.Asset[:min(16, len(payload.Asset))], subscribedEvents)
		}
		return
	}

	// Look up market name for 3-way markets (e.g., "WOL", "DRAW", "NEW")
	var marketName string
	if payload.Asset != "" {
		m.assetMu.RLock()
		marketName = m.assetToMarketName[payload.Asset]
		m.assetMu.RUnlock()
	}

	tradeInfo := &TradeInfo{
		EventSlug:   matchedSlug,
		ProxyWallet: payload.ProxyWallet,
		Pseudonym:   payload.Name,
		Side:        payload.Side,
		Outcome:     payload.Outcome,
		MarketName:  marketName,
		Size:        payload.Size,
		Price:       payload.Price,
		Timestamp:   payload.Timestamp,
	}

	m.broadcastToTelegram(matchedSlug, tradeInfo)
	m.broadcastToWeb(matchedSlug, tradeInfo)
}

func (m *LiveTradeManager) Stop() error {
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

func (m *LiveTradeManager) SubscribeTelegram(ctx context.Context, chatID int64, eventSlug string) (*EventInfo, error) {
	eventInfo, err := m.resolver.GetEventInfo(ctx, eventSlug)
	if err != nil {
		return nil, err
	}

	isNew := m.subscriptions.SubscribeTelegram(chatID, eventSlug)

	if isNew {
		m.trackEventAssets(eventSlug, eventInfo)
	}

	return eventInfo, nil
}

func (m *LiveTradeManager) UnsubscribeTelegram(chatID int64, eventSlug string) bool {
	return m.subscriptions.UnsubscribeTelegram(chatID, eventSlug)
}

func (m *LiveTradeManager) UnsubscribeAllTelegram(chatID int64) []string {
	return m.subscriptions.UnsubscribeAllTelegram(chatID)
}

func (m *LiveTradeManager) GetUserSubscriptions(chatID int64) []string {
	return m.subscriptions.GetUserEvents(chatID)
}

func (m *LiveTradeManager) SubscribeWeb(conn *websocket.Conn, eventSlug string) error {
	eventInfo, err := m.resolver.GetEventInfo(context.Background(), eventSlug)
	if err != nil {
		return err
	}

	isNew := !m.subscriptions.IsWebSubscribed(conn, eventSlug)
	m.subscriptions.SubscribeWeb(conn, eventSlug)

	if isNew {
		m.trackEventAssets(eventSlug, eventInfo)
	}

	return nil
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

func (m *LiveTradeManager) UnsubscribeWeb(conn *websocket.Conn) {
	m.subscriptions.UnsubscribeWeb(conn)
}

func (m *LiveTradeManager) UnsubscribeWebFromEvent(conn *websocket.Conn, eventSlug string) bool {
	return m.subscriptions.UnsubscribeWebFromEvent(conn, eventSlug)
}

func (m *LiveTradeManager) GetWebConnectionEvents(conn *websocket.Conn) []string {
	return m.subscriptions.GetWebConnectionEvents(conn)
}

func (m *LiveTradeManager) IsWebSubscribed(conn *websocket.Conn, eventSlug string) bool {
	return m.subscriptions.IsWebSubscribed(conn, eventSlug)
}

func (m *LiveTradeManager) broadcastToTelegram(eventSlug string, trade *TradeInfo) {
	if m.telegramBot == nil {
		return
	}

	subscribers := m.subscriptions.GetTelegramSubscribers(eventSlug)
	if len(subscribers) == 0 {
		return
	}

	message := m.formatter.FormatForTelegram(trade)
	for _, chatID := range subscribers {
		m.telegramBot.SendMessage(chatID, message)
	}
}

func (m *LiveTradeManager) broadcastToWeb(eventSlug string, trade *TradeInfo) {
	subscribers := m.subscriptions.GetWebSubscribers(eventSlug)
	if len(subscribers) == 0 {
		return
	}

	webFormat := m.formatter.FormatForWeb(trade)
	data, err := json.Marshal(webFormat)
	if err != nil {
		return
	}

	for _, conn := range subscribers {
		conn.WriteMessage(websocket.TextMessage, data)
	}
}

func (m *LiveTradeManager) GetResolver() *EventSlugResolver {
	return m.resolver
}
