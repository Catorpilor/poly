package live

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Catorpilor/poly/internal/config"
)

// resolveWebTradeBySlug addresses a sub-market by its stable slug (not an
// index into the full market list, which reorders). It searches all of the
// event's markets, rejects closed/inactive ones, and bounds-checks the
// outcome.
func TestResolveWebTradeBySlug(t *testing.T) {
	t.Parallel()

	markets := []MarketInfo{
		{ID: "ml", Slug: "lol-hle1-ly-2026-07-11", Question: "HLE vs. LY", OutcomesRaw: `["HLE","LY"]`, ClobTokenIdsRaw: `["ml-hle","ml-ly"]`, Active: true},
		{ID: "g1", Slug: "lol-hle1-ly-2026-07-11-game1", Question: "Game 1 Winner", OutcomesRaw: `["HLE","LY"]`, ClobTokenIdsRaw: `["g1-hle","g1-ly"]`, Active: true},
		{ID: "g0", Slug: "lol-hle1-ly-2026-07-11-game0", Question: "Game 0 Winner", OutcomesRaw: `["HLE","LY"]`, ClobTokenIdsRaw: `["g0-hle","g0-ly"]`, Active: true, Closed: true},
		{ID: "inact", Slug: "lol-hle1-ly-2026-07-11-fb", Question: "First Blood", OutcomesRaw: `["HLE","LY"]`, ClobTokenIdsRaw: `["fb-hle","fb-ly"]`, Active: false},
	}

	tests := []struct {
		name         string
		slug         string
		outcomeIndex int
		wantMarketID string
		wantTokenID  string
		wantOutcome  string
		wantErr      string
	}{
		{
			name: "sub-market first outcome", slug: "lol-hle1-ly-2026-07-11-game1", outcomeIndex: 0,
			wantMarketID: "g1", wantTokenID: "g1-hle", wantOutcome: "HLE",
		},
		{
			name: "sub-market second outcome", slug: "lol-hle1-ly-2026-07-11-game1", outcomeIndex: 1,
			wantMarketID: "g1", wantTokenID: "g1-ly", wantOutcome: "LY",
		},
		{
			// A slug can also name the ML market — the endpoint lists only
			// sub-markets, but resolution itself is over all markets.
			name: "ML by slug", slug: "lol-hle1-ly-2026-07-11", outcomeIndex: 0,
			wantMarketID: "ml", wantTokenID: "ml-hle", wantOutcome: "HLE",
		},
		{name: "unknown slug", slug: "nope", outcomeIndex: 0, wantErr: "not found"},
		{name: "closed market rejected", slug: "lol-hle1-ly-2026-07-11-game0", outcomeIndex: 0, wantErr: "closed"},
		{name: "inactive market rejected", slug: "lol-hle1-ly-2026-07-11-fb", outcomeIndex: 0, wantErr: "not active"},
		{name: "outcome out of range", slug: "lol-hle1-ly-2026-07-11-game1", outcomeIndex: 2, wantErr: "outcomeIndex"},
		{name: "negative outcome", slug: "lol-hle1-ly-2026-07-11-game1", outcomeIndex: -1, wantErr: "outcomeIndex"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			marketID, tokenID, outcome, err := resolveWebTradeBySlug(markets, tt.slug, tt.outcomeIndex)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want containing %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if marketID != tt.wantMarketID || tokenID != tt.wantTokenID || outcome != tt.wantOutcome {
				t.Errorf("= (%q,%q,%q), want (%q,%q,%q)", marketID, tokenID, outcome, tt.wantMarketID, tt.wantTokenID, tt.wantOutcome)
			}
		})
	}
}

// fakeResolverServer builds an EventSlugResolver pointed at a fake Gamma
// endpoint that serves one event (or 404s for any other slug).
func fakeResolverServer(t *testing.T, wantSlug string, markets []MarketInfo) *EventSlugResolver {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("slug") != wantSlug {
			json.NewEncoder(w).Encode([]EventInfo{})
			return
		}
		json.NewEncoder(w).Encode([]EventInfo{{ID: "e", Slug: wantSlug, Title: "HLE vs LY", Markets: markets}})
	}))
	t.Cleanup(srv.Close)

	r := NewEventSlugResolver()
	r.gammaAPIURL = srv.URL
	return r
}

func newSubmarketTestServer(t *testing.T, r *EventSlugResolver) http.Handler {
	t.Helper()
	m := &LiveTradeManager{subscriptions: NewSubscriptionRegistry(), resolver: r, formatter: NewTradeFormatter()}
	cfg := &config.Config{}
	cfg.App.LiveWebURL = "http://localhost:8081"
	return NewWebServer(m, 0, nil, cfg, nil, nil).httpServer.Handler
}

func TestListEventMarketsEndpoint(t *testing.T) {
	t.Parallel()

	markets := []MarketInfo{
		{ID: "ml", Slug: "lol-hle1-ly-2026-07-11", Question: "HLE vs. LY", OutcomesRaw: `["HLE","LY"]`, OutcomePricesRaw: `["0.6","0.4"]`, Active: true},
		{ID: "g1", Slug: "lol-hle1-ly-2026-07-11-game1", Question: "Game 1 Winner", OutcomesRaw: `["HLE","LY"]`, OutcomePricesRaw: `["0.55","0.46"]`, Active: true},
		{ID: "g0", Slug: "lol-hle1-ly-2026-07-11-game0", Question: "Game 0 Winner", OutcomesRaw: `["HLE","LY"]`, Active: true, Closed: true},
	}
	handler := newSubmarketTestServer(t, fakeResolverServer(t, "lol-hle1-ly-2026-07-11", markets))

	t.Run("lists only active sub-markets with prices", func(t *testing.T) {
		req := httptest.NewRequest("GET", "http://localhost:8081/api/events/lol-hle1-ly-2026-07-11/markets", nil)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200; body: %s", rec.Code, rec.Body.String())
		}
		var resp struct {
			Event   string `json:"event"`
			Markets []struct {
				Slug     string   `json:"slug"`
				Question string   `json:"question"`
				Outcomes []string `json:"outcomes"`
				Prices   []string `json:"prices"`
			} `json:"markets"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if len(resp.Markets) != 1 || resp.Markets[0].Slug != "lol-hle1-ly-2026-07-11-game1" {
			t.Fatalf("markets = %+v, want only game1 (ML and closed excluded)", resp.Markets)
		}
		if len(resp.Markets[0].Prices) != 2 || resp.Markets[0].Prices[0] != "0.55" {
			t.Errorf("prices = %v, want indicative [0.55 0.46]", resp.Markets[0].Prices)
		}
	})

	t.Run("unknown event is 404", func(t *testing.T) {
		req := httptest.NewRequest("GET", "http://localhost:8081/api/events/no-such-event/markets", nil)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusNotFound {
			t.Errorf("status = %d, want 404; body: %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("bad origin is rejected", func(t *testing.T) {
		req := httptest.NewRequest("GET", "http://192.168.1.5:8081/api/events/lol-hle1-ly-2026-07-11/markets", nil)
		req.Header.Set("Origin", "https://evil.example")
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Errorf("status = %d, want 403 (endpoint must share guardAPI)", rec.Code)
		}
	})
}

// A trade carrying marketSlug resolves over all event markets, not the ML
// list — so a sub-market buy reaches the executor (503 here on nil deps,
// proving it passed validation and resolution).
func TestHandleTradeBySlugReachesExecutor(t *testing.T) {
	t.Parallel()

	markets := []MarketInfo{
		{ID: "ml", Slug: "lol-hle1-ly-2026-07-11", Question: "HLE vs. LY", OutcomesRaw: `["HLE","LY"]`, ClobTokenIdsRaw: `["ml-hle","ml-ly"]`, Active: true},
		{ID: "g1", Slug: "lol-hle1-ly-2026-07-11-game1", Question: "Game 1 Winner", OutcomesRaw: `["HLE","LY"]`, ClobTokenIdsRaw: `["g1-hle","g1-ly"]`, Active: true},
	}
	handler := newSubmarketTestServer(t, fakeResolverServer(t, "lol-hle1-ly-2026-07-11", markets))

	body := `{"trade":{"eventSlug":"lol-hle1-ly-2026-07-11","marketSlug":"lol-hle1-ly-2026-07-11-game1","outcomeIndex":1,"side":"BUY","amount":10}}`
	req := httptest.NewRequest("POST", "http://localhost:8081/api/trade", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	// nil tradingClient/walletManager → 503 after validation+resolution.
	// A 400 would mean the slug failed to resolve.
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503 (validation+resolution passed); body: %s", rec.Code, rec.Body.String())
	}
}
