package live

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeClosedEventChecker implements ClosedEventChecker. A slug without a
// configured event or error behaves like Gamma's live-verified negative: the
// closed=true filter returns nothing, surfaced as ErrEventNotClosed.
type fakeClosedEventChecker struct {
	mu     sync.Mutex
	calls  []string
	events map[string]*EventInfo
	errs   map[string]error
}

func newFakeClosedEventChecker() *fakeClosedEventChecker {
	return &fakeClosedEventChecker{
		events: make(map[string]*EventInfo),
		errs:   make(map[string]error),
	}
}

func (c *fakeClosedEventChecker) ClosedEventBySlug(_ context.Context, slug string) (*EventInfo, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls = append(c.calls, slug)
	if err := c.errs[slug]; err != nil {
		return nil, err
	}
	if ev := c.events[slug]; ev != nil {
		return ev, nil
	}
	return nil, fmt.Errorf("%w: %s", ErrEventNotClosed, slug)
}

func (c *fakeClosedEventChecker) setClosed(slug, title string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.events[slug] = &EventInfo{
		ID: "e", Slug: slug, Title: title,
		Markets: []MarketInfo{{ID: "m1", Question: title, Closed: true, Active: false}},
	}
}

// setPartlyClosed returns an event where one market is still open — the sweep's
// belt (allMarketsClosed) must reject it even though it "came back".
func (c *fakeClosedEventChecker) setPartlyClosed(slug string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.events[slug] = &EventInfo{
		ID: "e", Slug: slug, Title: "partly",
		Markets: []MarketInfo{
			{ID: "m1", Closed: true, Active: false},
			{ID: "m2", Closed: false, Active: true}, // still open
		},
	}
}

func (c *fakeClosedEventChecker) setErr(slug string, err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.errs[slug] = err
}

func (c *fakeClosedEventChecker) callCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.calls)
}

// newExpiryManager wires the snipe-watch + persistence harness with a Telegram
// sender and a fake closed-event checker, so a positive sweep can be asserted
// end to end (registry, store, snipe release, grouped notice).
func newExpiryManager(t *testing.T) (*LiveTradeManager, *SnipeWatcher, *fakeWatchStore, *fakeFeedSender, *fakeClosedEventChecker) {
	t.Helper()
	m, w, store := newPersistedManager(t)
	sender := &fakeFeedSender{}
	m.SetTelegramBot(sender)
	checker := newFakeClosedEventChecker()
	m.SetClosedEventChecker(checker)
	return m, w, store, sender, checker
}

// A watch is expired only on positive all-markets-closed evidence: every
// subscriber is removed, the durable row deleted, the snipe watch released, and
// each user gets one grouped notice.
func TestWatchExpiry_AllMarketsClosedSweeps(t *testing.T) {
	t.Parallel()
	m, w, store, sender, checker := newExpiryManager(t)
	ctx := context.Background()

	// Two users on the same event (snipe watches ml-blg/ml-hle at subscribe).
	for _, chatID := range []int64{7, 8} {
		if _, err := m.SubscribeTelegram(ctx, chatID, pinnedFeedEventSlug, false); err != nil {
			t.Fatalf("SubscribeTelegram(%d): %v", chatID, err)
		}
	}
	if !w.isWatched("ml-blg") || !w.isWatched("ml-hle") {
		t.Fatal("snipe tokens not watched after subscribe")
	}
	checker.setClosed(pinnedFeedEventSlug, "BLG vs. HLE")

	swept, kept, errCount := m.sweepExpiredWatches(ctx)
	if swept != 1 || kept != 0 || errCount != 0 {
		t.Fatalf("counts swept=%d kept=%d errors=%d, want 1/0/0", swept, kept, errCount)
	}

	// Registry cleared for every subscriber.
	if m.subscriptions.HasTelegramSubscribers(pinnedFeedEventSlug) {
		t.Error("event still has subscribers after sweep")
	}
	for _, chatID := range []int64{7, 8} {
		if len(m.GetUserSubscriptions(chatID)) != 0 {
			t.Errorf("chat %d still subscribed after sweep", chatID)
		}
	}
	// Durable row deleted per (chat, slug): two subscribers → two deletes.
	if store.deletes != 2 {
		t.Errorf("store deletes = %d, want 2 (one per subscriber)", store.deletes)
	}
	if store.rowCount() != 0 {
		t.Errorf("store rows remain after sweep: %d", store.rowCount())
	}
	// Snipe watch released once the last subscriber left.
	if w.isWatched("ml-blg") || w.isWatched("ml-hle") {
		t.Error("snipe tokens still watched after sweep — resources leaked")
	}
	// Exactly one grouped 🧹 notice per user.
	msgs := sender.messages()
	if len(msgs) != 2 {
		t.Fatalf("notices = %d, want 2 (one per user)", len(msgs))
	}
	seen := map[int64]bool{}
	for _, msg := range msgs {
		seen[msg.chatID] = true
		if !strings.Contains(msg.text, "🧹") || !strings.Contains(msg.text, "BLG vs. HLE") {
			t.Errorf("notice to %d missing 🧹 or title: %q", msg.chatID, msg.text)
		}
	}
	if !seen[7] || !seen[8] {
		t.Errorf("notices did not reach both users: %v", seen)
	}
}

// One still-open market keeps the whole event's watch (the allMarketsClosed
// belt), even though the checker returned an event.
func TestWatchExpiry_OneMarketOpenKept(t *testing.T) {
	t.Parallel()
	m, w, store, sender, checker := newExpiryManager(t)
	ctx := context.Background()

	if _, err := m.SubscribeTelegram(ctx, 7, pinnedFeedEventSlug, false); err != nil {
		t.Fatalf("SubscribeTelegram: %v", err)
	}
	checker.setPartlyClosed(pinnedFeedEventSlug)

	swept, kept, _ := m.sweepExpiredWatches(ctx)
	if swept != 0 || kept != 1 {
		t.Fatalf("counts swept=%d kept=%d, want 0/1", swept, kept)
	}
	if !m.subscriptions.HasTelegramSubscribers(pinnedFeedEventSlug) {
		t.Error("watch dropped despite an open market")
	}
	if !w.isWatched("ml-blg") {
		t.Error("snipe watch released despite an open market")
	}
	if store.deletes != 0 {
		t.Errorf("store deletes = %d, want 0", store.deletes)
	}
	if len(sender.messages()) != 0 {
		t.Errorf("notice sent for a kept watch: %v", sender.messages())
	}
}

// Not-found (the common negative) keeps the watch, quietly.
func TestWatchExpiry_NotFoundKept(t *testing.T) {
	t.Parallel()
	m, _, store, sender, _ := newExpiryManager(t) // checker has no entry → ErrEventNotClosed
	ctx := context.Background()

	if _, err := m.SubscribeTelegram(ctx, 7, pinnedFeedEventSlug, false); err != nil {
		t.Fatalf("SubscribeTelegram: %v", err)
	}

	swept, kept, errCount := m.sweepExpiredWatches(ctx)
	if swept != 0 || kept != 1 || errCount != 0 {
		t.Fatalf("counts swept=%d kept=%d errors=%d, want 0/1/0", swept, kept, errCount)
	}
	if !m.subscriptions.HasTelegramSubscribers(pinnedFeedEventSlug) {
		t.Error("not-found must keep the watch")
	}
	if store.deletes != 0 {
		t.Errorf("store deletes = %d, want 0", store.deletes)
	}
	if len(sender.messages()) != 0 {
		t.Error("no notice expected for a kept watch")
	}
}

// A real lookup error keeps the watch (fail-safe) and counts as an error —
// never a delete. Covers "resolve error / identity mismatch → kept, logged".
func TestWatchExpiry_LookupErrorKept(t *testing.T) {
	t.Parallel()
	m, _, store, sender, checker := newExpiryManager(t)
	ctx := context.Background()

	if _, err := m.SubscribeTelegram(ctx, 7, pinnedFeedEventSlug, false); err != nil {
		t.Fatalf("SubscribeTelegram: %v", err)
	}
	checker.setErr(pinnedFeedEventSlug, errors.New("gamma 500 / filter not applied"))

	swept, kept, errCount := m.sweepExpiredWatches(ctx)
	if swept != 0 || kept != 1 || errCount != 1 {
		t.Fatalf("counts swept=%d kept=%d errors=%d, want 0/1/1", swept, kept, errCount)
	}
	if !m.subscriptions.HasTelegramSubscribers(pinnedFeedEventSlug) {
		t.Error("lookup error must keep the watch")
	}
	if store.deletes != 0 {
		t.Errorf("store deletes = %d, want 0 (never delete on error)", store.deletes)
	}
	if len(sender.messages()) != 0 {
		t.Error("no notice expected on error")
	}
}

// One user subscribed to two closed events gets a SINGLE grouped notice listing
// both; a mixed open/closed event set sweeps only the closed one.
func TestWatchExpiry_GroupedNoticePerUser(t *testing.T) {
	t.Parallel()
	m, _, _, sender, checker := newExpiryManager(t)
	ctx := context.Background()

	// pinnedFeedGame3Slug is a pinned sub-market slug in the harness cache, so it
	// resolves for subscribe; both are watched by chat 7.
	for _, slug := range []string{pinnedFeedEventSlug, pinnedFeedGame3Slug} {
		if _, err := m.SubscribeTelegram(ctx, 7, slug, false); err != nil {
			t.Fatalf("SubscribeTelegram(%s): %v", slug, err)
		}
	}
	// Both closed → both swept for chat 7 in one pass.
	checker.setClosed(pinnedFeedEventSlug, "BLG vs. HLE")
	checker.setClosed(pinnedFeedGame3Slug, "BLG vs. HLE - Game 3")

	swept, _, _ := m.sweepExpiredWatches(ctx)
	if swept != 2 {
		t.Fatalf("swept=%d, want 2", swept)
	}
	msgs := sender.messages()
	if len(msgs) != 1 {
		t.Fatalf("notices = %d, want 1 grouped message for the user", len(msgs))
	}
	if !strings.Contains(msgs[0].text, "BLG vs. HLE") ||
		!strings.Contains(msgs[0].text, "Game 3") {
		t.Errorf("grouped notice missing one of the two titles: %q", msgs[0].text)
	}
	if !strings.Contains(msgs[0].text, "2 finished event") {
		t.Errorf("grouped notice should report 2 events: %q", msgs[0].text)
	}
}

// After a positive sweep the event is gone from the registry, so the Event
// Refresh loop no longer scans it (and stops logging the per-cycle failure).
func TestWatchExpiry_SweptEventGoneFromRefresh(t *testing.T) {
	t.Parallel()
	m, _, _, _, checker := newExpiryManager(t)
	m.refreshPause = 0
	ctx := context.Background()

	if _, err := m.SubscribeTelegram(ctx, 7, pinnedFeedEventSlug, false); err != nil {
		t.Fatalf("SubscribeTelegram: %v", err)
	}
	// Before the sweep the refresh cycle scans the event.
	if events, _, _ := m.refreshCycle(ctx); events != 1 {
		t.Fatalf("pre-sweep refresh scanned %d events, want 1", events)
	}

	checker.setClosed(pinnedFeedEventSlug, "BLG vs. HLE")
	if swept, _, _ := m.sweepExpiredWatches(ctx); swept != 1 {
		t.Fatalf("sweep did not remove the watch")
	}

	// After the sweep the refresh cycle has nothing to scan — the failure noise
	// for this closed event is gone.
	events, _, failed := m.refreshCycle(ctx)
	if events != 0 || failed != 0 {
		t.Errorf("post-sweep refresh events=%d failed=%d, want 0/0", events, failed)
	}
}

// No checker wired: the sweep is a disabled no-op and StartWatchExpirySweep
// launches nothing.
func TestWatchExpiry_DisabledWithoutChecker(t *testing.T) {
	t.Parallel()
	m, _, _ := newSnipeWiredManager(t)
	m.subscriptions.SubscribeTelegram(7, "evt", false)

	swept, kept, errCount := m.sweepExpiredWatches(context.Background())
	if swept != 0 || kept != 0 || errCount != 0 {
		t.Errorf("disabled sweep counts = %d/%d/%d, want 0/0/0", swept, kept, errCount)
	}
	if !m.subscriptions.HasTelegramSubscribers("evt") {
		t.Error("disabled sweep must not touch subscriptions")
	}
	m.StartWatchExpirySweep() // must not panic or start anything
}

// The sweep loop fires from a manager Start and stops when the manager stops
// (m.ctx cancel), mirroring the SL/TP sweeper lifecycle test.
func TestWatchExpiry_LoopStartsAndStopsOnManagerStop(t *testing.T) {
	t.Parallel()
	m := NewLiveTradeManager() // real ctx/cancel; Stop() cancels it
	m.sweepInitialDelay = time.Millisecond
	m.sweepInterval = time.Millisecond
	checker := newFakeClosedEventChecker()
	m.SetClosedEventChecker(checker)

	const slug = "loop-evt"
	m.subscriptions.SubscribeTelegram(1, slug, false)
	checker.setClosed(slug, "Loop Event")

	m.StartWatchExpirySweep()
	waitFor(t, func() bool { return !m.subscriptions.HasTelegramSubscribers(slug) })

	if err := m.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	// After Stop, later intervals must not run another sweep. Re-subscribe and
	// confirm it is never swept (the goroutine has exited).
	m.subscriptions.SubscribeTelegram(2, slug, false)
	before := checker.callCount()
	time.Sleep(20 * time.Millisecond)
	if got := checker.callCount(); got != before {
		t.Errorf("sweep ran after Stop: calls went %d → %d", before, got)
	}
	if !m.subscriptions.HasTelegramSubscribers(slug) {
		t.Error("post-Stop subscription was swept — loop did not stop")
	}
}

// The loop also stops on plain ctx cancel (driven directly, tiny delays).
func TestWatchExpiry_LoopStopsOnCtxCancel(t *testing.T) {
	t.Parallel()
	m, _, _, _, checker := newExpiryManager(t)
	m.sweepInitialDelay = time.Millisecond
	m.sweepInterval = time.Millisecond

	const slug = "ctx-evt"
	m.subscriptions.SubscribeTelegram(1, slug, false) // never closed → checker keeps calling

	ctx, cancel := context.WithCancel(context.Background())
	go m.runWatchExpirySweep(ctx)
	waitFor(t, func() bool { return checker.callCount() >= 2 }) // loop is live
	cancel()

	time.Sleep(10 * time.Millisecond) // drain in-flight cycle
	before := checker.callCount()
	time.Sleep(30 * time.Millisecond) // several would-be intervals
	if got := checker.callCount(); got != before {
		t.Errorf("sweeps after cancel = %d, want frozen at %d", got, before)
	}
}

func TestWatchExpiredText(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		titles []string
		want   []string // substrings that must appear
	}{
		{"single", []string{"BLG vs. HLE"}, []string{"🧹", "1 finished event", "BLG vs. HLE", "Nothing was traded"}},
		{"grouped", []string{"A vs B", "C vs D"}, []string{"2 finished event", "A vs B, C vs D"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := watchExpiredText(tc.titles)
			for _, sub := range tc.want {
				if !strings.Contains(got, sub) {
					t.Errorf("text %q missing %q", got, sub)
				}
			}
		})
	}
}
