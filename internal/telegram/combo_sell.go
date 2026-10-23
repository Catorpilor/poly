package telegram

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"

	"github.com/Catorpilor/poly/internal/polymarket"
)

// Combo sells (issue #125, ADR 0010 addendum). Keys are 12 hex chars.
const (
	comboSellPrefix        = "cs:"  // cs:<key>            open the sell screen
	comboSellQtyPrefix     = "csq:" // csq:<key>:<percent> sell that share of the position
	comboSellRefreshPrefix = "csr:" // csr:<key>           refresh the sell quote
)

var comboSellPercents = []int{25, 50, 75, 100}

// rememberComboSell keeps a held combo's Combo Condition under a short key
// (shared store with Combo Links; the "sell:" namespace cannot collide).
func (b *Bot) rememberComboSell(comboConditionID string) string {
	return b.rememberComboLink("sell:" + comboConditionID)
}

func (b *Bot) lookupComboSell(key string) (string, bool) {
	v, ok := b.lookupComboLink(key)
	if !ok || !strings.HasPrefix(v, "sell:") {
		return "", false
	}
	return strings.TrimPrefix(v, "sell:"), true
}

// comboSellPendingKey namespaces sell state (displayed proceeds, Pending
// block) apart from the buy state of Combo Links.
func comboSellPendingKey(key string) string { return "s:" + key }

// comboSellShares is pct% of a position in 6-decimal shares, rounded down
// to the gateway's 0.01-share step so the quote shows exactly what sells.
// Dust below one step can't be sold (0).
func comboSellShares(sharesE6 int64, pct int) int64 {
	s := sharesE6
	if pct < 100 {
		s = sharesE6 * int64(pct) / 100
	}
	return s - s%polymarket.ComboShareStepE6
}

func comboSellPctLabel(pct int) string {
	if pct >= 100 {
		return "💯"
	}
	return strconv.Itoa(pct) + "%"
}

// comboSellRows are the 💰 Sell buttons for the /positions Combos section,
// numbered like the section; only sellable combos get one.
func (b *Bot) comboSellRows(combos []*polymarket.ComboPosition) [][]tgbotapi.InlineKeyboardButton {
	var rows [][]tgbotapi.InlineKeyboardButton
	var row []tgbotapi.InlineKeyboardButton
	for i, c := range combos {
		if !c.Sellable() {
			continue
		}
		row = append(row, tgbotapi.NewInlineKeyboardButtonData(
			fmt.Sprintf("💰 Sell combo %d", i+1), comboSellPrefix+b.rememberComboSell(c.ComboConditionID)))
		if len(row) == 2 {
			rows, row = append(rows, row), nil
		}
	}
	if len(row) > 0 {
		rows = append(rows, row)
	}
	return rows
}

// loadComboSell loads the account and the held combo behind a sell key. On
// failure it returns the final screen text instead.
func (b *Bot) loadComboSell(ctx context.Context, userID int64, key string) (*comboAccount, *polymarket.ComboPosition, string) {
	cond, ok := b.lookupComboSell(key)
	if !ok {
		return nil, nil, "⌛ This sell screen expired — open /positions again."
	}
	acct, err := b.loadComboAccount(ctx, userID)
	if err != nil {
		return nil, nil, comboQuoteLine(nil, err)
	}
	c, err := b.comboPosition(ctx, acct, cond)
	switch {
	case err != nil:
		return nil, nil, "⚠️ Couldn't load this combo: " + escapeMarkdown(err.Error())
	case c == nil:
		return nil, nil, "You no longer hold this combo — check /positions."
	case c.Redeemable || c.Status == "REDEEMABLE":
		return nil, nil, comboPositionText(c) + "🏆 This combo already won — redeem it on polymarket.com instead of selling."
	case !c.Sellable():
		return nil, nil, comboPositionText(c) + "❌ This combo can't be sold: a leg already lost, so it can't pay."
	}
	return acct, c, ""
}

// comboSellScreen shows a held combo with a live SELL quote for all of it.
func (b *Bot) comboSellScreen(ctx context.Context, userID int64, key string) (string, *tgbotapi.InlineKeyboardMarkup) {
	acct, c, text := b.loadComboSell(ctx, userID, key)
	if c == nil {
		return text, nil
	}
	q, err := b.comboSellQuote(ctx, acct, c.LegPositionIDs(), comboSellShares(c.SharesE6, 100))
	b.setComboDisplayedValue(userID, comboSellPendingKey(key), comboSellDisplayed(q, err))
	return b.renderComboSell(key, c, comboSellQuoteLine(q, err, 100), "")
}

func comboSellDisplayed(q *polymarket.ComboQuote, err error) float64 {
	if err != nil {
		return 0
	}
	return q.ProceedsPerShare()
}

func (b *Bot) renderComboSell(key string, c *polymarket.ComboPosition, quoteLine, notice string) (string, *tgbotapi.InlineKeyboardMarkup) {
	var sb strings.Builder
	sb.WriteString(comboPositionText(c))
	sb.WriteString(quoteLine)
	fmt.Fprintf(&sb, "\n🕒 %s UTC", time.Now().UTC().Format("15:04:05"))
	if notice != "" {
		sb.WriteString("\n\n" + notice)
	}
	var qty []tgbotapi.InlineKeyboardButton
	for _, pct := range comboSellPercents[:3] {
		qty = append(qty, tgbotapi.NewInlineKeyboardButtonData(comboSellPctLabel(pct), fmt.Sprintf("%s%s:%d", comboSellQtyPrefix, key, pct)))
	}
	kb := tgbotapi.NewInlineKeyboardMarkup(
		tgbotapi.NewInlineKeyboardRow(qty...),
		tgbotapi.NewInlineKeyboardRow(tgbotapi.NewInlineKeyboardButtonData("💯 Sell All", fmt.Sprintf("%s%s:100", comboSellQtyPrefix, key))),
		tgbotapi.NewInlineKeyboardRow(tgbotapi.NewInlineKeyboardButtonData("🔄 Refresh quote", comboSellRefreshPrefix+key)),
	)
	return sb.String(), &kb
}

func comboPositionText(c *polymarket.ComboPosition) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "💰 *Sell combo* · %d legs\n\n", len(c.Legs))
	for i, l := range c.Legs {
		status := fmt.Sprintf("@ %.2f ⏳", l.Price)
		switch l.Status {
		case "RESOLVED_WIN":
			status = "✅"
		case "RESOLVED_LOSS":
			status = "❌"
		}
		fmt.Fprintf(&sb, "%d. %s — *%s* %s\n", i+1, escapeMarkdown(l.Question), escapeMarkdown(l.OutcomeLabel), status)
	}
	fmt.Fprintf(&sb, "\nYou hold %.2f shares (paid $%.2f)\n", float64(c.SharesE6)/1e6, c.Paid)
	return sb.String()
}

func comboSellQuoteLine(q *polymarket.ComboQuote, err error, pct int) string {
	if err != nil || q.NoQuote() {
		return comboQuoteLine(q, err)
	}
	what := "all " + fmt.Sprintf("%.2f shares", float64(q.TotalRequiredE6)/1e6)
	if pct < 100 {
		what = fmt.Sprintf("%d%% = %.2f shares", pct, float64(q.TotalRequiredE6)/1e6)
	}
	return fmt.Sprintf("💹 *Live sell quote* (%s): you receive *$%.2f* (%.3f/share after fees)",
		what, float64(q.NetReceiveE6)/1e6, q.ProceedsPerShare())
}

// comboSell runs one sell tap: fresh SELL quote for pct% → Take within the
// Multiplier Tolerance on proceeds per share (one retry after a refusal) →
// Sold, Failed or Pending (ADR 0010).
func (b *Bot) comboSell(ctx context.Context, userID, chatID int64, msgID int, key string, pct int) {
	pkey := comboSellPendingKey(key)
	if !b.markComboPending(userID, pkey) {
		b.comboNotify(chatID, "⏳ You already have a pending sell on this combo — wait for it to settle.")
		return
	}
	keepPending := false
	defer func() {
		if !keepPending {
			b.clearComboPending(userID, pkey)
		}
	}()

	label := comboSellPctLabel(pct)
	b.showComboScreen(chatID, msgID, fmt.Sprintf("⏳ Selling %s of this combo…", label), nil)
	acct, c, text := b.loadComboSell(ctx, userID, key)
	if c == nil {
		b.showComboScreen(chatID, msgID, text, nil)
		return
	}
	show := func(quoteLine, notice string) {
		t, k := b.renderComboSell(key, c, quoteLine, notice)
		b.showComboScreen(chatID, msgID, t, k)
	}
	shares := comboSellShares(c.SharesE6, pct)
	if shares <= 0 {
		show("", "❌ That's less than 0.01 share — nothing to sell.")
		return
	}

	retried := ""
	for attempt := 0; attempt < 2; attempt++ {
		q, err := b.comboSellQuote(ctx, acct, c.LegPositionIDs(), shares)
		if err != nil || q.NoQuote() {
			show(comboSellQuoteLine(q, err, pct), retried+"Nothing was sold.")
			return
		}
		ref := b.comboDisplayedValue(userID, pkey)
		if per := q.ProceedsPerShare(); ref <= 0 || per < ref*(1-comboMultiplierTolerance) {
			b.setComboDisplayedValue(userID, pkey, per)
			show(comboSellQuoteLine(q, nil, pct), fmt.Sprintf("%s👆 New quote: you'd receive *$%.2f* (%.3f/share). Tap %s again to sell at this price.",
				retried, float64(q.NetReceiveE6)/1e6, per, label))
			return
		}

		res, err := b.comboTake(ctx, acct, q)
		if err != nil {
			if polymarket.ComboRFQHTTPStatus(err) != 0 {
				show(comboSellQuoteLine(q, nil, pct), "❌ Combo sell rejected: "+escapeMarkdown(err.Error())+". Nothing was sold.")
				return
			}
			res = &polymarket.ComboTakeResult{RFQID: q.RFQID, Status: "UNKNOWN"}
		}
		switch {
		case res.Filled():
			b.showComboScreen(chatID, msgID, comboSoldText(c, q, res), nil)
			return
		case res.Terminal() && attempt == 0:
			retried = "The first quote was refused (" + escapeMarkdown(comboRefusal(res)) + "); retried once. "
			continue
		case res.Terminal():
			show(comboSellQuoteLine(q, nil, pct), "❌ Combo sell failed: "+escapeMarkdown(comboRefusal(res))+". Nothing was sold.")
			return
		}

		keepPending = true
		b.followComboTake(ctx, acct, chatID, msgID, res.RFQID, comboFollow{
			pendingText: comboPositionText(c) +
				"\n⏳ *Pending* — the market maker hasn't settled this sell yet. I'll update this message when it does.\n" +
				"Don't sell this combo again until then.",
			unknownText: func(w time.Duration) string {
				return comboPositionText(c) + fmt.Sprintf(
					"\n⚠️ Still no final status after %s — the outcome is *unknown*. Check /positions before selling again.", w)
			},
			finish: func(res *polymarket.ComboTakeResult) {
				if res.Filled() {
					b.showComboScreen(chatID, msgID, comboSoldText(c, q, res), nil)
					return
				}
				show(comboSellQuoteLine(q, nil, pct), "❌ Combo sell failed: "+escapeMarkdown(comboRefusal(res))+". Nothing was sold.")
			},
			release: func() { b.clearComboPending(userID, pkey) },
		})
		return
	}
}

func comboSoldText(c *polymarket.ComboPosition, q *polymarket.ComboQuote, res *polymarket.ComboTakeResult) string {
	var sb strings.Builder
	sb.WriteString("✅ *Combo sold*\n")
	fmt.Fprintf(&sb, "Sold %.2f shares → received $%.2f (%.3f/share after fees)\n",
		float64(q.TotalRequiredE6)/1e6, float64(q.NetReceiveE6)/1e6, q.ProceedsPerShare())
	for i, l := range c.Legs {
		fmt.Fprintf(&sb, "%d. %s — *%s*\n", i+1, escapeMarkdown(l.Question), escapeMarkdown(l.OutcomeLabel))
	}
	if res.TxHash != "" {
		fmt.Fprintf(&sb, "\n🔗 [Transaction](https://polygonscan.com/tx/%s)", res.TxHash)
	}
	sb.WriteString("\n\nTrack what's left in /positions.")
	return sb.String()
}

// comboPosition loads one held combo (seam: comboPositionFn).
func (b *Bot) comboPosition(ctx context.Context, acct *comboAccount, cond string) (*polymarket.ComboPosition, error) {
	if b.comboPositionFn != nil {
		return b.comboPositionFn(ctx, acct, cond)
	}
	return polymarket.NewPositionManager().GetComboPosition(ctx, acct.proxy, cond)
}

// handleComboSellOpen answers 💰 Sell combo N in /positions: cs:<key>.
func (b *Bot) handleComboSellOpen(ctx context.Context, update *tgbotapi.Update) {
	cq := update.CallbackQuery
	key := strings.TrimPrefix(cq.Data, comboSellPrefix)
	msgID := b.comboSend(cq.Message.Chat.ID, "💰 Loading sell quote…")
	text, kb := b.comboSellScreen(ctx, cq.From.ID, key)
	b.showComboScreen(cq.Message.Chat.ID, msgID, text, kb)
}

// handleComboSellRefresh answers 🔄 Refresh on the sell screen: csr:<key>.
func (b *Bot) handleComboSellRefresh(ctx context.Context, update *tgbotapi.Update) {
	cq := update.CallbackQuery
	text, kb := b.comboSellScreen(ctx, cq.From.ID, strings.TrimPrefix(cq.Data, comboSellRefreshPrefix))
	b.showComboScreen(cq.Message.Chat.ID, cq.Message.MessageID, text, kb)
}

// handleComboSellQty answers 25% … 💯 on the sell screen: csq:<key>:<pct>.
func (b *Bot) handleComboSellQty(ctx context.Context, update *tgbotapi.Update) {
	cq := update.CallbackQuery
	key, pctStr, ok := strings.Cut(strings.TrimPrefix(cq.Data, comboSellQtyPrefix), ":")
	pct, err := strconv.Atoi(pctStr)
	if !ok || err != nil || pct <= 0 || pct > 100 {
		b.comboNotify(cq.Message.Chat.ID, "❌ Invalid sell button.")
		return
	}
	b.comboSell(ctx, cq.From.ID, cq.Message.Chat.ID, cq.Message.MessageID, key, pct)
}
