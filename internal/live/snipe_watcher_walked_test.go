package live

import (
	"testing"
	"time"
)

// holderClassOf returns (walked, present) for chatID's entry on tokenID.
func holderClassOf(t *testing.T, w *SnipeWatcher, tokenID string, chatID int64) (walked, present bool) {
	t.Helper()
	w.mu.Lock()
	defer w.mu.Unlock()
	st := w.tokens[tokenID]
	if st == nil {
		return false, false
	}
	e, ok := st.holders[chatID]
	return e.walked, ok
}

// Series-walked source class (issue #102): a token registered ONLY via the walk
// is alert-only, a directly registered token keeps full auto-buy semantics, and
// the upgrade rule (direct always wins) governs collisions — a direct
// registration overwrites a walked one, and a re-walk never downgrades a direct
// entry.
func TestWatchWalked_UpgradeMatrix(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		register   func(w *SnipeWatcher, m SnipeMarket)
		wantWalked bool // expected WalkedOnlyHolder result
	}{
		{
			name:       "walk only ⇒ alert-only",
			register:   func(w *SnipeWatcher, m SnipeMarket) { w.WatchWalked(7, m, time.Hour) },
			wantWalked: true,
		},
		{
			name:       "direct only ⇒ auto-buy",
			register:   func(w *SnipeWatcher, m SnipeMarket) { w.WatchHeld(7, m, time.Hour) },
			wantWalked: false,
		},
		{
			name: "direct after walk ⇒ upgrade to auto-buy",
			register: func(w *SnipeWatcher, m SnipeMarket) {
				w.WatchWalked(7, m, time.Hour)
				w.WatchHeld(7, m, time.Hour)
			},
			wantWalked: false,
		},
		{
			name: "walk after direct ⇒ never downgrade",
			register: func(w *SnipeWatcher, m SnipeMarket) {
				w.WatchHeld(7, m, time.Hour)
				w.WatchWalked(7, m, time.Hour)
			},
			wantWalked: false,
		},
		{
			name: "walk then walk ⇒ still alert-only",
			register: func(w *SnipeWatcher, m SnipeMarket) {
				w.WatchWalked(7, m, time.Hour)
				w.WatchWalked(7, m, time.Hour)
			},
			wantWalked: true,
		},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			w, _, _, _, _ := snipeHarness()
			m := startedMarket("T1")
			tt.register(w, m)
			if got := w.WalkedOnlyHolder(7, "T1"); got != tt.wantWalked {
				t.Errorf("WalkedOnlyHolder = %v, want %v", got, tt.wantWalked)
			}
		})
	}
}

// WalkedOnlyHolder is false for tokens/chats the watcher never registered — the
// gate fails toward the existing auto-buy behavior on anything it can't classify.
func TestWalkedOnlyHolder_UnknownIsFalse(t *testing.T) {
	t.Parallel()
	w, _, _, _, _ := snipeHarness()
	if w.WalkedOnlyHolder(7, "nope") {
		t.Error("unknown token classified walked-only")
	}
	w.WatchWalked(7, startedMarket("T1"), time.Hour)
	if w.WalkedOnlyHolder(9, "T1") {
		t.Error("a different chat classified walked-only on someone else's walk")
	}
}

// A chat can be walked-only for one token and direct for another at the same
// time — the class is per (chat, token), not per chat (invariant 7).
func TestWatchWalked_PerTokenIndependence(t *testing.T) {
	t.Parallel()
	w, _, _, _, _ := snipeHarness()
	w.WatchWalked(7, startedMarket("walked-tok"), time.Hour)
	w.WatchHeld(7, startedMarket("direct-tok"), time.Hour)

	if !w.WalkedOnlyHolder(7, "walked-tok") {
		t.Error("walked token not classified walked-only")
	}
	if w.WalkedOnlyHolder(7, "direct-tok") {
		t.Error("direct token wrongly classified walked-only")
	}
}

// RenewHeldMarket preserves each entry's class while extending its TTL — a
// walked continuation stays alert-only and a direct hold stays auto-buyable
// across the event-group renewal (invariant 5, both directions).
func TestRenewHeldMarket_PreservesClass(t *testing.T) {
	t.Parallel()
	w, _, _, _, clock := snipeHarness()

	mk := func(tok, marketID, slug string) SnipeMarket {
		m := startedMarket(tok)
		m.MarketID = marketID
		m.EventSlug = slug
		return m
	}
	// Anchor is a direct hold; a sibling of the same event is a walked
	// continuation. Both share event ev-1 so one renewal touches the group.
	w.WatchHeld(7, mk("held", "m-held", "ev-1"), time.Hour)
	w.WatchWalked(7, mk("walked", "m-walked", "ev-1"), time.Hour)

	clock.advance(30 * time.Minute)
	if !w.RenewHeldMarket(7, "held", time.Hour) {
		t.Fatal("RenewHeldMarket returned false for a watched token")
	}

	wantExp := clock.now().Add(time.Hour)
	for _, tc := range []struct {
		tok        string
		wantWalked bool
	}{
		{"held", false},
		{"walked", true},
	} {
		walked, ok := holderClassOf(t, w, tc.tok, 7)
		if !ok {
			t.Fatalf("%s: holder 7 missing after renewal", tc.tok)
		}
		if walked != tc.wantWalked {
			t.Errorf("%s walked = %v after renewal, want %v (class must be preserved)", tc.tok, walked, tc.wantWalked)
		}
		w.mu.Lock()
		exp := w.tokens[tc.tok].holders[7].expiry
		w.mu.Unlock()
		if !exp.Equal(wantExp) {
			t.Errorf("%s expiry = %v, want group renewal to %v", tc.tok, exp, wantExp)
		}
	}
}

// RenewHeldMarket's group loop can ADD this chat to an already-watched sibling
// or continuation it had no entry on (issue #102 over-spend hole): the class of
// a NEWLY-ADDED entry must follow the group — a same-EVENT continuation defaults
// WALKED (alert-only), a same-MARKET sibling of a market the chat actually holds
// defaults DIRECT. Reachable when a /positions renewal runs before the
// rate-limited walk has stamped the continuation walked; without this the chat
// is promoted to full auto-buy on a market it never traded.
func TestRenewHeldMarket_AddedHolderDefaultsByGroup(t *testing.T) {
	t.Parallel()
	w, _, _, _, _ := snipeHarness()

	mk := func(tok, marketID, slug string) SnipeMarket {
		m := startedMarket(tok)
		m.MarketID = marketID
		m.EventSlug = slug
		return m
	}
	// Chat 7 holds ml-a directly. ml-b (same market) and g3-a (same event,
	// different market) are watched by ANOTHER chat, so they sit in w.tokens but
	// chat 7 has no entry on them until the group renewal adds it.
	w.WatchHeld(7, mk("ml-a", "ml", "ev-1"), time.Hour)
	w.WatchHeld(9, mk("ml-b", "ml", "ev-1"), time.Hour)
	w.WatchHeld(9, mk("g3-a", "g3", "ev-1"), time.Hour)

	if !w.RenewHeldMarket(7, "ml-a", time.Hour) {
		t.Fatal("RenewHeldMarket returned false for a watched token")
	}

	if w.WalkedOnlyHolder(7, "ml-b") {
		t.Error("ml-b added as walked — the sibling of a held market must default DIRECT")
	}
	if !w.WalkedOnlyHolder(7, "g3-a") {
		t.Error("g3-a added as direct — an untouched continuation must default WALKED (over-spend hole)")
	}
}

// TTL pruning is class-blind: sourcesLive still evicts an expired holder whether
// it was walked or direct (invariant 6).
func TestSourcesLive_PrunesWalkedAndDirect(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		reg  func(w *SnipeWatcher, m SnipeMarket)
	}{
		{"walked", func(w *SnipeWatcher, m SnipeMarket) { w.WatchWalked(7, m, time.Minute) }},
		{"direct", func(w *SnipeWatcher, m SnipeMarket) { w.WatchHeld(7, m, time.Minute) }},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			w, _, _, _, clock := snipeHarness()
			tc.reg(w, startedMarket("T1"))

			w.mu.Lock()
			st := w.tokens["T1"]
			w.mu.Unlock()
			if st == nil {
				t.Fatal("token not registered")
			}
			if !st.sourcesLive(clock.now()) {
				t.Fatal("holder not live immediately after registration")
			}
			// Past the TTL the holder is pruned and no source remains.
			if st.sourcesLive(clock.now().Add(2 * time.Minute)) {
				t.Error("expired holder not pruned — TTL pruning must be class-blind")
			}
			w.mu.Lock()
			_, present := st.holders[7]
			w.mu.Unlock()
			if present {
				t.Error("expired holder entry still present after sourcesLive prune")
			}
		})
	}
}
