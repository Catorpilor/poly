package telegram

import (
	"context"
	"fmt"
	"log"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/common"
	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"

	"github.com/Catorpilor/poly/internal/database"
	"github.com/Catorpilor/poly/internal/live"
	"github.com/Catorpilor/poly/internal/polymarket"
)

// SnipeAskSource is the slice of the price feed the snipe tap handler needs:
// a fresh best ask for the repricing guard.
type SnipeAskSource interface {
	BestAsk(tokenID string) (float64, bool)
}

// snipeWatch is the slice of the comeback-snipe watcher the bot drives: watch
// registration and the bought latch. *live.SnipeWatcher implements it; tests
// inject a recording fake.
type snipeWatch interface {
	WatchArmed(m live.SnipeMarket)
	UnwatchArmed(tokenID string)
	WatchHeld(chatID int64, m live.SnipeMarket, ttl time.Duration)
	RenewHeld(chatID int64, tokenID string, ttl time.Duration) bool
	MarkBought(tokenID string)
}

// Comeback Snipe v2 auto-buy sizing — product policy, deliberately global
// constants, not per-user configuration (see CONTEXT.md "Comeback Snipe").
const (
	// snipeAutoBuyUSD is the fixed stake auto-bought on every genuine alert.
	snipeAutoBuyUSD = 10.0
	// snipeAutoBuyDailyCapUSD bounds one recipient's auto-snipe spend per UTC
	// day.
	snipeAutoBuyDailyCapUSD = 50.0
)

// The bot is the snipe watcher's notifier (wired in cmd/bot/main.go).
var _ live.SnipeNotifier = (*Bot)(nil)

// snipeAlertTTL is how long a snipe alert's buttons stay tappable. Alerts are
// in-memory only; expired entries are pruned lazily.
const snipeAlertTTL = 12 * time.Hour

// snipeAlertStatus classifies a claim attempt.
type snipeAlertStatus int

const (
	snipeAlertOK      snipeAlertStatus = iota
	snipeAlertUsed                     // already claimed — never buy twice
	snipeAlertExpired                  // unknown or past TTL
)

// snipeAlertEntry is the market info behind one alert message. Token IDs are
// ~78 digits and cannot ride in Telegram's 64-byte callback data, so buttons
// carry a short alert ID that resolves here.
type snipeAlertEntry struct {
	tokenID   string
	marketID  string
	question  string
	outcome   string
	createdAt time.Time
	used      bool
}

// snipeAlertRegistry maps short alert IDs to market info, in-memory,
// expiring after snipeAlertTTL. Claiming is atomic: the first tap wins and
// every later tap reports used.
type snipeAlertRegistry struct {
	mu      sync.Mutex
	seq     uint64
	entries map[string]*snipeAlertEntry
	ttl     time.Duration
	now     func() time.Time
}

func newSnipeAlertRegistry() *snipeAlertRegistry {
	return &snipeAlertRegistry{
		entries: make(map[string]*snipeAlertEntry),
		ttl:     snipeAlertTTL,
		now:     time.Now,
	}
}

// add registers an alert's market info and returns its short ID (base36
// sequence — a handful of bytes). Expired entries are pruned lazily here.
func (r *snipeAlertRegistry) add(m live.SnipeMarket) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.now()
	for id, e := range r.entries {
		if now.Sub(e.createdAt) > r.ttl {
			delete(r.entries, id)
		}
	}
	r.seq++
	id := strconv.FormatUint(r.seq, 36)
	r.entries[id] = &snipeAlertEntry{
		tokenID:   m.TokenID,
		marketID:  m.MarketID,
		question:  m.Question,
		outcome:   m.Outcome,
		createdAt: now,
	}
	return id
}

// claim atomically marks the alert used and returns its entry. A used or
// expired ID reports its status — the caller answers and never buys.
func (r *snipeAlertRegistry) claim(id string) (snipeAlertEntry, snipeAlertStatus) {
	r.mu.Lock()
	defer r.mu.Unlock()
	e := r.entries[id]
	if e == nil {
		return snipeAlertEntry{}, snipeAlertExpired
	}
	if r.now().Sub(e.createdAt) > r.ttl {
		delete(r.entries, id)
		return snipeAlertEntry{}, snipeAlertExpired
	}
	if e.used {
		return *e, snipeAlertUsed
	}
	e.used = true
	return *e, snipeAlertOK
}

// snipeSpendLedger accumulates per-chat auto-snipe spend for the current UTC
// day. In-memory only — a restart resets the cap. That's a soft rail by
// design: it bounds a runaway alert day, not an adversary. Reservation-style:
// reserve before the buy so racing alerts cannot overshoot the cap, release on
// failure so only successful buys consume it.
type snipeSpendLedger struct {
	mu    sync.Mutex
	day   string // UTC date of the amounts in spent; rollover clears them
	spent map[int64]float64
	now   func() time.Time
}

func newSnipeSpendLedger() *snipeSpendLedger {
	return &snipeSpendLedger{spent: make(map[int64]float64), now: time.Now}
}

// rollLocked clears the accumulator on UTC-day change. Callers hold mu.
func (l *snipeSpendLedger) rollLocked() {
	day := l.now().UTC().Format("2006-01-02")
	if day != l.day {
		l.day = day
		l.spent = make(map[int64]float64)
	}
}

// reserve claims amount of chatID's daily cap, reporting the cap remaining
// after the claim. A claim that would exceed the cap is refused whole — no
// partial auto-buys.
func (l *snipeSpendLedger) reserve(chatID int64, amount float64) (left float64, ok bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.rollLocked()
	if l.spent[chatID]+amount > snipeAutoBuyDailyCapUSD {
		return snipeAutoBuyDailyCapUSD - l.spent[chatID], false
	}
	l.spent[chatID] += amount
	return snipeAutoBuyDailyCapUSD - l.spent[chatID], true
}

// release refunds a reservation whose buy failed. A release landing after a
// UTC rollover is dropped — the fresh day's accumulator never goes negative.
func (l *snipeSpendLedger) release(chatID int64, amount float64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.rollLocked()
	l.spent[chatID] -= amount
	if l.spent[chatID] <= 0 {
		delete(l.spent, chatID)
	}
}

// snipeAlertText builds the alert body. Pure — table-tested.
func snipeAlertText(question, outcome string, sessionHigh, ask float64) string {
	multiple := 0.0
	if ask > 0 {
		multiple = 1 / ask
	}
	return fmt.Sprintf(
		"🎯 *Comeback Snipe*\n\n"+
			"*%s*\n"+
			"*Outcome:* %s\n\n"+
			"was $%.2f, now $%.2f ask — %.1f× payout if it comes back.\n\n"+
			"In-play crash on a formerly competitive side. No auto-buy went "+
			"through for this alert — judge the game state and tap below.",
		truncateUTF8(question, 60), outcome, sessionHigh, ask, multiple)
}

// snipeKeyboard builds the one-tap buy buttons for an alert.
func snipeKeyboard(alertID string) tgbotapi.InlineKeyboardMarkup {
	return tgbotapi.NewInlineKeyboardMarkup(
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("⚡ Snipe $10", fmt.Sprintf("snipe:%s:10", alertID)),
			tgbotapi.NewInlineKeyboardButtonData("⚡ Snipe $25", fmt.Sprintf("snipe:%s:25", alertID)),
		),
	)
}

// snipeAutoBoughtText builds the v2 auto-buy confirmation. Pure — table-tested.
func snipeAutoBoughtText(question, outcome string, sessionHigh, ask, amount float64, orderID string, capLeft float64) string {
	return fmt.Sprintf(
		"⚡ *Auto-sniped $%.0f*\n\n"+
			"*%s*\n"+
			"*Side:* Buy %s\n"+
			"was $%.2f, now $%.2f ask.\n\n"+
			"*Order ID:* %s\n"+
			"Auto-snipe cap left today: $%.0f\n\n"+
			"Top it up or protect it below.",
		amount, truncateUTF8(question, 60), outcome, sessionHigh, ask, orderID, capLeft)
}

// snipeCapNote is the one-liner appended to the manual alert when the daily
// auto-snipe cap blocked the buy.
const snipeCapNote = "\n\n⚠️ Daily auto-snipe cap reached — manual taps only until UTC midnight."

// snipeSkipNote explains why the v2 auto-buy did not run, appended to the
// manual alert. Issue #50 follow-up: the degraded alert used to carry the v1
// "bot never buys on its own" copy with no reason — contradictory when a
// parallel evaluation's auto-buy confirmation landed seconds later.
func snipeSkipNote(res snipeBuyResult) string {
	var reason string
	switch res.outcome {
	case snipeBuyRepriced:
		reason = "the ask moved past the snipe guard before the buy"
	case snipeBuyMarketErr:
		reason = "market lookup failed"
	case snipeBuyMismatch:
		reason = "market data mismatch"
	case snipeBuyRejected:
		reason = "the order was rejected"
	case snipeBuyNoWallet:
		reason = "no trading wallet on this account"
	default:
		reason = "auto-buy unavailable"
	}
	return fmt.Sprintf("\n\n⚠️ Auto-buy skipped: %s — tap below if you still want it.", reason)
}

// snipeAutoBoughtKeyboard builds the auto-sniped message's buttons. The top-up
// rides the SAME registry entry — the auto-buy never claims it, so the normal
// snipe callback still works exactly once.
func snipeAutoBoughtKeyboard(alertID string) tgbotapi.InlineKeyboardMarkup {
	return tgbotapi.NewInlineKeyboardMarkup(
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("⚡ Add $25", fmt.Sprintf("snipe:%s:25", alertID)),
			tgbotapi.NewInlineKeyboardButtonData("🎯 Arm SL/TP", "sltp_list"),
		),
	)
}

// snipeRefuseBuy is the repricing guard: refuse when the live ask is
// unavailable or has moved above 1.5× the crash threshold since the alert.
// Pure — table-tested.
func snipeRefuseBuy(ask float64, ok bool) bool {
	return !ok || ask <= 0 || ask > live.SnipeCrashAsk*1.5
}

// snipeRepricedText builds the guard-refusal body. Pure — table-tested.
func snipeRepricedText(outcome string, ask float64, ok bool) string {
	if !ok || ask <= 0 {
		return fmt.Sprintf(
			"↩️ *Repriced — not buying*\n\n"+
				"No live ask available for %s right now. The moment has passed; "+
				"wait for a fresh alert.",
			outcome)
	}
	return fmt.Sprintf(
		"↩️ *Repriced — not buying*\n\n"+
			"%s ask is now $%.2f, above the $%.2f snipe guard. The crash "+
			"price is gone; wait for a fresh alert.",
		outcome, ask, live.SnipeCrashAsk*1.5)
}

// snipeFilledText builds the fill confirmation. Pure — table-tested.
func snipeFilledText(question, outcome string, amount float64, orderID string) string {
	return fmt.Sprintf(
		"✅ *Sniped!*\n\n"+
			"*Market:* %s\n"+
			"*Side:* Buy %s\n"+
			"*Amount:* $%.2f\n"+
			"*Order ID:* %s\n\n"+
			"Protect it? Arm SL/TP below.",
		truncateUTF8(question, 60), outcome, amount, orderID)
}

// SetSnipe wires the comeback-snipe watcher and the price feed's ask source
// into the bot. Must be called after construction (the watcher depends on the
// bot as its notifier — same cycle-break as SetSLTPMonitor).
func (b *Bot) SetSnipe(w *live.SnipeWatcher, feed SnipeAskSource) {
	if w != nil { // guard the typed-nil-in-interface trap
		b.snipeWatcher = w
	}
	b.snipeFeed = feed
}

// NotifySnipeAlert implements live.SnipeNotifier: registers the alert for
// one-tap buying, attempts the v2 fixed-stake auto-buy, and DMs the recipient.
// The alert is always delivered — the message is picked from the auto-buy's
// status and the send is unconditional, so no failure path can block it.
func (b *Bot) NotifySnipeAlert(chatID int64, market live.SnipeMarket, sessionHigh, ask float64) {
	alertID := b.snipeAlerts.add(market)
	text, keyboard := b.snipeAlertMessage(chatID, alertID, market, sessionHigh, ask)
	b.sendMessageWithKeyboard(chatID, text, keyboard)
}

// snipeAlertMessage picks the alert body and buttons: the auto-sniped
// confirmation when the auto-buy filled, otherwise the unchanged manual alert
// (with a one-line note when the daily cap was the blocker).
func (b *Bot) snipeAlertMessage(chatID int64, alertID string, market live.SnipeMarket, sessionHigh, ask float64) (string, tgbotapi.InlineKeyboardMarkup) {
	res, capLeft, status := b.snipeAutoBuy(chatID, market)
	switch status {
	case snipeAutoBought:
		return snipeAutoBoughtText(market.Question, market.Outcome, sessionHigh, ask, snipeAutoBuyUSD, res.orderID, capLeft),
			snipeAutoBoughtKeyboard(alertID)
	case snipeAutoCapReached:
		return snipeAlertText(market.Question, market.Outcome, sessionHigh, ask) + snipeCapNote,
			snipeKeyboard(alertID)
	default:
		return snipeAlertText(market.Question, market.Outcome, sessionHigh, ask) + snipeSkipNote(res),
			snipeKeyboard(alertID)
	}
}

// snipeAutoStatus classifies a snipeAutoBuy attempt for messaging.
type snipeAutoStatus int

const (
	snipeAutoBought     snipeAutoStatus = iota
	snipeAutoCapReached                 // daily cap blocked the buy — noted on the alert
	snipeAutoSkipped                    // no wallet / guard / market / buy failure — plain manual alert
)

// snipeAutoBuy attempts the fixed $10 auto-buy for one alert recipient (the
// chat ID is the recipient's telegram ID — alerts are DMs). It never claims
// the registry entry, so the Add-$25 tap later claims it normally and
// double-tap safety is preserved. Cap headroom is reserved before the buy so
// racing alerts cannot overshoot the daily cap; a failed buy refunds it.
func (b *Bot) snipeAutoBuy(chatID int64, market live.SnipeMarket) (snipeBuyResult, float64, snipeAutoStatus) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	user, err := b.userRepo.GetByTelegramID(ctx, chatID)
	if err != nil || user == nil {
		return snipeBuyResult{outcome: snipeBuyNoWallet}, 0, snipeAutoSkipped
	}
	capLeft, ok := b.snipeSpend.reserve(chatID, snipeAutoBuyUSD)
	if !ok {
		log.Printf("Snipe auto-buy: cap reached chat=%d", chatID)
		return snipeBuyResult{}, capLeft, snipeAutoCapReached
	}
	entry := snipeAlertEntry{
		tokenID:  market.TokenID,
		marketID: market.MarketID,
		question: market.Question,
		outcome:  market.Outcome,
	}
	res := b.snipeGuardedBuy(ctx, user, entry, snipeAutoBuyUSD)
	if res.outcome != snipeBuyFilled {
		b.snipeSpend.release(chatID, snipeAutoBuyUSD)
		log.Printf("Snipe auto-buy: skipped chat=%d token=%.12s… reason=%d err=%v msg=%s",
			chatID, market.TokenID, res.outcome, res.err, res.errorMsg)
		return res, 0, snipeAutoSkipped
	}
	log.Printf("Snipe auto-buy: accepted chat=%d token=%.12s… $%.0f order=%s cap-left=$%.2f",
		chatID, market.TokenID, snipeAutoBuyUSD, res.orderID, capLeft)
	return res, capLeft, snipeAutoBought
}

// snipeBuyOutcome classifies a snipeGuardedBuy attempt.
type snipeBuyOutcome int

const (
	snipeBuyFilled    snipeBuyOutcome = iota
	snipeBuyRepriced                  // guard refusal: fresh ask missing or repriced
	snipeBuyMarketErr                 // market fetch failed
	snipeBuyMismatch                  // alert token not in the market's clobTokenIds
	snipeBuyRejected                  // executor reported Success=false
	snipeBuyNoWallet                  // recipient has no trading wallet — buy path never attempted
)

// snipeBuyResult carries what each caller needs to message the user.
type snipeBuyResult struct {
	outcome  snipeBuyOutcome
	ask      float64 // fresh ask at guard time (snipeBuyRepriced)
	askOK    bool
	err      error  // snipeBuyMarketErr
	orderID  string // snipeBuyFilled
	errorMsg string // snipeBuyRejected
}

// snipeGuardedBuy is the shared guarded snipe buy behind both the one-tap
// callback and the v2 auto-buy: repricing guard on a fresh ask, market fetch,
// token-index verify, buy, MarkBought on success. Claiming the registry entry
// and cap accounting stay with the callers.
func (b *Bot) snipeGuardedBuy(ctx context.Context, user *database.User, entry snipeAlertEntry, amount float64) snipeBuyResult {
	var ask float64
	var ok bool
	if b.snipeFeed != nil {
		ask, ok = b.snipeFeed.BestAsk(entry.tokenID)
	}
	if snipeRefuseBuy(ask, ok) {
		return snipeBuyResult{outcome: snipeBuyRepriced, ask: ask, askOK: ok}
	}

	mc := b.snipeMarkets
	if mc == nil {
		mc = polymarket.NewMarketClient()
	}
	market, err := fetchSnipeMarket(ctx, mc, entry.marketID)
	if err != nil {
		return snipeBuyResult{outcome: snipeBuyMarketErr, err: err}
	}
	idx := -1
	for i, id := range market.GetClobTokenIds() {
		if id == entry.tokenID {
			idx = i
			break
		}
	}
	if idx < 0 {
		return snipeBuyResult{outcome: snipeBuyMismatch}
	}

	exec := b.snipeBuyExec
	if exec == nil {
		exec = func(ctx context.Context, user *database.User, market *polymarket.GammaMarket, idx int, amount float64) *polymarket.TradeResult {
			return b.executeBuyOrderByIndex(ctx, user, market, idx, amount, 0)
		}
	}
	result := exec(ctx, user, market, idx, amount)
	if !result.Success {
		return snipeBuyResult{outcome: snipeBuyRejected, errorMsg: result.ErrorMsg}
	}
	if b.snipeWatcher != nil {
		b.snipeWatcher.MarkBought(entry.tokenID)
	}
	return snipeBuyResult{outcome: snipeBuyFilled, ask: ask, orderID: result.OrderID}
}

// handleSnipeCallback executes the one-tap snipe buy.
// Callback format: "snipe:<alertID>:<10|25>".
func (b *Bot) handleSnipeCallback(ctx context.Context, update *tgbotapi.Update) {
	chatID := update.CallbackQuery.Message.Chat.ID
	messageID := update.CallbackQuery.Message.MessageID
	userID := update.CallbackQuery.From.ID

	parts := strings.Split(update.CallbackQuery.Data, ":")
	if len(parts) != 3 || (parts[2] != "10" && parts[2] != "25") {
		return
	}
	alertID := parts[1]
	amount, _ := strconv.ParseFloat(parts[2], 64)

	user, err := b.userRepo.GetByTelegramID(ctx, userID)
	if err != nil || user == nil {
		b.sendMessage(chatID, "❌ You need to set up a wallet first. Use /start")
		return
	}

	// Claim before anything irreversible: the first tap wins, every later tap
	// (double-tap, second button) answers without buying.
	entry, status := b.snipeAlerts.claim(alertID)
	switch status {
	case snipeAlertUsed:
		b.sendMessage(chatID, "⚡ *Already handled* — this snipe was acted on. Not buying twice.")
		return
	case snipeAlertExpired:
		b.sendMessage(chatID, "⚡ *Snipe expired* — this alert is stale. Wait for a fresh one.")
		return
	}

	question := truncateUTF8(entry.question, 60)
	b.editMessage(chatID, messageID, fmt.Sprintf(
		"⏳ *Sniping...*\n\n*Market:* %s\n*Side:* Buy %s\n*Amount:* $%.2f",
		question, entry.outcome, amount))

	// Guard, market verify, buy — shared with the auto-buy path.
	res := b.snipeGuardedBuy(ctx, user, entry, amount)
	switch res.outcome {
	case snipeBuyRepriced:
		log.Printf("Snipe guard: refuse user=%d token=%s ask=%.4f ok=%v", userID, entry.tokenID, res.ask, res.askOK)
		b.editMessage(chatID, messageID, snipeRepricedText(entry.outcome, res.ask, res.askOK))
	case snipeBuyMarketErr:
		b.editMessage(chatID, messageID, fmt.Sprintf("❌ Snipe failed: couldn't fetch market: %v", res.err))
	case snipeBuyMismatch:
		b.editMessage(chatID, messageID, "❌ Snipe failed: market data mismatch — not buying.")
	case snipeBuyRejected:
		b.editMessage(chatID, messageID, fmt.Sprintf(
			"❌ *Snipe failed*\n\n*Market:* %s\n*Error:* %s",
			question, res.errorMsg))
	case snipeBuyFilled:
		keyboard := tgbotapi.NewInlineKeyboardMarkup(
			tgbotapi.NewInlineKeyboardRow(
				tgbotapi.NewInlineKeyboardButtonData("🎯 Arm SL/TP", "sltp_list"),
			),
		)
		b.editMessageWithKeyboard(chatID, messageID,
			snipeFilledText(entry.question, entry.outcome, amount, res.orderID), keyboard)
	}
}

// snipeMarketFromGamma builds the watcher's per-token metadata from a Gamma
// market. The outcome is resolved by the token's index; fallbackOutcome
// covers responses without clobTokenIds.
func snipeMarketFromGamma(market *polymarket.GammaMarket, tokenID, fallbackOutcome string) live.SnipeMarket {
	outcome := fallbackOutcome
	outcomes := market.GetOutcomes()
	for i, id := range market.GetClobTokenIds() {
		if id == tokenID && i < len(outcomes) {
			outcome = outcomes[i]
			break
		}
	}
	return live.SnipeMarket{
		TokenID:   tokenID,
		MarketID:  market.ID,
		Question:  market.Question,
		Outcome:   outcome,
		GameStart: market.GetGameStartTime(),
	}
}

// registerSnipeArmed resolves an armed token's market metadata (game start,
// title, outcome) from Gamma and registers it with the watcher. Runs in the
// caller's goroutine — call with `go` from handlers.
func (b *Bot) registerSnipeArmed(marketID, tokenID, fallbackOutcome string) {
	if b.snipeWatcher == nil || marketID == "" || tokenID == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	market, err := fetchSnipeMarket(ctx, polymarket.NewMarketClient(), marketID)
	if err != nil {
		log.Printf("Snipe: fetch market %s for armed token: %v", marketID, err)
		return
	}
	b.snipeWatcher.WatchArmed(snipeMarketFromGamma(market, tokenID, fallbackOutcome))
}

// SeedSnipeArmed registers all armed tokens with the snipe watcher at boot
// (asynchronously — Gamma lookups must not block startup).
func (b *Bot) SeedSnipeArmed() {
	if b.snipeWatcher == nil {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		tokenIDs, err := b.sltpArmRepo.ListArmedTokenIDs(ctx)
		if err != nil {
			log.Printf("Snipe: seed armed tokens: %v", err)
			return
		}
		for _, tokenID := range tokenIDs {
			arms, err := b.sltpArmRepo.ListArmedByToken(ctx, tokenID)
			if err != nil || len(arms) == 0 || arms[0].MarketID == nil {
				continue
			}
			b.registerSnipeArmed(*arms[0].MarketID, tokenID, string(arms[0].Outcome))
		}
		log.Printf("Snipe: seeded %d armed token(s)", len(tokenIDs))
	}()
}

// registerSnipeHeld registers fetched positions as held-token watches with a
// TTL. Metadata is fetched once per market and skipped entirely for tokens the
// watcher already knows (TTL renewal only). Call with `go` from handlers.
func (b *Bot) registerSnipeHeld(chatID int64, positions []*polymarket.Position) {
	if b.snipeWatcher == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	marketClient := polymarket.NewMarketClient()
	markets := make(map[string]*polymarket.GammaMarket)
	for _, pos := range positions {
		if pos.TokenID == "" {
			continue
		}
		if b.snipeWatcher.RenewHeld(chatID, pos.TokenID, live.SnipeHeldTTL) {
			continue
		}
		if pos.MarketID == "" {
			continue
		}
		market := markets[pos.MarketID]
		if market == nil {
			m, err := fetchSnipeMarket(ctx, marketClient, pos.MarketID)
			if err != nil {
				log.Printf("Snipe: fetch market %s for held token: %v", pos.MarketID, err)
				continue
			}
			market = m
			markets[pos.MarketID] = m
		}
		b.snipeWatcher.WatchHeld(chatID, snipeMarketFromGamma(market, pos.TokenID, pos.Outcome), live.SnipeHeldTTL)
	}
}

// fetchSnipeMarket fetches a Gamma market for a held position. The Data API's
// position "market ID" is the 0x-prefixed condition ID, which Gamma's
// /markets/{id} path form rejects with 422 (issue #33) — hashes must go
// through the ?condition_id= query; numeric Gamma ids keep the path form.
func fetchSnipeMarket(ctx context.Context, mc *polymarket.MarketClient, id string) (*polymarket.GammaMarket, error) {
	if !strings.HasPrefix(id, "0x") {
		return mc.GetMarketByID(ctx, id)
	}
	m, err := mc.GetMarketByConditionID(ctx, id)
	if err != nil {
		return nil, err
	}
	// Gamma's ?condition_id= form omits gameStartTime entirely (the by-slug
	// form has it). Without a game start the in-play gate never opens, so
	// refetch by slug to enrich; on failure keep the unenriched market —
	// watching without alerting still beats not watching.
	if m.GetGameStartTime().IsZero() && m.Slug != "" {
		if enriched, err := mc.GetMarketBySlug(ctx, m.Slug); err == nil && enriched != nil {
			return enriched, nil
		}
	}
	return m, nil
}

// snipeRegisterHeldForUser fetches the user's positions and registers them as
// held-token watches. Used by the /positions flows, which only produce a
// rendered summary — the raw positions come from a second Data API call here,
// off the render path. Call with `go`.
func (b *Bot) snipeRegisterHeldForUser(chatID int64, proxyAddr common.Address) {
	if b.snipeWatcher == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	positions, err := polymarket.NewUnifiedPositionScanner().GetPositions(ctx, proxyAddr)
	if err != nil {
		log.Printf("Snipe: fetch positions for held registration: %v", err)
		return
	}
	b.registerSnipeHeld(chatID, positions)
}
