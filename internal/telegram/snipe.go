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
	// WatchWalked registers a series-walked held watch (issue #102): alert-only,
	// no auto-buy at either tier. A later direct WatchHeld upgrades it; the
	// hourly re-walk never downgrades a direct entry.
	WatchWalked(chatID int64, m live.SnipeMarket, ttl time.Duration)
	// WalkedOnlyHolder reports whether chatID watches tokenID ONLY via the series
	// walk — the gate-time query both auto-buy tiers use to keep continuations
	// alert-only (issue #102).
	WalkedOnlyHolder(chatID int64, tokenID string) bool
	// EventSlugOf returns a watched token's event slug ("" when unwatched or
	// unknown) — the renewal path's key into the series walk (issue #94).
	EventSlugOf(tokenID string) string
	// RenewHeldMarket extends the holder TTL for the token AND its watched
	// siblings (issue #78), so a position refresh keeps both sides of a held
	// market alive. False ⇒ unwatched, caller must WatchHeld with metadata.
	RenewHeldMarket(chatID int64, tokenID string, ttl time.Duration) bool
	MarkBought(tokenID string)
	// SiblingTokenIDs returns other watched token IDs in the same market — the
	// boxed tier's case-3 sibling lookup.
	SiblingTokenIDs(marketID, tokenID string) []string
}

// Comeback Snipe v2 auto-buy sizing — product policy, deliberately global
// constants, not per-user configuration (see CONTEXT.md "Comeback Snipe").
const (
	// snipeAutoBuyUSD is the fixed stake auto-bought on every genuine alert.
	snipeAutoBuyUSD = 10.0
	// snipeAutoBuyDailyCapUSD bounds one recipient's auto-snipe spend per UTC
	// day.
	snipeAutoBuyDailyCapUSD = 50.0
	// snipeBoxedTrancheUSD is the stake per boxed ladder rung (issue #78): the
	// case-3 flip is bought as two $5 tranches ($5 at ≤ $0.10, $5 at ≤ $0.05)
	// instead of a single $10 at ≤ $0.10 — same $10 max exposure, half the corpse
	// bleed on the shallower rung. Each tranche draws the main daily cap.
	snipeBoxedTrancheUSD = 5.0
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

// SnipeBuyStore is the durable snipe-buy log (issue #84) — the consumer-side
// interface, mirroring ADR 0008's LiveWatchStore. The in-memory snipeBoughtRecord
// and the two spend ledgers are the runtime view; this store is what a restart
// re-reads (RestoreSnipeBuys) to close the restart-amnesia gap: an already-bought
// token re-alerting/re-buying after a reboot, and the daily cap resetting to
// zero. repositories.SnipeBuyRepository satisfies it structurally, so
// internal/telegram never imports the concrete repo. A nil store keeps the
// pre-#84 in-memory-only behavior (tests construct bots without it).
type SnipeBuyStore interface {
	Save(ctx context.Context, chatID int64, tokenID string, amountUSD float64, pool string) error
	ListSince(ctx context.Context, since time.Time) ([]*database.SnipeBuy, error)
}

// snipeBuyPersistTimeout bounds each write-through so a hung DB cannot stall the
// alert-delivery goroutine that owns the post-fill bookkeeping.
const snipeBuyPersistTimeout = 5 * time.Second

// snipeBoughtRecord tracks, per recipient, the tokens the bot snipe-bought via
// the in-band auto-buy, a one-tap buy, or a boxed tranche. In-memory, never
// cleared during a run — matches end with their markets, so staleness is
// bounded. The boxed case-3 sibling gate reads it as the lag-free half of the
// holdings check — it must not ladder a flip a prior buy already funds — with
// the Data API positions read as the lagging other half; hence both. (Until
// issue #105 the Deep Crash tier read it too; that tier is now alert-only.)
//
// It also OWNS the durable buy-log write-through (issue #84): when a store is
// wired, mark() persists one row per accepted buy so a restart can rebuild this
// record, the watcher's bought latch, and the main spend ledger. Every accept
// funnels through mark() — the retired Deep Crash tier's own write-through
// (logDeepBuy) is gone with the tier (issue #105), so 'main' is now the only
// pool ever written.
type snipeBoughtRecord struct {
	mu     sync.Mutex
	bought map[int64]map[string]bool // chatID -> tokenID -> true
	// store, when set, durably logs every accepted buy. Set once at boot
	// (SetStore) before any mark, so it is read without the mutex.
	store SnipeBuyStore
}

func newSnipeBoughtRecord() *snipeBoughtRecord {
	return &snipeBoughtRecord{bought: make(map[int64]map[string]bool)}
}

// SetStore wires the durable buy log. Optional: leaving it unset keeps the
// pre-#84 in-memory-only behavior. Follows the setter-injection pattern of the
// bot's other durable dependencies.
func (r *snipeBoughtRecord) SetStore(s SnipeBuyStore) { r.store = s }

// mark records that chatID holds tokenID from a MAIN-pool snipe buy (in-band
// auto, one-tap, or boxed tranche) and, when a store is wired, writes it through
// to the durable log as a 'main' row. amountUSD is the reserved stake so the
// main spend ledger can be reconstructed on restore. With a nil store the
// behavior is byte-identical to pre-#84.
func (r *snipeBoughtRecord) mark(chatID int64, tokenID string, amountUSD float64) {
	r.mu.Lock()
	if r.bought[chatID] == nil {
		r.bought[chatID] = make(map[string]bool)
	}
	r.bought[chatID][tokenID] = true
	store := r.store
	r.mu.Unlock()
	writeSnipeBuy(store, chatID, tokenID, amountUSD, database.SnipeBuyPoolMain)
}

// restore sets the in-memory bought flag WITHOUT writing through — used by boot
// restore to rebuild the record from the durable log; re-persisting would
// duplicate the very rows being read.
func (r *snipeBoughtRecord) restore(chatID int64, tokenID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.bought[chatID] == nil {
		r.bought[chatID] = make(map[string]bool)
	}
	r.bought[chatID][tokenID] = true
}

// listSince returns the durable buy rows at or after since (the boot-restore
// scan). Nil store ⇒ no rows, no error.
func (r *snipeBoughtRecord) listSince(ctx context.Context, since time.Time) ([]*database.SnipeBuy, error) {
	if r.store == nil {
		return nil, nil
	}
	return r.store.ListSince(ctx, since)
}

// held reports whether chatID already snipe-bought tokenID.
func (r *snipeBoughtRecord) held(chatID int64, tokenID string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.bought[chatID][tokenID]
}

// writeSnipeBuy persists one accepted snipe buy through the durable log. A nil
// store is a no-op (pre-#84 behavior). A write failure is logged LOUDLY and
// swallowed — the buy already filled and the in-memory state is authoritative
// for the session (issue #84); persistence only closes the restart-amnesia gap
// and must never fail or block the buy. Bounded by snipeBuyPersistTimeout.
func writeSnipeBuy(store SnipeBuyStore, chatID int64, tokenID string, amountUSD float64, pool string) {
	if store == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), snipeBuyPersistTimeout)
	defer cancel()
	if err := store.Save(ctx, chatID, tokenID, amountUSD, pool); err != nil {
		log.Printf("Snipe buy log: persist FAILED chat=%d token=%.12s… $%.2f pool=%s: %v — in-memory authoritative, buy stands",
			chatID, tokenID, amountUSD, pool, err)
	}
}

// snipeBoxedLatchTTL bounds how long a boxed latch stays live (issue #78 F4).
// Episodes last minutes; a latch older than this belongs to an episode the
// recipient never saw a fresh alert for (e.g. a held TTL lapse then a
// mid-episode re-register) and must not fire. Far below SnipeHeldTTL (6h).
const snipeBoxedLatchTTL = time.Hour

// snipeBoxedEntry is a recipient's per-episode boxed eligibility: one live flag
// per ladder rung, plus the set time for staleness. Per-tranche state (not one
// bool) lets an immediate case-3 buy take rung 1 now while leaving rung 2 for
// the watcher's ≤0.05 fire — $10 max, never stacked (issue #78 F3).
type snipeBoxedEntry struct {
	t1, t2 bool
	at     time.Time
}

// snipeBoxedLatch records, per recipient, which boxed rungs an alerted token is
// still eligible for this episode. It is armed at in-band alert time (case-3)
// and overwritten on every in-band alert for that (chatID, tokenID) — the
// watcher fires the in-band alert once per episode, so that per-alert overwrite
// IS the episode boundary. On a boxed tranche fire the notifier claims the rung
// from THIS latch instead of re-checking sibling holdings (issue #78): a
// mid-episode ceiling harvest of the held winner — exactly when the flip ticket
// is most wanted (ledger r72) — must not cancel the buy. A manual tap or an
// in-band fill of the token clears it (tap supersedes the ladder, F2). Entries
// carry a set time and expire after snipeBoxedLatchTTL (F4); stale entries are
// pruned opportunistically on write.
type snipeBoxedLatch struct {
	mu      sync.Mutex
	latched map[int64]map[string]*snipeBoxedEntry
	now     func() time.Time
	ttl     time.Duration
}

func newSnipeBoxedLatch() *snipeBoxedLatch {
	return &snipeBoxedLatch{
		latched: make(map[int64]map[string]*snipeBoxedEntry),
		now:     time.Now,
		ttl:     snipeBoxedLatchTTL,
	}
}

// arm overwrites (chatID, tokenID)'s boxed eligibility for a fresh episode: t1/t2
// mark which rungs the watcher's fires should still buy, stamped now. Every
// in-band alert re-arms (or clears via the caller), which is the episode
// boundary. Prunes stale entries opportunistically to bound growth.
func (l *snipeBoxedLatch) arm(chatID int64, tokenID string, t1, t2 bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.pruneLocked()
	if l.latched[chatID] == nil {
		l.latched[chatID] = make(map[string]*snipeBoxedEntry)
	}
	l.latched[chatID][tokenID] = &snipeBoxedEntry{t1: t1, t2: t2, at: l.now()}
}

// clear disarms both rungs (a manual tap or an in-band buy of the token
// supersedes the ladder, F2). Absent entry is a no-op.
func (l *snipeBoxedLatch) clear(chatID int64, tokenID string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if toks := l.latched[chatID]; toks != nil {
		delete(toks, tokenID)
		if len(toks) == 0 {
			delete(l.latched, chatID)
		}
	}
}

// claim atomically reports whether (chatID, tokenID) may still buy the given
// tranche and consumes that rung, so a duplicate fire cannot double-buy. A stale
// entry (older than ttl, F4) or a consumed/absent rung yields false.
func (l *snipeBoxedLatch) claim(chatID int64, tokenID string, tranche int) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	e := l.latched[chatID][tokenID]
	if e == nil || l.now().Sub(e.at) > l.ttl {
		return false
	}
	switch tranche {
	case 1:
		if !e.t1 {
			return false
		}
		e.t1 = false
	case 2:
		if !e.t2 {
			return false
		}
		e.t2 = false
	default:
		return false
	}
	return true
}

// eligible reports whether any rung of (chatID, tokenID) is still live and not
// stale — a read-only view for tests and messaging.
func (l *snipeBoxedLatch) eligible(chatID int64, tokenID string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	e := l.latched[chatID][tokenID]
	return e != nil && l.now().Sub(e.at) <= l.ttl && (e.t1 || e.t2)
}

// pruneLocked drops entries older than ttl. Callers hold mu.
func (l *snipeBoxedLatch) pruneLocked() {
	cutoff := l.now().Add(-l.ttl)
	for chatID, toks := range l.latched {
		for tok, e := range toks {
			if e.at.Before(cutoff) {
				delete(toks, tok)
			}
		}
		if len(toks) == 0 {
			delete(l.latched, chatID)
		}
	}
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

// seed adds amount to chatID's accumulator for the CURRENT UTC day and pins the
// ledger's day, so boot restore can re-seed the cap from the durable buy log
// (issue #84). Two rollLocked hazards it must dodge, hence the deliberate order:
//
//   - At construction l.day is empty, so the FIRST reserve/release would roll —
//     wiping a seed that ran before it. seed calls rollLocked FIRST, which sets
//     l.day to today; the later reserve then sees day == l.day and keeps the seed.
//   - A seed must never resurrect yesterday's spend after a post-midnight boot.
//     The caller (RestoreSnipeBuys) only seeds rows dated to the current UTC day,
//     so a pre-midnight row is never passed here; rollLocked pinning today makes
//     that boundary authoritative even if the very first reserve arrives seconds
//     later.
func (l *snipeSpendLedger) seed(chatID int64, amount float64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.rollLocked()
	l.spent[chatID] += amount
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
	case snipeBuyBoxedWait:
		reason = "you hold the other side — laddering the flip deep ($5 at ≤ $0.10 + $5 at ≤ $0.05)"
	case snipeBuyFutureGame:
		reason = "this game hasn't started (an earlier game of the series is still live) — the crash is series-sweep repricing, not an in-play collapse"
	case snipeBuySeriesWalked:
		// Issue #102: the market entered this recipient's watch only via the series
		// walk (holding/arming an EARLIER game registers the continuations). No auto
		// money on a market they never personally traded; the tap buttons stay live.
		// (The template appends "— tap below if you still want it.")
		reason = "this market entered your watch from a series you traded — continuations are alert-only"
	case snipeBuyManualArmed:
		// Issue #86: an active manual stop already governs this token. The auto-buy
		// would be stop-sold moments later or ride unprotected; the manual tap stays
		// live for the user who wants it anyway. (The template appends "— tap below
		// if you still want it.", completing the ratified copy.)
		reason = "your stop is already managing this token"
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

// SetSnipeBuyStore wires the durable snipe-buy log (issue #84) into the bought
// record's write-through seam. Optional — a nil store keeps in-memory-only
// behavior. Call before RestoreSnipeBuys (restore reads through it) and before
// the first buy can be accepted.
func (b *Bot) SetSnipeBuyStore(s SnipeBuyStore) {
	if b.snipeBought != nil {
		b.snipeBought.SetStore(s)
	}
}

// snipeBuyRestoreWindow is how far back boot restore rebuilds the bought record
// and the watcher's bought latch (issue #84). No comeback match spans anywhere
// near this; 24h comfortably covers any in-flight game plus a generous restart
// gap while bounding the scan (indexed on bought_at). The spend ledgers seed
// from a NARROWER window — the current UTC day only — so a pre-midnight buy
// restores the bought latch but not the (already-rolled-over) daily cap.
const snipeBuyRestoreWindow = 24 * time.Hour

// RestoreSnipeBuys rebuilds the in-memory snipe state a restart would otherwise
// lose (issue #84), from the durable buy log:
//
//	(a) the per-recipient bought record — MAIN-pool rows only, mirroring live
//	    exactly (the deep tier never marks that record);
//	(b) the watcher's token-level bought latch, via MarkBought on EVERY row
//	    (in-band, manual, boxed AND deep all call watcher.MarkBought live) —
//	    lazy-capable, so a token only watched later this session is still
//	    silenced; this latch is THE gate that suppresses re-alerts;
//	(c) both daily spend ledgers, seeded per pool from the CURRENT UTC day's
//	    rows only (the cap is a soft rail that resets at UTC midnight).
//
// Ordering is load-bearing: cmd/bot calls this BEFORE any snipe registration
// (SeedSnipeArmed, RestoreWatches) so the pending bought marks are in place
// before a token can be watched and re-evaluated — that is what closes the
// re-alert/re-buy race. A nil store (or absent record) is a no-op.
func (b *Bot) RestoreSnipeBuys(ctx context.Context) (restored int, err error) {
	return b.restoreSnipeBuysAt(ctx, time.Now().UTC())
}

// restoreSnipeBuysAt is RestoreSnipeBuys with an injected clock for tests — the
// same "now" pins both the 24h window and the current-UTC-day spend boundary.
func (b *Bot) restoreSnipeBuysAt(ctx context.Context, now time.Time) (restored int, err error) {
	if b.snipeBought == nil {
		return 0, nil
	}
	now = now.UTC()
	rows, err := b.snipeBought.listSince(ctx, now.Add(-snipeBuyRestoreWindow))
	if err != nil {
		return 0, fmt.Errorf("restore snipe buys: %w", err)
	}
	today := now.Format("2006-01-02")
	var seededMain, ignoredDeep int
	for _, row := range rows {
		// (b) Re-latch the watcher for EVERY buy tier — all called MarkBought
		// live. Lazy: a not-yet-watched token latches when it is first
		// registered. Historical 'deep' rows re-latch too: the old $5 fill was a
		// real holding, so suppressing its re-alert stays correct.
		if b.snipeWatcher != nil {
			b.snipeWatcher.MarkBought(row.TokenID)
		}
		// (a) Rebuild the bought record and seed the spend ledger — 'main' rows
		// only. Historical 'deep' rows (the retired Deep Crash tier, issue #105)
		// never fed the record and now seed no pool — the deep pool is gone. They
		// are tolerated without error; no new deep rows are ever written.
		if row.Pool == database.SnipeBuyPoolDeep {
			ignoredDeep++
			restored++
			continue
		}
		b.snipeBought.restore(row.ChatID, row.TokenID)
		// (c) Seed the main spend ledger — current UTC day only, so a pre-midnight
		// row (still in the 24h window above) does not resurrect spend into a
		// freshly rolled-over day.
		if b.snipeSpend != nil && row.BoughtAt.UTC().Format("2006-01-02") == today {
			b.snipeSpend.seed(row.ChatID, row.AmountUSD)
			seededMain++
		}
		restored++
	}
	log.Printf("Snipe buys: restored %d row(s) in %s (bought record + watcher latch); seeded %d main spend row(s) for %s (ignored %d historical deep row(s))",
		restored, snipeBuyRestoreWindow, seededMain, today, ignoredDeep)
	return restored, nil
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
	// snipeAutoBuy owns the boxed latch (it holds the case-3 decision): it clears
	// the latch for the episode and re-arms it for case-3 recipients (issue #78).
	res, capLeft, status := b.snipeAutoBuy(chatID, market)
	switch status {
	case snipeAutoBought:
		if res.boxedTranche == 1 {
			// Immediate case-3 rung 1: report the $5 tranche, not the flat $10, and
			// the deep fill price. The second $5 rung is still armed for ≤ $0.05.
			price := res.ask
			if price <= 0 {
				price = ask
			}
			return snipeBoxedBoughtText(market.Question, market.Outcome, price, snipeBoxedTrancheUSD, 1, res.orderID, capLeft) + snipeFillNote(res),
				snipeAutoBoughtKeyboard(alertID)
		}
		return snipeAutoBoughtText(market.Question, market.Outcome, sessionHigh, ask, snipeAutoBuyUSD, res.orderID, capLeft) + snipeFillNote(res),
			snipeAutoBoughtKeyboard(alertID)
	case snipeAutoCapReached:
		return snipeAlertText(market.Question, market.Outcome, sessionHigh, ask) + snipeCapNote,
			snipeKeyboard(alertID)
	default:
		// Case 3 boxed-wait: distinct note explaining the postponement, not the
		// generic skip note.
		if res.outcome == snipeBuyBoxedWait {
			return snipeAlertText(market.Question, market.Outcome, sessionHigh, ask) + snipeBoxedWaitNote,
				snipeKeyboard(alertID)
		}
		return snipeAlertText(market.Question, market.Outcome, sessionHigh, ask) + snipeSkipNote(res),
			snipeKeyboard(alertID)
	}
}

// NotifySnipeBoxed implements one rung of the boxed ladder (issue #78): the
// watcher re-offers the alerted token in a boxed flip zone (tranche 1 at
// ≤ $0.10, tranche 2 at ≤ $0.05). Only a recipient latched case-3 at this
// episode's in-band alert acts — the latch, not a fire-time sibling re-check, is
// the decision, so a mid-episode ceiling harvest of the held winner cannot
// cancel the flip buy (ledger r72). Everyone else had their chance at the
// in-band alert and gets nothing here (no message). On a fill it runs the
// identical post-fill ceremony as the in-band buy (mark, TP-only auto-arm) via
// snipeAutoBuyExec at the $5 tranche stake, and DMs a boxed confirmation.
func (b *Bot) NotifySnipeBoxed(chatID int64, market live.SnipeMarket, sessionHigh, ask float64, tranche int) {
	// Eligibility is the alert-time latch alone, claimed per rung: a recipient not
	// case-3 when the episode alerted never boxed-buys, a latched one buys their
	// still-live rungs even if the held side was sold since, and claiming consumes
	// the rung so a duplicate fire can't double-buy. A stale latch from an episode
	// the recipient never saw a fresh alert for is rejected here too (F4).
	if b.snipeBoxedLatch == nil || !b.snipeBoxedLatch.claim(chatID, market.TokenID, tranche) {
		return
	}
	// Sport gate (esports-only), mirroring the other tiers.
	if !snipeIsEsports(market.Question) {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	user, err := b.userRepo.GetByTelegramID(ctx, chatID)
	if err != nil || user == nil {
		return
	}
	res, capLeft, status := b.snipeAutoBuyExec(ctx, chatID, user, market, snipeBoxedTrancheUSD)
	if status != snipeAutoBought {
		log.Printf("Snipe boxed-buy: skipped chat=%d token=%.12s… tranche=%d reason=%d", chatID, market.TokenID, tranche, res.outcome)
		return
	}
	alertID := b.snipeAlerts.add(market)
	b.sendMessageWithKeyboard(chatID,
		snipeBoxedBoughtText(market.Question, market.Outcome, ask, snipeBoxedTrancheUSD, tranche, res.orderID, capLeft)+snipeFillNote(res),
		snipeAutoBoughtKeyboard(alertID))
}

// snipeBoxedWaitNote is appended to the in-band alert for a case-3 recipient
// whose ask is still above the boxed threshold: the auto-buy is postponed into
// the ladder, but the manual tap buttons remain live.
const snipeBoxedWaitNote = "\n\n📦 You already hold the other side. The auto-buy is *waiting to buy the flip deep — $5 at ≤ $0.10 + $5 at ≤ $0.05* — your held side rides to the ceiling. Tap below to buy now anyway."

// snipeBoxedBoughtText is the boxed ladder confirmation for one tranche. Rung 1
// notes the second $5 still armed for ≤ $0.05 so the user reads the $5 (not $10)
// as deliberate. Pure — table-tested.
func snipeBoxedBoughtText(question, outcome string, ask, amount float64, tranche int, orderID string, capLeft float64) string {
	body := fmt.Sprintf(
		"📦 *Boxed flip tranche %d — auto-sniped $%.0f*\n\n"+
			"*%s*\n"+
			"*Side:* Buy %s\n"+
			"You hold the other side; grabbed this flip ticket deep at $%.3f.\n\n"+
			"*Order ID:* %s\n"+
			"Auto-snipe cap left today: $%.0f",
		tranche, amount, truncateUTF8(question, 60), outcome, ask, orderID, capLeft)
	if tranche == 1 {
		body += fmt.Sprintf("\n\n_A second $%.0f rung is armed for ≤ $%.2f._", snipeBoxedTrancheUSD, live.SnipeBoxedDeepAsk)
	}
	return body
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
			"⚠️ Corpse territory: games this cheap are usually over. Any entry "+
			"here is your read of the game state.",
		truncateUTF8(question, 60), outcome, alertAsk, ask, multiple)
}

// snipeDeepAlertOnlyNote is the honest one-liner every Deep Crash DM carries:
// the tier is alert-only — no $5 auto-buy at this depth (retired by the
// September review, issue #105; the pool was 0-for-13, −$64.73). The message
// must not imply a buy happened or will (v0.18.1 message-truth rule); the ⚡ tap
// buttons stay live for a judgment buy on a real comeback read.
const snipeDeepAlertOnlyNote = "\n\nDeep crashes are alert-only — no auto-buy at this depth. Tap below if you read a comeback."

// NotifySnipeDeepCrash implements the Deep Crash tier of live.SnipeNotifier
// (ADR 0007, retired to alert-only by the September review — issue #105): the
// $5 deep auto-buy is gone, so this always registers the alert for one-tap
// buying and DMs the alert-only Deep Crash notice — the crash summary, the
// corpse-territory warning, an honest no-auto-buy line, and the ⚡ Snipe $10/$25
// tap buttons for a judgment buy. Delivery is unconditional. sessionHigh and
// sinceAlert are unused now (no auto-buy decision consumes them) but stay in the
// signature the watcher calls.
func (b *Bot) NotifySnipeDeepCrash(chatID int64, market live.SnipeMarket, sessionHigh, ask, alertAsk float64, sinceAlert time.Duration) {
	alertID := b.snipeAlerts.add(market)
	text := snipeDeepText(market.Question, market.Outcome, alertAsk, ask) + snipeDeepAlertOnlyNote
	b.sendMessageWithKeyboard(chatID, text, snipeKeyboard(alertID))
}

// snipeHoldsSibling reports whether chatID already holds ANY OTHER token of the
// same market as the alerted token ("case 3": e.g. holds the favorite at ~0.80
// while the underdog crashed to 0.20). Sibling token IDs come from the watcher's
// in-memory index — no Gamma round-trip in the alert path. Checked cheapest
// first, first hit wins: (a) the in-memory bought record, (b) a live SL/TP arm,
// (c) the positions API. A positions read failure is treated as NOT case-3 so
// the buy proceeds normally — conservative toward existing behavior.
func (b *Bot) snipeHoldsSibling(ctx context.Context, user *database.User, chatID int64, market live.SnipeMarket) bool {
	if b.snipeWatcher == nil {
		return false
	}
	siblings := b.snipeWatcher.SiblingTokenIDs(market.MarketID, market.TokenID)
	if len(siblings) == 0 {
		return false
	}
	if b.snipeBought != nil { // (a) lag-free local record
		for _, sib := range siblings {
			if b.snipeBought.held(chatID, sib) {
				return true
			}
		}
	}
	if b.sltpArmRepo != nil { // (b) a live SL/TP arm on the other side
		for _, sib := range siblings {
			if arm, err := b.sltpArmRepo.GetByUserAndToken(ctx, chatID, sib); err == nil && arm != nil {
				return true
			}
		}
	}
	return b.snipeHoldsSiblingPosition(ctx, user, siblings) // (c) positions API
}

// snipeHoldsSiblingPosition reports whether the user's proxy holds shares of any
// sibling token. Matches by TOKEN ID (a Data API position's MarketID is often
// the 0x condition ID, not the numeric Gamma ID the alert carries, so token ID
// is the reliable key). A read failure is not-case-3 (conservative).
func (b *Bot) snipeHoldsSiblingPosition(ctx context.Context, user *database.User, siblings []string) bool {
	if user.ProxyAddress == "" {
		return false
	}
	scanner := b.snipePositions
	if scanner == nil {
		scanner = polymarket.NewUnifiedPositionScanner()
	}
	positions, err := scanner.GetPositions(ctx, common.HexToAddress(user.ProxyAddress))
	if err != nil {
		log.Printf("Snipe boxed: sibling positions read failed: %v", err)
		return false
	}
	sibSet := make(map[string]bool, len(siblings))
	for _, s := range siblings {
		sibSet[s] = true
	}
	for _, pos := range positions {
		if sibSet[pos.TokenID] && pos.Shares != nil && pos.Shares.Sign() > 0 {
			return true
		}
	}
	return false
}

// Fill confirmation (issue #92): an executor Success on a GTC snipe buy only
// means the CLOB accepted the order — on a fast-moving book it can REST
// unfilled while the quoted level vanishes. Arming (and keeping the stake)
// off an unfilled order produced the VISION-series chain: instant TP against
// zero balance, a false "closed outside the bot" disarm, and an orphaned
// resting bid that later filled unarmed. The confirm path polls the order
// until it matches, cancels it when the window expires, and refunds the
// unfilled slice of the stake.
const (
	snipeFillPollInterval  = 3 * time.Second
	snipeFillConfirmWindow = 60 * time.Second
	// snipeFillGoneGrace mirrors the FOK path's fokGoneGraceWindow (issue #27):
	// an accepted in-play order is NOT queryable on GET /data/order during the
	// bet delay — production delayed orders polled "gone" ~1s after submission
	// and then FILLED seconds later. A found=false reading inside this window
	// is inconclusive, never terminal.
	snipeFillGoneGrace = 15 * time.Second
)

// snipeFillPendingNote is appended to a bought confirmation whose fill the
// executor did not confirm (the order may be resting) — the DM must not claim
// a fill that hasn't happened (the v0.18.1 message-truth rule).
const snipeFillPendingNote = "\n\n⏳ Fill not confirmed yet — protection arms when it lands, or you'll get a refund note if it doesn't."

// snipeFillNote returns the pending-fill disclaimer for an unconfirmed buy,
// empty for an executor-confirmed fill. Pure.
func snipeFillNote(res snipeBuyResult) string {
	if res.filledSize > 0 {
		return ""
	}
	return snipeFillPendingNote
}

// snipeUnfilledText is the note for a snipe order that never filled and was
// cancelled. The refund line only appears when a cap was actually drawn — the
// one-tap path draws none (issue #92 review F5). Pure.
func snipeUnfilledText(question, outcome string, stake float64, capRefunded bool) string {
	tail := "cancelled — nothing was bought."
	if capRefunded {
		tail = fmt.Sprintf("cancelled, $%.2f returned to today's cap.", stake)
	}
	return fmt.Sprintf("⏳ *Snipe order didn't fill*\n\n%s\nSide: Buy %s\n\nThe book moved before the order could match — %s",
		question, outcome, tail)
}

// snipeConfirmFillThenArm gates the TP-only auto-arm behind an actual fill
// (issue #92). An executor-confirmed fill arms immediately (the pre-#92
// path). Otherwise it polls the order until matched; an order still open at
// the window edge is cancelled (a cancel that races a fill still arms the
// matched shares) and the unfilled slice of the stake is released via the
// caller's ledger closure. When the order status can never be read, it fails
// closed both ways: no arm, no refund, one loud warning.
func (b *Bot) snipeConfirmFillThenArm(chatID int64, user *database.User, tokenID, question, outcome string, res snipeBuyResult, stake float64, release func(float64)) {
	if res.filledSize > 0 {
		b.snipeAutoArmTPOnly(chatID, tokenID, question, outcome, res, stake)
		return
	}
	poll, window := b.snipeFillPoll, b.snipeFillWindow
	if poll <= 0 {
		poll = snipeFillPollInterval
	}
	if window <= 0 {
		window = snipeFillConfirmWindow
	}
	check, kill := b.snipeOrderFill, b.snipeOrderKill
	if check == nil {
		check = b.snipeOrderFillLive
	}
	if kill == nil {
		kill = b.snipeOrderKillLive
	}
	goneGrace := b.snipeFillGoneGrace
	if goneGrace <= 0 {
		goneGrace = snipeFillGoneGrace
	}
	ctx, cancel := context.WithTimeout(context.Background(), window+30*time.Second)
	defer cancel()

	// matched/price only ever update from found=true readings — a bet-delay
	// 404 blip must not wipe an observed partial fill (review F3). terminal
	// means the order's final state is CONFIRMED: only a confirmed-terminal
	// order may trigger a refund (review F2).
	start := time.Now()
	deadline := start.Add(window)
	var matched, price float64
	var terminal bool
	for {
		m, p, o, found, err := check(ctx, user, res.orderID)
		switch {
		case err != nil:
			log.Printf("Snipe fill-confirm: status read chat=%d order=%s: %v", chatID, res.orderID, err)
		case found:
			matched = m
			if p > 0 {
				price = p
			}
			if !o {
				terminal = true // fully matched or killed
			}
		case time.Since(start) > goneGrace:
			terminal = true // not queryable well past the bet delay: reaped
		default:
			// found=false right after submit is the bet-delay blip (issue #27) —
			// inconclusive, keep polling.
		}
		if terminal || !time.Now().Before(deadline) {
			break
		}
		time.Sleep(poll)
	}

	if !terminal {
		// Window expired with the order possibly still on the book: cancel so
		// nothing can fill unarmed later, then confirm the final state — a
		// cancel that raced a fill reports the matched shares here. A cancel
		// AND final read that both fail leave the stake reserved and the user
		// warned instead of a false "cancelled" note (review F2).
		killErr := kill(ctx, user, res.orderID)
		if killErr != nil {
			log.Printf("Snipe fill-confirm: cancel chat=%d order=%s: %v", chatID, res.orderID, killErr)
		}
		for i := 0; i < 3 && !terminal; i++ {
			m, p, o, found, err := check(ctx, user, res.orderID)
			if err != nil {
				log.Printf("Snipe fill-confirm: post-cancel read chat=%d order=%s: %v", chatID, res.orderID, err)
				time.Sleep(poll)
				continue
			}
			if found {
				matched = m
				if p > 0 {
					price = p
				}
				if !o {
					terminal = true
				}
			} else if killErr == nil || time.Since(start) > goneGrace {
				terminal = true // reaped (or cancelled and reaped)
			}
			break
		}
	}

	fillPrice := price
	if fillPrice <= 0 {
		fillPrice = res.ask
	}
	switch {
	case matched > 0:
		res.filledSize = matched
		if price > 0 {
			res.filledPrice = price
		}
		if terminal {
			if refund := stake - matched*fillPrice; refund > 0.01 && release != nil {
				log.Printf("Snipe fill-confirm: partial fill chat=%d order=%s matched=%.2f — releasing $%.2f", chatID, res.orderID, matched, refund)
				release(refund)
			}
		} else {
			log.Printf("Snipe fill-confirm: arming %.2f matched shares with UNCONFIRMED remainder chat=%d order=%s — stake kept reserved", matched, chatID, res.orderID)
		}
		b.snipeAutoArmTPOnly(chatID, tokenID, question, outcome, res, stake)
	case terminal:
		log.Printf("Snipe fill-confirm: unfilled chat=%d order=%s — cancelled, $%.2f released", chatID, res.orderID, stake)
		if release != nil {
			release(stake)
		}
		b.sendMessage(chatID, snipeUnfilledText(question, outcome, stake, release != nil))
	default:
		log.Printf("Snipe fill-confirm: UNRESOLVED order state chat=%d order=%s — no arm, no refund, stake kept reserved", chatID, res.orderID)
		warn := "⚠️ Couldn't settle your snipe order's state (status/cancel checks failing) — check the order and position manually and arm by hand if it filled."
		if release != nil {
			warn += " The stake stays counted against today's cap."
		}
		b.sendMessage(chatID, warn)
	}
}

// snipeOrderFillLive is the production fill probe: an L2-authed CLOB order
// read with the user's derived credentials.
func (b *Bot) snipeOrderFillLive(ctx context.Context, user *database.User, orderID string) (float64, float64, bool, bool, error) {
	creds, addr, err := b.snipeOrderCreds(ctx, user)
	if err != nil {
		return 0, 0, false, false, err
	}
	return b.tradingClient.OrderFill(ctx, addr, creds, orderID)
}

// snipeOrderKillLive is the production cancel for an unfilled snipe order.
func (b *Bot) snipeOrderKillLive(ctx context.Context, user *database.User, orderID string) error {
	creds, addr, err := b.snipeOrderCreds(ctx, user)
	if err != nil {
		return err
	}
	return b.tradingClient.CancelOrder(ctx, addr, creds, orderID)
}

// snipeOrderCreds derives the user's CLOB API credentials for the fill
// probe / cancel, mirroring the manual cancel-all flow.
func (b *Bot) snipeOrderCreds(ctx context.Context, user *database.User) (*polymarket.APICredentials, common.Address, error) {
	if b.walletManager == nil || b.tradingClient == nil || user == nil {
		return nil, common.Address{}, fmt.Errorf("order probe unavailable: trading wiring missing")
	}
	uw, err := b.walletManager.DecryptPrivateKey(user.EncryptedKey)
	if err != nil {
		return nil, common.Address{}, fmt.Errorf("decrypt wallet: %w", err)
	}
	creds, err := b.tradingClient.GetOrCreateAPICredentials(ctx, uw.PrivateKey)
	if err != nil {
		return nil, common.Address{}, fmt.Errorf("api credentials: %w", err)
	}
	return creds, common.HexToAddress(user.EOAAddress), nil
}

// snipeAutoArmTPOnly arms TP + ceiling only (no trailing SL) on a freshly
// snipe-bought token. Rationale (user-ratified): a trailing SL on a snipe
// tranche amputates the 5× tail the band's economics need (wick-amputations in
// gapped in-play books), while TP + the $0.95 ceiling harvested every big
// winner. One arm per user per token: an existing arm is never clobbered — a
// later MANUAL arm re-arms over it with the full TP+SL.
//
// Run with `go`: the tick-size fetch and DB write must never block alert
// delivery or the buy flow (design constraint), and every failure here is
// log-only. Arm data comes from the FILL, never a Data API positions read
// (issue #67 lag): price is the confirmed fill VWAP, or the guard's fresh ask
// for a delayed in-play order; shares are the filled size, or stake/price when
// the fill size is still unconfirmed.
func (b *Bot) snipeAutoArmTPOnly(chatID int64, tokenID, question, outcome string, res snipeBuyResult, stake float64) {
	if b.sltpArmRepo == nil || res.market == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	existing, err := b.sltpArmRepo.GetByUserAndToken(ctx, chatID, tokenID)
	if err != nil {
		// Fail closed (issue #87): a read failure can't distinguish "no arm" from
		// "arm exists", so never auto-arm on an unverifiable token — that risks
		// clobbering an existing arm. Warn the recipient to arm manually. No retry.
		log.Printf("Snipe auto-arm: existing-arm read FAILED chat=%d token=%.12s…: %v — failing closed, no auto-arm", chatID, tokenID, err)
		b.sendMessage(chatID, "⚠️ Couldn't verify this token's existing protection — auto-arm skipped. Tap 🎯 SL/TP to arm manually.")
		return
	}
	if existing != nil {
		log.Printf("Snipe auto-arm: arm already exists chat=%d token=%.12s… — skipping", chatID, tokenID)
		return
	}

	price := res.filledPrice
	if price <= 0 {
		price = res.ask // delayed in-play fill: the guard ask is our best price estimate
	}
	if price <= 0 {
		log.Printf("Snipe auto-arm: no fill price chat=%d token=%.12s… — skipping", chatID, tokenID)
		return
	}
	shares := res.filledSize
	if shares <= 0 {
		shares = stake / price // derive from stake when the executor didn't confirm a size
	}

	marketID := res.market.ID
	arm := &database.SLTPArm{
		TelegramID:  chatID,
		TokenID:     tokenID,
		ConditionID: res.market.ConditionID,
		MarketID:    &marketID,
		Outcome:     normalizeOutcome(outcome),
		AvgPrice:    price,
		SharesAtArm: shares,
		TickSize:    b.armTickSize(ctx, tokenID),
		NegRisk:     res.market.NegRisk,
		TPArmed:     true,
		SLArmed:     false,
	}
	saved, err := b.sltpArmRepo.ArmTPOnly(ctx, arm)
	if err != nil {
		log.Printf("Snipe auto-arm: %d/%.12s…: %v", chatID, tokenID, err)
		return
	}

	// Mirror the manual arm flow: inform the SL/TP monitor and add the snipe
	// watcher's armed source — but reuse the market already in hand rather than
	// re-fetching Gamma (the manual path's registerSnipeArmed does), keeping it
	// lag-free (issue #67).
	if b.sltpMonitor != nil {
		b.sltpMonitor.SubscribeFor(saved.TokenID)
	}
	if b.snipeWatcher != nil {
		b.snipeWatcher.WatchArmed(snipeMarketFromGamma(res.market, tokenID, outcome))
		// Series walk (issue #94): the snipe auto-buy flow was the recipients=0
		// incident path — the buyer must become a held watcher of the event's
		// other winner markets, surviving this arm's eventual sweep. Already in
		// the async arm goroutine, so the Gamma fetch blocks no handler.
		b.snipeWatchEventMates(chatID, res.market, live.SnipeHeldTTL)
	}
	b.sendMessage(chatID, snipeAutoArmedText(question, outcome, saved))
}

// snipeAutoArmedText is the TP-only auto-arm confirmation — no trailing-stop
// line, and it states plainly that the stake is the max loss (there is no
// stop). Pure — table-tested.
func snipeAutoArmedText(title, outcome string, arm *database.SLTPArm) string {
	// Deep entries (≤ $0.05) fire the multi-rung exit ladder (issue #81); list
	// every rung so the confirmation is ladder-truthful. Otherwise the same
	// honesty rule as sltpTPLine (issue #74): a trigger at/above the ceiling never
	// fires, so promise the ceiling's sell-everything instead.
	tpLine := sltpCeilingTPLine()
	switch {
	case arm.IsDeepEntry():
		tpLine = sltpTPLadderLine(arm)
	case arm.TPTriggerPrice() < database.CeilingTPPrice:
		tpLine = fmt.Sprintf("• TP: bid ≥ $%.4f → sell %.0f%%, then ride to the $%.2f ceiling",
			arm.TPTriggerPrice(), database.TPSellFraction*100, database.CeilingTPPrice)
	}
	return fmt.Sprintf(
		"🎯 *Auto-armed (TP only)* %s %s\n\n"+
			"• Entry: $%.4f\n"+
			"%s\n"+
			"• No stop-loss — max loss is your ~$%.2f stake.\n\n"+
			"Snipe tranches keep their tail; tap 🎯 SL/TP to manage it manually.",
		title, outcome,
		arm.AvgPrice,
		tpLine,
		arm.SharesAtArm*arm.AvgPrice,
	)
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
	// Reset this recipient's boxed latch for the new episode — every in-band
	// alert overwrites it (the overwrite IS the episode boundary, issue #78). The
	// case-3 branches below re-arm it; a non-case-3 alert leaves it cleared, so a
	// stale latch from a prior episode can never fire against this token.
	if b.snipeBoxedLatch != nil {
		b.snipeBoxedLatch.clear(chatID, market.TokenID)
	}

	// Gate 0 (series-walked gate): a market this recipient watches ONLY via the
	// series walk (issue #102) is alert-only — no auto money on a continuation
	// they never personally traded, armed, or subscribed. Checked BEFORE the sport
	// gate so September attributes these skips to recipiency class, and before any
	// wallet lookup or cap reservation. The tap buttons stay live.
	if b.snipeWatcher != nil && b.snipeWatcher.WalkedOnlyHolder(chatID, market.TokenID) {
		log.Printf("Snipe auto-buy: series-walked chat=%d token=%.12s… q=%q", chatID, market.TokenID, market.Question)
		return snipeBuyResult{outcome: snipeBuySeriesWalked}, 0, snipeAutoSkipped
	}

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

	// Case 3 (boxed ladder): the recipient already holds the OTHER side of this
	// market. With TP-only auto-arms the held side harvests at the $0.95 ceiling,
	// so the flip ticket is better bought deep — as the two-rung ladder ($5 at
	// ≤ $0.10 + $5 at ≤ $0.05) rather than the in-band $10 (issue #78). The
	// per-tranche latch, not a fire-time sibling re-check, drives the watcher's
	// later fires, so a mid-episode ceiling harvest cannot cancel the flip.
	if b.snipeHoldsSibling(ctx, user, chatID, market) {
		var ask float64
		var ok bool
		if b.snipeFeed != nil {
			ask, ok = b.snipeFeed.BestAsk(market.TokenID)
		}
		if !ok || ask > live.SnipeBoxedMaxAsk {
			// Postponed: the ask is not yet in the boxed zone. Latch BOTH rungs;
			// the watcher buys them at ≤ 0.10 and ≤ 0.05.
			if b.snipeBoxedLatch != nil {
				b.snipeBoxedLatch.arm(chatID, market.TokenID, true, true)
			}
			log.Printf("Snipe auto-buy: boxed-wait chat=%d token=%.12s… ask=%.3f ok=%v", chatID, market.TokenID, ask, ok)
			return snipeBuyResult{outcome: snipeBuyBoxedWait, ask: ask, askOK: ok}, 0, snipeAutoSkipped
		}
		// Immediate: the ask is already ≤ 0.10. Buy tranche 1 ($5) now and leave
		// tranche 2 live for the watcher's ≤ 0.05 fire — the ladder joins ALL
		// case-3 flips, never a flat $10 that then stacks $5+$5 (issue #78 F3).
		res, capLeft, status := b.snipeAutoBuyExec(ctx, chatID, user, market, snipeBoxedTrancheUSD)
		if status == snipeAutoBought {
			if b.snipeBoxedLatch != nil {
				b.snipeBoxedLatch.arm(chatID, market.TokenID, false, true) // rung 1 taken, rung 2 live
			}
			res.boxedTranche = 1
			return res, capLeft, status
		}
		// The immediate $5 failed (repriced/rejected/corpse/cap): latch BOTH rungs
		// so the watcher's boxed re-offers retry — restoring main's
		// retry-at-re-offer behavior instead of dropping the recipient (F3).
		if b.snipeBoxedLatch != nil {
			b.snipeBoxedLatch.arm(chatID, market.TokenID, true, true)
		}
		return res, capLeft, status
	}

	// Manual-arm gate (issue #86): skip the normal in-band $10 auto-buy when this
	// recipient already carries an ACTIVE manual stop (sl_armed = TRUE) on the
	// CRASHED/alerted token. Rationale is coherence, not just orphan-avoidance: a
	// trailing stop armed above the snipe band means the buy is either stop-sold
	// moments later or rides unprotected — the snipe thesis can't play out under a
	// live manual stop (DK G2 exhibit: the machine bought 0.26 while its own stop
	// stood at 0.464 and fired minutes later).
	//
	// Precedence is load-bearing and deliberate:
	//   - It runs AFTER case-3 classification. A recipient holding the OTHER side
	//     already latched boxed-wait / bought a tranche and returned above, so
	//     case-3 WINS — this gate reads the arm on the ALERTED token only, never a
	//     sibling, and a holder of both sides never reaches here.
	//   - It runs BEFORE snipeAutoBuyExec's cap reserve, so a gated alert never
	//     touches the daily cap (no reserve, no refund, no MarkBought).
	//
	// Scope (binding, from the ratified spec):
	//   - TP-only auto-arms (sl_armed = FALSE) never gate — they already carry
	//     fire-time whole-position TP coverage, so a top-up stays orphan-safe.
	//   - Disarmed/swept arms never gate: every disarm path (user disarm, SL-fire
	//     completion, sweep) DELETEs the row, so GetByUserAndToken returns nil;
	//     the sl_armed=FALSE rows that do exist (TP-only arms, incl. post-ClearTP)
	//     fall through to the buy on the flag check.
	//   - The gate is a GUARD, not a dependency: a DB-read failure fails OPEN (the
	//     buy proceeds exactly as today) and logs loudly. A nil repo (test bots,
	//     legacy wiring) is likewise a no-op.
	if b.sltpArmRepo != nil {
		arm, err := b.sltpArmRepo.GetByUserAndToken(ctx, chatID, market.TokenID)
		switch {
		case err != nil:
			log.Printf("Snipe auto-buy: manual-arm gate read FAILED chat=%d token=%.12s…: %v — failing open, buy proceeds",
				chatID, market.TokenID, err)
		case arm != nil && arm.SLArmed:
			log.Printf("Snipe auto-buy: manual-armed chat=%d token=%.12s…", chatID, market.TokenID)
			return snipeBuyResult{outcome: snipeBuyManualArmed}, 0, snipeAutoSkipped
		}
	}

	return b.snipeAutoBuyExec(ctx, chatID, user, market, snipeAutoBuyUSD)
}

// snipeAutoBuyExec is the shared main-pool buy ceremony — cap reserve/refund,
// the guarded buy with the corpse-spread gate, bought-record bookkeeping, and
// the TP-only auto-arm. Reused by the in-band alert path (snipeAutoBuy, $10) and
// each boxed ladder tranche (NotifySnipeBoxed, $5) so the reserve/refund cap
// logic lives in one place; amount is the per-call stake. Callers own the sport
// gate, wallet lookup, and case-3 decision.
func (b *Bot) snipeAutoBuyExec(ctx context.Context, chatID int64, user *database.User, market live.SnipeMarket, amount float64) (snipeBuyResult, float64, snipeAutoStatus) {
	capLeft, ok := b.snipeSpend.reserve(chatID, amount)
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
	// corpseGuard=true adds Gate 2; futureGate=true adds the issue #97 gate.
	res := b.snipeGuardedBuyRefuse(ctx, user, entry, amount, snipeRefuseBuy, true, true)
	if res.outcome != snipeBuyFilled {
		b.snipeSpend.release(chatID, amount)
		log.Printf("Snipe auto-buy: skipped chat=%d token=%.12s… reason=%d err=%v msg=%s",
			chatID, market.TokenID, res.outcome, res.err, res.errorMsg)
		return res, 0, snipeAutoSkipped
	}
	// Record the holding — the boxed case-3 sibling gate and restart restore
	// read this record (the deep holdings gate it once fed retired with the
	// deep auto-buy, #105). mark also writes the durable buy row (pool 'main',
	// this stake) when a store is wired (#84).
	if b.snipeBought != nil {
		b.snipeBought.mark(chatID, market.TokenID, amount)
	}
	// Confirm the fill, then arm TP + ceiling (no trailing SL) — async, never
	// blocks alert delivery. An unfilled order is cancelled and the stake
	// released back to the main cap (issue #92).
	go b.snipeConfirmFillThenArm(chatID, user, market.TokenID, market.Question, market.Outcome, res, amount,
		func(a float64) { b.snipeSpend.release(chatID, a) })
	log.Printf("Snipe auto-buy: accepted chat=%d token=%.12s… $%.0f order=%s cap-left=$%.2f",
		chatID, market.TokenID, amount, res.orderID, capLeft)
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
	snipeBuyBoxedWait                    // boxed tier: recipient holds the other side — postpone until ask ≤ $0.10
	snipeBuyManualArmed                  // manual-arm gate (issue #86): recipient has an ACTIVE sl_armed stop on the crashed token
	snipeBuyFutureGame                   // future-game gate (issue #97): an earlier game of the event is still live — this game hasn't started
	snipeBuySeriesWalked                 // series-walked gate (issue #102): market entered the watch ONLY via the series walk — alert-only
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
	// Fill context for the TP-only auto-arm (snipeBuyFilled only). market
	// supplies ConditionID / NegRisk / outcomes / gameStart without a refetch;
	// idx is the bought token's outcome index. filledSize/filledPrice come from
	// the executor but are 0 for a delayed in-play order — the auto-arm then
	// derives shares from stake and price from the guard ask (issue #67: never
	// block on a Data API positions read).
	market      *polymarket.GammaMarket
	idx         int
	filledSize  float64
	filledPrice float64
	// boxedTranche marks an immediate case-3 buy as ladder rung 1 (issue #78 F3),
	// so snipeAlertMessage renders the $5 boxed confirmation instead of the flat
	// $10 auto-sniped copy. 0 for every other buy.
	boxedTranche int
}

// snipeGuardedBuy is the shared guarded snipe buy behind the one-tap callback
// (manual taps are never gated): repricing guard on a fresh ask, market fetch,
// token-index verify, buy, MarkBought on success. Claiming the registry entry
// and cap accounting stay with the callers.
func (b *Bot) snipeGuardedBuy(ctx context.Context, user *database.User, entry snipeAlertEntry, amount float64) snipeBuyResult {
	// Manual taps: no corpse gate, no future-game gate — judgment buys.
	return b.snipeGuardedBuyRefuse(ctx, user, entry, amount, snipeRefuseBuy, false, false)
}

// snipeGuardedBuyRefuse is snipeGuardedBuy with a caller-chosen repricing guard
// and an optional corpse-spread gate (Gate 2, in-band $10 only). When
// corpseGuard is set, the same fresh-book read that feeds the repricing guard
// also reads the best bid and skips the buy on corpse geometry.
func (b *Bot) snipeGuardedBuyRefuse(ctx context.Context, user *database.User, entry snipeAlertEntry, amount float64, refuse func(ask float64, ok bool) bool, corpseGuard, futureGate bool) snipeBuyResult {
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
	// Future-game gate (issue #97, auto tiers only): per-game markets carry the
	// SERIES gameStartTime, so a not-yet-played game passes the in-play gate the
	// moment the series starts. When an EARLIER game of the same event is
	// demonstrably still live, this crash is series-sweep repricing, not an
	// in-play collapse — the band's economics don't cover "game happens AND
	// team wins it". Positive evidence only: fail-open on any doubt (a genuine
	// live-game crash — the r105 winner class — must never be false-gated).
	if futureGate && b.snipeFutureGame(ctx, mc, market) {
		log.Printf("Snipe auto-buy: future-game-gated token=%.12s… q=%q", entry.tokenID, market.Question)
		return snipeBuyResult{outcome: snipeBuyFutureGame, ask: ask, askOK: ok}
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
	return snipeBuyResult{
		outcome: snipeBuyFilled, ask: ask, orderID: result.OrderID,
		market: market, idx: idx, filledSize: result.FilledSize, filledPrice: result.AveragePrice,
	}
}

// snipeFutureGame reports whether market's game has demonstrably NOT started:
// it is a game/map-winner with ordinal N ≥ 2 and an earlier game-winner of the
// same event is still LIVE — open and with both outcome prices strictly inside
// the decided bands. Everything else — series moneylines, game 1, decided or
// closed earlier games (the between-games gap), fetch/parse trouble — is
// fail-open: only positive evidence withholds a buy (issue #97).
func (b *Bot) snipeFutureGame(ctx context.Context, mc *polymarket.MarketClient, market *polymarket.GammaMarket) bool {
	n := live.GameNumber(market.Question)
	if n <= 1 {
		return false
	}
	slug := ""
	if len(market.Events) > 0 {
		slug = market.Events[0].Slug
	}
	if slug == "" {
		return false
	}
	// Own tight timeout: a HUNG (not erroring) Gamma must not hold the buy
	// path for the caller's full 60s ctx — cap it and fail open.
	gateCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	mates, err := mc.GetEventMarkets(gateCtx, slug)
	if err != nil {
		log.Printf("Snipe future-game gate: event fetch %s: %v — failing open", slug, err)
		return false
	}
	for _, mate := range mates {
		if mate == nil || mate.Closed {
			continue
		}
		m := live.GameNumber(mate.Question)
		if m >= 1 && m < n && snipeGameLive(mate.GetOutcomePrices()) {
			return true
		}
	}
	return false
}

// snipeGameLive reports whether outcome prices describe a game still being
// played: every parsed price strictly inside (0.03, 0.97). Decided games print
// ~0/1 (redeem pending) even before Gamma flips closed. Unparseable or missing
// prices are NOT live (fail-open for the gate). Pure — table-tested.
func snipeGameLive(prices []string) bool {
	if len(prices) == 0 {
		return false
	}
	for _, p := range prices {
		v, err := strconv.ParseFloat(p, 64)
		if err != nil {
			return false
		}
		if v <= 0.03 || v >= 0.97 {
			return false
		}
	}
	return true
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
		// Record the holding — boxed case-3 gating and restore read it, same as
		// the in-band auto-buy (#105 retired the deep gate it once fed). mark
		// also writes the durable buy row (pool 'main', this tap's stake) (#84).
		if b.snipeBought != nil {
			b.snipeBought.mark(chatID, entry.tokenID, amount)
		}
		// A manual tap supersedes the boxed ladder: clear the latch so the
		// watcher's tranche fires don't stack another $5+$5 on top of the manual
		// buy the user just made from the "buy now anyway" button (issue #78 F2).
		if b.snipeBoxedLatch != nil {
			b.snipeBoxedLatch.clear(chatID, entry.tokenID)
		}
		// Register the tapped market as a DIRECT held watch (issue #102): a tap is
		// the user personally trading this market, so it upgrades a series-walked
		// entry to full auto-buy semantics for later crashes — and, via the house
		// bought-token registration, brings the sibling watch every other buy path
		// already has. res carries the market + bought index from the fill, so no
		// refetch. In-memory and cheap; the event-mate walk inside runs detached.
		b.snipeRegisterBoughtToken(chatID, res.market, res.idx)
		// Confirm the fill, then arm TP + ceiling (no trailing SL) — async. The
		// tap draws no auto-cap, so there is no ledger to release (issue #92).
		go b.snipeConfirmFillThenArm(chatID, user, entry.tokenID, entry.question, entry.outcome, res, amount, nil)
		keyboard := tgbotapi.NewInlineKeyboardMarkup(
			tgbotapi.NewInlineKeyboardRow(
				tgbotapi.NewInlineKeyboardButtonData("🎯 Arm SL/TP", "sltp_list"),
			),
		)
		b.editMessageWithKeyboard(chatID, messageID,
			snipeFilledText(entry.question, entry.outcome, amount, res.orderID)+snipeFillNote(res), keyboard)
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
	// EventSlug takes only a GENUINE parent-event slug — GetEventSlug's
	// market-slug fallback would launder single-market positions into hourly
	// bogus event fetches via the renewal trigger (round-2 review F2).
	// Grouping loses nothing: a slugless token falls back to same-market
	// renewal, which is all a single-market position has.
	eventSlug := ""
	if len(market.Events) > 0 {
		eventSlug = market.Events[0].Slug
	}
	return live.SnipeMarket{
		TokenID:   tokenID,
		MarketID:  market.ID,
		Question:  market.Question,
		Outcome:   outcome,
		GameStart: market.GetGameStartTime(),
		EventSlug: eventSlug,
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

// snipeWatchHeldMarket registers EVERY token of market as a Held Watch for
// chatID — the held/bought token AND its siblings (issue #78). The flip side is
// where a comeback crash and the boxed case-3 buy actually land, and every
// auto-buy tier hangs off that token's own in-band alert, so both sides must be
// watched. Empty token IDs are skipped; the bought latch is never touched.
// Shared by the buy path (snipeRegisterBoughtToken) and the position-refresh
// path (registerSnipeHeld) so the sibling fan-out lives in one place.
func (b *Bot) snipeWatchHeldMarket(chatID int64, market *polymarket.GammaMarket, ttl time.Duration) {
	if b.snipeWatcher == nil || market == nil {
		return
	}
	// Path-form fetches omit events[] (issue #99); graft it before own-token
	// registration so EventSlug stamping and the walk below both see the slug.
	b.ensureMarketEvents(market)
	outcomes := market.GetOutcomes()
	for i, tokenID := range market.GetClobTokenIds() {
		if tokenID == "" {
			continue
		}
		outcome := ""
		if i < len(outcomes) {
			outcome = outcomes[i]
		}
		b.snipeWatcher.WatchHeld(chatID, snipeMarketFromGamma(market, tokenID, outcome), ttl)
	}
	b.snipeWatchEventMates(chatID, market, ttl)
}

// snipeEventMateMaxTokens bounds the series-watch fan-out per registration —
// generous for a BO5 (series + 5 games = 12 tokens) while capping a pathological
// event. Drops are logged, never silent.
const snipeEventMateMaxTokens = 16

// walkRelevant reports whether a path-form market could carry a series worth
// walking: it must have an ID to look up and a gameStartTime. Every
// walk-relevant market is in-play sports and carries gameStartTime on the path
// form (issue #99); a market with no game start can never pass the in-play gate,
// so it can't alert and needs no walk. Skipping it (no fetch, no bail) kills the
// enrichment amplification a /positions sweep would pay on non-sports positions.
// Same empty/zero semantics as the in-play gate (fetchSnipeMarket).
func walkRelevant(market *polymarket.GammaMarket) bool {
	return market != nil && market.ID != "" && !market.GetGameStartTime().IsZero()
}

// enrichEventsFor fetches a market's parent events via the list form
// GET /markets?id={id} (issue #99), logging the fail-open bail on an empty
// response or an error and returning nil in that case. It is PURE with respect
// to the market — the caller decides whether to graft the result (mutate) or use
// it locally. Callers apply walkRelevant before calling.
func (b *Bot) enrichEventsFor(marketID string) []*polymarket.GammaEvent {
	mc := b.snipeMarkets
	if mc == nil {
		mc = polymarket.NewMarketClient()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	events, err := mc.GetMarketEvents(ctx, marketID)
	if err != nil {
		log.Printf("Snipe series watch: events enrichment failed market=%s: %v", marketID, err)
		return nil
	}
	if len(events) == 0 {
		log.Printf("Snipe series watch: events enrichment empty market=%s — walk unavailable", marketID)
		return nil
	}
	return events
}

// ensureMarketEvents GRAFTS the parent event onto a market fetched via the path
// form GET /markets/{id}, which omits events[] (issue #99). Idempotent — a
// no-op once Events is present. MUTATING: it writes market.Events, so it is only
// safe from single-goroutine contexts where the write happens-before any spawn
// that shares the pointer. Only the registration seams (snipeRegisterBoughtToken,
// snipeWatchHeldMarket) use it — the graft is what stamps EventSlug for own-token
// registration. The walk seam (snipeWatchEventMates) must NOT: on the tap path
// two sibling goroutines share res.market, so it resolves the slug locally.
func (b *Bot) ensureMarketEvents(market *polymarket.GammaMarket) {
	if market == nil || len(market.Events) > 0 || !walkRelevant(market) {
		return
	}
	if events := b.enrichEventsFor(market.ID); len(events) > 0 {
		market.Events = events
	}
}

// resolveEventSlug returns the market's parent-event slug WITHOUT mutating the
// market. Events already present ⇒ Events[0].Slug (a registration seam grafted,
// or the market arrived enriched); otherwise, for a walk-relevant market, it
// fetches the list form and uses the returned slug locally. "" ⇒ no walk
// (single-market position, or fail-open on empty/error — enrichEventsFor logs
// the bail).
func (b *Bot) resolveEventSlug(market *polymarket.GammaMarket) string {
	if market == nil {
		return ""
	}
	if len(market.Events) > 0 {
		return market.Events[0].Slug
	}
	if !walkRelevant(market) {
		return ""
	}
	if events := b.enrichEventsFor(market.ID); len(events) > 0 {
		return events[0].Slug
	}
	return ""
}

// snipeWatchEventMates walks the held market's parent event. NON-MUTATING: the
// slug is resolved locally (never written back to market), so the tap path's two
// sibling goroutines — the walk (spawned from snipeRegisterBoughtToken) and the
// arm (snipeConfirmFillThenArm ~:1400) — cannot race a graft on the shared
// res.market (issue #99 hardening). The 1h snipeSeriesWalkDue limit dedups their
// concurrent walk attempts.
func (b *Bot) snipeWatchEventMates(chatID int64, market *polymarket.GammaMarket, ttl time.Duration) {
	slug := b.resolveEventSlug(market)
	if slug == "" {
		return // no parent event known: single-market position, nothing to walk
	}
	b.snipeWalkEventSlug(chatID, slug, market.ID, ttl)
}

// snipeSeriesWalkInterval bounds how often one event is re-walked per bot
// process: the walk is idempotent but each run costs a Gamma fetch, and the
// renewal paths call in on every /positions view.
const snipeSeriesWalkInterval = time.Hour

// snipeSeriesWalkDue reports whether eventSlug's walk interval has elapsed and
// stamps the attempt. Per-(chat,event): two holders of the same event each get
// their own registration.
func (b *Bot) snipeSeriesWalkDue(chatID int64, eventSlug string) bool {
	key := fmt.Sprintf("%d:%s", chatID, eventSlug)
	b.snipeSeriesWalkMu.Lock()
	defer b.snipeSeriesWalkMu.Unlock()
	if b.snipeSeriesWalked == nil {
		b.snipeSeriesWalked = make(map[string]time.Time)
	}
	if last, ok := b.snipeSeriesWalked[key]; ok && time.Since(last) < snipeSeriesWalkInterval {
		return false
	}
	b.snipeSeriesWalked[key] = time.Now()
	return true
}

// snipeWalkEventSlug registers the WINNER-class markets of eventSlug (series
// moneyline + game/map winners, active and unresolved) as held watches for
// chatID (issue #94): a series' next game must not crash to recipients=0 while
// the holder's exposure carries over. Props never register; excludeMarketID
// (the already-registered held market) is skipped. Rate-limited per
// (chat, event) by snipeSeriesWalkInterval; fail-open on fetch errors — the
// held market itself stays watched regardless.
func (b *Bot) snipeWalkEventSlug(chatID int64, eventSlug, excludeMarketID string, ttl time.Duration) {
	if b.snipeWatcher == nil || eventSlug == "" || !b.snipeSeriesWalkDue(chatID, eventSlug) {
		return
	}
	mc := b.snipeMarkets
	if mc == nil {
		mc = polymarket.NewMarketClient()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	mates, err := mc.GetEventMarkets(ctx, eventSlug)
	if err != nil {
		// Un-stamp so the next trigger retries: a stamped failure would leave
		// the recipients=0 window open for the whole walk interval (round-2
		// review F1). Retry pressure stays bounded — triggers are buy events
		// and user-driven position views.
		b.snipeSeriesWalkMu.Lock()
		delete(b.snipeSeriesWalked, fmt.Sprintf("%d:%s", chatID, eventSlug))
		b.snipeSeriesWalkMu.Unlock()
		log.Printf("Snipe series watch: event fetch %s: %v — held market stays watched alone, walk retryable", eventSlug, err)
		return
	}
	count := 0
	for _, mate := range mates {
		if mate == nil || mate.ID == "" || mate.ID == excludeMarketID ||
			!mate.Active || mate.Closed || !live.SeriesWatchMarket(mate.Question) {
			continue
		}
		outcomes := mate.GetOutcomes()
		for i, tokenID := range mate.GetClobTokenIds() {
			if tokenID == "" {
				continue
			}
			if count >= snipeEventMateMaxTokens {
				log.Printf("Snipe series watch: event %s exceeds %d tokens — remaining winner markets dropped", eventSlug, snipeEventMateMaxTokens)
				return
			}
			outcome := ""
			if i < len(outcomes) {
				outcome = outcomes[i]
			}
			// Series continuations register as WALKED (issue #102): alert-only —
			// the holder never personally traded these markets, so no auto money.
			b.snipeWatcher.WatchWalked(chatID, snipeMarketFromGamma(mate, tokenID, outcome), ttl)
			count++
		}
	}
}

// registerSnipeHeld registers fetched positions as held-token watches with a
// TTL. Each position's WHOLE market is registered (both sides, issue #78);
// metadata is fetched once per market and skipped entirely for tokens the
// watcher already knows — RenewHeldMarket then renews the held token AND its
// watched sibling, so the flip side's TTL never lapses out from under it. Call
// with `go` from handlers.
func (b *Bot) registerSnipeHeld(chatID int64, positions []*polymarket.Position) {
	if b.snipeWatcher == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	marketClient := b.snipeMarkets
	if marketClient == nil {
		marketClient = polymarket.NewMarketClient()
	}
	markets := make(map[string]*polymarket.GammaMarket)
	for _, pos := range positions {
		if pos.TokenID == "" {
			continue
		}
		if b.snipeWatcher.RenewHeldMarket(chatID, pos.TokenID, live.SnipeHeldTTL) {
			// Renewed without a metadata fetch — but the series walk must still
			// run for events whose mates were never registered (armed-only or
			// pre-#94 states) or have lapsed. Deduped per (chat, event) by
			// snipeSeriesWalkInterval, so steady-state refreshes stay free.
			if slug := b.snipeWatcher.EventSlugOf(pos.TokenID); slug != "" {
				b.snipeWalkEventSlug(chatID, slug, "", live.SnipeHeldTTL)
			}
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
		b.snipeWatchHeldMarket(chatID, market, live.SnipeHeldTTL)
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
	// idx validates the buy is real (the caller's bought outcome); the fan-out
	// then registers BOTH sides of the market, since the flip side is where the
	// comeback crash and the boxed case-3 buy land (issue #78).
	if idx < 0 || idx >= len(tokenIDs) || tokenIDs[idx] == "" {
		return
	}
	// The buy handlers fetch by path form, which omits events[] (issue #99).
	// Graft it before own-token registration so BOTH this market's EventSlug
	// stamping (renewal grouping) and the series walk below have the slug — but
	// after the guards above, so a rejected registration never fetches.
	b.ensureMarketEvents(market)
	// Own-market registration stays inline (in-memory, cheap); the series walk
	// does a Gamma fetch and must not block the buy handler (issue #94 review
	// F3) — it runs detached.
	outcomes := market.GetOutcomes()
	for i, tokenID := range tokenIDs {
		if tokenID == "" {
			continue
		}
		outcome := ""
		if i < len(outcomes) {
			outcome = outcomes[i]
		}
		b.snipeWatcher.WatchHeld(chatID, snipeMarketFromGamma(market, tokenID, outcome), live.SnipeHeldTTL)
	}
	go b.snipeWatchEventMates(chatID, market, live.SnipeHeldTTL)
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
