package live

import (
	"context"
	"log"
	"time"
)

// eventRefreshInterval is the Event Refresh cadence (ADR 0008 phase 2): every
// subscribed event is re-resolved against Gamma this often so a market created
// after subscribe time is watched within one interval. Series games appear
// mid-series and an unrefreshed watch misses their whole crash (the Team WE
// Game-2 miss, issue #55). Product policy, a deliberate constant.
const eventRefreshInterval = 2 * time.Minute

// eventRefreshDefaultPause spaces the sequential per-event resolves inside one
// cycle so a large watch set does not burst Gamma. Applied between resolves,
// never before the first — mirrors watchRestoreDefaultPause.
const eventRefreshDefaultPause = 200 * time.Millisecond

// StartEventRefresh launches the Event Refresh loop bound to the manager's
// lifecycle: it runs on m.ctx, so Stop() (which cancels m.ctx) also stops the
// loop, exactly like pingLoop/readLoop. Call once, after the snipe watcher and
// (optionally) the watch store are wired.
func (m *LiveTradeManager) StartEventRefresh() {
	go m.runEventRefresh(m.ctx)
}

// runEventRefresh ticks refreshCycle at refreshInterval until ctx is cancelled.
// One goroutine owns the whole loop; a cycle runs to completion before the next
// tick is honored. Separated from StartEventRefresh so tests can drive it with
// their own context and a tiny interval.
func (m *LiveTradeManager) runEventRefresh(ctx context.Context) {
	t := time.NewTicker(m.refreshInterval)
	defer t.Stop()
	log.Printf("LiveTradeManager: Event Refresh loop started (interval=%v)", m.refreshInterval)
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			m.refreshCycle(ctx)
		}
	}
}

// refreshCycle re-resolves every currently-subscribed event once and registers
// any newly appeared markets/assets. Sequential with refreshPause between
// resolves. Returns the per-cycle counts (events scanned, events that gained
// something, events whose resolve failed) — also emitted as one summary line.
//
// Failure semantics are load-bearing (ADR 0008): a resolve error logs a single
// quiet line and skips to the next event. It NEVER unsubscribes and NEVER
// touches the store — closed events fail every cycle until Phase 4 ships
// expiry, and only positive closed-market evidence may drop a watch.
func (m *LiveTradeManager) refreshCycle(ctx context.Context) (events, newEvents, failed int) {
	slugs := m.subscriptions.TelegramSubscribedEvents()

	for i, slug := range slugs {
		if i > 0 && m.refreshPause > 0 {
			select {
			case <-ctx.Done():
				return events, newEvents, failed
			case <-time.After(m.refreshPause):
			}
		}
		select {
		case <-ctx.Done():
			return events, newEvents, failed
		default:
		}

		events++
		eventInfo, err := m.resolver.GetEventInfo(ctx, slug)
		if err != nil {
			// Quiet on purpose: closed events fail this lookup every cycle until
			// expiry (Phase 4). One DEBUG line per event per cycle, never a drop.
			log.Printf("EventRefresh: resolve failed event=%s: %v (keeping watch)", slug, err)
			failed++
			continue
		}

		newMarkets, newAssets := m.refreshEvent(slug, eventInfo)
		if newMarkets > 0 || newAssets > 0 {
			newEvents++
			log.Printf("EventRefresh: event=%s newMarkets=%d newAssets=%d", slug, newMarkets, newAssets)
		}
	}

	log.Printf("EventRefresh cycle: events=%d new=%d failed=%d", events, newEvents, failed)
	return events, newEvents, failed
}

// refreshEvent idempotently registers any markets/assets in eventInfo that
// appeared since eventSlug's watch was last registered, and returns the counts
// of newly registered markets and feed assets (observability only).
//
// Delta-only registration is the correctness constraint of this phase. It falls
// out of reusing the subscribe-time paths, both idempotent:
//   - trackEventAssets only rewrites the assetToEvent routing map and re-sends
//     the (already-guarded) RTDS subscribe; it never touches the price feed.
//   - WatchEventMarkets re-subscribes the price feed only for tokens not already
//     watched for this event, and returns exactly those. The feed's Subscribe is
//     ref-counted, so re-registering an unchanged event MUST yield zero calls —
//     it does, because every already-watched token is skipped.
//
// newAssets is that returned feed delta; newMarkets is how many of the resolved
// markets contributed at least one of those new tokens.
func (m *LiveTradeManager) refreshEvent(eventSlug string, eventInfo *EventInfo) (newMarkets, newAssets int) {
	// RTDS trade-feed routing: pick up any newly-active Moneyline assets.
	m.trackEventAssets(eventSlug, eventInfo)

	if m.snipeWatcher == nil {
		return 0, 0
	}

	markets := m.eventSnipeMarkets(eventSlug, eventInfo)
	newTokens := m.snipeWatcher.WatchEventMarkets(eventSlug, snipeMarketsFor(markets))
	if len(newTokens) == 0 {
		return 0, 0
	}

	newSet := make(map[string]bool, len(newTokens))
	for _, id := range newTokens {
		newSet[id] = true
	}
	for _, mkt := range markets {
		for _, tok := range mkt.GetClobTokenIds() {
			if newSet[tok] {
				newMarkets++
				break
			}
		}
	}
	return newMarkets, len(newTokens)
}
