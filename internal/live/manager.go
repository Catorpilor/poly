package live

import (
	"context"
	"encoding/json"
	"log"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	polymarketrealtime "github.com/ivanzzeth/polymarket-go-real-time-data-client"
)

// TelegramSender interface for sending messages to Telegram
type TelegramSender interface {
	SendMessage(chatID int64, text string)
}

// SubscriptionRegistry tracks all active subscriptions
type SubscriptionRegistry struct {
	mu sync.RWMutex
	// eventSlug -> set of chatIDs subscribed
	telegramSubs map[string]map[int64]bool
	// chatID -> set of eventSlugs (for multi-event support)
	userEvents map[int64]map[string]bool
	// eventSlug -> set of WebSocket connections
	webSubs map[string]map[*websocket.Conn]bool
}

// NewSubscriptionRegistry creates a new subscription registry
func NewSubscriptionRegistry() *SubscriptionRegistry {
	return &SubscriptionRegistry{
		telegramSubs: make(map[string]map[int64]bool),
		userEvents:   make(map[int64]map[string]bool),
		webSubs:      make(map[string]map[*websocket.Conn]bool),
	}
}

// SubscribeTelegram adds a telegram user to an event subscription
func (r *SubscriptionRegistry) SubscribeTelegram(chatID int64, eventSlug string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()

	// Check if already subscribed
	if events, ok := r.userEvents[chatID]; ok {
		if events[eventSlug] {
			return false // Already subscribed
		}
	}

	// Add to event -> users mapping
	if r.telegramSubs[eventSlug] == nil {
		r.telegramSubs[eventSlug] = make(map[int64]bool)
	}
	r.telegramSubs[eventSlug][chatID] = true

	// Add to user -> events mapping
	if r.userEvents[chatID] == nil {
		r.userEvents[chatID] = make(map[string]bool)
	}
	r.userEvents[chatID][eventSlug] = true

	return true
}

// UnsubscribeTelegram removes a telegram user from an event subscription
func (r *SubscriptionRegistry) UnsubscribeTelegram(chatID int64, eventSlug string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()

	// Remove from event -> users mapping
	if users, ok := r.telegramSubs[eventSlug]; ok {
		delete(users, chatID)
		if len(users) == 0 {
			delete(r.telegramSubs, eventSlug)
		}
	}

	// Remove from user -> events mapping
	if events, ok := r.userEvents[chatID]; ok {
		if !events[eventSlug] {
			return false // Wasn't subscribed
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

// UnsubscribeAllTelegram removes all subscriptions for a user
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
		// Remove from event -> users mapping
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

// GetUserEvents returns all events a user is subscribed to
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

// GetTelegramSubscribers returns all chatIDs subscribed to an event
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

// HasTelegramSubscribers checks if an event has any telegram subscribers
func (r *SubscriptionRegistry) HasTelegramSubscribers(eventSlug string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.telegramSubs[eventSlug]) > 0
}

// SubscribeWeb adds a web client to an event subscription
func (r *SubscriptionRegistry) SubscribeWeb(conn *websocket.Conn, eventSlug string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.webSubs[eventSlug] == nil {
		r.webSubs[eventSlug] = make(map[*websocket.Conn]bool)
	}
	r.webSubs[eventSlug][conn] = true
}

// UnsubscribeWeb removes a web client from all subscriptions
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

// UnsubscribeWebFromEvent removes a web client from a specific event subscription
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

// GetWebConnectionEvents returns all events a web connection is subscribed to
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

// IsWebSubscribed checks if a web connection is subscribed to an event
func (r *SubscriptionRegistry) IsWebSubscribed(conn *websocket.Conn, eventSlug string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()

	conns, ok := r.webSubs[eventSlug]
	if !ok {
		return false
	}
	return conns[conn]
}

// GetWebSubscribers returns all web connections subscribed to an event
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

// GetAllSubscribedEvents returns all events with at least one subscriber
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
	client        *polymarketrealtime.Client
	subscriptions *SubscriptionRegistry
	resolver      *EventSlugResolver
	formatter     *TradeFormatter
	telegramBot   TelegramSender

	mu        sync.RWMutex
	connected bool
	ctx       context.Context
	cancel    context.CancelFunc

	// Track which events we're subscribed to at the RTDS level
	rtdsSubscriptions map[string]bool
	rtdsMu            sync.Mutex
}

// NewLiveTradeManager creates a new live trade manager
func NewLiveTradeManager() *LiveTradeManager {
	ctx, cancel := context.WithCancel(context.Background())

	return &LiveTradeManager{
		subscriptions:     NewSubscriptionRegistry(),
		resolver:          NewEventSlugResolver(),
		formatter:         NewTradeFormatter(),
		ctx:               ctx,
		cancel:            cancel,
		rtdsSubscriptions: make(map[string]bool),
	}
}

// SetTelegramBot sets the telegram sender for broadcasting messages
func (m *LiveTradeManager) SetTelegramBot(bot TelegramSender) {
	m.telegramBot = bot
}

// Start establishes connection to the RTDS WebSocket
func (m *LiveTradeManager) Start() error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.connected {
		return nil
	}

	// Create client with options
	m.client = polymarketrealtime.New(
		polymarketrealtime.WithLogger(polymarketrealtime.NewSilentLogger()),
		polymarketrealtime.WithAutoReconnect(true),
		polymarketrealtime.WithMaxReconnectAttempts(10),
		polymarketrealtime.WithPingInterval(5*time.Second),
		polymarketrealtime.WithOnConnect(func() {
			log.Println("LiveTradeManager: Connected to RTDS WebSocket")
			m.mu.Lock()
			m.connected = true
			m.mu.Unlock()
		}),
		polymarketrealtime.WithOnDisconnect(func(err error) {
			log.Printf("LiveTradeManager: Disconnected from RTDS WebSocket: %v", err)
			m.mu.Lock()
			m.connected = false
			m.mu.Unlock()
		}),
	)

	if err := m.client.Connect(); err != nil {
		return err
	}

	// Subscribe to all activity trades
	// The Client embeds RealtimeTypedSubscriptionHandler, so we can call Subscribe methods directly
	if err := m.client.SubscribeToActivityTrades(m.handleTrade, nil); err != nil {
		log.Printf("LiveTradeManager: Failed to subscribe to activity trades: %v", err)
		return err
	}

	log.Println("LiveTradeManager: Started and subscribed to activity trades")
	return nil
}

// Stop closes the WebSocket connection
func (m *LiveTradeManager) Stop() error {
	m.cancel()

	m.mu.Lock()
	defer m.mu.Unlock()

	if m.client != nil {
		if err := m.client.Disconnect(); err != nil {
			return err
		}
	}

	m.connected = false
	return nil
}

// IsConnected returns whether the manager is connected to RTDS
func (m *LiveTradeManager) IsConnected() bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.connected
}

// SubscribeTelegram adds a telegram user to monitor an event
func (m *LiveTradeManager) SubscribeTelegram(ctx context.Context, chatID int64, eventSlug string) (*EventInfo, error) {
	// Validate event exists
	eventInfo, err := m.resolver.GetEventInfo(ctx, eventSlug)
	if err != nil {
		return nil, err
	}

	// Add to registry
	if !m.subscriptions.SubscribeTelegram(chatID, eventSlug) {
		// Already subscribed, but return event info anyway
		return eventInfo, nil
	}

	log.Printf("LiveTradeManager: User %d subscribed to event %s", chatID, eventSlug)
	return eventInfo, nil
}

// UnsubscribeTelegram removes a telegram user from monitoring an event
func (m *LiveTradeManager) UnsubscribeTelegram(chatID int64, eventSlug string) bool {
	result := m.subscriptions.UnsubscribeTelegram(chatID, eventSlug)
	if result {
		log.Printf("LiveTradeManager: User %d unsubscribed from event %s", chatID, eventSlug)
	}
	return result
}

// UnsubscribeAllTelegram removes all subscriptions for a user
func (m *LiveTradeManager) UnsubscribeAllTelegram(chatID int64) []string {
	result := m.subscriptions.UnsubscribeAllTelegram(chatID)
	if len(result) > 0 {
		log.Printf("LiveTradeManager: User %d unsubscribed from all events: %v", chatID, result)
	}
	return result
}

// GetUserSubscriptions returns all events a user is subscribed to
func (m *LiveTradeManager) GetUserSubscriptions(chatID int64) []string {
	return m.subscriptions.GetUserEvents(chatID)
}

// SubscribeWeb adds a web client to monitor an event
func (m *LiveTradeManager) SubscribeWeb(conn *websocket.Conn, eventSlug string) error {
	// Validate event exists
	_, err := m.resolver.GetEventInfo(context.Background(), eventSlug)
	if err != nil {
		return err
	}

	m.subscriptions.SubscribeWeb(conn, eventSlug)
	log.Printf("LiveTradeManager: Web client subscribed to event %s", eventSlug)
	return nil
}

// UnsubscribeWeb removes a web client from all subscriptions
func (m *LiveTradeManager) UnsubscribeWeb(conn *websocket.Conn) {
	m.subscriptions.UnsubscribeWeb(conn)
}

// UnsubscribeWebFromEvent removes a web client from a specific event
func (m *LiveTradeManager) UnsubscribeWebFromEvent(conn *websocket.Conn, eventSlug string) bool {
	result := m.subscriptions.UnsubscribeWebFromEvent(conn, eventSlug)
	if result {
		log.Printf("LiveTradeManager: Web client unsubscribed from event %s", eventSlug)
	}
	return result
}

// GetWebConnectionEvents returns all events a web connection is subscribed to
func (m *LiveTradeManager) GetWebConnectionEvents(conn *websocket.Conn) []string {
	return m.subscriptions.GetWebConnectionEvents(conn)
}

// IsWebSubscribed checks if a web connection is subscribed to an event
func (m *LiveTradeManager) IsWebSubscribed(conn *websocket.Conn, eventSlug string) bool {
	return m.subscriptions.IsWebSubscribed(conn, eventSlug)
}

// handleTrade processes incoming trades from RTDS
func (m *LiveTradeManager) handleTrade(trade polymarketrealtime.Trade) error {
	// Get event slug from the trade
	// The trade contains market/asset info we can use to match against subscriptions
	eventSlug := m.matchTradeToEvent(trade)
	if eventSlug == "" {
		return nil // No subscribers for this trade
	}

	// Convert to our TradeInfo format
	tradeInfo := &TradeInfo{
		EventSlug:   eventSlug,
		ProxyWallet: trade.ProxyWallet,
		Pseudonym:   trade.Pseudonym,
		Side:        string(trade.Side),
		Outcome:     trade.Outcome,
		Size:        trade.Size,
		Price:       trade.Price,
		Timestamp:   trade.Timestamp,
	}

	// Broadcast to telegram subscribers
	m.broadcastToTelegram(eventSlug, tradeInfo)

	// Broadcast to web subscribers
	m.broadcastToWeb(eventSlug, tradeInfo)

	return nil
}

// matchTradeToEvent finds which subscribed event this trade belongs to
func (m *LiveTradeManager) matchTradeToEvent(trade polymarketrealtime.Trade) string {
	// Get all subscribed events
	subscribedEvents := m.subscriptions.GetAllSubscribedEvents()
	if len(subscribedEvents) == 0 {
		return ""
	}

	// Check if trade's event slug matches any subscription
	// The trade may have EventSlug or we need to look it up
	tradeEventSlug := trade.EventSlug
	if tradeEventSlug != "" {
		for _, eventSlug := range subscribedEvents {
			if eventSlug == tradeEventSlug {
				return eventSlug
			}
		}
	}

	// Try matching by market/condition ID
	tradeConditionID := trade.ConditionID
	if tradeConditionID != "" {
		for _, eventSlug := range subscribedEvents {
			eventInfo, err := m.resolver.GetEventInfo(context.Background(), eventSlug)
			if err != nil {
				continue
			}
			for _, market := range eventInfo.Markets {
				if market.ConditionID == tradeConditionID || market.ID == tradeConditionID {
					return eventSlug
				}
			}
		}
	}

	return ""
}

// broadcastToTelegram sends trade to all telegram subscribers
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

// broadcastToWeb sends trade to all web subscribers
func (m *LiveTradeManager) broadcastToWeb(eventSlug string, trade *TradeInfo) {
	subscribers := m.subscriptions.GetWebSubscribers(eventSlug)
	if len(subscribers) == 0 {
		return
	}

	webFormat := m.formatter.FormatForWeb(trade)
	data, err := json.Marshal(webFormat)
	if err != nil {
		log.Printf("LiveTradeManager: Failed to marshal trade for web: %v", err)
		return
	}

	for _, conn := range subscribers {
		if err := conn.WriteMessage(websocket.TextMessage, data); err != nil {
			log.Printf("LiveTradeManager: Failed to send to web client: %v", err)
			// Connection will be cleaned up by the web server
		}
	}
}

// GetResolver returns the event slug resolver
func (m *LiveTradeManager) GetResolver() *EventSlugResolver {
	return m.resolver
}
