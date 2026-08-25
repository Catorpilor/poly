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
