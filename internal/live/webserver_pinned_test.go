package live

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

// Subscribing with a market slug must pin that market as the panel's
// primary trade target. Before this, pasting "…-game3" resolved to the
// parent event and the buy buttons silently traded the event's Moneyline —
// a mis-trade, not a UX nit.

func pinnedTestEvent() []MarketInfo {
	return []MarketInfo{
		{ID: "ml", Slug: "lol-blg-hle1-2026-07-12", Question: "BLG vs. HLE", OutcomesRaw: `["BLG","HLE"]`, Active: true},
		{ID: "g3", Slug: "lol-blg-hle1-2026-07-12-game3", Question: "BLG vs. HLE - Game 3 Winner", OutcomesRaw: `["Bilibili Gaming","Hanwha Life Esports"]`, Active: true},
		{ID: "g0", Slug: "lol-blg-hle1-2026-07-12-game0", Question: "BLG vs. HLE - Game 0 Winner", OutcomesRaw: `["BLG","HLE"]`, Active: true, Closed: true},
	}
}

func TestPinnedMarket(t *testing.T) {
	t.Parallel()

	resolver := NewEventSlugResolver()
	event := &EventInfo{ID: "e", Slug: "lol-blg-hle1-2026-07-12", Markets: pinnedTestEvent()}

	tests := []struct {
		name     string
		slug     string
		wantPin  string // market ID, "" = no pin
	}{
		{name: "sub-market slug pins that market", slug: "lol-blg-hle1-2026-07-12-game3", wantPin: "g3"},
		{name: "event slug does not pin", slug: "lol-blg-hle1-2026-07-12", wantPin: ""},
		{name: "closed market does not pin", slug: "lol-blg-hle1-2026-07-12-game0", wantPin: ""},
		{name: "unknown slug does not pin", slug: "something-else", wantPin: ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := pinnedMarket(resolver, event, tt.slug)
			if tt.wantPin == "" {
				if got != nil {
					t.Fatalf("pinnedMarket(%s) = %s, want nil", tt.slug, got.ID)
				}
				return
			}
			if got == nil || got.ID != tt.wantPin {
				t.Fatalf("pinnedMarket(%s) = %v, want market %s", tt.slug, got, tt.wantPin)
			}
		})
	}
}

// Note: the ML market's own slug equals the event slug in the fixture
// above (common on Polymarket); when they differ, pinning the ML market
// is harmless — the buttons then name the ML explicitly.

func newPinnedSubscribeFixture(t *testing.T) (*WebServer, *LiveTradeManager) {
	t.Helper()

	r := fakeResolverServer(t, "lol-blg-hle1-2026-07-12", pinnedTestEvent())
	m := &LiveTradeManager{
		subscriptions:     NewSubscriptionRegistry(),
		resolver:          r,
		formatter:         NewTradeFormatter(),
		assetToEvent:      make(map[string]string),
		assetToMarketName: make(map[string]string),
		subscribed:        true, // skip the RTDS dial in trackEventAssets
	}
	return NewWebServer(m, 0, nil, nil, nil, nil), m
}

func readWSResponse(t *testing.T, client interface {
	SetReadDeadline(time.Time) error
	ReadMessage() (int, []byte, error)
}) map[string]interface{} {
	t.Helper()
	client.SetReadDeadline(time.Now().Add(5 * time.Second))
	_, raw, err := client.ReadMessage()
	if err != nil {
		t.Fatalf("read ws response: %v", err)
	}
	var resp map[string]interface{}
	if err := json.Unmarshal(raw, &resp); err != nil {
		t.Fatalf("decode ws response: %v", err)
	}
	return resp
}

func TestSubscribeWithMarketSlugPinsMarket(t *testing.T) {
	t.Parallel()

	ws, m := newPinnedSubscribeFixture(t)
	server, client := newWSPair(t)
	m.RegisterWebConn(server)

	// The fake Gamma only knows the event slug, so resolve the market slug
	// through the same fallback production uses: prime the cache the way
	// GetEventInfo would after a /markets fallback.
	eventInfo, err := m.resolver.GetEventInfo(context.Background(), "lol-blg-hle1-2026-07-12")
	if err != nil {
		t.Fatalf("prime event: %v", err)
	}
	m.resolver.cacheEvent("lol-blg-hle1-2026-07-12-game3", eventInfo)

	ws.handleSubscribe(server, "lol-blg-hle1-2026-07-12-game3", false)
	resp := readWSResponse(t, client)

	if resp["type"] != "subscribed" {
		t.Fatalf("type = %v, want subscribed; resp: %v", resp["type"], resp)
	}
	if resp["market"] != "lol-blg-hle1-2026-07-12-game3" {
		t.Errorf("market = %v, want the pinned game-3 slug", resp["market"])
	}
	if resp["marketQuestion"] != "BLG vs. HLE - Game 3 Winner" {
		t.Errorf("marketQuestion = %v, want the game-3 question", resp["marketQuestion"])
	}
	outcomes, _ := resp["outcomes"].([]interface{})
	if len(outcomes) != 2 || outcomes[0] != "Bilibili Gaming" {
		t.Errorf("outcomes = %v, want the game-3 market's outcomes, not the ML's", outcomes)
	}
}

func TestSubscribeWithEventSlugDoesNotPin(t *testing.T) {
	t.Parallel()

	ws, m := newPinnedSubscribeFixture(t)
	server, client := newWSPair(t)
	m.RegisterWebConn(server)

	ws.handleSubscribe(server, "lol-blg-hle1-2026-07-12", false)
	resp := readWSResponse(t, client)

	if resp["type"] != "subscribed" {
		t.Fatalf("type = %v, want subscribed; resp: %v", resp["type"], resp)
	}
	if market, ok := resp["market"]; ok && market != "" {
		t.Errorf("market = %v, want absent for a plain event subscribe", market)
	}
	outcomes, _ := resp["outcomes"].([]interface{})
	if len(outcomes) != 2 || outcomes[0] != "BLG" {
		t.Errorf("outcomes = %v, want the ML outcomes", outcomes)
	}
}
