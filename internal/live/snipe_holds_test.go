package live

import (
	"bytes"
	"log"
	"strings"
	"testing"
	"time"
)

// heldMarket returns a SnipeMarket for tokenID inside marketID/eventSlug — the
// shape the bought-side mark tests need (siblings share a market, continuations
// share only the event).
func heldMarket(tokenID, marketID, eventSlug string) SnipeMarket {
	m := startedMarket(tokenID)
	m.MarketID = marketID
	m.EventSlug = eventSlug
	return m
}

// Bought-side mark (issue #111): Holds answers "does this chat ACTUALLY own
// this token", not "is this chat watching it". Only a registration that names
// the bought/held token sets it; sibling and series-walk registrations never do,
// and the mark is upgrade-only — a later plain WatchHeld must not clear it.
func TestSnipeWatcherHolds_RegistrationMatrix(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		register func(w *SnipeWatcher, m SnipeMarket)
		want     bool
	}{
		{
			name:     "bought registration marks the token",
			register: func(w *SnipeWatcher, m SnipeMarket) { w.WatchBought(7, m, time.Hour) },
			want:     true,
		},
		{
			name:     "sibling-only direct watch never marks",
			register: func(w *SnipeWatcher, m SnipeMarket) { w.WatchHeld(7, m, time.Hour) },
			want:     false,
		},
		{
			name:     "series walk never marks",
			register: func(w *SnipeWatcher, m SnipeMarket) { w.WatchWalked(7, m, time.Hour) },
			want:     false,
		},
		{
			name: "plain WatchHeld after a bought mark keeps it (upgrade-only)",
			register: func(w *SnipeWatcher, m SnipeMarket) {
				w.WatchBought(7, m, time.Hour)
				w.WatchHeld(7, m, time.Hour)
			},
			want: true,
		},
		{
			name: "re-walk after a bought mark never clears it",
			register: func(w *SnipeWatcher, m SnipeMarket) {
				w.WatchBought(7, m, time.Hour)
				w.WatchWalked(7, m, time.Hour)
			},
			want: true,
		},
		{
			name: "bought after a walked entry marks and upgrades to direct",
			register: func(w *SnipeWatcher, m SnipeMarket) {
				w.WatchWalked(7, m, time.Hour)
				w.WatchBought(7, m, time.Hour)
			},
			want: true,
		},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			w, _, _, _, _ := snipeHarness()
			tt.register(w, startedMarket("T1"))
			if got := w.Holds(7, "T1"); got != tt.want {
				t.Errorf("Holds = %v, want %v", got, tt.want)
			}
			if tt.want && w.WalkedOnlyHolder(7, "T1") {
				t.Error("a bought-side mark must also be a DIRECT entry (never walked-only)")
			}
		})
	}
}

// An unknown token, another chat, and an empty token ID are all "not held" —
// the gate fails toward today's buy behavior.
func TestSnipeWatcherHolds_UnknownIsFalse(t *testing.T) {
	t.Parallel()
	w, _, _, _, _ := snipeHarness()
	w.WatchBought(7, startedMarket("T1"), time.Hour)

	for _, tc := range []struct {
		name    string
		chatID  int64
		tokenID string
	}{
		{"unwatched token", 7, "nope"},
		{"empty token", 7, ""},
		{"another chat", 8, "T1"},
	} {
		if w.Holds(tc.chatID, tc.tokenID) {
			t.Errorf("%s: Holds = true, want false", tc.name)
		}
	}
}

// The mark shares the holder entry's lifecycle: it lapses with the TTL and dies
// with the janitor sweep — no restart survival, no stale gate (issue #111).
func TestSnipeWatcherHolds_ExpiryAndSweep(t *testing.T) {
	t.Parallel()
	w, _, _, _, clock := snipeHarness()
	w.WatchBought(7, startedMarket("T1"), time.Hour)
	if !w.Holds(7, "T1") {
		t.Fatal("Holds = false right after WatchBought")
	}

	clock.advance(time.Hour + time.Second)
	if w.Holds(7, "T1") {
		t.Error("expired holder entry still reports Holds")
	}
	w.sweepExpired()
	if w.Holds(7, "T1") {
		t.Error("swept holder entry still reports Holds")
	}
}

// RenewHeldMarket's anchor is a real position, so it marks the ANCHOR only. The
// group renewals it fans out to (the market sibling, the series continuation)
// are watches, not holdings, and must stay unmarked (issue #111).
func TestRenewHeldMarket_MarksAnchorOnly(t *testing.T) {
	t.Parallel()
	w, _, _, _, _ := snipeHarness()
	w.WatchHeld(7, heldMarket("anchor", "m1", "ev"), time.Hour)
	w.WatchHeld(7, heldMarket("sibling", "m1", "ev"), time.Hour)
	w.WatchWalked(7, heldMarket("game3", "m2", "ev"), time.Hour)

	if !w.RenewHeldMarket(7, "anchor", 2*time.Hour, true) {
		t.Fatal("RenewHeldMarket = false for a watched token")
	}

	if !w.Holds(7, "anchor") {
		t.Error("renewed anchor must carry the bought-side mark — it IS a held position")
	}
	for _, tok := range []string{"sibling", "game3"} {
		if w.Holds(7, tok) {
			t.Errorf("group renewal marked %s — only the anchor is a real holding", tok)
		}
	}
	if !w.WalkedOnlyHolder(7, "game3") {
		t.Error("group renewal must not change a continuation's walked class")
	}
}

// The mark carries its OWN expiry (issue #111): a group renewal keeps a
// SIBLING's entry alive for the series, but must never extend the sibling's
// bought-side mark — sell the flip side and its mark has to lapse on schedule
// even while the still-held anchor renews hourly.
func TestSnipeWatcherHolds_GroupRenewalDoesNotExtendMark(t *testing.T) {
	t.Parallel()
	w, _, _, _, clock := snipeHarness()
	w.WatchBought(7, heldMarket("anchor", "m1", "ev"), time.Hour)
	w.WatchBought(7, heldMarket("sold", "m1", "ev"), time.Hour)

	clock.advance(30 * time.Minute)
	if !w.RenewHeldMarket(7, "anchor", time.Hour, true) {
		t.Fatal("RenewHeldMarket = false for a watched token")
	}
	clock.advance(31 * time.Minute) // past the sold side's original mark TTL

	if !w.Holds(7, "anchor") {
		t.Error("anchor mark lapsed despite its own renewal")
	}
	if w.Holds(7, "sold") {
		t.Error("group renewal extended a sibling's mark — a sold side would gate forever")
	}
}

// An EXPIRED mark is never resurrected by a plain registration: only a fresh
// WatchBought (or an anchor renewal) may re-mark, since only those observe a
// real position.
func TestSnipeWatcherHolds_ExpiredMarkNotRevived(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		register func(w *SnipeWatcher, m SnipeMarket)
		want     bool
	}{
		{"WatchHeld after expiry", func(w *SnipeWatcher, m SnipeMarket) { w.WatchHeld(7, m, time.Hour) }, false},
		{"WatchWalked after expiry", func(w *SnipeWatcher, m SnipeMarket) { w.WatchWalked(7, m, time.Hour) }, false},
		{"WatchBought after expiry re-marks", func(w *SnipeWatcher, m SnipeMarket) { w.WatchBought(7, m, time.Hour) }, true},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			w, _, _, _, clock := snipeHarness()
			m := startedMarket("T1")
			w.WatchBought(7, m, time.Hour)
			clock.advance(time.Hour + time.Second)

			tt.register(w, m)

			if got := w.Holds(7, "T1"); got != tt.want {
				t.Errorf("Holds after %s = %v, want %v", tt.name, got, tt.want)
			}
		})
	}
}

// The mark's debug line is the only production evidence that a registration
// path stamped a holding (2026-08-01 lesson: silent-success paths). It fires
// once per false→true transition — not per registration — and names its source.
// Not parallel: log output is global.
func TestSnipeWatcherHolds_MarkLogsOncePerTransition(t *testing.T) {
	var buf bytes.Buffer
	prev := log.Writer()
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(prev) })
	// Token IDs are logged truncated to 12 bytes, so keep the probes short and
	// unique — parallel tests share the logger.
	marks := func(src string) int {
		n := 0
		for _, line := range strings.Split(buf.String(), "\n") {
			if strings.Contains(line, "bought-side mark") && strings.Contains(line, "logmark") && strings.Contains(line, "src="+src) {
				n++
			}
		}
		return n
	}

	w, _, _, _, clock := snipeHarness()
	w.WatchBought(7, heldMarket("logmark-a", "lm", "lm-ev"), time.Hour)
	w.WatchBought(7, heldMarket("logmark-a", "lm", "lm-ev"), time.Hour) // still marked ⇒ silent
	if got := marks(snipeMarkSrcWatch); got != 1 {
		t.Errorf("src=watch lines after two WatchBought calls = %d, want 1", got)
	}

	// An anchor renewal on an as-yet-unmarked entry is a transition of its own.
	w.WatchHeld(7, heldMarket("logmark-b", "lm2", "lm-ev"), time.Hour)
	w.RenewHeldMarket(7, "logmark-b", time.Hour, true)
	if got := marks(snipeMarkSrcRenew); got != 1 {
		t.Errorf("src=renew lines = %d, want 1", got)
	}

	// Once the mark has lapsed, re-marking is a new transition and logs again.
	clock.advance(2 * time.Hour)
	w.WatchBought(7, heldMarket("logmark-a", "lm", "lm-ev"), time.Hour)
	if got := marks(snipeMarkSrcWatch); got != 2 {
		t.Errorf("src=watch lines after re-mark = %d, want 2", got)
	}
}

// A position the Data API still lists at ZERO shares (it keeps returning sold
// rows) renews the watch group but is NOT a holding: held=false must neither
// set nor extend the mark, however many refreshes run (issue #111).
func TestRenewHeldMarket_ZeroShareAnchorNeverMarks(t *testing.T) {
	t.Parallel()
	t.Run("never sets the mark", func(t *testing.T) {
		t.Parallel()
		w, _, _, _, clock := snipeHarness()
		w.WatchHeld(7, heldMarket("sold", "m1", "ev"), time.Hour)
		for i := 0; i < 20; i++ {
			clock.advance(30 * time.Minute)
			if !w.RenewHeldMarket(7, "sold", time.Hour, false) {
				t.Fatal("RenewHeldMarket = false for a watched token")
			}
		}
		if w.Holds(7, "sold") {
			t.Error("zero-share anchor marked as held — a sold token would gate forever")
		}
	})

	t.Run("never extends a lapsing mark", func(t *testing.T) {
		t.Parallel()
		w, _, _, _, clock := snipeHarness()
		w.WatchBought(7, heldMarket("sold", "m1", "ev"), time.Hour)
		// The position is sold moments later; every refresh thereafter renews the
		// watch with held=false, so the mark must still lapse on its own TTL.
		for i := 0; i < 20; i++ {
			clock.advance(5 * time.Minute)
			w.RenewHeldMarket(7, "sold", time.Hour, false)
		}
		if w.Holds(7, "sold") {
			t.Error("held=false renewals extended the mark past its own TTL")
		}
	})
}

// The mark can never outlive its holder entry: a shorter plain registration
// shortens the entry, and Holds must respect that (the entry, not the mark, is
// what the janitor sweeps).
func TestSnipeWatcherHolds_EntryExpiryBoundsMark(t *testing.T) {
	t.Parallel()
	w, _, _, _, clock := snipeHarness()
	m := startedMarket("T1")
	w.WatchBought(7, m, 6*time.Hour)
	w.WatchHeld(7, m, time.Hour) // entry now expires in 1h, mark would live 6h

	clock.advance(2 * time.Hour)

	if w.Holds(7, "T1") {
		t.Error("Holds true on an EXPIRED holder entry — the mark must not outlive it")
	}
}
