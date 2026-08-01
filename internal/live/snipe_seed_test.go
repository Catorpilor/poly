package live

import (
	"context"
	"sync"
	"testing"
	"time"
)

// fakeSeeder is a scriptable SnipeHistorySeeder recording every fetch.
type fakeSeeder struct {
	mu    sync.Mutex
	price float64
	ok    bool
	calls []seedCall
}

type seedCall struct {
	tokenID string
	since   time.Time
}

func (s *fakeSeeder) MaxTradePriceSince(_ context.Context, tokenID string, since time.Time) (float64, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, seedCall{tokenID: tokenID, since: since})
	return s.price, s.ok
}

func (s *fakeSeeder) callCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.calls)
}

func (s *fakeSeeder) call(i int) seedCall {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls[i]
}

// seededHarness is snipeHarness with a fake seeder wired before any
// registration, mirroring the production wiring order.
func seededHarness(price float64, ok bool) (*SnipeWatcher, *fakeSeeder, *fakeSnipeRecipients, *fakeSnipeNotifier, *fakeClock) {
	w, _, rec, notif, clock := snipeHarness()
	seeder := &fakeSeeder{price: price, ok: ok}
	w.SetHistorySeeder(seeder)
	return w, seeder, rec, notif, clock
}

// TestSnipeWatcher_SeedEnablesLateWatchAlert replays the 2026-08-01 Nongshim
// production case (lol-ns-fox1-2026-08-01-game1): the token sat at 0.495
// pre-game and crashed in-play before any watcher registered. Without the
// seed no post-registration observation reaches the competitive bar and the
// comeback can never alert.
func TestSnipeWatcher_SeedEnablesLateWatchAlert(t *testing.T) {
	t.Parallel()
	w, seeder, rec, notif, _ := seededHarness(0.495, true)
	rec.eventSubs["evt"] = []int64{101}

	m := startedMarket("T1")
	w.WatchEventMarkets("evt", []SnipeMarket{m})

	waitFor(t, func() bool { return w.sessionHigh("T1") >= 0.495 })

	// The history window starts 2h before the scheduled game start.
	if got, want := seeder.call(0), (seedCall{tokenID: "T1", since: m.GameStart.Add(-2 * time.Hour)}); got.tokenID != want.tokenID || !got.since.Equal(want.since) {
		t.Errorf("seeder called with %+v, want %+v", got, want)
	}

	w.evaluate("T1", 0.10, 0.20)
	if notif.count() != 1 {
		t.Fatalf("alerts = %d, want 1 (seeded high makes the late watch alert)", notif.count())
	}
}

func TestSnipeWatcher_SeedSinceFallsBackWithoutGameStart(t *testing.T) {
	t.Parallel()
	w, seeder, _, _, clock := seededHarness(0.50, true)

	m := startedMarket("T1")
	m.GameStart = time.Time{}
	w.WatchHeld(404, m, time.Hour)

	waitFor(t, func() bool { return seeder.callCount() == 1 })
	if got, want := seeder.call(0).since, clock.now().Add(-6*time.Hour); !got.Equal(want) {
		t.Errorf("seed since = %v, want now-6h %v", got, want)
	}
}

func TestSnipeWatcher_SeedNeverLowersRatchetedHigh(t *testing.T) {
	t.Parallel()
	w, _, rec, _, _ := snipeHarness()
	rec.eventSubs["evt"] = []int64{101}
	w.WatchEventMarkets("evt", []SnipeMarket{startedMarket("T1")})

	w.evaluate("T1", 0.45, 0.50) // live ratchet ahead of the seed
	w.seedSessionHigh("T1", 0.30)
	if got := w.sessionHigh("T1"); got != 0.45 {
		t.Errorf("sessionHigh after lower seed = %v, want 0.45 (raise only)", got)
	}

	w.seedSessionHigh("T1", 0.60)
	if got := w.sessionHigh("T1"); got != 0.60 {
		t.Errorf("sessionHigh after higher seed = %v, want 0.60", got)
	}

	// Unknown tokens are a no-op, not a resurrection.
	w.seedSessionHigh("UNKNOWN", 0.99)
	if w.isWatched("UNKNOWN") {
		t.Error("seeding an unknown token created state")
	}
}

func TestSnipeWatcher_LateSeedKeepsEpisodeState(t *testing.T) {
	t.Parallel()
	w, _, rec, notif, _ := snipeHarness()
	rec.eventSubs["evt"] = []int64{101}
	w.WatchEventMarkets("evt", []SnipeMarket{startedMarket("T1")})

	w.evaluate("T1", 0.45, 0.50)
	w.evaluate("T1", 0.10, 0.17)
	if notif.count() != 1 {
		t.Fatalf("alerts = %d, want 1 before the late seed", notif.count())
	}
	w.MarkBought("T1")

	// A seed landing after the alert and the buy must not un-latch either.
	w.seedSessionHigh("T1", 0.80)
	w.evaluate("T1", 0.08, 0.16)
	if notif.count() != 1 {
		t.Errorf("alerts after late seed = %d, want 1 (alerted/bought latches intact)", notif.count())
	}
}

func TestSnipeWatcher_SeedAtMostOncePerTokenState(t *testing.T) {
	t.Parallel()
	w, seeder, rec, _, _ := seededHarness(0.50, true)
	rec.eventSubs["evt"] = []int64{101}

	m := startedMarket("T1")
	w.WatchEventMarkets("evt", []SnipeMarket{m})
	waitFor(t, func() bool { return seeder.callCount() == 1 })

	// Every re-registration path reuses the existing state: no second fetch.
	w.WatchEventMarkets("evt", []SnipeMarket{m})
	w.WatchArmed(m)
	w.WatchHeld(404, m, time.Hour)
	if got := seeder.callCount(); got != 1 {
		t.Errorf("seeder calls after re-registrations = %d, want 1", got)
	}
}

func TestSnipeWatcher_SeedFetchFailureLeavesHighUntouched(t *testing.T) {
	t.Parallel()
	w, seeder, rec, notif, _ := seededHarness(0, false)
	rec.eventSubs["evt"] = []int64{101}
	w.WatchEventMarkets("evt", []SnipeMarket{startedMarket("T1")})

	waitFor(t, func() bool { return seeder.callCount() == 1 })
	w.evaluate("T1", 0.10, 0.17)
	if notif.count() != 0 {
		t.Errorf("alerts = %d, want 0 (failed seed must not fire)", notif.count())
	}
	if got := w.sessionHigh("T1"); got != 0.10 {
		t.Errorf("sessionHigh = %v, want the live bid only", got)
	}
}

func TestSnipeWatcher_ConcurrentSeedAndEvaluate(t *testing.T) {
	t.Parallel()
	w, _, rec, _, _ := snipeHarness()
	rec.eventSubs["evt"] = []int64{101}
	w.WatchEventMarkets("evt", []SnipeMarket{startedMarket("T1")})

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			w.seedSessionHigh("T1", 0.50)
		}()
		go func() {
			defer wg.Done()
			w.evaluate("T1", 0.45, 0.60)
		}()
	}
	wg.Wait()

	if got := w.sessionHigh("T1"); got != 0.50 {
		t.Errorf("sessionHigh after concurrent seed+evaluate = %v, want 0.50", got)
	}
}
