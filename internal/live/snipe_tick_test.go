package live

import (
	"sync"
	"testing"
	"time"
)

// tickHarness is snipeHarness with a fast tick interval, set before Start the
// way production wiring sets the default.
func tickHarness(interval time.Duration) (*SnipeWatcher, *fakeFeed, *fakeSnipeRecipients, *fakeSnipeNotifier, *fakeClock) {
	w, feed, rec, notif, clock := snipeHarness()
	w.tickInterval = interval
	return w, feed, rec, notif, clock
}

func TestSnipeWatcher_DefaultTickInterval(t *testing.T) {
	t.Parallel()
	w := NewSnipeWatcher(newFakeFeed(), newFakeSnipeRecipients(), &fakeSnipeNotifier{})
	if w.tickInterval != 20*time.Second {
		t.Errorf("default tickInterval = %v, want 20s", w.tickInterval)
	}
}

// TestSnipeWatcher_TickAlertsWithoutAnyFeedUpdate replays the 2026-08-01 FaZe
// production miss (arm #81): seeded Session High 0.695, in-play, prints
// through the crash zone — and the WS delivering zero events for the token
// (mid-session resubscribe rejected, issue #42). With evaluation driven only
// by OnUpdate the watcher was blind; the tick must fire the alert on its own.
func TestSnipeWatcher_TickAlertsWithoutAnyFeedUpdate(t *testing.T) {
	t.Parallel()
	w, feed, rec, notif, _ := tickHarness(10 * time.Millisecond)
	rec.eventSubs["evt"] = []int64{101}
	w.Start()
	defer w.Stop()

	w.WatchEventMarkets("evt", []SnipeMarket{startedMarket("T1")})
	w.seedSessionHigh("T1", 0.695)
	feed.setBid("T1", 0.13)
	feed.setAsk("T1", 0.145)
	// Deliberately no feed.emit: the OnUpdate callback never runs.

	waitFor(t, func() bool { return notif.count() == 1 })
	got := notif.alertAt(0)
	if got.chatID != 101 || got.tokenID != "T1" || got.high != 0.695 || got.ask != 0.145 {
		t.Errorf("alert = %+v, want chat 101 T1 high=0.695 ask=0.145", got)
	}

	// Several more ticks with the ask still crashed: the episode latch holds.
	time.Sleep(50 * time.Millisecond)
	if got := notif.count(); got != 1 {
		t.Errorf("alerts after further ticks = %d, want exactly 1", got)
	}
}

func TestSnipeWatcher_TickAndWSRaceAlertOnce(t *testing.T) {
	t.Parallel()
	// The tick loop itself stays quiet (1h interval); the race is driven by
	// hand so tick-path and WS-path evaluations of the same crash overlap.
	w, feed, rec, notif, _ := tickHarness(time.Hour)
	rec.eventSubs["evt"] = []int64{101}
	w.Start()
	defer w.Stop()

	w.WatchEventMarkets("evt", []SnipeMarket{startedMarket("T1")})
	w.seedSessionHigh("T1", 0.55)
	feed.setBid("T1", 0.10)
	feed.setAsk("T1", 0.17)

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			w.tickEvaluateAll() // tick-driven evaluation
		}()
		go func() {
			defer wg.Done()
			feed.emit("T1") // WS-driven evaluation
		}()
	}
	wg.Wait()

	waitFor(t, func() bool { return notif.count() >= 1 })
	time.Sleep(50 * time.Millisecond) // let every spawned evaluation land
	if got := notif.count(); got != 1 {
		t.Errorf("alerts under tick+WS race = %d, want exactly 1", got)
	}
}

func TestSnipeWatcher_TickStopsOnStop(t *testing.T) {
	t.Parallel()
	w, feed, rec, _, _ := tickHarness(10 * time.Millisecond)
	rec.eventSubs["evt"] = []int64{101}
	w.Start()
	w.WatchEventMarkets("evt", []SnipeMarket{startedMarket("T1")})

	waitFor(t, func() bool { return feed.bidCallsFor("T1") >= 2 }) // loop is live
	w.Stop()

	time.Sleep(20 * time.Millisecond) // drain evaluations already in flight
	before := feed.bidCallsFor("T1")
	time.Sleep(50 * time.Millisecond) // several would-be tick periods
	if got := feed.bidCallsFor("T1"); got != before {
		t.Errorf("feed reads after Stop = %d, want frozen at %d", got, before)
	}
}

func TestSnipeWatcher_TickWithNoWatchedTokensIsNoOp(t *testing.T) {
	t.Parallel()
	w, feed, _, notif, _ := snipeHarness()
	w.tickEvaluateAll()
	if got := feed.bidCallCount(); got != 0 {
		t.Errorf("feed bid reads = %d, want 0 with nothing watched", got)
	}
	if got := notif.count(); got != 0 {
		t.Errorf("alerts = %d, want 0 with nothing watched", got)
	}
}

func TestSnipeWatcher_TickSkipsReleasedTokens(t *testing.T) {
	t.Parallel()
	w, feed, rec, notif, _ := snipeHarness()
	rec.eventSubs["evt"] = []int64{101}
	w.WatchEventMarkets("evt", []SnipeMarket{startedMarket("T1")})
	w.WatchHeld(404, startedMarket("T2"), time.Hour)
	w.seedSessionHigh("T1", 0.60)
	w.seedSessionHigh("T2", 0.60)
	feed.setBid("T1", 0.10)
	feed.setAsk("T1", 0.15)
	feed.setBid("T2", 0.10)
	feed.setAsk("T2", 0.15)

	w.UnwatchEventMarkets("evt") // releases T1; T2 stays held

	w.tickEvaluateAll()
	waitFor(t, func() bool { return notif.count() == 1 })
	if got := notif.alertAt(0); got.tokenID != "T2" || got.chatID != 404 {
		t.Errorf("alert = %+v, want held token T2 to chat 404", got)
	}
	if got := feed.bidCallsFor("T1"); got != 0 {
		t.Errorf("released token T1 evaluated %d time(s) by the tick, want 0", got)
	}
}
