package live

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// seriesEvent models a BO-series event whose second game market is created (or
// activated) on Gamma AFTER subscribe time — the #55 scenario. With g2Active
// false, GetAllMLMarkets returns only g1; flipping it to true is the "new
// market appeared" event a refresh cycle must discover.
func seriesEvent(slug string, g2Active bool) *EventInfo {
	const start = "2026-07-12 10:00:00+00"
	return &EventInfo{
		ID: "e", Slug: slug, Title: "AA vs BB series",
		Markets: []MarketInfo{
			{ID: "g1", Slug: slug, Question: "AA vs BB", OutcomesRaw: `["AA","BB"]`,
				ClobTokenIdsRaw: `["g1-a","g1-b"]`, GameStartTimeRaw: start, Active: true},
			{ID: "g2", Slug: slug + "-2", Question: "AA vs BB rematch", OutcomesRaw: `["AA","BB"]`,
				ClobTokenIdsRaw: `["g2-a","g2-b"]`, GameStartTimeRaw: start, Active: g2Active},
		},
	}
}

// feedSubscribes snapshots the fake feed's Subscribe call log.
func feedSubscribes(f *fakeFeed) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.subscribes...)
}

func countOccurrences(s []string, want string) int {
	n := 0
	for _, v := range s {
		if v == want {
			n++
		}
	}
	return n
}

// countedEventJSON is a Gamma /events response body for one always-active event.
func countedEventJSON(slug string) string {
	return `[{"id":"e","slug":"` + slug + `","title":"t","markets":[` +
		`{"id":"m","question":"AA vs BB","outcomes":"[\"AA\",\"BB\"]",` +
		`"clobTokenIds":"[\"t1\",\"t2\"]","active":true,` +
		`"gameStartTime":"2026-07-12 10:00:00+00"}]}]`
}

func TestEventRefresh_DefaultInterval(t *testing.T) {
	t.Parallel()
	m := NewLiveTradeManager()
	if m.refreshInterval != 2*time.Minute {
		t.Errorf("default refreshInterval = %v, want 2m", m.refreshInterval)
	}
	if m.refreshPause != eventRefreshDefaultPause {
		t.Errorf("default refreshPause = %v, want %v", m.refreshPause, eventRefreshDefaultPause)
	}
}

// A market that appears (or activates) after subscribe time is registered on
// the next cycle — and only its assets, exactly once (the #55 fix).
func TestEventRefresh_RegistersNewMarketDelta(t *testing.T) {
	t.Parallel()
	m, w, feed := newSnipeWiredManager(t)
	m.refreshPause = 0
	ctx := context.Background()

	const slug = "series-evt"
	m.resolver.cacheEvent(slug, seriesEvent(slug, false)) // g2 not active yet
	// Subscribed in the runtime view; assets not yet registered (the refresh
	// does the first registration here, as a boot re-register otherwise would).
	m.subscriptions.SubscribeTelegram(7, slug, false)

	// Cycle 1: baseline (g1 only).
	events, newEvents, failed := m.refreshCycle(ctx)
	if events != 1 || failed != 0 {
		t.Fatalf("cycle1 events=%d failed=%d, want 1/0", events, failed)
	}
	if newEvents != 1 {
		t.Errorf("cycle1 new=%d, want 1 (baseline registered)", newEvents)
	}
	for _, tok := range []string{"g1-a", "g1-b"} {
		if !w.isWatched(tok) {
			t.Errorf("baseline token %s not watched after cycle1", tok)
		}
	}
	if got := feedSubscribes(feed); len(got) != 2 {
		t.Fatalf("feed subscribes after cycle1 = %v, want 2 (g1 tokens)", got)
	}

	// A new game market appears on Gamma mid-series.
	m.resolver.cacheEvent(slug, seriesEvent(slug, true))

	// Cycle 2: exactly the delta (g2), never re-subscribing g1.
	_, newEvents, _ = m.refreshCycle(ctx)
	if newEvents != 1 {
		t.Errorf("cycle2 new=%d, want 1", newEvents)
	}
	for _, tok := range []string{"g2-a", "g2-b"} {
		if !w.isWatched(tok) {
			t.Errorf("new token %s not watched after cycle2", tok)
		}
	}
	subs := feedSubscribes(feed)
	if len(subs) != 4 {
		t.Fatalf("feed subscribes after cycle2 = %v, want 4 (g1+g2, each once)", subs)
	}
	if countOccurrences(subs, "g2-a") != 1 || countOccurrences(subs, "g2-b") != 1 {
		t.Errorf("a new token was subscribed more than once: %v", subs)
	}
}

// The refcount-leak proof: two cycles over an unchanged event produce ZERO
// additional feed.Subscribe calls. The price feed's Subscribe is ref-counted,
// so a non-delta refresh would leak a subscription every cycle forever.
func TestEventRefresh_UnchangedEventNoNewFeedSubscribe(t *testing.T) {
	t.Parallel()
	m, _, feed := newSnipeWiredManager(t)
	m.refreshPause = 0
	ctx := context.Background()

	// snipeWiringEvent (cached by the harness) has one ML market: ml-blg/ml-hle.
	m.subscriptions.SubscribeTelegram(7, pinnedFeedEventSlug, false)

	m.refreshCycle(ctx) // cycle 1: baseline
	base := feedSubscribes(feed)
	if len(base) != 2 {
		t.Fatalf("feed subscribes after cycle1 = %v, want 2", base)
	}

	_, newEvents, _ := m.refreshCycle(ctx) // cycle 2: unchanged
	if newEvents != 0 {
		t.Errorf("cycle2 new=%d, want 0 (nothing changed)", newEvents)
	}
	if after := feedSubscribes(feed); len(after) != len(base) {
		t.Errorf("feed.Subscribe called again on unchanged refresh: before=%v after=%v", base, after)
	}
}

// A resolve error on one event never touches the store or the registry, and the
// sibling event still refreshes (ADR 0008: errors log-and-skip, never expire).
func TestEventRefresh_ResolveErrorSkipsAndKeeps(t *testing.T) {
	t.Parallel()
	m, w, _ := newSnipeWiredManager(t)
	store := newFakeWatchStore()
	m.SetLiveWatchStore(store)
	m.refreshPause = 0

	// Gamma 404s every network lookup; the harness-cached slug still resolves.
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, _ *http.Request) {
		rw.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()
	m.resolver.gammaAPIURL = srv.URL

	const badSlug = "closed-or-missing"
	m.subscriptions.SubscribeTelegram(7, badSlug, false)             // uncached → 404
	m.subscriptions.SubscribeTelegram(8, pinnedFeedEventSlug, false) // cached → resolves

	events, _, failed := m.refreshCycle(context.Background())
	if events != 2 || failed != 1 {
		t.Fatalf("events=%d failed=%d, want 2/1", events, failed)
	}
	if !w.isWatched("ml-blg") || !w.isWatched("ml-hle") {
		t.Error("resolvable event not registered after a sibling resolve error")
	}
	if !m.subscriptions.HasTelegramSubscribers(badSlug) {
		t.Error("failed event's watch was dropped — refresh must never unsubscribe")
	}
	if store.saves != 0 || store.deletes != 0 || store.deleteAlls != 0 {
		t.Errorf("refresh mutated the store: saves=%d deletes=%d deleteAlls=%d",
			store.saves, store.deletes, store.deleteAlls)
	}
}

// The loop stops when its context is cancelled.
func TestEventRefresh_LoopStopsOnCtxCancel(t *testing.T) {
	t.Parallel()
	m, _, _ := newSnipeWiredManager(t)
	m.refreshInterval = 10 * time.Millisecond
	m.refreshPause = 0
	m.resolver.cacheTTL = 0 // force a Gamma resolve every cycle → observable ticks

	var hits int64
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/events") {
			atomic.AddInt64(&hits, 1)
			rw.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(rw, countedEventJSON("loop-evt"))
			return
		}
		rw.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()
	m.resolver.gammaAPIURL = srv.URL
	m.subscriptions.SubscribeTelegram(7, "loop-evt", false)

	ctx, cancel := context.WithCancel(context.Background())
	go m.runEventRefresh(ctx)

	waitFor(t, func() bool { return atomic.LoadInt64(&hits) >= 2 }) // loop is live
	cancel()

	time.Sleep(30 * time.Millisecond) // drain an in-flight cycle
	before := atomic.LoadInt64(&hits)
	time.Sleep(60 * time.Millisecond) // several would-be intervals
	if got := atomic.LoadInt64(&hits); got != before {
		t.Errorf("resolves after cancel = %d, want frozen at %d", got, before)
	}
}

// The interval field drives the loop: overridden to a few ms, the baseline is
// registered almost immediately. At the production default (2m) this would time
// out — proving the override takes effect.
func TestEventRefresh_IntervalOverrideDrivesLoop(t *testing.T) {
	t.Parallel()
	m, w, feed := newSnipeWiredManager(t)
	m.refreshInterval = 10 * time.Millisecond
	m.refreshPause = 0
	m.subscriptions.SubscribeTelegram(7, pinnedFeedEventSlug, false)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go m.runEventRefresh(ctx)

	waitFor(t, func() bool { return w.isWatched("ml-blg") && w.isWatched("ml-hle") })
	if got := feedSubscribes(feed); len(got) < 2 {
		t.Errorf("feed subscribes after loop start = %v, want >=2", got)
	}
}
