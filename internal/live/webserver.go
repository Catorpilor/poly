package live

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"log"
	"mime"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/gorilla/websocket"
	"github.com/Catorpilor/poly/internal/config"
	"github.com/Catorpilor/poly/internal/database"
	"github.com/Catorpilor/poly/internal/database/repositories"
	"github.com/Catorpilor/poly/internal/polymarket"
	"github.com/Catorpilor/poly/internal/wallet"
)

// WebSocket message types
type wsMessage struct {
	Action     string `json:"action"`     // subscribe, unsubscribe, list
	Event      string `json:"event"`      // event slug
	AllMarkets bool   `json:"allMarkets"` // true to show all markets, false for ML only
}

type wsResponse struct {
	Type     string   `json:"type"`              // subscribed, unsubscribed, subscriptions, error
	Event    string   `json:"event,omitempty"`   // event slug
	Title    string   `json:"title,omitempty"`   // event title (for subscribe response)
	Outcomes []string `json:"outcomes,omitempty"` // outcome names for the main market (for subscribe response)
	Events   []string `json:"events,omitempty"`  // list of subscribed events
	Message  string   `json:"message,omitempty"` // error message
}

//go:embed static/*
var staticFiles embed.FS

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
	allowedHost    string // hostname from LIVE_WEB_URL, allowed alongside localhost/IP literals
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
	mux.HandleFunc("/ws", ws.handleWebSocket)

	// Health check
	mux.HandleFunc("/health", ws.handleHealth)

	// Auth endpoints for Telegram login
	mux.HandleFunc("/api/auth/init", ws.guardAPI(ws.handleAuthInit))
	mux.HandleFunc("/api/auth/status", ws.guardAPI(ws.handleAuthStatus))
	mux.HandleFunc("/api/auth/complete", ws.guardAPI(ws.handleAuthComplete))

	// Trade endpoint
	mux.HandleFunc("/api/trade", ws.guardAPI(ws.handleTrade))

	ws.httpServer = &http.Server{
		Addr:    fmt.Sprintf(":%d", port),
		Handler: mux,
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

	// Extract outcomes from the Moneyline market using resolver's logic
	var outcomes []string
	mlMarkets := ws.liveManager.resolver.GetAllMLMarkets(eventInfo)
	if len(mlMarkets) >= 3 {
		// 3-way market (soccer): use market short names as outcomes
		for _, m := range mlMarkets {
			shortName := ExtractMarketShortName(m.Question)
			if shortName != "" {
				outcomes = append(outcomes, shortName)
			}
		}
	} else if len(mlMarkets) > 0 {
		// 2-way market (NBA, esports): use outcomes from primary market
		outcomes = mlMarkets[0].GetOutcomes()
	}

	ws.sendResponse(conn, wsResponse{
		Type:     "subscribed",
		Event:    eventSlug,
		Title:    eventInfo.Title,
		Outcomes: outcomes,
	})
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

// sendResponse sends a JSON response to the client
func (ws *WebServer) sendResponse(conn *websocket.Conn, resp wsResponse) {
	data, err := json.Marshal(resp)
	if err != nil {
		log.Printf("WebServer: Failed to marshal response: %v", err)
		return
	}
	if err := conn.WriteMessage(websocket.TextMessage, data); err != nil {
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

	// Only allow POST
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		json.NewEncoder(w).Encode(map[string]string{"error": "Method not allowed"})
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
	botUsername := "poly_trade_test_bot"
	if ws.config != nil && ws.config.Telegram.BotUsername != "" {
		botUsername = ws.config.Telegram.BotUsername
	}
	telegramURL := fmt.Sprintf("https://t.me/%s?start=login_%s", botUsername, tokenStr)

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

	// Only allow GET
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		json.NewEncoder(w).Encode(map[string]string{"error": "Method not allowed"})
		return
	}

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

	// Only allow POST
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		json.NewEncoder(w).Encode(map[string]string{"error": "Method not allowed"})
		return
	}

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

// webTradeData addresses one side of one Moneyline market: MarketIndex picks
// the market within the event's ML list (0 for 2-way events, 0-2 for 3-way
// soccer), OutcomeIndex picks the side within it (see CONTEXT.md: Market
// Index vs Outcome Index).
type webTradeData struct {
	EventSlug    string  `json:"eventSlug"`
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
	if t.MarketIndex < 0 {
		return fmt.Errorf("marketIndex must be non-negative")
	}
	if t.OutcomeIndex < 0 || t.OutcomeIndex > 1 {
		return fmt.Errorf("outcomeIndex must be 0 or 1")
	}
	return nil
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

// handleTrade handles trade execution from the web interface
func (ws *WebServer) handleTrade(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	// Only allow POST
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		json.NewEncoder(w).Encode(webTradeResponse{Success: false, Error: "Method not allowed"})
		return
	}

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

	// Check if user repo is available
	if ws.userRepo == nil {
		w.WriteHeader(http.StatusServiceUnavailable)
		json.NewEncoder(w).Encode(webTradeResponse{Success: false, Error: "User authentication not configured"})
		return
	}

	// Validate session
	if req.Session.TelegramID == 0 {
		w.WriteHeader(http.StatusUnauthorized)
		json.NewEncoder(w).Encode(webTradeResponse{Success: false, Error: "Not authenticated"})
		return
	}

	// Fetch user from database
	user, err := ws.userRepo.GetByTelegramID(r.Context(), req.Session.TelegramID)
	if err != nil {
		log.Printf("WebServer: Failed to get user: %v", err)
		w.WriteHeader(http.StatusUnauthorized)
		json.NewEncoder(w).Encode(webTradeResponse{Success: false, Error: "User not found"})
		return
	}

	// Verify wallet address matches (security check)
	if user.ProxyAddress == "" || user.ProxyAddress != req.Session.ProxyAddress {
		w.WriteHeader(http.StatusUnauthorized)
		json.NewEncoder(w).Encode(webTradeResponse{Success: false, Error: "Wallet address mismatch"})
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

	// Get API credentials
	creds, err := ws.tradingClient.GetOrCreateAPICredentials(r.Context(), decryptedWallet.PrivateKey)
	if err != nil {
		log.Printf("WebServer: Failed to get API credentials: %v", err)
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(webTradeResponse{Success: false, Error: "Failed to get API credentials"})
		return
	}

	// Resolve the event's ML markets and map the indexes onto a token
	eventInfo, err := ws.liveManager.resolver.GetEventInfo(r.Context(), req.Trade.EventSlug)
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(webTradeResponse{Success: false, Error: "Event not found"})
		return
	}

	mlMarkets := ws.liveManager.resolver.GetAllMLMarkets(eventInfo)
	marketID, tokenID, outcome, err := resolveWebTrade(mlMarkets, req.Trade.MarketIndex, req.Trade.OutcomeIndex)
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(webTradeResponse{Success: false, Error: err.Error()})
		return
	}
	log.Printf("WebServer: Trade resolved: event=%s market=%s (%d/%d) outcome=%s token=%s",
		req.Trade.EventSlug, marketID, req.Trade.MarketIndex, len(mlMarkets), outcome, tokenID)

	// Get fee rates and negRisk: Gamma feeSchedule for calculation, CLOB for order submission
	var calcFeeBps, orderFeeBps int
	var negRisk bool
	mc := polymarket.NewMarketClient()
	if gammaMarket, err := mc.GetMarketByID(r.Context(), marketID); err != nil {
		log.Printf("WebServer: Failed to get Gamma market for fee schedule: %v (using defaults)", err)
	} else {
		calcFeeBps = gammaMarket.GetFeeRateBps()
		negRisk = gammaMarket.NegRisk
		log.Printf("WebServer: feeSchedule=%+v, feeType=%s, calcFeeBps=%d, negRisk=%v", gammaMarket.FeeSchedule, gammaMarket.FeeType, calcFeeBps, negRisk)
	}
	if feeRate, err := ws.tradingClient.GetFeeRate(r.Context(), tokenID); err != nil {
		log.Printf("WebServer: Failed to get CLOB fee rate: %v (using 0)", err)
	} else {
		orderFeeBps = feeRate
	}

	// Build trade request
	tradeReq := &polymarket.TradeRequest{
		MarketID:     marketID,
		TokenID:      tokenID,
		Side:         "BUY", // validateWebTrade enforces buy-only
		Outcome:      outcome,
		Amount:       req.Trade.Amount,
		Price:        0, // Market order - uses VWAP
		OrderType:    polymarket.OrderTypeGTC,
		TakerFeeBps:  orderFeeBps,
		CalcFeeBps:   calcFeeBps,
		NegativeRisk: negRisk,
		AccountType:  user.AccountType,
	}

	// Execute the trade
	proxyAddr := common.HexToAddress(user.ProxyAddress)
	result, err := ws.tradingClient.ExecuteTrade(r.Context(), decryptedWallet.PrivateKey, proxyAddr, creds, tradeReq)
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

	json.NewEncoder(w).Encode(webTradeResponse{
		Success: true,
		OrderID: result.OrderID,
		Message: "Trade executed successfully",
	})
}
