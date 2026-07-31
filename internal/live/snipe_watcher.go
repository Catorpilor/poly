package live

import (
	"context"
	"log"
	"sync"
	"time"
)

// Comeback Snipe thresholds are product policy — deliberately global
// constants, not per-user configuration (see CONTEXT.md "Comeback Snipe").
const (
	// SnipeCompetitiveBid is the Session High bar: a token must have traded
	// with a best bid at or above this to count as "formerly competitive".
	SnipeCompetitiveBid = 0.40
	// SnipeCrashAsk is the crash bar: an ask at or below this on a formerly
	// competitive in-play token triggers the alert.
	SnipeCrashAsk = 0.18
)

// snipeResetAsk is the episode reset level: after an alert, the token re-alerts
// only once its ask has recovered ABOVE this midpoint and crashed again — a
// real recovery, not spread noise. Derived, never a literal.
func snipeResetAsk() float64 {
	return (SnipeCrashAsk + SnipeCompetitiveBid) / 2
}

// SnipeHeldTTL is how long a held-position registration keeps a token watched
// without renewal. Long enough to cover any single game; refreshed every time
// the user fetches positions.
const SnipeHeldTTL = 6 * time.Hour

// snipeJanitorInterval is how often the watcher sweeps expired held
// registrations for tokens whose feed has gone quiet (no update ever reaches
// evaluate, so lazy pruning alone would leak the feed subscription).
const snipeJanitorInterval = 10 * time.Minute

// SnipeMarket is the market metadata the watcher carries per token — enough to
// build an alert and to route the one-tap buy without re-deriving anything.
type SnipeMarket struct {
	TokenID  string
	MarketID string // Gamma market ID, used by the buy path
	Question string // market title
	Outcome  string
	// GameStart is the scheduled game start from Gamma's market metadata
	// (gameStartTime). Zero means unknown — such tokens are never considered
	// in-play and never alert (fewer alerts, never wrong ones).
	GameStart time.Time
}

// SnipeNotifier delivers a snipe alert to one recipient.
type SnipeNotifier interface {
	NotifySnipeAlert(chatID int64, market SnipeMarket, sessionHigh, ask float64)
}

// SnipeRecipientResolver resolves the externally-tracked alert audiences at
// fire time: event subscribers (SubscriptionRegistry) and SL/TP arm owners
// (arm store). Held-position holders are tracked by the watcher itself.
type SnipeRecipientResolver interface {
	EventSubscribers(eventSlug string) []int64
	ArmOwners(tokenID string) []int64
}

// snipeTokenState is the in-memory, restart-resettable per-token state. After
// a restart the session high rebuilds from zero, so a crash immediately after
// a restart cannot alert.
type snipeTokenState struct {
	market      SnipeMarket
	sessionHigh float64
	alerted     bool
	bought      bool
	// Watch sources. A token stays watched while any source is live.
	events  map[string]bool     // subscribed event slugs
	armed   bool                // watched because an SL/TP arm exists
	holders map[int64]time.Time // held-position chatID -> registration expiry
	// feedRef records whether the watcher holds its own price-feed
	// subscription for this token (event/held sources). Armed tokens ride the
	// SL/TP monitor's existing subscription and never set it.
	feedRef bool
}

func newSnipeTokenState(m SnipeMarket) *snipeTokenState {
	return &snipeTokenState{
		market:  m,
		events:  make(map[string]bool),
		holders: make(map[int64]time.Time),
	}
}

// sourcesLive reports whether any watch source remains after pruning holders
// expired at now.
func (st *snipeTokenState) sourcesLive(now time.Time) bool {
	for id, exp := range st.holders {
		if now.After(exp) {
			delete(st.holders, id)
		}
	}
	return st.armed || len(st.events) > 0 || len(st.holders) > 0
}

// SnipeWatcher watches a universe of tokens for the comeback-snipe pattern:
// in-play market, Session High bid >= SnipeCompetitiveBid, ask crashed to
// <= SnipeCrashAsk. It alerts (episode-based, see snipeResetAsk) — it never
// buys on its own. All state is in-memory by design.
type SnipeWatcher struct {
	ctx        context.Context
	cancel     context.CancelFunc
	feed       PriceFeedSubscriber
	recipients SnipeRecipientResolver
	notifier   SnipeNotifier
	// now is the watcher's clock — overridable in tests.
	now func() time.Time

	mu     sync.Mutex
	tokens map[string]*snipeTokenState
	// eventTokens indexes event slug -> token IDs so UnwatchEventMarkets can
	// find its tokens without scanning.
	eventTokens map[string]map[string]bool
}

// NewSnipeWatcher builds the watcher. Call Start to hook it into the feed.
func NewSnipeWatcher(feed PriceFeedSubscriber, recipients SnipeRecipientResolver, notifier SnipeNotifier) *SnipeWatcher {
	ctx, cancel := context.WithCancel(context.Background())
	return &SnipeWatcher{
		ctx:         ctx,
		cancel:      cancel,
		feed:        feed,
		recipients:  recipients,
		notifier:    notifier,
		now:         time.Now,
		tokens:      make(map[string]*snipeTokenState),
		eventTokens: make(map[string]map[string]bool),
	}
}

// Start registers the feed update handler and launches the expiry janitor.
func (w *SnipeWatcher) Start() {
	w.feed.OnUpdate(w.handleUpdate)
	go w.janitorLoop()
	log.Println("SnipeWatcher: Started")
}

// Stop cancels the janitor. Feed subscriptions are dropped with the process.
func (w *SnipeWatcher) Stop() {
	w.cancel()
}

// WatchEventMarkets registers an event's market tokens, subscribing each to
// the price feed. Idempotent per (eventSlug, token). Called when an event
// gains a subscriber (telegram or web).
func (w *SnipeWatcher) WatchEventMarkets(eventSlug string, markets []SnipeMarket) {
	var subscribe []string
	w.mu.Lock()
	for _, m := range markets {
		if m.TokenID == "" {
			continue
		}
		st := w.ensureStateLocked(m)
		if st.events[eventSlug] {
			continue
		}
		st.events[eventSlug] = true
		if w.eventTokens[eventSlug] == nil {
			w.eventTokens[eventSlug] = make(map[string]bool)
		}
		w.eventTokens[eventSlug][m.TokenID] = true
		if !st.feedRef {
			st.feedRef = true
			subscribe = append(subscribe, m.TokenID)
		}
	}
	w.mu.Unlock()
	for _, id := range subscribe {
		w.feed.Subscribe(id)
	}
}

// UnwatchEventMarkets drops the event source from its tokens. Called when the
// event's last subscriber (telegram or web) leaves. Tokens with no remaining
// source are released.
func (w *SnipeWatcher) UnwatchEventMarkets(eventSlug string) {
	now := w.now()
	var unsub []string
	w.mu.Lock()
	for tokenID := range w.eventTokens[eventSlug] {
		st := w.tokens[tokenID]
		if st == nil {
			continue
		}
		delete(st.events, eventSlug)
		if !st.sourcesLive(now) {
			if st.feedRef {
				unsub = append(unsub, tokenID)
			}
			delete(w.tokens, tokenID)
		}
	}
	delete(w.eventTokens, eventSlug)
	w.mu.Unlock()
	w.unsubscribeReleased(unsub)
}

// WatchArmed registers a token watched because an SL/TP arm exists on it. The
// SL/TP monitor already holds the feed subscription for armed tokens, so the
// watcher does not add its own ref. Alert recipients resolve live via
// SnipeRecipientResolver.ArmOwners.
func (w *SnipeWatcher) WatchArmed(m SnipeMarket) {
	if m.TokenID == "" {
		return
	}
	w.mu.Lock()
	st := w.ensureStateLocked(m)
	st.armed = true
	w.mu.Unlock()
}

// UnwatchArmed drops the armed source (manual disarm). The token is released
// when no other source remains; the feed ref is dropped only when the watcher
// holds one (event/held) — armed tokens ride the SL/TP monitor's subscription
// and Unsubscribing it here would steal the monitor's ref.
func (w *SnipeWatcher) UnwatchArmed(tokenID string) {
	now := w.now()
	var unsub []string
	w.mu.Lock()
	if st := w.tokens[tokenID]; st != nil {
		st.armed = false
		if !st.sourcesLive(now) {
			if st.feedRef {
				unsub = append(unsub, tokenID)
			}
			delete(w.tokens, tokenID)
		}
	}
	w.mu.Unlock()
	w.unsubscribeReleased(unsub)
}

// WatchHeld registers a held position: chatID is alerted while the
// registration lives. The watcher holds a feed subscription for held tokens
// (they are not otherwise on the feed) and releases it when the TTL expires
// without renewal.
func (w *SnipeWatcher) WatchHeld(chatID int64, m SnipeMarket, ttl time.Duration) {
	if m.TokenID == "" {
		return
	}
	var subscribe bool
	w.mu.Lock()
	st := w.ensureStateLocked(m)
	st.holders[chatID] = w.now().Add(ttl)
	if !st.feedRef {
		st.feedRef = true
		subscribe = true
	}
	w.mu.Unlock()
	if subscribe {
		w.feed.Subscribe(m.TokenID)
	}
}

// RenewHeld extends a holder's TTL when the token is already watched, so
// callers can skip re-fetching market metadata. Returns false for unknown
// tokens (caller must WatchHeld with full metadata instead).
func (w *SnipeWatcher) RenewHeld(chatID int64, tokenID string, ttl time.Duration) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	st := w.tokens[tokenID]
	if st == nil {
		return false
	}
	st.holders[chatID] = w.now().Add(ttl)
	return true
}

// MarkBought latches the bought flag: a snipe buy silences the token's alerts
// for the rest of the match (state is per-session; matches end with the
// market, so no un-latch is needed).
func (w *SnipeWatcher) MarkBought(tokenID string) {
	w.mu.Lock()
	if st := w.tokens[tokenID]; st != nil {
		st.bought = true
	}
	w.mu.Unlock()
}

// ensureStateLocked returns the token's state, creating it from m when absent.
// Existing state keeps its session high / episode flags; empty metadata fields
// are backfilled so a later, richer registration can fill gaps.
func (w *SnipeWatcher) ensureStateLocked(m SnipeMarket) *snipeTokenState {
	st, ok := w.tokens[m.TokenID]
	if !ok {
		st = newSnipeTokenState(m)
		w.tokens[m.TokenID] = st
		return st
	}
	if st.market.MarketID == "" {
		st.market.MarketID = m.MarketID
	}
	if st.market.Question == "" {
		st.market.Question = m.Question
	}
	if st.market.Outcome == "" {
		st.market.Outcome = m.Outcome
	}
	if st.market.GameStart.IsZero() {
		st.market.GameStart = m.GameStart
	}
	return st
}

// unsubscribeReleased drops the watcher's feed refs for released tokens.
// Callers collect only tokens whose feedRef was set (event/held sources) —
// armed tokens ride the SL/TP monitor's subscription and are never passed.
func (w *SnipeWatcher) unsubscribeReleased(tokenIDs []string) {
	for _, id := range tokenIDs {
		w.feed.Unsubscribe(id)
	}
}

// handleUpdate is registered with the price feed. Untracked tokens (e.g.
// armed-only SL/TP tokens the watcher never registered) are filtered before
// spawning the evaluation goroutine.
func (w *SnipeWatcher) handleUpdate(tokenID string) {
	if !w.isWatched(tokenID) {
		return
	}
	go w.evaluateFromFeed(tokenID)
}

func (w *SnipeWatcher) isWatched(tokenID string) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.tokens[tokenID] != nil
}

// evaluateFromFeed reads prices from the feed and evaluates. The ask is only
// consulted when it can matter — a session high at/above the bar, or an armed
// episode awaiting reset — because BestAsk may fall back to an HTTP fetch.
func (w *SnipeWatcher) evaluateFromFeed(tokenID string) {
	bid, ok := w.feed.BestBid(tokenID)
	if !ok {
		bid = 0
	}
	if !w.askRelevant(tokenID, bid) {
		w.evaluate(tokenID, bid, 0)
		return
	}
	ask, ok := w.feed.BestAsk(tokenID)
	if !ok {
		ask = 0
	}
	w.evaluate(tokenID, bid, ask)
}

// askRelevant reports whether the ask can influence this evaluation: it can
// fire an alert only once the session high (including this bid) reaches the
// bar, and it can reset an episode only when one is latched.
func (w *SnipeWatcher) askRelevant(tokenID string, bid float64) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	st := w.tokens[tokenID]
	if st == nil {
		return false
	}
	return st.alerted || st.sessionHigh >= SnipeCompetitiveBid || bid >= SnipeCompetitiveBid
}

// evaluate runs the snipe state machine for one observation. bid/ask <= 0
// mean "no data" for that side: the bid still ratchets the session high when
// positive; a missing ask can neither fire nor reset.
//
//   - ratchet: sessionHigh = max(sessionHigh, bid)
//   - episode reset: alerted clears once ask > snipeResetAsk()
//   - fire: sessionHigh >= SnipeCompetitiveBid && 0 < ask <= SnipeCrashAsk
//     && !alerted && !bought && in-play (game started per Gamma metadata)
//
// The alerted latch flips under the mutex, so concurrent evaluations of the
// same crash fire exactly once. Expired held registrations are pruned here;
// a token left without sources is released.
func (w *SnipeWatcher) evaluate(tokenID string, bid, ask float64) {
	now := w.now()

	w.mu.Lock()
	st := w.tokens[tokenID]
	if st == nil {
		w.mu.Unlock()
		return
	}
	if !st.sourcesLive(now) {
		hadFeedRef := st.feedRef
		delete(w.tokens, tokenID)
		w.mu.Unlock()
		if hadFeedRef {
			w.unsubscribeReleased([]string{tokenID})
		}
		return
	}

	if bid > st.sessionHigh {
		st.sessionHigh = bid
	}
	if st.alerted && ask > snipeResetAsk() {
		st.alerted = false
	}

	fire := !st.alerted && !st.bought &&
		st.sessionHigh >= SnipeCompetitiveBid &&
		ask > 0 && ask <= SnipeCrashAsk &&
		w.inPlay(st.market, now)
	if fire {
		st.alerted = true
	}
	market := st.market
	high := st.sessionHigh
	var eventSlugs []string
	var holders []int64
	if fire {
		for slug := range st.events {
			eventSlugs = append(eventSlugs, slug)
		}
		for id := range st.holders {
			holders = append(holders, id)
		}
	}
	w.mu.Unlock()

	if fire {
		log.Printf("SnipeWatcher: alert token=%s high=%.4f ask=%.4f", tokenID, high, ask)
		w.dispatch(market, high, ask, eventSlugs, holders)
	}
}

// inPlay gates alerts to started games: the scheduled start is known and has
// passed. Unknown start (zero) is never in-play — fewer alerts, never wrong
// ones.
func (w *SnipeWatcher) inPlay(m SnipeMarket, now time.Time) bool {
	return !m.GameStart.IsZero() && !now.Before(m.GameStart)
}

// dispatch notifies the union of event subscribers, arm owners, and holders —
// each recipient exactly once.
func (w *SnipeWatcher) dispatch(market SnipeMarket, high, ask float64, eventSlugs []string, holders []int64) {
	seen := make(map[int64]bool)
	var order []int64
	add := func(ids []int64) {
		for _, id := range ids {
			if !seen[id] {
				seen[id] = true
				order = append(order, id)
			}
		}
	}
	for _, slug := range eventSlugs {
		add(w.recipients.EventSubscribers(slug))
	}
	add(w.recipients.ArmOwners(market.TokenID))
	add(holders)

	for _, chatID := range order {
		w.notifier.NotifySnipeAlert(chatID, market, high, ask)
	}
}

// janitorLoop sweeps expired held registrations so quiet tokens (no feed
// updates reaching evaluate) still release their feed subscription.
func (w *SnipeWatcher) janitorLoop() {
	t := time.NewTicker(snipeJanitorInterval)
	defer t.Stop()
	for {
		select {
		case <-w.ctx.Done():
			return
		case <-t.C:
			w.sweepExpired()
		}
	}
}

// sweepExpired releases every token whose sources have all lapsed.
func (w *SnipeWatcher) sweepExpired() {
	now := w.now()
	var unsub []string
	w.mu.Lock()
	for tokenID, st := range w.tokens {
		if st.sourcesLive(now) {
			continue
		}
		if st.feedRef {
			unsub = append(unsub, tokenID)
		}
		delete(w.tokens, tokenID)
	}
	w.mu.Unlock()
	w.unsubscribeReleased(unsub)
}

// snipeRecipientAdapter is the production SnipeRecipientResolver: event
// subscribers from the live manager's SubscriptionRegistry, arm owners from
// the SL/TP arm store.
type snipeRecipientAdapter struct {
	manager *LiveTradeManager
	store   SLTPArmStore
}

// NewSnipeRecipientResolver builds the production recipient resolver.
func NewSnipeRecipientResolver(m *LiveTradeManager, store SLTPArmStore) SnipeRecipientResolver {
	return &snipeRecipientAdapter{manager: m, store: store}
}

func (a *snipeRecipientAdapter) EventSubscribers(eventSlug string) []int64 {
	return a.manager.subscriptions.GetTelegramSubscribers(eventSlug)
}

func (a *snipeRecipientAdapter) ArmOwners(tokenID string) []int64 {
	arms, err := a.store.ListArmedByToken(context.Background(), tokenID)
	if err != nil {
		log.Printf("SnipeWatcher: list arm owners for %s: %v", tokenID, err)
		return nil
	}
	seen := make(map[int64]bool)
	var owners []int64
	for _, arm := range arms {
		if !seen[arm.TelegramID] {
			seen[arm.TelegramID] = true
			owners = append(owners, arm.TelegramID)
		}
	}
	return owners
}

// sessionHigh exposes a token's current session high for tests.
func (w *SnipeWatcher) sessionHigh(tokenID string) float64 {
	w.mu.Lock()
	defer w.mu.Unlock()
	if st := w.tokens[tokenID]; st != nil {
		return st.sessionHigh
	}
	return 0
}
