package live

import (
	"context"
	"crypto/ecdsa"
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"log"
	"mime"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/Catorpilor/poly/internal/config"
	"github.com/Catorpilor/poly/internal/database"
	"github.com/Catorpilor/poly/internal/database/repositories"
	"github.com/Catorpilor/poly/internal/polymarket"
	"github.com/Catorpilor/poly/internal/wallet"
	"github.com/ethereum/go-ethereum/common"
	"github.com/gorilla/websocket"
)

// WebSocket message types
type wsMessage struct {
	Action     string `json:"action"`     // subscribe, unsubscribe, list
	Event      string `json:"event"`      // event slug
	AllMarkets bool   `json:"allMarkets"` // true to show all markets, false for ML only
}

type wsResponse struct {
	Type     string   `json:"type"`               // subscribed, unsubscribed, subscriptions, error
	Event    string   `json:"event,omitempty"`    // event slug
	Title    string   `json:"title,omitempty"`    // event title (for subscribe response)
	Outcomes []string `json:"outcomes,omitempty"` // outcome names for the main market (for subscribe response)
	Events   []string `json:"events,omitempty"`   // list of subscribed events
	Message  string   `json:"message,omitempty"`  // error message
	// Set when the subscribe slug named a specific sub-market: the panel's
	// primary buy buttons target this market instead of the Moneyline.
	Market         string `json:"market,omitempty"`
	MarketQuestion string `json:"marketQuestion,omitempty"`
}

//go:embed static/*
var staticFiles embed.FS

// liveWatchManager is the subset of *LiveTradeManager the Live Watch web
// endpoints depend on (ADR 0008 phase 3). Extracted as a seam so handler
// tests can inject a fake: SubscribeTelegram otherwise resolves against Gamma
// and tracks RTDS assets. Web-managed watches funnel through these same
// methods as /live, so a web-created watch and a Telegram-created one are one
// durable object (the DB PK dedupes, Event Refresh covers both).
// *LiveTradeManager satisfies this in production.
type liveWatchManager interface {
	SubscribeTelegram(ctx context.Context, chatID int64, eventSlug string, tape bool) (*EventInfo, error)
	UnsubscribeTelegram(chatID int64, eventSlug string) bool
	GetUserSubscriptions(chatID int64) []string
	IsTapeSubscription(chatID int64, eventSlug string) bool
}

// maxLiveWatchesPerUser caps active web-created Live Watches per user (ADR
// 0008 guardrail). The 31st DISTINCT event is refused; a re-subscribe to an
// existing watch (e.g. a tape flip) is never capped. Telegram /live is
// deliberately uncapped.
const maxLiveWatchesPerUser = 30

// webTradeExecutor is the slice of the trade executor the web buy path drives.
// *polymarket.TradeExecutor satisfies it in production; tests inject a fake.
type webTradeExecutor interface {
	Execute(ctx context.Context, privateKey *ecdsa.PrivateKey, proxyAddress common.Address, req *polymarket.TradeRequest) (*polymarket.TradeResult, error)
}

// WebServer serves the live monitoring web interface
type WebServer struct {
	liveManager    *LiveTradeManager
	upgrader       websocket.Upgrader
	httpServer     *http.Server
	port           int
	db             *database.DB
	config         *config.Config
	loginTokenRepo repositories.LoginTokenRepository
	userRepo       repositories.UserRepository
	walletManager  *wallet.Manager
	tradingClient  *polymarket.TradingClient
	// tradeExecutor runs the per-trade ceremony. Held behind an interface so
	// handler tests can inject a fake; in production it is a
	// *polymarket.TradeExecutor.
	tradeExecutor webTradeExecutor
	allowedHost   string // hostname from LIVE_WEB_URL, allowed alongside localhost/IP literals
	// watches is the Live Watch surface of liveManager, held behind an
	// interface so handler tests can inject a fake. In production it is the
	// same *LiveTradeManager as liveManager.
	watches liveWatchManager
}

// NewWebServer creates a new web server for live monitoring
func NewWebServer(
	liveManager *LiveTradeManager,
	port int,
	db *database.DB,
	cfg *config.Config,
	walletManager *wallet.Manager,
	tradingClient *polymarket.TradingClient,
) *WebServer {
	ws := &WebServer{
		liveManager:   liveManager,
		port:          port,
		db:            db,
		config:        cfg,
		walletManager: walletManager,
		tradingClient: tradingClient,
	}
	ws.upgrader = websocket.Upgrader{CheckOrigin: ws.requestAllowed}

	// Hold the Live Watch surface behind the interface seam. Guarded so a
	// typed-nil *LiveTradeManager never becomes a non-nil interface value.
	if liveManager != nil {
		ws.watches = liveManager
	}

	if tradingClient != nil {
		ws.tradeExecutor = polymarket.NewTradeExecutor(tradingClient, polymarket.NewMarketClient())
	}

	if cfg != nil && cfg.App.LiveWebURL != "" {
		if u, err := url.Parse(cfg.App.LiveWebURL); err == nil {
			ws.allowedHost = u.Hostname()
		}
	}

	// Initialize repositories if db is available
	if db != nil {
		ws.loginTokenRepo = repositories.NewLoginTokenRepository(db)
		ws.userRepo = repositories.NewUserRepository(db)
	}

	mux := http.NewServeMux()

	// Serve static files
	staticFS, err := fs.Sub(staticFiles, "static")
	if err != nil {
		log.Printf("WebServer: Failed to setup static files: %v", err)
	} else {
		mux.Handle("/", http.FileServer(http.FS(staticFS)))
	}

	// WebSocket endpoint
	mux.HandleFunc("GET /ws", ws.handleWebSocket)

	// Health check
	mux.HandleFunc("GET /health", ws.handleHealth)

	// Auth endpoints for Telegram login
	mux.HandleFunc("POST /api/auth/init", ws.guardAPI(ws.handleAuthInit))
	mux.HandleFunc("GET /api/auth/status", ws.guardAPI(ws.handleAuthStatus))
	mux.HandleFunc("POST /api/auth/complete", ws.guardAPI(ws.handleAuthComplete))

	// Sub-market listing for the picker
	mux.HandleFunc("GET /api/events/{slug}/markets", ws.guardAPI(ws.handleListEventMarkets))

	// Trade endpoint
	mux.HandleFunc("POST /api/trade", ws.guardAPI(ws.handleTrade))

	// Live Watch management (ADR 0008 phase 3). Session-validated exactly
	// like /api/trade. PUT/DELETE are non-simple methods (always preflighted
	// cross-origin) and, with the Host/Origin guard, need no extra CSRF
	// handling beyond guardAPI. The list endpoint is POST-with-body for
	// consistency with how the page already sends its authenticated session.
	mux.HandleFunc("PUT /api/events/{slug}/subscription", ws.guardAPI(ws.handlePutSubscription))
	mux.HandleFunc("DELETE /api/events/{slug}/subscription", ws.guardAPI(ws.handleDeleteSubscription))
	mux.HandleFunc("POST /api/subscriptions/list", ws.guardAPI(ws.handleListSubscriptions))

	// API namespace fallback. Without this, a wrong-method or unknown
	// /api/ request falls through to the "/" file server and gets an HTML
	// 404; the method-scoped patterns above only produce a 405 when no
	// broader pattern matches.
	mux.HandleFunc("/api/", handleAPIFallback)

	ws.httpServer = &http.Server{
		Addr:    fmt.Sprintf(":%d", port),
		Handler: mux,
		// Don't hold connections open forever (slowloris posture, even on
		// a LAN). Gorilla clears these deadlines when it hijacks the /ws
		// connection, so the long-lived WebSocket is unaffected.
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	return ws
}

// Start starts the web server
func (ws *WebServer) Start() error {
	log.Printf("WebServer: Starting on port %d", ws.port)
	go func() {
		if err := ws.httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Printf("WebServer: Error starting server: %v", err)
		}
	}()
	return nil
}

// Stop stops the web server
func (ws *WebServer) Stop() error {
	return ws.httpServer.Close()
}

// The server is LAN-only and issues no bearer credentials, so the API's only
// protection against a LAN browser being used as a CSRF proxy is rejecting
// requests that couldn't have come from a page this server served:
//   - the Host must be localhost, an IP literal, or the LIVE_WEB_URL host —
//     under DNS rebinding the Host carries the attacker's domain instead;
//   - an Origin header, when present, must match the request Host exactly —
//     cross-site fetches carry the foreign page's origin;
//   - POST bodies must declare Content-Type: application/json — cross-origin
//     fetches then require a CORS preflight, which this server never answers.

// requestAllowed is the shared predicate for /api/ requests and the /ws
// upgrade (websocket.Upgrader.CheckOrigin).
func (ws *WebServer) requestAllowed(r *http.Request) bool {
	if !ws.hostAllowed(r.Host) {
		return false
	}
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}
	u, err := url.Parse(origin)
	if err != nil {
		return false
	}
	return strings.EqualFold(u.Host, r.Host)
}

func (ws *WebServer) hostAllowed(hostport string) bool {
	host := hostport
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		host = h
	}
	if strings.EqualFold(host, "localhost") || net.ParseIP(host) != nil {
		return true
	}
	return ws.allowedHost != "" && strings.EqualFold(host, ws.allowedHost)
}

// apiEndpoints are the registered API paths, used by the fallback to tell
// a wrong method (405) from an unknown path (404).
var apiEndpoints = map[string]bool{
	"/api/auth/init":          true,
	"/api/auth/status":        true,
	"/api/auth/complete":      true,
	"/api/trade":              true,
	"/api/subscriptions/list": true,
}

func handleAPIFallback(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if apiEndpoints[r.URL.Path] {
		w.WriteHeader(http.StatusMethodNotAllowed)
		json.NewEncoder(w).Encode(map[string]string{"error": "Method not allowed"})
		return
	}
	w.WriteHeader(http.StatusNotFound)
	json.NewEncoder(w).Encode(map[string]string{"error": "Not found"})
}

// guardAPI wraps an /api/ handler with the LAN-only request checks
func (ws *WebServer) guardAPI(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !ws.requestAllowed(r) {
			log.Printf("WebServer: Rejected request to %s (Host=%q Origin=%q)", r.URL.Path, r.Host, r.Header.Get("Origin"))
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusForbidden)
			json.NewEncoder(w).Encode(map[string]string{"error": "Forbidden origin"})
			return
		}
		if r.Method == http.MethodPost {
			mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
			if err != nil || mediaType != "application/json" {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusUnsupportedMediaType)
				json.NewEncoder(w).Encode(map[string]string{"error": "Content-Type must be application/json"})
				return
			}
		}
		next(w, r)
	}
}

// handleWebSocket handles WebSocket connections for live trade streaming
// Supports multi-subscribe protocol:
//   - {"action": "subscribe", "event": "slug"} - subscribe to an event
//   - {"action": "unsubscribe", "event": "slug"} - unsubscribe from an event
//   - {"action": "list"} - list current subscriptions
func (ws *WebServer) handleWebSocket(w http.ResponseWriter, r *http.Request) {
	// Upgrade to WebSocket
	conn, err := ws.upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("WebServer: Failed to upgrade connection: %v", err)
		return
	}
	defer conn.Close()
	defer ws.liveManager.UnsubscribeWeb(conn)

	// Register before anything can write to this conn: acks (this
	// goroutine) and trade broadcasts (RTDS goroutine) share it, and both
	// go through the registry's serialized write path.
	ws.liveManager.RegisterWebConn(conn)

	log.Printf("WebServer: Client connected")

	// Handle incoming messages
	for {
		_, message, err := conn.ReadMessage()
		if err != nil {
			if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseAbnormalClosure) {
				log.Printf("WebServer: Connection error: %v", err)
			}
			break
		}

		// Parse the message
		var msg wsMessage
		if err := json.Unmarshal(message, &msg); err != nil {
			ws.sendResponse(conn, wsResponse{
				Type:    "error",
				Message: "Invalid JSON message",
			})
			continue
		}

		// Handle the action
		switch msg.Action {
		case "subscribe":
			ws.handleSubscribe(conn, msg.Event, msg.AllMarkets)
		case "unsubscribe":
			ws.handleUnsubscribe(conn, msg.Event)
		case "list":
			ws.handleList(conn)
		default:
			ws.sendResponse(conn, wsResponse{
				Type:    "error",
				Message: fmt.Sprintf("Unknown action: %s", msg.Action),
			})
		}
	}

	log.Printf("WebServer: Client disconnected")
}

// handleSubscribe handles a subscribe request
func (ws *WebServer) handleSubscribe(conn *websocket.Conn, eventSlug string, allMarkets bool) {
	if eventSlug == "" {
		ws.sendResponse(conn, wsResponse{
			Type:    "error",
			Message: "Missing event slug",
		})
		return
	}

	// Check if already subscribed
	if ws.liveManager.IsWebSubscribed(conn, eventSlug) {
		ws.sendResponse(conn, wsResponse{
			Type:    "error",
			Event:   eventSlug,
			Message: "Already subscribed to this event",
		})
		return
	}

	// Get event info and subscribe
	eventInfo, err := ws.liveManager.resolver.GetEventInfo(context.Background(), eventSlug)
	if err != nil {
		ws.sendResponse(conn, wsResponse{
			Type:    "error",
			Event:   eventSlug,
			Message: fmt.Sprintf("Event not found: %s", err.Error()),
		})
		return
	}

	if err := ws.liveManager.SubscribeWeb(conn, eventSlug, allMarkets); err != nil {
		ws.sendResponse(conn, wsResponse{
			Type:    "error",
			Event:   eventSlug,
			Message: err.Error(),
		})
		return
	}

	resp := wsResponse{
		Type:  "subscribed",
		Event: eventSlug,
		Title: eventInfo.Title,
	}

	if pinned := pinnedMarket(ws.liveManager.resolver, eventInfo, eventSlug); pinned != nil {
		// The subscriber addressed a specific sub-market — its outcomes
		// drive the panel's primary buy buttons, not the Moneyline's.
		resp.Market = pinned.Slug
		resp.MarketQuestion = pinned.Question
		resp.Outcomes = pinned.GetOutcomes()
	} else {
		// Extract outcomes from the Moneyline market using resolver's logic
		mlMarkets := ws.liveManager.resolver.GetAllMLMarkets(eventInfo)
		if len(mlMarkets) >= 3 {
			// 3-way market (soccer): use market short names as outcomes
			for _, m := range mlMarkets {
				shortName := ExtractMarketShortName(m.Question)
				if shortName != "" {
					resp.Outcomes = append(resp.Outcomes, shortName)
				}
			}
		} else if len(mlMarkets) > 0 {
			// 2-way market (NBA, esports): use outcomes from primary market
			resp.Outcomes = mlMarkets[0].GetOutcomes()
		}
	}

	ws.sendResponse(conn, resp)
}

// pinnedMarket returns the market the subscriber addressed directly, when
// the subscribe slug names a tradeable non-ML market within the event.
// Subscribing with a market slug (a Polymarket market page URL ends in
// one) pins that market as the panel's primary trade target — otherwise
// the buy buttons would silently trade the event's Moneyline instead of
// the market the user asked for.
func pinnedMarket(resolver *EventSlugResolver, eventInfo *EventInfo, slug string) *MarketInfo {
	var match *MarketInfo
	for i := range eventInfo.Markets {
		if eventInfo.Markets[i].Slug == slug {
			match = &eventInfo.Markets[i]
			break
		}
	}
	if match == nil || !match.Active || match.Closed {
		return nil
	}
	for _, ml := range resolver.GetAllMLMarkets(eventInfo) {
		if ml.ID == match.ID {
			return nil // it's the Moneyline: normal panel behavior
		}
	}
	return match
}

// handleUnsubscribe handles an unsubscribe request
func (ws *WebServer) handleUnsubscribe(conn *websocket.Conn, eventSlug string) {
	if eventSlug == "" {
		ws.sendResponse(conn, wsResponse{
			Type:    "error",
			Message: "Missing event slug",
		})
		return
	}

	if !ws.liveManager.UnsubscribeWebFromEvent(conn, eventSlug) {
		ws.sendResponse(conn, wsResponse{
			Type:    "error",
			Event:   eventSlug,
			Message: "Not subscribed to this event",
		})
		return
	}

	ws.sendResponse(conn, wsResponse{
		Type:  "unsubscribed",
		Event: eventSlug,
	})
}

// handleList handles a list subscriptions request
func (ws *WebServer) handleList(conn *websocket.Conn) {
	events := ws.liveManager.GetWebConnectionEvents(conn)
	ws.sendResponse(conn, wsResponse{
		Type:   "subscriptions",
		Events: events,
	})
}

// sendResponse sends a JSON response to the client through the registry's
// serialized write path — a raw conn.WriteMessage here would race the
// RTDS goroutine's trade broadcasts on the same conn.
func (ws *WebServer) sendResponse(conn *websocket.Conn, resp wsResponse) {
	data, err := json.Marshal(resp)
	if err != nil {
		log.Printf("WebServer: Failed to marshal response: %v", err)
		return
	}
	if err := ws.liveManager.WriteWeb(conn, data); err != nil {
		log.Printf("WebServer: Failed to send response: %v", err)
	}
}

// handleHealth returns a simple health check response
func (ws *WebServer) handleHealth(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	subscribedEvents := ws.liveManager.subscriptions.GetAllSubscribedEvents()
	trackedAssets := ws.liveManager.GetTrackedAssetCount()

	resp := map[string]interface{}{
		"status":            "ok",
		"rtds_connected":    ws.liveManager.IsConnected(),
		"rtds_subscribed":   ws.liveManager.IsSubscribed(),
		"subscribed_events": subscribedEvents,
		"tracked_assets":    trackedAssets,
	}

	json.NewEncoder(w).Encode(resp)
}

// Sub-market listing types
type marketListItem struct {
	Slug     string   `json:"slug"`
	Question string   `json:"question"`
	Outcomes []string `json:"outcomes"`
	Prices   []string `json:"prices"` // indicative — fills price off the live book
}

type marketListResponse struct {
	Event   string           `json:"event"`
	Markets []marketListItem `json:"markets"`
}

// handleListEventMarkets returns an event's tradeable sub-markets for the
// picker (the ML markets have their own buttons and are excluded).
func (ws *WebServer) handleListEventMarkets(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	slug := r.PathValue("slug")
	if slug == "" {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error": "Missing event slug"})
		return
	}

	eventInfo, err := ws.liveManager.resolver.GetEventInfo(r.Context(), slug)
	if err != nil {
		w.WriteHeader(http.StatusNotFound)
		json.NewEncoder(w).Encode(map[string]string{"error": "Event not found"})
		return
	}

	resp := marketListResponse{Event: slug, Markets: []marketListItem{}}
	for _, m := range ws.liveManager.resolver.GetSubMarkets(eventInfo) {
		resp.Markets = append(resp.Markets, marketListItem{
			Slug:     m.Slug,
			Question: m.Question,
			Outcomes: m.GetOutcomes(),
			Prices:   m.GetOutcomePrices(),
		})
	}

	json.NewEncoder(w).Encode(resp)
}

// Auth response types
type authInitResponse struct {
	Token       string `json:"token"`
	TelegramURL string `json:"telegramUrl"`
	ExpiresAt   int64  `json:"expiresAt"`
}

type authStatusResponse struct {
	Status        string  `json:"status"`
	WalletAddress *string `json:"walletAddress,omitempty"`
	ProxyAddress  *string `json:"proxyAddress,omitempty"`
}

type authCompleteResponse struct {
	Success       bool    `json:"success"`
	TelegramID    *int64  `json:"telegramId,omitempty"`
	WalletAddress *string `json:"walletAddress,omitempty"`
	ProxyAddress  *string `json:"proxyAddress,omitempty"`
	Error         string  `json:"error,omitempty"`
}

// handleAuthInit creates a new login token and returns the Telegram deep link
func (ws *WebServer) handleAuthInit(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	// Without a configured bot username there is no valid deep link to
	// hand out — refuse rather than linking to a wrong bot.
	if ws.config == nil || ws.config.Telegram.BotUsername == "" {
		log.Printf("WebServer: auth init refused — TELEGRAM_BOT_USERNAME not configured")
		w.WriteHeader(http.StatusServiceUnavailable)
		json.NewEncoder(w).Encode(map[string]string{"error": "Bot username not configured"})
		return
	}

	// Check if login token repo is available
	if ws.loginTokenRepo == nil {
		w.WriteHeader(http.StatusServiceUnavailable)
		json.NewEncoder(w).Encode(map[string]string{"error": "Authentication not configured"})
		return
	}

	// Create a new login token with 5 minute expiry
	token, err := ws.loginTokenRepo.Create(r.Context(), 5*time.Minute)
	if err != nil {
		log.Printf("WebServer: Failed to create login token: %v", err)
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(map[string]string{"error": "Failed to create login token"})
		return
	}

	// Build Telegram deep link URL
	tokenStr := repositories.TokenToString(token.Token)
	telegramURL := fmt.Sprintf("https://t.me/%s?start=login_%s", ws.config.Telegram.BotUsername, tokenStr)

	resp := authInitResponse{
		Token:       tokenStr,
		TelegramURL: telegramURL,
		ExpiresAt:   token.ExpiresAt.Unix(),
	}

	json.NewEncoder(w).Encode(resp)
}

// handleAuthStatus checks the status of a login token
func (ws *WebServer) handleAuthStatus(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	// Get token from query parameter
	tokenStr := r.URL.Query().Get("token")
	if tokenStr == "" {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error": "Missing token parameter"})
		return
	}

	// Check if login token repo is available
	if ws.loginTokenRepo == nil {
		w.WriteHeader(http.StatusServiceUnavailable)
		json.NewEncoder(w).Encode(map[string]string{"error": "Authentication not configured"})
		return
	}

	// Get token status
	token, err := ws.loginTokenRepo.GetByToken(r.Context(), tokenStr)
	if err != nil {
		log.Printf("WebServer: Failed to get login token: %v", err)
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(map[string]string{"error": "Failed to check token status"})
		return
	}

	if token == nil {
		w.WriteHeader(http.StatusNotFound)
		json.NewEncoder(w).Encode(map[string]string{"error": "Token not found"})
		return
	}

	// Check if expired
	status := token.Status
	if time.Now().After(token.ExpiresAt) && status == database.LoginTokenStatusPending {
		status = database.LoginTokenStatusExpired
	}

	resp := authStatusResponse{
		Status:        status,
		WalletAddress: token.WalletAddress,
		ProxyAddress:  token.ProxyAddress,
	}

	json.NewEncoder(w).Encode(resp)
}

// handleAuthComplete completes the login and returns user data
func (ws *WebServer) handleAuthComplete(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	// Parse request body
	var req struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid request body"})
		return
	}

	if req.Token == "" {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error": "Missing token"})
		return
	}

	// Check if login token repo is available
	if ws.loginTokenRepo == nil {
		w.WriteHeader(http.StatusServiceUnavailable)
		json.NewEncoder(w).Encode(authCompleteResponse{Success: false, Error: "Authentication not configured"})
		return
	}

	// Mark token as used and get user data
	token, err := ws.loginTokenRepo.MarkUsed(r.Context(), req.Token)
	if err != nil {
		log.Printf("WebServer: Failed to complete login: %v", err)
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(authCompleteResponse{Success: false, Error: "Token not authenticated or expired"})
		return
	}

	resp := authCompleteResponse{
		Success:       true,
		TelegramID:    token.TelegramID,
		WalletAddress: token.WalletAddress,
		ProxyAddress:  token.ProxyAddress,
	}

	json.NewEncoder(w).Encode(resp)
}

// Trade request/response types
type webTradeSession struct {
	TelegramID    int64  `json:"telegramId"`
	WalletAddress string `json:"walletAddress"`
	ProxyAddress  string `json:"proxyAddress"`
}

// webTradeData addresses one side of one market. When MarketSlug is set the
// trade targets that market directly (used by the sub-market picker) and
// MarketIndex is ignored; otherwise MarketIndex picks the market within the
// event's ML list (0 for 2-way events, 0-2 for 3-way soccer). OutcomeIndex
// picks the side within the chosen market (see CONTEXT.md: Market Index vs
// Outcome Index, Sub-market).
type webTradeData struct {
	EventSlug    string  `json:"eventSlug"`
	MarketSlug   string  `json:"marketSlug"` // sub-market target; empty = ML by MarketIndex
	MarketIndex  int     `json:"marketIndex"`
	OutcomeIndex int     `json:"outcomeIndex"`
	Side         string  `json:"side"`
	Amount       float64 `json:"amount"`
}

type webTradeRequest struct {
	Session webTradeSession `json:"session"`
	Trade   webTradeData    `json:"trade"`
}

type webTradeResponse struct {
	Success bool   `json:"success"`
	OrderID string `json:"orderId,omitempty"`
	Error   string `json:"error,omitempty"`
	Message string `json:"message,omitempty"`
}

// maxWebTradeAmount caps a single web buy in USDC — a fat-finger guard
// mirroring the UI input's max attribute.
const maxWebTradeAmount = 1000

// validateWebTrade checks the request-shape rules that need no market data.
// The web endpoint is buy-only: selling is a "how much of my position at
// what P&L" decision that belongs in the Telegram flow, which sells exact
// share counts (SharesRaw) instead of back-solving them from a USD amount.
func validateWebTrade(t webTradeData) error {
	if t.EventSlug == "" {
		return fmt.Errorf("eventSlug is required")
	}
	if side := strings.ToUpper(t.Side); side != "BUY" {
		if side == "SELL" {
			return fmt.Errorf("selling is not available on the web — use the Telegram bot")
		}
		return fmt.Errorf("side must be BUY")
	}
	if t.Amount <= 0 {
		return fmt.Errorf("amount must be positive")
	}
	if t.Amount > maxWebTradeAmount {
		return fmt.Errorf("amount must be at most %d USDC", maxWebTradeAmount)
	}
	// MarketIndex only matters for the ML path; a picker trade carries a
	// MarketSlug and leaves MarketIndex unused.
	if t.MarketSlug == "" && t.MarketIndex < 0 {
		return fmt.Errorf("marketIndex must be non-negative")
	}
	if t.OutcomeIndex < 0 || t.OutcomeIndex > 1 {
		return fmt.Errorf("outcomeIndex must be 0 or 1")
	}
	return nil
}

// resolveWebTradeBySlug maps a market slug + outcome onto a concrete token,
// searching all of the event's markets (not just the ML list). Rejects a
// closed or inactive market — the CLOB would reject the order anyway, but
// failing here gives a clearer error.
func resolveWebTradeBySlug(markets []MarketInfo, slug string, outcomeIndex int) (marketID, tokenID, outcome string, err error) {
	for i := range markets {
		m := &markets[i]
		if m.Slug != slug {
			continue
		}
		if m.Closed {
			return "", "", "", fmt.Errorf("market %s is closed", slug)
		}
		if !m.Active {
			return "", "", "", fmt.Errorf("market %s is not active", slug)
		}

		tokenIDs := m.GetClobTokenIds()
		if outcomeIndex < 0 || outcomeIndex >= len(tokenIDs) {
			return "", "", "", fmt.Errorf("outcomeIndex %d out of range (market has %d outcomes)", outcomeIndex, len(tokenIDs))
		}
		outcomes := m.GetOutcomes()
		if outcomeIndex < len(outcomes) {
			outcome = outcomes[outcomeIndex]
		}
		return m.ID, tokenIDs[outcomeIndex], outcome, nil
	}
	return "", "", "", fmt.Errorf("market not found: %s", slug)
}

// resolveWebTrade maps (marketIndex, outcomeIndex) onto a concrete market
// and token. mlMarkets is the event's Moneyline market list in resolver
// order — the same order the subscribe response presents outcomes in, so
// the indexes the frontend sends line up positionally.
func resolveWebTrade(mlMarkets []*MarketInfo, marketIndex, outcomeIndex int) (marketID, tokenID, outcome string, err error) {
	if len(mlMarkets) == 0 {
		return "", "", "", fmt.Errorf("event has no Moneyline markets")
	}
	if marketIndex < 0 || marketIndex >= len(mlMarkets) {
		return "", "", "", fmt.Errorf("marketIndex %d out of range (event has %d markets)", marketIndex, len(mlMarkets))
	}
	market := mlMarkets[marketIndex]

	tokenIDs := market.GetClobTokenIds()
	if outcomeIndex < 0 || outcomeIndex >= len(tokenIDs) {
		return "", "", "", fmt.Errorf("outcomeIndex %d out of range (market has %d outcomes)", outcomeIndex, len(tokenIDs))
	}

	// The outcome label is display metadata only — never identity.
	outcomes := market.GetOutcomes()
	if outcomeIndex < len(outcomes) {
		outcome = outcomes[outcomeIndex]
	}

	return market.ID, tokenIDs[outcomeIndex], outcome, nil
}

// writeJSONError writes a {"error": msg} body with the given status.
func writeJSONError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

// authWebSession validates a web request's session against the stored user
// record — the single source of truth for "is this session allowed to act as
// this user", shared by /api/trade and the Live Watch endpoints so none is
// weaker than another. It requires a present TelegramID, an existing user, and
// a session ProxyAddress that matches the user's Trading Wallet. On failure it
// writes the JSON error + status and returns (nil, false); on success it
// returns the user. The nil-user check matters: GetByTelegramID returns
// (nil, nil) for a missing row, which would otherwise nil-panic downstream.
func (ws *WebServer) authWebSession(w http.ResponseWriter, r *http.Request, session webTradeSession) (*database.User, bool) {
	if ws.userRepo == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "User authentication not configured")
		return nil, false
	}
	if session.TelegramID == 0 {
		writeJSONError(w, http.StatusUnauthorized, "Not authenticated")
		return nil, false
	}
	user, err := ws.userRepo.GetByTelegramID(r.Context(), session.TelegramID)
	if err != nil {
		log.Printf("WebServer: Failed to get user: %v", err)
		writeJSONError(w, http.StatusUnauthorized, "User not found")
		return nil, false
	}
	if user == nil {
		writeJSONError(w, http.StatusUnauthorized, "User not found")
		return nil, false
	}
	if user.ProxyAddress == "" || user.ProxyAddress != session.ProxyAddress {
		writeJSONError(w, http.StatusUnauthorized, "Wallet address mismatch")
		return nil, false
	}
	return user, true
}

// handleTrade handles trade execution from the web interface
func (ws *WebServer) handleTrade(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	// Parse request body
	var req webTradeRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(webTradeResponse{Success: false, Error: "Invalid request body"})
		return
	}

	// Validate trade data before dependency checks, so a malformed request
	// never reads as "trading not configured"
	if err := validateWebTrade(req.Trade); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(webTradeResponse{Success: false, Error: err.Error()})
		return
	}

	// Check if trading is configured
	if ws.tradingClient == nil || ws.walletManager == nil {
		w.WriteHeader(http.StatusServiceUnavailable)
		json.NewEncoder(w).Encode(webTradeResponse{Success: false, Error: "Trading not configured"})
		return
	}

	// Authenticate the session against the stored user. Same validation the
	// Live Watch endpoints use (shared helper), so no path is weaker: a
	// present TelegramID, an existing user, and a matching Trading Wallet.
	user, ok := ws.authWebSession(w, r, req.Session)
	if !ok {
		return
	}

	// Check if user has encrypted key
	if user.EncryptedKey == "" {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(webTradeResponse{Success: false, Error: "No wallet configured for this user"})
		return
	}

	// Decrypt user's private key
	decryptedWallet, err := ws.walletManager.DecryptPrivateKey(user.EncryptedKey)
	if err != nil {
		log.Printf("WebServer: Failed to decrypt wallet: %v", err)
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(webTradeResponse{Success: false, Error: "Failed to decrypt wallet"})
		return
	}

	// Resolve the event's ML markets and map the indexes onto a token
	eventInfo, err := ws.liveManager.resolver.GetEventInfo(r.Context(), req.Trade.EventSlug)
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(webTradeResponse{Success: false, Error: "Event not found"})
		return
	}

	var marketID, tokenID, outcome string
	if req.Trade.MarketSlug != "" {
		// Sub-market picker: address the market directly by slug
		marketID, tokenID, outcome, err = resolveWebTradeBySlug(eventInfo.Markets, req.Trade.MarketSlug, req.Trade.OutcomeIndex)
	} else {
		mlMarkets := ws.liveManager.resolver.GetAllMLMarkets(eventInfo)
		marketID, tokenID, outcome, err = resolveWebTrade(mlMarkets, req.Trade.MarketIndex, req.Trade.OutcomeIndex)
	}
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(webTradeResponse{Success: false, Error: err.Error()})
		return
	}
	log.Printf("WebServer: Trade resolved: event=%s marketSlug=%q market=%s outcome=%s token=%s",
		req.Trade.EventSlug, req.Trade.MarketSlug, marketID, outcome, tokenID)

	// Build trade request; the executor fills the fee fields
	tradeReq := &polymarket.TradeRequest{
		MarketID:    marketID,
		TokenID:     tokenID,
		Side:        "BUY", // validateWebTrade enforces buy-only
		Outcome:     outcome,
		Amount:      req.Trade.Amount,
		Price:       0, // Market order - uses VWAP
		OrderType:   polymarket.OrderTypeGTC,
		AccountType: user.AccountType,
	}

	// Execute: credentials, L2 auth pre-check, fee discovery, submission
	proxyAddr := common.HexToAddress(user.ProxyAddress)
	result, err := ws.tradeExecutor.Execute(r.Context(), decryptedWallet.PrivateKey, proxyAddr, tradeReq)
	if err != nil {
		log.Printf("WebServer: Trade execution failed: %v", err)
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(webTradeResponse{Success: false, Error: fmt.Sprintf("Trade failed: %v", err)})
		return
	}

	if !result.Success {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(webTradeResponse{Success: false, Error: result.ErrorMsg})
		return
	}

	// Every successful buy makes the buyer a Held Watch holder, so a
	// comeback-snipe crash on the token they now hold reaches them even without
	// an open positions view (issue #64). Off the response path.
	go ws.liveManager.RegisterHeldBuy(user.TelegramID, req.Trade.EventSlug, tokenID, eventInfo)

	json.NewEncoder(w).Encode(webTradeResponse{
		Success: true,
		OrderID: result.OrderID,
		Message: "Trade executed successfully",
	})
}

// Live Watch management request/response types (ADR 0008 phase 3). All reuse
// webTradeSession for the session block, so the page sends the same session
// shape it already sends to /api/trade.
type subscriptionRequest struct {
	Session webTradeSession `json:"session"`
	Tape    bool            `json:"tape"`
}

type sessionRequest struct {
	Session webTradeSession `json:"session"`
}

type subscriptionResponse struct {
	Success    bool   `json:"success"`
	EventTitle string `json:"eventTitle,omitempty"`
	Tape       bool   `json:"tape"`
}

type subscriptionListItem struct {
	EventSlug string `json:"eventSlug"`
	Tape      bool   `json:"tape"`
}

// handlePutSubscription creates (or updates the tape flag of) a Live Watch on
// the event named in the path. It funnels through SubscribeTelegram — the same
// method /live uses — so the watch is one durable, refreshed object. The
// 30-watch cap is enforced here: a new distinct event past the cap is 409; a
// re-subscribe to an existing watch is never capped.
func (ws *WebServer) handlePutSubscription(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	slug := r.PathValue("slug")
	if slug == "" {
		writeJSONError(w, http.StatusBadRequest, "Missing event slug")
		return
	}

	var req subscriptionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSONError(w, http.StatusBadRequest, "Invalid request body")
		return
	}

	user, ok := ws.authWebSession(w, r, req.Session)
	if !ok {
		return
	}

	if ws.watches == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "Live watches not configured")
		return
	}

	// Cap only new distinct events; a re-subscribe (tape flip) is exempt.
	existing := ws.watches.GetUserSubscriptions(user.TelegramID)
	if !slices.Contains(existing, slug) && len(existing) >= maxLiveWatchesPerUser {
		writeJSONError(w, http.StatusConflict, fmt.Sprintf("watch limit reached (%d)", maxLiveWatchesPerUser))
		return
	}

	eventInfo, err := ws.watches.SubscribeTelegram(r.Context(), user.TelegramID, slug, req.Tape)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, fmt.Sprintf("Event not found: %v", err))
		return
	}

	resp := subscriptionResponse{Success: true, Tape: req.Tape}
	if eventInfo != nil {
		resp.EventTitle = eventInfo.Title
	}
	json.NewEncoder(w).Encode(resp)
}

// handleDeleteSubscription drops a Live Watch. 404 when the user has no watch
// on that event.
func (ws *WebServer) handleDeleteSubscription(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	slug := r.PathValue("slug")
	if slug == "" {
		writeJSONError(w, http.StatusBadRequest, "Missing event slug")
		return
	}

	var req sessionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSONError(w, http.StatusBadRequest, "Invalid request body")
		return
	}

	user, ok := ws.authWebSession(w, r, req.Session)
	if !ok {
		return
	}

	if ws.watches == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "Live watches not configured")
		return
	}

	if !ws.watches.UnsubscribeTelegram(user.TelegramID, slug) {
		writeJSONError(w, http.StatusNotFound, "Not subscribed to this event")
		return
	}

	json.NewEncoder(w).Encode(subscriptionResponse{Success: true})
}

// handleListSubscriptions returns the user's Live Watches with their tape
// flags. POST-with-body (not GET) so the authenticated session travels in the
// JSON body, consistent with the page's other authenticated calls and keeping
// the ProxyAddress out of URLs/logs.
func (ws *WebServer) handleListSubscriptions(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	var req sessionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSONError(w, http.StatusBadRequest, "Invalid request body")
		return
	}

	user, ok := ws.authWebSession(w, r, req.Session)
	if !ok {
		return
	}

	if ws.watches == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "Live watches not configured")
		return
	}

	slugs := ws.watches.GetUserSubscriptions(user.TelegramID)
	items := make([]subscriptionListItem, 0, len(slugs))
	for _, slug := range slugs {
		items = append(items, subscriptionListItem{
			EventSlug: slug,
			Tape:      ws.watches.IsTapeSubscription(user.TelegramID, slug),
		})
	}
	json.NewEncoder(w).Encode(items)
}
