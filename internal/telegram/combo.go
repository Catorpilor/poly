package telegram

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/common"
	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"

	"github.com/Catorpilor/poly/internal/polymarket"
)

// comboReferenceStakeUSD is the stake of the Combo Quote shown when a Combo
// Link opens (issue #118). Buying re-quotes at the user's chosen stake.
const comboReferenceStakeUSD = 10.0

// comboRefreshPrefix is the callback-data prefix of the 🔄 Refresh button.
const comboRefreshPrefix = "cr:"

// errComboNoWallet: the user has no wallet, so no quote can be requested.
var errComboNoWallet = errors.New("no wallet")

// comboScreen builds the screen for a Combo Link: its legs, the verdict and,
// for a buyable combo, a live Combo Quote at the reference stake beside the
// Combo Estimate. kb is nil when nothing can be done from the screen.
func (b *Bot) comboScreen(ctx context.Context, userID int64, param string) (string, *tgbotapi.InlineKeyboardMarkup) {
	refs, err := polymarket.ParseComboLink(param)
	if err != nil {
		return "❌ Invalid combo link: " + escapeMarkdown(err.Error()), nil
	}

	legs, err := b.resolveComboLegs(ctx, refs)
	if err != nil {
		if errors.Is(err, polymarket.ErrMarketNotFound) {
			return "❌ Invalid combo link: " + escapeMarkdown(err.Error()), nil
		}
		return "⚠️ Couldn't load the combo's legs: " + escapeMarkdown(err.Error()), b.comboRefreshKeyboard(param)
	}
	view := polymarket.ComboView{Legs: legs}
	verdict, blocker := view.Verdict()

	var sb strings.Builder
	fmt.Fprintf(&sb, "🧩 *Combo* · %d legs\n\n", len(legs))
	for i, l := range legs {
		fmt.Fprintf(&sb, "%d. %s — %s [↗](%s)\n", i+1, escapeMarkdown(l.Question), comboLegStatus(l), comboLegURL(l))
	}
	sb.WriteString("\n")

	switch verdict {
	case polymarket.ComboLost:
		fmt.Fprintf(&sb, "❌ Leg %d already lost — this combo can't pay. Nothing to buy.", blocker)
		return sb.String(), nil
	case polymarket.ComboWon:
		sb.WriteString("🏁 Every leg already won — this combo is decided. Nothing to buy.")
		return sb.String(), nil
	case polymarket.ComboUnavailable:
		fmt.Fprintf(&sb, "⛔ Leg %d is %s — this combo can't be bought right now.", blocker, legs[blocker-1].State)
		return sb.String(), nil
	}

	quote, qErr := b.comboQuote(ctx, userID, view.LegPositionIDs(), comboReferenceStakeUSD)
	sb.WriteString(comboQuoteLine(quote, qErr))
	if _, mult, ok := view.Estimate(); ok {
		fmt.Fprintf(&sb, "\n📐 Estimate: x%.2f from leg prices (before fees)", mult)
	}
	fmt.Fprintf(&sb, "\n🕒 %s UTC", time.Now().UTC().Format("15:04:05"))
	sb.WriteString("\n\n🛒 Buying combos from the bot comes in the next release.")
	return sb.String(), b.comboRefreshKeyboard(param)
}

func comboLegStatus(l polymarket.ComboLeg) string {
	label := "*" + escapeMarkdown(l.OutcomeLabel) + "*"
	switch l.State {
	case polymarket.LegOpen:
		return fmt.Sprintf("%s @ %.2f", label, l.Price)
	case polymarket.LegWon:
		return label + " ✅ won"
	case polymarket.LegLost:
		return label + " ❌ lost"
	case polymarket.LegPaused:
		return label + " ⏸ paused"
	case polymarket.LegNotComboable:
		return label + " 🚫 not combo-enabled"
	default:
		return label + " 🔒 closed"
	}
}

// comboLegURL links a leg to its page. Gamma's by-id lookup omits events[],
// so the market route is the usual fallback.
func comboLegURL(l polymarket.ComboLeg) string {
	if l.EventSlug != "" {
		return "https://polymarket.com/event/" + l.EventSlug
	}
	return "https://polymarket.com/market/" + l.MarketSlug
}

func comboQuoteLine(q *polymarket.ComboQuote, err error) string {
	switch {
	case errors.Is(err, errComboNoWallet):
		return "🔑 Set up a wallet with /start to get a live quote."
	case err != nil:
		return "⚠️ Live quote unavailable: " + escapeMarkdown(err.Error())
	case q.NoQuote() && q.ErrorCode == "NO_QUOTES":
		return "⚠️ No quotes right now — market makers aren't filling this combo. Try 🔄 Refresh."
	case q.NoQuote():
		return fmt.Sprintf("⚠️ Quote failed: %s — %s", escapeMarkdown(q.ErrorCode), escapeMarkdown(q.ErrorMessage))
	}
	payout := float64(q.NetReceiveE6) / 1e6
	stake := float64(q.TotalRequiredE6) / 1e6
	return fmt.Sprintf("💹 *Live quote* ($%.2f): *x%.2f* — pays $%.2f if every leg wins (fees included)", stake, q.Multiplier(), payout)
}

// resolveComboLegs looks up every leg's market concurrently, keeping link order.
func (b *Bot) resolveComboLegs(ctx context.Context, refs []polymarket.ComboLegRef) ([]polymarket.ComboLeg, error) {
	fetch := b.comboMarketFn
	if fetch == nil {
		mc := polymarket.NewMarketClient()
		fetch = mc.GetMarketByID
	}
	legs := make([]polymarket.ComboLeg, len(refs))
	errs := make([]error, len(refs))
	var wg sync.WaitGroup
	for i, ref := range refs {
		wg.Add(1)
		go func(i int, ref polymarket.ComboLegRef) {
			defer wg.Done()
			m, err := fetch(ctx, ref.MarketID)
			if err != nil {
				errs[i] = fmt.Errorf("market %s: %w", ref.MarketID, err)
				return
			}
			legs[i], errs[i] = polymarket.BuildComboLeg(ref, m)
		}(i, ref)
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			return nil, err
		}
	}
	return legs, nil
}

// comboQuote requests a Combo Quote for the user (seam: comboQuoteFn).
func (b *Bot) comboQuote(ctx context.Context, userID int64, legs []string, stakeUSD float64) (*polymarket.ComboQuote, error) {
	if b.comboQuoteFn != nil {
		return b.comboQuoteFn(ctx, userID, legs, stakeUSD)
	}
	user, err := b.userRepo.GetByTelegramID(ctx, userID)
	if err != nil {
		return nil, fmt.Errorf("load user: %w", err)
	}
	if user == nil || user.EncryptedKey == "" {
		return nil, errComboNoWallet
	}
	key, err := b.walletManager.DecryptPrivateKey(user.EncryptedKey)
	if err != nil {
		return nil, fmt.Errorf("decrypt wallet: %w", err)
	}
	creds, err := b.tradingClient.GetOrCreateAPICredentials(ctx, key.PrivateKey)
	if err != nil {
		return nil, fmt.Errorf("API credentials: %w", err)
	}
	return b.tradingClient.RequestComboQuote(ctx, polymarket.ComboQuoteRequest{
		EOA:            common.HexToAddress(user.EOAAddress),
		Proxy:          common.HexToAddress(user.ProxyAddress),
		AccountType:    user.AccountType,
		Creds:          creds,
		LegPositionIDs: legs,
		StakeUSD:       stakeUSD,
	})
}

// comboRefreshKeyboard is the 🔄 Refresh quote button. The link itself can
// fill Telegram's 64-byte callback limit, so it is kept server-side under a
// short key (lost on restart: the user just reopens the link).
func (b *Bot) comboRefreshKeyboard(param string) *tgbotapi.InlineKeyboardMarkup {
	kb := tgbotapi.NewInlineKeyboardMarkup(tgbotapi.NewInlineKeyboardRow(
		tgbotapi.NewInlineKeyboardButtonData("🔄 Refresh quote", comboRefreshPrefix+b.rememberComboLink(param)),
	))
	return &kb
}

func (b *Bot) rememberComboLink(param string) string {
	sum := sha256.Sum256([]byte(param))
	key := hex.EncodeToString(sum[:6])
	b.comboLinksMu.Lock()
	defer b.comboLinksMu.Unlock()
	if b.comboLinks == nil {
		b.comboLinks = make(map[string]string)
	}
	b.comboLinks[key] = param
	return key
}

func (b *Bot) lookupComboLink(key string) (string, bool) {
	b.comboLinksMu.Lock()
	defer b.comboLinksMu.Unlock()
	p, ok := b.comboLinks[key]
	return p, ok
}

// handleComboLink answers a /start c_... deep link.
func (b *Bot) handleComboLink(ctx context.Context, update *tgbotapi.Update, param string) {
	chatID := update.Message.Chat.ID
	loading := b.sendMessageAndReturn(chatID, "🧩 Loading combo…")
	text, kb := b.comboScreen(ctx, update.Message.From.ID, param)
	b.showComboScreen(chatID, loading.MessageID, text, kb)
}

// handleComboRefresh answers the 🔄 Refresh quote button.
func (b *Bot) handleComboRefresh(ctx context.Context, update *tgbotapi.Update) {
	cq := update.CallbackQuery
	chatID, msgID := cq.Message.Chat.ID, cq.Message.MessageID
	param, ok := b.lookupComboLink(strings.TrimPrefix(cq.Data, comboRefreshPrefix))
	if !ok {
		b.editMessage(chatID, msgID, "⌛ This combo screen expired — open the combo link again.")
		return
	}
	text, kb := b.comboScreen(ctx, cq.From.ID, param)
	b.showComboScreen(chatID, msgID, text, kb)
}

func (b *Bot) showComboScreen(chatID int64, msgID int, text string, kb *tgbotapi.InlineKeyboardMarkup) {
	edit := tgbotapi.NewEditMessageText(chatID, msgID, text)
	edit.ParseMode = "Markdown"
	edit.DisableWebPagePreview = true
	edit.ReplyMarkup = kb
	if _, err := b.api.Send(edit); err != nil {
		log.Printf("combo screen: edit failed: %v", err)
	}
}

// escapeMarkdown escapes Telegram legacy-Markdown control characters in
// untrusted text (market questions, outcome labels, error messages).
func escapeMarkdown(s string) string {
	return strings.NewReplacer("_", "\\_", "*", "\\*", "`", "\\`", "[", "\\[").Replace(s)
}
