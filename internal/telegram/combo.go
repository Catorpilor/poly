package telegram

import (
	"context"
	"crypto/ecdsa"
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

// Callback-data prefixes for the combo screen. Keys are 12 hex chars, so the
// longest ("cb:<key>:<stake>") stays far below Telegram's 64 bytes.
const (
	comboRefreshPrefix = "cr:"  // cr:<key>
	comboBuyPrefix     = "cb:"  // cb:<key>:<stake>
	comboCustomPrefix  = "cbc:" // cbc:<key>
)

// comboStakePresets are the one-tap stakes, as on single markets.
var comboStakePresets = []int{10, 25, 50, 100, 250}

// errComboNoWallet: the user has no wallet, so no quote can be requested.
var errComboNoWallet = errors.New("no wallet")

// comboAccount is everything needed to quote, Take and poll for one user,
// loaded once per screen or buy.
type comboAccount struct {
	userID      int64
	eoa         common.Address
	proxy       common.Address
	accountType string
	creds       *polymarket.APICredentials
	key         *ecdsa.PrivateKey
}

// loadedCombo is a parsed and resolved Combo Link.
type loadedCombo struct {
	param   string
	key     string // refresh/buy key for the link
	view    polymarket.ComboView
	verdict polymarket.ComboVerdict
	blocker int
}

// loadCombo parses and resolves a link. On failure it returns the final
// screen (text, keyboard) instead.
func (b *Bot) loadCombo(ctx context.Context, param string) (*loadedCombo, string, *tgbotapi.InlineKeyboardMarkup) {
	refs, err := polymarket.ParseComboLink(param)
	if err != nil {
		return nil, "❌ Invalid combo link: " + escapeMarkdown(err.Error()), nil
	}
	legs, err := b.resolveComboLegs(ctx, refs)
	if err != nil {
		if errors.Is(err, polymarket.ErrMarketNotFound) {
			return nil, "❌ Invalid combo link: " + escapeMarkdown(err.Error()), nil
		}
		return nil, "⚠️ Couldn't load the combo's legs: " + escapeMarkdown(err.Error()), b.comboRefreshKeyboard(param)
	}
	lc := &loadedCombo{param: param, key: b.rememberComboLink(param), view: polymarket.ComboView{Legs: legs}}
	lc.verdict, lc.blocker = lc.view.Verdict()
	return lc, "", nil
}

// comboScreen builds the screen for a Combo Link: its legs, the verdict and,
// for a buyable combo, a live Combo Quote at the reference stake beside the
// Combo Estimate plus the stake buttons. kb is nil when nothing can be done.
func (b *Bot) comboScreen(ctx context.Context, userID int64, param string) (string, *tgbotapi.InlineKeyboardMarkup) {
	lc, text, kb := b.loadCombo(ctx, param)
	if lc == nil {
		return text, kb
	}
	if lc.verdict != polymarket.ComboBuyable {
		return comboLegsText(lc.view) + comboVerdictText(lc), nil
	}

	var quote *polymarket.ComboQuote
	acct, qErr := b.loadComboAccount(ctx, userID)
	if qErr == nil {
		quote, qErr = b.comboQuote(ctx, acct, lc.view.LegPositionIDs(), comboReferenceStakeUSD)
	}
	b.setComboDisplayed(userID, lc.key, quote, qErr)
	return b.renderBuyableCombo(lc, comboQuoteLine(quote, qErr), "")
}

// renderBuyableCombo is the buyable screen: legs, quote section, estimate,
// an optional notice, and the stake + refresh buttons.
func (b *Bot) renderBuyableCombo(lc *loadedCombo, quoteLine, notice string) (string, *tgbotapi.InlineKeyboardMarkup) {
	var sb strings.Builder
	sb.WriteString(comboLegsText(lc.view))
	sb.WriteString(quoteLine)
	if _, mult, ok := lc.view.Estimate(); ok {
		fmt.Fprintf(&sb, "\n📐 Estimate: x%.2f from leg prices (before fees)", mult)
	}
	fmt.Fprintf(&sb, "\n🕒 %s UTC", time.Now().UTC().Format("15:04:05"))
	if notice != "" {
		sb.WriteString("\n\n" + notice)
	}
	return sb.String(), comboBuyKeyboard(lc.key)
}

func comboLegsText(v polymarket.ComboView) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "🧩 *Combo* · %d legs\n\n", len(v.Legs))
	for i, l := range v.Legs {
		fmt.Fprintf(&sb, "%d. %s — %s [↗](%s)\n", i+1, escapeMarkdown(l.Question), comboLegStatus(l), comboLegURL(l))
	}
	sb.WriteString("\n")
	return sb.String()
}

func comboVerdictText(lc *loadedCombo) string {
	switch lc.verdict {
	case polymarket.ComboLost:
		return fmt.Sprintf("❌ Leg %d already lost — this combo can't pay. Nothing to buy.", lc.blocker)
	case polymarket.ComboWon:
		return "🏁 Every leg already won — this combo is decided. Nothing to buy."
	default:
		return fmt.Sprintf("⛔ Leg %d is %s — this combo can't be bought right now.", lc.blocker, lc.view.Legs[lc.blocker-1].State)
	}
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
	case comboRateLimited(err):
		return "⏳ Quote limit reached — try again in about a minute."
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

// loadComboAccount loads the user's wallet and CLOB credentials once
// (seam: comboAccountFn).
func (b *Bot) loadComboAccount(ctx context.Context, userID int64) (*comboAccount, error) {
	if b.comboAccountFn != nil {
		return b.comboAccountFn(ctx, userID)
	}
	user, err := b.userRepo.GetByTelegramID(ctx, userID)
	if err != nil {
		return nil, fmt.Errorf("load user: %w", err)
	}
	if user == nil || user.EncryptedKey == "" {
		return nil, errComboNoWallet
	}
	w, err := b.walletManager.DecryptPrivateKey(user.EncryptedKey)
	if err != nil {
		return nil, fmt.Errorf("decrypt wallet: %w", err)
	}
	creds, err := b.tradingClient.GetOrCreateAPICredentials(ctx, w.PrivateKey)
	if err != nil {
		return nil, fmt.Errorf("API credentials: %w", err)
	}
	return &comboAccount{
		userID:      userID,
		eoa:         common.HexToAddress(user.EOAAddress),
		proxy:       common.HexToAddress(user.ProxyAddress),
		accountType: user.AccountType,
		creds:       creds,
		key:         w.PrivateKey,
	}, nil
}

// comboQuote requests a Combo Quote (seam: comboQuoteFn).
func (b *Bot) comboQuote(ctx context.Context, acct *comboAccount, legs []string, stakeUSD float64) (*polymarket.ComboQuote, error) {
	if b.comboQuoteFn != nil {
		return b.comboQuoteFn(ctx, acct, legs, stakeUSD)
	}
	return b.tradingClient.RequestComboQuote(ctx, polymarket.ComboQuoteRequest{
		EOA: acct.eoa, Proxy: acct.proxy, AccountType: acct.accountType,
		Creds: acct.creds, LegPositionIDs: legs, StakeUSD: stakeUSD,
	})
}

// comboBuyKeyboard is the buyable screen's stake buttons and 🔄 Refresh.
func comboBuyKeyboard(key string) *tgbotapi.InlineKeyboardMarkup {
	btn := func(stake int) tgbotapi.InlineKeyboardButton {
		return tgbotapi.NewInlineKeyboardButtonData(fmt.Sprintf("💵 $%d", stake), fmt.Sprintf("%s%s:%d", comboBuyPrefix, key, stake))
	}
	p := comboStakePresets
	kb := tgbotapi.NewInlineKeyboardMarkup(
		tgbotapi.NewInlineKeyboardRow(btn(p[0]), btn(p[1]), btn(p[2])),
		tgbotapi.NewInlineKeyboardRow(btn(p[3]), btn(p[4])),
		tgbotapi.NewInlineKeyboardRow(tgbotapi.NewInlineKeyboardButtonData("✏️ Custom", comboCustomPrefix+key)),
		tgbotapi.NewInlineKeyboardRow(tgbotapi.NewInlineKeyboardButtonData("🔄 Refresh quote", comboRefreshPrefix+key)),
	)
	return &kb
}

// comboRefreshKeyboard is a lone 🔄 Refresh button (legs failed to load).
func (b *Bot) comboRefreshKeyboard(param string) *tgbotapi.InlineKeyboardMarkup {
	kb := tgbotapi.NewInlineKeyboardMarkup(tgbotapi.NewInlineKeyboardRow(
		tgbotapi.NewInlineKeyboardButtonData("🔄 Refresh quote", comboRefreshPrefix+b.rememberComboLink(param)),
	))
	return &kb
}

// rememberComboLink keeps a link server-side under a short key: the link can
// fill Telegram's 64-byte callback limit on its own. In-memory, lost on
// restart (the user just reopens the link).
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
		b.showComboScreen(chatID, msgID, comboExpiredText, nil)
		return
	}
	text, kb := b.comboScreen(ctx, cq.From.ID, param)
	b.showComboScreen(chatID, msgID, text, kb)
}

const comboExpiredText = "⌛ This combo screen expired — open the combo link again."

// showComboScreen edits the combo message (seam: comboEditFn).
func (b *Bot) showComboScreen(chatID int64, msgID int, text string, kb *tgbotapi.InlineKeyboardMarkup) {
	if b.comboEditFn != nil {
		b.comboEditFn(chatID, msgID, text, kb)
		return
	}
	edit := tgbotapi.NewEditMessageText(chatID, msgID, text)
	edit.ParseMode = "Markdown"
	edit.DisableWebPagePreview = true
	edit.ReplyMarkup = kb
	if _, err := b.api.Send(edit); err != nil {
		log.Printf("combo screen: edit failed: %v", err)
	}
}

// comboNotify sends a separate message (seam: comboNotifyFn).
func (b *Bot) comboNotify(chatID int64, text string) {
	if b.comboNotifyFn != nil {
		b.comboNotifyFn(chatID, text)
		return
	}
	b.sendMessage(chatID, text)
}

// escapeMarkdown escapes Telegram legacy-Markdown control characters in
// untrusted text (market questions, outcome labels, error messages).
func escapeMarkdown(s string) string {
	return strings.NewReplacer("_", "\\_", "*", "\\*", "`", "\\`", "[", "\\[").Replace(s)
}
