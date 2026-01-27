package live

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"log"
	"net/http"
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
		upgrader: websocket.Upgrader{
			CheckOrigin: func(r *http.Request) bool {
				return true // Allow all origins for development
			},
		},
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
	mux.HandleFunc("/api/auth/init", ws.handleAuthInit)
	mux.HandleFunc("/api/auth/status", ws.handleAuthStatus)
	mux.HandleFunc("/api/auth/complete", ws.handleAuthComplete)

	// Trade endpoint
	mux.HandleFunc("/api/trade", ws.handleTrade)

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

	// Extract outcomes from the first active market
	var outcomes []string
	for _, market := range eventInfo.Markets {
		if market.Active && !market.Closed {
			outcomes = market.GetOutcomes()
			break
		}
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

type webTradeData struct {
	EventSlug    string  `json:"eventSlug"`
	MarketID     string  `json:"marketId"`
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

// handleTrade handles trade execution from the web interface
func (ws *WebServer) handleTrade(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	// Only allow POST
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		json.NewEncoder(w).Encode(webTradeResponse{Success: false, Error: "Method not allowed"})
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

	// Parse request body
	var req webTradeRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(webTradeResponse{Success: false, Error: "Invalid request body"})
		return
	}

	// Validate session
	if req.Session.TelegramID == 0 {
		w.WriteHeader(http.StatusUnauthorized)
		json.NewEncoder(w).Encode(webTradeResponse{Success: false, Error: "Not authenticated"})
		return
	}

	// Validate trade data
	if req.Trade.EventSlug == "" && req.Trade.MarketID == "" {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(webTradeResponse{Success: false, Error: "Event slug or market ID required"})
		return
	}

	if req.Trade.Amount <= 0 {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(webTradeResponse{Success: false, Error: "Amount must be positive"})
		return
	}

	side := strings.ToUpper(req.Trade.Side)
	if side != "BUY" && side != "SELL" {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(webTradeResponse{Success: false, Error: "Side must be BUY or SELL"})
		return
	}

	if req.Trade.OutcomeIndex < 0 || req.Trade.OutcomeIndex > 1 {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(webTradeResponse{Success: false, Error: "Outcome index must be 0 or 1"})
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

	// Resolve event to market and token ID
	var tokenID string
	var marketID string
	var outcome string

	if req.Trade.MarketID != "" {
		// Direct market ID provided
		marketID = req.Trade.MarketID
		// Fetch market info to get token ID
		eventInfo, err := ws.liveManager.resolver.GetEventInfo(r.Context(), req.Trade.EventSlug)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(webTradeResponse{Success: false, Error: "Event not found"})
			return
		}
		for _, market := range eventInfo.Markets {
			if market.ID == marketID {
				tokenIDs := market.GetClobTokenIds()
				if len(tokenIDs) > req.Trade.OutcomeIndex {
					tokenID = tokenIDs[req.Trade.OutcomeIndex]
				}
				outcomes := market.GetOutcomes()
				if len(outcomes) > req.Trade.OutcomeIndex {
					outcome = outcomes[req.Trade.OutcomeIndex]
				}
				break
			}
		}
	} else {
		// Resolve from event slug - use first active ML market
		eventInfo, err := ws.liveManager.resolver.GetEventInfo(r.Context(), req.Trade.EventSlug)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(webTradeResponse{Success: false, Error: "Event not found"})
			return
		}

		// Find the Moneyline market (not spreads/totals/props)
		// Moneyline markets have simple questions like "Team A vs. Team B" without:
		// - "Spread" or spread numbers like "(-10.5)"
		// - "O/U" or "Over" or "Under" (totals)
		// - "Points", "Rebounds", "Assists" (player props)
		// - "1H" or "1Q" (half/quarter markets)
		for _, market := range eventInfo.Markets {
			if market.Active && !market.Closed {
				tokenIDs := market.GetClobTokenIds()
				outcomes := market.GetOutcomes()
				if len(tokenIDs) >= 2 && len(outcomes) >= 2 {
					q := market.Question
					// Skip non-moneyline markets based on question
					if strings.Contains(q, "Spread") ||
						strings.Contains(q, "O/U") ||
						strings.Contains(q, "Over") ||
						strings.Contains(q, "Under") ||
						strings.Contains(q, "Points") ||
						strings.Contains(q, "Rebounds") ||
						strings.Contains(q, "Assists") ||
						strings.Contains(q, "1H ") ||
						strings.Contains(q, "1Q ") ||
						strings.Contains(q, "(-") ||
						strings.Contains(q, "(+") {
						continue
					}
					// Also skip if outcomes contain Over/Under
					if outcomes[0] == "Over" || outcomes[0] == "Under" ||
						outcomes[0] == "Yes" || outcomes[0] == "No" {
						continue
					}
					// This looks like a moneyline market
					marketID = market.ID
					if req.Trade.OutcomeIndex < len(tokenIDs) {
						tokenID = tokenIDs[req.Trade.OutcomeIndex]
					}
					if req.Trade.OutcomeIndex < len(outcomes) {
						outcome = outcomes[req.Trade.OutcomeIndex]
					}
					log.Printf("WebServer: Selected moneyline market: %s, question: %s, outcomes: %v", marketID, q, outcomes)
					break
				}
			}
		}
		// Fallback: if no moneyline found, use first active market with 2 team-like outcomes
		if tokenID == "" {
			for _, market := range eventInfo.Markets {
				if market.Active && !market.Closed {
					tokenIDs := market.GetClobTokenIds()
					outcomes := market.GetOutcomes()
					if len(tokenIDs) >= 2 && len(outcomes) >= 2 {
						marketID = market.ID
						if req.Trade.OutcomeIndex < len(tokenIDs) {
							tokenID = tokenIDs[req.Trade.OutcomeIndex]
						}
						if req.Trade.OutcomeIndex < len(outcomes) {
							outcome = outcomes[req.Trade.OutcomeIndex]
						}
						log.Printf("WebServer: Fallback to first market: %s, question: %s, outcomes: %v", marketID, market.Question, outcomes)
						break
					}
				}
			}
		}
	}

	if tokenID == "" {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(webTradeResponse{Success: false, Error: "Could not resolve market token ID"})
		return
	}

	// Fetch market info from CLOB API to get taker fee and negRisk
	var takerFeeBps int
	var negRisk bool
	marketInfo, err := ws.tradingClient.GetMarketInfo(r.Context(), tokenID)
	if err != nil {
		log.Printf("WebServer: Failed to get market info: %v (using defaults)", err)
		// Continue with defaults (0 fee, no negRisk)
	} else {
		takerFeeBps = marketInfo.TakerBaseFee
		negRisk = marketInfo.NegRisk
		log.Printf("WebServer: Market info fetched - takerFeeBps=%d, negRisk=%v", takerFeeBps, negRisk)
	}

	// Build trade request
	tradeReq := &polymarket.TradeRequest{
		MarketID:     marketID,
		TokenID:      tokenID,
		Side:         side,
		Outcome:      outcome,
		Amount:       req.Trade.Amount,
		Price:        0, // Market order - uses VWAP
		OrderType:    polymarket.OrderTypeGTC,
		TakerFeeBps:  takerFeeBps,
		NegativeRisk: negRisk,
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
