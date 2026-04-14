package polymarket

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

// nbaMinBosEvent is a realistic Gamma API response for a NBA game event
// with multiple sub-markets (moneyline, spread, total points, player props, etc.)
var nbaMinBosEvent = []*GammaEventDetail{
	{
		ID:    "evt-9f3a7b2c-1d4e-4f5a-8b6c-2e7d8f9a0b1c",
		Slug:  "nba-min-bos-2026-03-22",
		Title: "NBA - Timberwolves vs Celtics",
		Markets: []*GammaMarket{
			{
				ID:               "500001",
				Question:         "Timberwolves vs Celtics",
				ConditionID:      "0xabc1230000000000000000000000000000000000000000000000000000000001",
				Slug:             "nba-min-bos-2026-03-22-winner",
				EndDate:          "2026-03-23T05:00:00Z",
				OutcomesRaw:      `["Timberwolves", "Celtics"]`,
				OutcomePricesRaw: `["0.42", "0.58"]`,
				Volume:           1850000,
				Volume24hr:       320000,
				Liquidity:        95000,
				Active:           true,
				Closed:           false,
				AcceptingOrders:  true,
				NegRisk:          true,
				NegRiskMarketID:  "0xdeadbeef01",
			},
			{
				ID:               "500002",
				Question:         "Spread: Timberwolves +6.5",
				ConditionID:      "0xabc1230000000000000000000000000000000000000000000000000000000002",
				Slug:             "nba-min-bos-2026-03-22-spread-6-5",
				EndDate:          "2026-03-23T05:00:00Z",
				OutcomesRaw:      `["Yes", "No"]`,
				OutcomePricesRaw: `["0.53", "0.47"]`,
				Volume:           720000,
				Volume24hr:       145000,
				Liquidity:        42000,
				Active:           true,
				Closed:           false,
				AcceptingOrders:  true,
				NegRisk:          false,
			},
			{
				ID:               "500003",
				Question:         "Total Points O/U 218.5",
				ConditionID:      "0xabc1230000000000000000000000000000000000000000000000000000000003",
				Slug:             "nba-min-bos-2026-03-22-total-218-5",
				EndDate:          "2026-03-23T05:00:00Z",
				OutcomesRaw:      `["Over", "Under"]`,
				OutcomePricesRaw: `["0.51", "0.49"]`,
				Volume:           580000,
				Volume24hr:       98000,
				Liquidity:        38000,
				Active:           true,
				Closed:           false,
				AcceptingOrders:  true,
				NegRisk:          false,
			},
			{
				ID:               "500004",
				Question:         "Anthony Edwards 25+ Points",
				ConditionID:      "0xabc1230000000000000000000000000000000000000000000000000000000004",
				Slug:             "nba-min-bos-2026-03-22-edwards-25-pts",
				EndDate:          "2026-03-23T05:00:00Z",
				OutcomesRaw:      `["Yes", "No"]`,
				OutcomePricesRaw: `["0.61", "0.39"]`,
				Volume:           210000,
				Volume24hr:       45000,
				Liquidity:        18000,
				Active:           true,
				Closed:           false,
				AcceptingOrders:  true,
				NegRisk:          false,
			},
			{
				ID:               "500005",
				Question:         "Jayson Tatum 30+ Points",
				ConditionID:      "0xabc1230000000000000000000000000000000000000000000000000000000005",
				Slug:             "nba-min-bos-2026-03-22-tatum-30-pts",
				EndDate:          "2026-03-23T05:00:00Z",
				OutcomesRaw:      `["Yes", "No"]`,
				OutcomePricesRaw: `["0.35", "0.65"]`,
				Volume:           190000,
				Volume24hr:       38000,
				Liquidity:        15000,
				Active:           true,
				Closed:           false,
				AcceptingOrders:  true,
				NegRisk:          false,
			},
			{
				ID:               "500006",
				Question:         "1Q Winner",
				ConditionID:      "0xabc1230000000000000000000000000000000000000000000000000000000006",
				Slug:             "nba-min-bos-2026-03-22-1q-winner",
				EndDate:          "2026-03-23T05:00:00Z",
				OutcomesRaw:      `["Timberwolves", "Celtics"]`,
				OutcomePricesRaw: `["0.40", "0.60"]`,
				Volume:           95000,
				Volume24hr:       22000,
				Liquidity:        12000,
				Active:           true,
				Closed:           false,
				AcceptingOrders:  true,
				NegRisk:          false,
			},
			{
				ID:               "500007",
				Question:         "1H Spread: Timberwolves +3.5",
				ConditionID:      "0xabc1230000000000000000000000000000000000000000000000000000000007",
				Slug:             "nba-min-bos-2026-03-22-1h-spread-3-5",
				EndDate:          "2026-03-23T05:00:00Z",
				OutcomesRaw:      `["Yes", "No"]`,
				OutcomePricesRaw: `["0.55", "0.45"]`,
				Volume:           68000,
				Volume24hr:       14000,
				Liquidity:        8000,
				Active:           true,
				Closed:           false,
				AcceptingOrders:  false, // paused market
				NegRisk:          false,
			},
			{
				ID:               "500008",
				Question:         "Halftime Lead: Timberwolves or Celtics",
				ConditionID:      "0xabc1230000000000000000000000000000000000000000000000000000000008",
				Slug:             "nba-min-bos-2026-03-22-halftime-lead",
				EndDate:          "2026-03-23T05:00:00Z",
				OutcomesRaw:      `["Timberwolves", "Celtics"]`,
				OutcomePricesRaw: `["0.38", "0.62"]`,
				Volume:           42000,
				Volume24hr:       0,
				Liquidity:        5000,
				Active:           false,
				Closed:           true, // closed market
				AcceptingOrders:  false,
				NegRisk:          false,
			},
		},
	},
}

// newNBAEventServer creates a test HTTP server that serves the NBA event fixture.
// It validates request path and query params.
func newNBAEventServer(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/events" {
			t.Errorf("unexpected path: %s, want /events", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		slug := r.URL.Query().Get("slug")
		if slug != "nba-min-bos-2026-03-22" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`[]`)) // empty for unknown slugs
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(nbaMinBosEvent)
	}))
}

func TestEventDetection_EndToEnd(t *testing.T) {
	t.Parallel()

	server := newNBAEventServer(t)
	t.Cleanup(server.Close)

	mc := NewMarketClientWithURL(server.URL)

	t.Run("URL_to_slug_to_event", func(t *testing.T) {
		t.Parallel()

		rawURL := "https://polymarket.com/sports/nba/nba-min-bos-2026-03-22"
		slug, ok := ParseEventSlug(rawURL)
		if !ok {
			t.Fatalf("ParseEventSlug(%q) returned ok=false", rawURL)
		}
		if slug != "nba-min-bos-2026-03-22" {
			t.Fatalf("slug = %q, want %q", slug, "nba-min-bos-2026-03-22")
		}

		event, err := mc.GetEventBySlug(context.Background(), slug)
		if err != nil {
			t.Fatalf("GetEventBySlug(%q): %v", slug, err)
		}

		if event.Title != "NBA - Timberwolves vs Celtics" {
			t.Errorf("event title = %q, want %q", event.Title, "NBA - Timberwolves vs Celtics")
		}
		if len(event.Markets) != 8 {
			t.Fatalf("len(markets) = %d, want 8", len(event.Markets))
		}
	})

	t.Run("all_sub_markets_parsed_correctly", func(t *testing.T) {
		t.Parallel()

		event, err := mc.GetEventBySlug(context.Background(), "nba-min-bos-2026-03-22")
		if err != nil {
			t.Fatalf("GetEventBySlug: %v", err)
		}

		wantMarkets := []struct {
			id       string
			question string
			outcomes []string
			prices   []string
		}{
			{"500001", "Timberwolves vs Celtics", []string{"Timberwolves", "Celtics"}, []string{"0.42", "0.58"}},
			{"500002", "Spread: Timberwolves +6.5", []string{"Yes", "No"}, []string{"0.53", "0.47"}},
			{"500003", "Total Points O/U 218.5", []string{"Over", "Under"}, []string{"0.51", "0.49"}},
			{"500004", "Anthony Edwards 25+ Points", []string{"Yes", "No"}, []string{"0.61", "0.39"}},
			{"500005", "Jayson Tatum 30+ Points", []string{"Yes", "No"}, []string{"0.35", "0.65"}},
			{"500006", "1Q Winner", []string{"Timberwolves", "Celtics"}, []string{"0.40", "0.60"}},
			{"500007", "1H Spread: Timberwolves +3.5", []string{"Yes", "No"}, []string{"0.55", "0.45"}},
			{"500008", "Halftime Lead: Timberwolves or Celtics", []string{"Timberwolves", "Celtics"}, []string{"0.38", "0.62"}},
		}

		for i, want := range wantMarkets {
			m := event.Markets[i]
			if m.ID != want.id {
				t.Errorf("market[%d] ID = %q, want %q", i, m.ID, want.id)
			}
			if m.Question != want.question {
				t.Errorf("market[%d] question = %q, want %q", i, m.Question, want.question)
			}
			outcomes := m.GetOutcomes()
			if len(outcomes) != len(want.outcomes) {
				t.Errorf("market[%d] len(outcomes) = %d, want %d", i, len(outcomes), len(want.outcomes))
				continue
			}
			for j := range outcomes {
				if outcomes[j] != want.outcomes[j] {
					t.Errorf("market[%d] outcome[%d] = %q, want %q", i, j, outcomes[j], want.outcomes[j])
				}
			}
			prices := m.GetOutcomePrices()
			if len(prices) != len(want.prices) {
				t.Errorf("market[%d] len(prices) = %d, want %d", i, len(prices), len(want.prices))
				continue
			}
			for j := range prices {
				if prices[j] != want.prices[j] {
					t.Errorf("market[%d] price[%d] = %q, want %q", i, j, prices[j], want.prices[j])
				}
			}
		}
	})

	t.Run("market_states", func(t *testing.T) {
		t.Parallel()

		event, err := mc.GetEventBySlug(context.Background(), "nba-min-bos-2026-03-22")
		if err != nil {
			t.Fatalf("GetEventBySlug: %v", err)
		}

		tests := []struct {
			index           int
			question        string
			active          bool
			closed          bool
			acceptingOrders bool
		}{
			{0, "Timberwolves vs Celtics", true, false, true},    // active moneyline
			{1, "Spread: Timberwolves +6.5", true, false, true},  // active spread
			{2, "Total Points O/U 218.5", true, false, true},     // active total
			{3, "Anthony Edwards 25+ Points", true, false, true}, // active player prop
			{4, "Jayson Tatum 30+ Points", true, false, true},    // active player prop
			{5, "1Q Winner", true, false, true},                   // active quarter market
			{6, "1H Spread: Timberwolves +3.5", true, false, false}, // paused (not accepting orders)
			{7, "Halftime Lead: Timberwolves or Celtics", false, true, false}, // closed
		}

		for _, tt := range tests {
			m := event.Markets[tt.index]
			if m.Question != tt.question {
				t.Errorf("market[%d] question = %q, want %q", tt.index, m.Question, tt.question)
			}
			if m.Active != tt.active {
				t.Errorf("market[%d] %q active = %v, want %v", tt.index, tt.question, m.Active, tt.active)
			}
			if m.Closed != tt.closed {
				t.Errorf("market[%d] %q closed = %v, want %v", tt.index, tt.question, m.Closed, tt.closed)
			}
			if m.AcceptingOrders != tt.acceptingOrders {
				t.Errorf("market[%d] %q acceptingOrders = %v, want %v", tt.index, tt.question, m.AcceptingOrders, tt.acceptingOrders)
			}
		}
	})

	t.Run("tradeable_markets_filter", func(t *testing.T) {
		t.Parallel()

		event, err := mc.GetEventBySlug(context.Background(), "nba-min-bos-2026-03-22")
		if err != nil {
			t.Fatalf("GetEventBySlug: %v", err)
		}

		// Count markets that should have trade buttons (not closed AND accepting orders)
		var tradeable int
		for _, m := range event.Markets {
			if !m.Closed && m.AcceptingOrders {
				tradeable++
			}
		}

		// 6 active + accepting, 1 paused (not accepting), 1 closed = 6 tradeable
		if tradeable != 6 {
			t.Errorf("tradeable markets = %d, want 6", tradeable)
		}
	})

	t.Run("neg_risk_market", func(t *testing.T) {
		t.Parallel()

		event, err := mc.GetEventBySlug(context.Background(), "nba-min-bos-2026-03-22")
		if err != nil {
			t.Fatalf("GetEventBySlug: %v", err)
		}

		ml := event.Markets[0] // moneyline is typically neg risk
		if !ml.NegRisk {
			t.Error("moneyline market NegRisk = false, want true")
		}
		if ml.NegRiskMarketID != "0xdeadbeef01" {
			t.Errorf("moneyline NegRiskMarketID = %q, want %q", ml.NegRiskMarketID, "0xdeadbeef01")
		}

		// Spread should NOT be neg risk
		spread := event.Markets[1]
		if spread.NegRisk {
			t.Error("spread market NegRisk = true, want false")
		}
	})

	t.Run("volume_and_liquidity", func(t *testing.T) {
		t.Parallel()

		event, err := mc.GetEventBySlug(context.Background(), "nba-min-bos-2026-03-22")
		if err != nil {
			t.Fatalf("GetEventBySlug: %v", err)
		}

		// Moneyline should have the highest volume
		ml := event.Markets[0]
		if ml.Volume != 1850000 {
			t.Errorf("moneyline total volume = %f, want 1850000", ml.Volume)
		}
		if ml.Volume24hr != 320000 {
			t.Errorf("moneyline 24h volume = %f, want 320000", ml.Volume24hr)
		}
		if ml.Liquidity != 95000 {
			t.Errorf("moneyline liquidity = %f, want 95000", ml.Liquidity)
		}

		// Verify FormatVolume for display
		if got := FormatVolume(ml.Volume); got != "$1.9M" {
			t.Errorf("FormatVolume(1850000) = %q, want %q", got, "$1.9M")
		}
		if got := FormatVolume(ml.Volume24hr); got != "$320.0K" {
			t.Errorf("FormatVolume(320000) = %q, want %q", got, "$320.0K")
		}
	})

	t.Run("condition_ids_unique", func(t *testing.T) {
		t.Parallel()

		event, err := mc.GetEventBySlug(context.Background(), "nba-min-bos-2026-03-22")
		if err != nil {
			t.Fatalf("GetEventBySlug: %v", err)
		}

		seen := make(map[string]bool)
		for _, m := range event.Markets {
			if seen[m.ConditionID] {
				t.Errorf("duplicate conditionID: %s (market %s)", m.ConditionID, m.Question)
			}
			seen[m.ConditionID] = true
		}
	})

	t.Run("market_ids_unique", func(t *testing.T) {
		t.Parallel()

		event, err := mc.GetEventBySlug(context.Background(), "nba-min-bos-2026-03-22")
		if err != nil {
			t.Fatalf("GetEventBySlug: %v", err)
		}

		seen := make(map[string]bool)
		for _, m := range event.Markets {
			if seen[m.ID] {
				t.Errorf("duplicate market ID: %s (market %s)", m.ID, m.Question)
			}
			seen[m.ID] = true
		}
	})
}

func TestEventDetection_URLVariants(t *testing.T) {
	t.Parallel()

	server := newNBAEventServer(t)
	t.Cleanup(server.Close)

	mc := NewMarketClientWithURL(server.URL)

	urls := []struct {
		name string
		url  string
	}{
		{"standard sports URL", "https://polymarket.com/sports/nba/nba-min-bos-2026-03-22"},
		{"www prefix", "https://www.polymarket.com/sports/nba/nba-min-bos-2026-03-22"},
		{"trailing slash", "https://polymarket.com/sports/nba/nba-min-bos-2026-03-22/"},
		{"query params", "https://polymarket.com/sports/nba/nba-min-bos-2026-03-22?tab=markets"},
		{"http scheme", "http://polymarket.com/sports/nba/nba-min-bos-2026-03-22"},
	}

	for _, tt := range urls {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			slug, ok := ParseEventSlug(tt.url)
			if !ok {
				t.Fatalf("ParseEventSlug(%q) returned ok=false", tt.url)
			}
			if slug != "nba-min-bos-2026-03-22" {
				t.Fatalf("slug = %q, want %q", slug, "nba-min-bos-2026-03-22")
			}

			event, err := mc.GetEventBySlug(context.Background(), slug)
			if err != nil {
				t.Fatalf("GetEventBySlug(%q): %v", slug, err)
			}
			if len(event.Markets) != 8 {
				t.Errorf("len(markets) = %d, want 8", len(event.Markets))
			}
		})
	}
}

func TestEventDetection_ErrorCases(t *testing.T) {
	t.Parallel()

	t.Run("unknown_slug_returns_error", func(t *testing.T) {
		t.Parallel()

		server := newNBAEventServer(t)
		t.Cleanup(server.Close)

		mc := NewMarketClientWithURL(server.URL)
		_, err := mc.GetEventBySlug(context.Background(), "nba-nonexistent-game")
		if err == nil {
			t.Fatal("expected error for unknown slug, got nil")
		}
	})

	t.Run("server_404", func(t *testing.T) {
		t.Parallel()

		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusNotFound)
		}))
		t.Cleanup(server.Close)

		mc := NewMarketClientWithURL(server.URL)
		_, err := mc.GetEventBySlug(context.Background(), "nba-min-bos-2026-03-22")
		if err == nil {
			t.Fatal("expected error for 404, got nil")
		}
	})

	t.Run("server_500", func(t *testing.T) {
		t.Parallel()

		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
		}))
		t.Cleanup(server.Close)

		mc := NewMarketClientWithURL(server.URL)
		_, err := mc.GetEventBySlug(context.Background(), "nba-min-bos-2026-03-22")
		if err == nil {
			t.Fatal("expected error for 500, got nil")
		}
	})

	t.Run("malformed_json", func(t *testing.T) {
		t.Parallel()

		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{not valid json`))
		}))
		t.Cleanup(server.Close)

		mc := NewMarketClientWithURL(server.URL)
		_, err := mc.GetEventBySlug(context.Background(), "nba-min-bos-2026-03-22")
		if err == nil {
			t.Fatal("expected error for malformed JSON, got nil")
		}
	})

	t.Run("empty_array_response", func(t *testing.T) {
		t.Parallel()

		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`[]`))
		}))
		t.Cleanup(server.Close)

		mc := NewMarketClientWithURL(server.URL)
		_, err := mc.GetEventBySlug(context.Background(), "nba-min-bos-2026-03-22")
		if err == nil {
			t.Fatal("expected error for empty array, got nil")
		}
	})

	t.Run("cancelled_context", func(t *testing.T) {
		t.Parallel()

		server := newNBAEventServer(t)
		t.Cleanup(server.Close)

		mc := NewMarketClientWithURL(server.URL)
		ctx, cancel := context.WithCancel(context.Background())
		cancel() // cancel immediately

		_, err := mc.GetEventBySlug(ctx, "nba-min-bos-2026-03-22")
		if err == nil {
			t.Fatal("expected error for cancelled context, got nil")
		}
	})

	t.Run("invalid_url_not_polymarket", func(t *testing.T) {
		t.Parallel()

		_, ok := ParseEventSlug("https://example.com/sports/nba/nba-min-bos-2026-03-22")
		if ok {
			t.Error("expected ok=false for non-polymarket URL")
		}
	})

	t.Run("bare_slug_not_detected_as_url", func(t *testing.T) {
		t.Parallel()

		_, ok := ParseEventSlug("nba-min-bos-2026-03-22")
		if ok {
			t.Error("expected ok=false for bare slug")
		}
	})
}

func TestEventDetection_MarketOutcomeParsing(t *testing.T) {
	t.Parallel()

	server := newNBAEventServer(t)
	t.Cleanup(server.Close)

	mc := NewMarketClientWithURL(server.URL)
	event, err := mc.GetEventBySlug(context.Background(), "nba-min-bos-2026-03-22")
	if err != nil {
		t.Fatalf("GetEventBySlug: %v", err)
	}

	t.Run("team_name_outcomes", func(t *testing.T) {
		t.Parallel()

		ml := event.Markets[0]
		outcomes := ml.GetOutcomes()
		if outcomes[0] != "Timberwolves" || outcomes[1] != "Celtics" {
			t.Errorf("moneyline outcomes = %v, want [Timberwolves Celtics]", outcomes)
		}
		prices := ml.GetOutcomePrices()
		if prices[0] != "0.42" || prices[1] != "0.58" {
			t.Errorf("moneyline prices = %v, want [0.42 0.58]", prices)
		}
	})

	t.Run("yes_no_outcomes", func(t *testing.T) {
		t.Parallel()

		spread := event.Markets[1]
		outcomes := spread.GetOutcomes()
		if outcomes[0] != "Yes" || outcomes[1] != "No" {
			t.Errorf("spread outcomes = %v, want [Yes No]", outcomes)
		}
	})

	t.Run("over_under_outcomes", func(t *testing.T) {
		t.Parallel()

		total := event.Markets[2]
		outcomes := total.GetOutcomes()
		if outcomes[0] != "Over" || outcomes[1] != "Under" {
			t.Errorf("total outcomes = %v, want [Over Under]", outcomes)
		}
	})

	t.Run("price_formatting", func(t *testing.T) {
		t.Parallel()

		tests := []struct {
			marketIdx int
			question  string
			wantFmt0  string
			wantFmt1  string
		}{
			{0, "moneyline", "42%", "58%"},
			{1, "spread", "53%", "47%"},
			{2, "total", "51%", "49%"},
			{3, "edwards props", "61%", "39%"},
			{4, "tatum props", "35%", "65%"},
		}

		for _, tt := range tests {
			m := event.Markets[tt.marketIdx]
			prices := m.GetOutcomePrices()
			var p0, p1 float64
			fmt.Sscanf(prices[0], "%f", &p0)
			fmt.Sscanf(prices[1], "%f", &p1)

			if got := FormatPrice(p0); got != tt.wantFmt0 {
				t.Errorf("%s: FormatPrice(%s) = %q, want %q", tt.question, prices[0], got, tt.wantFmt0)
			}
			if got := FormatPrice(p1); got != tt.wantFmt1 {
				t.Errorf("%s: FormatPrice(%s) = %q, want %q", tt.question, prices[1], got, tt.wantFmt1)
			}
		}
	})
}

func TestEventDetection_EventWithZeroMarkets(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`[{
			"id": "evt-empty",
			"slug": "nba-empty-event",
			"title": "Some Empty Event",
			"markets": []
		}]`))
	}))
	t.Cleanup(server.Close)

	mc := NewMarketClientWithURL(server.URL)
	event, err := mc.GetEventBySlug(context.Background(), "nba-empty-event")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(event.Markets) != 0 {
		t.Errorf("len(markets) = %d, want 0", len(event.Markets))
	}
}

func TestEventDetection_RequestValidation(t *testing.T) {
	t.Parallel()

	t.Run("sends_correct_query_param", func(t *testing.T) {
		t.Parallel()

		var receivedSlug string
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			receivedSlug = r.URL.Query().Get("slug")
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			json.NewEncoder(w).Encode(nbaMinBosEvent)
		}))
		t.Cleanup(server.Close)

		mc := NewMarketClientWithURL(server.URL)
		_, err := mc.GetEventBySlug(context.Background(), "nba-min-bos-2026-03-22")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		if receivedSlug != "nba-min-bos-2026-03-22" {
			t.Errorf("server received slug = %q, want %q", receivedSlug, "nba-min-bos-2026-03-22")
		}
	})

	t.Run("sends_GET_method", func(t *testing.T) {
		t.Parallel()

		var receivedMethod string
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			receivedMethod = r.Method
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			json.NewEncoder(w).Encode(nbaMinBosEvent)
		}))
		t.Cleanup(server.Close)

		mc := NewMarketClientWithURL(server.URL)
		mc.GetEventBySlug(context.Background(), "nba-min-bos-2026-03-22")

		if receivedMethod != http.MethodGet {
			t.Errorf("method = %q, want %q", receivedMethod, http.MethodGet)
		}
	})

	t.Run("sends_accept_json_header", func(t *testing.T) {
		t.Parallel()

		var receivedAccept string
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			receivedAccept = r.Header.Get("Accept")
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			json.NewEncoder(w).Encode(nbaMinBosEvent)
		}))
		t.Cleanup(server.Close)

		mc := NewMarketClientWithURL(server.URL)
		mc.GetEventBySlug(context.Background(), "nba-min-bos-2026-03-22")

		if receivedAccept != "application/json" {
			t.Errorf("Accept header = %q, want %q", receivedAccept, "application/json")
		}
	})

	t.Run("hits_events_path", func(t *testing.T) {
		t.Parallel()

		var receivedPath string
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			receivedPath = r.URL.Path
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			json.NewEncoder(w).Encode(nbaMinBosEvent)
		}))
		t.Cleanup(server.Close)

		mc := NewMarketClientWithURL(server.URL)
		mc.GetEventBySlug(context.Background(), "nba-min-bos-2026-03-22")

		if receivedPath != "/events" {
			t.Errorf("path = %q, want %q", receivedPath, "/events")
		}
	})
}

func TestGetFeeRateBps(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		market   GammaMarket
		wantBps  int
	}{
		{
			name: "Sports market (rate=0.03)",
			market: GammaMarket{
				FeeSchedule: &FeeSchedule{Rate: 0.03, Exponent: 1, TakerOnly: true, RebateRate: 0.25},
				FeeType:     "sports_fees_v2",
			},
			wantBps: 30,
		},
		{
			name: "Crypto market (rate=0.072)",
			market: GammaMarket{
				FeeSchedule: &FeeSchedule{Rate: 0.072, Exponent: 1, TakerOnly: true, RebateRate: 0.20},
				FeeType:     "crypto_fees_v2",
			},
			wantBps: 72,
		},
		{
			name:    "No feeSchedule (nil) = 0 bps",
			market:  GammaMarket{},
			wantBps: 0,
		},
		{
			name: "Zero rate = 0 bps",
			market: GammaMarket{
				FeeSchedule: &FeeSchedule{Rate: 0},
			},
			wantBps: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := tt.market.GetFeeRateBps()
			if got != tt.wantBps {
				t.Errorf("GetFeeRateBps() = %d, want %d", got, tt.wantBps)
			}
		})
	}
}
