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
	"github.com/Catorpilor/poly/internal/live"
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
	scanner := polymarket.NewUnifiedPositionScanner()
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
			"• *SL:* trailing — activates once bid ≥ entry × %.2f, then\n"+
			"  stop = max(entry, peak × %.2f) → sell 100%%\n\n"+
			"Tap a position to arm or disarm.\n\n",
		len(positions),
		database.TPMultiplier,
		database.TPSellFraction*100,
		database.SLActivationMult,
		database.SLTrailMult,
	)

	var rows [][]tgbotapi.InlineKeyboardButton
	for i, pos := range positions {
		if i >= 8 {
			header += fmt.Sprintf("\n_...and %d more positions_", len(positions)-8)
			break
		}
		rows = append(rows, sltpRowForPosition(i, pos, armed[pos.TokenID]))
		// If armed, expose the lottery-ticket toggle on a second row.
		if armed[pos.TokenID] != nil {
			rows = append(rows, sltpLotteryRow(armed[pos.TokenID]))
		}
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
		// Key the disarm button on the arm's stable DB ID, not the position
		// index. The index is meaningless once the cached position list expires
		// (10 min TTL) or a buffered tap arrives late — which silently dropped
		// disarms into "Session expired". The arm ID resolves from the DB
		// regardless of UI state, so disarm always hits the right row.
		return tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData(truncateUTF8(label, 60), fmt.Sprintf("sltp:off:%d", existing.ID)),
		)
	}

	label := fmt.Sprintf("🎯 Arm: %s - %s %s", title, sharesStr, pos.Outcome)
	return tgbotapi.NewInlineKeyboardRow(
		tgbotapi.NewInlineKeyboardButtonData(truncateUTF8(label, 60), fmt.Sprintf("sltp:arm:%d", i)),
	)
}

// sltpLotteryRow renders the per-arm lottery toggle. Tapping it flips the
// lottery_ticket_armed flag in the DB. Visible only for armed positions.
// Keyed on the arm's stable DB ID (not the position index) for the same
// reason as the disarm button — see sltpRowForPosition.
func sltpLotteryRow(arm *database.SLTPArm) []tgbotapi.InlineKeyboardButton {
	state := "OFF"
	if arm.LotteryTicketArmed {
		state = "ON ✓"
	}
	label := fmt.Sprintf("🎫 Lottery (other side @ ≤$%.2f): %s", database.LotteryMaxPrice, state)
	return tgbotapi.NewInlineKeyboardRow(
		tgbotapi.NewInlineKeyboardButtonData(truncateUTF8(label, 60), fmt.Sprintf("sltp:lot:%d", arm.ID)),
	)
}

// handleSLTPLotteryCallback flips the lottery_ticket_armed flag for the
// selected armed position and re-renders the list so the toggle reflects new
// state. Invoked from the `sltp:lot:<idx>` callback.
func (b *Bot) handleSLTPLotteryCallback(ctx context.Context, update *tgbotapi.Update) {
	userID := update.CallbackQuery.From.ID
	chatID := update.CallbackQuery.Message.Chat.ID
	messageID := update.CallbackQuery.Message.MessageID

	arm, err := b.resolveSLTPArm(ctx, update)
	if err != nil {
		log.Printf("SLTP lottery: resolve arm for %d: %v", userID, err)
		b.editMessage(chatID, messageID, "❌ Lottery toggle failed. Tap 🎯 SL/TP again.")
		return
	}
	if arm == nil {
		b.editMessage(chatID, messageID, "❌ Position is not armed. Arm it first.")
		return
	}

	target := !arm.LotteryTicketArmed
	if err := b.sltpArmRepo.SetLotteryTicket(ctx, userID, arm.TokenID, target); err != nil {
		if errors.Is(err, repositories.ErrSLTPArmNotFound) {
			b.editMessage(chatID, messageID, "❌ Position is not armed. Arm it first.")
			return
		}
		log.Printf("SLTP lottery: set for %d/%s: %v", userID, arm.TokenID, err)
		b.editMessage(chatID, messageID, fmt.Sprintf("❌ Lottery toggle failed: %v", err))
		return
	}

	state := "OFF"
	if target {
		state = "ON"
	}
	b.sendMessage(chatID, fmt.Sprintf(
		"🎫 *Lottery ticket: %s* for %s\n\n"+
			"When the ceiling-TP fires (bid ≥ $%.2f), the bot will FOK-buy the other side at ≤ $%.2f, capped at $%.2f.",
		state, b.sltpArmDisplay(userID, arm), database.CeilingTPPrice, database.LotteryMaxPrice, database.LotteryMaxSpend,
	))

	b.handleSLTPList(ctx, update)
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
		Outcome:     normalizeOutcome(pos.Outcome),
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

	b.sendMessage(chatID, sltpArmedText(pos.MarketTitle, pos.Outcome, saved))

	// Re-render the list so the button flips to disarm.
	b.handleSLTPList(ctx, update)
}

// handleSLTPDisarmCallback clears a user's arm for the selected position. It
// resolves the arm from its stable DB ID (carried in the callback data), so a
// disarm reliably takes effect even when the cached position list has expired
// or the tap was buffered by Telegram and delivered late — the failure mode
// that previously dropped disarms into a silent "Session expired" while the
// position stayed armed and auto-sold.
func (b *Bot) handleSLTPDisarmCallback(ctx context.Context, update *tgbotapi.Update) {
	userID := update.CallbackQuery.From.ID
	chatID := update.CallbackQuery.Message.Chat.ID
	messageID := update.CallbackQuery.Message.MessageID

	arm, err := b.resolveSLTPArm(ctx, update)
	if err != nil {
		log.Printf("SLTP disarm: resolve arm for %d: %v", userID, err)
		b.editMessage(chatID, messageID, "❌ Disarm failed: couldn't identify the position. Tap 🎯 SL/TP again.")
		return
	}
	if arm == nil {
		// No armed row for this ID/user — already disarmed (e.g. a double-tap, or
		// the arm fired and cleared in the meantime). Treat as idempotent success
		// rather than a scary error.
		b.sendMessage(chatID, "⏹ *Already disarmed* — this position is no longer being auto-watched.")
		b.handleSLTPList(ctx, update)
		return
	}

	if err := b.sltpArmRepo.Disarm(ctx, userID, arm.TokenID); err != nil &&
		!errors.Is(err, repositories.ErrSLTPArmNotFound) {
		log.Printf("SLTP disarm: %d/%s: %v", userID, arm.TokenID, err)
		b.editMessage(chatID, messageID, fmt.Sprintf("❌ Disarm failed: %v", err))
		return
	}

	if b.sltpMonitor != nil {
		b.sltpMonitor.UnsubscribeFor(arm.TokenID)
	}

	b.sendMessage(chatID, fmt.Sprintf("⏹ *Disarmed* %s", b.sltpArmDisplay(userID, arm)))
	b.handleSLTPList(ctx, update)
}

// resolveSLTPArm parses the arm DB ID from callback data (sltp:off:<id> or
// sltp:lot:<id>) and loads the arm directly from the DB, scoped to the calling
// user. Unlike resolveSLTPPosition it does NOT depend on the cached position
// list, so it works even after the UI state has expired or a tap is delivered
// late. Returns (nil, nil) when no matching armed row exists (already disarmed).
func (b *Bot) resolveSLTPArm(ctx context.Context, update *tgbotapi.Update) (*database.SLTPArm, error) {
	userID := update.CallbackQuery.From.ID
	parts := strings.Split(update.CallbackQuery.Data, ":")
	if len(parts) != 3 {
		return nil, fmt.Errorf("malformed sltp callback %q", update.CallbackQuery.Data)
	}
	id, err := strconv.Atoi(parts[2])
	if err != nil {
		return nil, fmt.Errorf("bad arm id %q: %w", parts[2], err)
	}
	return b.sltpArmRepo.GetByID(ctx, userID, id)
}

// sltpArmDisplay returns a human label for an arm. It prefers the market title
// from the cached position list for nicer UX, but falls back to the outcome
// alone so confirmations never depend on that cache being present.
func (b *Bot) sltpArmDisplay(userID int64, arm *database.SLTPArm) string {
	if st, ok := b.stateManager.GetState(userID); ok {
		if positions, ok := st.Data["positions"].([]*polymarket.Position); ok {
			for _, p := range positions {
				if p.TokenID == arm.TokenID {
					return fmt.Sprintf("%s %s", p.MarketTitle, p.Outcome)
				}
			}
		}
	}
	return string(arm.Outcome)
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

// normalizeOutcome upper-cases an outcome string so it matches the
// database.Outcome enum. The unified position scanner emits "Yes"/"No"
// (display casing); SLTPArm.Validate requires "YES"/"NO".
func normalizeOutcome(s string) database.Outcome {
	return database.Outcome(strings.ToUpper(s))
}

// --- live.TradeExecutor / live.Notifier adapters ---

// ExecuteSell implements live.TradeExecutor. It resolves the user's wallet and
// reuses the existing sell-from-position path, synthesizing a minimal Position
// from the arm's snapshot. limitPrice=0 makes this a market order with the same
// 2% slippage guard that manual sells use.
func (b *Bot) ExecuteSell(ctx context.Context, arm *database.SLTPArm, sharesRaw int64,
	limitPrice float64, orderType polymarket.OrderType) *polymarket.TradeResult {
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

	return b.executeSellOrderFromPosition(ctx, user, pos, 0, sharesRaw, limitPrice, orderType)
}

// NotifySLExitPending implements live.Notifier. Sent at most once per breach
// episode when the floored FOK exit can't fill; the monitor keeps retrying
// while the price stays below the stop.
func (b *Bot) NotifySLExitPending(telegramID int64, arm *database.SLTPArm, bid, trigger, floor float64) {
	text := fmt.Sprintf(
		"⏳ *Trailing stop hit — exit pending*\n\n"+
			"*%s* bid $%.3f is below your stop $%.3f, but the book is too thin "+
			"to sell at ≥ $%.3f (floor).\n\n"+
			"Holding and retrying while the price stays below the stop — "+
			"never selling below the floor.",
		arm.Outcome, bid, trigger, floor)
	b.sendMessage(telegramID, text)
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
	b.sendMessage(telegramID, sltpFiredText(kind, arm, bid, result))
}

// sltpFiredText builds the fire notification body. Pure — table-tested.
// SL failures never reach the failure branch: the monitor sends the pending
// notice instead and keeps the arm; the branch remains for TP kinds.
func sltpFiredText(kind string, arm *database.SLTPArm, bid float64, result *polymarket.TradeResult) string {
	if result == nil || !result.Success {
		errMsg := "(no result)"
		if result != nil && result.ErrorMsg != "" {
			errMsg = result.ErrorMsg
		}
		return fmt.Sprintf(
			"⚠️ *%s trigger fired* at $%.4f but sell failed:\n`%s`\n\n"+
				"Position remains unsold. Check /positions.",
			kind, bid, errMsg,
		)
	}
	switch kind {
	case "TP":
		// By TP time the bid reached 2× entry, so the trailing stop is
		// necessarily active; show where it protects the remainder.
		return fmt.Sprintf(
			"✅ *TP hit* at $%.4f\n\n"+
				"Sold %.0f%% of %s position.\n"+
				"Trailing stop ($%.4f, follows the peak) watching the remainder.",
			bid, database.TPSellFraction*100, arm.Outcome, arm.SLTriggerPrice(),
		)
	case "TP-ceiling":
		return fmt.Sprintf(
			"🏁 *TP ceiling hit* at $%.4f (≥ $%.2f)\n\n"+
				"Sold remaining %s shares — locking in upside, no point holding for the last few cents.\n"+
				"Position fully disarmed.",
			bid, database.CeilingTPPrice, arm.Outcome,
		)
	case "SL":
		return fmt.Sprintf(
			"🛑 *Trailing stop hit* at $%.4f\n\n"+
				"Peak was $%.4f, stop $%.4f — sold remaining %s shares at ≥ $%.4f (FOK floor).\n"+
				"Position fully disarmed.",
			bid, arm.HighWaterMark, arm.SLTriggerPrice(), arm.Outcome, arm.SLFloorPrice(),
		)
	default:
		return fmt.Sprintf("ℹ️ %s fired at $%.4f", kind, bid)
	}
}

// sltpArmedText builds the arm-confirmation message. Pure — table-tested.
func sltpArmedText(title, outcome string, arm *database.SLTPArm) string {
	activation := arm.AvgPrice * database.SLActivationMult
	return fmt.Sprintf(
		"🎯 *Armed* %s %s\n\n"+
			"• Entry: $%.4f\n"+
			"• TP: bid ≥ $%.4f → sell %.0f%%\n"+
			"• SL: trailing — wakes once bid ≥ $%.4f, then stops at\n"+
			"  max(entry, peak − 20%%) → sell 100%% (FOK, floored)\n\n"+
			"⚠️ No stop until the bid reaches $%.4f — max loss is your stake.",
		title, outcome,
		arm.AvgPrice,
		arm.TPTriggerPrice(), database.TPSellFraction*100,
		activation, activation,
	)
}

// ResolveOtherToken implements live.TradeExecutor. Returns the second CTF
// token in the binary market that contains arm.TokenID. For markets with
// != 2 outcomes (multi-outcome / neg-risk), returns live.ErrMultiOutcome.
func (b *Bot) ResolveOtherToken(ctx context.Context, arm *database.SLTPArm) (string, string, error) {
	market, err := b.tradingClient.GetMarketInfo(ctx, arm.TokenID)
	if err != nil {
		return "", "", fmt.Errorf("get market info: %w", err)
	}
	if len(market.Tokens) != 2 {
		return "", "", live.ErrMultiOutcome
	}
	for _, tok := range market.Tokens {
		if tok.TokenID != arm.TokenID {
			return tok.TokenID, tok.Outcome, nil
		}
	}
	// Both tokens equal to arm.TokenID would be a server-side data bug.
	return "", "", fmt.Errorf("market has no token differing from arm token %s", arm.TokenID)
}

// ExecuteLotteryBuy implements live.TradeExecutor. Submits a FOK BUY of
// otherTokenID at price <= maxPrice for at most maxSpend USDC.
func (b *Bot) ExecuteLotteryBuy(ctx context.Context, arm *database.SLTPArm,
	otherTokenID, otherOutcome string, maxSpend, maxPrice float64) *polymarket.TradeResult {

	user, err := b.userRepo.GetByTelegramID(ctx, arm.TelegramID)
	if err != nil || user == nil {
		return &polymarket.TradeResult{
			Success:  false,
			ErrorMsg: fmt.Sprintf("user not found: %v", err),
		}
	}

	userWallet, err := b.walletManager.DecryptPrivateKey(user.EncryptedKey)
	if err != nil {
		return &polymarket.TradeResult{
			Success:  false,
			ErrorMsg: fmt.Sprintf("wallet decrypt: %v", err),
		}
	}

	creds, err := b.tradingClient.GetOrCreateAPICredentials(ctx, userWallet.PrivateKey)
	if err != nil {
		return &polymarket.TradeResult{
			Success:  false,
			ErrorMsg: fmt.Sprintf("get api creds: %v", err),
		}
	}

	proxyAddress := common.HexToAddress(user.ProxyAddress)

	orderFeeBps := 0
	if feeRate, err := b.tradingClient.GetFeeRate(ctx, otherTokenID); err == nil {
		orderFeeBps = feeRate
	}

	tradeReq := &polymarket.TradeRequest{
		// MarketID isn't strictly required for the FOK BUY since the order
		// payload is built from TokenID + price; leave empty for simplicity.
		TokenID:      otherTokenID,
		Side:         "BUY",
		Outcome:      otherOutcome,
		Amount:       maxSpend,
		Price:        maxPrice,
		OrderType:    polymarket.OrderTypeFOK,
		NegativeRisk: arm.NegRisk, // same condition → same neg-risk
		TakerFeeBps:  orderFeeBps,
		CalcFeeBps:   0,
		AccountType:  user.AccountType,
	}

	log.Printf("Lottery BUY: user=%d arm=%d otherToken=%s outcome=%s spend<=%.2f price<=%.4f",
		arm.TelegramID, arm.ID, otherTokenID, otherOutcome, maxSpend, maxPrice)

	result, err := b.tradingClient.ExecuteTrade(ctx, userWallet.PrivateKey, proxyAddress, creds, tradeReq)
	if err != nil {
		return &polymarket.TradeResult{
			Success:  false,
			ErrorMsg: fmt.Sprintf("execute trade: %v", err),
		}
	}
	return result
}

// NotifyLottery implements live.Notifier. Always notifies — success, skip
// (with reason), or failure — so the user knows what happened on the lottery
// side after a ceiling-TP fire.
func (b *Bot) NotifyLottery(telegramID int64, arm *database.SLTPArm, otherOutcome string,
	reason string, detail string, result *polymarket.TradeResult) {

	var text string
	switch reason {
	case "filled":
		// result.AveragePrice may not be populated by the executor; fall back
		// to FilledSize description.
		shares := 0.0
		spent := 0.0
		if result != nil {
			shares = result.FilledSize
			if result.AveragePrice > 0 {
				spent = shares * result.AveragePrice
			}
		}
		if spent > 0 {
			text = fmt.Sprintf(
				"🎫 *Lottery filled* — bought %.2f %s shares @ $%.4f avg ($%.2f total).\n\n"+
					"If the improbable lands, this 5%% turns into 100%%.",
				shares, otherOutcome, result.AveragePrice, spent,
			)
		} else {
			text = fmt.Sprintf(
				"🎫 *Lottery filled* — bought %s side.\nOrder: `%s`",
				otherOutcome, result.OrderID,
			)
		}
	case "ask-too-high":
		text = fmt.Sprintf(
			"🎫 *Lottery skipped* — %s\n\n"+
				"The other side (%s) isn't cheap enough to be a fair lottery ticket.",
			detail, otherOutcome,
		)
	case "no-liquidity":
		text = fmt.Sprintf(
			"🎫 *Lottery skipped* — %s side has no live ask.",
			otherOutcome,
		)
	case "multi-outcome":
		text = "🎫 *Lottery skipped* — multi-outcome market, no single \"other side\" to buy."
	case "resolve-failed":
		text = fmt.Sprintf("🎫 *Lottery skipped* — couldn't resolve other token: %s", detail)
	case "failed":
		text = fmt.Sprintf(
			"🎫 *Lottery failed* — order rejected:\n`%s`",
			detail,
		)
	default:
		text = fmt.Sprintf("🎫 Lottery: %s — %s", reason, detail)
	}
	b.sendMessage(telegramID, text)
}
