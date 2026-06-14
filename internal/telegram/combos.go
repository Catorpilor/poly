package telegram

import (
	"context"
	"fmt"
	"log"

	"github.com/Catorpilor/poly/internal/database"
	"github.com/Catorpilor/poly/internal/polymarket"
	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"

	"github.com/ethereum/go-ethereum/common"
)

// handleCombos handles the /combos command — a read-only view of the user's
// Polymarket combo (multi-leg RFQ) positions.
func (b *Bot) handleCombos(ctx context.Context, bot *Bot, update *tgbotapi.Update) error {
	userID := update.Message.From.ID

	user, err := b.userRepo.GetByTelegramID(ctx, userID)
	if err != nil {
		return fmt.Errorf("failed to get user: %w", err)
	}
	if user == nil {
		b.sendMessage(update.Message.Chat.ID,
			"❌ You need to set up a wallet first. Use /start to begin.")
		return nil
	}

	proxyAddr, ok := b.resolveProxyAddress(ctx, user)
	if !ok {
		b.sendMessage(update.Message.Chat.ID,
			"❌ Could not find your proxy wallet. Please ensure you have traded on Polymarket before.")
		return nil
	}

	client := polymarket.NewCombosClient(b.config.Polymarket.DataAPIUrl)
	combos, err := client.GetComboPositions(ctx, proxyAddr)
	if err != nil {
		log.Printf("combo positions fetch error: %v", err)
		b.sendMessage(update.Message.Chat.ID, "❌ Failed to fetch combo positions. Please try again.")
		return nil
	}

	message := polymarket.FormatComboPositions(combos)
	keyboard := tgbotapi.NewInlineKeyboardMarkup(
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("🔄 Refresh", "refresh_combos"),
		),
	)
	b.sendMessageWithKeyboard(update.Message.Chat.ID, message, keyboard)
	return nil
}

// handleRefreshCombos re-fetches combo positions and edits the existing message
// (the "refresh_combos" inline button).
func (b *Bot) handleRefreshCombos(ctx context.Context, update *tgbotapi.Update) {
	userID := update.CallbackQuery.From.ID
	chatID := update.CallbackQuery.Message.Chat.ID
	messageID := update.CallbackQuery.Message.MessageID

	user, err := b.userRepo.GetByTelegramID(ctx, userID)
	if err != nil || user == nil {
		b.editMessage(chatID, messageID, "❌ User not found. Please use /start to set up your wallet.")
		return
	}

	proxyAddr, ok := b.resolveProxyAddress(ctx, user)
	if !ok {
		b.editMessage(chatID, messageID, "❌ No proxy wallet found. Please ensure you have traded on Polymarket.")
		return
	}

	b.editMessage(chatID, messageID, "🔄 *Refreshing combo positions...*")

	client := polymarket.NewCombosClient(b.config.Polymarket.DataAPIUrl)
	combos, err := client.GetComboPositions(ctx, proxyAddr)
	if err != nil {
		log.Printf("combo positions fetch error: %v", err)
		b.editMessage(chatID, messageID, "❌ Failed to fetch combo positions. Please try again.")
		return
	}

	keyboard := tgbotapi.NewInlineKeyboardMarkup(
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("🔄 Refresh", "refresh_combos"),
		),
	)
	b.editMessageWithKeyboard(chatID, messageID, polymarket.FormatComboPositions(combos), keyboard)
}

// resolveProxyAddress returns the user's proxy wallet address, attempting a
// deterministic on-chain resolution (and persisting it) if not already stored.
func (b *Bot) resolveProxyAddress(ctx context.Context, user *database.User) (common.Address, bool) {
	if user.ProxyAddress == "" && b.blockchain != nil {
		deterministic := polymarket.NewDeterministicProxyResolver(b.blockchain.GetClient())
		eoaAddr := common.HexToAddress(user.EOAAddress)
		if proxy, err := deterministic.GetPolymarketProxy(ctx, eoaAddr); err == nil && proxy != (common.Address{}) {
			user.ProxyAddress = proxy.Hex()
			b.userRepo.Update(ctx, user)
		}
	}
	if user.ProxyAddress == "" {
		return common.Address{}, false
	}
	return common.HexToAddress(user.ProxyAddress), true
}
