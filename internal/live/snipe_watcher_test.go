package live

import (
	"math"
	"sync"
	"testing"
	"time"
)

// --- fakes ---

type snipeAlertRec struct {
	chatID  int64
	tokenID string
	high    float64
	ask     float64
}

type fakeSnipeNotifier struct {
	mu     sync.Mutex
	alerts []snipeAlertRec
}

func (n *fakeSnipeNotifier) NotifySnipeAlert(chatID int64, market SnipeMarket, sessionHigh, ask float64) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.alerts = append(n.alerts, snipeAlertRec{chatID: chatID, tokenID: market.TokenID, high: sessionHigh, ask: ask})
}

func (n *fakeSnipeNotifier) count() int {
	n.mu.Lock()
	defer n.mu.Unlock()
	return len(n.alerts)
}

func (n *fakeSnipeNotifier) recipients() []int64 {
	n.mu.Lock()
	defer n.mu.Unlock()
	out := make([]int64, 0, len(n.alerts))
	for _, a := range n.alerts {
		out = append(out, a.chatID)
	}
	return out
}

type fakeSnipeRecipients struct {
	mu        sync.Mutex
	eventSubs map[string][]int64
	armOwners map[string][]int64
}

func newFakeSnipeRecipients() *fakeSnipeRecipients {
	return &fakeSnipeRecipients{
		eventSubs: make(map[string][]int64),
		armOwners: make(map[string][]int64),
	}
}

func (r *fakeSnipeRecipients) EventSubscribers(eventSlug string) []int64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]int64(nil), r.eventSubs[eventSlug]...)
}

func (r *fakeSnipeRecipients) ArmOwners(tokenID string) []int64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]int64(nil), r.armOwners[tokenID]...)
}

// snipeHarness builds a watcher with a manual clock, a fake feed, fake
// recipients, and a fake notifier. The janitor is not started (tests drive
// evaluation directly); Start is only used in the feed-driven test.
func snipeHarness() (*SnipeWatcher, *fakeFeed, *fakeSnipeRecipients, *fakeSnipeNotifier, *fakeClock) {
	feed := newFakeFeed()
	rec := newFakeSnipeRecipients()
	notif := &fakeSnipeNotifier{}
	clock := newFakeClock()
	w := NewSnipeWatcher(feed, rec, notif)
	w.now = clock.now
	return w, feed, rec, notif, clock
}

// startedMarket returns a SnipeMarket whose game started an hour before the
// fake clock's origin, i.e. in-play for every harness test.
func startedMarket(tokenID string) SnipeMarket {
	return SnipeMarket{
		TokenID:   tokenID,
		MarketID:  "m-" + tokenID,
		Question:  "Will X win?",
		Outcome:   "X",
		GameStart: time.Unix(1_700_000_000, 0).Add(-time.Hour),
	}
}

func TestSnipeResetAskIsDerivedMidpoint(t *testing.T) {
	t.Parallel()
	want := (SnipeCrashAsk + SnipeCompetitiveBid) / 2
	if got := snipeResetAsk(); got != want {
		t.Errorf("snipeResetAsk() = %v, want derived midpoint %v", got, want)
	}
	if math.Abs(snipeResetAsk()-0.29) > 1e-9 {
		t.Errorf("snipeResetAsk() = %v, want ~0.29", snipeResetAsk())
	}
}

func TestSnipeWatcher_TriggerBoundaries(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		steps [][2]float64 // (bid, ask) applied in order
		want  int
	}{
		{
			name:  "fires exactly at high 0.40 ask 0.18",
			steps: [][2]float64{{0.40, 0.18}},
			want:  1,
		},
		{
			name:  "no fire when high only 0.39",
			steps: [][2]float64{{0.39, 0.17}},
			want:  0,
		},
		{
			name:  "no fire when ask 0.19",
			steps: [][2]float64{{0.45, 0.19}},
			want:  0,
		},
		{
			name:  "no fire without an ask",
			steps: [][2]float64{{0.45, 0}},
			want:  0,
		},
		{
			name:  "high latched earlier, crash later fires",
			steps: [][2]float64{{0.55, 0.60}, {0.10, 0.17}},
			want:  1,
		},
		{
			name:  "flap 0.17 / 0.19 / 0.17 produces one alert",
			steps: [][2]float64{{0.45, 0.50}, {0.10, 0.17}, {0.10, 0.19}, {0.10, 0.17}},
			want:  1,
		},
		{
			name:  "recovery to exactly the reset ask does not re-arm",
			steps: [][2]float64{{0.45, 0.50}, {0.10, 0.17}, {0.10, (SnipeCrashAsk + SnipeCompetitiveBid) / 2}, {0.10, 0.17}},
			want:  1,
		},
		{
			name: "Kudermetova replay: crash, recover 0.34, re-crash fires twice",
			steps: [][2]float64{
				{0.45, 0.50}, {0.10, 0.17}, {0.12, 0.34}, {0.08, 0.16},
			},
			want: 2,
		},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			w, _, rec, notif, _ := snipeHarness()
			rec.eventSubs["evt"] = []int64{101}
			w.WatchEventMarkets("evt", []SnipeMarket{startedMarket("T1")})
			for _, s := range tt.steps {
				w.evaluate("T1", s[0], s[1])
			}
			if got := notif.count(); got != tt.want {
				t.Errorf("alerts = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestSnipeWatcher_MarkBoughtSilencesForMatch(t *testing.T) {
	t.Parallel()
	w, _, rec, notif, _ := snipeHarness()
	rec.eventSubs["evt"] = []int64{101}
	w.WatchEventMarkets("evt", []SnipeMarket{startedMarket("T1")})

	w.evaluate("T1", 0.45, 0.50)
	w.evaluate("T1", 0.10, 0.17)
	if notif.count() != 1 {
		t.Fatalf("expected 1 alert before buy, got %d", notif.count())
	}

	w.MarkBought("T1")

	// Full recovery and a second crash: bought latches for the match.
	w.evaluate("T1", 0.12, 0.34)
	w.evaluate("T1", 0.08, 0.16)
	if notif.count() != 1 {
		t.Errorf("alerts after MarkBought = %d, want 1 (silenced)", notif.count())
	}
}

func TestSnipeWatcher_FreshWatcherHasNoSessionHigh(t *testing.T) {
	t.Parallel()
	// Simulates a restart: a new watcher never saw the pre-crash high, so a
	// crash immediately after start cannot alert.
	w, _, rec, notif, _ := snipeHarness()
	rec.eventSubs["evt"] = []int64{101}
	w.WatchEventMarkets("evt", []SnipeMarket{startedMarket("T1")})

	w.evaluate("T1", 0.10, 0.17)
	if notif.count() != 0 {
		t.Errorf("alerts = %d, want 0 (session high rebuilds from zero)", notif.count())
	}
}

func TestSnipeWatcher_InPlayGate(t *testing.T) {
	t.Parallel()
	t.Run("pre-start game never alerts", func(t *testing.T) {
		t.Parallel()
		w, _, rec, notif, clock := snipeHarness()
		rec.eventSubs["evt"] = []int64{101}
		m := startedMarket("T1")
		m.GameStart = clock.now().Add(time.Hour) // starts in the future
		w.WatchEventMarkets("evt", []SnipeMarket{m})

		w.evaluate("T1", 0.45, 0.50)
		w.evaluate("T1", 0.10, 0.17)
		if notif.count() != 0 {
			t.Fatalf("alerts = %d, want 0 before scheduled start", notif.count())
		}

		// Once the clock passes the start, the same conditions alert.
		clock.advance(time.Hour + time.Minute)
		w.evaluate("T1", 0.10, 0.17)
		if notif.count() != 1 {
			t.Errorf("alerts = %d, want 1 after scheduled start", notif.count())
		}
	})
	t.Run("unknown start time never alerts", func(t *testing.T) {
		t.Parallel()
		w, _, rec, notif, _ := snipeHarness()
		rec.eventSubs["evt"] = []int64{101}
		m := startedMarket("T1")
		m.GameStart = time.Time{}
		w.WatchEventMarkets("evt", []SnipeMarket{m})

		w.evaluate("T1", 0.45, 0.50)
		w.evaluate("T1", 0.10, 0.17)
		if notif.count() != 0 {
			t.Errorf("alerts = %d, want 0 with unknown game start", notif.count())
		}
	})
}

func TestSnipeWatcher_ConcurrentEvaluateAlertsOnce(t *testing.T) {
	t.Parallel()
	w, _, rec, notif, _ := snipeHarness()
	rec.eventSubs["evt"] = []int64{101}
	w.WatchEventMarkets("evt", []SnipeMarket{startedMarket("T1")})
	w.evaluate("T1", 0.45, 0.50) // latch the session high

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			w.evaluate("T1", 0.10, 0.17)
		}()
	}
	wg.Wait()

	if got := notif.count(); got != 1 {
		t.Errorf("alerts under concurrent evaluate = %d, want exactly 1", got)
	}
}

func TestSnipeWatcher_RecipientsUnionDeduped(t *testing.T) {
	t.Parallel()
	w, _, rec, notif, clock := snipeHarness()
	rec.eventSubs["evt"] = []int64{101, 202}
	rec.armOwners["T1"] = []int64{202, 303} // 202 also an event subscriber
	w.WatchEventMarkets("evt", []SnipeMarket{startedMarket("T1")})
	w.WatchHeld(404, startedMarket("T1"), time.Hour)
	_ = clock

	w.evaluate("T1", 0.45, 0.50)
	w.evaluate("T1", 0.10, 0.17)

	got := notif.recipients()
	if len(got) != 4 {
		t.Fatalf("recipients = %v, want 4 unique (101, 202, 303, 404)", got)
	}
	seen := make(map[int64]bool)
	for _, id := range got {
		if seen[id] {
			t.Errorf("recipient %d notified twice", id)
		}
		seen[id] = true
	}
	for _, want := range []int64{101, 202, 303, 404} {
		if !seen[want] {
			t.Errorf("recipient %d missing from %v", want, got)
		}
	}
}

func TestSnipeWatcher_EventWatchSubscribesAndUnwatchReleases(t *testing.T) {
	t.Parallel()
	w, feed, rec, notif, _ := snipeHarness()
	rec.eventSubs["evt"] = []int64{101}
	w.WatchEventMarkets("evt", []SnipeMarket{startedMarket("T1"), startedMarket("T2")})

	feed.mu.Lock()
	subs := append([]string(nil), feed.subscribes...)
	feed.mu.Unlock()
	if len(subs) != 2 {
		t.Fatalf("feed subscribes = %v, want [T1 T2]", subs)
	}

	// Re-watching the same event must not double-subscribe.
	w.WatchEventMarkets("evt", []SnipeMarket{startedMarket("T1"), startedMarket("T2")})
	feed.mu.Lock()
	n := len(feed.subscribes)
	feed.mu.Unlock()
	if n != 2 {
		t.Fatalf("feed subscribes after re-watch = %d, want 2", n)
	}

	w.UnwatchEventMarkets("evt")
	feed.mu.Lock()
	unsubs := append([]string(nil), feed.unsubscribes...)
	feed.mu.Unlock()
	if len(unsubs) != 2 {
		t.Fatalf("feed unsubscribes = %v, want [T1 T2]", unsubs)
	}

	// State dropped: a crash on a released token no longer alerts.
	w.evaluate("T1", 0.45, 0.50)
	w.evaluate("T1", 0.10, 0.17)
	if notif.count() != 0 {
		t.Errorf("alerts on released token = %d, want 0", notif.count())
	}
}

func TestSnipeWatcher_HeldTTLExpires(t *testing.T) {
	t.Parallel()
	w, feed, _, notif, clock := snipeHarness()
	w.WatchHeld(404, startedMarket("T1"), time.Hour)

	w.evaluate("T1", 0.45, 0.50)
	clock.advance(2 * time.Hour) // holder registration expires

	w.evaluate("T1", 0.10, 0.17)
	if notif.count() != 0 {
		t.Errorf("alerts after held TTL expiry = %d, want 0", notif.count())
	}
	feed.mu.Lock()
	unsubs := len(feed.unsubscribes)
	feed.mu.Unlock()
	if unsubs != 1 {
		t.Errorf("feed unsubscribes = %d, want 1 (released on expiry)", unsubs)
	}
}

func TestSnipeWatcher_RenewHeld(t *testing.T) {
	t.Parallel()
	w, _, _, notif, clock := snipeHarness()
	if w.RenewHeld(404, "T1", time.Hour) {
		t.Fatal("RenewHeld on unknown token = true, want false")
	}
	w.WatchHeld(404, startedMarket("T1"), time.Hour)
	clock.advance(30 * time.Minute)
	if !w.RenewHeld(404, "T1", time.Hour) {
		t.Fatal("RenewHeld on watched token = false, want true")
	}
	clock.advance(45 * time.Minute) // original TTL passed, renewed one has not

	w.evaluate("T1", 0.45, 0.50)
	w.evaluate("T1", 0.10, 0.17)
	if notif.count() != 1 {
		t.Errorf("alerts after renew = %d, want 1 (registration still live)", notif.count())
	}
}

func TestSnipeWatcher_ArmedWatchDoesNotOwnFeedSubscription(t *testing.T) {
	t.Parallel()
	// Armed tokens already ride the SL/TP monitor's feed subscription; the
	// watcher must not add its own ref (contract: "already on the feed").
	w, feed, rec, notif, _ := snipeHarness()
	rec.armOwners["T1"] = []int64{55}
	w.WatchArmed(startedMarket("T1"))

	feed.mu.Lock()
	subs := len(feed.subscribes)
	feed.mu.Unlock()
	if subs != 0 {
		t.Fatalf("feed subscribes = %d, want 0 for armed-only watch", subs)
	}

	w.evaluate("T1", 0.45, 0.50)
	w.evaluate("T1", 0.10, 0.17)
	if notif.count() != 1 {
		t.Fatalf("alerts = %d, want 1 for armed watch", notif.count())
	}

	w.UnwatchArmed("T1")
	w.evaluate("T1", 0.08, 0.16)
	if notif.count() != 1 {
		t.Errorf("alerts after UnwatchArmed = %d, want no new alert", notif.count())
	}

	// Releasing an armed-only token must not Unsubscribe either: the feed ref
	// belongs to the SL/TP monitor, and stealing it would starve its arms.
	feed.mu.Lock()
	unsubs := len(feed.unsubscribes)
	feed.mu.Unlock()
	if unsubs != 0 {
		t.Errorf("feed unsubscribes after UnwatchArmed = %d, want 0 (ref owned by the SL/TP monitor)", unsubs)
	}
}

func TestSnipeWatcher_FeedDrivenEvaluation(t *testing.T) {
	t.Parallel()
	w, feed, rec, notif, _ := snipeHarness()
	rec.eventSubs["evt"] = []int64{101}
	w.Start()
	defer w.Stop()
	w.WatchEventMarkets("evt", []SnipeMarket{startedMarket("T1")})

	feed.setBid("T1", 0.45)
	feed.setAsk("T1", 0.50)
	feed.emit("T1")
	waitFor(t, func() bool { return w.sessionHigh("T1") >= SnipeCompetitiveBid })

	feed.setBid("T1", 0.10)
	feed.setAsk("T1", 0.17)
	feed.emit("T1")
	waitFor(t, func() bool { return notif.count() == 1 })

	// A token the watcher does not track is ignored.
	feed.setBid("OTHER", 0.45)
	feed.emit("OTHER")
	if notif.count() != 1 {
		t.Errorf("alerts = %d, want 1 (untracked token ignored)", notif.count())
	}
}
