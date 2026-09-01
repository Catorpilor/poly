package live

import (
	"bytes"
	"log"
	"math"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// syncBuffer is a concurrency-safe bytes.Buffer for capturing the global logger
// while parallel siblings are paused (the log-line test is non-parallel).
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// --- fakes ---

type snipeAlertRec struct {
	chatID  int64
	tokenID string
	high    float64
	ask     float64
}

type snipeDeepRec struct {
	chatID     int64
	tokenID    string
	ask        float64
	alertAsk   float64
	sinceAlert time.Duration
}

type snipeBoxedRec struct {
	chatID  int64
	tokenID string
	high    float64
	ask     float64
	tranche int
}

type fakeSnipeNotifier struct {
	mu     sync.Mutex
	alerts []snipeAlertRec
	deeps  []snipeDeepRec
	boxeds []snipeBoxedRec
}

func (n *fakeSnipeNotifier) NotifySnipeAlert(chatID int64, market SnipeMarket, sessionHigh, ask float64) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.alerts = append(n.alerts, snipeAlertRec{chatID: chatID, tokenID: market.TokenID, high: sessionHigh, ask: ask})
}

func (n *fakeSnipeNotifier) NotifySnipeDeepCrash(chatID int64, market SnipeMarket, sessionHigh, ask, alertAsk float64, sinceAlert time.Duration) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.deeps = append(n.deeps, snipeDeepRec{chatID: chatID, tokenID: market.TokenID, ask: ask, alertAsk: alertAsk, sinceAlert: sinceAlert})
}

func (n *fakeSnipeNotifier) NotifySnipeBoxed(chatID int64, market SnipeMarket, sessionHigh, ask float64, tranche int) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.boxeds = append(n.boxeds, snipeBoxedRec{chatID: chatID, tokenID: market.TokenID, high: sessionHigh, ask: ask, tranche: tranche})
}

func (n *fakeSnipeNotifier) deepCount() int {
	n.mu.Lock()
	defer n.mu.Unlock()
	return len(n.deeps)
}

func (n *fakeSnipeNotifier) deepAt(i int) snipeDeepRec {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.deeps[i]
}

func (n *fakeSnipeNotifier) boxedCount() int {
	n.mu.Lock()
	defer n.mu.Unlock()
	return len(n.boxeds)
}

func (n *fakeSnipeNotifier) boxedAt(i int) snipeBoxedRec {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.boxeds[i]
}

func (n *fakeSnipeNotifier) count() int {
	n.mu.Lock()
	defer n.mu.Unlock()
	return len(n.alerts)
}

func (n *fakeSnipeNotifier) alertAt(i int) snipeAlertRec {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.alerts[i]
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
	if math.Abs(snipeResetAsk()-0.30) > 1e-9 {
		t.Errorf("snipeResetAsk() = %v, want ~0.30", snipeResetAsk())
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
			name:  "fires exactly at high 0.40 ask 0.20",
			steps: [][2]float64{{0.40, 0.20}},
			want:  1,
		},
		{
			name:  "no fire when high only 0.39",
			steps: [][2]float64{{0.39, 0.17}},
			want:  0,
		},
		{
			name:  "no fire when ask 0.21",
			steps: [][2]float64{{0.45, 0.21}},
			want:  0,
		},
		{
			name:  "no fire without an ask",
			steps: [][2]float64{{0.45, 0}},
			want:  0,
		},
		{
			name:  "corpse floor: MOUZ replay ask 0.001 never fires",
			steps: [][2]float64{{0.625, 0.60}, {0.01, 0.001}},
			want:  0,
		},
		{
			name:  "corpse floor: ask 0.029 just below the floor",
			steps: [][2]float64{{0.45, 0.029}},
			want:  0,
		},
		{
			name:  "fires exactly at the corpse floor 0.03",
			steps: [][2]float64{{0.45, 0.03}},
			want:  1,
		},
		{
			name:  "high latched earlier, crash later fires",
			steps: [][2]float64{{0.55, 0.60}, {0.10, 0.17}},
			want:  1,
		},
		{
			name:  "flap 0.19 / 0.21 / 0.19 produces one alert",
			steps: [][2]float64{{0.45, 0.50}, {0.10, 0.19}, {0.10, 0.21}, {0.10, 0.19}},
			want:  1,
		},
		{
			name:  "recovery to exactly the reset ask does not re-arm",
			steps: [][2]float64{{0.45, 0.50}, {0.10, 0.17}, {0.10, (SnipeCrashAsk + SnipeCompetitiveBid) / 2}, {0.10, 0.17}},
			want:  1,
		},
		{
			name: "instant recover-and-recrash yields one alert — reset needs a sustained hold (#50)",
			steps: [][2]float64{
				{0.45, 0.50}, {0.10, 0.17}, {0.32, 0.34}, {0.08, 0.16},
			},
			want: 1,
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

// TestSnipeWatcher_PairAlertInstrumentation: each fire stamps lastAlertAt,
// and pairLastAlertLocked surfaces the most recent alert among OTHER tokens
// of the same market — log-only fuel for the September corpse-filter review
// (every winning tap so far came from a game where BOTH sides alerted; the
// corpse losses came from one-sided crashes).
func TestSnipeWatcher_PairAlertInstrumentation(t *testing.T) {
	t.Parallel()
	w, _, rec, notif, clock := snipeHarness()
	rec.eventSubs["evt"] = []int64{101}
	a := startedMarket("TA")
	b := startedMarket("TB")
	b.MarketID = a.MarketID      // two outcomes of one market share the market ID
	other := startedMarket("TC") // unrelated market
	w.WatchEventMarkets("evt", []SnipeMarket{a, b, other})

	// No alerts anywhere yet.
	w.mu.Lock()
	if _, ok := w.pairLastAlertLocked(a.MarketID, "TA"); ok {
		t.Error("pairLastAlertLocked before any alert = ok, want none")
	}
	w.mu.Unlock()

	// Side A crashes and alerts.
	w.evaluate("TA", 0.61, 0.50)
	w.evaluate("TA", 0.10, 0.20)
	if notif.count() != 1 {
		t.Fatalf("alerts = %d, want 1", notif.count())
	}
	firedAt := clock.now()

	clock.advance(7 * time.Minute)

	// Side B's view: the pair (A) alerted 7 minutes ago.
	w.mu.Lock()
	got, ok := w.pairLastAlertLocked(b.MarketID, "TB")
	w.mu.Unlock()
	if !ok || !got.Equal(firedAt) {
		t.Errorf("pairLastAlertLocked for TB = (%v, %v), want (%v, true)", got, ok, firedAt)
	}

	// A's own view excludes itself; B never alerted.
	w.mu.Lock()
	_, ok = w.pairLastAlertLocked(a.MarketID, "TA")
	w.mu.Unlock()
	if ok {
		t.Error("pairLastAlertLocked must exclude the asking token itself")
	}

	// Unrelated market sees nothing.
	w.mu.Lock()
	_, ok = w.pairLastAlertLocked(other.MarketID, "TC")
	w.mu.Unlock()
	if ok {
		t.Error("pairLastAlertLocked crossed market boundaries")
	}
}

// TestSnipeWatcher_ShadowUnderdogDip: log-only instrumentation for the
// September review of the underdog-dip class (Enterprise case, 2026-08-07:
// high 0.365 → 0.095 → won the series — structurally excluded by the 0.40
// competitiveness bar). A shadow alert latches state and logs; it must never
// notify, buy, or touch episode machinery for real alerts.
func TestSnipeWatcher_ShadowUnderdogDip(t *testing.T) {
	t.Parallel()
	type step struct {
		bid, ask float64
		advance  time.Duration
	}
	tests := []struct {
		name        string
		steps       []step
		wantShadows int
		wantAlerts  int
	}{
		{
			name: "Enterprise replay: sub-bar high dips into the shadow band, latches once",
			steps: []step{
				{bid: 0.365, ask: 0.40},
				{bid: 0.10, ask: 0.12}, // shadow #1
				{bid: 0.08, ask: 0.10}, // still latched
			},
			wantShadows: 1, wantAlerts: 0,
		},
		{
			name: "competitive high belongs to the real alert, not the shadow",
			steps: []step{
				{bid: 0.45, ask: 0.50},
				{bid: 0.10, ask: 0.12},
			},
			wantShadows: 0, wantAlerts: 1,
		},
		{
			name:        "high below 0.30 never shadows",
			steps:       []step{{bid: 0.28, ask: 0.30}, {bid: 0.08, ask: 0.10}},
			wantShadows: 0, wantAlerts: 0,
		},
		{
			name:        "ask above the shadow band does not fire",
			steps:       []step{{bid: 0.36, ask: 0.40}, {bid: 0.14, ask: 0.16}},
			wantShadows: 0, wantAlerts: 0,
		},
		{
			name:        "settlement dust below the deep floor does not fire",
			steps:       []step{{bid: 0.36, ask: 0.40}, {bid: 0.003, ask: 0.004}},
			wantShadows: 0, wantAlerts: 0,
		},
		{
			name: "sustained recovery re-arms the shadow episode",
			steps: []step{
				{bid: 0.36, ask: 0.40},
				{bid: 0.10, ask: 0.12}, // shadow #1
				{bid: 0.32, ask: 0.34}, // corroborated recovery (bid >= snipeResetBid, #100)
				{bid: 0.32, ask: 0.34, advance: snipeResetConfirm + time.Second},
				{bid: 0.08, ask: 0.11}, // shadow #2
			},
			wantShadows: 2, wantAlerts: 0,
		},
		{
			// Issue #100 for the shadow tier: pulled asks over a frozen bid must
			// not un-latch a shadow episode any more than a real one.
			name: "#100: pulled asks over a frozen bid never re-arm the shadow",
			steps: []step{
				{bid: 0.36, ask: 0.40},
				{bid: 0.10, ask: 0.12},                                           // shadow #1
				{bid: 0.12, ask: 0.34, advance: snipeResetConfirm + time.Second}, // MMs pull asks
				{bid: 0.12, ask: 0.34, advance: snipeResetConfirm + time.Second}, // bid frozen
				{bid: 0.08, ask: 0.11},                                           // re-dip: still latched
			},
			wantShadows: 1, wantAlerts: 0,
		},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			w, _, rec, notif, clock := snipeHarness()
			rec.eventSubs["evt"] = []int64{101}
			w.WatchEventMarkets("evt", []SnipeMarket{startedMarket("T1")})
			for _, s := range tt.steps {
				clock.advance(s.advance)
				w.evaluate("T1", s.bid, s.ask)
			}
			w.mu.Lock()
			shadows := w.tokens["T1"].shadowCount
			w.mu.Unlock()
			if shadows != tt.wantShadows {
				t.Errorf("shadow fires = %d, want %d", shadows, tt.wantShadows)
			}
			if got := notif.count(); got != tt.wantAlerts {
				t.Errorf("real alerts = %d, want %d", got, tt.wantAlerts)
			}
			if notif.deepCount() != 0 {
				t.Errorf("shadow path must never reach the deep tier or any notifier")
			}
		})
	}
}

// TestSnipeWatcher_ResetRequiresSustainedRecovery covers issue #50: on
// 2026-08-05 a single transient ask tick above the reset level un-latched an
// episode while the first alert's auto-buy was still in flight, and the same
// crash double-alerted 2 seconds apart. A recovery now resets the episode
// only after the ask HOLDS above snipeResetAsk for snipeResetConfirm.
func TestSnipeWatcher_ResetRequiresSustainedRecovery(t *testing.T) {
	t.Parallel()
	type step struct {
		bid, ask float64
		advance  time.Duration // clock advance BEFORE this observation
	}
	tests := []struct {
		name  string
		steps []step
		want  int
	}{
		{
			name: "HANJIN replay: transient tick above reset does not re-arm",
			steps: []step{
				{bid: 0.61, ask: 0.50},
				{bid: 0.10, ask: 0.20},                           // alert #1
				{bid: 0.10, ask: 0.31},                           // one tick above reset
				{bid: 0.10, ask: 0.14, advance: 2 * time.Second}, // re-crash 2s later
			},
			want: 1,
		},
		{
			// Genuine recovery: bids corroborate above snipeResetBid (#100).
			name: "Kudermetova replay: sustained recovery re-arms, re-crash fires twice",
			steps: []step{
				{bid: 0.45, ask: 0.50},
				{bid: 0.10, ask: 0.17}, // alert #1
				{bid: 0.32, ask: 0.34},
				{bid: 0.32, ask: 0.34, advance: snipeResetConfirm + time.Second}, // held above reset
				{bid: 0.08, ask: 0.16}, // alert #2
			},
			want: 2,
		},
		{
			// Corroborating bids throughout; the ONLY thing breaking the streak is
			// the ask dipping below the reset level.
			name: "dip below reset restarts the confirm window",
			steps: []step{
				{bid: 0.45, ask: 0.50},
				{bid: 0.10, ask: 0.17}, // alert #1
				{bid: 0.32, ask: 0.34},
				{bid: 0.32, ask: 0.25, advance: 6 * time.Second}, // streak broken (ask below reset)
				{bid: 0.32, ask: 0.34, advance: 6 * time.Second}, // streak restarts here
				{bid: 0.08, ask: 0.16, advance: 6 * time.Second}, // 6s < confirm: no reset
			},
			want: 1,
		},
		{
			// Corroborating bids throughout; the missing ask alone interrupts.
			name: "missing ask interrupts the confirm window",
			steps: []step{
				{bid: 0.45, ask: 0.50},
				{bid: 0.10, ask: 0.17}, // alert #1
				{bid: 0.32, ask: 0.34},
				{bid: 0.32, ask: 0, advance: 6 * time.Second},    // no data: streak broken
				{bid: 0.32, ask: 0.34, advance: 6 * time.Second}, // restart
				{bid: 0.08, ask: 0.16, advance: 6 * time.Second}, // 6s < confirm: no reset
			},
			want: 1,
		},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			w, _, rec, notif, clock := snipeHarness()
			rec.eventSubs["evt"] = []int64{101}
			w.WatchEventMarkets("evt", []SnipeMarket{startedMarket("T1")})
			for _, s := range tt.steps {
				clock.advance(s.advance)
				w.evaluate("T1", s.bid, s.ask)
			}
			if got := notif.count(); got != tt.want {
				t.Errorf("alerts = %d, want %d", got, tt.want)
			}
		})
	}
}

// TestSnipeWatcher_BidCorroboratedReset covers issue #100: on 2026-08-27 the
// Trabzonspor +1.5 token phantom-reset four times in 26 minutes because the
// episode reset consulted only the ask. A dying book whose market makers pulled
// their asks read ask > 0.30 sustained past the confirm window while the
// trade-proxy bid stayed frozen at 0.120 — un-latching the episode and re-firing
// the full alert cascade (two of the four re-alerts landed AFTER the match was
// final). The reset now also requires a two-sided market above the crash band:
// bid >= snipeResetBid(). A bid that could not itself re-fire the alert band is
// not recovery evidence; a missing bid (feed no-data) fails corroboration and
// holds the latch (fail-latched).
func TestSnipeWatcher_BidCorroboratedReset(t *testing.T) {
	t.Parallel()
	type step struct {
		bid, ask float64
		advance  time.Duration // clock advance BEFORE this observation
	}
	tests := []struct {
		name  string
		steps []step
		want  int
	}{
		{
			name: "Trabzonspor phantom: pulled asks over a frozen 0.120 bid never un-latch",
			steps: []step{
				{bid: 0.55, ask: 0.50},                                           // session high
				{bid: 0.12, ask: 0.18},                                           // alert #1
				{bid: 0.12, ask: 0.34, advance: snipeResetConfirm + time.Second}, // MMs pull asks
				{bid: 0.12, ask: 0.34, advance: snipeResetConfirm + time.Second}, // bid still frozen
				{bid: 0.12, ask: 0.16, advance: time.Second},                     // re-crash: must NOT re-alert
			},
			want: 1,
		},
		{
			name: "no bid data (feed no-data) holds the latch — fail-latched",
			steps: []step{
				{bid: 0.55, ask: 0.50},
				{bid: 0.12, ask: 0.18},                                        // alert #1
				{bid: 0, ask: 0.34, advance: snipeResetConfirm + time.Second}, // no bid
				{bid: 0, ask: 0.34, advance: snipeResetConfirm + time.Second}, // still no bid
				{bid: 0.10, ask: 0.16, advance: time.Second},                  // re-crash: must NOT re-alert
			},
			want: 1,
		},
		{
			name: "boundary bid exactly 0.20 with a sustained above-band ask resets (>= semantics)",
			steps: []step{
				{bid: 0.55, ask: 0.50},
				{bid: 0.12, ask: 0.18},                                           // alert #1
				{bid: 0.20, ask: 0.34},                                           // both sides hold: streak starts
				{bid: 0.20, ask: 0.34, advance: snipeResetConfirm + time.Second}, // held: episode resets
				{bid: 0.08, ask: 0.16},                                           // re-crash: alert #2
			},
			want: 2,
		},
		{
			name: "bid corroboration appears mid-window: a FULL confirm window with both sides is required",
			steps: []step{
				{bid: 0.55, ask: 0.50},
				{bid: 0.10, ask: 0.18},                           // alert #1
				{bid: 0.10, ask: 0.34, advance: 6 * time.Second}, // ask above band, bid fails: no clock
				{bid: 0.10, ask: 0.34, advance: 6 * time.Second}, // 12s of pulled ask, still no clock
				{bid: 0.32, ask: 0.34, advance: 2 * time.Second}, // bid corroborates NOW: clock starts here
				{bid: 0.08, ask: 0.16, advance: 6 * time.Second}, // 6s < confirm since corroboration: no reset
			},
			want: 1,
		},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			w, _, rec, notif, clock := snipeHarness()
			rec.eventSubs["evt"] = []int64{101}
			w.WatchEventMarkets("evt", []SnipeMarket{startedMarket("T1")})
			for _, s := range tt.steps {
				clock.advance(s.advance)
				w.evaluate("T1", s.bid, s.ask)
			}
			if got := notif.count(); got != tt.want {
				t.Errorf("alerts = %d, want %d", got, tt.want)
			}
		})
	}
}

// reentrantSnipeNotifier drives more evaluations from inside a delivery,
// simulating WS/tick evaluations landing while the v2 auto-buy's HTTP
// round-trip is still in flight.
type reentrantSnipeNotifier struct {
	inner  fakeSnipeNotifier
	during func() // run on the first delivery only
	ran    bool
}

func (n *reentrantSnipeNotifier) NotifySnipeAlert(chatID int64, market SnipeMarket, sessionHigh, ask float64) {
	n.inner.NotifySnipeAlert(chatID, market, sessionHigh, ask)
	if !n.ran && n.during != nil {
		n.ran = true
		n.during()
	}
}

func (n *reentrantSnipeNotifier) NotifySnipeDeepCrash(chatID int64, market SnipeMarket, sessionHigh, ask, alertAsk float64, sinceAlert time.Duration) {
	n.inner.NotifySnipeDeepCrash(chatID, market, sessionHigh, ask, alertAsk, sinceAlert)
}

func (n *reentrantSnipeNotifier) NotifySnipeBoxed(chatID int64, market SnipeMarket, sessionHigh, ask float64, tranche int) {
	n.inner.NotifySnipeBoxed(chatID, market, sessionHigh, ask, tranche)
}

// TestSnipeWatcher_DeepCrash covers ADR 0007: the sub-corpse-floor tier fires
// only inside an episode that already produced a genuine in-band alert (the
// only evidence a cheap token is a live panic and not a corpse), once per
// episode, past the bought latch, inside [SnipeDeepFloor, SnipeMinAsk).
func TestSnipeWatcher_DeepCrash(t *testing.T) {
	t.Parallel()

	t.Run("HANJIN replay: alert, buy, keep collapsing — deep fires once past the bought latch", func(t *testing.T) {
		t.Parallel()
		w, _, rec, notif, clock := snipeHarness()
		rec.eventSubs["evt"] = []int64{101}
		w.WatchEventMarkets("evt", []SnipeMarket{startedMarket("T1")})

		w.evaluate("T1", 0.585, 0.50)
		w.evaluate("T1", 0.10, 0.09) // in-band alert
		w.MarkBought("T1")           // the $10 auto-buy landed
		clock.advance(3 * time.Minute)
		w.evaluate("T1", 0.02, 0.025) // sub-floor print → deep fires
		w.evaluate("T1", 0.01, 0.015) // still in zone → already latched

		if notif.count() != 1 || notif.deepCount() != 1 {
			t.Fatalf("alerts=%d deeps=%d, want 1 and 1", notif.count(), notif.deepCount())
		}
		d := notif.deepAt(0)
		if d.ask != 0.025 || d.alertAsk != 0.09 || d.sinceAlert != 3*time.Minute {
			t.Errorf("deep record = %+v, want ask 0.025 alertAsk 0.09 sinceAlert 3m", d)
		}
	})

	t.Run("MOUZ replay: first sighted already cheap — no prior alert, deep stays silent", func(t *testing.T) {
		t.Parallel()
		w, _, rec, notif, _ := snipeHarness()
		rec.eventSubs["evt"] = []int64{101}
		w.WatchEventMarkets("evt", []SnipeMarket{startedMarket("T1")})

		w.evaluate("T1", 0.625, 0.60)
		w.evaluate("T1", 0.01, 0.02) // in deep zone, but never alerted in-band
		w.evaluate("T1", 0.01, 0.008)

		if notif.count() != 0 || notif.deepCount() != 0 {
			t.Errorf("alerts=%d deeps=%d, want 0 and 0", notif.count(), notif.deepCount())
		}
	})

	t.Run("zone bounds: floor is inclusive, corpse dust below it never fires", func(t *testing.T) {
		t.Parallel()
		w, _, rec, notif, _ := snipeHarness()
		rec.eventSubs["evt"] = []int64{101}
		w.WatchEventMarkets("evt", []SnipeMarket{startedMarket("T1")})

		w.evaluate("T1", 0.45, 0.50)
		w.evaluate("T1", 0.10, 0.17)  // in-band alert
		w.evaluate("T1", 0.01, 0.004) // below SnipeDeepFloor: dust, silent
		if notif.deepCount() != 0 {
			t.Fatalf("deep fired below the floor")
		}
		w.evaluate("T1", 0.01, SnipeDeepFloor) // exactly at the floor: fires
		if notif.deepCount() != 1 {
			t.Errorf("deep at exactly SnipeDeepFloor = %d fires, want 1", notif.deepCount())
		}
	})

	t.Run("sustained recovery resets both tiers (unbought token re-fires both)", func(t *testing.T) {
		t.Parallel()
		w, _, rec, notif, clock := snipeHarness()
		rec.eventSubs["evt"] = []int64{101}
		w.WatchEventMarkets("evt", []SnipeMarket{startedMarket("T1")})

		w.evaluate("T1", 0.45, 0.50)
		w.evaluate("T1", 0.10, 0.17) // alert #1 (no buy)
		w.evaluate("T1", 0.01, 0.02) // deep #1
		w.evaluate("T1", 0.32, 0.34) // corroborated recovery (bid >= snipeResetBid, #100)
		clock.advance(snipeResetConfirm + time.Second)
		w.evaluate("T1", 0.32, 0.34) // sustained recovery: episode resets
		w.evaluate("T1", 0.08, 0.16) // alert #2
		w.evaluate("T1", 0.01, 0.01) // deep #2
		if notif.count() != 2 || notif.deepCount() != 2 {
			t.Errorf("alerts=%d deeps=%d, want 2 and 2", notif.count(), notif.deepCount())
		}
	})

	t.Run("no deep fire while a delivery is in flight", func(t *testing.T) {
		t.Parallel()
		feed := newFakeFeed()
		rec := newFakeSnipeRecipients()
		clock := newFakeClock()
		notif := &reentrantSnipeNotifier{}
		w := NewSnipeWatcher(feed, rec, notif)
		w.now = clock.now
		notif.during = func() {
			w.evaluate("T1", 0.01, 0.02) // deep-zone print mid-delivery: must wait
		}
		rec.eventSubs["evt"] = []int64{101}
		w.WatchEventMarkets("evt", []SnipeMarket{startedMarket("T1")})
		w.evaluate("T1", 0.45, 0.50)
		w.evaluate("T1", 0.10, 0.17) // in-band alert; re-entrant deep print inside
		if notif.inner.deepCount() != 0 {
			t.Fatalf("deep fired during in-band delivery")
		}
		w.evaluate("T1", 0.01, 0.02) // delivery done: now it fires
		if notif.inner.deepCount() != 1 {
			t.Errorf("deep after delivery = %d, want 1", notif.inner.deepCount())
		}
	})
}

// TestSnipeWatcher_Boxed covers the boxed ladder (issue #78): after an in-band
// alert, the alerted token is re-offered as two independent $5 rungs — tranche 1
// at the first touch of [SnipeDeepFloor, SnipeBoxedMaxAsk], tranche 2 at the
// deeper [SnipeDeepFloor, SnipeBoxedDeepAsk]. Each rung fires once per episode,
// ignores the bought latch, and resets with the episode.
func TestSnipeWatcher_Boxed(t *testing.T) {
	t.Parallel()

	t.Run("tranche 1 fires once at the 0.10 cross, tranche 2 stays silent above 0.05", func(t *testing.T) {
		t.Parallel()
		w, _, rec, notif, _ := snipeHarness()
		rec.eventSubs["evt"] = []int64{101}
		w.WatchEventMarkets("evt", []SnipeMarket{startedMarket("T1")})

		w.evaluate("T1", 0.585, 0.50)
		w.evaluate("T1", 0.10, 0.17) // in-band alert (boxed NOT this tick)
		if notif.boxedCount() != 0 {
			t.Fatalf("boxed fired in the same evaluate as the in-band alert")
		}
		w.MarkBought("T1")           // someone bought the crashed side
		w.evaluate("T1", 0.02, 0.09) // ≤ 0.10 but > 0.05 → tranche 1 only
		w.evaluate("T1", 0.02, 0.08) // still > 0.05, tranche 1 latched → nothing
		if notif.count() != 1 || notif.boxedCount() != 1 {
			t.Fatalf("alerts=%d boxed=%d, want 1 and 1", notif.count(), notif.boxedCount())
		}
		if bx := notif.boxedAt(0); bx.ask != 0.09 || bx.chatID != 101 || bx.tranche != 1 {
			t.Errorf("boxed record = %+v, want ask 0.09 chat 101 tranche 1", bx)
		}
	})

	t.Run("gradual fall fires tranche 1 then tranche 2 on separate ticks", func(t *testing.T) {
		t.Parallel()
		w, _, rec, notif, _ := snipeHarness()
		rec.eventSubs["evt"] = []int64{101}
		w.WatchEventMarkets("evt", []SnipeMarket{startedMarket("T1")})

		w.evaluate("T1", 0.585, 0.50)
		w.evaluate("T1", 0.10, 0.17) // in-band alert
		w.evaluate("T1", 0.02, 0.09) // tranche 1 (≤ 0.10, > 0.05)
		if notif.boxedCount() != 1 {
			t.Fatalf("after the 0.10 cross boxed=%d, want 1", notif.boxedCount())
		}
		w.evaluate("T1", 0.02, 0.045) // tranche 2 (≤ 0.05)
		if notif.boxedCount() != 2 {
			t.Fatalf("after the 0.05 cross boxed=%d, want 2", notif.boxedCount())
		}
		if a, b := notif.boxedAt(0), notif.boxedAt(1); a.tranche != 1 || b.tranche != 2 || b.ask != 0.045 {
			t.Errorf("tranches = [%d@%.3f, %d@%.3f], want [1@0.090, 2@0.045]", a.tranche, a.ask, b.tranche, b.ask)
		}
	})

	t.Run("gap straight to ≤ 0.05 fires BOTH tranches in one evaluate", func(t *testing.T) {
		t.Parallel()
		w, _, rec, notif, _ := snipeHarness()
		rec.eventSubs["evt"] = []int64{101}
		w.WatchEventMarkets("evt", []SnipeMarket{startedMarket("T1")})

		w.evaluate("T1", 0.585, 0.50)
		w.evaluate("T1", 0.10, 0.17) // in-band alert
		// Gap past both zones but above the deep zone (≥ 0.03, so deep does not
		// pre-empt): one evaluate fires tranche 1 AND tranche 2.
		w.evaluate("T1", 0.02, 0.045)
		if notif.deepCount() != 0 {
			t.Fatalf("deep fired at 0.045 (should be ≥ SnipeMinAsk)")
		}
		if notif.boxedCount() != 2 {
			t.Fatalf("gap-to-deep boxed=%d, want 2 (both tranches one tick)", notif.boxedCount())
		}
		if a, b := notif.boxedAt(0), notif.boxedAt(1); a.tranche != 1 || b.tranche != 2 {
			t.Errorf("tranches on the gap tick = [%d, %d], want [1, 2]", a.tranche, b.tranche)
		}
	})

	t.Run("gap into the deep zone: deep pre-empts, both tranches fire the next tick", func(t *testing.T) {
		t.Parallel()
		w, _, rec, notif, _ := snipeHarness()
		rec.eventSubs["evt"] = []int64{101}
		w.WatchEventMarkets("evt", []SnipeMarket{startedMarket("T1")})

		w.evaluate("T1", 0.45, 0.50)
		w.evaluate("T1", 0.10, 0.17)  // in-band alert
		w.evaluate("T1", 0.01, 0.004) // below SnipeDeepFloor: nothing fires
		if notif.boxedCount() != 0 || notif.deepCount() != 0 {
			t.Fatalf("something fired below the deep floor (deep=%d boxed=%d)", notif.deepCount(), notif.boxedCount())
		}
		// Inside [SnipeDeepFloor, SnipeMinAsk) deep and both boxed rungs all apply;
		// the dispatching latch makes deep win the first tick, both rungs the next.
		w.evaluate("T1", 0.01, 0.02)
		if notif.deepCount() != 1 || notif.boxedCount() != 0 {
			t.Fatalf("first deep-zone tick: deep=%d boxed=%d, want 1 and 0", notif.deepCount(), notif.boxedCount())
		}
		w.evaluate("T1", 0.01, 0.02)
		if notif.deepCount() != 1 || notif.boxedCount() != 2 {
			t.Errorf("deep=%d boxed=%d, want 1 and 2 (both rungs after deep)", notif.deepCount(), notif.boxedCount())
		}
	})

	t.Run("boxed is mutually exclusive with the in-band fire in one evaluate", func(t *testing.T) {
		t.Parallel()
		w, _, rec, notif, _ := snipeHarness()
		rec.eventSubs["evt"] = []int64{101}
		w.WatchEventMarkets("evt", []SnipeMarket{startedMarket("T1")})

		w.evaluate("T1", 0.585, 0.50)
		// A first crash straight into the fire∩boxed overlap [0.03, 0.10]: fire
		// takes the tick (sets dispatching); boxed must wait.
		w.evaluate("T1", 0.10, 0.08)
		if notif.count() != 1 || notif.boxedCount() != 0 {
			t.Fatalf("overlap tick: alerts=%d boxed=%d, want 1 and 0", notif.count(), notif.boxedCount())
		}
		w.evaluate("T1", 0.10, 0.08) // next tick: tranche 1 fires
		if notif.boxedCount() != 1 || notif.boxedAt(0).tranche != 1 {
			t.Errorf("post-fire boxed=%d tranche=%d, want 1 and tranche 1", notif.boxedCount(), notif.boxedAt(0).tranche)
		}
	})

	t.Run("no boxed without a prior in-band alert", func(t *testing.T) {
		t.Parallel()
		w, _, rec, notif, _ := snipeHarness()
		rec.eventSubs["evt"] = []int64{101}
		w.WatchEventMarkets("evt", []SnipeMarket{startedMarket("T1")})

		// Sub-competitive high: never fires the in-band alert, so the boxed ladder
		// (which requires it) must stay silent even at a ≤ 0.05 ask.
		w.evaluate("T1", 0.30, 0.35)
		w.evaluate("T1", 0.02, 0.04)
		if notif.count() != 0 || notif.boxedCount() != 0 {
			t.Errorf("alerts=%d boxed=%d, want 0 and 0", notif.count(), notif.boxedCount())
		}
	})

	t.Run("tranche 1 upper bound: 0.10 inclusive, just above stays silent", func(t *testing.T) {
		t.Parallel()
		w, _, rec, notif, _ := snipeHarness()
		rec.eventSubs["evt"] = []int64{101}
		w.WatchEventMarkets("evt", []SnipeMarket{startedMarket("T1")})

		w.evaluate("T1", 0.45, 0.50)
		w.evaluate("T1", 0.10, 0.17) // in-band alert
		w.evaluate("T1", 0.05, 0.11) // just above 0.10: silent
		if notif.boxedCount() != 0 {
			t.Fatalf("boxed fired above SnipeBoxedMaxAsk")
		}
		w.evaluate("T1", 0.05, SnipeBoxedMaxAsk) // exactly 0.10: tranche 1 fires
		if notif.boxedCount() != 1 || notif.boxedAt(0).tranche != 1 {
			t.Errorf("boxed at exactly SnipeBoxedMaxAsk = %d fires tranche %d, want 1 and tranche 1", notif.boxedCount(), notif.boxedAt(0).tranche)
		}
	})

	t.Run("tranche 2 upper bound: 0.05 inclusive, just above fires only tranche 1", func(t *testing.T) {
		t.Parallel()
		w, _, rec, notif, _ := snipeHarness()
		rec.eventSubs["evt"] = []int64{101}
		w.WatchEventMarkets("evt", []SnipeMarket{startedMarket("T1")})

		w.evaluate("T1", 0.45, 0.50)
		w.evaluate("T1", 0.10, 0.17)  // in-band alert
		w.evaluate("T1", 0.02, 0.051) // just above 0.05 → tranche 1 only
		if notif.boxedCount() != 1 || notif.boxedAt(0).tranche != 1 {
			t.Fatalf("at 0.051 boxed=%d (tranche %d), want 1 tranche 1", notif.boxedCount(), notif.boxedAt(0).tranche)
		}
		w.evaluate("T1", 0.02, SnipeBoxedDeepAsk) // exactly 0.05 → tranche 2 fires
		if notif.boxedCount() != 2 || notif.boxedAt(1).tranche != 2 {
			t.Errorf("at exactly SnipeBoxedDeepAsk boxed=%d, want 2 with tranche 2", notif.boxedCount())
		}
	})

	t.Run("sustained recovery resets BOTH latches (re-fires both next episode)", func(t *testing.T) {
		t.Parallel()
		w, _, rec, notif, clock := snipeHarness()
		rec.eventSubs["evt"] = []int64{101}
		w.WatchEventMarkets("evt", []SnipeMarket{startedMarket("T1")})

		w.evaluate("T1", 0.45, 0.50)
		w.evaluate("T1", 0.10, 0.17) // alert #1
		w.evaluate("T1", 0.02, 0.04) // both tranches (gap ≤ 0.05)
		if notif.boxedCount() != 2 {
			t.Fatalf("episode 1 boxed=%d, want 2", notif.boxedCount())
		}
		w.evaluate("T1", 0.32, 0.34) // corroborated recovery (bid >= snipeResetBid, #100)
		clock.advance(snipeResetConfirm + time.Second)
		w.evaluate("T1", 0.32, 0.34) // sustained recovery: episode resets
		w.evaluate("T1", 0.08, 0.16) // alert #2
		w.evaluate("T1", 0.02, 0.04) // both tranches again
		if notif.boxedCount() != 4 {
			t.Errorf("boxed=%d, want 4 (both rungs re-fire after reset)", notif.boxedCount())
		}
	})
}

// TestSnipeWatcher_BoxedLogLine pins the production log contract: a boxed fire
// logs the exact prefix "SnipeWatcher: boxed" (a monitor matches on it) and a
// tranche= field so the September review can score the two rungs separately.
// Not parallel: it redirects the global logger for the duration.
func TestSnipeWatcher_BoxedLogLine(t *testing.T) {
	buf := &syncBuffer{}
	log.SetOutput(buf)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })

	w, _, rec, _, _ := snipeHarness()
	rec.eventSubs["evt"] = []int64{101}
	w.WatchEventMarkets("evt", []SnipeMarket{startedMarket("T1")})
	w.evaluate("T1", 0.45, 0.50)
	w.evaluate("T1", 0.10, 0.17) // in-band alert
	w.evaluate("T1", 0.02, 0.04) // both tranches fire

	out := buf.String()
	for _, want := range []string{"SnipeWatcher: boxed", "tranche=1", "tranche=2"} {
		if !strings.Contains(out, want) {
			t.Errorf("boxed log missing %q in:\n%s", want, out)
		}
	}
}

// TestSnipeWatcher_EpisodeResetLogLine pins the production log contract for the
// bid-corroborated reset (#100): when the latch actually clears, the watcher
// logs exactly one line with the "SnipeWatcher: episode reset" prefix (a monitor
// greps it) carrying the token and the ask/bid that cleared it. Not parallel: it
// redirects the global logger for the duration.
func TestSnipeWatcher_EpisodeResetLogLine(t *testing.T) {
	buf := &syncBuffer{}
	log.SetOutput(buf)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })

	w, _, rec, _, clock := snipeHarness()
	rec.eventSubs["evt"] = []int64{101}
	w.WatchEventMarkets("evt", []SnipeMarket{startedMarket("T1")})
	w.evaluate("T1", 0.45, 0.50)
	w.evaluate("T1", 0.10, 0.17) // in-band alert
	w.evaluate("T1", 0.32, 0.34) // corroborated recovery: streak starts
	clock.advance(snipeResetConfirm + time.Second)
	w.evaluate("T1", 0.32, 0.34) // held past confirm: episode resets, logs once

	out := buf.String()
	for _, want := range []string{"SnipeWatcher: episode reset", "token=T1", "ask=0.340", "bid=0.320"} {
		if !strings.Contains(out, want) {
			t.Errorf("episode-reset log missing %q in:\n%s", want, out)
		}
	}
	if n := strings.Count(out, "SnipeWatcher: episode reset"); n != 1 {
		t.Errorf("episode-reset log lines = %d, want exactly 1", n)
	}
}

// TestSnipeWatcher_FavoriteCollapseLogLine pins the log-only favorite-collapse
// split (issue #107, September review proposal #2): when an in-band alert fires
// on a token whose session high reached snipeFavCollapseHigh (≥ 0.90), the
// watcher emits exactly one additional "SnipeWatcher: favorite-collapse" line —
// once per episode, never gating or notifying. It rides the in-band alert fire
// only (never deep-crash, boxed, or shadow-dip) and re-fires per episode. Not
// parallel: it redirects the global logger for the duration.
func TestSnipeWatcher_FavoriteCollapseLogLine(t *testing.T) {
	type step struct {
		bid, ask float64
		advance  time.Duration // clock advance BEFORE this observation
	}
	tests := []struct {
		name       string
		steps      []step
		wantFav    int      // "SnipeWatcher: favorite-collapse" lines
		wantAlerts int      // "SnipeWatcher: alert" lines (must stay unchanged)
		favFields  []string // substrings the split line must carry (checked when wantFav >= 1)
	}{
		{
			name: "high 0.945 crash fires one favorite-collapse split with correct fields",
			steps: []step{
				{bid: 0.945, ask: 0.95},
				{bid: 0.10, ask: 0.17}, // in-band alert
			},
			wantFav:    1,
			wantAlerts: 1,
			favFields:  []string{"SnipeWatcher: favorite-collapse", "token=T1", "high=0.945", "ask=0.170"},
		},
		{
			name: "high 0.880 crash logs no favorite-collapse split (the cut sits above 0.88)",
			steps: []step{
				{bid: 0.880, ask: 0.90},
				{bid: 0.10, ask: 0.17}, // in-band alert, but sub-0.90 high
			},
			wantFav:    0,
			wantAlerts: 1,
		},
		{
			name: "boundary high exactly 0.90 emits the split (>= semantics)",
			steps: []step{
				{bid: 0.90, ask: 0.92},
				{bid: 0.10, ask: 0.17}, // in-band alert
			},
			wantFav:    1,
			wantAlerts: 1,
			favFields:  []string{"high=0.900", "ask=0.170"},
		},
		{
			name: "deep/boxed continuation in the same episode does not re-emit the split",
			steps: []step{
				{bid: 0.945, ask: 0.95},
				{bid: 0.10, ask: 0.17}, // in-band alert → favorite-collapse #1
				{bid: 0.01, ask: 0.02}, // deep + boxed zone, no new in-band fire
			},
			wantFav:    1,
			wantAlerts: 1,
		},
		{
			name: "bid-corroborated reset then re-crash emits a second split (per-episode)",
			steps: []step{
				{bid: 0.945, ask: 0.95},
				{bid: 0.10, ask: 0.17},                                           // alert #1 → favorite-collapse #1
				{bid: 0.32, ask: 0.34},                                           // corroborated recovery: streak starts
				{bid: 0.32, ask: 0.34, advance: snipeResetConfirm + time.Second}, // held past confirm: resets
				{bid: 0.08, ask: 0.16},                                           // alert #2 → favorite-collapse #2
			},
			wantFav:    2,
			wantAlerts: 2,
		},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			buf := &syncBuffer{}
			log.SetOutput(buf)
			t.Cleanup(func() { log.SetOutput(os.Stderr) })

			w, _, rec, _, clock := snipeHarness()
			rec.eventSubs["evt"] = []int64{101}
			w.WatchEventMarkets("evt", []SnipeMarket{startedMarket("T1")})
			for _, s := range tt.steps {
				clock.advance(s.advance)
				w.evaluate("T1", s.bid, s.ask)
			}

			out := buf.String()
			if n := strings.Count(out, "SnipeWatcher: favorite-collapse"); n != tt.wantFav {
				t.Errorf("favorite-collapse lines = %d, want %d; log:\n%s", n, tt.wantFav, out)
			}
			if n := strings.Count(out, "SnipeWatcher: alert"); n != tt.wantAlerts {
				t.Errorf("alert lines = %d, want %d; log:\n%s", n, tt.wantAlerts, out)
			}
			if tt.wantFav >= 1 {
				for _, want := range tt.favFields {
					if !strings.Contains(out, want) {
						t.Errorf("favorite-collapse log missing %q in:\n%s", want, out)
					}
				}
			}
		})
	}
}

// TestSnipeWatcher_NoResetWhileDispatching: even a SUSTAINED above-reset
// recovery observed while the alert is still being delivered must not
// un-latch the episode (issue #50 belt-and-braces) — but once delivery
// completes, the normal sustained-reset path works again.
func TestSnipeWatcher_NoResetWhileDispatching(t *testing.T) {
	t.Parallel()
	feed := newFakeFeed()
	rec := newFakeSnipeRecipients()
	clock := newFakeClock()
	notif := &reentrantSnipeNotifier{}
	w := NewSnipeWatcher(feed, rec, notif)
	w.now = clock.now
	notif.during = func() {
		w.evaluate("T1", 0.32, 0.31)
		clock.advance(snipeResetConfirm + time.Second)
		w.evaluate("T1", 0.32, 0.31) // sustained + corroborated, but delivery in flight
		w.evaluate("T1", 0.10, 0.14) // re-crash: must NOT re-alert
	}

	rec.eventSubs["evt"] = []int64{101}
	w.WatchEventMarkets("evt", []SnipeMarket{startedMarket("T1")})
	w.evaluate("T1", 0.61, 0.50)
	w.evaluate("T1", 0.10, 0.20) // alert #1; re-entrant evals run inside delivery

	if got := notif.inner.count(); got != 1 {
		t.Fatalf("alerts with delivery in flight = %d, want 1", got)
	}

	// Delivery done: the same sustained recovery now resets and re-fires.
	w.evaluate("T1", 0.32, 0.31)
	clock.advance(snipeResetConfirm + time.Second)
	w.evaluate("T1", 0.32, 0.31)
	w.evaluate("T1", 0.10, 0.14)
	if got := notif.inner.count(); got != 2 {
		t.Errorf("alerts after delivery completed = %d, want 2 (dispatching flag must clear)", got)
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
	// No seeder wired (nil = seeding disabled): a fresh watcher never saw the
	// pre-crash high, so a crash immediately after start cannot alert. With
	// the production seeder the high re-seeds from trade history — see
	// snipe_seed_test.go.
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

// TestSnipeWatcher_RenewHeldMarket: renewing a held token also renews every
// watched token sharing its market (the sibling watch, issue #78), so a position
// refresh that only sees the held side keeps the flip side alive too — otherwise
// the sibling's TTL lapses and only one side stays watched.
func TestSnipeWatcher_RenewHeldMarket(t *testing.T) {
	t.Parallel()
	w, _, _, notif, clock := snipeHarness()
	if w.RenewHeldMarket(404, "A", time.Hour) {
		t.Fatal("RenewHeldMarket on unknown token = true, want false")
	}

	a := startedMarket("A")
	sib := startedMarket("B")
	sib.MarketID = a.MarketID // two outcomes of one market
	w.WatchHeld(404, a, time.Hour)
	w.WatchHeld(404, sib, time.Hour)

	clock.advance(30 * time.Minute)
	if !w.RenewHeldMarket(404, "A", time.Hour) {
		t.Fatal("RenewHeldMarket on watched token = false, want true")
	}
	clock.advance(45 * time.Minute) // original TTLs (60m) passed; renewed ones (90m) have not

	// The SIBLING — never itself refreshed as a position — still alerts: its TTL
	// rode the held token's market renewal.
	w.evaluate("B", 0.45, 0.50)
	w.evaluate("B", 0.10, 0.17)
	if notif.count() != 1 {
		t.Errorf("sibling alerts after market renew = %d, want 1 (TTL renewed with the market)", notif.count())
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
