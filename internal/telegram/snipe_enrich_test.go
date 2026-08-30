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

// enrichGammaStub models the PRODUCTION shapes that broke the v0.22.2 walk
// (issue #99): the market handed to the registration seams was fetched via the
// path form GET /markets/{id}, which OMITS events[]. The revived walk must
// recover the event slug from the list form GET /markets?id={id}, which DOES
// carry events[] for in-play sports markets. This stub serves BOTH the list-form
// enrichment fetch and the downstream /events?slug= walk fetch, counting each.
func enrichGammaStub(t *testing.T, idFetches, walkFetches *int32) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/markets" && r.URL.Query().Get("id") == "m3":
			atomic.AddInt32(idFetches, 1)
			fmt.Fprint(w, `[{"id":"m3","question":"A vs B - Map 3 Winner","outcomes":"[\"A\",\"B\"]","clobTokenIds":"[\"m3a\",\"m3b\"]","events":[{"slug":"cs2-a-b-2026"}]}]`)
		case r.URL.Path == "/events" && r.URL.Query().Get("slug") == "cs2-a-b-2026":
			atomic.AddInt32(walkFetches, 1)
			fmt.Fprint(w, `[{"slug":"cs2-a-b-2026","markets":[
				{"id":"m4","question":"A vs B - Map 4 Winner","outcomes":"[\"A\",\"B\"]","clobTokenIds":"[\"m4a\",\"m4b\"]","gameStartTime":"2026-08-23 12:15:00+00","active":true,"closed":false}
			]}]`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// noEventsGammaClient is a hermetic Gamma client whose list form carries no
// events — enrichment fails open, so a registration test never touches the
// network or fires a walk. For tests whose focus is direct registration, not
// the series walk.
func noEventsGammaClient(t *testing.T) *polymarket.MarketClient {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `[]`)
	}))
	t.Cleanup(srv.Close)
	return polymarket.NewMarketClientWithURL(srv.URL)
}

// pathFormBoughtMarket is the market as the manual buy handlers' path-form
// GetMarketByID returns it (issue #99): every field EXCEPT events[] — no events
// key at all. Handing this to the walk on unmodified code registers nothing.
func pathFormBoughtMarket() *polymarket.GammaMarket {
	return &polymarket.GammaMarket{
		ID:              "m3",
		Question:        "A vs B - Map 3 Winner",
		OutcomesRaw:     `["A","B"]`,
		ClobTokenIdsRaw: `["m3a","m3b"]`,
		// Production shape: an in-play sports market carries gameStartTime on the
		// path form (only events[] is omitted). The walk needs the game start to
		// pass the in-play gate.
		GameStartTimeRaw: "2026-08-23 12:15:00+00",
		// Events deliberately absent — the path form omits it.
	}
}

// nonSportsMarket is a single-market position: no parent event AND no
// gameStartTime. It can never pass the in-play gate, so it can never alert and
// needs no series walk — the enrichment seams must skip it entirely.
func nonSportsMarket() *polymarket.GammaMarket {
	return &polymarket.GammaMarket{
		ID:              "solo-1",
		Question:        "Will X happen by year end?",
		OutcomesRaw:     `["Yes","No"]`,
		ClobTokenIdsRaw: `["ya","yb"]`,
		// no GameStartTimeRaw, no Events — a non-series market.
	}
}

// TestNonSportsMarketSkipsEnrichment: a market with no gameStartTime cannot go
// in-play, so it needs no walk. The enrichment seams must skip it — NO list-form
// fetch (and therefore no bail line) — so a /positions sweep of N single-market
// positions costs 0 enrichment fetches instead of 2N. RED before the guard: the
// seams fetch and bail on every such market.
func TestNonSportsMarketSkipsEnrichment(t *testing.T) {
	t.Parallel()
	var idFetches int32
	gamma := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/markets" && r.URL.Query().Get("id") != "" {
			atomic.AddInt32(&idFetches, 1)
		}
		fmt.Fprint(w, `[]`)
	}))
	t.Cleanup(gamma.Close)

	watch := &fakeSnipeWatch{}
	b := &Bot{snipeWatcher: watch, snipeMarkets: polymarket.NewMarketClientWithURL(gamma.URL)}

	// Both walk-trigger seams: the bought-token path and the arm-path direct call.
	b.snipeRegisterBoughtToken(7, nonSportsMarket(), 0)
	b.snipeWatchEventMates(7, nonSportsMarket(), time.Hour)

	// Give any detached walk goroutine a beat to (not) fetch. No fetch ⇒ no bail.
	if eventuallyTrue(t, 200*time.Millisecond, func() bool { return atomic.LoadInt32(&idFetches) > 0 }) {
		t.Fatalf("enrichment fetches = %d, want 0 — a market with no gameStartTime can never alert and needs no walk", atomic.LoadInt32(&idFetches))
	}
	// Own-market registration is unaffected by the skip.
	held := watch.heldTokens()
	if len(held) != 2 {
		t.Fatalf("direct held = %v, want the 2 own tokens (registration unaffected by the enrichment skip)", held)
	}
}

func eventuallyTrue(t *testing.T, timeout time.Duration, cond func() bool) bool {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(5 * time.Millisecond)
	}
	return cond()
}

// TestSnipeBoughtTokenPathFormRevivesWalk is the "Jim never got the G3 alert"
// regression: a manual buy fetches the market path-form (no events[]), so the
// series walk that should register the event's OTHER open winner markets as
// alert-only continuations (#102) silently no-ops. Post-fix, registering the
// bought token enriches the market from the list form and the walk fires.
//
// RED (unmodified code): the walk registers nothing — no continuation token.
func TestSnipeBoughtTokenPathFormRevivesWalk(t *testing.T) {
	t.Parallel()
	var idFetches, walkFetches int32
	gamma := enrichGammaStub(t, &idFetches, &walkFetches)

	watch := &fakeSnipeWatch{}
	b := &Bot{snipeWatcher: watch, snipeMarkets: polymarket.NewMarketClientWithURL(gamma.URL)}

	b.snipeRegisterBoughtToken(7, pathFormBoughtMarket(), 0)

	// The own market's tokens register DIRECT inline; the walk (go, detached)
	// registers the Map 4 continuation WALKED — alert-only (#102).
	if !eventuallyTrue(t, 2*time.Second, func() bool { return len(watch.walkedTokens()) == 2 }) {
		t.Fatalf("walk registered %v, want the Map 4 continuations [m4a m4b] — the bought-token path stayed dead", watch.walkedTokens())
	}

	// Composed with #102: the continuations are registered walked-only (alert-
	// only) — via WatchWalked, never WatchHeld. The held (direct) set is the
	// bought market's own tokens only; the flip-side/next-game mates are walked.
	wantWalked := map[string]bool{"m4a": true, "m4b": true}
	for _, tok := range watch.walkedTokens() {
		if !wantWalked[tok] {
			t.Errorf("unexpected walked token %q", tok)
		}
	}
	wantHeld := map[string]bool{"m3a": true, "m3b": true}
	held := watch.heldTokens()
	if len(held) != 2 {
		t.Fatalf("direct held = %v, want exactly the bought market's own tokens %v", held, wantHeld)
	}
	for _, tok := range held {
		if !wantHeld[tok] {
			t.Errorf("continuation %q was registered DIRECT — must be walked-only (#102)", tok)
		}
		if wantWalked[tok] {
			t.Errorf("continuation %q registered both direct and walked", tok)
		}
	}
	if got := atomic.LoadInt32(&idFetches); got != 1 {
		t.Errorf("list-form enrichment fetches = %d, want exactly 1 (idempotent across seams)", got)
	}
}

// TestSnipeWatchEventMatesPathFormRevivesWalk drives the arm path's direct call
// into the catch-all seam: snipeWatchEventMates on a path-form market (no
// events[]) must enrich and walk. RED on unmodified code: no continuation.
func TestSnipeWatchEventMatesPathFormRevivesWalk(t *testing.T) {
	t.Parallel()
	var idFetches, walkFetches int32
	gamma := enrichGammaStub(t, &idFetches, &walkFetches)

	watch := &fakeSnipeWatch{}
	b := &Bot{snipeWatcher: watch, snipeMarkets: polymarket.NewMarketClientWithURL(gamma.URL)}

	// The arm path calls snipeWatchEventMates directly and synchronously (~:1400).
	b.snipeWatchEventMates(7, pathFormBoughtMarket(), time.Hour)

	got := watch.walkedTokens()
	if len(got) != 2 {
		t.Fatalf("walked = %v, want the Map 4 continuations [m4a m4b] — the arm path stayed dead", got)
	}
	if got := atomic.LoadInt32(&idFetches); got != 1 {
		t.Errorf("list-form enrichment fetches = %d, want 1", got)
	}
	if got := atomic.LoadInt32(&walkFetches); got != 1 {
		t.Errorf("walk fetches = %d, want 1", got)
	}
}

// TestSnipeBoughtTokenEnrichmentFailOpen: when the list form is empty (a
// lifecycle-edge market), enrichment fails open — the bought market itself still
// registers DIRECT, and no continuation is walked. The own-market watch must
// never depend on the series enrichment succeeding.
func TestSnipeBoughtTokenEnrichmentFailOpen(t *testing.T) {
	t.Parallel()
	gamma := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/markets" && r.URL.Query().Get("id") == "m3" {
			fmt.Fprint(w, `[]`) // list form empty at the lifecycle edge
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(gamma.Close)

	watch := &fakeSnipeWatch{}
	b := &Bot{snipeWatcher: watch, snipeMarkets: polymarket.NewMarketClientWithURL(gamma.URL)}

	b.snipeRegisterBoughtToken(7, pathFormBoughtMarket(), 0)

	// Own market registered direct regardless of enrichment outcome.
	held := watch.heldTokens()
	wantHeld := map[string]bool{"m3a": true, "m3b": true}
	if len(held) != 2 {
		t.Fatalf("direct held = %v, want the bought market's own tokens %v even when enrichment is empty", held, wantHeld)
	}
	for _, tok := range held {
		if !wantHeld[tok] {
			t.Errorf("unexpected direct held token %q", tok)
		}
	}
	// No parent event recovered ⇒ no walk. Give any detached walk goroutine a
	// beat; it must still register nothing.
	if eventuallyTrue(t, 200*time.Millisecond, func() bool { return len(watch.walkedTokens()) > 0 }) {
		t.Fatalf("walked = %v, want none (enrichment empty ⇒ no series to walk)", watch.walkedTokens())
	}
}
