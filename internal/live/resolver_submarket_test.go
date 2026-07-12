package live

import "testing"

// GetSubMarkets returns the tradeable non-Moneyline markets of an event:
// active, not closed, and not part of the ML set. Identity is the market
// ID, so whatever GetAllMLMarkets selects (including its fallbacks) is
// excluded regardless of question wording.
func TestGetSubMarkets(t *testing.T) {
	t.Parallel()

	resolver := NewEventSlugResolver()

	event := &EventInfo{
		ID:    "lol-hle1-ly-2026-07-11",
		Title: "HLE vs LY",
		Markets: []MarketInfo{
			{ID: "ml", Question: "HLE vs. LY", Slug: "lol-hle1-ly-2026-07-11", OutcomesRaw: `["HLE","LY"]`, Active: true},
			{ID: "g1", Question: "HLE vs. LY: Game 1 Winner", Slug: "lol-hle1-ly-2026-07-11-game1", OutcomesRaw: `["HLE","LY"]`, Active: true},
			{ID: "g2", Question: "HLE vs. LY: Game 2 Winner", Slug: "lol-hle1-ly-2026-07-11-game2", OutcomesRaw: `["HLE","LY"]`, Active: true},
			{ID: "kills", Question: "Total Kills O/U 26.5", Slug: "lol-hle1-ly-2026-07-11-total-kills", OutcomesRaw: `["Over","Under"]`, Active: true},
			{ID: "closed-g0", Question: "HLE vs. LY: Game 0 Winner", Slug: "lol-hle1-ly-2026-07-11-game0", OutcomesRaw: `["HLE","LY"]`, Active: true, Closed: true},
			{ID: "inactive", Question: "HLE vs. LY: First Blood", Slug: "lol-hle1-ly-2026-07-11-fb", OutcomesRaw: `["HLE","LY"]`, Active: false},
		},
	}

	got := resolver.GetSubMarkets(event)

	var gotIDs []string
	for _, m := range got {
		gotIDs = append(gotIDs, m.ID)
	}
	want := map[string]bool{"g1": true, "g2": true, "kills": true}

	if len(gotIDs) != len(want) {
		t.Fatalf("GetSubMarkets returned %v, want the 3 active non-ML markets", gotIDs)
	}
	for _, id := range gotIDs {
		if !want[id] {
			t.Errorf("GetSubMarkets included %q, which should be excluded (ML, closed, or inactive)", id)
		}
	}
}

func TestGetSubMarketsEmptyWhenOnlyML(t *testing.T) {
	t.Parallel()

	resolver := NewEventSlugResolver()
	event := &EventInfo{
		ID: "nba-atl-ind",
		Markets: []MarketInfo{
			{ID: "ml", Question: "Hawks vs. Pacers", OutcomesRaw: `["Hawks","Pacers"]`, Active: true},
		},
	}
	if got := resolver.GetSubMarkets(event); len(got) != 0 {
		t.Errorf("GetSubMarkets on ML-only event = %d markets, want 0", len(got))
	}
}

// The parsed price accessor mirrors GetOutcomes: parse the raw JSON once,
// cache it, tolerate a malformed field.
func TestMarketInfoGetOutcomePrices(t *testing.T) {
	t.Parallel()

	m := &MarketInfo{OutcomePricesRaw: `["0.55","0.46"]`}
	prices := m.GetOutcomePrices()
	if len(prices) != 2 || prices[0] != "0.55" || prices[1] != "0.46" {
		t.Errorf("GetOutcomePrices() = %v, want [0.55 0.46]", prices)
	}

	bad := &MarketInfo{OutcomePricesRaw: `not json`}
	if got := bad.GetOutcomePrices(); got != nil {
		t.Errorf("GetOutcomePrices() on malformed raw = %v, want nil", got)
	}
}
