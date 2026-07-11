package live

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Catorpilor/poly/internal/config"
)

// The web trade protocol is {marketIndex, outcomeIndex}: Market Index picks
// the Moneyline market within the event (always 0 for 2-way events, 0-2 for
// 3-way soccer), Outcome Index picks the side within that market (0 or 1).
// See CONTEXT.md. The old protocol overloaded outcomeIndex to mean "which
// market" on 3-way events, which got it validated against the wrong axis
// and made soccer's third outcome untradeable.

func TestValidateWebTrade(t *testing.T) {
	t.Parallel()

	valid := webTradeData{
		EventSlug:    "epl-ars-che-2026-07-12",
		MarketIndex:  0,
		OutcomeIndex: 1,
		Side:         "BUY",
		Amount:       10,
	}

	tests := []struct {
		name    string
		mutate  func(*webTradeData)
		wantErr string // "" means valid
	}{
		{name: "valid buy", mutate: func(t *webTradeData) {}},
		{name: "lowercase side accepted", mutate: func(t *webTradeData) { t.Side = "buy" }},
		{
			name:    "sell rejected — web is buy-only",
			mutate:  func(t *webTradeData) { t.Side = "SELL" },
			wantErr: "Telegram",
		},
		{
			name:    "unknown side rejected",
			mutate:  func(t *webTradeData) { t.Side = "HOLD" },
			wantErr: "side",
		},
		{
			name:    "zero amount rejected",
			mutate:  func(t *webTradeData) { t.Amount = 0 },
			wantErr: "amount",
		},
		{
			name:    "negative amount rejected",
			mutate:  func(t *webTradeData) { t.Amount = -5 },
			wantErr: "amount",
		},
		{
			// Fat-finger guard, mirroring the UI input's max="1000".
			name:    "amount above server cap rejected",
			mutate:  func(t *webTradeData) { t.Amount = 1000.01 },
			wantErr: "amount",
		},
		{
			name:   "amount at the cap accepted",
			mutate: func(t *webTradeData) { t.Amount = 1000 },
		},
		{
			name:    "missing event slug rejected",
			mutate:  func(t *webTradeData) { t.EventSlug = "" },
			wantErr: "eventSlug",
		},
		{
			name:    "negative market index rejected",
			mutate:  func(t *webTradeData) { t.MarketIndex = -1 },
			wantErr: "marketIndex",
		},
		{
			name:    "negative outcome index rejected",
			mutate:  func(t *webTradeData) { t.OutcomeIndex = -1 },
			wantErr: "outcomeIndex",
		},
		{
			name:    "outcome index beyond binary market rejected",
			mutate:  func(t *webTradeData) { t.OutcomeIndex = 2 },
			wantErr: "outcomeIndex",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			trade := valid
			tt.mutate(&trade)

			err := validateWebTrade(trade)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("validateWebTrade(%+v) = %v, want nil", trade, err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("validateWebTrade(%+v) = %v, want error containing %q", trade, err, tt.wantErr)
			}
		})
	}
}

func TestResolveWebTrade(t *testing.T) {
	t.Parallel()

	twoWay := []*MarketInfo{
		{
			ID:           "m1",
			Question:     "Knicks vs. Pacers",
			Outcomes:     []string{"Knicks", "Pacers"},
			ClobTokenIds: []string{"tok-knicks", "tok-pacers"},
		},
	}
	threeWay := []*MarketInfo{
		{ID: "m-home", Question: "Will Real Madrid win?", Outcomes: []string{"Yes", "No"}, ClobTokenIds: []string{"h-yes", "h-no"}},
		{ID: "m-draw", Question: "Will it be a draw?", Outcomes: []string{"Yes", "No"}, ClobTokenIds: []string{"d-yes", "d-no"}},
		{ID: "m-away", Question: "Will Rayo win?", Outcomes: []string{"Yes", "No"}, ClobTokenIds: []string{"a-yes", "a-no"}},
	}

	tests := []struct {
		name         string
		markets      []*MarketInfo
		marketIndex  int
		outcomeIndex int
		wantMarketID string
		wantTokenID  string
		wantOutcome  string
		wantErr      string
	}{
		{
			name:    "2-way first outcome",
			markets: twoWay, marketIndex: 0, outcomeIndex: 0,
			wantMarketID: "m1", wantTokenID: "tok-knicks", wantOutcome: "Knicks",
		},
		{
			name:    "2-way second outcome",
			markets: twoWay, marketIndex: 0, outcomeIndex: 1,
			wantMarketID: "m1", wantTokenID: "tok-pacers", wantOutcome: "Pacers",
		},
		{
			// F2 regression: the third soccer market must be tradeable.
			name:    "3-way third market Yes",
			markets: threeWay, marketIndex: 2, outcomeIndex: 0,
			wantMarketID: "m-away", wantTokenID: "a-yes", wantOutcome: "Yes",
		},
		{
			name:    "3-way draw No leg",
			markets: threeWay, marketIndex: 1, outcomeIndex: 1,
			wantMarketID: "m-draw", wantTokenID: "d-no", wantOutcome: "No",
		},
		{
			name:    "market index beyond event's markets",
			markets: twoWay, marketIndex: 1, outcomeIndex: 0,
			wantErr: "marketIndex",
		},
		{
			name:    "negative market index",
			markets: twoWay, marketIndex: -1, outcomeIndex: 0,
			wantErr: "marketIndex",
		},
		{
			name:    "outcome index beyond market's tokens",
			markets: twoWay, marketIndex: 0, outcomeIndex: 2,
			wantErr: "outcomeIndex",
		},
		{
			name:    "negative outcome index",
			markets: twoWay, marketIndex: 0, outcomeIndex: -1,
			wantErr: "outcomeIndex",
		},
		{
			name:    "no moneyline markets",
			markets: nil, marketIndex: 0, outcomeIndex: 0,
			wantErr: "no Moneyline markets",
		},
		{
			name: "market with missing token IDs",
			markets: []*MarketInfo{
				{ID: "m-broken", Question: "Broken", Outcomes: []string{"Yes", "No"}},
			},
			marketIndex: 0, outcomeIndex: 0,
			wantErr: "outcomeIndex",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			marketID, tokenID, outcome, err := resolveWebTrade(tt.markets, tt.marketIndex, tt.outcomeIndex)

			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("resolveWebTrade() err = %v, want containing %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("resolveWebTrade() unexpected error: %v", err)
			}
			if marketID != tt.wantMarketID || tokenID != tt.wantTokenID || outcome != tt.wantOutcome {
				t.Errorf("resolveWebTrade() = (%q, %q, %q), want (%q, %q, %q)",
					marketID, tokenID, outcome, tt.wantMarketID, tt.wantTokenID, tt.wantOutcome)
			}
		})
	}
}

// Request-shape errors must be reported as 400 before dependency checks, so
// a malformed trade never reads as "trading not configured".
func TestHandleTradeRejectsBadRequests(t *testing.T) {
	t.Parallel()

	cfg := &config.Config{}
	cfg.App.LiveWebURL = "http://localhost:8081"
	ws := NewWebServer(nil, 0, nil, cfg, nil, nil)
	handler := ws.httpServer.Handler

	tests := []struct {
		name       string
		body       string
		wantStatus int
		wantError  string
	}{
		{
			name:       "sell rejected with pointer to Telegram",
			body:       `{"trade":{"eventSlug":"epl-ars-che","marketIndex":0,"outcomeIndex":0,"side":"SELL","amount":10}}`,
			wantStatus: http.StatusBadRequest,
			wantError:  "Telegram",
		},
		{
			name:       "missing event slug rejected",
			body:       `{"trade":{"marketIndex":0,"outcomeIndex":0,"side":"BUY","amount":10}}`,
			wantStatus: http.StatusBadRequest,
			wantError:  "eventSlug",
		},
		{
			name:       "invalid JSON rejected",
			body:       `{not json`,
			wantStatus: http.StatusBadRequest,
			wantError:  "Invalid request body",
		},
		{
			// Valid shape passes validation and reaches the dependency
			// check, which reports 503 on this nil-dep test server.
			name:       "valid buy reaches dependency check",
			body:       `{"trade":{"eventSlug":"epl-ars-che","marketIndex":0,"outcomeIndex":0,"side":"BUY","amount":10}}`,
			wantStatus: http.StatusServiceUnavailable,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			req := httptest.NewRequest("POST", "http://localhost:8081/api/trade", strings.NewReader(tt.body))
			req.Header.Set("Content-Type", "application/json")
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)

			if rec.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d; body: %s", rec.Code, tt.wantStatus, rec.Body.String())
			}
			if tt.wantError != "" && !strings.Contains(rec.Body.String(), tt.wantError) {
				t.Errorf("body %q should contain %q", rec.Body.String(), tt.wantError)
			}
		})
	}
}
