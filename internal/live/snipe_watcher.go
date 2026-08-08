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
	SnipeCrashAsk = 0.20
	// SnipeMinAsk is the corpse floor: an ask below this means the market is
	// declaring death or settlement, not overreacting — there is no game-end
	// signal in the metadata, so without this bound a finished game's loser
	// token alerts at $0.001 with a fantasy 1000× payout (first live alert,
	// 2026-08-01 MOUZ). Comeback snipes want panic pricing, not corpse
	// pricing; every genuine studied case (0.125–0.195) clears this easily.
	SnipeMinAsk = 0.03
)

// snipeResetAsk is the episode reset level: after an alert, the token re-alerts
// only once its ask has recovered ABOVE this midpoint and crashed again — a
// real recovery, not spread noise. Derived, never a literal.
func snipeResetAsk() float64 {
	return (SnipeCrashAsk + SnipeCompetitiveBid) / 2
}

// SnipeDeepFloor bounds the Deep Crash tier (ADR 0007): inside an episode
// that already produced a genuine in-band alert, an ask in
// [SnipeDeepFloor, SnipeMinAsk) fires the deep tier — the prior alert is the
// evidence that this is a live panic that crashed through the band under
// watch, not a corpse first sighted cheap. Below this floor is settlement
// dust and never fires (observed winners bottomed at 0.0065 and 0.015).
const SnipeDeepFloor = 0.005

// Shadow underdog-dip instrumentation (log-only, September review). The
// Enterprise case (2026-08-07): a 0.365-high underdog dipped to 0.095 and won
// the series — structurally excluded by SnipeCompetitiveBid. Whether that
// class has positive expectancy is unknown; these bounds define the shadow
// band that gets LOGGED (never notified, never bought) so September can
// decide on recovery-rate data instead of one glorious counterexample.
const (
	snipeShadowHighMin  = 0.30 // session high at or above this (and below SnipeCompetitiveBid)
	snipeShadowCrashAsk = 0.15 // ask at or below this fires the shadow log
)

// snipeResetConfirm is how long the ask must HOLD above snipeResetAsk before
// the episode un-latches. A single tick above the reset level used to clear
// the latch instantly — in a whipsawing thin book that re-alerted the same
// crash 2 seconds apart while the first alert's auto-buy was still in flight
// (issue #50, 2026-08-05 HANJIN BRION). A real recovery holds the level;
// spread noise doesn't.
const snipeResetConfirm = 10 * time.Second

// SnipeHeldTTL is how long a held-position registration keeps a token watched
// without renewal. Long enough to cover any single game; refreshed every time
// the user fetches positions.
const SnipeHeldTTL = 6 * time.Hour

// snipeJanitorInterval is how often the watcher sweeps expired held
// registrations for tokens whose feed has gone quiet (no update ever reaches
// evaluate, so lazy pruning alone would leak the feed subscription).
const snipeJanitorInterval = 10 * time.Minute

// snipeTickInterval is the periodic re-evaluation cadence, mirroring the
// SL/TP monitor's tick. The WS OnUpdate callback is the fast path; the tick
// is the guarantee that a token whose WS subscription went silent (issue #42:
// rejected mid-session resubscribes) is still evaluated (issue #41).
const snipeTickInterval = 20 * time.Second

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

// SnipeNotifier delivers snipe alerts to one recipient. NotifySnipeDeepCrash
// is the Deep Crash tier (ADR 0007): alertAsk and sinceAlert describe the
// episode's earlier in-band alert.
type SnipeNotifier interface {
	NotifySnipeAlert(chatID int64, market SnipeMarket, sessionHigh, ask float64)
	NotifySnipeDeepCrash(chatID int64, market SnipeMarket, sessionHigh, ask, alertAsk float64, sinceAlert time.Duration)
}

// SnipeHistorySeeder supplies a token's recent trade high so a Session High
// can be seeded at watch-start. Production: TradingClient.MaxTradePriceSince
// over the CLOB's public price history. (0, false) means "no seed".
type SnipeHistorySeeder interface {
	MaxTradePriceSince(ctx context.Context, tokenID string, since time.Time) (float64, bool)
}

// Session High seeding windows: the fetch reaches back to 2h before the
// scheduled game start (pre-game trading carries the competitive price), or
// 6h from now when the start is unknown.
const (
	snipeSeedPreGameWindow  = 2 * time.Hour
	snipeSeedFallbackWindow = 6 * time.Hour
)

// SnipeRecipientResolver resolves the externally-tracked alert audiences at
// fire time: event subscribers (SubscriptionRegistry) and SL/TP arm owners
// (arm store). Held-position holders are tracked by the watcher itself.
type SnipeRecipientResolver interface {
	EventSubscribers(eventSlug string) []int64
	ArmOwners(tokenID string) []int64
}

// snipeTokenState is the in-memory per-token state. The session high is
// seeded asynchronously from recent trade history when a seeder is wired
// (a restart re-seeds); without one it rebuilds from zero on live bids only.
type snipeTokenState struct {
	market      SnipeMarket
	sessionHigh float64
	alerted     bool
	bought      bool
	// resetSince is when the current above-reset recovery streak began; zero
	// when none is running. The episode un-latches only once the streak spans
	// snipeResetConfirm (issue #50).
	resetSince time.Time
	// dispatching marks an alert delivery (including the v2 auto-buy) in
	// flight; the episode never un-latches underneath it (issue #50).
	dispatching bool
	// lastAlertAt is when this token last fired. Log-only instrumentation
	// for the corpse-filter review: a crash whose same-market complement also
	// alerted recently is a see-saw game, not a decided one.
	lastAlertAt time.Time
	// alertAsk is the ask at this episode's in-band alert — carried into the
	// Deep Crash notification so the message can say "alerted at $X earlier".
	alertAsk float64
	// shadowAlerted latches the log-only underdog-dip shadow once per
	// episode; shadowCount tallies fires for tests and has no runtime role.
	shadowAlerted bool
	shadowCount   int
	// deepAlerted latches the Deep Crash tier: once per episode, cleared by
	// the same sustained recovery that clears alerted (ADR 0007). Deliberately
	// NOT gated on bought — the in-band auto-buy usually already fired.
	deepAlerted bool
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
	// seeder, when set, seeds a new token state's Session High from recent
	// trade history. Nil disables seeding. Set before Start.
	seeder SnipeHistorySeeder
	// now is the watcher's clock — overridable in tests.
	now func() time.Time
	// tickInterval is the periodic re-evaluation cadence. Tests override it
	// before Start() to make ticker-driven tests deterministic.
	tickInterval time.Duration

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
		ctx:          ctx,
		cancel:       cancel,
		feed:         feed,
		recipients:   recipients,
		notifier:     notifier,
		now:          time.Now,
		tickInterval: snipeTickInterval,
		tokens:       make(map[string]*snipeTokenState),
		eventTokens:  make(map[string]map[string]bool),
	}
}

// SetHistorySeeder wires the optional Session High seeder. Call before Start
// (and before any registration) — the field is read unguarded.
func (w *SnipeWatcher) SetHistorySeeder(s SnipeHistorySeeder) {
	w.seeder = s
}

// Start registers the feed update handler and launches the expiry janitor and
// the periodic re-evaluation tick.
func (w *SnipeWatcher) Start() {
	w.feed.OnUpdate(w.handleUpdate)
	go w.janitorLoop()
	go w.tickLoop()
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
// A newly created state gets its Session High seeded asynchronously from
// trade history (once per state — every registration path funnels through
// here, and existing states never re-fetch). Existing state keeps its session
// high / episode flags; empty metadata fields are backfilled so a later,
// richer registration can fill gaps.
func (w *SnipeWatcher) ensureStateLocked(m SnipeMarket) *snipeTokenState {
	st, ok := w.tokens[m.TokenID]
	if !ok {
		st = newSnipeTokenState(m)
		w.tokens[m.TokenID] = st
		if w.seeder != nil {
			go w.seedFromHistory(m)
		}
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

// seedFromHistory fetches the token's recent trade high and applies it as the
// initial Session High, so a watch registered mid-game (late subscription or
// restart) still knows the token was formerly competitive. Runs in its own
// goroutine; w.ctx bounds the fetch.
func (w *SnipeWatcher) seedFromHistory(m SnipeMarket) {
	since := w.now().Add(-snipeSeedFallbackWindow)
	if !m.GameStart.IsZero() {
		since = m.GameStart.Add(-snipeSeedPreGameWindow)
	}
	price, ok := w.seeder.MaxTradePriceSince(w.ctx, m.TokenID, since)
	if !ok {
		log.Printf("SnipeWatcher: seed unavailable token=%.12s…", m.TokenID)
		return
	}
	log.Printf("SnipeWatcher: seeded token=%.12s… high=%.3f since=%s",
		m.TokenID, price, since.UTC().Format("15:04"))
	w.seedSessionHigh(m.TokenID, price)
}

// seedSessionHigh raises tokenID's session high to price — never lowers it
// (live bids may already have ratcheted higher while the fetch was in
// flight) and never touches episode latches, so a late-arriving seed cannot
// corrupt an alerted or bought token.
func (w *SnipeWatcher) seedSessionHigh(tokenID string, price float64) {
	w.mu.Lock()
	defer w.mu.Unlock()
	st := w.tokens[tokenID]
	if st == nil {
		return
	}
	if price > st.sessionHigh {
		st.sessionHigh = price
	}
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
	if (st.alerted || st.shadowAlerted) && !st.dispatching && ask > snipeResetAsk() {
		if st.resetSince.IsZero() {
			st.resetSince = now
		} else if now.Sub(st.resetSince) >= snipeResetConfirm {
			st.alerted = false
			st.deepAlerted = false
			st.shadowAlerted = false
			st.resetSince = time.Time{}
		}
	} else {
		st.resetSince = time.Time{}
	}

	fire := !st.alerted && !st.bought &&
		st.sessionHigh >= SnipeCompetitiveBid &&
		ask >= SnipeMinAsk && ask <= SnipeCrashAsk &&
		w.inPlay(st.market, now)
	var pairAgo string
	if fire {
		st.alerted = true
		st.dispatching = true
		st.lastAlertAt = now
		st.alertAsk = ask
		st.deepAlerted = false
		pairAgo = "never"
		if at, ok := w.pairLastAlertLocked(st.market.MarketID, tokenID); ok {
			pairAgo = now.Sub(at).Round(time.Second).String()
		}
	}

	// Deep Crash tier (ADR 0007): requires this episode's in-band alert (the
	// live-panic evidence), fires once, ignores bought — the $10 usually
	// already bought and the dip is the top-up moment. Zones are disjoint, so
	// fire and deepFire are mutually exclusive.
	deepFire := st.alerted && !st.deepAlerted && !st.dispatching && !fire &&
		ask >= SnipeDeepFloor && ask < SnipeMinAsk &&
		w.inPlay(st.market, now)
	var sinceAlert time.Duration
	var alertAsk float64
	if deepFire {
		st.deepAlerted = true
		st.dispatching = true
		sinceAlert = now.Sub(st.lastAlertAt)
		alertAsk = st.alertAsk
	}

	// Shadow underdog-dip (log-only): sub-competitive high in [0.30, 0.40)
	// printing at or below the shadow band. Never notifies, never buys,
	// never touches the real tiers' state beyond its own latch.
	shadowFire := !st.shadowAlerted && !st.alerted && !st.bought &&
		st.sessionHigh >= snipeShadowHighMin && st.sessionHigh < SnipeCompetitiveBid &&
		ask >= SnipeDeepFloor && ask <= snipeShadowCrashAsk &&
		w.inPlay(st.market, now)
	if shadowFire {
		st.shadowAlerted = true
		st.shadowCount++
	}

	market := st.market
	high := st.sessionHigh
	var eventSlugs []string
	var holders []int64
	if fire || deepFire {
		for slug := range st.events {
			eventSlugs = append(eventSlugs, slug)
		}
		for id := range st.holders {
			holders = append(holders, id)
		}
	}
	w.mu.Unlock()

	if fire {
		recipients := w.dispatch(market, high, ask, eventSlugs, holders)
		// bid/impliedComplement/pairAlerted are log-only instrumentation for
		// the corpse-filter review: complement bid is mechanically 1-ask on
		// Polymarket's mirrored binary books; a wide own-side spread and a
		// never-alerted pair are the corpse signature, a recently-alerted
		// pair is the see-saw signature.
		log.Printf("SnipeWatcher: alert token=%.12s… high=%.3f ask=%.3f bid=%.3f impliedComplement=%.3f pairAlerted=%s recipients=%d",
			tokenID, high, ask, bid, 1-ask, pairAgo, recipients)
	}
	if deepFire {
		recipients := 0
		for _, chatID := range w.recipientOrder(market, eventSlugs, holders) {
			w.notifier.NotifySnipeDeepCrash(chatID, market, high, ask, alertAsk, sinceAlert)
			recipients++
		}
		log.Printf("SnipeWatcher: deep-crash token=%.12s… high=%.3f ask=%.3f bid=%.3f sinceAlert=%s alertAsk=%.3f recipients=%d",
			tokenID, high, ask, bid, sinceAlert.Round(time.Second), alertAsk, recipients)
	}
	if shadowFire {
		log.Printf("SnipeWatcher: shadow-alert class=underdog-dip token=%.12s… high=%.3f ask=%.3f bid=%.3f impliedComplement=%.3f",
			tokenID, high, ask, bid, 1-ask)
	}
	if fire || deepFire {
		w.mu.Lock()
		if st := w.tokens[tokenID]; st != nil {
			st.dispatching = false
		}
		w.mu.Unlock()
	}
}

// pairLastAlertLocked returns the most recent alert time among OTHER tokens
// of the same market — the complement side(s) in a binary market. Callers
// hold w.mu. Log-only instrumentation: blocks nothing.
func (w *SnipeWatcher) pairLastAlertLocked(marketID, exceptTokenID string) (time.Time, bool) {
	if marketID == "" {
		return time.Time{}, false
	}
	var latest time.Time
	for id, st := range w.tokens {
		if id == exceptTokenID || st.market.MarketID != marketID || st.lastAlertAt.IsZero() {
			continue
		}
		if st.lastAlertAt.After(latest) {
			latest = st.lastAlertAt
		}
	}
	return latest, !latest.IsZero()
}

// inPlay gates alerts to started games: the scheduled start is known and has
// passed. Unknown start (zero) is never in-play — fewer alerts, never wrong
// ones.
func (w *SnipeWatcher) inPlay(m SnipeMarket, now time.Time) bool {
	return !m.GameStart.IsZero() && !now.Before(m.GameStart)
}

// dispatch notifies the union of event subscribers, arm owners, and holders —
// each recipient exactly once. Returns the number of recipients notified.
func (w *SnipeWatcher) dispatch(market SnipeMarket, high, ask float64, eventSlugs []string, holders []int64) int {
	order := w.recipientOrder(market, eventSlugs, holders)
	for _, chatID := range order {
		w.notifier.NotifySnipeAlert(chatID, market, high, ask)
	}
	return len(order)
}

// recipientOrder resolves the alert audience — the union of event
// subscribers, arm owners, and holders, each exactly once — shared by the
// in-band and Deep Crash tiers.
func (w *SnipeWatcher) recipientOrder(market SnipeMarket, eventSlugs []string, holders []int64) []int64 {
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
	return order
}

// tickLoop runs the periodic re-evaluation. The WS OnUpdate callback is the
// only other evaluation trigger, and a rejected resubscribe can silence it for
// an entire game (issue #41: the 2026-08-01 FaZe crash printed through the
// snipe zone with zero WS events). Exits when the watcher's context is
// cancelled (Stop()).
func (w *SnipeWatcher) tickLoop() {
	t := time.NewTicker(w.tickInterval)
	defer t.Stop()
	for {
		select {
		case <-w.ctx.Done():
			return
		case <-t.C:
			w.tickEvaluateAll()
		}
	}
}

// tickEvaluateAll snapshots the watched tokens under the mutex, then fans out
// one evaluation goroutine per token — the same pattern as handleUpdate and
// the SL/TP tick — because BestBid/BestAsk may fall back to an HTTP fetch
// (5s timeout) and one slow token must not delay crash detection on the
// others. Double-fire safety against a racing WS evaluation rests on the
// alerted latch flipping under the mutex in evaluate.
func (w *SnipeWatcher) tickEvaluateAll() {
	w.mu.Lock()
	tokenIDs := make([]string, 0, len(w.tokens))
	for id := range w.tokens {
		tokenIDs = append(tokenIDs, id)
	}
	w.mu.Unlock()
	for _, id := range tokenIDs {
		go w.evaluateFromFeed(id)
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
