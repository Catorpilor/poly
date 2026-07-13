package live

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// A pinned subscription's feed must carry only the pinned market's trades.
// Before this, SubscribeWeb tracked the parent event's Moneyline assets
// under the pinned slug: every series-winner trade flushed into the pinned
// panel, two pinned panels from one event overwrote each other's asset
// mapping (last subscriber won all ML trades), and the pinned market's own
// trades never matched at all — its assets were untracked and the prefix
// fallback compares the parent event slug against the longer pinned slug.

const (
	pinnedFeedEventSlug = "lol-blg-hle1-2026-07-12"
	pinnedFeedGame3Slug = "lol-blg-hle1-2026-07-12-game3"
	pinnedFeedGame4Slug = "lol-blg-hle1-2026-07-12-game4"
	pinnedFeedKillsSlug = "lol-blg-hle1-2026-07-12-game3-total-kills"
)

func pinnedFeedEvent() *EventInfo {
	return &EventInfo{
		ID:    "e",
		Slug:  pinnedFeedEventSlug,
		Title: "BLG vs. HLE",
		Markets: []MarketInfo{
			{ID: "ml", Slug: pinnedFeedEventSlug, Question: "BLG vs. HLE", OutcomesRaw: `["BLG","HLE"]`, ClobTokenIdsRaw: `["ml-blg","ml-hle"]`, Active: true},
			{ID: "g3", Slug: pinnedFeedGame3Slug, Question: "BLG vs. HLE - Game 3 Winner", OutcomesRaw: `["BLG","HLE"]`, ClobTokenIdsRaw: `["g3-blg","g3-hle"]`, Active: true},
			{ID: "g4", Slug: pinnedFeedGame4Slug, Question: "BLG vs. HLE - Game 4 Winner", OutcomesRaw: `["BLG","HLE"]`, ClobTokenIdsRaw: `["g4-blg","g4-hle"]`, Active: true},
			{ID: "km", Slug: pinnedFeedKillsSlug, Question: "Game 3 Total Kills", OutcomesRaw: `["Over","Under"]`, ClobTokenIdsRaw: `["km-over","km-under"]`, Active: true},
		},
	}
}

func newPinnedFeedManager(t *testing.T) *LiveTradeManager {
	t.Helper()

	m := &LiveTradeManager{
		subscriptions:     NewSubscriptionRegistry(),
		resolver:          NewEventSlugResolver(),
		formatter:         NewTradeFormatter(),
		assetToEvent:      make(map[string]string),
		assetToMarketName: make(map[string]string),
		subscribed:        true, // skip the RTDS dial in asset tracking
	}
	event := pinnedFeedEvent()
	for _, slug := range []string{pinnedFeedEventSlug, pinnedFeedGame3Slug, pinnedFeedGame4Slug, pinnedFeedKillsSlug} {
		m.resolver.cacheEvent(slug, event)
	}
	return m
}

func pinnedFeedTrade(asset, marketSlug, outcome string) *rtdsTradePayload {
	return &rtdsTradePayload{
		Asset:     asset,
		Side:      "BUY",
		Outcome:   outcome,
		Slug:      marketSlug,
		EventSlug: pinnedFeedEventSlug,
		Timestamp: time.Now().UnixMilli(),
	}
}

// readTradeFrame reads one broadcast frame and returns its eventSlug.
func readTradeFrame(t *testing.T, client *websocket.Conn) string {
	t.Helper()

	client.SetReadDeadline(time.Now().Add(2 * time.Second))
	_, data, err := client.ReadMessage()
	if err != nil {
		t.Fatalf("expected a trade frame, got read error: %v", err)
	}
	var frame struct {
		EventSlug string `json:"eventSlug"`
	}
	if err := json.Unmarshal(data, &frame); err != nil {
		t.Fatalf("unmarshal trade frame: %v", err)
	}
	return frame.EventSlug
}

func assertNoFrame(t *testing.T, client *websocket.Conn) {
	t.Helper()

	client.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
	if _, data, err := client.ReadMessage(); err == nil {
		t.Fatalf("expected no frame, got: %s", data)
	}
}

func TestSubscribeWebPinnedTracksPinnedAssets(t *testing.T) {
	t.Parallel()

	m := newPinnedFeedManager(t)
	server, _ := newWSPair(t)
	m.RegisterWebConn(server)

	if err := m.SubscribeWeb(server, pinnedFeedGame3Slug, false); err != nil {
		t.Fatalf("SubscribeWeb: %v", err)
	}

	m.assetMu.RLock()
	defer m.assetMu.RUnlock()
	for _, asset := range []string{"g3-blg", "g3-hle"} {
		if got := m.assetToEvent[asset]; got != pinnedFeedGame3Slug {
			t.Errorf("assetToEvent[%s] = %q, want %q", asset, got, pinnedFeedGame3Slug)
		}
	}
	for _, asset := range []string{"ml-blg", "ml-hle"} {
		if got, ok := m.assetToEvent[asset]; ok {
			t.Errorf("assetToEvent[%s] = %q, want untracked — pinned subscription must not track the Moneyline", asset, got)
		}
	}
}

func TestSubscribeWebEventSlugStillTracksMoneyline(t *testing.T) {
	t.Parallel()

	m := newPinnedFeedManager(t)
	server, _ := newWSPair(t)
	m.RegisterWebConn(server)

	if err := m.SubscribeWeb(server, pinnedFeedEventSlug, false); err != nil {
		t.Fatalf("SubscribeWeb: %v", err)
	}

	m.assetMu.RLock()
	defer m.assetMu.RUnlock()
	for _, asset := range []string{"ml-blg", "ml-hle"} {
		if got := m.assetToEvent[asset]; got != pinnedFeedEventSlug {
			t.Errorf("assetToEvent[%s] = %q, want %q", asset, got, pinnedFeedEventSlug)
		}
	}
}

// Two pinned panels from the same event, one connection — the user's
// actual setup. Each panel gets its own market's trades; Moneyline trades
// reach neither.
func TestHandleTradeRoutesToPinnedPanel(t *testing.T) {
	t.Parallel()

	m := newPinnedFeedManager(t)
	server, client := newWSPair(t)
	m.RegisterWebConn(server)

	for _, slug := range []string{pinnedFeedGame3Slug, pinnedFeedGame4Slug} {
		if err := m.SubscribeWeb(server, slug, false); err != nil {
			t.Fatalf("SubscribeWeb(%s): %v", slug, err)
		}
	}

	if !m.handleTrade(pinnedFeedTrade("g3-blg", pinnedFeedGame3Slug, "BLG")) {
		t.Fatal("game3 trade should match the game3 subscription")
	}
	if got := readTradeFrame(t, client); got != pinnedFeedGame3Slug {
		t.Fatalf("game3 trade routed to %q, want %q", got, pinnedFeedGame3Slug)
	}

	if !m.handleTrade(pinnedFeedTrade("g4-hle", pinnedFeedGame4Slug, "HLE")) {
		t.Fatal("game4 trade should match the game4 subscription")
	}
	if got := readTradeFrame(t, client); got != pinnedFeedGame4Slug {
		t.Fatalf("game4 trade routed to %q, want %q", got, pinnedFeedGame4Slug)
	}

	if m.handleTrade(pinnedFeedTrade("ml-blg", pinnedFeedEventSlug, "BLG")) {
		t.Fatal("Moneyline trade must not match when only pinned subscriptions exist")
	}
	assertNoFrame(t, client)
}

// A pinned market whose slug carries sub-market indicators ("-total-",
// "-kills-", …) is still the market the subscriber asked for. The
// allMarkets gate applies to prefix-matched spillover only, never to
// asset-matched trades.
func TestHandleTradePinnedSubMarketBypassesAllMarketsGate(t *testing.T) {
	t.Parallel()

	m := newPinnedFeedManager(t)
	server, client := newWSPair(t)
	m.RegisterWebConn(server)

	if err := m.SubscribeWeb(server, pinnedFeedKillsSlug, false); err != nil {
		t.Fatalf("SubscribeWeb: %v", err)
	}

	if !m.handleTrade(pinnedFeedTrade("km-over", pinnedFeedKillsSlug, "Over")) {
		t.Fatal("pinned kills trade should match")
	}
	if got := readTradeFrame(t, client); got != pinnedFeedKillsSlug {
		t.Fatalf("kills trade routed to %q, want %q", got, pinnedFeedKillsSlug)
	}
}
