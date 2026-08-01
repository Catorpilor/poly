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

	"github.com/Catorpilor/poly/internal/live"
	"github.com/Catorpilor/poly/internal/polymarket"
)

// SnipeAskSource is the slice of the price feed the snipe tap handler needs:
// a fresh best ask for the repricing guard.
type SnipeAskSource interface {
	BestAsk(tokenID string) (float64, bool)
}

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
			"In-play crash on a formerly competitive side. You judge the game "+
			"state — the bot never buys on its own.",
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
	b.snipeWatcher = w
	b.snipeFeed = feed
}

// NotifySnipeAlert implements live.SnipeNotifier: registers the alert for
// one-tap buying and DMs the recipient.
func (b *Bot) NotifySnipeAlert(chatID int64, market live.SnipeMarket, sessionHigh, ask float64) {
	alertID := b.snipeAlerts.add(market)
	text := snipeAlertText(market.Question, market.Outcome, sessionHigh, ask)
	b.sendMessageWithKeyboard(chatID, text, snipeKeyboard(alertID))
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

	// Repricing guard: re-check the live ask at tap time.
	var ask float64
	var ok bool
	if b.snipeFeed != nil {
		ask, ok = b.snipeFeed.BestAsk(entry.tokenID)
	}
	if snipeRefuseBuy(ask, ok) {
		log.Printf("Snipe guard: refuse user=%d token=%s ask=%.4f ok=%v", userID, entry.tokenID, ask, ok)
		b.editMessage(chatID, messageID, snipeRepricedText(entry.outcome, ask, ok))
		return
	}

	marketClient := polymarket.NewMarketClient()
	market, err := fetchSnipeMarket(ctx, marketClient, entry.marketID)
	if err != nil {
		b.editMessage(chatID, messageID, fmt.Sprintf("❌ Snipe failed: couldn't fetch market: %v", err))
		return
	}
	idx := -1
	for i, id := range market.GetClobTokenIds() {
		if id == entry.tokenID {
			idx = i
			break
		}
	}
	if idx < 0 {
		b.editMessage(chatID, messageID, "❌ Snipe failed: market data mismatch — not buying.")
		return
	}

	question := truncateUTF8(entry.question, 60)
	b.editMessage(chatID, messageID, fmt.Sprintf(
		"⏳ *Sniping...*\n\n*Market:* %s\n*Side:* Buy %s\n*Amount:* $%.2f",
		question, entry.outcome, amount))

	result := b.executeBuyOrderByIndex(ctx, user, market, idx, amount, 0)
	if result.Success {
		if b.snipeWatcher != nil {
			b.snipeWatcher.MarkBought(entry.tokenID)
		}
		keyboard := tgbotapi.NewInlineKeyboardMarkup(
			tgbotapi.NewInlineKeyboardRow(
				tgbotapi.NewInlineKeyboardButtonData("🎯 Arm SL/TP", "sltp_list"),
			),
		)
		b.editMessageWithKeyboard(chatID, messageID,
			snipeFilledText(entry.question, entry.outcome, amount, result.OrderID), keyboard)
		return
	}
	b.editMessage(chatID, messageID, fmt.Sprintf(
		"❌ *Snipe failed*\n\n*Market:* %s\n*Error:* %s",
		question, result.ErrorMsg))
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
