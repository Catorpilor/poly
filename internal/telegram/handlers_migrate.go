package telegram

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/ethereum/go-ethereum/common"
	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"

	"github.com/Catorpilor/poly/internal/blockchain"
)

// handleMigrate handles the /migrate command — wraps any leftover USDC.e
// into pUSD and grants the V2 exchanges the approvals they need to move
// the user's collateral and outcome tokens. Idempotent: skips work that's
// already done. Submitted gas-less via the Polymarket Builder Relayer.
func (b *Bot) handleMigrate(ctx context.Context, _ *Bot, update *tgbotapi.Update) error {
	chatID := update.Message.Chat.ID
	userID := update.Message.From.ID

	user, err := b.userRepo.GetByTelegramID(ctx, userID)
	if err != nil {
		return fmt.Errorf("failed to get user: %w", err)
	}
	if user == nil {
		b.sendMessage(chatID, "❌ You need to set up a wallet first. Use /start.")
		return nil
	}
	if user.ProxyAddress == "" {
		b.sendMessage(chatID, "❌ No proxy wallet found. Trade once on Polymarket to create one.")
		return nil
	}
	if b.blockchain == nil {
		b.sendMessage(chatID, "❌ Blockchain client not configured.")
		return nil
	}
	if b.relayerClient == nil {
		b.sendMessage(chatID, "❌ Builder Relayer not configured. Cannot submit migration.")
		return nil
	}

	loadingMsg := b.sendMessageAndReturn(chatID, "🔄 *Checking V2 migration status...*")

	migrateCtx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()

	proxyAddress := common.HexToAddress(user.ProxyAddress)
	bc := blockchain.NewBalanceChecker(b.blockchain.GetClient())

	plan, err := blockchain.PlanV2Bootstrap(migrateCtx, bc, proxyAddress)
	if err != nil {
		b.editMessage(chatID, loadingMsg.MessageID, fmt.Sprintf("❌ Failed to plan migration: %v", err))
		return nil
	}

	if plan.IsEmpty() {
		b.editMessage(chatID, loadingMsg.MessageID, "✅ *Wallet is already V2-ready.* Nothing to migrate.")
		return nil
	}

	b.editMessage(chatID, loadingMsg.MessageID, fmt.Sprintf("⏳ *Migrating to V2...*\n\n%s", plan.Summary()))
	log.Printf("Migrate: proxy=%s, txs=%d, summary=%s", proxyAddress.Hex(), len(plan.Txs), plan.Summary())

	userWallet, err := b.walletManager.DecryptPrivateKey(user.EncryptedKey)
	if err != nil {
		b.editMessage(chatID, loadingMsg.MessageID, "❌ Failed to decrypt wallet.")
		return nil
	}
	eoaAddress := common.HexToAddress(user.EOAAddress)

	var (
		txHash   string
		execErr  error
	)
	if len(plan.Txs) == 1 {
		tx := plan.Txs[0]
		txHash, execErr = b.relayerClient.ExecSafeTransaction(migrateCtx, eoaAddress, proxyAddress, tx.To, tx.Data, userWallet.PrivateKey)
	} else {
		txHash, execErr = b.relayerClient.ExecMultiSendTransaction(migrateCtx, eoaAddress, proxyAddress, plan.Txs, userWallet.PrivateKey)
	}

	if execErr != nil {
		log.Printf("Migrate error: %v", execErr)
		b.editMessage(chatID, loadingMsg.MessageID, fmt.Sprintf("❌ Migration failed: %v", execErr))
		return nil
	}

	log.Printf("Migrate: SUCCESS tx=%s, ops=%d", txHash, len(plan.Txs))
	b.editMessage(chatID, loadingMsg.MessageID, fmt.Sprintf("✅ *V2 Migration Complete*\n\n%s\n\nTx: `%s`", plan.Summary(), txHash))
	return nil
}
