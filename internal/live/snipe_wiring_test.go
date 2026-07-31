package live

import (
	"context"
	"testing"
	"time"

	"github.com/Catorpilor/poly/internal/database"
)

// snipeWiringEvent mirrors the pinned-feed fixture with a game start time so
// snipe wiring can be asserted end to end.
func snipeWiringEvent() *EventInfo {
	return &EventInfo{
		ID:    "e",
		Slug:  pinnedFeedEventSlug,
		Title: "BLG vs. HLE",
		Markets: []MarketInfo{
			{ID: "ml", Slug: pinnedFeedEventSlug, Question: "BLG vs. HLE",
				OutcomesRaw: `["BLG","HLE"]`, ClobTokenIdsRaw: `["ml-blg","ml-hle"]`,
				GameStartTimeRaw: "2026-07-12 10:00:00+00", Active: true},
			{ID: "g3", Slug: pinnedFeedGame3Slug, Question: "BLG vs. HLE - Game 3 Winner",
				OutcomesRaw: `["BLG","HLE"]`, ClobTokenIdsRaw: `["g3-blg","g3-hle"]`,
				GameStartTimeRaw: "2026-07-12 10:00:00+00", Active: true},
		},
	}
}

// newSnipeWiredManager builds a LiveTradeManager with a cached event and a
// snipe watcher backed by fakes, skipping all network dials.
func newSnipeWiredManager(t *testing.T) (*LiveTradeManager, *SnipeWatcher, *fakeFeed) {
	t.Helper()

	m := &LiveTradeManager{
		subscriptions:     NewSubscriptionRegistry(),
		resolver:          NewEventSlugResolver(),
		formatter:         NewTradeFormatter(),
		assetToEvent:      make(map[string]string),
		assetToMarketName: make(map[string]string),
		subscribed:        true, // skip the RTDS dial in asset tracking
	}
	event := snipeWiringEvent()
	for _, slug := range []string{pinnedFeedEventSlug, pinnedFeedGame3Slug} {
		m.resolver.cacheEvent(slug, event)
	}

	feed := newFakeFeed()
	w := NewSnipeWatcher(feed, newFakeSnipeRecipients(), &fakeSnipeNotifier{})
	m.SetSnipeWatcher(w)
	return m, w, feed
}

func TestMarketInfoGetGameStartTime(t *testing.T) {
	t.Parallel()
	m := &MarketInfo{GameStartTimeRaw: "2026-01-18 03:00:00+00"}
	want := time.Date(2026, 1, 18, 3, 0, 0, 0, time.UTC)
	if got := m.GetGameStartTime(); !got.Equal(want) {
		t.Errorf("GetGameStartTime() = %v, want %v", got, want)
	}
	if got := (&MarketInfo{}).GetGameStartTime(); !got.IsZero() {
		t.Errorf("empty GetGameStartTime() = %v, want zero", got)
	}
}

func TestRegistryHasAnySubscribers(t *testing.T) {
	t.Parallel()
	r := NewSubscriptionRegistry()
	if r.HasAnySubscribers("evt") {
		t.Fatal("empty registry reports subscribers")
	}
	r.SubscribeTelegram(7, "evt")
	if !r.HasAnySubscribers("evt") {
		t.Fatal("telegram subscriber not reported")
	}
	r.UnsubscribeTelegram(7, "evt")
	if r.HasAnySubscribers("evt") {
		t.Fatal("subscriber reported after last telegram unsubscribe")
	}
}

func TestSnipeMarketsForMLMarkets(t *testing.T) {
	t.Parallel()
	event := snipeWiringEvent()
	resolver := NewEventSlugResolver()
	got := snipeMarketsFor(resolver.GetAllMLMarkets(event))
	if len(got) != 2 {
		t.Fatalf("snipe markets = %d, want 2 (ML tokens only)", len(got))
	}
	wantStart := time.Date(2026, 7, 12, 10, 0, 0, 0, time.UTC)
	for i, want := range []SnipeMarket{
		{TokenID: "ml-blg", MarketID: "ml", Question: "BLG vs. HLE", Outcome: "BLG", GameStart: wantStart},
		{TokenID: "ml-hle", MarketID: "ml", Question: "BLG vs. HLE", Outcome: "HLE", GameStart: wantStart},
	} {
		if got[i].TokenID != want.TokenID || got[i].MarketID != want.MarketID ||
			got[i].Question != want.Question || got[i].Outcome != want.Outcome ||
			!got[i].GameStart.Equal(want.GameStart) {
			t.Errorf("snipe market[%d] = %+v, want %+v", i, got[i], want)
		}
	}
}

func TestSnipeWiringTelegramSubscribeLifecycle(t *testing.T) {
	t.Parallel()
	m, w, _ := newSnipeWiredManager(t)
	ctx := context.Background()

	if _, err := m.SubscribeTelegram(ctx, 7, pinnedFeedEventSlug); err != nil {
		t.Fatalf("SubscribeTelegram: %v", err)
	}
	for _, tok := range []string{"ml-blg", "ml-hle"} {
		if !w.isWatched(tok) {
			t.Errorf("token %s not watched after telegram subscribe", tok)
		}
	}
	if w.isWatched("g3-blg") {
		t.Error("sub-market token g3-blg watched — snipe universe must mirror the trade feed's ML resolution")
	}

	// A second subscriber keeps the watch alive after the first leaves.
	if _, err := m.SubscribeTelegram(ctx, 8, pinnedFeedEventSlug); err != nil {
		t.Fatalf("SubscribeTelegram(second): %v", err)
	}
	m.UnsubscribeTelegram(7, pinnedFeedEventSlug)
	if !w.isWatched("ml-blg") {
		t.Fatal("token released while a subscriber remains")
	}

	// Last subscriber leaving releases the tokens.
	m.UnsubscribeTelegram(8, pinnedFeedEventSlug)
	if w.isWatched("ml-blg") || w.isWatched("ml-hle") {
		t.Error("tokens still watched after the last subscriber left")
	}
}

func TestSnipeWiringUnsubscribeAllTelegram(t *testing.T) {
	t.Parallel()
	m, w, _ := newSnipeWiredManager(t)
	ctx := context.Background()

	if _, err := m.SubscribeTelegram(ctx, 7, pinnedFeedEventSlug); err != nil {
		t.Fatalf("SubscribeTelegram: %v", err)
	}
	m.UnsubscribeAllTelegram(7)
	if w.isWatched("ml-blg") {
		t.Error("token still watched after UnsubscribeAllTelegram")
	}
}

func TestSnipeWiringWebPinnedSubscribeAndDisconnect(t *testing.T) {
	t.Parallel()
	m, w, _ := newSnipeWiredManager(t)
	server, _ := newWSPair(t)
	m.RegisterWebConn(server)

	if err := m.SubscribeWeb(server, pinnedFeedGame3Slug, false); err != nil {
		t.Fatalf("SubscribeWeb: %v", err)
	}
	for _, tok := range []string{"g3-blg", "g3-hle"} {
		if !w.isWatched(tok) {
			t.Errorf("pinned token %s not watched after web subscribe", tok)
		}
	}
	if w.isWatched("ml-blg") {
		t.Error("pinned subscription must watch the pinned market's tokens, not the Moneyline")
	}

	// Disconnect (removes the conn from every event) releases the tokens.
	m.UnsubscribeWeb(server)
	if w.isWatched("g3-blg") || w.isWatched("g3-hle") {
		t.Error("pinned tokens still watched after web disconnect")
	}
}

func TestSnipeWiringWebKeepsTelegramWatchAlive(t *testing.T) {
	t.Parallel()
	m, w, _ := newSnipeWiredManager(t)
	ctx := context.Background()
	server, _ := newWSPair(t)
	m.RegisterWebConn(server)

	if err := m.SubscribeWeb(server, pinnedFeedEventSlug, false); err != nil {
		t.Fatalf("SubscribeWeb: %v", err)
	}
	if _, err := m.SubscribeTelegram(ctx, 7, pinnedFeedEventSlug); err != nil {
		t.Fatalf("SubscribeTelegram: %v", err)
	}

	// Telegram leaves but the web panel remains: keep watching.
	m.UnsubscribeTelegram(7, pinnedFeedEventSlug)
	if !w.isWatched("ml-blg") {
		t.Fatal("token released while a web subscriber remains")
	}

	if !m.UnsubscribeWebFromEvent(server, pinnedFeedEventSlug) {
		t.Fatal("UnsubscribeWebFromEvent returned false")
	}
	if w.isWatched("ml-blg") {
		t.Error("token still watched after the last (web) subscriber left")
	}
}

func TestSnipeRecipientResolverAdapter(t *testing.T) {
	t.Parallel()
	m := &LiveTradeManager{subscriptions: NewSubscriptionRegistry()}
	m.subscriptions.SubscribeTelegram(7, "evt")
	m.subscriptions.SubscribeTelegram(8, "evt")

	store := newFakeStore()
	store.seed(&database.SLTPArm{ID: 1, TelegramID: 42, TokenID: "T1", SLArmed: true})
	store.seed(&database.SLTPArm{ID: 2, TelegramID: 42, TokenID: "T1", TPArmed: true})

	r := NewSnipeRecipientResolver(m, store)

	subs := r.EventSubscribers("evt")
	if len(subs) != 2 {
		t.Errorf("EventSubscribers = %v, want 2 chat IDs", subs)
	}
	owners := r.ArmOwners("T1")
	if len(owners) != 1 || owners[0] != 42 {
		t.Errorf("ArmOwners = %v, want deduped [42]", owners)
	}
	if got := r.ArmOwners("unknown"); len(got) != 0 {
		t.Errorf("ArmOwners(unknown) = %v, want empty", got)
	}
}
