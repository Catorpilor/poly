package live

import (
	"testing"
	"time"
)

// RenewHeldMarket (issue #94): renewing one held token extends every watched
// token of the same EVENT — the series' later games stay watched as long as
// any of the event's positions renew. Other events are untouched; empty
// EventSlug keeps the same-market-only grouping.
func TestRenewHeldMarket_EventGroup(t *testing.T) {
	t.Parallel()
	w, _, _, _, clock := snipeHarness()

	mk := func(tok, marketID, slug string) SnipeMarket {
		m := startedMarket(tok)
		m.MarketID = marketID
		m.EventSlug = slug
		return m
	}
	w.WatchHeld(7, mk("ml-a", "ml", "ev-1"), time.Hour)
	w.WatchHeld(7, mk("ml-b", "ml", "ev-1"), time.Hour)
	w.WatchHeld(7, mk("g3-a", "g3", "ev-1"), time.Hour)
	w.WatchHeld(7, mk("other", "om", "ev-2"), time.Hour)

	clock.advance(30 * time.Minute)
	if !w.RenewHeldMarket(7, "ml-a", time.Hour) {
		t.Fatal("RenewHeldMarket returned false for a watched token")
	}

	wantExp := clock.now().Add(time.Hour)
	for _, tok := range []string{"ml-a", "ml-b", "g3-a"} {
		w.mu.Lock()
		exp := w.tokens[tok].holders[7]
		w.mu.Unlock()
		if !exp.Equal(wantExp) {
			t.Errorf("%s holder expiry = %v, want event-group renewal to %v", tok, exp, wantExp)
		}
	}
	w.mu.Lock()
	otherExp := w.tokens["other"].holders[7]
	w.mu.Unlock()
	if otherExp.Equal(wantExp) {
		t.Errorf("other-event token renewed — event grouping must not cross events")
	}
}

func TestRenewHeldMarket_EmptySlugKeepsMarketGrouping(t *testing.T) {
	t.Parallel()
	w, _, _, _, clock := snipeHarness()

	mk := func(tok, marketID string) SnipeMarket {
		m := startedMarket(tok)
		m.MarketID = marketID
		return m
	}
	w.WatchHeld(7, mk("m1-a", "m1"), time.Hour)
	w.WatchHeld(7, mk("m1-b", "m1"), time.Hour)
	w.WatchHeld(7, mk("m2-a", "m2"), time.Hour)

	clock.advance(30 * time.Minute)
	w.RenewHeldMarket(7, "m1-a", time.Hour)

	wantExp := clock.now().Add(time.Hour)
	w.mu.Lock()
	sib, other := w.tokens["m1-b"].holders[7], w.tokens["m2-a"].holders[7]
	w.mu.Unlock()
	if !sib.Equal(wantExp) {
		t.Errorf("same-market sibling not renewed (issue #78 regression)")
	}
	if other.Equal(wantExp) {
		t.Errorf("unrelated market renewed — empty EventSlug must group by market only")
	}
}

// ensureStateLocked backfills EventSlug on an existing slugless state (issue
// #94 review F2): an event-subscription registration creates tokens without a
// slug; the later held registration's slug must stick or renewal grouping
// silently fails.
func TestEventSlugBackfillOnExistingState(t *testing.T) {
	t.Parallel()
	w, _, _, _, _ := snipeHarness()

	slugless := startedMarket("ml-a")
	slugless.MarketID = "ml"
	w.WatchEventMarkets("ev-sub", []SnipeMarket{slugless})
	if got := w.EventSlugOf("ml-a"); got != "" {
		t.Fatalf("precondition: slug = %q, want empty", got)
	}

	stamped := startedMarket("ml-a")
	stamped.MarketID = "ml"
	stamped.EventSlug = "ev-1"
	w.WatchHeld(7, stamped, time.Hour)

	if got := w.EventSlugOf("ml-a"); got != "ev-1" {
		t.Errorf("EventSlugOf after held registration = %q, want backfilled ev-1", got)
	}
}
