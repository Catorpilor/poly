package live

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"time"

	"github.com/gorilla/websocket"
	"github.com/Catorpilor/poly/internal/config"
	"github.com/Catorpilor/poly/internal/database"
	"github.com/Catorpilor/poly/internal/database/repositories"
)

// WebSocket message types
type wsMessage struct {
	Action     string `json:"action"`     // subscribe, unsubscribe, list
	Event      string `json:"event"`      // event slug
	AllMarkets bool   `json:"allMarkets"` // true to show all markets, false for ML only
}

type wsResponse struct {
	Type    string   `json:"type"`              // subscribed, unsubscribed, subscriptions, error
	Event   string   `json:"event,omitempty"`   // event slug
	Title   string   `json:"title,omitempty"`   // event title (for subscribe response)
	Events  []string `json:"events,omitempty"`  // list of subscribed events
	Message string   `json:"message,omitempty"` // error message
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
}

// NewWebServer creates a new web server for live monitoring
func NewWebServer(liveManager *LiveTradeManager, port int, db *database.DB, cfg *config.Config) *WebServer {
	ws := &WebServer{
		liveManager: liveManager,
		port:        port,
		db:          db,
		config:      cfg,
		upgrader: websocket.Upgrader{
			CheckOrigin: func(r *http.Request) bool {
				return true // Allow all origins for development
			},
		},
	}

	// Initialize login token repository if db is available
	if db != nil {
		ws.loginTokenRepo = repositories.NewLoginTokenRepository(db)
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

	ws.sendResponse(conn, wsResponse{
		Type:  "subscribed",
		Event: eventSlug,
		Title: eventInfo.Title,
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
