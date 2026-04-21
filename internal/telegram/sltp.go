package telegram

import (
	"context"
	"errors"
	"fmt"
	"log"
	"math/big"
	"strconv"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/common"
	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"

	"github.com/Catorpilor/poly/internal/database"
	"github.com/Catorpilor/poly/internal/database/repositories"
	"github.com/Catorpilor/poly/internal/polymarket"
)

// handleSLTPList renders the SL/TP view: one row per current position with an
// Arm/Disarm toggle. Positions armed for this user show a disarm button; others
// show an arm button.
func (b *Bot) handleSLTPList(ctx context.Context, update *tgbotapi.Update) {
	userID := update.CallbackQuery.From.ID
	chatID := update.CallbackQuery.Message.Chat.ID
	messageID := update.CallbackQuery.Message.MessageID

	user, err := b.userRepo.GetByTelegramID(ctx, userID)
	if err != nil || user == nil {
		b.editMessage(chatID, messageID, "❌ User not found. Please use /start to set up your wallet.")
		return
	}
	if user.ProxyAddress == "" {
		b.editMessage(chatID, messageID, "❌ No proxy wallet found. Please ensure you have traded on Polymarket.")
		return
	}

	b.editMessage(chatID, messageID, "🎯 *Loading SL/TP view...*")

	proxyAddr := common.HexToAddress(user.ProxyAddress)
	scanner := polymarket.NewUnifiedPositionScanner(nil)
	positions, err := scanner.GetPositions(ctx, proxyAddr)
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
		b.editMessageWithKeyboard(chatID, messageID, "📊 *No positions found*", keyboard)
		return
	}

	// Look up existing arms for this user, keyed by tokenID.
	armed := make(map[string]*database.SLTPArm)
	for _, pos := range positions {
		arm, err := b.sltpArmRepo.GetByUserAndToken(ctx, userID, pos.TokenID)
		if err != nil {
			log.Printf("SLTP list: repo lookup for %d/%s: %v", userID, pos.TokenID, err)
			continue
		}
		if arm != nil && (arm.TPArmed || arm.SLArmed) {
			armed[pos.TokenID] = arm
		}
	}

	header := fmt.Sprintf(
		"🎯 *SL/TP Auto-Sell* (%d positions)\n\n"+
			"• *TP:* bid ≥ entry × %.1f → sell %.0f%%\n"+
			"• *SL:* bid ≤ entry × %.2f → sell 100%%\n\n"+
			"Tap a position to arm or disarm.\n\n",
		len(positions),
		database.TPMultiplier,
		database.TPSellFraction*100,
		database.SLMultiplier,
	)

	var rows [][]tgbotapi.InlineKeyboardButton
	for i, pos := range positions {
		if i >= 8 {
			header += fmt.Sprintf("\n_...and %d more positions_", len(positions)-8)
			break
		}
		rows = append(rows, sltpRowForPosition(i, pos, armed[pos.TokenID]))
	}
	rows = append(rows, tgbotapi.NewInlineKeyboardRow(
		tgbotapi.NewInlineKeyboardButtonData("← Back to Positions", "back_to_positions"),
	))

	keyboard := tgbotapi.InlineKeyboardMarkup{InlineKeyboard: rows}
	b.editMessageWithKeyboard(chatID, messageID, header, keyboard)

	b.stateManager.SetState(userID, StateSelectingPosition, map[string]interface{}{
		"positions": positions,
	}, 10*time.Minute)
}

func sltpRowForPosition(i int, pos *polymarket.Position, existing *database.SLTPArm) []tgbotapi.InlineKeyboardButton {
	title := truncateUTF8(pos.MarketTitle, 22)
	sharesStr := polymarket.FormatShares(pos.Shares)

	if existing != nil {
		prefix := "⏹ Disarm"
		if existing.TPArmed && !existing.SLArmed {
			prefix = "⏹ Disarm (SL only gone)"
		} else if !existing.TPArmed && existing.SLArmed {
			prefix = "⏹ Disarm (SL only)"
		}
		label := fmt.Sprintf("%s: %s - %s %s", prefix, title, sharesStr, pos.Outcome)
		return tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData(truncateUTF8(label, 60), fmt.Sprintf("sltp:off:%d", i)),
		)
	}

	label := fmt.Sprintf("🎯 Arm: %s - %s %s", title, sharesStr, pos.Outcome)
	return tgbotapi.NewInlineKeyboardRow(
		tgbotapi.NewInlineKeyboardButtonData(truncateUTF8(label, 60), fmt.Sprintf("sltp:arm:%d", i)),
	)
}

// handleSLTPArmCallback arms TP+SL for the selected position.
func (b *Bot) handleSLTPArmCallback(ctx context.Context, update *tgbotapi.Update) {
	userID := update.CallbackQuery.From.ID
	chatID := update.CallbackQuery.Message.Chat.ID
	messageID := update.CallbackQuery.Message.MessageID

	pos, ok := b.resolveSLTPPosition(update)
	if !ok {
		b.editMessage(chatID, messageID, "❌ Session expired. Tap 🎯 SL/TP again.")
		return
	}

	if pos.AveragePrice <= 0 {
		b.editMessage(chatID, messageID,
			"❌ Cannot arm: no entry price known for this position.\n\n"+
				"This happens for positions acquired outside the bot. Try selling a tiny amount via /sell first to refresh.")
		return
	}

	sharesFloat := sharesBigIntToFloat(pos.Shares)
	if sharesFloat <= 0 {
		b.editMessage(chatID, messageID, "❌ Cannot arm: position has zero shares.")
		return
	}

	marketID := pos.MarketID
	arm := &database.SLTPArm{
		TelegramID:  userID,
		TokenID:     pos.TokenID,
		ConditionID: pos.ConditionID,
		MarketID:    &marketID,
		Outcome:     database.Outcome(pos.Outcome),
		AvgPrice:    pos.AveragePrice,
		SharesAtArm: sharesFloat,
		NegRisk:     pos.NegativeRisk,
	}

	// Check if this is a new arm so we only Subscribe on the first Arm call.
	existing, _ := b.sltpArmRepo.GetByUserAndToken(ctx, userID, pos.TokenID)

	saved, err := b.sltpArmRepo.Arm(ctx, arm)
	if err != nil {
		log.Printf("SLTP arm: %d/%s: %v", userID, pos.TokenID, err)
		b.editMessage(chatID, messageID, fmt.Sprintf("❌ Arm failed: %v", err))
		return
	}

	if existing == nil && b.sltpMonitor != nil {
		b.sltpMonitor.SubscribeFor(saved.TokenID)
	}

	msg := fmt.Sprintf(
		"🎯 *Armed* %s %s\n\n"+
			"• Entry: $%.4f\n"+
			"• TP: bid ≥ $%.4f → sell %.0f%%\n"+
			"• SL: bid ≤ $%.4f → sell 100%%",
		pos.MarketTitle, pos.Outcome,
		saved.AvgPrice,
		saved.TPTriggerPrice(), database.TPSellFraction*100,
		saved.SLTriggerPrice(),
	)
	b.sendMessage(chatID, msg)

	// Re-render the list so the button flips to disarm.
	b.handleSLTPList(ctx, update)
}

// handleSLTPDisarmCallback clears a user's arm for the selected position.
func (b *Bot) handleSLTPDisarmCallback(ctx context.Context, update *tgbotapi.Update) {
	userID := update.CallbackQuery.From.ID
	chatID := update.CallbackQuery.Message.Chat.ID
	messageID := update.CallbackQuery.Message.MessageID

	pos, ok := b.resolveSLTPPosition(update)
	if !ok {
		b.editMessage(chatID, messageID, "❌ Session expired. Tap 🎯 SL/TP again.")
		return
	}

	err := b.sltpArmRepo.Disarm(ctx, userID, pos.TokenID)
	if err != nil && !errors.Is(err, repositories.ErrSLTPArmNotFound) {
		log.Printf("SLTP disarm: %d/%s: %v", userID, pos.TokenID, err)
		b.editMessage(chatID, messageID, fmt.Sprintf("❌ Disarm failed: %v", err))
		return
	}

	if b.sltpMonitor != nil {
		b.sltpMonitor.UnsubscribeFor(pos.TokenID)
	}

	b.sendMessage(chatID, fmt.Sprintf("⏹ *Disarmed* %s %s", pos.MarketTitle, pos.Outcome))
	b.handleSLTPList(ctx, update)
}

// resolveSLTPPosition parses the callback data for a position index and pulls the
// corresponding position from state. Returns (nil, false) on any parse/state error.
func (b *Bot) resolveSLTPPosition(update *tgbotapi.Update) (*polymarket.Position, bool) {
	userID := update.CallbackQuery.From.ID
	parts := strings.Split(update.CallbackQuery.Data, ":")
	if len(parts) != 3 {
		return nil, false
	}
	idx, err := strconv.Atoi(parts[2])
	if err != nil {
		return nil, false
	}
	userCtx, exists := b.stateManager.GetState(userID)
	if !exists {
		return nil, false
	}
	positions, ok := userCtx.Data["positions"].([]*polymarket.Position)
	if !ok || idx < 0 || idx >= len(positions) {
		return nil, false
	}
	return positions[idx], true
}

// sharesBigIntToFloat converts a polymarket shares big.Int (6 decimal fixed-point)
// to a float share count.
func sharesBigIntToFloat(b *big.Int) float64 {
	if b == nil {
		return 0
	}
	f, _ := new(big.Float).Quo(new(big.Float).SetInt(b), big.NewFloat(1e6)).Float64()
	return f
}

// --- live.TradeExecutor / live.Notifier adapters ---

// ExecuteSell implements live.TradeExecutor. It resolves the user's wallet and
// reuses the existing sell-from-position path, synthesizing a minimal Position
// from the arm's snapshot. limitPrice=0 makes this a market order with the same
// 2% slippage guard that manual sells use.
func (b *Bot) ExecuteSell(ctx context.Context, arm *database.SLTPArm, sharesRaw int64) *polymarket.TradeResult {
	user, err := b.userRepo.GetByTelegramID(ctx, arm.TelegramID)
	if err != nil || user == nil {
		return &polymarket.TradeResult{
			Success:  false,
			ErrorMsg: fmt.Sprintf("user not found: %v", err),
		}
	}

	marketID := ""
	if arm.MarketID != nil {
		marketID = *arm.MarketID
	}
	pos := &polymarket.Position{
		MarketID:     marketID,
		TokenID:      arm.TokenID,
		ConditionID:  arm.ConditionID,
		Outcome:      string(arm.Outcome),
		Shares:       big.NewInt(sharesRaw),
		AveragePrice: arm.AvgPrice,
		NegativeRisk: arm.NegRisk,
	}

	return b.executeSellOrderFromPosition(ctx, user, pos, 0, sharesRaw, 0)
}

// NotifySLTPPaused implements live.Notifier. Sent once per user when the monitor
// enters the V2 cutover (or any other) pause window, so users know why their
// arms aren't firing.
func (b *Bot) NotifySLTPPaused(telegramID int64, arm *database.SLTPArm) {
	text := fmt.Sprintf(
		"⏸ *SL/TP monitoring paused*\n\n"+
			"The Polymarket V2 exchange cutover is in progress. "+
			"Auto-sells are suspended until %s UTC.\n\n"+
			"Your arms remain in place and will resume evaluating once the cutover completes.",
		"12:00", // end hour of the V2 cutover window
	)
	b.sendMessage(telegramID, text)
}

// NotifySLTPFired implements live.Notifier. Sends a Telegram DM describing the
// fire outcome.
func (b *Bot) NotifySLTPFired(telegramID int64, kind string, arm *database.SLTPArm, bid float64, result *polymarket.TradeResult) {
	var text string
	if result != nil && result.Success {
		switch kind {
		case "TP":
			text = fmt.Sprintf(
				"✅ *TP hit* at $%.4f\n\n"+
					"Sold %.0f%% of %s position.\n"+
					"SL (≤ $%.4f) still watching remainder.",
				bid, database.TPSellFraction*100, arm.Outcome, arm.SLTriggerPrice(),
			)
		case "SL":
			text = fmt.Sprintf(
				"🛑 *SL hit* at $%.4f\n\n"+
					"Sold remaining %s shares. Position fully disarmed.",
				bid, arm.Outcome,
			)
		default:
			text = fmt.Sprintf("ℹ️ %s fired at $%.4f", kind, bid)
		}
	} else {
		errMsg := "(no result)"
		if result != nil && result.ErrorMsg != "" {
			errMsg = result.ErrorMsg
		}
		text = fmt.Sprintf(
			"⚠️ *%s trigger fired* at $%.4f but sell failed:\n`%s`\n\n"+
				"Position remains unsold. Check /positions.",
			kind, bid, errMsg,
		)
	}
	b.sendMessage(telegramID, text)
}
