package telegram

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Catorpilor/poly/internal/polymarket"
)

// snipeWatchHeldMarket (issue #94): registering a held market also registers
// the parent event's open WINNER-class markets — the series' next game must
// not crash to recipients=0. Props, closed games, and the held market itself
// are excluded from the walk.
func TestSnipeWatchHeldMarketRegistersEventMates(t *testing.T) {
	t.Parallel()
	gamma := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/events" && r.URL.Query().Get("slug") == "cs2-a-b-2026" {
			fmt.Fprint(w, `[{"slug":"cs2-a-b-2026","markets":[
				{"id":"m3","question":"A vs B - Map 3 Winner","outcomes":"[\"A\",\"B\"]","clobTokenIds":"[\"m3a\",\"m3b\"]","gameStartTime":"2026-08-23 12:15:00+00","active":true,"closed":false},
				{"id":"m4","question":"A vs B - Map 4 Winner","outcomes":"[\"A\",\"B\"]","clobTokenIds":"[\"m4a\",\"m4b\"]","gameStartTime":"2026-08-23 12:15:00+00","active":true,"closed":false},
				{"id":"m1","question":"A vs B - Map 1 Winner","outcomes":"[\"A\",\"B\"]","clobTokenIds":"[\"m1a\",\"m1b\"]","gameStartTime":"2026-08-23 12:15:00+00","active":true,"closed":true},
				{"id":"pr","question":"Total Kills Over/Under 50.5 in Game 1?","outcomes":"[\"Over\",\"Under\"]","clobTokenIds":"[\"pra\",\"prb\"]","active":true,"closed":false}
			]}]`)
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(gamma.Close)

	watch := &fakeSnipeWatch{}
	b := &Bot{
		snipeWatcher: watch,
		snipeMarkets: polymarket.NewMarketClientWithURL(gamma.URL),
	}
	held := &polymarket.GammaMarket{
		ID: "m3", Question: "A vs B - Map 3 Winner",
		OutcomesRaw:     `["A","B"]`,
		ClobTokenIdsRaw: `["m3a","m3b"]`,
		Events:          []*polymarket.GammaEvent{{Slug: "cs2-a-b-2026"}},
	}

	b.snipeWatchHeldMarket(7, held, time.Hour)

	// The held market's own tokens register DIRECT; the event's open Map 4
	// mates register WALKED (issue #102) — alert-only continuations.
	gotDirect := watch.heldTokens()
	wantDirect := map[string]bool{"m3a": true, "m3b": true}
	if len(gotDirect) != len(wantDirect) {
		t.Fatalf("direct held tokens = %v, want exactly %v (the held market only)", gotDirect, wantDirect)
	}
	for _, tok := range gotDirect {
		if !wantDirect[tok] {
			t.Errorf("unexpected direct held registration %q", tok)
		}
	}
	gotWalked := watch.walkedTokens()
	wantWalked := map[string]bool{"m4a": true, "m4b": true}
	if len(gotWalked) != len(wantWalked) {
		t.Fatalf("walked tokens = %v, want exactly %v (open Map 4; no props, no closed Map 1, no double-register of m3)", gotWalked, wantWalked)
	}
	for _, tok := range gotWalked {
		if !wantWalked[tok] {
			t.Errorf("unexpected walked registration %q", tok)
		}
	}
	// Walked event-mates carry the event slug and a real game start for renewal
	// grouping and in-play gating.
	watch.mu.Lock()
	defer watch.mu.Unlock()
	for _, m := range watch.walked {
		if m.EventSlug != "cs2-a-b-2026" {
			t.Errorf("%s EventSlug = %q, want cs2-a-b-2026", m.TokenID, m.EventSlug)
		}
		if m.GameStart.IsZero() {
			t.Errorf("%s GameStart zero — an inert watch never alerts", m.TokenID)
		}
	}
}

// A market with no parent event (plain single-market position) skips the walk
// and an event-fetch failure is fail-open: the held market itself stays
// registered either way.
func TestSnipeWatchHeldMarketEventWalkFailOpen(t *testing.T) {
	t.Parallel()
	gamma := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	t.Cleanup(gamma.Close)

	watch := &fakeSnipeWatch{}
	b := &Bot{snipeWatcher: watch, snipeMarkets: polymarket.NewMarketClientWithURL(gamma.URL)}

	noEvent := &polymarket.GammaMarket{ID: "solo", Question: "Will X win?",
		OutcomesRaw: `["Yes","No"]`, ClobTokenIdsRaw: `["ya","yb"]`}
	b.snipeWatchHeldMarket(7, noEvent, time.Hour)

	withEvent := &polymarket.GammaMarket{ID: "m3", Question: "A vs B - Map 3 Winner",
		OutcomesRaw: `["A","B"]`, ClobTokenIdsRaw: `["m3a","m3b"]`,
		Events: []*polymarket.GammaEvent{{Slug: "cs2-a-b-2026"}}}
	b.snipeWatchHeldMarket(7, withEvent, time.Hour)

	got := watch.heldTokens()
	if len(got) != 4 {
		t.Fatalf("held tokens = %v, want the 4 own-market tokens (walk skipped/failed open)", got)
	}
}

// seriesGammaStub serves /events?slug=cs2-a-b-2026 with one open winner mate
// and counts event fetches.
func seriesGammaStub(t *testing.T, fetches *int32) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/events" && r.URL.Query().Get("slug") == "cs2-a-b-2026" {
			atomic.AddInt32(fetches, 1)
			fmt.Fprint(w, `[{"slug":"cs2-a-b-2026","markets":[
				{"id":"m4","question":"A vs B - Map 4 Winner","outcomes":"[\"A\",\"B\"]","clobTokenIds":"[\"m4a\",\"m4b\"]","gameStartTime":"2026-08-23 12:15:00+00","active":true,"closed":false}
			]}]`)
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// The renewal short-circuit must still walk the series (issue #94 review F1):
// a position whose token renews without a metadata fetch (armed-only or
// already-held states) uses EventSlugOf to trigger the deduped walk, so mates
// that were never registered (or lapsed) come back.
func TestRegisterSnipeHeldRenewPathWalksSeries(t *testing.T) {
	t.Parallel()
	var fetches int32
	gamma := seriesGammaStub(t, &fetches)

	watch := &recordingHeldWatch{renewResult: true, eventSlug: "cs2-a-b-2026"}
	b := &Bot{snipeWatcher: watch, snipeMarkets: polymarket.NewMarketClientWithURL(gamma.URL)}

	b.registerSnipeHeld(7, []*polymarket.Position{{TokenID: "m3a", MarketID: "m3"}})

	walked := watch.walkedTokens()
	if len(walked) != 2 || walked[0] != "m4a" || walked[1] != "m4b" {
		t.Fatalf("walked after renew-path walk = %v, want [m4a m4b] (continuations are alert-only, issue #102)", walked)
	}
	if got := atomic.LoadInt32(&fetches); got != 1 {
		t.Errorf("event fetches = %d, want 1", got)
	}

	// Second refresh within the walk interval: renewed again, no new fetch.
	b.registerSnipeHeld(7, []*polymarket.Position{{TokenID: "m3a", MarketID: "m3"}})
	if got := atomic.LoadInt32(&fetches); got != 1 {
		t.Errorf("event fetches after second refresh = %d, want 1 (deduped)", got)
	}
}

// The snipe auto-arm path walks the series (issue #94 review F1): the auto-buy
// flow was the recipients=0 incident path, so arming must register the buyer
// as a held watcher of the event's other winner markets.
func TestSnipeAutoArmWalksSeries(t *testing.T) {
	t.Parallel()
	var fetches int32
	gamma := seriesGammaStub(t, &fetches)

	watch := &fakeSnipeWatch{}
	repo := &recordingArmRepo{}
	b, _, _ := newFillConfirmBot(t, nil)
	b.snipeWatcher = watch
	b.snipeMarkets = polymarket.NewMarketClientWithURL(gamma.URL)
	b.sltpArmRepo = repo

	res := snipeBuyResult{
		outcome: snipeBuyFilled, ask: 0.20, orderID: "ord-1",
		market: &polymarket.GammaMarket{
			ID: "m3", Question: "A vs B - Map 3 Winner",
			OutcomesRaw: `["A","B"]`, ClobTokenIdsRaw: `["m3a","m3b"]`,
			Events: []*polymarket.GammaEvent{{Slug: "cs2-a-b-2026"}},
		},
		idx: 0, filledSize: 49.8, filledPrice: 0.20,
	}
	b.snipeAutoArmTPOnly(7, "m3a", "A vs B - Map 3 Winner", "A", res, 10)

	if calls := repo.armedCalls(); len(calls) != 1 {
		t.Fatalf("ArmTPOnly calls = %d, want 1", len(calls))
	}
	got := watch.walkedTokens()
	if len(got) != 2 {
		t.Fatalf("walked after arm-path walk = %v, want the Map 4 mates (alert-only, issue #102)", got)
	}
}

// A failed event fetch must un-stamp the walk dedup so the next trigger
// retries — a stamped failure would hold the recipients=0 window open for the
// whole walk interval (round-2 review F1).
func TestSeriesWalkFailureUnstampsDedup(t *testing.T) {
	t.Parallel()
	var fetches int32
	fail := true
	gamma := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&fetches, 1)
		if fail {
			http.Error(w, "boom", http.StatusInternalServerError)
			return
		}
		fmt.Fprint(w, `[{"slug":"cs2-a-b-2026","markets":[
			{"id":"m4","question":"A vs B - Map 4 Winner","outcomes":"[\"A\",\"B\"]","clobTokenIds":"[\"m4a\",\"m4b\"]","gameStartTime":"2026-08-23 12:15:00+00","active":true,"closed":false}
		]}]`)
	}))
	t.Cleanup(gamma.Close)

	watch := &fakeSnipeWatch{}
	b := &Bot{snipeWatcher: watch, snipeMarkets: polymarket.NewMarketClientWithURL(gamma.URL)}

	b.snipeWalkEventSlug(7, "cs2-a-b-2026", "", time.Hour)
	if got := watch.walkedTokens(); len(got) != 0 {
		t.Fatalf("walked after failed walk = %v, want none", got)
	}

	fail = false
	b.snipeWalkEventSlug(7, "cs2-a-b-2026", "", time.Hour)
	if got := watch.walkedTokens(); len(got) != 2 {
		t.Fatalf("walked after retried walk = %v, want the mates (failure must un-stamp)", got)
	}
	if got := atomic.LoadInt32(&fetches); got != 2 {
		t.Errorf("fetches = %d, want 2 (fail then retry)", got)
	}
}
