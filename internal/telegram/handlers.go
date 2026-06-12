package telegram

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/common"
	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"github.com/Catorpilor/poly/internal/blockchain"
	"github.com/Catorpilor/poly/internal/database"
	"github.com/Catorpilor/poly/internal/polymarket"
	"github.com/Catorpilor/poly/internal/wallet"
)

// handleStart handles the /start command
func (b *Bot) handleStart(ctx context.Context, bot *Bot, update *tgbotapi.Update) error {
	userID := update.Message.From.ID
	username := update.Message.From.UserName

	// Check if user already exists
	existingUser, err := b.userRepo.GetByTelegramID(ctx, userID)
	if err != nil {
		return fmt.Errorf("failed to check existing user: %w", err)
	}

	if existingUser != nil {
		// User already exists
		message := fmt.Sprintf(`
👋 Welcome back, *%s*!

Your wallet is already set up:
📍 EOA Address: `+"`%s`"+`
🏦 Proxy Address: `+"`%s`"+`

Use /wallet to check your balances
Use /markets to view active markets
Use /help for all available commands
`, username, existingUser.EOAAddress, existingUser.ProxyAddress)

		b.sendMessage(update.Message.Chat.ID, message)
		return nil
	}

	// New user - show welcome message with options
	message := `
🎯 *Welcome to Polymarket Trading Bot!*

I'll help you trade on Polymarket directly from Telegram.

To get started, you need a wallet. Choose an option:

🆕 *Create New Wallet* - Generate a new wallet with secure key storage
📥 *Import Wallet* - Import your existing private key

What would you like to do?
`

	// Create inline keyboard
	keyboard := tgbotapi.NewInlineKeyboardMarkup(
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("🆕 Create New Wallet", "create_wallet"),
			tgbotapi.NewInlineKeyboardButtonData("📥 Import Wallet", "import_wallet"),
		),
	)

	b.sendMessageWithKeyboard(update.Message.Chat.ID, message, keyboard)
	return nil
}

// handleWallet handles the /wallet command
func (b *Bot) handleWallet(ctx context.Context, bot *Bot, update *tgbotapi.Update) error {
	userID := update.Message.From.ID

	// Get user from database
	user, err := b.userRepo.GetByTelegramID(ctx, userID)
	if err != nil {
		return fmt.Errorf("failed to get user: %w", err)
	}

	if user == nil {
		b.sendMessage(update.Message.Chat.ID,
			"❌ You haven't set up a wallet yet. Use /start to begin.")
		return nil
	}

	// Fetch the proxy address dynamically if not already stored
	proxyAddress := user.ProxyAddress
	if proxyAddress == "" && b.blockchain != nil {
		// Try to fetch proxy address using the deterministic resolver
		deterministic := polymarket.NewDeterministicProxyResolver(b.blockchain.GetClient())
		eoaAddr := common.HexToAddress(user.EOAAddress)

		if proxy, err := deterministic.GetPolymarketProxy(ctx, eoaAddr); err == nil && proxy != (common.Address{}) {
			proxyAddress = proxy.Hex()

			// Update the database with the proxy address
			user.ProxyAddress = proxyAddress
			if err := b.userRepo.Update(ctx, user); err != nil {
				log.Printf("Failed to update user proxy in database: %v", err)
			}
		}
	}

	// Prepare the initial message
	message := fmt.Sprintf(`
💼 *Your Wallet Information*

📍 *EOA Address:*
`+"`%s`"+`

🏦 *Proxy Wallet (Trading):*
`+"`%s`"+`

💰 *Balances:*
`, user.EOAAddress, proxyAddress)

	// Fetch balances if blockchain client is available
	if b.blockchain != nil && proxyAddress != "" {
		balanceChecker := blockchain.NewBalanceChecker(b.blockchain.GetClient())

		eoaAddr := common.HexToAddress(user.EOAAddress)
		proxyAddr := common.HexToAddress(proxyAddress)

		// Get balances for both addresses
		eoaBalance, proxyBalance, err := balanceChecker.GetAllBalances(ctx, eoaAddr, proxyAddr)
		if err != nil {
			message += "\n⚠️ _Failed to fetch balances_"
			log.Printf("Failed to fetch balances: %v", err)
		} else {
			// Format the balance display
			message += fmt.Sprintf(`

*EOA Wallet:*
• MATIC: %s (gas)
• pUSD: %s

*Proxy Wallet (Trading):*
• MATIC: %s (gas)
• pUSD: %s

`,
				blockchain.FormatMATIC(eoaBalance.MATIC),
				blockchain.FormatUSDC(eoaBalance.USDC),
				blockchain.FormatMATIC(proxyBalance.MATIC),
				blockchain.FormatUSDC(proxyBalance.USDC))
		}
	} else {
		message += "\n_Balance checking unavailable_"
	}

	message += `
Use /positions to see your market positions
Use /pnl to calculate unrealized P&L`

	b.sendMessage(update.Message.Chat.ID, message)
	go func() {
		// This is where we would fetch balances from blockchain
		// For now, just log
		log.Printf("TODO: Fetch balances for user %d", userID)
	}()

	return nil
}

// handleImport handles the /import command
func (b *Bot) handleImport(ctx context.Context, bot *Bot, update *tgbotapi.Update) error {
	userID := update.Message.From.ID

	// Check if user already has a wallet
	existingUser, err := b.userRepo.GetByTelegramID(ctx, userID)
	if err != nil {
		return fmt.Errorf("failed to check existing user: %w", err)
	}

	if existingUser != nil {
		b.sendMessage(update.Message.Chat.ID,
			"⚠️ You already have a wallet set up. For security reasons, you cannot import another wallet.")
		return nil
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

	b.sendMessage(update.Message.Chat.ID, message)

	// Set user state to waiting for private key
	b.stateManager.SetState(userID, StateWaitingForKey, nil, 5*time.Minute)

	return nil
}

// handleExport handles the /export command
func (b *Bot) handleExport(ctx context.Context, bot *Bot, update *tgbotapi.Update) error {
	userID := update.Message.From.ID

	// Get user from database
	user, err := b.userRepo.GetByTelegramID(ctx, userID)
	if err != nil {
		return fmt.Errorf("failed to get user: %w", err)
	}

	if user == nil {
		b.sendMessage(update.Message.Chat.ID,
			"❌ You don't have a wallet to export. Use /start to create one.")
		return nil
	}

	// For security, we don't directly export the private key
	// Instead, provide instructions for secure backup
	message := `
🔐 *Wallet Export & Backup*

For security reasons, we don't directly export private keys through Telegram.

*Your wallet addresses for reference:*
EOA: `+"`%s`"+`
Proxy: `+"`%s`"+`

⚠️ *Important:*
• Your private key is encrypted and stored securely
• Never share your private key in plain text
• Consider using a hardware wallet for large amounts
• Keep multiple secure backups of your key

If you need to recover your wallet, you can use the /import command on a new installation.
`

	b.sendMessage(update.Message.Chat.ID,
		fmt.Sprintf(message, user.EOAAddress, user.ProxyAddress))

	return nil
}

// handleMarkets handles the /markets command
func (b *Bot) handleMarkets(ctx context.Context, bot *Bot, update *tgbotapi.Update) error {
	b.sendMessage(update.Message.Chat.ID, "📊 *Fetching trending markets...*")

	// Fetch trending markets from Gamma API
	marketClient := polymarket.NewMarketClient()
	markets, err := marketClient.GetTrendingMarkets(ctx, 10)
	if err != nil {
		b.sendMessage(update.Message.Chat.ID, fmt.Sprintf("❌ Failed to fetch markets: %v", err))
		return nil
	}

	if len(markets) == 0 {
		b.sendMessage(update.Message.Chat.ID, "📊 *No Active Markets Found*\n\nNo markets are currently available.")
		return nil
	}

	// Get bot username for deep links
	botUsername := b.api.Self.UserName

	// Format markets message with clickable deep links
	message := fmt.Sprintf("📊 *Trending Markets* (Top %d by 24h Volume)\n\n", len(markets))

	for i, m := range markets {
		// Truncate long questions
		question := truncateUTF8(m.Question, 40)

		// Get Yes price from OutcomePrices
		yesPrice := 0.0
		prices := m.GetOutcomePrices()
		if len(prices) > 0 {
			fmt.Sscanf(prices[0], "%f", &yesPrice)
		}

		// Create clickable deep link: https://t.me/botname?start=m_ID
		deepLink := fmt.Sprintf("https://t.me/%s?start=m_%s", botUsername, m.ID)

		message += fmt.Sprintf("*%d.* [%s](%s)\n", i+1, question, deepLink)
		message += fmt.Sprintf("   Yes: %s | Vol: %s\n",
			polymarket.FormatPrice(yesPrice),
			polymarket.FormatVolume(m.Volume24hr))

		if i < len(markets)-1 {
			message += "\n"
		}
	}

	message += "\n_Tap market name to view details & trade_"

	b.sendMessage(update.Message.Chat.ID, message)
	return nil
}

// handleMarket handles the /market command
func (b *Bot) handleMarket(ctx context.Context, bot *Bot, update *tgbotapi.Update) error {
	args := strings.Fields(update.Message.CommandArguments())
	if len(args) == 0 {
		b.sendMessage(update.Message.Chat.ID,
			"❌ Please provide a market slug. Usage: /market <slug>")
		return nil
	}

	slug := args[0]

	// Fetch market details from Gamma API
	marketClient := polymarket.NewMarketClient()
	market, err := marketClient.GetMarketBySlug(ctx, slug)
	if err != nil {
		// Try by ID if slug fails
		market, err = marketClient.GetMarketByID(ctx, slug)
		if err != nil {
			b.sendMessage(update.Message.Chat.ID, fmt.Sprintf("❌ Market not found: %s", slug))
			return nil
		}
	}

	// Get outcomes and prices - they should be in the same order
	outcomes := market.GetOutcomes()
	prices := market.GetOutcomePrices()

	log.Printf("handleMarket: market=%s, outcomes=%v, prices=%v", market.ID, outcomes, prices)

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
		endDate = endDate[:10] // Just the date part
	}

	message := fmt.Sprintf(`📈 *%s*

*Current Prices:*
   %s: %s
   %s: %s

*Market Stats:*
   24h Volume: %s
   Total Volume: %s
   Liquidity: %s

*Status:* %s
*End Date:* %s
`,
		market.Question,
		outcome0Label, polymarket.FormatPrice(price0),
		outcome1Label, polymarket.FormatPrice(price1),
		polymarket.FormatVolume(market.Volume24hr),
		polymarket.FormatVolume(market.Volume),
		polymarket.FormatVolume(market.Liquidity),
		getMarketStatus(market),
		endDate,
	)

	// Create buy buttons using index 0 and 1 instead of yes/no
	// This ensures the button matches the displayed price
	keyboard := tgbotapi.NewInlineKeyboardMarkup(
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData(fmt.Sprintf("📈 Buy %s", outcome0Label), fmt.Sprintf("buy:0:%s", market.ID)),
			tgbotapi.NewInlineKeyboardButtonData(fmt.Sprintf("📉 Buy %s", outcome1Label), fmt.Sprintf("buy:1:%s", market.ID)),
		),
	)

	b.sendMessageWithKeyboard(update.Message.Chat.ID, message, keyboard)
	return nil
}

// handleEvent handles the /event command
func (b *Bot) handleEvent(ctx context.Context, bot *Bot, update *tgbotapi.Update) error {
	args := strings.Fields(update.Message.CommandArguments())
	if len(args) == 0 {
		b.sendMessage(update.Message.Chat.ID,
			"❌ Please provide an event URL or slug. Usage: /event <polymarket-url-or-slug>")
		return nil
	}

	arg := args[0]

	// Try to parse as a Polymarket URL first
	slug, ok := polymarket.ParseEventSlug(arg)
	if !ok {
		// Treat as a bare slug
		slug = arg
	}

	b.handleEventBySlug(ctx, update.Message.Chat.ID, slug)
	return nil
}

// getMarketStatus returns a status string for a market
func getMarketStatus(market *polymarket.GammaMarket) string {
	if market.Closed {
		return "Closed"
	}
	if !market.AcceptingOrders {
		return "Not accepting orders"
	}
	return "Active"
}

// handleBuy handles the /buy command
func (b *Bot) handleBuy(ctx context.Context, bot *Bot, update *tgbotapi.Update) error {
	userID := update.Message.From.ID

	// Check if user has a wallet
	user, err := b.userRepo.GetByTelegramID(ctx, userID)
	if err != nil {
		return fmt.Errorf("failed to get user: %w", err)
	}

	if user == nil {
		b.sendMessage(update.Message.Chat.ID,
			"❌ You need to set up a wallet first. Use /start to begin.")
		return nil
	}

	args := strings.Fields(update.Message.CommandArguments())
	if len(args) < 3 {
		b.sendMessage(update.Message.Chat.ID,
			"❌ Invalid format. Usage: /buy <amount> <YES/NO> <market_id> [price]")
		return nil
	}

	// TODO: Parse arguments and execute buy order
	message := `
💰 *Buy Order*

Processing your buy order...

_This feature will be implemented soon._

Order details will appear here once the trading engine is connected.
`

	b.sendMessage(update.Message.Chat.ID, message)
	return nil
}

// handleSell handles the /sell command
func (b *Bot) handleSell(ctx context.Context, bot *Bot, update *tgbotapi.Update) error {
	userID := update.Message.From.ID

	// Check if user has a wallet
	user, err := b.userRepo.GetByTelegramID(ctx, userID)
	if err != nil {
		return fmt.Errorf("failed to get user: %w", err)
	}

	if user == nil {
		b.sendMessage(update.Message.Chat.ID,
			"❌ You need to set up a wallet first. Use /start to begin.")
		return nil
	}

	args := strings.Fields(update.Message.CommandArguments())
	if len(args) < 3 {
		b.sendMessage(update.Message.Chat.ID,
			"❌ Invalid format. Usage: /sell <amount> <YES/NO> <market_id> [price]")
		return nil
	}

	// TODO: Parse arguments and execute sell order
	message := `
💸 *Sell Order*

Processing your sell order...

_This feature will be implemented soon._

Order details will appear here once the trading engine is connected.
`

	b.sendMessage(update.Message.Chat.ID, message)
	return nil
}

// handleOrders handles the /orders command
func (b *Bot) handleOrders(ctx context.Context, bot *Bot, update *tgbotapi.Update) error {
	userID := update.Message.From.ID

	// Check if user has a wallet
	user, err := b.userRepo.GetByTelegramID(ctx, userID)
	if err != nil {
		return fmt.Errorf("failed to get user: %w", err)
	}

	if user == nil {
		b.sendMessage(update.Message.Chat.ID,
			"❌ You need to set up a wallet first. Use /start to begin.")
		return nil
	}

	// Send loading message
	loadingMsg := b.sendMessageAndReturn(update.Message.Chat.ID, "📋 Fetching your open orders...")

	// Decrypt the wallet to get API credentials
	userWallet, err := b.walletManager.DecryptPrivateKey(user.EncryptedKey)
	if err != nil {
		b.editMessage(update.Message.Chat.ID, loadingMsg.MessageID, "❌ Failed to decrypt wallet")
		return fmt.Errorf("failed to decrypt wallet: %w", err)
	}

	// Get API credentials
	creds, err := b.tradingClient.GetOrCreateAPICredentials(ctx, userWallet.PrivateKey)
	if err != nil {
		b.editMessage(update.Message.Chat.ID, loadingMsg.MessageID, "❌ Failed to get API credentials")
		return fmt.Errorf("failed to get API credentials: %w", err)
	}

	// Fetch open orders
	eoaAddress := common.HexToAddress(user.EOAAddress)
	orders, err := b.tradingClient.GetOpenOrders(ctx, eoaAddress, creds)
	if err != nil {
		b.editMessage(update.Message.Chat.ID, loadingMsg.MessageID, fmt.Sprintf("❌ Failed to fetch orders: %v", err))
		return nil
	}

	if len(orders) == 0 {
		msg := `📋 *Your Open Orders*

No open orders found.

Use /markets to find markets and place orders.`
		b.editMessage(update.Message.Chat.ID, loadingMsg.MessageID, msg)
		return nil
	}

	// Format orders for display
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("📋 *Your Open Orders* (%d)\n\n", len(orders)))

	for i, order := range orders {
		if i >= 10 {
			sb.WriteString(fmt.Sprintf("\n_...and %d more orders_", len(orders)-10))
			break
		}

		// Parse size values
		originalSize := order.OriginalSize
		sizeMatched := order.SizeMatched
		price := order.Price

		// Format side emoji
		sideEmoji := "📈"
		if strings.ToUpper(order.Side) == "SELL" {
			sideEmoji = "📉"
		}

		// Format creation time
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

	editMsg := tgbotapi.NewEditMessageText(update.Message.Chat.ID, loadingMsg.MessageID, sb.String())
	editMsg.ParseMode = "Markdown"
	editMsg.ReplyMarkup = &keyboard
	b.api.Send(editMsg)

	return nil
}

// truncateOrderID truncates an order ID for display
func truncateOrderID(orderID string) string {
	if len(orderID) <= 16 {
		return orderID
	}
	return orderID[:8] + "..." + orderID[len(orderID)-4:]
}

// handleCancel handles the /cancel command
func (b *Bot) handleCancel(ctx context.Context, bot *Bot, update *tgbotapi.Update) error {
	userID := update.Message.From.ID
	args := strings.Fields(update.Message.CommandArguments())
	if len(args) == 0 {
		b.sendMessage(update.Message.Chat.ID,
			"❌ Please provide an order ID. Usage: /cancel <order_id>")
		return nil
	}

	orderID := args[0]

	// Check if user has a wallet
	user, err := b.userRepo.GetByTelegramID(ctx, userID)
	if err != nil {
		return fmt.Errorf("failed to get user: %w", err)
	}

	if user == nil {
		b.sendMessage(update.Message.Chat.ID,
			"❌ You need to set up a wallet first. Use /start to begin.")
		return nil
	}

	// Send loading message
	loadingMsg := b.sendMessageAndReturn(update.Message.Chat.ID, fmt.Sprintf("🚫 Cancelling order `%s`...", truncateOrderID(orderID)))

	// Decrypt the wallet to get API credentials
	userWallet, err := b.walletManager.DecryptPrivateKey(user.EncryptedKey)
	if err != nil {
		b.editMessage(update.Message.Chat.ID, loadingMsg.MessageID, "❌ Failed to decrypt wallet")
		return fmt.Errorf("failed to decrypt wallet: %w", err)
	}

	// Get API credentials
	creds, err := b.tradingClient.GetOrCreateAPICredentials(ctx, userWallet.PrivateKey)
	if err != nil {
		b.editMessage(update.Message.Chat.ID, loadingMsg.MessageID, "❌ Failed to get API credentials")
		return fmt.Errorf("failed to get API credentials: %w", err)
	}

	// Cancel the order
	eoaAddress := common.HexToAddress(user.EOAAddress)
	err = b.tradingClient.CancelOrder(ctx, eoaAddress, creds, orderID)
	if err != nil {
		b.editMessage(update.Message.Chat.ID, loadingMsg.MessageID, fmt.Sprintf("❌ Failed to cancel order: %v", err))
		return nil
	}

	message := fmt.Sprintf(`✅ *Order Cancelled*

Successfully cancelled order:
`+"`%s`"+`

Use /orders to view remaining orders.
`, truncateOrderID(orderID))

	b.editMessage(update.Message.Chat.ID, loadingMsg.MessageID, message)
	return nil
}

// handlePositions handles the /positions command
func (b *Bot) handlePositions(ctx context.Context, bot *Bot, update *tgbotapi.Update) error {
	userID := update.Message.From.ID

	// Check if user has a wallet
	user, err := b.userRepo.GetByTelegramID(ctx, userID)
	if err != nil {
		return fmt.Errorf("failed to get user: %w", err)
	}

	if user == nil {
		b.sendMessage(update.Message.Chat.ID,
			"❌ You need to set up a wallet first. Use /start to begin.")
		return nil
	}

	// Check if user has a proxy address
	if user.ProxyAddress == "" {
		// Try to fetch proxy address
		if b.blockchain != nil {
			deterministic := polymarket.NewDeterministicProxyResolver(b.blockchain.GetClient())
			eoaAddr := common.HexToAddress(user.EOAAddress)

			if proxy, err := deterministic.GetPolymarketProxy(ctx, eoaAddr); err == nil && proxy != (common.Address{}) {
				user.ProxyAddress = proxy.Hex()
				b.userRepo.Update(ctx, user)
			}
		}

		if user.ProxyAddress == "" {
			b.sendMessage(update.Message.Chat.ID, "❌ Could not find your proxy wallet. Please ensure you have traded on Polymarket before.")
			return nil
		}
	}

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

	// Add refresh, sell, redeem, and SL/TP buttons
	keyboard := tgbotapi.NewInlineKeyboardMarkup(
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("🔄 Refresh", "refresh_positions"),
			tgbotapi.NewInlineKeyboardButtonData("💰 Sell", "sell_positions"),
		),
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("🎁 Redeem", "redeem_positions"),
			tgbotapi.NewInlineKeyboardButtonData("🎯 SL/TP", "sltp_list"),
		),
	)

	b.sendMessageWithKeyboard(update.Message.Chat.ID, fullMessage, keyboard)
	return nil
}

// handlePNL handles the /pnl command
func (b *Bot) handlePNL(ctx context.Context, bot *Bot, update *tgbotapi.Update) error {
	userID := update.Message.From.ID

	// Check if user has a wallet
	user, err := b.userRepo.GetByTelegramID(ctx, userID)
	if err != nil {
		return fmt.Errorf("failed to get user: %w", err)
	}

	if user == nil {
		b.sendMessage(update.Message.Chat.ID,
			"❌ You need to set up a wallet first. Use /start to begin.")
		return nil
	}

	// TODO: Calculate P&L from positions
	message := `
💹 *Unrealized P&L*

Calculating your profit and loss...

_This feature will be implemented soon._

This will show:
• Total invested
• Current value
• Unrealized P&L
• Percentage return
`

	b.sendMessage(update.Message.Chat.ID, message)
	return nil
}

// handleHistory handles the /history command
func (b *Bot) handleHistory(ctx context.Context, bot *Bot, update *tgbotapi.Update) error {
	userID := update.Message.From.ID

	// Check if user has a wallet
	user, err := b.userRepo.GetByTelegramID(ctx, userID)
	if err != nil {
		return fmt.Errorf("failed to get user: %w", err)
	}

	if user == nil {
		b.sendMessage(update.Message.Chat.ID,
			"❌ You need to set up a wallet first. Use /start to begin.")
		return nil
	}

	// TODO: Fetch trade history from database
	message := `
📜 *Trade History*

Loading your recent trades...

_This feature will be implemented soon._
`

	b.sendMessage(update.Message.Chat.ID, message)
	return nil
}

// handleSettings handles the /settings command
func (b *Bot) handleSettings(ctx context.Context, bot *Bot, update *tgbotapi.Update) error {
	userID := update.Message.From.ID

	// Check if user has a wallet
	user, err := b.userRepo.GetByTelegramID(ctx, userID)
	if err != nil {
		return fmt.Errorf("failed to get user: %w", err)
	}

	if user == nil {
		b.sendMessage(update.Message.Chat.ID,
			"❌ You need to set up a wallet first. Use /start to begin.")
		return nil
	}

	// TODO: Show and manage user settings
	message := `
⚙️ *Settings*

Current settings:
• Default slippage: 2%
• Max order size: $10,000
• Price alerts: Enabled

_Settings management will be implemented soon._
`

	b.sendMessage(update.Message.Chat.ID, message)
	return nil
}

// handleAlerts handles the /alerts command
func (b *Bot) handleAlerts(ctx context.Context, bot *Bot, update *tgbotapi.Update) error {
	userID := update.Message.From.ID

	// Check if user has a wallet
	user, err := b.userRepo.GetByTelegramID(ctx, userID)
	if err != nil {
		return fmt.Errorf("failed to get user: %w", err)
	}

	if user == nil {
		b.sendMessage(update.Message.Chat.ID,
			"❌ You need to set up a wallet first. Use /start to begin.")
		return nil
	}

	// TODO: Manage price alerts
	message := `
🔔 *Price Alerts*

Managing your price alerts...

_This feature will be implemented soon._

You'll be able to:
• Set price alerts for markets
• Get notified when prices hit targets
• Manage active alerts
`

	b.sendMessage(update.Message.Chat.ID, message)
	return nil
}

// handleGas handles the /gas command
func (b *Bot) handleGas(ctx context.Context, bot *Bot, update *tgbotapi.Update) error {
	userID := update.Message.From.ID

	// Check if user has a wallet
	user, err := b.userRepo.GetByTelegramID(ctx, userID)
	if err != nil {
		return fmt.Errorf("failed to get user: %w", err)
	}

	if user == nil {
		b.sendMessage(update.Message.Chat.ID,
			"❌ You need to set up a wallet first. Use /start to begin.")
		return nil
	}

	// TODO: Check MATIC balance for gas
	message := fmt.Sprintf(`
⛽ *Gas Balance*

Checking MATIC balance for gas fees...

EOA Address: `+"`%s`"+`

_Balance check will be implemented soon._
`, user.EOAAddress)

	b.sendMessage(update.Message.Chat.ID, message)
	return nil
}

// handleHelp handles the /help command
func (b *Bot) handleHelp(ctx context.Context, bot *Bot, update *tgbotapi.Update) error {
	message := `
📚 *Polymarket Trading Bot Help*

*Wallet Commands:*
/start - Initialize bot and create/import wallet
/wallet - Show wallet addresses and balances
/import - Import existing wallet
/export - Export wallet backup

*Trading Commands:*
/markets - List active markets
/market <id> - Show market details
/buy <amount> <YES/NO> <market_id> [price] - Buy tokens
/sell <amount> <YES/NO> <market_id> [price] - Sell tokens
/orders - Show open orders
/cancel <order_id> - Cancel an order

*Portfolio Commands:*
/positions - Show all positions
/pnl - Calculate unrealized P&L
/history - View trade history

*Settings & Utilities:*
/settings - Configure preferences
/alerts - Set price alerts
/gas - Check MATIC balance for gas
/help - Show this help message

*Quick Tips:*
• All prices are between 0 and 1 (0% to 100%)
• YES tokens pay $1 if outcome happens
• NO tokens pay $1 if outcome doesn't happen
• Always keep some MATIC for gas fees

Need more help? Contact support.
`

	b.sendMessage(update.Message.Chat.ID, message)
	return nil
}

// handlePrivateKeyInput handles private key input from users
func (b *Bot) handlePrivateKeyInput(ctx context.Context, update *tgbotapi.Update) {
	userID := update.Message.From.ID
	chatID := update.Message.Chat.ID
	messageID := update.Message.MessageID
	privateKeyInput := strings.TrimSpace(update.Message.Text)

	// Delete the message immediately for security
	b.deleteMessage(chatID, messageID)

	// Clear the state
	defer b.stateManager.ClearState(userID)

	// Check if user typed /cancel
	if strings.ToLower(privateKeyInput) == "/cancel" {
		b.sendMessage(chatID, "❌ Import cancelled.")
		return
	}

	// Validate the private key
	if err := wallet.ValidatePrivateKey(privateKeyInput); err != nil {
		b.sendMessage(chatID, fmt.Sprintf("❌ Invalid private key: %v\n\nPlease use /import to try again.", err))
		return
	}

	// Import the private key
	userWallet, err := b.walletManager.ImportPrivateKey(privateKeyInput)
	if err != nil {
		b.sendMessage(chatID, fmt.Sprintf("❌ Failed to import wallet: %v\n\nPlease use /import to try again.", err))
		return
	}

	// Encrypt the private key for storage
	encryptedKey, err := b.walletManager.EncryptPrivateKey(userWallet)
	if err != nil {
		b.sendMessage(chatID, "❌ Failed to encrypt private key. Please try again.")
		log.Printf("Failed to encrypt key for user %d: %v", userID, err)
		return
	}

	// Get username
	username := update.Message.From.UserName
	if username == "" {
		username = update.Message.From.FirstName
	}

	// Check for existing proxy wallet
	var proxyAddress string

	// Log the EOA we're checking
	log.Printf("Checking for proxy wallet for EOA: %s", userWallet.EOAAddress.Hex())

	// Try deterministic calculation first (most reliable for Polymarket)
	if b.blockchain != nil {
		deterministic := polymarket.NewDeterministicProxyResolver(b.blockchain.GetClient())

		// Debug mode - log what we're finding
		deterministic.DebugProxyDetection(ctx, userWallet.EOAAddress)

		if proxy, err := deterministic.GetPolymarketProxy(ctx, userWallet.EOAAddress); err == nil && proxy.Hex() != "0x0000000000000000000000000000000000000000" {
			proxyAddress = proxy.Hex()
			log.Printf("Found proxy wallet using deterministic calculation: %s for EOA %s", proxyAddress, userWallet.EOAAddress.Hex())
		} else {
			// Try registry method
			if proxy, err := deterministic.GetProxyUsingRegistry(ctx, userWallet.EOAAddress); err == nil && proxy.Hex() != "0x0000000000000000000000000000000000000000" {
				proxyAddress = proxy.Hex()
				log.Printf("Found proxy wallet using registry: %s for EOA %s", proxyAddress, userWallet.EOAAddress.Hex())
			}
		}
	}

	// Fallback to API method
	if proxyAddress == "" && b.proxyResolver != nil {
		if proxy, err := b.proxyResolver.GetProxyWallet(ctx, userWallet.EOAAddress); err == nil && proxy.Hex() != "0x0000000000000000000000000000000000000000" {
			proxyAddress = proxy.Hex()
			log.Printf("Found proxy wallet via Polymarket API: %s for EOA %s", proxyAddress, userWallet.EOAAddress.Hex())
		}
	}

	// Last resort: check Gnosis Safe factories
	if proxyAddress == "" && b.blockchain != nil {
		detector := blockchain.NewGnosisSafeDetector(b.blockchain.GetClient())
		if proxy, err := detector.FindProxyWallet(ctx, userWallet.EOAAddress); err == nil && proxy.Hex() != "0x0000000000000000000000000000000000000000" {
			proxyAddress = proxy.Hex()
			log.Printf("Found proxy wallet via Gnosis Safe detection: %s for EOA %s", proxyAddress, userWallet.EOAAddress.Hex())
		}
	}

	// If still not found, calculate the expected address
	if proxyAddress == "" {
		expectedProxy := polymarket.CalculatePolymarketProxyAddress(userWallet.EOAAddress)
		log.Printf("No proxy found. Expected proxy address would be: %s", expectedProxy.Hex())
	}

	// Create user in database
	newUser := &database.User{
		TelegramID:   userID,
		Username:     username,
		EOAAddress:   userWallet.EOAAddress.Hex(),
		ProxyAddress: proxyAddress, // Set the proxy if found
		EncryptedKey: encryptedKey,
		Settings:     database.JSONB{},
		IsActive:     true,
	}

	if err := b.userRepo.Create(ctx, newUser); err != nil {
		b.sendMessage(chatID, "❌ Failed to save wallet. Please try again.")
		log.Printf("Failed to create user %d: %v", userID, err)
		return
	}

	// Success message
	message := fmt.Sprintf(`
✅ *Wallet Imported Successfully!*

Your wallet has been imported and encrypted.

📍 *EOA Address:* `+"`%s`"+`

⚠️ *Important:*
• Your private key has been encrypted and stored securely
• The original message has been deleted
• Never share your private key with anyone

🚀 *Next Steps:*
• Use /wallet to check your balances
• Fund your wallet with MATIC for gas fees
• Use /markets to start trading

_Note: A Gnosis Safe proxy wallet will be deployed for trading when you make your first transaction._
`, userWallet.EOAAddress.Hex())

	b.sendMessage(chatID, message)
}

// handleCreateWallet handles wallet creation callback
func (b *Bot) handleCreateWallet(ctx context.Context, update *tgbotapi.Update) {
	userID := update.CallbackQuery.From.ID
	chatID := update.CallbackQuery.Message.Chat.ID

	// Check if user already exists
	existingUser, err := b.userRepo.GetByTelegramID(ctx, userID)
	if err != nil {
		b.sendMessage(chatID, "❌ Failed to check existing wallet. Please try again.")
		log.Printf("Failed to check existing user %d: %v", userID, err)
		return
	}

	if existingUser != nil {
		b.sendMessage(chatID, "⚠️ You already have a wallet set up.")
		return
	}

	// Generate new wallet
	newWallet, err := b.walletManager.GenerateNewWallet()
	if err != nil {
		b.sendMessage(chatID, "❌ Failed to generate wallet. Please try again.")
		log.Printf("Failed to generate wallet for user %d: %v", userID, err)
		return
	}

	// Encrypt the private key for storage
	encryptedKey, err := b.walletManager.EncryptPrivateKey(newWallet)
	if err != nil {
		b.sendMessage(chatID, "❌ Failed to secure wallet. Please try again.")
		log.Printf("Failed to encrypt key for user %d: %v", userID, err)
		return
	}

	// Get username
	username := update.CallbackQuery.From.UserName
	if username == "" {
		username = update.CallbackQuery.From.FirstName
	}

	// Create user in database
	newUser := &database.User{
		TelegramID:   userID,
		Username:     username,
		EOAAddress:   newWallet.EOAAddress.Hex(),
		ProxyAddress: "", // Will be set when Gnosis Safe is deployed
		EncryptedKey: encryptedKey,
		Settings:     database.JSONB{},
		IsActive:     true,
	}

	if err := b.userRepo.Create(ctx, newUser); err != nil {
		b.sendMessage(chatID, "❌ Failed to save wallet. Please try again.")
		log.Printf("Failed to create user %d: %v", userID, err)
		return
	}

	// Success message
	message := fmt.Sprintf(`
✅ *New Wallet Created Successfully!*

Your new wallet has been generated and encrypted.

📍 *EOA Address:* `+"`%s`"+`

⚠️ *Important:*
• Your private key has been generated and encrypted
• Keep your wallet address safe
• Fund your wallet with MATIC for gas fees

🚀 *Next Steps:*
• Use /wallet to check your balances
• Fund your wallet with USDC to start trading
• Use /markets to view available markets

_Note: A Gnosis Safe proxy wallet will be deployed for trading when you make your first transaction._
`, newWallet.EOAAddress.Hex())

	b.sendMessage(chatID, message)
}

// handleRefresh handles the /refresh command to update proxy wallet
func (b *Bot) handleRefresh(ctx context.Context, bot *Bot, update *tgbotapi.Update) error {
	userID := update.Message.From.ID
	chatID := update.Message.Chat.ID

	// Get user from database
	user, err := b.userRepo.GetByTelegramID(ctx, userID)
	if err != nil {
		return fmt.Errorf("failed to get user: %w", err)
	}

	if user == nil {
		b.sendMessage(chatID, "❌ You don't have a wallet set up. Use /start to create one.")
		return nil
	}

	// Check if already has proxy
	if user.ProxyAddress != "" {
		b.sendMessage(chatID, fmt.Sprintf("✅ Your proxy wallet is already set:\n`%s`", user.ProxyAddress))
		return nil
	}

	b.sendMessage(chatID, "🔄 Checking for proxy wallet...")

	eoaAddress := common.HexToAddress(user.EOAAddress)
	var proxyAddress string

	// First try Polymarket's API
	if b.proxyResolver != nil {
		if proxy, err := b.proxyResolver.GetProxyWallet(ctx, eoaAddress); err == nil && proxy.Hex() != "0x0000000000000000000000000000000000000000" {
			proxyAddress = proxy.Hex()
			log.Printf("Found proxy wallet via Polymarket API: %s for EOA %s", proxyAddress, eoaAddress.Hex())
		}
	}

	// If not found via API and we have blockchain client, check on-chain
	if proxyAddress == "" && b.blockchain != nil {
		detector := blockchain.NewGnosisSafeDetector(b.blockchain.GetClient())
		if proxy, err := detector.FindProxyWallet(ctx, eoaAddress); err == nil && proxy.Hex() != "0x0000000000000000000000000000000000000000" {
			proxyAddress = proxy.Hex()
			log.Printf("Found proxy wallet on-chain: %s for EOA %s", proxyAddress, eoaAddress.Hex())
		}
	}

	// Update if found
	if proxyAddress != "" {
		user.ProxyAddress = proxyAddress
		if err := b.userRepo.Update(ctx, user); err != nil {
			b.sendMessage(chatID, "❌ Found proxy wallet but failed to save. Please try again.")
			return fmt.Errorf("failed to update user proxy: %w", err)
		}

		message := fmt.Sprintf(`
✅ *Proxy Wallet Found!*

Your Gnosis Safe proxy wallet has been detected and linked:

🏦 *Proxy Address:*
`+"`%s`"+`

You can now trade on Polymarket using this proxy wallet.
`, proxyAddress)

		b.sendMessage(chatID, message)
	} else {
		message := `
❌ *No Proxy Wallet Found*

No Gnosis Safe proxy wallet was detected for your EOA.

This could mean:
• You haven't traded on Polymarket yet
• The proxy hasn't been deployed yet
• The detection failed

A proxy wallet will be automatically created when you make your first trade on Polymarket.

You can try /refresh again later to check for updates.
`
		b.sendMessage(chatID, message)
	}

	return nil
}