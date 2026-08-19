package live

import (
	"testing"
	"time"
)

// heldStateOf reads a token's snipe state directly (white-box, same package).
func heldStateOf(t *testing.T, w *SnipeWatcher, tokenID string) *snipeTokenState {
	t.Helper()
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.tokens[tokenID]
}

// RegisterHeldBuy is the buy-side seam of the Held Watch invariant (issue #64):
// a successful BUY makes its buyer a holder, so a comeback-snipe crash on the
// token they now hold reaches them even without an open positions view.
func TestRegisterHeldBuy(t *testing.T) {
	t.Parallel()

	wantStart := time.Date(2026, 7, 12, 10, 0, 0, 0, time.UTC)

	t.Run("nil watcher is a no-op", func(t *testing.T) {
		t.Parallel()
		m := &LiveTradeManager{} // no snipeWatcher wired
		// Must not panic.
		m.RegisterHeldBuy(7, pinnedFeedEventSlug, "ml-blg", snipeWiringEvent())
	})

	t.Run("moneyline token registers the buyer as holder", func(t *testing.T) {
		t.Parallel()
		m, w, _ := newSnipeWiredManager(t)
		clock := newFakeClock()
		w.now = clock.now

		m.RegisterHeldBuy(7, pinnedFeedEventSlug, "ml-blg", snipeWiringEvent())

		st := heldStateOf(t, w, "ml-blg")
		if st == nil {
			t.Fatal("ml-blg not registered after RegisterHeldBuy")
		}
		if st.market.MarketID != "ml" || st.market.Outcome != "BLG" || !st.market.GameStart.Equal(wantStart) {
			t.Errorf("held market = %+v, want MarketID=ml Outcome=BLG GameStart=%v", st.market, wantStart)
		}
		exp, ok := st.holders[7]
		if !ok {
			t.Fatal("chatID 7 not among holders")
		}
		if want := clock.now().Add(SnipeHeldTTL); !exp.Equal(want) {
			t.Errorf("holder expiry = %v, want now+SnipeHeldTTL %v", exp, want)
		}
	})

	// Sibling watch (issue #78): buying one side of a market makes the buyer a
	// holder of BOTH sides. The flip side is where a comeback-snipe crash — and
	// the boxed case-3 flip buy — actually lands, so it must enter the watcher
	// too, not just the bought token.
	t.Run("bought token's sibling in the same market also registers", func(t *testing.T) {
		t.Parallel()
		m, w, feed := newSnipeWiredManager(t)
		clock := newFakeClock()
		w.now = clock.now

		m.RegisterHeldBuy(7, pinnedFeedEventSlug, "ml-blg", snipeWiringEvent())

		wantExp := clock.now().Add(SnipeHeldTTL)
		for _, tok := range []string{"ml-blg", "ml-hle"} {
			st := heldStateOf(t, w, tok)
			if st == nil {
				t.Fatalf("%s not registered — sibling watch must register both sides", tok)
			}
			exp, ok := st.holders[7]
			if !ok {
				t.Fatalf("%s: chatID 7 not among holders", tok)
			}
			if !exp.Equal(wantExp) {
				t.Errorf("%s holder expiry = %v, want now+SnipeHeldTTL %v", tok, exp, wantExp)
			}
		}
		// Both sides subscribed to the feed (they are not otherwise on it).
		feed.mu.Lock()
		subs := append([]string(nil), feed.subscribes...)
		feed.mu.Unlock()
		if len(subs) != 2 {
			t.Fatalf("feed subscribes = %v, want both ml-blg and ml-hle", subs)
		}
		// The other market's tokens stay untouched.
		if st := heldStateOf(t, w, "g3-blg"); st != nil {
			t.Errorf("g3-blg registered from a buy in the ml market: %+v", st)
		}
	})

	t.Run("sub-market token registers even though ML resolution excludes it", func(t *testing.T) {
		t.Parallel()
		m, w, _ := newSnipeWiredManager(t)

		// g3-blg lives in the Game 3 sub-market, which eventSnipeMarkets/
		// GetAllMLMarkets never returns — the buy may still target it, and its
		// sibling g3-hle rides along.
		m.RegisterHeldBuy(7, pinnedFeedEventSlug, "g3-blg", snipeWiringEvent())

		st := heldStateOf(t, w, "g3-blg")
		if st == nil {
			t.Fatal("sub-market token g3-blg not registered — RegisterHeldBuy must search all markets")
		}
		if st.market.MarketID != "g3" || st.market.Outcome != "BLG" {
			t.Errorf("held market = %+v, want MarketID=g3 Outcome=BLG", st.market)
		}
		if _, ok := st.holders[7]; !ok {
			t.Error("chatID 7 not among holders")
		}
		sib := heldStateOf(t, w, "g3-hle")
		if sib == nil {
			t.Fatal("sub-market sibling g3-hle not registered")
		}
		if _, ok := sib.holders[7]; !ok {
			t.Error("sibling g3-hle: chatID 7 not among holders")
		}
	})

	t.Run("token not in event does not register", func(t *testing.T) {
		t.Parallel()
		m, w, _ := newSnipeWiredManager(t)

		m.RegisterHeldBuy(7, pinnedFeedEventSlug, "not-a-token", snipeWiringEvent())

		if st := heldStateOf(t, w, "not-a-token"); st != nil {
			t.Errorf("unknown token registered: %+v", st)
		}
		// An unknown token registers nothing at all — not even a stray sibling.
		if st := heldStateOf(t, w, "ml-blg"); st != nil {
			t.Errorf("unknown token buy registered an unrelated token: %+v", st)
		}
	})
}
