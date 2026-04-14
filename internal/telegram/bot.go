package telegram

import (
	"context"
	"fmt"
	"log"
	"math/big"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/ethereum/go-ethereum/common"
	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"github.com/Catorpilor/poly/internal/blockchain"
	"github.com/Catorpilor/poly/internal/config"
	"github.com/Catorpilor/poly/internal/database"
	"github.com/Catorpilor/poly/internal/database/repositories"
	"github.com/Catorpilor/poly/internal/live"
	"github.com/Catorpilor/poly/internal/polymarket"
	"github.com/Catorpilor/poly/internal/wallet"
)

// Bot represents the Telegram bot
type Bot struct {
	api            *tgbotapi.BotAPI
	config         *config.Config
	db             *database.DB
	userRepo       repositories.UserRepository
	loginTokenRepo repositories.LoginTokenRepository
	handlers       map[string]CommandHandler
	rateLimiter    *RateLimiter
	stateManager   *StateManager
	walletManager  *wallet.Manager
	blockchain     *blockchain.Client
	proxyResolver  *polymarket.ProxyResolver
	tradingClient  *polymarket.TradingClient
	relayerClient  *polymarket.RelayerClient
	liveManager    *live.LiveTradeManager
}

// CommandHandler is a function that handles a command
type CommandHandler func(ctx context.Context, bot *Bot, update *tgbotapi.Update) error

// NewBot creates a new Telegram bot instance
func NewBot(cfg *config.Config, db *database.DB) (*Bot, error) {
	api, err := tgbotapi.NewBotAPIWithClient(
		cfg.Telegram.BotToken,
		tgbotapi.APIEndpoint,
		&http.Client{Timeout: 75 * time.Second},
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create bot API: %w", err)
	}

	// Set debug mode based on environment
	api.Debug = cfg.App.Environment == "development"

	// Create wallet manager
	walletManager, err := wallet.NewManager(cfg.Security.EncryptionKey)
	if err != nil {
		return nil, fmt.Errorf("failed to create wallet manager: %w", err)
	}

	// Create blockchain client
	blockchainClient, err := blockchain.NewClient(&cfg.Blockchain)
	if err != nil {
		log.Printf("Warning: Failed to create blockchain client: %v", err)
		// Don't fail completely, just log the error
		blockchainClient = nil
	}

	// Create proxy resolver
	proxyResolver := polymarket.NewProxyResolver(&cfg.Polymarket)

	// Create trading client
	tradingClient := polymarket.NewTradingClient(cfg.Polymarket.CLOBAPIUrl, cfg.Blockchain.ChainID)

	// Create relayer client (optional — redeem won't work without Builder credentials)
	var relayerClient *polymarket.RelayerClient
	if cfg.Builder.APIKey != "" && cfg.Builder.Secret != "" {
		relayerClient = polymarket.NewRelayerClient(&cfg.Builder, big.NewInt(cfg.Blockchain.ChainID))
		log.Printf("Builder Relayer client initialized (url: %s)", cfg.Builder.RelayerURL)
	} else {
		log.Printf("Warning: Builder credentials not configured — /redeem will be unavailable")
	}

	// Create live trade manager
	liveManager := live.NewLiveTradeManager()

	bot := &Bot{
		api:            api,
		config:         cfg,
		db:             db,
		userRepo:       repositories.NewUserRepository(db),
		loginTokenRepo: repositories.NewLoginTokenRepository(db),
		handlers:       make(map[string]CommandHandler),
		rateLimiter:    NewRateLimiter(cfg.Security.RateLimitPerUser, time.Duration(cfg.Security.RateLimitWindowMins)*time.Minute),
		stateManager:   NewStateManager(),
		walletManager:  walletManager,
		blockchain:     blockchainClient,
		proxyResolver:  proxyResolver,
		tradingClient:  tradingClient,
		relayerClient:  relayerClient,
		liveManager:    liveManager,
	}

	// Set bot as telegram sender for live manager
	liveManager.SetTelegramBot(bot)

	// Start live trade manager
	if err := liveManager.Start(); err != nil {
		log.Printf("Warning: Failed to start live trade manager: %v", err)
		// Don't fail completely, just log the error
	}

	// Register command handlers
	bot.registerHandlers()

	log.Printf("Authorized on account %s", api.Self.UserName)

	return bot, nil
}

// registerHandlers registers all command handlers
func (b *Bot) registerHandlers() {
	b.handlers["/start"] = b.handleStart
	b.handlers["/wallet"] = b.handleWallet
	b.handlers["/import"] = b.handleImport
	b.handlers["/export"] = b.handleExport
	b.handlers["/markets"] = b.handleMarkets
	b.handlers["/market"] = b.handleMarket
	b.handlers["/buy"] = b.handleBuy
	b.handlers["/sell"] = b.handleSell
	b.handlers["/orders"] = b.handleOrders
	b.handlers["/cancel"] = b.handleCancel
	b.handlers["/positions"] = b.handlePositions
	b.handlers["/pnl"] = b.handlePNL
	b.handlers["/history"] = b.handleHistory
	b.handlers["/settings"] = b.handleSettings
	b.handlers["/alerts"] = b.handleAlerts
	b.handlers["/gas"] = b.handleGas
	b.handlers["/help"] = b.handleHelp
	b.handlers["/refresh"] = b.handleRefresh
	b.handlers["/redeem"] = b.handleRedeem
	b.handlers["/event"] = b.handleEvent
	// Live monitoring commands
	b.handlers["/live"] = b.handleLive
	b.handlers["/stoplive"] = b.handleStopLive
	b.handlers["/subs"] = b.handleSubs
}

// Start starts the bot and begins listening for updates
func (b *Bot) Start(ctx context.Context) error {
	u := tgbotapi.NewUpdate(0)
	u.Timeout = 60

	updates := b.api.GetUpdatesChan(u)

	// Set bot commands for the menu
	commands := []tgbotapi.BotCommand{
		{Command: "start", Description: "Initialize bot and create/import wallet"},
		{Command: "wallet", Description: "Show wallet addresses and balances"},
		{Command: "import", Description: "Import existing wallet"},
		{Command: "markets", Description: "List active markets"},
		{Command: "market", Description: "Show market details"},
		{Command: "buy", Description: "Buy YES or NO tokens"},
		{Command: "sell", Description: "Sell YES or NO tokens"},
		{Command: "orders", Description: "Show open orders"},
		{Command: "positions", Description: "Show all positions"},
		{Command: "pnl", Description: "Calculate unrealized P&L"},
		{Command: "redeem", Description: "Claim all resolved positions"},
		{Command: "help", Description: "Show help message"},
	}

	cmdConfig := tgbotapi.NewSetMyCommands(commands...)
	_, err := b.api.Request(cmdConfig)
	if err != nil {
		log.Printf("Failed to set bot commands: %v", err)
	}

	log.Println("Bot started successfully. Listening for updates...")

	for {
		select {
		case <-ctx.Done():
			log.Println("Bot stopping due to context cancellation")
			return ctx.Err()

		case update := <-updates:
			go b.handleUpdate(ctx, update)
		}
	}
}

// handleUpdate handles incoming updates
func (b *Bot) handleUpdate(ctx context.Context, update tgbotapi.Update) {
	// Handle callback queries (inline keyboard buttons)
	if update.CallbackQuery != nil {
		b.handleCallbackQuery(ctx, &update)
		return
	}

	// Handle only messages for now
	if update.Message == nil {
		return
	}

	// Check rate limiting
	userID := update.Message.From.ID
	if !b.rateLimiter.Allow(userID) {
		b.sendMessage(update.Message.Chat.ID,
			"⚠️ Rate limit exceeded. Please wait a moment before sending more commands.")
		return
	}

	// Log the received message
	log.Printf("[%s] %s", update.Message.From.UserName, update.Message.Text)

	// Handle deep links first (start with parameters) - must check before IsCommand()
	// because /start with params is still detected as a command
	if strings.HasPrefix(update.Message.Text, "/start ") {
		b.handleDeepLink(ctx, &update)
		return
	}

	// Check if it's a command
	if update.Message.IsCommand() {
		b.handleCommand(ctx, update)
		return
	}

	// Handle regular text messages (could be responses to prompts)
	b.handleTextMessage(ctx, &update)
}

// handleCommand routes commands to their handlers
func (b *Bot) handleCommand(ctx context.Context, update tgbotapi.Update) {
	command := update.Message.Command()
	handler, exists := b.handlers["/"+command]

	if !exists {
		b.sendMessage(update.Message.Chat.ID,
			"❓ Unknown command. Type /help to see available commands.")
		return
	}

	// Execute the handler
	if err := handler(ctx, b, &update); err != nil {
		log.Printf("Error handling command /%s: %v", command, err)
		b.sendMessage(update.Message.Chat.ID,
			fmt.Sprintf("❌ Error executing command: %v", err))
	}
}

// handleDeepLink handles deep links with start parameters
// Supported formats:
//   - /start m_<marketID> - View market details by market ID
//   - /start s_<slug> - View market by slug (RECOMMENDED for copy trading)
//   - /start (no param) - Normal start
//
// To generate a deep link for copy trading, use the market slug:
//
//	slug := "us-operation-to-capture-maduro-in-2025"
//	link := fmt.Sprintf("https://t.me/bot?start=s_%s", slug)
//
// Note: conditionId lookups are not supported by the Gamma API.
// Always use slug from the workflow data for reliable market lookups.
func (b *Bot) handleDeepLink(ctx context.Context, update *tgbotapi.Update) {
	// Extract the parameter from "/start parameter"
	parts := strings.SplitN(update.Message.Text, " ", 2)
	if len(parts) != 2 {
		b.handleStart(ctx, b, update)
		return
	}

	parameter := parts[1]
	log.Printf("Received deep link with parameter: %s (length: %d)", parameter, len(parameter))

	// Handle market deep links: m_<marketID>
	if strings.HasPrefix(parameter, "m_") {
		marketID := strings.TrimPrefix(parameter, "m_")
		b.handleMarketByID(ctx, update, marketID)
		return
	}

	// Handle slug deep links: s_<slug> (RECOMMENDED for copy trading)
	// Slugs are URL-safe and the Gamma API reliably supports slug lookups
	if strings.HasPrefix(parameter, "s_") {
		slug := strings.TrimPrefix(parameter, "s_")
		b.handleMarketBySlug(ctx, update, slug)
		return
	}

	// Handle login deep links: login_<token>
	// Used for web authentication via Telegram
	if strings.HasPrefix(parameter, "login_") {
		token := strings.TrimPrefix(parameter, "login_")
		b.handleLoginToken(ctx, update, token)
		return
	}

	// Unknown parameter, just start normally
	b.handleStart(ctx, b, update)
}

// handleMarketBySlug fetches and displays market by slug (used for copy trading)
func (b *Bot) handleMarketBySlug(ctx context.Context, update *tgbotapi.Update, slug string) {
	chatID := update.Message.Chat.ID

	// Fetch market details from Gamma API using slug
	marketClient := polymarket.NewMarketClient()
	market, err := marketClient.GetMarketBySlug(ctx, slug)
	if err != nil {
		b.sendMessage(chatID, fmt.Sprintf("❌ Market not found for slug: %s", slug))
		return
	}

	// Reuse the same display logic
	b.displayMarketForTrading(ctx, chatID, market)
}

// handleLoginToken handles web authentication via Telegram
// When user clicks "Login with Telegram" on web, they're redirected here
func (b *Bot) handleLoginToken(ctx context.Context, update *tgbotapi.Update, token string) {
	chatID := update.Message.Chat.ID
	userID := update.Message.From.ID
	username := update.Message.From.UserName

	log.Printf("Login token received from user %d: %s", userID, token)

	// Validate token format (should be UUID, 36 chars)
	if len(token) != 36 {
		b.sendMessage(chatID, "❌ Invalid login token format.")
		return
	}

	// Check if token exists and is valid
	loginToken, err := b.loginTokenRepo.GetByToken(ctx, token)
	if err != nil {
		log.Printf("Error getting login token: %v", err)
		b.sendMessage(chatID, "❌ Invalid login token.")
		return
	}

	if loginToken == nil {
		b.sendMessage(chatID, "❌ Login token not found or expired.")
		return
	}

	// Check if token is still pending
	if loginToken.Status != database.LoginTokenStatusPending {
		b.sendMessage(chatID, "❌ This login token has already been used.")
		return
	}

	// Check if token has expired
	if time.Now().After(loginToken.ExpiresAt) {
		b.sendMessage(chatID, "❌ This login token has expired. Please try again.")
		return
	}

	// Get or create user
	user, err := b.userRepo.GetByTelegramID(ctx, userID)
	if err != nil {
		log.Printf("Error getting user: %v", err)
		b.sendMessage(chatID, "❌ Failed to process login. Please try again.")
		return
	}

	// If user doesn't exist, create wallet
	if user == nil {
		log.Printf("Creating new wallet for login user %d", userID)

		// Generate new wallet
		newWallet, err := b.walletManager.GenerateNewWallet()
		if err != nil {
			log.Printf("Error generating wallet: %v", err)
			b.sendMessage(chatID, "❌ Failed to create wallet. Please try again.")
			return
		}

		// Encrypt the private key
		encryptedKey, err := b.walletManager.EncryptPrivateKey(newWallet)
		if err != nil {
			log.Printf("Error encrypting private key: %v", err)
			b.sendMessage(chatID, "❌ Failed to secure wallet. Please try again.")
			return
		}

		// Resolve proxy address
		proxyAddress := ""
		if b.proxyResolver != nil {
			eoaAddress := newWallet.EOAAddress
			if proxy, err := b.proxyResolver.GetProxyWallet(ctx, eoaAddress); err == nil && proxy.Hex() != "0x0000000000000000000000000000000000000000" {
				proxyAddress = proxy.Hex()
				log.Printf("Found proxy wallet for new user: %s", proxyAddress)
			}
		}

		// Create user in database
		user = &database.User{
			TelegramID:   userID,
			Username:     username,
			EOAAddress:   newWallet.EOAAddress.Hex(),
			ProxyAddress: proxyAddress,
			EncryptedKey: encryptedKey,
			Settings:     make(database.JSONB),
			IsActive:     true,
		}

		if err := b.userRepo.Create(ctx, user); err != nil {
			log.Printf("Error creating user: %v", err)
			b.sendMessage(chatID, "❌ Failed to create account. Please try again.")
			return
		}

		log.Printf("Created new user %d with wallet %s", userID, newWallet.EOAAddress.Hex())
	}

	// Authenticate the token
	walletAddr := user.EOAAddress
	proxyAddr := user.ProxyAddress
	if err := b.loginTokenRepo.Authenticate(ctx, token, userID, walletAddr, proxyAddr); err != nil {
		log.Printf("Error authenticating token: %v", err)
		b.sendMessage(chatID, "❌ Failed to authenticate. Please try again.")
		return
	}

	// Build callback URL
	callbackURL := fmt.Sprintf("%s?token=%s", b.config.App.LiveWebURL, token)

	// Send success message
	// Note: Telegram doesn't allow localhost URLs in inline buttons, so we only show
	// the button for https URLs. For localhost, the web page will auto-detect the login.
	message := fmt.Sprintf(`✅ *Login Successful!*

You are now authenticated as *%s*.

Your wallet:
%s

`, username, walletAddr)

	// Only show URL button for https URLs (Telegram rejects http/localhost)
	if strings.HasPrefix(b.config.App.LiveWebURL, "https://") {
		message += "Click the button below to return to the web interface."
		keyboard := tgbotapi.NewInlineKeyboardMarkup(
			tgbotapi.NewInlineKeyboardRow(
				tgbotapi.NewInlineKeyboardButtonURL("🌐 Open Live Web", callbackURL),
			),
		)
		b.sendMessageWithKeyboard(chatID, message, keyboard)
	} else {
		message += "Return to your browser - the page will automatically log you in."
		b.sendMessage(chatID, message)
	}

	log.Printf("User %d authenticated successfully for web login", userID)
}

// displayMarketForTrading shows market details with trading buttons
func (b *Bot) displayMarketForTrading(ctx context.Context, chatID int64, market *polymarket.GammaMarket) {
	// Get outcomes and prices - they should be in the same order
	outcomes := market.GetOutcomes()
	prices := market.GetOutcomePrices()

	log.Printf("displayMarketForTrading: market=%s, outcomes=%v, prices=%v", market.ID, outcomes, prices)

	// Determine outcome labels and prices
	// outcomes[i] corresponds to prices[i] and clobTokenIds[i]
	outcome0Label := "Yes"
	outcome1Label := "No"
	if len(outcomes) >= 2 {
		// Use actual outcome names if not "Yes"/"No"
		o0 := strings.ToLower(outcomes[0])
		o1 := strings.ToLower(outcomes[1])
		if o0 != "yes" && o0 != "no" {
			outcome0Label = outcomes[0]
		}
		if o1 != "yes" && o1 != "no" {
			outcome1Label = outcomes[1]
		}
	}

	// Get prices - outcomes[0] has prices[0], outcomes[1] has prices[1]
	price0, price1 := 0.0, 0.0
	if len(prices) >= 2 {
		fmt.Sscanf(prices[0], "%f", &price0)
		fmt.Sscanf(prices[1], "%f", &price1)
	}

	// Format end date
	endDate := market.EndDate
	if len(endDate) > 10 {
		endDate = endDate[:10]
	}

	// Build Polymarket URL using event slug (not market slug)
	polymarketURL := fmt.Sprintf("https://polymarket.com/event/%s", market.GetEventSlug())

	message := fmt.Sprintf(`📈 *%s*

*Current Prices:*
   %s: %s ($%.2f)
   %s: %s ($%.2f)

*Market Stats:*
   24h Volume: %s
   Total Volume: %s
   Liquidity: %s

*Status:* %s
*End Date:* %s

🔗 [View on Polymarket](%s)
`,
		market.Question,
		outcome0Label, polymarket.FormatPrice(price0), price0,
		outcome1Label, polymarket.FormatPrice(price1), price1,
		polymarket.FormatVolume(market.Volume24hr),
		polymarket.FormatVolume(market.Volume),
		polymarket.FormatVolume(market.Liquidity),
		getMarketStatusFromBot(market),
		endDate,
		polymarketURL,
	)

	// Create buy buttons using index 0 and 1 instead of yes/no
	// This ensures the button matches the displayed price
	keyboard := tgbotapi.NewInlineKeyboardMarkup(
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonURL("🌐 Open on Polymarket", polymarketURL),
		),
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData(fmt.Sprintf("📈 Buy %s", outcome0Label), fmt.Sprintf("buy:0:%s", market.ID)),
			tgbotapi.NewInlineKeyboardButtonData(fmt.Sprintf("📉 Buy %s", outcome1Label), fmt.Sprintf("buy:1:%s", market.ID)),
		),
	)

	b.sendMessageWithKeyboard(chatID, message, keyboard)
}

// handleMarketByID fetches and displays market by ID (used by deep links)
func (b *Bot) handleMarketByID(ctx context.Context, update *tgbotapi.Update, marketID string) {
	chatID := update.Message.Chat.ID

	// Fetch market details from Gamma API
	marketClient := polymarket.NewMarketClient()
	market, err := marketClient.GetMarketByID(ctx, marketID)
	if err != nil {
		b.sendMessage(chatID, fmt.Sprintf("❌ Market not found: %s", marketID))
		return
	}

	// Reuse the shared display logic
	b.displayMarketForTrading(ctx, chatID, market)
}

// handleEventBySlug fetches an event by slug and displays all its markets
func (b *Bot) handleEventBySlug(ctx context.Context, chatID int64, slug string) {
	marketClient := polymarket.NewMarketClient()
	event, err := marketClient.GetEventBySlug(ctx, slug)
	if err != nil {
		b.sendMessage(chatID, fmt.Sprintf("❌ Event not found: %s", slug))
		return
	}

	if len(event.Markets) == 0 {
		b.sendMessage(chatID, fmt.Sprintf("❌ No markets found for event: %s", event.Title))
		return
	}

	// Build the message with all markets
	message := fmt.Sprintf("🏆 *%s*\n\n📊 *Available Markets:*\n", event.Title)

	var rows [][]tgbotapi.InlineKeyboardButton
	for i, m := range event.Markets {
		outcomes := m.GetOutcomes()
		prices := m.GetOutcomePrices()

		// Parse prices
		price0, price1 := 0.0, 0.0
		if len(prices) >= 2 {
			fmt.Sscanf(prices[0], "%f", &price0)
			fmt.Sscanf(prices[1], "%f", &price1)
		}

		// Determine outcome labels
		o0Label, o1Label := "Yes", "No"
		if len(outcomes) >= 2 {
			if strings.ToLower(outcomes[0]) != "yes" && strings.ToLower(outcomes[0]) != "no" {
				o0Label = outcomes[0]
			}
			if strings.ToLower(outcomes[1]) != "yes" && strings.ToLower(outcomes[1]) != "no" {
				o1Label = outcomes[1]
			}
		}

		// Market status indicator
		status := ""
		if m.Closed {
			status = " (Closed)"
		} else if !m.AcceptingOrders {
			status = " (Paused)"
		}

		message += fmt.Sprintf("\n*%d.* %s%s\n   %s: %s | %s: %s | Vol: %s\n",
			i+1, m.Question, status,
			o0Label, polymarket.FormatPrice(price0),
			o1Label, polymarket.FormatPrice(price1),
			polymarket.FormatVolume(m.Volume24hr),
		)

		// Only add button for active markets
		if !m.Closed && m.AcceptingOrders {
			btnText := truncateUTF8(m.Question, 40)
			rows = append(rows, tgbotapi.NewInlineKeyboardRow(
				tgbotapi.NewInlineKeyboardButtonData(
					fmt.Sprintf("📈 %s", btnText),
					fmt.Sprintf("mkt:%s", m.ID),
				),
			))
		}
	}

	message += "\n_Tap a market to view details & trade_"

	if len(rows) > 0 {
		keyboard := tgbotapi.NewInlineKeyboardMarkup(rows...)
		b.sendMessageWithKeyboard(chatID, message, keyboard)
	} else {
		b.sendMessage(chatID, message)
	}
}

// handleMarketDetailCallback handles the mkt:<marketID> callback from event listings
func (b *Bot) handleMarketDetailCallback(ctx context.Context, update *tgbotapi.Update) {
	chatID := update.CallbackQuery.Message.Chat.ID
	data := update.CallbackQuery.Data
	marketID := strings.TrimPrefix(data, "mkt:")

	marketClient := polymarket.NewMarketClient()
	market, err := marketClient.GetMarketByID(ctx, marketID)
	if err != nil {
		b.sendMessage(chatID, fmt.Sprintf("❌ Market not found: %s", marketID))
		return
	}

	b.displayMarketForTrading(ctx, chatID, market)
}

// getMarketStatusFromBot returns market status (duplicate to avoid import cycle)
func getMarketStatusFromBot(market *polymarket.GammaMarket) string {
	if market.Closed {
		return "Closed"
	}
	if !market.AcceptingOrders {
		return "Not accepting orders"
	}
	return "Active"
}

// handleTextMessage handles regular text messages
func (b *Bot) handleTextMessage(ctx context.Context, update *tgbotapi.Update) {
	userID := update.Message.From.ID
	chatID := update.Message.Chat.ID

	// Auto-detect Polymarket URLs and show event markets
	if slug, ok := polymarket.ParseEventSlug(strings.TrimSpace(update.Message.Text)); ok {
		b.handleEventBySlug(ctx, chatID, slug)
		return
	}

	// Check if user is in a specific state
	userCtx, exists := b.stateManager.GetState(userID)
	if !exists {
		// No state, just inform user to use commands
		b.sendMessage(chatID,
			"💬 Please use commands to interact with the bot. Type /help for available commands.")
		return
	}

	// Handle based on state
	switch userCtx.State {
	case StateWaitingForKey:
		// User is sending their private key
		b.handlePrivateKeyInput(ctx, update)

	case StateWaitingForAmount:
		// User is entering custom amount
		b.handleCustomAmountInput(ctx, update, userCtx)

	case StateWaitingForLimitPrice:
		// User is entering limit price for sell order
		b.handleLimitPriceInput(ctx, update, userCtx)

	case StateWaitingForBuyLimitPrice:
		// User is entering limit price for buy order
		b.handleBuyLimitPriceInput(ctx, update, userCtx)

	default:
		b.sendMessage(chatID,
			"💬 I received your message. Please use commands to interact with the bot. Type /help for available commands.")
	}
}

// handleCustomAmountInput handles when user enters a custom amount
func (b *Bot) handleCustomAmountInput(ctx context.Context, update *tgbotapi.Update, userCtx *UserContext) {
	chatID := update.Message.Chat.ID
	userID := update.Message.From.ID
	text := strings.TrimSpace(update.Message.Text)

	// Clear state
	b.stateManager.ClearState(userID)

	// Check for cancel
	if strings.ToLower(text) == "/cancel" {
		b.sendMessage(chatID, "❌ Order cancelled.")
		return
	}

	// Parse amount
	var amount float64
	text = strings.TrimPrefix(text, "$")
	if _, err := fmt.Sscanf(text, "%f", &amount); err != nil || amount <= 0 {
		b.sendMessage(chatID, "❌ Invalid amount. Please enter a positive number (e.g., 75)")
		return
	}

	// Get order context from state - now uses outcome_index instead of outcome
	outcomeIndexStr, _ := userCtx.Data["outcome_index"].(string)
	marketID, _ := userCtx.Data["market_id"].(string)

	if outcomeIndexStr == "" || marketID == "" {
		b.sendMessage(chatID, "❌ Order context lost. Please start again with /market")
		return
	}

	// Parse outcome index
	outcomeIndex, err := strconv.Atoi(outcomeIndexStr)
	if err != nil || (outcomeIndex != 0 && outcomeIndex != 1) {
		b.sendMessage(chatID, "❌ Invalid outcome. Please start again with /market")
		return
	}

	// Check if user has wallet
	user, err := b.userRepo.GetByTelegramID(ctx, userID)
	if err != nil || user == nil {
		b.sendMessage(chatID, "❌ You need to set up a wallet first. Use /start")
		return
	}

	// Fetch market info for display
	marketClient := polymarket.NewMarketClient()
	market, err := marketClient.GetMarketByID(ctx, marketID)
	if err != nil {
		b.sendMessage(chatID, fmt.Sprintf("❌ Failed to fetch market: %v", err))
		return
	}

	marketName := market.Question
	marketName = truncateUTF8(marketName, 40)
	// Get actual outcome name
	outcomes := market.GetOutcomes()
	outcomeName := "Unknown"
	if outcomeIndex < len(outcomes) {
		outcomeName = outcomes[outcomeIndex]
	}

	// Get the REAL orderbook price (not the stale outcomePrices)
	var realPrice float64
	var priceWarning string

	tokenID, err := b.tradingClient.GetTokenIDByIndex(ctx, marketID, outcomeIndex)
	if err != nil {
		log.Printf("Failed to get tokenID for price check: %v", err)
		// Fallback to outcomePrices if we can't get orderbook
		prices := market.GetOutcomePrices()
		if outcomeIndex < len(prices) {
			fmt.Sscanf(prices[outcomeIndex], "%f", &realPrice)
		}
		priceWarning = "\n⚠️ _Price is indicative, actual may vary_"
	} else {
		// Fetch actual orderbook price for this amount
		realPrice, err = b.tradingClient.GetBestPrice(ctx, tokenID, "BUY", amount)
		if err != nil {
			log.Printf("Failed to get orderbook price: %v", err)
			// Fallback to outcomePrices
			prices := market.GetOutcomePrices()
			if outcomeIndex < len(prices) {
				fmt.Sscanf(prices[outcomeIndex], "%f", &realPrice)
			}
			priceWarning = "\n⚠️ _Price is indicative, actual may vary_"
		} else {
			// Check if price differs significantly from displayed price
			prices := market.GetOutcomePrices()
			displayedPrice := 0.0
			if outcomeIndex < len(prices) {
				fmt.Sscanf(prices[outcomeIndex], "%f", &displayedPrice)
			}
			if displayedPrice > 0 && realPrice > displayedPrice*1.1 {
				// Price is >10% higher than displayed
				priceWarning = fmt.Sprintf("\n⚠️ _Orderbook price ($%.2f) is higher than displayed ($%.2f)!_", realPrice, displayedPrice)
			}
		}
	}

	// Calculate estimated shares
	estimatedShares := 0.0
	if realPrice > 0 {
		estimatedShares = amount / realPrice
	}

	// Show order type selection with REAL price
	message := fmt.Sprintf(`🎯 *Buy Order*

*Market:* %s
*Outcome:* %s
*Amount:* $%.2f
*Est. Price:* $%.2f (%.0f%%)
*Est. Shares:* %.2f%s

───────────────

📊 *Select order type:*
`, marketName, outcomeName, amount, realPrice, realPrice*100, estimatedShares, priceWarning)

	// Create order type selection keyboard
	keyboard := tgbotapi.NewInlineKeyboardMarkup(
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("⚡ Market Order", fmt.Sprintf("buyexec:%.0f:%d:%s:market", amount, outcomeIndex, marketID)),
			tgbotapi.NewInlineKeyboardButtonData("📝 Limit Order", fmt.Sprintf("buylimit:%.0f:%d:%s", amount, outcomeIndex, marketID)),
		),
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("⬅️ Back", fmt.Sprintf("buy:%d:%s", outcomeIndex, marketID)),
			tgbotapi.NewInlineKeyboardButtonData("❌ Cancel", "cancel_order"),
		),
	)

	b.sendMessageWithKeyboard(chatID, message, keyboard)
}

// sendMessage sends a message to a chat
func (b *Bot) sendMessage(chatID int64, text string) {
	msg := tgbotapi.NewMessage(chatID, text)
	msg.ParseMode = "Markdown"
	msg.DisableWebPagePreview = true

	if _, err := b.api.Send(msg); err != nil {
		log.Printf("Error sending message: %v", err)
	}
}

// SendMessage is a public wrapper for sendMessage to satisfy TelegramSender interface
func (b *Bot) SendMessage(chatID int64, text string) {
	b.sendMessage(chatID, text)
}

// GetLiveManager returns the live trade manager
func (b *Bot) GetLiveManager() *live.LiveTradeManager {
	return b.liveManager
}

// GetLoginTokenRepo returns the login token repository
func (b *Bot) GetLoginTokenRepo() repositories.LoginTokenRepository {
	return b.loginTokenRepo
}

// GetWalletManager returns the wallet manager
func (b *Bot) GetWalletManager() *wallet.Manager {
	return b.walletManager
}

// GetTradingClient returns the trading client
func (b *Bot) GetTradingClient() *polymarket.TradingClient {
	return b.tradingClient
}

// sendMessageAndReturn sends a message and returns the sent message
func (b *Bot) sendMessageAndReturn(chatID int64, text string) tgbotapi.Message {
	msg := tgbotapi.NewMessage(chatID, text)
	msg.ParseMode = "Markdown"
	msg.DisableWebPagePreview = true

	sent, err := b.api.Send(msg)
	if err != nil {
		log.Printf("Error sending message: %v", err)
		return tgbotapi.Message{}
	}
	return sent
}

// sendMessageWithKeyboard sends a message with an inline keyboard
func (b *Bot) sendMessageWithKeyboard(chatID int64, text string, keyboard tgbotapi.InlineKeyboardMarkup) {
	msg := tgbotapi.NewMessage(chatID, text)
	msg.ParseMode = "Markdown"
	msg.DisableWebPagePreview = true
	msg.ReplyMarkup = keyboard

	if _, err := b.api.Send(msg); err != nil {
		log.Printf("Error sending message with keyboard: %v", err)
	}
}

// deleteMessage deletes a message (useful for sensitive data)
func (b *Bot) deleteMessage(chatID int64, messageID int) {
	deleteConfig := tgbotapi.NewDeleteMessage(chatID, messageID)
	if _, err := b.api.Request(deleteConfig); err != nil {
		log.Printf("Error deleting message: %v", err)
	}
}

// handleCallbackQuery handles inline keyboard button callbacks
func (b *Bot) handleCallbackQuery(ctx context.Context, update *tgbotapi.Update) {
	// Answer the callback query to remove the loading state
	callback := tgbotapi.NewCallback(update.CallbackQuery.ID, "")
	if _, err := b.api.Request(callback); err != nil {
		log.Printf("Error answering callback: %v", err)
	}

	data := update.CallbackQuery.Data
	chatID := update.CallbackQuery.Message.Chat.ID

	// Handle based on callback data
	switch {
	case data == "create_wallet":
		b.handleCreateWallet(ctx, update)

	case data == "import_wallet":
		b.handleImportWalletCallback(ctx, update)

	case strings.HasPrefix(data, "buy:"):
		b.handleBuyCallback(ctx, update)

	case strings.HasPrefix(data, "amt:"):
		b.handleAmountCallback(ctx, update)

	case strings.HasPrefix(data, "cust:"):
		b.handleCustomAmountCallback(ctx, update)

	case strings.HasPrefix(data, "buyexec:"):
		b.handleBuyExecuteCallback(ctx, update)

	case strings.HasPrefix(data, "buylimit:"):
		b.handleBuyLimitCallback(ctx, update)

	case data == "cancel_order":
		b.editMessage(chatID, update.CallbackQuery.Message.MessageID, "❌ Order cancelled.")

	case data == "refresh_positions":
		b.handleRefreshPositions(ctx, update)

	case data == "sell_positions":
		b.handleSellPositions(ctx, update)

	case strings.HasPrefix(data, "sellpos:"):
		b.handleSellPositionDetail(ctx, update)

	case strings.HasPrefix(data, "sellqty:"):
		b.handleSellQuantityCallback(ctx, update)

	case strings.HasPrefix(data, "sell:"):
		b.handleSellAmountCallback(ctx, update)

	case data == "back_to_positions":
		b.handleRefreshPositions(ctx, update)

	case data == "refresh_orders":
		b.handleRefreshOrders(ctx, update)

	case data == "cancel_all_orders":
		b.handleCancelAllOrders(ctx, update)

	case data == "redeem_positions":
		b.handleRedeemPositions(ctx, update)

	case data == "redeem_all":
		b.handleRedeemAll(ctx, update)

	case strings.HasPrefix(data, "mkt:"):
		b.handleMarketDetailCallback(ctx, update)

	default:
		log.Printf("Unknown callback data: %s", data)
	}
}

// handleImportWalletCallback handles the import wallet button
func (b *Bot) handleImportWalletCallback(ctx context.Context, update *tgbotapi.Update) {
	userID := update.CallbackQuery.From.ID
	chatID := update.CallbackQuery.Message.Chat.ID

	// Check if user already has a wallet
	existingUser, err := b.userRepo.GetByTelegramID(ctx, userID)
	if err != nil {
		b.sendMessage(chatID, "❌ Failed to check existing wallet. Please try again.")
		return
	}

	if existingUser != nil {
		b.sendMessage(chatID, "⚠️ You already have a wallet set up.")
		return
	}

	message := `
🔐 *Import Your Wallet*

⚠️ *IMPORTANT SECURITY NOTICE:*
• Send your private key in the next message
• The message will be deleted immediately
• Your key will be encrypted before storage
• Never share your private key with anyone else

Please send your private key now (it should start with 0x or be 64 characters long).

_Type /cancel to abort the import process._
`
	b.sendMessage(chatID, message)
	b.stateManager.SetState(userID, StateWaitingForKey, nil, 5*time.Minute)
}

// handleBuyCallback handles Buy button clicks - shows amount selection
// Callback format: "buy:INDEX:marketID" where INDEX is 0 or 1
func (b *Bot) handleBuyCallback(ctx context.Context, update *tgbotapi.Update) {
	chatID := update.CallbackQuery.Message.Chat.ID
	messageID := update.CallbackQuery.Message.MessageID

	// Parse callback data: "buy:0:id" or "buy:1:id" (index-based)
	parts := strings.Split(update.CallbackQuery.Data, ":")
	if len(parts) != 3 {
		return
	}
	outcomeIndex := parts[1] // "0" or "1"
	marketID := parts[2]

	// Fetch market info to show in the order UI
	marketClient := polymarket.NewMarketClient()
	market, err := marketClient.GetMarketByID(ctx, marketID)
	if err != nil {
		b.sendMessage(chatID, "❌ Failed to fetch market info.")
		return
	}

	// Get the actual outcome name based on index
	outcomes := market.GetOutcomes()
	outcomeName := "Unknown"
	if outcomeIndex == "0" && len(outcomes) > 0 {
		outcomeName = outcomes[0]
	} else if outcomeIndex == "1" && len(outcomes) > 1 {
		outcomeName = outcomes[1]
	}

	log.Printf("handleBuyCallback: market=%s, outcomeIndex=%s, outcomeName=%s, outcomes=%v",
		marketID, outcomeIndex, outcomeName, outcomes)

	// Truncate question if too long
	question := market.Question
	question = truncateUTF8(question, 50)

	message := fmt.Sprintf(`🎯 *Buy Order*

*Market:* %s
*Outcome:* %s

───────────────

💰 *Select amount or enter custom amount:*
`, question, outcomeName)

	// Create amount selection keyboard - pass index for consistent lookup
	keyboard := tgbotapi.NewInlineKeyboardMarkup(
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("💵 $10", fmt.Sprintf("amt:10:%s:%s", outcomeIndex, marketID)),
			tgbotapi.NewInlineKeyboardButtonData("💰 $25", fmt.Sprintf("amt:25:%s:%s", outcomeIndex, marketID)),
			tgbotapi.NewInlineKeyboardButtonData("💎 $50", fmt.Sprintf("amt:50:%s:%s", outcomeIndex, marketID)),
		),
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("🚀 $100", fmt.Sprintf("amt:100:%s:%s", outcomeIndex, marketID)),
			tgbotapi.NewInlineKeyboardButtonData("🌟 $250", fmt.Sprintf("amt:250:%s:%s", outcomeIndex, marketID)),
		),
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("✏️ Custom Amount", fmt.Sprintf("cust:%s:%s", outcomeIndex, marketID)),
		),
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("❌ Cancel", "cancel_order"),
		),
	)

	b.editMessageWithKeyboard(chatID, messageID, message, keyboard)
}

// handleAmountCallback handles amount selection - shows order type choice (market vs limit)
func (b *Bot) handleAmountCallback(ctx context.Context, update *tgbotapi.Update) {
	chatID := update.CallbackQuery.Message.Chat.ID
	messageID := update.CallbackQuery.Message.MessageID

	// Parse callback data: "amt:100:INDEX:marketID" where INDEX is 0 or 1
	parts := strings.Split(update.CallbackQuery.Data, ":")
	if len(parts) != 4 {
		return
	}
	amountStr := parts[1]
	outcomeIndex := parts[2] // "0" or "1"
	marketID := parts[3]

	// Parse amount
	amount, err := strconv.ParseFloat(amountStr, 64)
	if err != nil {
		b.editMessage(chatID, messageID, "❌ Invalid amount")
		return
	}

	// Parse outcome index
	idx, err := strconv.Atoi(outcomeIndex)
	if err != nil || (idx != 0 && idx != 1) {
		b.editMessage(chatID, messageID, "❌ Invalid outcome")
		return
	}

	// Fetch market info
	marketClient := polymarket.NewMarketClient()
	market, err := marketClient.GetMarketByID(ctx, marketID)
	if err != nil {
		b.editMessage(chatID, messageID, fmt.Sprintf("❌ Failed to fetch market: %v", err))
		return
	}

	// Get outcome name
	outcomes := market.GetOutcomes()
	outcomeName := "Unknown"
	if idx < len(outcomes) {
		outcomeName = outcomes[idx]
	}

	marketName := market.Question
	marketName = truncateUTF8(marketName, 40)

	// Get the REAL orderbook price (not the stale outcomePrices)
	var realPrice float64
	var priceWarning string

	tokenID, err := b.tradingClient.GetTokenIDByIndex(ctx, marketID, idx)
	if err != nil {
		log.Printf("Failed to get tokenID for price check: %v", err)
		// Fallback to outcomePrices if we can't get orderbook
		prices := market.GetOutcomePrices()
		if idx < len(prices) {
			fmt.Sscanf(prices[idx], "%f", &realPrice)
		}
		priceWarning = "\n⚠️ _Price is indicative, actual may vary_"
	} else {
		// Fetch actual orderbook price for this amount
		realPrice, err = b.tradingClient.GetBestPrice(ctx, tokenID, "BUY", amount)
		if err != nil {
			log.Printf("Failed to get orderbook price: %v", err)
			// Fallback to outcomePrices
			prices := market.GetOutcomePrices()
			if idx < len(prices) {
				fmt.Sscanf(prices[idx], "%f", &realPrice)
			}
			priceWarning = "\n⚠️ _Price is indicative, actual may vary_"
		} else {
			// Check if price differs significantly from displayed price
			prices := market.GetOutcomePrices()
			displayedPrice := 0.0
			if idx < len(prices) {
				fmt.Sscanf(prices[idx], "%f", &displayedPrice)
			}
			if displayedPrice > 0 && realPrice > displayedPrice*1.1 {
				// Price is >10% higher than displayed
				priceWarning = fmt.Sprintf("\n⚠️ _Orderbook price ($%.2f) is higher than displayed ($%.2f)!_", realPrice, displayedPrice)
			}
		}
	}

	log.Printf("handleAmountCallback: market=%s, outcomeIndex=%d, outcomeName=%s, realPrice=%.4f",
		marketID, idx, outcomeName, realPrice)

	// Calculate estimated shares
	estimatedShares := 0.0
	if realPrice > 0 {
		estimatedShares = amount / realPrice
	}

	// Show order type selection with REAL price
	message := fmt.Sprintf(`🎯 *Buy Order*

*Market:* %s
*Outcome:* %s
*Amount:* $%.2f
*Est. Price:* $%.2f (%.0f%%)
*Est. Shares:* %.2f%s

───────────────

📊 *Select order type:*
`, marketName, outcomeName, amount, realPrice, realPrice*100, estimatedShares, priceWarning)

	// Create order type selection keyboard
	keyboard := tgbotapi.NewInlineKeyboardMarkup(
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("⚡ Market Order", fmt.Sprintf("buyexec:%.0f:%d:%s:market", amount, idx, marketID)),
			tgbotapi.NewInlineKeyboardButtonData("📝 Limit Order", fmt.Sprintf("buylimit:%.0f:%d:%s", amount, idx, marketID)),
		),
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("⬅️ Back", fmt.Sprintf("buy:%d:%s", idx, marketID)),
			tgbotapi.NewInlineKeyboardButtonData("❌ Cancel", "cancel_order"),
		),
	)

	b.editMessageWithKeyboard(chatID, messageID, message, keyboard)
}

// handleBuyExecuteCallback executes a market buy order
func (b *Bot) handleBuyExecuteCallback(ctx context.Context, update *tgbotapi.Update) {
	chatID := update.CallbackQuery.Message.Chat.ID
	messageID := update.CallbackQuery.Message.MessageID
	userID := update.CallbackQuery.From.ID

	// Parse callback data: "buyexec:AMOUNT:INDEX:marketID:market"
	parts := strings.Split(update.CallbackQuery.Data, ":")
	if len(parts) != 5 {
		return
	}
	amountStr := parts[1]
	outcomeIndex := parts[2]
	marketID := parts[3]
	// parts[4] is "market" (order type)

	amount, _ := strconv.ParseFloat(amountStr, 64)
	idx, _ := strconv.Atoi(outcomeIndex)

	// Check if user has wallet
	user, err := b.userRepo.GetByTelegramID(ctx, userID)
	if err != nil || user == nil {
		b.editMessage(chatID, messageID, "❌ You need to set up a wallet first. Use /start")
		return
	}

	// Fetch market info
	marketClient := polymarket.NewMarketClient()
	market, err := marketClient.GetMarketByID(ctx, marketID)
	if err != nil {
		b.editMessage(chatID, messageID, fmt.Sprintf("❌ Failed to fetch market: %v", err))
		return
	}

	outcomes := market.GetOutcomes()
	outcomeName := "Unknown"
	if idx < len(outcomes) {
		outcomeName = outcomes[idx]
	}

	marketName := market.Question
	marketName = truncateUTF8(marketName, 40)

	// Show processing message
	b.editMessage(chatID, messageID, fmt.Sprintf(`⏳ *Processing Market Order...*

*Market:* %s
*Side:* Buy %s
*Amount:* $%.2f

Please wait...`, marketName, outcomeName, amount))

	// Execute the trade using index (market order - price = 0)
	result := b.executeBuyOrderByIndex(ctx, user, market, idx, amount, 0)

	// Show result
	if result.Success {
		message := fmt.Sprintf(`✅ *Order Executed Successfully!*

*Market:* %s
*Side:* Buy %s
*Amount:* $%.2f
*Order ID:* %s

Use /positions to check your positions.
`, marketName, outcomeName, amount, result.OrderID)
		b.editMessage(chatID, messageID, message)
	} else {
		message := fmt.Sprintf(`❌ *Order Failed*

*Market:* %s
*Error:* %s

Please try again or contact support.
`, marketName, result.ErrorMsg)
		b.editMessage(chatID, messageID, message)
	}
}

// handleBuyLimitCallback prompts user for limit price
func (b *Bot) handleBuyLimitCallback(ctx context.Context, update *tgbotapi.Update) {
	chatID := update.CallbackQuery.Message.Chat.ID
	messageID := update.CallbackQuery.Message.MessageID
	userID := update.CallbackQuery.From.ID

	// Parse callback data: "buylimit:AMOUNT:INDEX:marketID"
	parts := strings.Split(update.CallbackQuery.Data, ":")
	if len(parts) != 4 {
		return
	}
	amountStr := parts[1]
	outcomeIndex := parts[2]
	marketID := parts[3]

	amount, _ := strconv.ParseFloat(amountStr, 64)
	idx, _ := strconv.Atoi(outcomeIndex)

	// Fetch market info for display
	marketClient := polymarket.NewMarketClient()
	market, err := marketClient.GetMarketByID(ctx, marketID)
	if err != nil {
		b.editMessage(chatID, messageID, fmt.Sprintf("❌ Failed to fetch market: %v", err))
		return
	}

	outcomes := market.GetOutcomes()
	prices := market.GetOutcomePrices()
	outcomeName := "Unknown"
	currentPrice := 0.0
	if idx < len(outcomes) {
		outcomeName = outcomes[idx]
	}
	if idx < len(prices) {
		fmt.Sscanf(prices[idx], "%f", &currentPrice)
	}

	marketName := market.Question
	marketName = truncateUTF8(marketName, 40)

	// Store context for the limit price input
	b.stateManager.SetState(userID, StateWaitingForBuyLimitPrice, map[string]interface{}{
		"market_id":      marketID,
		"outcome_index":  idx,
		"outcome_name":   outcomeName,
		"amount":         amount,
		"market_name":    marketName,
		"current_price":  currentPrice,
		"chat_id":        chatID,
		"message_id":     messageID,
	}, 5*time.Minute)

	// Show prompt for limit price
	message := fmt.Sprintf(`📝 *Limit Buy Order*

*Market:* %s
*Outcome:* %s
*Amount:* $%.2f
*Current Price:* $%.2f

───────────────

💰 *Enter your limit price (0.01 - 0.99):*

_For example, type: 0.45_

Your order will only fill if the price reaches your limit or better.
`, marketName, outcomeName, amount, currentPrice)

	b.editMessage(chatID, messageID, message)
}

// handleBuyLimitPriceInput handles when user enters a limit price for buy order
func (b *Bot) handleBuyLimitPriceInput(ctx context.Context, update *tgbotapi.Update, userCtx *UserContext) {
	chatID := update.Message.Chat.ID
	userID := update.Message.From.ID
	text := strings.TrimSpace(update.Message.Text)

	// Check for cancel
	if strings.ToLower(text) == "/cancel" {
		b.stateManager.ClearState(userID)
		b.sendMessage(chatID, "❌ Order cancelled.")
		return
	}

	// Parse price
	text = strings.TrimPrefix(text, "$")
	var limitPrice float64
	if _, err := fmt.Sscanf(text, "%f", &limitPrice); err != nil || limitPrice <= 0 || limitPrice >= 1 {
		b.sendMessage(chatID, "❌ Invalid price. Please enter a value between 0.01 and 0.99 (e.g., 0.45)")
		return
	}

	// Get order data from state
	marketID, _ := userCtx.Data["market_id"].(string)
	outcomeIndex, _ := userCtx.Data["outcome_index"].(int)
	outcomeName, _ := userCtx.Data["outcome_name"].(string)
	amount, _ := userCtx.Data["amount"].(float64)
	marketName, _ := userCtx.Data["market_name"].(string)

	if marketID == "" {
		b.stateManager.ClearState(userID)
		b.sendMessage(chatID, "❌ Session expired. Please start over with /market.")
		return
	}

	// Get user
	user, err := b.userRepo.GetByTelegramID(ctx, userID)
	if err != nil || user == nil {
		b.stateManager.ClearState(userID)
		b.sendMessage(chatID, "❌ User not found.")
		return
	}

	// Clear state before executing
	b.stateManager.ClearState(userID)

	// Fetch market info
	marketClient := polymarket.NewMarketClient()
	market, err := marketClient.GetMarketByID(ctx, marketID)
	if err != nil {
		b.sendMessage(chatID, fmt.Sprintf("❌ Failed to fetch market: %v", err))
		return
	}

	// Show processing message
	b.sendMessage(chatID, fmt.Sprintf(`⏳ *Processing Limit Buy Order...*

*Market:* %s
*Side:* Buy %s
*Amount:* $%.2f
*Limit Price:* $%.2f

Please wait...`, marketName, outcomeName, amount, limitPrice))

	// Execute the limit buy order
	result := b.executeBuyOrderByIndex(ctx, user, market, outcomeIndex, amount, limitPrice)

	// Show result
	if result.Success {
		message := fmt.Sprintf(`✅ *Limit Buy Order Placed!*

*Market:* %s
*Side:* Buy %s
*Amount:* $%.2f
*Limit Price:* $%.2f
*Order ID:* %s

Your order will fill when the price reaches $%.2f or lower.
Use /orders to check your open orders.
`, marketName, outcomeName, amount, limitPrice, result.OrderID, limitPrice)
		b.sendMessage(chatID, message)
	} else {
		message := fmt.Sprintf(`❌ *Order Failed*

*Market:* %s
*Error:* %s

Please try again or contact support.
`, marketName, result.ErrorMsg)
		b.sendMessage(chatID, message)
	}
}

// handleCustomAmountCallback handles custom amount input
func (b *Bot) handleCustomAmountCallback(ctx context.Context, update *tgbotapi.Update) {
	chatID := update.CallbackQuery.Message.Chat.ID
	userID := update.CallbackQuery.From.ID

	// Parse callback data: "cust:INDEX:marketID" where INDEX is 0 or 1
	parts := strings.Split(update.CallbackQuery.Data, ":")
	if len(parts) != 3 {
		return
	}
	outcomeIndex := parts[1] // "0" or "1"
	marketID := parts[2]

	message := `✏️ *Enter Custom Amount*

Please enter the amount in USD (e.g., "75" for $75):

_Type /cancel to abort._
`

	b.sendMessage(chatID, message)

	// Store order context in state with outcome INDEX (not name)
	b.stateManager.SetState(userID, StateWaitingForAmount, map[string]interface{}{
		"outcome_index": outcomeIndex,
		"market_id":     marketID,
	}, 5*time.Minute)
}

// handleRefreshPositions handles the refresh positions button callback
func (b *Bot) handleRefreshPositions(ctx context.Context, update *tgbotapi.Update) {
	userID := update.CallbackQuery.From.ID
	chatID := update.CallbackQuery.Message.Chat.ID
	messageID := update.CallbackQuery.Message.MessageID

	// Get user
	user, err := b.userRepo.GetByTelegramID(ctx, userID)
	if err != nil || user == nil {
		b.editMessage(chatID, messageID, "❌ User not found. Please use /start to set up your wallet.")
		return
	}

	if user.ProxyAddress == "" {
		b.editMessage(chatID, messageID, "❌ No proxy wallet found. Please ensure you have traded on Polymarket.")
		return
	}

	// Show loading state
	b.editMessage(chatID, messageID, "🔄 *Refreshing positions...*\n\n_Scanning blockchain activity..._")

	// Fetch positions using Polymarket Data API (no blockchain required)
	proxyAddr := common.HexToAddress(user.ProxyAddress)
	unifiedScanner := polymarket.NewUnifiedPositionScanner(nil)
	summary, err := unifiedScanner.ScanAllStrategies(ctx, proxyAddr)
	if err != nil {
		log.Printf("Unified position scan error: %v", err)
	}

	// Build full message with footer
	fullMessage := summary
	if err == nil {
		fullMessage += `

💡 *Tips:*
• Positions shown are from recent activity
• For complete history, visit Polymarket.com
• Use /wallet to check your USDC balance`
	}

	// Add refresh, sell, and redeem buttons
	keyboard := tgbotapi.NewInlineKeyboardMarkup(
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("🔄 Refresh", "refresh_positions"),
			tgbotapi.NewInlineKeyboardButtonData("💰 Sell", "sell_positions"),
		),
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("🎁 Redeem", "redeem_positions"),
		),
	)

	b.editMessageWithKeyboard(chatID, messageID, fullMessage, keyboard)
}

// handleSellPositions shows the list of positions available for selling
func (b *Bot) handleSellPositions(ctx context.Context, update *tgbotapi.Update) {
	userID := update.CallbackQuery.From.ID
	chatID := update.CallbackQuery.Message.Chat.ID
	messageID := update.CallbackQuery.Message.MessageID

	// Get user
	user, err := b.userRepo.GetByTelegramID(ctx, userID)
	if err != nil || user == nil {
		b.editMessage(chatID, messageID, "❌ User not found. Please use /start to set up your wallet.")
		return
	}

	if user.ProxyAddress == "" {
		b.editMessage(chatID, messageID, "❌ No proxy wallet found. Please ensure you have traded on Polymarket.")
		return
	}

	// Show loading
	b.editMessage(chatID, messageID, "💰 *Loading positions for sale...*")

	// Fetch positions using Polymarket Data API (no blockchain required)
	proxyAddr := common.HexToAddress(user.ProxyAddress)
	unifiedScanner := polymarket.NewUnifiedPositionScanner(nil)
	positions, err := unifiedScanner.GetPositions(ctx, proxyAddr)

	if err != nil {
		b.editMessage(chatID, messageID, fmt.Sprintf("❌ Failed to fetch positions: %v", err))
		return
	}

	if len(positions) == 0 {
		keyboard := tgbotapi.NewInlineKeyboardMarkup(
			tgbotapi.NewInlineKeyboardRow(
				tgbotapi.NewInlineKeyboardButtonData("← Back", "back_to_positions"),
			),
		)
		b.editMessageWithKeyboard(chatID, messageID, "📊 *No positions to sell*\n\nYou don't have any active positions.", keyboard)
		return
	}

	// Build position list with buttons
	message := fmt.Sprintf("💰 *Select Position to Sell* (%d positions)\n\n", len(positions))

	// Create buttons for each position (max 8 due to Telegram limits)
	var rows [][]tgbotapi.InlineKeyboardButton
	for i, pos := range positions {
		if i >= 8 {
			message += fmt.Sprintf("\n_...and %d more positions_", len(positions)-8)
			break
		}

		// Truncate title
		title := truncateUTF8(pos.MarketTitle, 25)

		// Format shares
		sharesStr := polymarket.FormatShares(pos.Shares)

		// Button text: "Title - 10.5 YES"
		btnText := fmt.Sprintf("%s - %s %s", title, sharesStr, pos.Outcome)
		btnText = truncateUTF8(btnText, 40)

		// Callback data: sellpos:<index>
		// We use index because callback data is limited to 64 bytes
		rows = append(rows, tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData(btnText, fmt.Sprintf("sellpos:%d", i)),
		))
	}

	// Add back button
	rows = append(rows, tgbotapi.NewInlineKeyboardRow(
		tgbotapi.NewInlineKeyboardButtonData("← Back to Positions", "back_to_positions"),
	))

	keyboard := tgbotapi.InlineKeyboardMarkup{InlineKeyboard: rows}
	b.editMessageWithKeyboard(chatID, messageID, message, keyboard)

	// Store positions in state for later access
	b.stateManager.SetState(userID, StateSelectingPosition, map[string]interface{}{
		"positions": positions,
	}, 10*time.Minute)
}

// handleSellPositionDetail shows details for a specific position with sell options
func (b *Bot) handleSellPositionDetail(ctx context.Context, update *tgbotapi.Update) {
	userID := update.CallbackQuery.From.ID
	chatID := update.CallbackQuery.Message.Chat.ID
	messageID := update.CallbackQuery.Message.MessageID

	// Parse position index from callback data: sellpos:<index>
	parts := strings.Split(update.CallbackQuery.Data, ":")
	if len(parts) != 2 {
		b.editMessage(chatID, messageID, "❌ Invalid position selection.")
		return
	}

	posIndex, err := strconv.Atoi(parts[1])
	if err != nil {
		b.editMessage(chatID, messageID, "❌ Invalid position index.")
		return
	}

	// Get positions from state
	userCtx, exists := b.stateManager.GetState(userID)
	if !exists {
		b.editMessage(chatID, messageID, "❌ Session expired. Please click 'Sell' again.")
		return
	}

	positions, ok := userCtx.Data["positions"].([]*polymarket.Position)
	if !ok || posIndex >= len(positions) {
		b.editMessage(chatID, messageID, "❌ Position not found. Please try again.")
		return
	}

	pos := positions[posIndex]

	// Format position details
	sharesStr := polymarket.FormatShares(pos.Shares)
	message := fmt.Sprintf(`💰 *Sell Position*

*Market:* %s
*Outcome:* %s
*Shares:* %s
*Current Price:* $%.2f
*Estimated Value:* $%.2f

───────────────

*Select amount to sell:*
`, pos.MarketTitle, pos.Outcome, sharesStr, pos.CurrentPrice, pos.Value)

	// Create sell amount buttons
	// Callback format: sell:<percentage>:<posIndex>:market (for market order)
	// Callback format: selllimit:<percentage>:<posIndex> (for limit order flow)
	keyboard := tgbotapi.NewInlineKeyboardMarkup(
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("25%", fmt.Sprintf("sellqty:25:%d", posIndex)),
			tgbotapi.NewInlineKeyboardButtonData("50%", fmt.Sprintf("sellqty:50:%d", posIndex)),
			tgbotapi.NewInlineKeyboardButtonData("75%", fmt.Sprintf("sellqty:75:%d", posIndex)),
		),
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("💯 Sell All", fmt.Sprintf("sellqty:100:%d", posIndex)),
		),
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("← Back to Positions", "sell_positions"),
			tgbotapi.NewInlineKeyboardButtonData("❌ Cancel", "back_to_positions"),
		),
	)

	b.editMessageWithKeyboard(chatID, messageID, message, keyboard)
}

// handleSellQuantityCallback handles quantity selection and shows price options
func (b *Bot) handleSellQuantityCallback(ctx context.Context, update *tgbotapi.Update) {
	userID := update.CallbackQuery.From.ID
	chatID := update.CallbackQuery.Message.Chat.ID
	messageID := update.CallbackQuery.Message.MessageID

	// Parse callback data: sellqty:<percentage>:<posIndex>
	parts := strings.Split(update.CallbackQuery.Data, ":")
	if len(parts) != 3 {
		b.editMessage(chatID, messageID, "❌ Invalid parameters.")
		return
	}

	percentage, err := strconv.Atoi(parts[1])
	if err != nil || percentage <= 0 || percentage > 100 {
		b.editMessage(chatID, messageID, "❌ Invalid percentage.")
		return
	}

	posIndex, err := strconv.Atoi(parts[2])
	if err != nil {
		b.editMessage(chatID, messageID, "❌ Invalid position index.")
		return
	}

	// Get positions from state
	userCtx, exists := b.stateManager.GetState(userID)
	if !exists {
		b.editMessage(chatID, messageID, "❌ Session expired. Please start over with /positions.")
		return
	}

	positions, ok := userCtx.Data["positions"].([]*polymarket.Position)
	if !ok || posIndex >= len(positions) {
		b.editMessage(chatID, messageID, "❌ Position not found. Please try again.")
		return
	}

	pos := positions[posIndex]
	sellValue := pos.Value * float64(percentage) / 100.0
	sharesStr := polymarket.FormatShares(pos.Shares)

	// Calculate shares to sell
	posSharesRaw := pos.Shares.Int64()
	sellSharesRaw := (posSharesRaw * int64(percentage)) / 100
	sellSharesFormatted := fmt.Sprintf("%.2f", float64(sellSharesRaw)/1e6)

	message := fmt.Sprintf(`💰 *Sell Order*

*Market:* %s
*Selling:* %d%% (%s of %s shares)
*Estimated Value:* $%.2f
*Current Price:* $%.2f

───────────────

*Choose order type:*

📊 *Market Order* - Sell immediately at best available price
📝 *Limit Order* - Set your minimum price
`, pos.MarketTitle, percentage, sellSharesFormatted, sharesStr, sellValue, pos.CurrentPrice)

	// Create order type buttons
	// sell:<percentage>:<posIndex>:market - market order (immediate)
	// sell:<percentage>:<posIndex>:limit - limit order (ask for price)
	keyboard := tgbotapi.NewInlineKeyboardMarkup(
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("📊 Market Order", fmt.Sprintf("sell:%d:%d:market", percentage, posIndex)),
		),
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("📝 Limit Order", fmt.Sprintf("sell:%d:%d:limit", percentage, posIndex)),
		),
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("← Back", fmt.Sprintf("sellpos:%d", posIndex)),
			tgbotapi.NewInlineKeyboardButtonData("❌ Cancel", "back_to_positions"),
		),
	)

	b.editMessageWithKeyboard(chatID, messageID, message, keyboard)
}

// handleSellAmountCallback executes the sell order
func (b *Bot) handleSellAmountCallback(ctx context.Context, update *tgbotapi.Update) {
	userID := update.CallbackQuery.From.ID
	chatID := update.CallbackQuery.Message.Chat.ID
	messageID := update.CallbackQuery.Message.MessageID

	// Parse callback data: sell:<percentage>:<posIndex>:<orderType>
	// orderType is "market" or "limit"
	parts := strings.Split(update.CallbackQuery.Data, ":")
	if len(parts) != 4 {
		b.editMessage(chatID, messageID, "❌ Invalid sell parameters.")
		return
	}

	percentage, err := strconv.Atoi(parts[1])
	if err != nil || percentage <= 0 || percentage > 100 {
		b.editMessage(chatID, messageID, "❌ Invalid percentage.")
		return
	}

	posIndex, err := strconv.Atoi(parts[2])
	if err != nil {
		b.editMessage(chatID, messageID, "❌ Invalid position index.")
		return
	}

	orderType := parts[3] // "market" or "limit"

	// Get positions from state
	userCtx, exists := b.stateManager.GetState(userID)
	if !exists {
		b.editMessage(chatID, messageID, "❌ Session expired. Please start over with /positions.")
		return
	}

	positions, ok := userCtx.Data["positions"].([]*polymarket.Position)
	if !ok || posIndex >= len(positions) {
		b.editMessage(chatID, messageID, "❌ Position not found. Please try again.")
		return
	}

	pos := positions[posIndex]

	// If limit order, prompt for price
	if orderType == "limit" {
		b.promptForLimitPrice(ctx, update, pos, percentage, posIndex)
		return
	}

	// Market order - execute immediately
	b.executeSellWithPrice(ctx, update, pos, percentage, 0) // 0 means market price
}

// promptForLimitPrice asks user to enter a limit price
func (b *Bot) promptForLimitPrice(ctx context.Context, update *tgbotapi.Update, pos *polymarket.Position, percentage int, posIndex int) {
	userID := update.CallbackQuery.From.ID
	chatID := update.CallbackQuery.Message.Chat.ID
	messageID := update.CallbackQuery.Message.MessageID

	sharesStr := polymarket.FormatShares(pos.Shares)
	posSharesRaw := pos.Shares.Int64()
	sellSharesRaw := (posSharesRaw * int64(percentage)) / 100
	sellSharesFormatted := fmt.Sprintf("%.2f", float64(sellSharesRaw)/1e6)

	message := fmt.Sprintf(`📝 *Set Limit Price*

*Market:* %s
*Selling:* %s of %s shares (%d%%)
*Current Price:* $%.2f

───────────────

Please enter your minimum sell price (0.01 - 0.99):

_Example: Type "0.65" to sell at $0.65 or higher_
_Type /cancel to abort_
`, pos.MarketTitle, sellSharesFormatted, sharesStr, percentage, pos.CurrentPrice)

	b.editMessage(chatID, messageID, message)

	// Get existing positions from state before overwriting
	userCtx, _ := b.stateManager.GetState(userID)
	var positions []*polymarket.Position
	if userCtx != nil && userCtx.Data != nil {
		positions, _ = userCtx.Data["positions"].([]*polymarket.Position)
	}

	// Store context for price input
	b.stateManager.SetState(userID, StateWaitingForLimitPrice, map[string]interface{}{
		"positions":  positions,
		"pos_index":  posIndex,
		"percentage": percentage,
	}, 5*time.Minute)
}

// executeSellWithPrice executes a sell order with optional limit price
func (b *Bot) executeSellWithPrice(ctx context.Context, update *tgbotapi.Update, pos *polymarket.Position, percentage int, limitPrice float64) {
	userID := update.CallbackQuery.From.ID
	chatID := update.CallbackQuery.Message.Chat.ID
	messageID := update.CallbackQuery.Message.MessageID

	// Get user
	user, err := b.userRepo.GetByTelegramID(ctx, userID)
	if err != nil || user == nil {
		b.editMessage(chatID, messageID, "❌ User not found.")
		return
	}

	// Calculate sell amount based on percentage
	sellValue := pos.Value * float64(percentage) / 100.0

	// Calculate exact shares to sell based on percentage of position
	posSharesRaw := pos.Shares.Int64()
	sellSharesRaw := (posSharesRaw * int64(percentage)) / 100

	// Show processing message
	marketName := pos.MarketTitle
	marketName = truncateUTF8(marketName, 40)

	orderTypeStr := "Market"
	priceStr := "best available"
	if limitPrice > 0 {
		orderTypeStr = "Limit"
		priceStr = fmt.Sprintf("$%.2f", limitPrice)
	}

	b.editMessage(chatID, messageID, fmt.Sprintf(`⏳ *Processing %s Sell Order...*

*Market:* %s
*Selling:* %d%% of %s position
*Price:* %s
*Estimated Value:* $%.2f

Please wait...`, orderTypeStr, marketName, percentage, pos.Outcome, priceStr, sellValue))

	// Execute the sell order using position data directly
	result := b.executeSellOrderFromPosition(ctx, user, pos, sellValue, sellSharesRaw, limitPrice)

	// Clear state
	b.stateManager.ClearState(userID)

	// Show result
	if result.Success {
		message := fmt.Sprintf(`✅ *Sell Order Executed!*

*Market:* %s
*Sold:* %d%% of %s position
*Amount:* $%.2f
*Order ID:* %s

Use /positions to check your updated positions.
`, marketName, percentage, pos.Outcome, sellValue, result.OrderID)
		b.editMessage(chatID, messageID, message)
	} else {
		message := fmt.Sprintf(`❌ *Sell Order Failed*

*Market:* %s
*Error:* %s

Please try again or contact support.
`, marketName, result.ErrorMsg)

		// Add retry button
		keyboard := tgbotapi.NewInlineKeyboardMarkup(
			tgbotapi.NewInlineKeyboardRow(
				tgbotapi.NewInlineKeyboardButtonData("🔄 Try Again", "sell_positions"),
				tgbotapi.NewInlineKeyboardButtonData("← Back", "back_to_positions"),
			),
		)
		b.editMessageWithKeyboard(chatID, messageID, message, keyboard)
	}
}

// handleLimitPriceInput handles when user enters a limit price for sell order
func (b *Bot) handleLimitPriceInput(ctx context.Context, update *tgbotapi.Update, userCtx *UserContext) {
	chatID := update.Message.Chat.ID
	userID := update.Message.From.ID
	text := strings.TrimSpace(update.Message.Text)

	// Check for cancel
	if strings.ToLower(text) == "/cancel" {
		b.stateManager.ClearState(userID)
		b.sendMessage(chatID, "❌ Order cancelled.")
		return
	}

	// Parse price
	text = strings.TrimPrefix(text, "$")
	var limitPrice float64
	if _, err := fmt.Sscanf(text, "%f", &limitPrice); err != nil || limitPrice <= 0 || limitPrice >= 1 {
		b.sendMessage(chatID, "❌ Invalid price. Please enter a value between 0.01 and 0.99 (e.g., 0.65)")
		return
	}

	// Get position data from state
	positions, ok := userCtx.Data["positions"].([]*polymarket.Position)
	posIndex, _ := userCtx.Data["pos_index"].(int)
	percentage, _ := userCtx.Data["percentage"].(int)

	if !ok || posIndex >= len(positions) {
		b.stateManager.ClearState(userID)
		b.sendMessage(chatID, "❌ Session expired. Please start over with /positions.")
		return
	}

	pos := positions[posIndex]

	// Get user
	user, err := b.userRepo.GetByTelegramID(ctx, userID)
	if err != nil || user == nil {
		b.stateManager.ClearState(userID)
		b.sendMessage(chatID, "❌ User not found.")
		return
	}

	// Clear state before executing
	b.stateManager.ClearState(userID)

	// Calculate sell values
	sellValue := pos.Value * float64(percentage) / 100.0
	posSharesRaw := pos.Shares.Int64()
	sellSharesRaw := (posSharesRaw * int64(percentage)) / 100

	// Show processing message
	marketName := pos.MarketTitle
	marketName = truncateUTF8(marketName, 40)

	b.sendMessage(chatID, fmt.Sprintf(`⏳ *Processing Limit Sell Order...*

*Market:* %s
*Selling:* %d%% of %s position
*Limit Price:* $%.2f
*Estimated Value:* $%.2f

Please wait...`, marketName, percentage, pos.Outcome, limitPrice, sellValue))

	// Execute the sell order
	result := b.executeSellOrderFromPosition(ctx, user, pos, sellValue, sellSharesRaw, limitPrice)

	// Show result
	if result.Success {
		message := fmt.Sprintf(`✅ *Limit Sell Order Placed!*

*Market:* %s
*Sold:* %d%% of %s position
*Limit Price:* $%.2f
*Order ID:* %s

Your order will fill when the price reaches $%.2f or higher.
Use /positions to check your positions.
`, marketName, percentage, pos.Outcome, limitPrice, result.OrderID, limitPrice)
		b.sendMessage(chatID, message)
	} else {
		message := fmt.Sprintf(`❌ *Limit Sell Order Failed*

*Market:* %s
*Error:* %s

Please try again or contact support.
`, marketName, result.ErrorMsg)
		b.sendMessage(chatID, message)
	}
}

// handleRefreshOrders refreshes the open orders display
func (b *Bot) handleRefreshOrders(ctx context.Context, update *tgbotapi.Update) {
	userID := update.CallbackQuery.From.ID
	chatID := update.CallbackQuery.Message.Chat.ID
	messageID := update.CallbackQuery.Message.MessageID

	// Check if user has a wallet
	user, err := b.userRepo.GetByTelegramID(ctx, userID)
	if err != nil || user == nil {
		b.editMessage(chatID, messageID, "❌ You need to set up a wallet first. Use /start to begin.")
		return
	}

	// Show loading
	b.editMessage(chatID, messageID, "📋 Refreshing orders...")

	// Decrypt the wallet to get API credentials
	userWallet, err := b.walletManager.DecryptPrivateKey(user.EncryptedKey)
	if err != nil {
		b.editMessage(chatID, messageID, "❌ Failed to decrypt wallet")
		return
	}

	// Get API credentials
	creds, err := b.tradingClient.GetOrCreateAPICredentials(ctx, userWallet.PrivateKey)
	if err != nil {
		b.editMessage(chatID, messageID, "❌ Failed to get API credentials")
		return
	}

	// Fetch open orders
	eoaAddress := common.HexToAddress(user.EOAAddress)
	orders, err := b.tradingClient.GetOpenOrders(ctx, eoaAddress, creds)
	if err != nil {
		b.editMessage(chatID, messageID, fmt.Sprintf("❌ Failed to fetch orders: %v", err))
		return
	}

	if len(orders) == 0 {
		msg := `📋 *Your Open Orders*

No open orders found.

Use /markets to find markets and place orders.`
		b.editMessage(chatID, messageID, msg)
		return
	}

	// Format orders for display
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("📋 *Your Open Orders* (%d)\n\n", len(orders)))

	for i, order := range orders {
		if i >= 10 {
			sb.WriteString(fmt.Sprintf("\n_...and %d more orders_", len(orders)-10))
			break
		}

		originalSize := order.OriginalSize
		sizeMatched := order.SizeMatched
		price := order.Price

		sideEmoji := "📈"
		if strings.ToUpper(order.Side) == "SELL" {
			sideEmoji = "📉"
		}

		createdTime := time.Unix(order.CreatedAt, 0).Format("Jan 2 15:04")

		sb.WriteString(fmt.Sprintf("*%d. %s %s*\n", i+1, sideEmoji, strings.ToUpper(order.Side)))
		sb.WriteString(fmt.Sprintf("   Price: $%s | Size: %s\n", price, originalSize))
		sb.WriteString(fmt.Sprintf("   Filled: %s | Type: %s\n", sizeMatched, order.OrderType))
		sb.WriteString(fmt.Sprintf("   Created: %s\n", createdTime))
		sb.WriteString(fmt.Sprintf("   ID: `%s`\n\n", truncateOrderID(order.ID)))
	}

	sb.WriteString("Use /cancel <order\\_id> to cancel an order")

	// Create keyboard with refresh and cancel all buttons
	keyboard := tgbotapi.NewInlineKeyboardMarkup(
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("🔄 Refresh", "refresh_orders"),
			tgbotapi.NewInlineKeyboardButtonData("❌ Cancel All", "cancel_all_orders"),
		),
	)

	b.editMessageWithKeyboard(chatID, messageID, sb.String(), keyboard)
}

// handleCancelAllOrders cancels all open orders
func (b *Bot) handleCancelAllOrders(ctx context.Context, update *tgbotapi.Update) {
	userID := update.CallbackQuery.From.ID
	chatID := update.CallbackQuery.Message.Chat.ID
	messageID := update.CallbackQuery.Message.MessageID

	// Check if user has a wallet
	user, err := b.userRepo.GetByTelegramID(ctx, userID)
	if err != nil || user == nil {
		b.editMessage(chatID, messageID, "❌ You need to set up a wallet first. Use /start to begin.")
		return
	}

	// Show loading
	b.editMessage(chatID, messageID, "⏳ Cancelling all orders...")

	// Decrypt the wallet to get API credentials
	userWallet, err := b.walletManager.DecryptPrivateKey(user.EncryptedKey)
	if err != nil {
		b.editMessage(chatID, messageID, "❌ Failed to decrypt wallet")
		return
	}

	// Get API credentials
	creds, err := b.tradingClient.GetOrCreateAPICredentials(ctx, userWallet.PrivateKey)
	if err != nil {
		b.editMessage(chatID, messageID, "❌ Failed to get API credentials")
		return
	}

	// Cancel all orders
	eoaAddress := common.HexToAddress(user.EOAAddress)
	cancelled, err := b.tradingClient.CancelAllOrders(ctx, eoaAddress, creds)
	if err != nil {
		b.editMessage(chatID, messageID, fmt.Sprintf("❌ Failed to cancel orders: %v", err))
		return
	}

	if cancelled == 0 {
		msg := `📋 *Your Open Orders*

No orders to cancel.

Use /markets to find markets and place orders.`
		b.editMessage(chatID, messageID, msg)
		return
	}

	msg := fmt.Sprintf(`✅ *Orders Cancelled*

Successfully cancelled %d order(s).

Use /markets to find markets and place new orders.`, cancelled)
	b.editMessage(chatID, messageID, msg)
}

// truncateUTF8 truncates a string to maxRunes runes and appends "..." if truncated.
// Unlike byte slicing (s[:n]), this never cuts in the middle of a multi-byte character.
func truncateUTF8(s string, maxRunes int) string {
	if utf8.RuneCountInString(s) <= maxRunes {
		return s
	}
	runes := []rune(s)
	return string(runes[:maxRunes-3]) + "..."
}

// editMessage edits an existing message
func (b *Bot) editMessage(chatID int64, messageID int, text string) {
	msg := tgbotapi.NewEditMessageText(chatID, messageID, text)
	msg.ParseMode = "Markdown"
	if _, err := b.api.Send(msg); err != nil {
		log.Printf("Error editing message: %v", err)
	}
}

// editMessageWithKeyboard edits an existing message with a keyboard
func (b *Bot) editMessageWithKeyboard(chatID int64, messageID int, text string, keyboard tgbotapi.InlineKeyboardMarkup) {
	msg := tgbotapi.NewEditMessageText(chatID, messageID, text)
	msg.ParseMode = "Markdown"
	msg.ReplyMarkup = &keyboard
	if _, err := b.api.Send(msg); err != nil {
		log.Printf("Error editing message with keyboard: %v", err)
	}
}

// Stop stops the bot gracefully
func (b *Bot) Stop() {
	b.api.StopReceivingUpdates()
	log.Println("Bot stopped")
}

// executeBuyOrder executes a buy order on Polymarket
func (b *Bot) executeBuyOrder(ctx context.Context, user *database.User, market *polymarket.GammaMarket, outcome string, amount float64) *polymarket.TradeResult {
	log.Printf("Executing buy order for user %d: %s %s $%.2f", user.TelegramID, outcome, market.ID, amount)

	// Decrypt user's private key
	userWallet, err := b.walletManager.DecryptPrivateKey(user.EncryptedKey)
	if err != nil {
		return &polymarket.TradeResult{
			Success:  false,
			ErrorMsg: fmt.Sprintf("Failed to decrypt wallet: %v", err),
		}
	}

	// Get or create API credentials
	creds, err := b.tradingClient.GetOrCreateAPICredentials(ctx, userWallet.PrivateKey)
	if err != nil {
		return &polymarket.TradeResult{
			Success:  false,
			ErrorMsg: fmt.Sprintf("Failed to get API credentials: %v", err),
		}
	}

	// Test L2 auth before executing trade
	eoaAddress := common.HexToAddress(user.EOAAddress)
	if err := b.tradingClient.TestL2Auth(ctx, eoaAddress, creds); err != nil {
		log.Printf("L2 auth test failed: %v", err)
	}

	// Get token ID for the outcome
	tokenID, err := b.tradingClient.GetTokenIDForOutcome(ctx, market, outcome)
	if err != nil {
		return &polymarket.TradeResult{
			Success:  false,
			ErrorMsg: fmt.Sprintf("Failed to get token ID: %v", err),
		}
	}

	// Determine proxy address
	var proxyAddress common.Address
	if user.ProxyAddress != "" {
		proxyAddress = common.HexToAddress(user.ProxyAddress)
	}

	// Get fee rates: Gamma feeSchedule for calculation, CLOB for order submission
	calcFeeBps := market.GetFeeRateBps()
	var orderFeeBps int
	if feeRate, err := b.tradingClient.GetFeeRate(ctx, tokenID); err != nil {
		log.Printf("Warning: Failed to get CLOB fee rate: %v (using 0)", err)
	} else {
		orderFeeBps = feeRate
	}
	log.Printf("executeBuyOrder: feeSchedule=%+v, feeType=%s, calcFeeBps=%d, orderFeeBps=%d", market.FeeSchedule, market.FeeType, calcFeeBps, orderFeeBps)

	// Build trade request
	tradeReq := &polymarket.TradeRequest{
		MarketID:     market.ID,
		TokenID:      tokenID,
		Side:         "BUY",
		Outcome:      outcome,
		Amount:       amount,
		OrderType:    polymarket.OrderTypeGTC, // Good-til-cancelled
		NegativeRisk: market.NegRisk,          // Use negRisk exchange if market is negRisk
		TakerFeeBps:  orderFeeBps,
		CalcFeeBps:   calcFeeBps,
	}

	log.Printf("Trade request: negRisk=%v, market.NegRisk=%v, orderFeeBps=%d, calcFeeBps=%d", tradeReq.NegativeRisk, market.NegRisk, orderFeeBps, calcFeeBps)

	// Execute the trade
	result, err := b.tradingClient.ExecuteTrade(ctx, userWallet.PrivateKey, proxyAddress, creds, tradeReq)
	if err != nil {
		return &polymarket.TradeResult{
			Success:  false,
			ErrorMsg: fmt.Sprintf("Trade execution failed: %v", err),
		}
	}

	return result
}

// executeBuyOrderByIndex executes a buy order using outcome index (0 or 1) instead of name
// limitPrice of 0 means market order, >0 means limit order at specified price
func (b *Bot) executeBuyOrderByIndex(ctx context.Context, user *database.User, market *polymarket.GammaMarket, outcomeIndex int, amount float64, limitPrice float64) *polymarket.TradeResult {
	log.Printf("Executing buy order (by index) for user %d: index=%d market=%s $%.2f limitPrice=%.2f", user.TelegramID, outcomeIndex, market.ID, amount, limitPrice)

	// Decrypt user's private key
	userWallet, err := b.walletManager.DecryptPrivateKey(user.EncryptedKey)
	if err != nil {
		return &polymarket.TradeResult{
			Success:  false,
			ErrorMsg: fmt.Sprintf("Failed to decrypt wallet: %v", err),
		}
	}

	// Get or create API credentials
	creds, err := b.tradingClient.GetOrCreateAPICredentials(ctx, userWallet.PrivateKey)
	if err != nil {
		return &polymarket.TradeResult{
			Success:  false,
			ErrorMsg: fmt.Sprintf("Failed to get API credentials: %v", err),
		}
	}

	// Test L2 auth before executing trade
	eoaAddress := common.HexToAddress(user.EOAAddress)
	if err := b.tradingClient.TestL2Auth(ctx, eoaAddress, creds); err != nil {
		log.Printf("L2 auth test failed: %v", err)
	}

	// Get token ID by index - this is the key fix!
	tokenID, err := b.tradingClient.GetTokenIDByIndex(ctx, market.ID, outcomeIndex)
	if err != nil {
		return &polymarket.TradeResult{
			Success:  false,
			ErrorMsg: fmt.Sprintf("Failed to get token ID: %v", err),
		}
	}

	// Get outcome name for logging
	outcomes := market.GetOutcomes()
	outcomeName := fmt.Sprintf("index_%d", outcomeIndex)
	if outcomeIndex < len(outcomes) {
		outcomeName = outcomes[outcomeIndex]
	}

	// Determine proxy address
	var proxyAddress common.Address
	if user.ProxyAddress != "" {
		proxyAddress = common.HexToAddress(user.ProxyAddress)
	}

	// Get fee rates: Gamma feeSchedule for calculation, CLOB for order submission
	calcFeeBps := market.GetFeeRateBps()
	var orderFeeBps int
	if feeRate, err := b.tradingClient.GetFeeRate(ctx, tokenID); err != nil {
		log.Printf("Warning: Failed to get CLOB fee rate: %v (using 0)", err)
	} else {
		orderFeeBps = feeRate
	}
	log.Printf("executeBuyOrderByIndex: feeSchedule=%+v, feeType=%s, calcFeeBps=%d, orderFeeBps=%d", market.FeeSchedule, market.FeeType, calcFeeBps, orderFeeBps)

	// Build trade request
	tradeReq := &polymarket.TradeRequest{
		MarketID:     market.ID,
		TokenID:      tokenID,
		Side:         "BUY",
		Outcome:      outcomeName,
		Amount:       amount,
		Price:        limitPrice, // 0 = market order, >0 = limit order
		OrderType:    polymarket.OrderTypeGTC,
		NegativeRisk: market.NegRisk,
		TakerFeeBps:  orderFeeBps,
		CalcFeeBps:   calcFeeBps,
	}

	log.Printf("Trade request (by index): outcomeIndex=%d, outcomeName=%s, tokenID=%s, negRisk=%v, limitPrice=%.2f, orderFeeBps=%d, calcFeeBps=%d",
		outcomeIndex, outcomeName, tokenID, tradeReq.NegativeRisk, limitPrice, orderFeeBps, calcFeeBps)

	// Execute the trade
	result, err := b.tradingClient.ExecuteTrade(ctx, userWallet.PrivateKey, proxyAddress, creds, tradeReq)
	if err != nil {
		return &polymarket.TradeResult{
			Success:  false,
			ErrorMsg: fmt.Sprintf("Trade execution failed: %v", err),
		}
	}

	return result
}

// executeSellOrder executes a sell order on Polymarket
func (b *Bot) executeSellOrder(ctx context.Context, user *database.User, market *polymarket.GammaMarket, outcome string, amount float64) *polymarket.TradeResult {
	log.Printf("Executing sell order for user %d: %s %s $%.2f", user.TelegramID, outcome, market.ID, amount)

	// Decrypt user's private key
	userWallet, err := b.walletManager.DecryptPrivateKey(user.EncryptedKey)
	if err != nil {
		return &polymarket.TradeResult{
			Success:  false,
			ErrorMsg: fmt.Sprintf("Failed to decrypt wallet: %v", err),
		}
	}

	// Get or create API credentials
	creds, err := b.tradingClient.GetOrCreateAPICredentials(ctx, userWallet.PrivateKey)
	if err != nil {
		return &polymarket.TradeResult{
			Success:  false,
			ErrorMsg: fmt.Sprintf("Failed to get API credentials: %v", err),
		}
	}

	// Get token ID for the outcome
	tokenID, err := b.tradingClient.GetTokenIDForOutcome(ctx, market, outcome)
	if err != nil {
		return &polymarket.TradeResult{
			Success:  false,
			ErrorMsg: fmt.Sprintf("Failed to get token ID: %v", err),
		}
	}

	// Determine proxy address
	var proxyAddress common.Address
	if user.ProxyAddress != "" {
		proxyAddress = common.HexToAddress(user.ProxyAddress)
	}

	// Get fee rates: Gamma feeSchedule for calculation, CLOB for order submission
	calcFeeBps := market.GetFeeRateBps()
	var orderFeeBps int
	if feeRate, err := b.tradingClient.GetFeeRate(ctx, tokenID); err != nil {
		log.Printf("Warning: Failed to get CLOB fee rate: %v (using 0)", err)
	} else {
		orderFeeBps = feeRate
	}
	log.Printf("executeSellOrder: feeSchedule=%+v, feeType=%s, calcFeeBps=%d, orderFeeBps=%d", market.FeeSchedule, market.FeeType, calcFeeBps, orderFeeBps)

	// Build trade request
	tradeReq := &polymarket.TradeRequest{
		MarketID:     market.ID,
		TokenID:      tokenID,
		Side:         "SELL",
		Outcome:      outcome,
		Amount:       amount,
		OrderType:    polymarket.OrderTypeGTC,
		NegativeRisk: market.NegRisk, // Use negRisk exchange if market is negRisk
		TakerFeeBps:  orderFeeBps,
		CalcFeeBps:   calcFeeBps,
	}

	log.Printf("Trade request (SELL): outcome=%s, tokenID=%s, negRisk=%v, orderFeeBps=%d, calcFeeBps=%d",
		outcome, tokenID, tradeReq.NegativeRisk, orderFeeBps, calcFeeBps)

	// Execute the trade
	result, execErr := b.tradingClient.ExecuteTrade(ctx, userWallet.PrivateKey, proxyAddress, creds, tradeReq)
	if execErr != nil {
		return &polymarket.TradeResult{
			Success:  false,
			ErrorMsg: fmt.Sprintf("Trade execution failed: %v", execErr),
		}
	}

	return result
}

// executeSellOrderFromPosition executes a sell order using position data directly
// This avoids needing to query the Gamma API for token ID since we have it from the position
// limitPrice of 0 means market order (use best bid), otherwise it's a limit order at specified price
func (b *Bot) executeSellOrderFromPosition(ctx context.Context, user *database.User, pos *polymarket.Position, amount float64, sharesRaw int64, limitPrice float64) *polymarket.TradeResult {
	log.Printf("Executing sell order from position for user %d: %s %s $%.2f (shares: %d, limitPrice: %.2f)", user.TelegramID, pos.Outcome, pos.TokenID, amount, sharesRaw, limitPrice)

	// Decrypt user's private key
	userWallet, err := b.walletManager.DecryptPrivateKey(user.EncryptedKey)
	if err != nil {
		return &polymarket.TradeResult{
			Success:  false,
			ErrorMsg: fmt.Sprintf("Failed to decrypt wallet: %v", err),
		}
	}

	// Get or create API credentials
	creds, err := b.tradingClient.GetOrCreateAPICredentials(ctx, userWallet.PrivateKey)
	if err != nil {
		return &polymarket.TradeResult{
			Success:  false,
			ErrorMsg: fmt.Sprintf("Failed to get API credentials: %v", err),
		}
	}

	// Determine proxy address
	var proxyAddress common.Address
	if user.ProxyAddress != "" {
		proxyAddress = common.HexToAddress(user.ProxyAddress)
	}

	// Get fee rates: Gamma feeSchedule for calculation, CLOB for order submission
	var calcFeeBps, orderFeeBps int
	mc := polymarket.NewMarketClient()
	if gammaMarket, err := mc.GetMarketByID(ctx, pos.MarketID); err != nil {
		log.Printf("Warning: Failed to get market for fee schedule: %v (using 0)", err)
	} else {
		calcFeeBps = gammaMarket.GetFeeRateBps()
		log.Printf("executeSellOrderFromPosition: feeSchedule=%+v, feeType=%s, calcFeeBps=%d", gammaMarket.FeeSchedule, gammaMarket.FeeType, calcFeeBps)
	}
	if feeRate, err := b.tradingClient.GetFeeRate(ctx, pos.TokenID); err != nil {
		log.Printf("Warning: Failed to get CLOB fee rate: %v (using 0)", err)
	} else {
		orderFeeBps = feeRate
	}

	// Build trade request using position data directly
	tradeReq := &polymarket.TradeRequest{
		MarketID:     pos.ConditionID,
		TokenID:      pos.TokenID, // Use token ID directly from position
		Side:         "SELL",
		Outcome:      pos.Outcome,
		Amount:       amount,
		SharesRaw:    sharesRaw,   // Use exact shares from position
		Price:        limitPrice,  // 0 means market order, >0 means limit order
		OrderType:    polymarket.OrderTypeGTC,
		NegativeRisk: pos.NegativeRisk,
		TakerFeeBps:  orderFeeBps,
		CalcFeeBps:   calcFeeBps,
	}

	log.Printf("Sell trade request: tokenID=%s, negRisk=%v, conditionID=%s, amount=$%.2f, posShares=%s, posValue=$%.2f, limitPrice=%.2f, orderFeeBps=%d, calcFeeBps=%d",
		tradeReq.TokenID, tradeReq.NegativeRisk, tradeReq.MarketID, amount, polymarket.FormatShares(pos.Shares), pos.Value, limitPrice, orderFeeBps, calcFeeBps)

	// Check if exchange has approval to transfer shares
	if b.blockchain != nil {
		balanceChecker := blockchain.NewBalanceChecker(b.blockchain.GetClient())
		approved, exchangeAddr, err := balanceChecker.CheckExchangeApproval(ctx, proxyAddress, pos.NegativeRisk)
		if err != nil {
			log.Printf("Warning: Failed to check exchange approval: %v", err)
		} else if !approved {
			exchangeName := "CTFExchange"
			if pos.NegativeRisk {
				exchangeName = "NegRiskCTFExchange"
			}
			log.Printf("Exchange %s (%s) is NOT approved to transfer shares from proxy %s",
				exchangeName, exchangeAddr.Hex(), proxyAddress.Hex())
			return &polymarket.TradeResult{
				Success: false,
				ErrorMsg: fmt.Sprintf("Exchange not approved. Please sell this position once on Polymarket.com to enable approval, then you can sell via bot. (Exchange: %s)", exchangeName),
			}
		} else {
			log.Printf("Exchange approval confirmed for proxy %s", proxyAddress.Hex())
		}
	}

	// Execute the trade
	result, err := b.tradingClient.ExecuteTrade(ctx, userWallet.PrivateKey, proxyAddress, creds, tradeReq)
	if err != nil {
		return &polymarket.TradeResult{
			Success:  false,
			ErrorMsg: fmt.Sprintf("Trade execution failed: %v", err),
		}
	}

	return result
}