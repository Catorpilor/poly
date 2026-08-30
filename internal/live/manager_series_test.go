package live

import (
	"testing"
	"time"
)

// RegisterHeldBuy (issue #94): buying into one market of an event also
// registers the event's other WINNER-class markets (game/map winners and the
// series ML) as held watches, so the series' next game never crashes to
// recipients=0. Props and closed markets stay out; grouping carries the event
// slug for renewal.
func TestRegisterHeldBuy_SeriesWatch(t *testing.T) {
	t.Parallel()
	m, w, _ := newSnipeWiredManager(t)
	clock := newFakeClock()
	w.now = clock.now

	ev := snipeWiringEvent()
	ev.Markets = append(ev.Markets,
		MarketInfo{ID: "prop", Slug: "prop", Question: "Total Kills Over/Under 50.5 in Game 1?",
			OutcomesRaw: `["Over","Under"]`, ClobTokenIdsRaw: `["p-over","p-under"]`,
			GameStartTimeRaw: "2026-07-12 10:00:00+00", Active: true},
		MarketInfo{ID: "g1", Slug: "g1", Question: "BLG vs. HLE - Game 1 Winner",
			OutcomesRaw: `["BLG","HLE"]`, ClobTokenIdsRaw: `["g1-blg","g1-hle"]`,
			GameStartTimeRaw: "2026-07-12 10:00:00+00", Active: true, Closed: true},
	)

	m.RegisterHeldBuy(7, pinnedFeedEventSlug, "ml-blg", ev)

	// The bought market AND the open Game 3 Winner register for the holder.
	for _, tok := range []string{"ml-blg", "ml-hle", "g3-blg", "g3-hle"} {
		st := heldStateOf(t, w, tok)
		if st == nil {
			t.Fatalf("%s not registered — series watch must cover open winner markets", tok)
		}
		if _, ok := st.holders[7]; !ok {
			t.Fatalf("%s: chatID 7 not among holders", tok)
		}
		if st.market.EventSlug != pinnedFeedEventSlug {
			t.Errorf("%s EventSlug = %q, want %q (renewal grouping)", tok, st.market.EventSlug, pinnedFeedEventSlug)
		}
	}
	// Props and closed games never register.
	for _, tok := range []string{"p-over", "p-under", "g1-blg", "g1-hle"} {
		w.mu.Lock()
		_, watched := w.tokens[tok]
		w.mu.Unlock()
		if watched {
			t.Errorf("%s registered — props/closed markets must stay out of the series watch", tok)
		}
	}
	_ = time.Hour
}

// Source class on the web buy path (issue #102): the BOUGHT market's tokens
// (both sides — the sibling watch) register DIRECT and keep full auto-buy; the
// event's OTHER winner-class markets (the continuations the buyer never touched)
// register WALKED and are alert-only. This is the live production path that
// registered the r114/r115 specimen, so the class must be stamped here.
func TestRegisterHeldBuy_ContinuationsAreWalked(t *testing.T) {
	t.Parallel()
	m, w, _ := newSnipeWiredManager(t)

	m.RegisterHeldBuy(7, pinnedFeedEventSlug, "ml-blg", snipeWiringEvent())

	// Bought market (series moneyline) — both sides direct.
	for _, tok := range []string{"ml-blg", "ml-hle"} {
		if w.WalkedOnlyHolder(7, tok) {
			t.Errorf("%s classified walked-only — the bought market must stay DIRECT (full auto-buy)", tok)
		}
	}
	// Continuation (Game 3 Winner) — both sides walked-only (alert-only).
	for _, tok := range []string{"g3-blg", "g3-hle"} {
		if !w.WalkedOnlyHolder(7, tok) {
			t.Errorf("%s not walked-only — an untouched continuation must be alert-only (issue #102)", tok)
		}
	}
}

// The upgrade rule composes on the web path: buying game 1 registers the series'
// later games as walked; a LATER buy of one of those games re-registers ITS
// market direct (upgrade), and the re-walk of the now-held earlier market never
// downgrades it.
func TestRegisterHeldBuy_LaterBuyUpgradesContinuation(t *testing.T) {
	t.Parallel()
	m, w, _ := newSnipeWiredManager(t)

	// Buy the series moneyline: ml direct, g3 walked.
	m.RegisterHeldBuy(7, pinnedFeedEventSlug, "ml-blg", snipeWiringEvent())
	if !w.WalkedOnlyHolder(7, "g3-blg") {
		t.Fatal("precondition: g3-blg should be walked after the moneyline buy")
	}

	// Now buy Game 3 itself: g3's market becomes the bought market ⇒ direct
	// (upgrade); ml is now an event-mate continuation whose re-walk must NOT
	// downgrade the existing direct entry.
	m.RegisterHeldBuy(7, pinnedFeedEventSlug, "g3-blg", snipeWiringEvent())
	if w.WalkedOnlyHolder(7, "g3-blg") {
		t.Error("g3-blg still walked-only — a direct buy of its market must UPGRADE it")
	}
	if w.WalkedOnlyHolder(7, "ml-blg") {
		t.Error("ml-blg downgraded to walked-only — a re-walk must never clobber a direct entry")
	}
}
