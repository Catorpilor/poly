package live

import (
	"testing"
	"time"
)

// pendingBoughtLen reads the pending-mark map size under the mutex — a white-box
// probe for the leak-bound tests (same package).
func pendingBoughtLen(w *SnipeWatcher) int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return len(w.pendingBought)
}

// TestSnipeMarkBoughtLazyAppliesWhenWatchedLater is the core of the #84 fix: a
// MarkBought for a token that is NOT yet watched (boot restore runs before the
// held/event registration that names it) must be parked and applied when the
// token is first watched — so the token stays silenced instead of re-alerting.
func TestSnipeMarkBoughtLazyAppliesWhenWatchedLater(t *testing.T) {
	t.Parallel()
	w, _, rec, notif, _ := snipeHarness()
	rec.eventSubs["evt"] = []int64{101}

	// Mark before the token is watched: parked as pending, nothing to latch yet.
	w.MarkBought("T1")
	if got := pendingBoughtLen(w); got != 1 {
		t.Fatalf("pendingBought after early MarkBought = %d, want 1", got)
	}

	// The token becomes watched later this session; the pending mark applies.
	w.WatchEventMarkets("evt", []SnipeMarket{startedMarket("T1")})
	if got := pendingBoughtLen(w); got != 0 {
		t.Errorf("pendingBought after watch = %d, want 0 (applied)", got)
	}

	// A crash that WOULD fire now finds the bought latch set — no alert.
	w.evaluate("T1", 0.45, 0.17)
	if got := notif.count(); got != 0 {
		t.Errorf("alerts = %d, want 0 — restored bought mark must suppress the re-alert", got)
	}
}

// TestSnipeMarkBoughtOnWatchedTokenUnchanged: the pre-#84 fast path is intact —
// marking an already-watched token latches it directly and parks nothing.
func TestSnipeMarkBoughtOnWatchedTokenUnchanged(t *testing.T) {
	t.Parallel()
	w, _, rec, notif, _ := snipeHarness()
	rec.eventSubs["evt"] = []int64{101}
	w.WatchEventMarkets("evt", []SnipeMarket{startedMarket("T1")})

	w.MarkBought("T1")
	if got := pendingBoughtLen(w); got != 0 {
		t.Errorf("pendingBought after marking a watched token = %d, want 0", got)
	}
	w.evaluate("T1", 0.45, 0.17)
	if got := notif.count(); got != 0 {
		t.Errorf("alerts = %d, want 0", got)
	}
}

// TestSnipeMarkBoughtPendingExpiresBeforeWatch: a pending mark older than
// snipePendingBoughtTTL is NOT applied when the token is finally watched — the
// buy is stale enough that a fresh episode may legitimately re-alert — and the
// entry is dropped, not leaked.
func TestSnipeMarkBoughtPendingExpiresBeforeWatch(t *testing.T) {
	t.Parallel()
	w, _, rec, notif, clock := snipeHarness()
	rec.eventSubs["evt"] = []int64{101}

	w.MarkBought("T1")
	clock.advance(snipePendingBoughtTTL + time.Minute)

	w.WatchEventMarkets("evt", []SnipeMarket{startedMarket("T1")})
	if got := pendingBoughtLen(w); got != 0 {
		t.Errorf("pendingBought after watch = %d, want 0 (expired entry dropped)", got)
	}
	// Expired mark was not applied, so the crash alerts normally.
	w.evaluate("T1", 0.45, 0.17)
	if got := notif.count(); got != 1 {
		t.Errorf("alerts = %d, want 1 — an expired pending mark must not suppress", got)
	}
}

// TestSnipeMarkBoughtPendingPrunedByJanitor: a mark for a token that never gets
// watched cannot leak forever — the janitor sweep prunes it once it ages past
// the TTL.
func TestSnipeMarkBoughtPendingPrunedByJanitor(t *testing.T) {
	t.Parallel()
	w, _, _, _, clock := snipeHarness()

	w.MarkBought("Tnever")
	if got := pendingBoughtLen(w); got != 1 {
		t.Fatalf("pendingBought = %d, want 1", got)
	}
	// Not yet expired: a sweep keeps it.
	w.sweepExpired()
	if got := pendingBoughtLen(w); got != 1 {
		t.Fatalf("pendingBought after early sweep = %d, want 1 (not yet expired)", got)
	}
	// Past the TTL: the next sweep drops it.
	clock.advance(snipePendingBoughtTTL + time.Minute)
	w.sweepExpired()
	if got := pendingBoughtLen(w); got != 0 {
		t.Errorf("pendingBought after expiry sweep = %d, want 0 (pruned)", got)
	}
}
