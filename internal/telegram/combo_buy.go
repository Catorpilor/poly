package telegram

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"

	"github.com/Catorpilor/poly/internal/blockchain"
	"github.com/Catorpilor/poly/internal/polymarket"
)

// Combo buy flow (issue #118, ADR 0010).
const (
	// comboMultiplierTolerance is the Multiplier Tolerance: a fresh quote is
	// Taken only if its multiplier is at least the displayed one × (1 − 5%).
	comboMultiplierTolerance = 0.05

	comboPollEveryDefault      = 2 * time.Second
	comboInlineWaitDefault     = 30 * time.Second
	comboBackgroundWaitDefault = 10 * time.Minute

	comboStakeInputTTL = 5 * time.Minute
)

// errComboRateLimited marks a rate-limited quote (see comboRateLimited).
var errComboRateLimited = errors.New("combo quote rate limited")

func comboRateLimited(err error) bool {
	return errors.Is(err, errComboRateLimited) || polymarket.IsComboRateLimited(err)
}

// comboUserKey scopes per-user state to one Combo Link.
type comboUserKey struct {
	userID int64
	key    string
}

// setComboDisplayed records the multiplier now on the user's screen for a
// link (0 = no quote shown). The Multiplier Tolerance is measured from it.
func (b *Bot) setComboDisplayed(userID int64, key string, q *polymarket.ComboQuote, err error) {
	mult := 0.0
	if err == nil && !q.NoQuote() {
		mult = q.Multiplier()
	}
	b.comboStateMu.Lock()
	defer b.comboStateMu.Unlock()
	if b.comboDisplayedMult == nil {
		b.comboDisplayedMult = make(map[comboUserKey]float64)
	}
	b.comboDisplayedMult[comboUserKey{userID, key}] = mult
}

func (b *Bot) comboDisplayed(userID int64, key string) float64 {
	b.comboStateMu.Lock()
	defer b.comboStateMu.Unlock()
	return b.comboDisplayedMult[comboUserKey{userID, key}]
}

// markComboPending claims the link for one buy; false if one is already in
// flight or Pending (ADR 0010: no second buy of the same Combo meanwhile).
func (b *Bot) markComboPending(userID int64, key string) bool {
	b.comboStateMu.Lock()
	defer b.comboStateMu.Unlock()
	if b.comboPending == nil {
		b.comboPending = make(map[comboUserKey]bool)
	}
	k := comboUserKey{userID, key}
	if b.comboPending[k] {
		return false
	}
	b.comboPending[k] = true
	return true
}

func (b *Bot) clearComboPending(userID int64, key string) {
	b.comboStateMu.Lock()
	defer b.comboStateMu.Unlock()
	delete(b.comboPending, comboUserKey{userID, key})
}

func (b *Bot) comboIsPending(userID int64, key string) bool {
	b.comboStateMu.Lock()
	defer b.comboStateMu.Unlock()
	return b.comboPending[comboUserKey{userID, key}]
}

// comboBuy runs one stake tap: balance check → fresh quote → Take within
// the Multiplier Tolerance (one automatic retry after a refusal) → Fill,
// Failed or Pending.
func (b *Bot) comboBuy(ctx context.Context, userID, chatID int64, msgID int, key string, stake float64) {
	param, ok := b.lookupComboLink(key)
	if !ok {
		b.showComboScreen(chatID, msgID, comboExpiredText, nil)
		return
	}
	if !b.markComboPending(userID, key) {
		b.comboNotify(chatID, "⏳ You already have a pending buy on this combo — wait for it to settle.")
		return
	}
	keepPending := false
	defer func() {
		if !keepPending {
			b.clearComboPending(userID, key)
		}
	}()

	b.showComboScreen(chatID, msgID, fmt.Sprintf("⏳ Buying $%s of this combo…", formatStake(stake)), nil)

	lc, text, kb := b.loadCombo(ctx, param)
	if lc == nil {
		b.showComboScreen(chatID, msgID, text, kb)
		return
	}
	if lc.verdict != polymarket.ComboBuyable {
		b.showComboScreen(chatID, msgID, comboLegsText(lc.view)+comboVerdictText(lc), nil)
		return
	}
	show := func(quoteLine, notice string) {
		t, k := b.renderBuyableCombo(lc, quoteLine, notice)
		b.showComboScreen(chatID, msgID, t, k)
	}

	acct, err := b.loadComboAccount(ctx, userID)
	if err != nil {
		show(comboQuoteLine(nil, err), "")
		return
	}
	balance, err := b.comboBalance(ctx, acct)
	if err != nil {
		show("", "⚠️ Couldn't read your pUSD balance: "+escapeMarkdown(err.Error()))
		return
	}
	if need := int64(math.Round(stake * 1e6)); balance < need {
		show("", fmt.Sprintf("❌ Not enough pUSD: you have $%.2f, this stake needs $%s.", float64(balance)/1e6, formatStakeCents(stake)))
		return
	}

	retried := ""
	for attempt := 0; attempt < 2; attempt++ {
		q, err := b.comboQuote(ctx, acct, lc.view.LegPositionIDs(), stake)
		if err != nil || q.NoQuote() {
			show(comboQuoteLine(q, err), retried+"Nothing was bought.")
			return
		}
		ref := b.comboDisplayed(userID, key)
		if ref <= 0 || q.Multiplier() < ref*(1-comboMultiplierTolerance) {
			b.setComboDisplayed(userID, key, q, nil)
			show(comboQuoteLine(q, nil), fmt.Sprintf("%s👆 New quote for $%s: *x%.2f*. Tap $%s again to buy at this price.",
				retried, formatStake(stake), q.Multiplier(), formatStake(stake)))
			return
		}

		res, err := b.comboTake(ctx, acct, q)
		if err != nil {
			if polymarket.ComboRFQHTTPStatus(err) != 0 {
				// The gateway rejected the order outright: nothing executed.
				show(comboQuoteLine(q, nil), "❌ Combo buy rejected: "+escapeMarkdown(err.Error())+". Nothing was bought.")
				return
			}
			// No response: the Take may have been recorded. Treat it as
			// Pending and let the status poll find out (ADR 0010).
			res = &polymarket.ComboTakeResult{RFQID: q.RFQID, Status: "UNKNOWN"}
		}
		switch {
		case res.Filled():
			b.showComboScreen(chatID, msgID, comboFilledText(lc, q, res), nil)
			return
		case res.Terminal() && attempt == 0:
			retried = "The first quote was refused (" + escapeMarkdown(comboRefusal(res)) + "); retried once. "
			continue
		case res.Terminal():
			show(comboQuoteLine(q, nil), "❌ Combo buy failed: "+escapeMarkdown(comboRefusal(res))+". Nothing was bought.")
			return
		}

		keepPending = true
		b.followComboTake(ctx, acct, chatID, msgID, lc, q, res.RFQID)
		return
	}
}

// followComboTake polls a Pending Take: inline for comboInlineWait, then in
// the background for comboBackgroundWait, editing the message on a terminal
// status. It never reports failure without one (ADR 0010) and releases the
// Pending block when done.
func (b *Bot) followComboTake(ctx context.Context, acct *comboAccount, chatID int64, msgID int, lc *loadedCombo, q *polymarket.ComboQuote, rfqID string) {
	every := durationOr(b.comboPollEvery, comboPollEveryDefault)
	inline := durationOr(b.comboInlineWait, comboInlineWaitDefault)
	background := durationOr(b.comboBackgroundWait, comboBackgroundWaitDefault)

	finish := func(res *polymarket.ComboTakeResult) {
		if res.Filled() {
			b.showComboScreen(chatID, msgID, comboFilledText(lc, q, res), nil)
		} else {
			t, k := b.renderBuyableCombo(lc, comboQuoteLine(q, nil), "❌ Combo buy failed: "+escapeMarkdown(comboRefusal(res))+". Nothing was bought.")
			b.showComboScreen(chatID, msgID, t, k)
		}
		b.clearComboPending(acct.userID, lc.key)
	}

	if res := b.pollComboTake(ctx, acct, rfqID, every, inline); res != nil {
		finish(res)
		return
	}
	b.showComboScreen(chatID, msgID, comboLegsText(lc.view)+
		"⏳ *Pending* — the market maker hasn't settled this buy yet. I'll update this message when it does.\n"+
		"Don't buy this combo again until then.", nil)

	go func() {
		if res := b.pollComboTake(ctx, acct, rfqID, every, background); res != nil {
			finish(res)
			return
		}
		b.showComboScreen(chatID, msgID, comboLegsText(lc.view)+fmt.Sprintf(
			"⚠️ Still no final status after %s — the outcome is *unknown*. Check /positions before buying this combo again.", background), nil)
		b.clearComboPending(acct.userID, lc.key)
	}()
}

// pollComboTake polls until a terminal status or the window ends (nil). A
// 409 means the Take was never recorded: terminal, nothing executed.
func (b *Bot) pollComboTake(ctx context.Context, acct *comboAccount, rfqID string, every, window time.Duration) *polymarket.ComboTakeResult {
	deadline := time.Now().Add(window)
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(every):
		}
		res, err := b.comboStatus(ctx, acct, rfqID)
		if err != nil {
			if polymarket.ComboRFQHTTPStatus(err) == 409 {
				return &polymarket.ComboTakeResult{RFQID: rfqID, Status: "FAILED", ErrorCode: "NOT_ACCEPTED", ErrorMessage: "the take was never recorded"}
			}
			continue
		}
		if res.Terminal() {
			return res
		}
	}
	return nil
}

func comboRefusal(res *polymarket.ComboTakeResult) string {
	switch {
	case res.ErrorCode != "" && res.ErrorMessage != "":
		return res.ErrorCode + " — " + res.ErrorMessage
	case res.ErrorCode != "":
		return res.ErrorCode
	}
	return strings.ToLower(res.Status)
}

func comboFilledText(lc *loadedCombo, q *polymarket.ComboQuote, res *polymarket.ComboTakeResult) string {
	paid := float64(q.TotalRequiredE6) / 1e6
	shares := float64(q.NetReceiveE6) / 1e6
	var sb strings.Builder
	sb.WriteString("✅ *Combo bought*\n")
	fmt.Fprintf(&sb, "Paid $%.2f (fees incl.) → %.2f shares · x%.2f\n", paid, shares, q.Multiplier())
	fmt.Fprintf(&sb, "Pays $%.2f if every leg wins\n\n", shares)
	for i, l := range lc.view.Legs {
		fmt.Fprintf(&sb, "%d. %s — *%s*\n", i+1, escapeMarkdown(l.Question), escapeMarkdown(l.OutcomeLabel))
	}
	if res.TxHash != "" {
		fmt.Fprintf(&sb, "\n🔗 [Transaction](https://polygonscan.com/tx/%s)", res.TxHash)
	}
	sb.WriteString("\n\nTrack it in /positions.")
	return sb.String()
}

// comboBalance is the Trading Wallet's pUSD balance in 6-decimal units
// (seam: comboBalanceFn).
func (b *Bot) comboBalance(ctx context.Context, acct *comboAccount) (int64, error) {
	if b.comboBalanceFn != nil {
		return b.comboBalanceFn(ctx, acct)
	}
	bal, err := blockchain.NewBalanceChecker(b.blockchain.GetClient()).GetBalances(ctx, acct.proxy)
	if err != nil {
		return 0, err
	}
	if bal.USDC == nil || !bal.USDC.IsInt64() {
		return math.MaxInt64, nil
	}
	return bal.USDC.Int64(), nil
}

// comboTake Takes a quote (seam: comboTakeFn).
func (b *Bot) comboTake(ctx context.Context, acct *comboAccount, q *polymarket.ComboQuote) (*polymarket.ComboTakeResult, error) {
	if b.comboTakeFn != nil {
		return b.comboTakeFn(ctx, acct, q)
	}
	return b.tradingClient.TakeComboQuote(ctx, polymarket.ComboTakeRequest{
		EOA: acct.eoa, Proxy: acct.proxy, AccountType: acct.accountType,
		Creds: acct.creds, PrivateKey: acct.key, Quote: q,
	})
}

// comboStatus reads a Take's RFQ status (seam: comboStatusFn).
func (b *Bot) comboStatus(ctx context.Context, acct *comboAccount, rfqID string) (*polymarket.ComboTakeResult, error) {
	if b.comboStatusFn != nil {
		return b.comboStatusFn(ctx, acct, rfqID)
	}
	return b.tradingClient.GetComboRFQStatus(ctx, acct.eoa, acct.creds, rfqID)
}

func durationOr(d, def time.Duration) time.Duration {
	if d > 0 {
		return d
	}
	return def
}

// formatStake renders a stake for buttons and prompts: "25", "12.50".
func formatStake(v float64) string {
	if v == math.Trunc(v) {
		return strconv.FormatFloat(v, 'f', 0, 64)
	}
	return strconv.FormatFloat(v, 'f', 2, 64)
}

func formatStakeCents(v float64) string { return strconv.FormatFloat(v, 'f', 2, 64) }

// parseComboStake parses a Custom stake: any positive, finite dollar amount.
func parseComboStake(s string) (float64, error) {
	s = strings.TrimPrefix(strings.TrimSpace(s), "$")
	v, err := strconv.ParseFloat(s, 64)
	if err != nil || math.IsNaN(v) || math.IsInf(v, 0) || v <= 0 {
		return 0, fmt.Errorf("invalid stake %q", s)
	}
	return v, nil
}

// handleComboBuyCallback answers a stake button: cb:<key>:<stake>.
func (b *Bot) handleComboBuyCallback(ctx context.Context, update *tgbotapi.Update) {
	cq := update.CallbackQuery
	rest := strings.TrimPrefix(cq.Data, comboBuyPrefix)
	key, stakeStr, ok := strings.Cut(rest, ":")
	stake, err := parseComboStake(stakeStr)
	if !ok || err != nil {
		b.comboNotify(cq.Message.Chat.ID, "❌ Invalid stake button.")
		return
	}
	b.comboBuy(ctx, cq.From.ID, cq.Message.Chat.ID, cq.Message.MessageID, key, stake)
}

// handleComboCustomCallback asks for a Custom stake: cbc:<key>.
func (b *Bot) handleComboCustomCallback(ctx context.Context, update *tgbotapi.Update) {
	cq := update.CallbackQuery
	key := strings.TrimPrefix(cq.Data, comboCustomPrefix)
	b.stateManager.SetState(cq.From.ID, StateWaitingForComboStake, map[string]interface{}{
		"combo_key": key,
		"msg_id":    cq.Message.MessageID,
	}, comboStakeInputTTL)
	b.comboNotify(cq.Message.Chat.ID, "✏️ Enter your combo stake in $ (e.g. 40), or /cancel.")
}

// handleComboStakeInput takes the typed Custom stake and buys.
func (b *Bot) handleComboStakeInput(ctx context.Context, update *tgbotapi.Update, userCtx *UserContext) {
	chatID, userID := update.Message.Chat.ID, update.Message.From.ID
	b.stateManager.ClearState(userID)
	text := strings.TrimSpace(update.Message.Text)
	if strings.EqualFold(text, "/cancel") {
		b.sendMessage(chatID, "❌ Combo buy cancelled.")
		return
	}
	stake, err := parseComboStake(text)
	if err != nil {
		b.sendMessage(chatID, "❌ Invalid amount. Tap ✏️ Custom again and enter a positive number (e.g. 40).")
		return
	}
	key, _ := userCtx.Data["combo_key"].(string)
	msgID, _ := userCtx.Data["msg_id"].(int)
	b.comboBuy(ctx, userID, chatID, msgID, key, stake)
}
