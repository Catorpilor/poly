package telegram

import (
	"context"
	"fmt"
	"log"
	"math/big"
	"time"

	"github.com/ethereum/go-ethereum/common"
	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"github.com/Catorpilor/poly/internal/blockchain"
	"github.com/Catorpilor/poly/internal/polymarket"
)

// handleRedeem handles the /redeem command (entry point)
func (b *Bot) handleRedeem(ctx context.Context, bot *Bot, update *tgbotapi.Update) error {
	userID := update.Message.From.ID

	user, err := b.userRepo.GetByTelegramID(ctx, userID)
	if err != nil {
		return fmt.Errorf("failed to get user: %w", err)
	}
	if user == nil {
		b.sendMessage(update.Message.Chat.ID, "❌ You need to set up a wallet first. Use /start to begin.")
		return nil
	}

	if user.ProxyAddress == "" {
		b.sendMessage(update.Message.Chat.ID, "❌ No proxy wallet found. Please ensure you have traded on Polymarket.")
		return nil
	}

	loadingMsg := b.sendMessageAndReturn(update.Message.Chat.ID, "🎁 *Checking redeemable positions...*")

	proxyAddr := common.HexToAddress(user.ProxyAddress)
	scanner := polymarket.NewUnifiedPositionScanner(nil)
	positions, err := scanner.GetRedeemablePositions(ctx, proxyAddr)
	if err != nil {
		b.editMessage(update.Message.Chat.ID, loadingMsg.MessageID, fmt.Sprintf("❌ Failed to fetch redeemable positions: %v", err))
		return nil
	}

	if len(positions) == 0 {
		b.editMessage(update.Message.Chat.ID, loadingMsg.MessageID, "🎁 *No Redeemable Positions*\n\nYou don't have any resolved positions to claim.")
		return nil
	}

	message, keyboard := b.buildRedeemSummary(positions)
	b.editMessageWithKeyboard(update.Message.Chat.ID, loadingMsg.MessageID, message, keyboard)

	b.stateManager.SetState(userID, StateRedeemingPositions, map[string]interface{}{
		"positions": positions,
	}, 10*time.Minute)

	return nil
}

// handleRedeemPositions handles the "redeem_positions" callback button (from /positions view)
func (b *Bot) handleRedeemPositions(ctx context.Context, update *tgbotapi.Update) {
	userID := update.CallbackQuery.From.ID
	chatID := update.CallbackQuery.Message.Chat.ID
	messageID := update.CallbackQuery.Message.MessageID

	user, err := b.userRepo.GetByTelegramID(ctx, userID)
	if err != nil || user == nil {
		b.editMessage(chatID, messageID, "❌ User not found. Please use /start to set up your wallet.")
		return
	}

	if user.ProxyAddress == "" {
		b.editMessage(chatID, messageID, "❌ No proxy wallet found.")
		return
	}

	b.editMessage(chatID, messageID, "🎁 *Checking redeemable positions...*")

	proxyAddr := common.HexToAddress(user.ProxyAddress)
	scanner := polymarket.NewUnifiedPositionScanner(nil)
	positions, err := scanner.GetRedeemablePositions(ctx, proxyAddr)
	if err != nil {
		b.editMessage(chatID, messageID, fmt.Sprintf("❌ Failed to fetch redeemable positions: %v", err))
		return
	}

	if len(positions) == 0 {
		keyboard := tgbotapi.NewInlineKeyboardMarkup(
			tgbotapi.NewInlineKeyboardRow(
				tgbotapi.NewInlineKeyboardButtonData("← Back", "back_to_positions"),
			),
		)
		b.editMessageWithKeyboard(chatID, messageID, "🎁 *No Redeemable Positions*\n\nYou don't have any resolved positions to claim.", keyboard)
		return
	}

	message, keyboard := b.buildRedeemSummary(positions)
	b.editMessageWithKeyboard(chatID, messageID, message, keyboard)

	b.stateManager.SetState(userID, StateRedeemingPositions, map[string]interface{}{
		"positions": positions,
	}, 10*time.Minute)
}

// handleRedeemAll handles the "redeem_all" callback — executes redemption via Polymarket's Builder Relayer.
func (b *Bot) handleRedeemAll(ctx context.Context, update *tgbotapi.Update) {
	userID := update.CallbackQuery.From.ID
	chatID := update.CallbackQuery.Message.Chat.ID
	messageID := update.CallbackQuery.Message.MessageID

	// Retrieve positions from state
	userCtx, ok := b.stateManager.GetState(userID)
	if !ok || userCtx.State != StateRedeemingPositions {
		b.editMessage(chatID, messageID, "❌ Session expired. Please use /redeem again.")
		return
	}

	positions, ok := userCtx.Data["positions"].([]*polymarket.RedeemablePositionInfo)
	if !ok || len(positions) == 0 {
		b.editMessage(chatID, messageID, "❌ No positions found in session.")
		return
	}

	// Get user
	user, err := b.userRepo.GetByTelegramID(ctx, userID)
	if err != nil || user == nil {
		b.editMessage(chatID, messageID, "❌ User not found.")
		return
	}

	// Require relayer client
	if b.relayerClient == nil {
		b.editMessage(chatID, messageID, "❌ Builder Relayer not configured. Cannot execute redemptions.")
		return
	}

	// Timeout context for all relayer operations (3 min per position + buffer)
	redeemCtx, cancel := context.WithTimeout(ctx, time.Duration(len(positions)+1)*3*time.Minute)
	defer cancel()

	// Decrypt private key
	userWallet, err := b.walletManager.DecryptPrivateKey(user.EncryptedKey)
	if err != nil {
		b.editMessage(chatID, messageID, "❌ Failed to decrypt wallet.")
		return
	}

	proxyAddress := common.HexToAddress(user.ProxyAddress)
	eoaAddress := common.HexToAddress(user.EOAAddress)

	// Group positions by conditionID to avoid duplicate redemptions
	type redeemGroup struct {
		conditionID  string
		title        string
		negativeRisk bool
		totalPayout  float64
		positions    []*polymarket.RedeemablePositionInfo
	}
	groups := make(map[string]*redeemGroup)
	for _, pos := range positions {
		g, exists := groups[pos.ConditionID]
		if !exists {
			g = &redeemGroup{
				conditionID:  pos.ConditionID,
				title:        pos.Title,
				negativeRisk: pos.NegativeRisk,
			}
			groups[pos.ConditionID] = g
		}
		g.totalPayout += pos.EstPayout
		g.positions = append(g.positions, pos)
	}

	groupList := make([]*redeemGroup, 0, len(groups))
	for _, g := range groups {
		groupList = append(groupList, g)
	}

	total := len(groupList)
	log.Printf("Redeem: %d unique conditions to redeem for proxy %s (via relayer)", total, proxyAddress.Hex())

	// Build all sub-transactions upfront
	b.editMessage(chatID, messageID, "⏳ *Preparing redemption transactions...*")

	var multiSendTxs []blockchain.MultiSendTx
	var totalPayout float64
	var encodeErrors []string

	// Check NegRisk approval and include in batch if needed
	hasNegRisk := false
	for _, g := range groupList {
		if g.negativeRisk {
			hasNegRisk = true
			break
		}
	}

	if hasNegRisk && b.blockchain != nil {
		log.Printf("Redeem: checking NegRisk adapter approval...")
		balanceChecker := blockchain.NewBalanceChecker(b.blockchain.GetClient())
		approved, err := balanceChecker.IsApprovedForAll(redeemCtx, proxyAddress, blockchain.NegRiskAdapterAddress)
		if err != nil {
			log.Printf("Redeem: failed to check NegRisk approval: %v", err)
		} else if !approved {
			log.Printf("Redeem: including NegRisk approval in batch")
			approvalData, err := blockchain.EncodeSetApprovalForAll(blockchain.NegRiskAdapterAddress, true)
			if err != nil {
				b.editMessage(chatID, messageID, fmt.Sprintf("❌ Failed to encode approval: %v", err))
				return
			}
			multiSendTxs = append(multiSendTxs, blockchain.MultiSendTx{
				To:   blockchain.ConditionalTokensAddress,
				Data: approvalData,
			})
		} else {
			log.Printf("Redeem: NegRisk adapter already approved")
		}
	}

	for i, g := range groupList {
		title := truncateUTF8(g.title, 40)
		conditionID := common.HexToHash(g.conditionID)
		log.Printf("Redeem [%d/%d]: encoding %s (conditionID=%s, negRisk=%v)", i+1, total, title, g.conditionID, g.negativeRisk)

		var (
			target   common.Address
			calldata []byte
		)

		if g.negativeRisk {
			if b.blockchain != nil {
				balanceChecker := blockchain.NewBalanceChecker(b.blockchain.GetClient())
				amounts, err := b.getNegRiskAmounts(redeemCtx, balanceChecker, proxyAddress, g.positions)
				if err != nil {
					encodeErrors = append(encodeErrors, fmt.Sprintf("%s: %v", title, err))
					continue
				}
				target, calldata, err = blockchain.EncodeNegRiskRedemption(conditionID, amounts)
				if err != nil {
					encodeErrors = append(encodeErrors, fmt.Sprintf("%s: %v", title, err))
					continue
				}
			} else {
				encodeErrors = append(encodeErrors, fmt.Sprintf("%s: blockchain client needed for neg-risk", title))
				continue
			}
		} else {
			var err error
			target, calldata, err = blockchain.EncodeStandardRedemption(conditionID)
			if err != nil {
				encodeErrors = append(encodeErrors, fmt.Sprintf("%s: %v", title, err))
				continue
			}
		}

		multiSendTxs = append(multiSendTxs, blockchain.MultiSendTx{To: target, Data: calldata})
		totalPayout += g.totalPayout
	}

	if len(multiSendTxs) == 0 {
		b.stateManager.ClearState(userID)
		b.editMessage(chatID, messageID, b.buildRedeemResult(0, total, 0, nil, encodeErrors))
		return
	}

	// Execute: single tx for one call, MultiSend for multiple
	b.editMessage(chatID, messageID, fmt.Sprintf("⏳ *Claiming %d positions...*", total))
	log.Printf("Redeem: submitting %d sub-transactions via relayer", len(multiSendTxs))

	var txHash string
	var execErr error

	if len(multiSendTxs) == 1 {
		// Single call — no need for MultiSend overhead
		tx := multiSendTxs[0]
		txHash, execErr = b.relayerClient.ExecSafeTransaction(redeemCtx, eoaAddress, proxyAddress, tx.To, tx.Data, userWallet.PrivateKey)
	} else {
		// Batch via MultiSend
		txHash, execErr = b.relayerClient.ExecMultiSendTransaction(redeemCtx, eoaAddress, proxyAddress, multiSendTxs, userWallet.PrivateKey)
	}

	b.stateManager.ClearState(userID)

	if execErr != nil {
		log.Printf("Redeem error: %v", execErr)
		allErrors := append(encodeErrors, fmt.Sprintf("relayer: %v", execErr))
		b.editMessage(chatID, messageID, b.buildRedeemResult(0, total, 0, nil, allErrors))
		return
	}

	log.Printf("Redeem: SUCCESS tx=%s, positions=%d, payout=%.2f", txHash, total, totalPayout)
	b.editMessage(chatID, messageID, b.buildRedeemResult(total-len(encodeErrors), total, totalPayout, []string{txHash}, encodeErrors))
}

// getNegRiskAmounts fetches on-chain ERC1155 balances for neg-risk redemption.
func (b *Bot) getNegRiskAmounts(ctx context.Context, bc *blockchain.BalanceChecker, proxyAddress common.Address, positions []*polymarket.RedeemablePositionInfo) ([]*big.Int, error) {
	var yesTokenID, noTokenID *big.Int

	for _, pos := range positions {
		tokenID, ok := new(big.Int).SetString(pos.Asset, 0)
		if !ok {
			tokenID, ok = new(big.Int).SetString(pos.Asset, 10)
			if !ok {
				return nil, fmt.Errorf("invalid token ID: %s", pos.Asset)
			}
		}

		oppositeID, _ := new(big.Int).SetString(pos.OppositeAsset, 0)
		if oppositeID == nil {
			oppositeID, _ = new(big.Int).SetString(pos.OppositeAsset, 10)
		}

		if pos.Outcome == "Yes" {
			yesTokenID = tokenID
			if oppositeID != nil {
				noTokenID = oppositeID
			}
		} else {
			noTokenID = tokenID
			if oppositeID != nil {
				yesTokenID = oppositeID
			}
		}
	}

	yesBalance := big.NewInt(0)
	noBalance := big.NewInt(0)

	if yesTokenID != nil {
		bal, err := bc.GetERC1155Balance(ctx, proxyAddress, yesTokenID)
		if err != nil {
			return nil, fmt.Errorf("failed to get YES balance: %w", err)
		}
		yesBalance = bal
	}

	if noTokenID != nil {
		bal, err := bc.GetERC1155Balance(ctx, proxyAddress, noTokenID)
		if err != nil {
			return nil, fmt.Errorf("failed to get NO balance: %w", err)
		}
		noBalance = bal
	}

	return []*big.Int{yesBalance, noBalance}, nil
}

// buildRedeemSummary builds the summary message and keyboard for redeemable positions.
func (b *Bot) buildRedeemSummary(positions []*polymarket.RedeemablePositionInfo) (string, tgbotapi.InlineKeyboardMarkup) {
	message := fmt.Sprintf("🎁 *Redeemable Positions (%d)*\n\n", len(positions))

	var totalPayout float64
	for i, pos := range positions {
		if i >= 10 {
			message += fmt.Sprintf("\n_...and %d more_\n", len(positions)-10)
			break
		}

		title := truncateUTF8(pos.Title, 40)
		message += fmt.Sprintf("*%d.* %s — %.1f %s\n", i+1, title, pos.Size, pos.Outcome)
		if pos.EstPayout > 0 {
			message += fmt.Sprintf("   Est. payout: ~$%.2f\n", pos.EstPayout)
		}
		totalPayout += pos.EstPayout
	}

	message += fmt.Sprintf("\n*Total est. payout: ~$%.2f USDC*\n", totalPayout)
	message += "\n⚠️ This will submit transactions via Polymarket's relayer.\nNo gas fees required from your wallet."

	keyboard := tgbotapi.NewInlineKeyboardMarkup(
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("✅ Claim All", "redeem_all"),
			tgbotapi.NewInlineKeyboardButtonData("❌ Cancel", "back_to_positions"),
		),
	)

	return message, keyboard
}

// buildRedeemResult builds the final summary message after redemption.
func (b *Bot) buildRedeemResult(succeeded, total int, totalPayout float64, txHashes, errors []string) string {
	var message string

	if succeeded == total {
		message = "✅ *All Positions Claimed!*\n\n"
	} else if succeeded > 0 {
		message = fmt.Sprintf("⚠️ *Partially Claimed (%d/%d)*\n\n", succeeded, total)
	} else {
		message = "❌ *Redemption Failed*\n\n"
	}

	if succeeded > 0 {
		message += fmt.Sprintf("Claimed: %d/%d positions\n", succeeded, total)
		message += fmt.Sprintf("Est. payout: ~$%.2f USDC\n\n", totalPayout)

		message += "*Transactions:*\n"
		for _, hash := range txHashes {
			if len(hash) > 16 {
				shortHash := hash[:10] + "..." + hash[len(hash)-6:]
				message += fmt.Sprintf("• [%s](https://polygonscan.com/tx/%s)\n", shortHash, hash)
			} else {
				message += fmt.Sprintf("• %s\n", hash)
			}
		}
	}

	if len(errors) > 0 {
		message += "\n*Errors:*\n"
		for _, e := range errors {
			message += fmt.Sprintf("• %s\n", truncateUTF8(e, 60))
		}
	}

	message += "\nUse /wallet to check your balance."
	return message
}
