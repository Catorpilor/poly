package telegram

import (
	"context"
	"fmt"
	"log"
	"regexp"
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
// a fresh best ask for the repricing guard and a fresh best bid for the in-band
// corpse-spread gate.
type SnipeAskSource interface {
	BestAsk(tokenID string) (float64, bool)
	BestBid(tokenID string) (float64, bool)
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
	// snipeDeepBuyUSD is the fixed stake auto-bought on a Deep Crash fire
	// (ADR 0007) — deliberately half the in-band stake: the base rate below
	// the corpse floor is worse and the payoff floor is 33×.
	snipeDeepBuyUSD = 5.0
	// snipeDeepDailyCapUSD bounds Deep Crash spend in its own pool, isolating
	// corpse false-positives from the main band's budget.
	snipeDeepDailyCapUSD = 20.0
)

// Comeback Snipe auto-buy gates (feat/snipe-auto-buy-gates). Alerts and manual
// tap buttons are NEVER gated — each gate only converts an auto-buy into the
// existing alert-only fallback, so the user still judges the game and taps.

// snipeEsportsMarkers is the case-insensitive allowlist that classifies a
// market as esports for the sport gate (Gate 1). Matched as whole words (word
// boundaries) against the market question (and event slugs when a caller has
// them) — bare substrings would false-positive on names like Lecce ("lec") or
// Alec, and a false positive auto-buys a non-esports corpse. The ledger's
// winners are all fast-bouncing esports crashes; every tennis tap went 0/5 and
// slow decided-sport crashes lost — so this is an allowlist, and non-esports or
// unclassifiable markets default to alert-only. Tunable: append a marker as new
// titles/leagues appear. Observed Gamma question prefixes: "Counter-Strike:",
// "Dota 2:", "LoL:", "Valorant:"; observed slugs: cs2-, dota2-, lol-, val-,
// lec-.
var snipeEsportsMarkers = []string{
	"counter-strike", "cs2", "cs:go",
	"dota", "dota2",
	"league of legends", "lol", "lec", "lck", "lpl",
	"valorant",
	"overwatch",
	"rocket league",
	"starcraft",
	"honor of kings", "king of glory",
	"mobile legends",
}

// snipeEsportsPattern compiles the marker allowlist into one word-bounded
// alternation, so "lec-…" slugs and "LoL:" prefixes match while "Lecce" and
// "Alec" do not.
var snipeEsportsPattern = func() *regexp.Regexp {
	quoted := make([]string, len(snipeEsportsMarkers))
	for i, m := range snipeEsportsMarkers {
		quoted[i] = regexp.QuoteMeta(m)
	}
	return regexp.MustCompile(`\b(?:` + strings.Join(quoted, "|") + `)\b`)
}()

// snipeIsEsports reports whether any of texts (the market question, plus event
// slugs when the caller has them) carries an esports marker. Unknown or empty ⇒
// false: the sport gate's conservative default is alert-only, because a false
// positive auto-buys a non-esports corpse — the exact loss pattern the gate
// exists to stop — while a false negative only degrades to manual taps.
func snipeIsEsports(texts ...string) bool {
	for _, t := range texts {
		if snipeEsportsPattern.MatchString(strings.ToLower(t)) {
			return true
		}
	}
	return false
}

// snipeCorpseSpreadRatio is the own-side spread that separates a live panic
// from a decided-game corpse (Gate 2, in-band $10 only): a fresh best bid below
// ask/ratio is the corpse signature (2026-08-07 SBV Excelsior: bid 0.022 / ask
// 0.100 ≈ 0.22×; 2026-08-11 JDG–EDG called the same way and lost).
const snipeCorpseSpreadRatio = 3.0

// snipeCorpseGeometry reports whether the fresh book shows corpse spread —
// bid < ask/snipeCorpseSpreadRatio. A missing or non-positive bid is treated as
// corpse geometry (conservative: skip the auto-buy, still alert). Strictly
// less-than, so bid == ask/ratio proceeds. Pure — table-tested.
func snipeCorpseGeometry(bid float64, bidOK bool, ask float64) bool {
	if !bidOK || bid <= 0 {
		return true
	}
	return bid < ask/snipeCorpseSpreadRatio
}

// snipeBoughtRecord tracks, per recipient, the tokens the bot snipe-bought via
// the in-band auto-buy or a one-tap buy. In-memory, never cleared during a run
// — matches end with their markets, so staleness is bounded. Gate 3 (Deep
// holdings) reads it so a $5 Deep Crash top-up never funds a token the in-band
// buy already holds (all 11 losing deep fires were top-ups onto held corpses).
// This is the lag-free half of the holdings check; the Data API positions read
// is the other half, and it lags fills by seconds — hence both.
type snipeBoughtRecord struct {
	mu     sync.Mutex
	bought map[int64]map[string]bool // chatID -> tokenID -> true
}

func newSnipeBoughtRecord() *snipeBoughtRecord {
	return &snipeBoughtRecord{bought: make(map[int64]map[string]bool)}
}

// mark records that chatID holds tokenID from a snipe buy.
func (r *snipeBoughtRecord) mark(chatID int64, tokenID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.bought[chatID] == nil {
		r.bought[chatID] = make(map[string]bool)
	}
	r.bought[chatID][tokenID] = true
}

// held reports whether chatID already snipe-bought tokenID.
func (r *snipeBoughtRecord) held(chatID int64, tokenID string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.bought[chatID][tokenID]
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

// snipeSpendLedger accumulates per-chat auto-snipe spend for the current UTC
// day. In-memory only — a restart resets the cap. That's a soft rail by
// design: it bounds a runaway alert day, not an adversary. Reservation-style:
// reserve before the buy so racing alerts cannot overshoot the cap, release on
// failure so only successful buys consume it.
type snipeSpendLedger struct {
	mu    sync.Mutex
	cap   float64 // per-chat daily budget this ledger enforces
	day   string  // UTC date of the amounts in spent; rollover clears them
	spent map[int64]float64
	now   func() time.Time
}

func newSnipeSpendLedger(capUSD float64) *snipeSpendLedger {
	return &snipeSpendLedger{cap: capUSD, spent: make(map[int64]float64), now: time.Now}
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
	if l.spent[chatID]+amount > l.cap {
		return l.cap - l.spent[chatID], false
	}
	l.spent[chatID] += amount
	return l.cap - l.spent[chatID], true
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
	case snipeBuyNotEsports:
		reason = "this isn't an esports market (auto-buy is esports-only)"
	case snipeBuyCorpseSpread:
		reason = "the book shows a decided-game spread (fresh bid far below ask)"
	case snipeBuyDeepHeld:
		reason = "you already hold this token — not topping up a held position"
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

// snipeDeepText builds the Deep Crash body — blunt about the base rate: this
// zone is usually corpses; the prior in-band alert is the only reason we're
// here at all. Pure — table-tested.
func snipeDeepText(question, outcome string, alertAsk, ask float64) string {
	multiple := 0.0
	if ask > 0 {
		multiple = 1 / ask
	}
	return fmt.Sprintf(
		"💎 *Deep Crash*\n\n"+
			"*%s*\n"+
			"*Outcome:* %s\n\n"+
			"Alerted at $%.2f earlier — now $%.3f ask. %.0f× if it turns.\n\n"+
			"⚠️ Corpse territory: games this cheap are usually over. Anything "+
			"beyond the system's stake is your read of the game state.",
		truncateUTF8(question, 60), outcome, alertAsk, ask, multiple)
}

// snipeDeepBoughtText builds the Deep Crash auto-buy confirmation. Pure —
// table-tested.
func snipeDeepBoughtText(question, outcome string, alertAsk, ask, amount float64, orderID string, poolLeft float64) string {
	return snipeDeepText(question, outcome, alertAsk, ask) + fmt.Sprintf(
		"\n\n⚡ *$%.0f auto-bought* (deep pool)\n"+
			"*Order ID:* %s\n"+
			"Deep pool left today: $%.0f",
		amount, orderID, poolLeft)
}

// snipeDeepCapNote is appended to the Deep Crash alert when the deep pool
// blocked the buy.
const snipeDeepCapNote = "\n\n⚠️ Deep pool exhausted — manual taps only until UTC midnight."

// NotifySnipeDeepCrash implements the Deep Crash tier of live.SnipeNotifier
// (ADR 0007): registers the alert for one-tap buying, attempts the $5
// auto-buy from the deep pool behind the strict zone guard, and DMs the
// recipient. Delivery is unconditional, exactly like the in-band tier.
func (b *Bot) NotifySnipeDeepCrash(chatID int64, market live.SnipeMarket, sessionHigh, ask, alertAsk float64, sinceAlert time.Duration) {
	alertID := b.snipeAlerts.add(market)
	text, keyboard := b.snipeDeepMessage(chatID, alertID, market, ask, alertAsk)
	b.sendMessageWithKeyboard(chatID, text, keyboard)
}

// snipeDeepMessage picks the Deep Crash body and buttons from the auto-buy's
// status, mirroring snipeAlertMessage.
func (b *Bot) snipeDeepMessage(chatID int64, alertID string, market live.SnipeMarket, ask, alertAsk float64) (string, tgbotapi.InlineKeyboardMarkup) {
	res, poolLeft, status := b.snipeDeepAutoBuy(chatID, market)
	base := snipeDeepText(market.Question, market.Outcome, alertAsk, ask)
	switch status {
	case snipeAutoBought:
		return snipeDeepBoughtText(market.Question, market.Outcome, alertAsk, ask, snipeDeepBuyUSD, res.orderID, poolLeft),
			snipeAutoBoughtKeyboard(alertID)
	case snipeAutoCapReached:
		return base + snipeDeepCapNote, snipeKeyboard(alertID)
	default:
		return base + snipeSkipNote(res), snipeKeyboard(alertID)
	}
}

// snipeDeepAutoBuy attempts the fixed $5 Deep Crash buy from the deep pool,
// mirroring snipeAutoBuy's reserve-then-refund but with the strict zone guard.
func (b *Bot) snipeDeepAutoBuy(chatID int64, market live.SnipeMarket) (snipeBuyResult, float64, snipeAutoStatus) {
	// Gate 1 (sport gate): the deep tier is esports-only too.
	if !snipeIsEsports(market.Question) {
		log.Printf("Snipe deep-buy: sport-gated chat=%d token=%.12s… q=%q", chatID, market.TokenID, market.Question)
		return snipeBuyResult{outcome: snipeBuyNotEsports}, 0, snipeAutoSkipped
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	user, err := b.userRepo.GetByTelegramID(ctx, chatID)
	if err != nil || user == nil {
		return snipeBuyResult{outcome: snipeBuyNoWallet}, 0, snipeAutoSkipped
	}

	// Gate 3 (deep holdings gate): never top up a token the recipient already
	// holds — all 11 losing deep fires were top-ups onto held corpses. The deep
	// buy is only a catch-up entry for when the in-band buy never funded. Two
	// independent checks; either one showing exposure ⇒ skip.
	if b.snipeBought != nil && b.snipeBought.held(chatID, market.TokenID) {
		log.Printf("Snipe deep-buy: holdings-gated (record) chat=%d token=%.12s…", chatID, market.TokenID)
		return snipeBuyResult{outcome: snipeBuyDeepHeld}, 0, snipeAutoSkipped
	}
	if held, ok := b.snipeHoldsPosition(ctx, user, market.TokenID); ok && held {
		log.Printf("Snipe deep-buy: holdings-gated (positions) chat=%d token=%.12s…", chatID, market.TokenID)
		return snipeBuyResult{outcome: snipeBuyDeepHeld}, 0, snipeAutoSkipped
	}
	// ok==false means the positions read failed: fall back to the record alone,
	// which is empty here (checked above) ⇒ allow the buy (the deep pool is
	// small and capped).

	poolLeft, ok := b.snipeDeepSpend.reserve(chatID, snipeDeepBuyUSD)
	if !ok {
		log.Printf("Snipe deep-buy: pool reached chat=%d", chatID)
		return snipeBuyResult{}, poolLeft, snipeAutoCapReached
	}
	entry := snipeAlertEntry{
		tokenID:  market.TokenID,
		marketID: market.MarketID,
		question: market.Question,
		outcome:  market.Outcome,
	}
	res := b.snipeGuardedBuyRefuse(ctx, user, entry, snipeDeepBuyUSD, snipeRefuseDeepBuy, false)
	if res.outcome != snipeBuyFilled {
		b.snipeDeepSpend.release(chatID, snipeDeepBuyUSD)
		log.Printf("Snipe deep-buy: skipped chat=%d token=%.12s… reason=%d err=%v msg=%s",
			chatID, market.TokenID, res.outcome, res.err, res.errorMsg)
		return res, 0, snipeAutoSkipped
	}
	log.Printf("Snipe deep-buy: accepted chat=%d token=%.12s… $%.0f order=%s pool-left=$%.2f",
		chatID, market.TokenID, snipeDeepBuyUSD, res.orderID, poolLeft)
	return res, poolLeft, snipeAutoBought
}

// snipeHoldsPosition reports whether the user's proxy holds shares of tokenID
// (Gate 3b). It mirrors snipeRegisterHeldForUser's Data API read, using the
// same injectable snipePositions seam. Returns ok=false when the position can't
// be determined (no proxy, or the Data API errored) — the caller falls back to
// the local bought record.
func (b *Bot) snipeHoldsPosition(ctx context.Context, user *database.User, tokenID string) (held, ok bool) {
	if user.ProxyAddress == "" {
		return false, false
	}
	scanner := b.snipePositions
	if scanner == nil {
		scanner = polymarket.NewUnifiedPositionScanner()
	}
	positions, err := scanner.GetPositions(ctx, common.HexToAddress(user.ProxyAddress))
	if err != nil {
		log.Printf("Snipe deep-buy: positions read failed for holdings gate: %v", err)
		return false, false
	}
	for _, pos := range positions {
		if pos.TokenID == tokenID && pos.Shares != nil && pos.Shares.Sign() > 0 {
			return true, true
		}
	}
	return false, true
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
	// Gate 1 (sport gate): auto-buy only esports; non-esports and
	// unclassifiable markets stay alert-only. Checked before any wallet lookup
	// or cap reservation — the classification alone decides.
	if !snipeIsEsports(market.Question) {
		log.Printf("Snipe auto-buy: sport-gated chat=%d token=%.12s… q=%q", chatID, market.TokenID, market.Question)
		return snipeBuyResult{outcome: snipeBuyNotEsports}, 0, snipeAutoSkipped
	}

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
	// corpseGuard=true adds Gate 2 to the shared guarded buy.
	res := b.snipeGuardedBuyRefuse(ctx, user, entry, snipeAutoBuyUSD, snipeRefuseBuy, true)
	if res.outcome != snipeBuyFilled {
		b.snipeSpend.release(chatID, snipeAutoBuyUSD)
		log.Printf("Snipe auto-buy: skipped chat=%d token=%.12s… reason=%d err=%v msg=%s",
			chatID, market.TokenID, res.outcome, res.err, res.errorMsg)
		return res, 0, snipeAutoSkipped
	}
	// Record the holding so a later Deep Crash fire on this token is
	// holdings-gated (Gate 3a). Only the in-band auto-buy and the one-tap feed
	// this record — never the deep tier itself.
	if b.snipeBought != nil {
		b.snipeBought.mark(chatID, market.TokenID)
	}
	log.Printf("Snipe auto-buy: accepted chat=%d token=%.12s… $%.0f order=%s cap-left=$%.2f",
		chatID, market.TokenID, snipeAutoBuyUSD, res.orderID, capLeft)
	return res, capLeft, snipeAutoBought
}

// snipeBuyOutcome classifies a snipeGuardedBuy attempt.
type snipeBuyOutcome int

const (
	snipeBuyFilled       snipeBuyOutcome = iota
	snipeBuyRepriced                     // guard refusal: fresh ask missing or repriced
	snipeBuyMarketErr                    // market fetch failed
	snipeBuyMismatch                     // alert token not in the market's clobTokenIds
	snipeBuyRejected                     // executor reported Success=false
	snipeBuyNoWallet                     // recipient has no trading wallet — buy path never attempted
	snipeBuyNotEsports                   // sport gate: non-esports/unclassifiable — auto-buy is esports-only
	snipeBuyCorpseSpread                 // corpse-spread gate: fresh bid far below ask (decided-game signature)
	snipeBuyDeepHeld                     // deep holdings gate: recipient already holds the crashed token
)

// snipeBuyResult carries what each caller needs to message the user.
type snipeBuyResult struct {
	outcome  snipeBuyOutcome
	ask      float64 // fresh ask at guard time (snipeBuyRepriced)
	askOK    bool
	bid      float64 // fresh bid at guard time (snipeBuyCorpseSpread)
	err      error   // snipeBuyMarketErr
	orderID  string  // snipeBuyFilled
	errorMsg string  // snipeBuyRejected
}

// snipeRefuseDeepBuy is the Deep Crash buy guard (ADR 0007): strict zone
// check — the $5 executes only while the fresh ask is still inside
// [SnipeDeepFloor, SnipeMinAsk). A bounce out of the zone means the dip is
// gone (the in-band tranche is already riding it); below the floor is dust.
// Pure — table-tested.
func snipeRefuseDeepBuy(ask float64, ok bool) bool {
	return !ok || ask < live.SnipeDeepFloor || ask >= live.SnipeMinAsk
}

// snipeGuardedBuy is the shared guarded snipe buy behind the one-tap callback
// (manual taps are never gated): repricing guard on a fresh ask, market fetch,
// token-index verify, buy, MarkBought on success. Claiming the registry entry
// and cap accounting stay with the callers.
func (b *Bot) snipeGuardedBuy(ctx context.Context, user *database.User, entry snipeAlertEntry, amount float64) snipeBuyResult {
	return b.snipeGuardedBuyRefuse(ctx, user, entry, amount, snipeRefuseBuy, false)
}

// snipeGuardedBuyRefuse is snipeGuardedBuy with a caller-chosen repricing guard
// (the Deep Crash tier substitutes its strict zone check) and an optional
// corpse-spread gate (Gate 2, in-band $10 only). When corpseGuard is set, the
// same fresh-book read that feeds the repricing guard also reads the best bid
// and skips the buy on corpse geometry.
func (b *Bot) snipeGuardedBuyRefuse(ctx context.Context, user *database.User, entry snipeAlertEntry, amount float64, refuse func(ask float64, ok bool) bool, corpseGuard bool) snipeBuyResult {
	var ask, bid float64
	var ok, bidOK bool
	if b.snipeFeed != nil {
		ask, ok = b.snipeFeed.BestAsk(entry.tokenID)
		if corpseGuard {
			bid, bidOK = b.snipeFeed.BestBid(entry.tokenID)
		}
	}
	if refuse(ask, ok) {
		return snipeBuyResult{outcome: snipeBuyRepriced, ask: ask, askOK: ok}
	}
	if corpseGuard && snipeCorpseGeometry(bid, bidOK, ask) {
		log.Printf("Snipe auto-buy: corpse-spread skip token=%.12s… bid=%.3f ask=%.3f (< ask/%.0f)",
			entry.tokenID, bid, ask, snipeCorpseSpreadRatio)
		return snipeBuyResult{outcome: snipeBuyCorpseSpread, ask: ask, askOK: ok, bid: bid}
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
		// Record the holding so a later Deep Crash fire on this token is
		// holdings-gated (Gate 3a), same as the in-band auto-buy.
		if b.snipeBought != nil {
			b.snipeBought.mark(chatID, entry.tokenID)
		}
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

// snipeRegisterBoughtToken registers a just-bought token as a Held Watch
// directly from the Gamma market the buy already used — no Data API round-trip,
// so it can't lose the race against the API's fill indexing (issue #67: a token
// bought at 09:35:16 wasn't indexed when the portfolio refetch ran ~1s later,
// so its 09:38 crash to 0.115 alerted nobody). These handlers fetch the market
// by numeric ID, whose response carries gameStartTime (only the ?condition_id=
// form drops it, per fetchSnipeMarket), so the resulting SnipeMarket is fully
// in-play-gated. In-memory and cheap — call inline, not in a goroutine, and
// keep the positions refetch as secondary rescue for older holdings. Never
// MarkBought: a manual buy is not a snipe fill.
func (b *Bot) snipeRegisterBoughtToken(chatID int64, market *polymarket.GammaMarket, idx int) {
	if b.snipeWatcher == nil || market == nil {
		return
	}
	tokenIDs := market.GetClobTokenIds()
	if idx < 0 || idx >= len(tokenIDs) {
		return
	}
	tokenID := tokenIDs[idx]
	if tokenID == "" {
		return
	}
	outcome := ""
	if outcomes := market.GetOutcomes(); idx < len(outcomes) {
		outcome = outcomes[idx]
	}
	b.snipeWatcher.WatchHeld(chatID, snipeMarketFromGamma(market, tokenID, outcome), live.SnipeHeldTTL)
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
	scanner := b.snipePositions
	if scanner == nil {
		scanner = polymarket.NewUnifiedPositionScanner()
	}
	positions, err := scanner.GetPositions(ctx, proxyAddr)
	if err != nil {
		log.Printf("Snipe: fetch positions for held registration: %v", err)
		return
	}
	b.registerSnipeHeld(chatID, positions)
}
