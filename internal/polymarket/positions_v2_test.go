package polymarket

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/ethereum/go-ethereum/common"
)

// writeV2Positions writes a Data API v2 page: {data: [...], pagination: {...}}.
// An empty nextCursor ends pagination (next_cursor: null).
func writeV2Positions(t *testing.T, w http.ResponseWriter, rows []DataAPIPosition, nextCursor string) {
	t.Helper()
	if rows == nil {
		rows = []DataAPIPosition{}
	}
	var cursor any
	if nextCursor != "" {
		cursor = nextCursor
	}
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(map[string]any{
		"data": rows,
		"pagination": map[string]any{
			"limit":       len(rows),
			"has_more":    nextCursor != "",
			"next_cursor": cursor,
		},
	}); err != nil {
		t.Errorf("encode v2 page: %v", err)
	}
}

// liveV2PositionsPage is a verbatim /v2/positions response (2026-10-22,
// trimmed to one row). It pins the real snake_case field names, so a struct
// tag regression fails here even though the other tests round-trip through
// DataAPIPosition itself.
const liveV2PositionsPage = `{"data":[{"proxy_wallet":"0x4b2f0b7b91319419d52f01caf7d73d453770318b","token_id":"112701874696106231959137275505703442097301509199703702432127541034893438217794","condition_id":"0x9fdd044a58c60de1adfc85582c31852b3f34ba680270f0967fde73daf97c92eb","current_size":184.0,"avg_price":0.0799,"entry_cost_usdc":14.7198,"entry_fees_usdc":0.55347,"total_cost_usdc":15.27327,"current_price":0.095,"current_value":17.48,"total_size":184.0,"realized_pnl":0.0,"unrealized_pnl":2.7602,"total_pnl":2.7602,"percent_pnl":18.75,"percent_realized_pnl":0.0,"status":"OPEN","redeemable":false,"mergeable":false,"negative_risk":true,"archived":false,"title":"Will Darius Acuff Jr. win the 2026-27 NBA Rookie of the Year?","slug":"will-darius-acuff-jrpt-win-the-2026-27-nba-rookie-of-the-year-20260624161716886","icon":"","event_id":"630058","event_slug":"nba-2026-27-rookie-of-the-year-20260624161716143","outcome":"Yes","outcome_index":0,"opposite_outcome":"No","opposite_token_id":"70302260522026652733199348037704033548729013125558756785003407774885182721354","end_date":"2027-05-01","last_event_at":1791290128,"first_entry_at":1791290128,"name":"huongf","profile_image":"","verified":false}],"pagination":{"limit":500,"offset":0,"has_more":false,"next_cursor":null}}`

func TestGetUserPositionsFromAPI_DecodesLiveV2Shape(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(liveV2PositionsPage))
	}))
	defer server.Close()

	pm := NewPositionManagerWithDataAPI(server.URL)
	positions, err := pm.GetUserPositionsFromAPI(context.Background(), common.HexToAddress("0x4b2f"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(positions) != 1 {
		t.Fatalf("expected 1 position, got %d", len(positions))
	}
	p := positions[0]
	checks := []struct {
		name      string
		got, want any
	}{
		{"TokenID", p.TokenID, "112701874696106231959137275505703442097301509199703702432127541034893438217794"},
		{"ConditionID", p.ConditionID, "0x9fdd044a58c60de1adfc85582c31852b3f34ba680270f0967fde73daf97c92eb"},
		{"MarketID", p.MarketID, "0x9fdd044a58c60de1adfc85582c31852b3f34ba680270f0967fde73daf97c92eb"},
		{"MarketTitle", p.MarketTitle, "Will Darius Acuff Jr. win the 2026-27 NBA Rookie of the Year?"},
		{"Outcome", p.Outcome, "Yes"},
		{"Shares", p.Shares.String(), "184000000"},
		{"AveragePrice", p.AveragePrice, 0.0799},
		{"CurrentPrice", p.CurrentPrice, 0.095},
		{"Value", p.Value, 17.48},
		{"PnL", p.PnL, 2.7602},
		{"PnLPercent", p.PnLPercent, 18.75},
		{"NegativeRisk", p.NegativeRisk, true},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("%s = %v, want %v", c.name, c.got, c.want)
		}
	}
}

func TestGetUserPositionsFromAPI_RequestShape(t *testing.T) {
	t.Parallel()

	var got *http.Request
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Clone(context.Background())
		writeV2Positions(t, w, nil, "")
	}))
	defer server.Close()

	pm := NewPositionManagerWithDataAPI(server.URL)
	if _, err := pm.GetUserPositionsFromAPI(context.Background(), common.HexToAddress("0xAbCdEf1234567890AbCdEf1234567890AbCdEf12")); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got == nil {
		t.Fatal("no request made")
	}
	if got.URL.Path != "/v2/positions" {
		t.Errorf("path = %q, want /v2/positions", got.URL.Path)
	}
	q := got.URL.Query()
	if q.Get("user") != "0xabcdef1234567890abcdef1234567890abcdef12" {
		t.Errorf("user = %q, want lowercased address", q.Get("user"))
	}
	// OPEN is v2's default and the superset that matches v1 /positions
	// (it includes settled-but-unredeemed winners).
	if s := q.Get("status"); s != "" && s != "OPEN" {
		t.Errorf("status = %q, want OPEN or omitted", s)
	}
	if q.Get("limit") == "" {
		t.Error("limit must be set to avoid the 100-row default page")
	}
	for _, banned := range []string{"offset", "redeemable", "market", "sizeThreshold"} {
		if q.Has(banned) {
			t.Errorf("obsolete v1 param %q must not be sent to v2", banned)
		}
	}
}

func TestGetUserPositionsFromAPI_FiltersResolvedLosers(t *testing.T) {
	t.Parallel()

	rows := []DataAPIPosition{
		{Title: "live", ConditionID: "0x1", Asset: "a", Size: 10, CurPrice: 0.4},
		{Title: "settled loser", ConditionID: "0x2", Asset: "b", Size: 5, CurPrice: 0},
		{Title: "settled winner", ConditionID: "0x3", Asset: "c", Size: 3, CurPrice: 1},
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeV2Positions(t, w, rows, "")
	}))
	defer server.Close()

	pm := NewPositionManagerWithDataAPI(server.URL)
	positions, err := pm.GetUserPositionsFromAPI(context.Background(), common.HexToAddress("0x1234"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var titles []string
	for _, p := range positions {
		titles = append(titles, p.MarketTitle)
	}
	if strings.Join(titles, ",") != "live,settled winner" {
		t.Errorf("titles = %v, want [live settled winner]", titles)
	}
}

func TestPositionsV2_FollowsCursorPagination(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		fetch func(*PositionManager) (int, error)
	}{
		{"open positions", func(pm *PositionManager) (int, error) {
			ps, err := pm.GetUserPositionsFromAPI(context.Background(), common.HexToAddress("0x1234"))
			return len(ps), err
		}},
		{"redeemable positions", func(pm *PositionManager) (int, error) {
			ps, err := pm.GetRedeemablePositions(context.Background(), common.HexToAddress("0x1234"))
			return len(ps), err
		}},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var mu sync.Mutex
			var seen []string // cursor of each request ("" = first page)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				q := r.URL.Query()
				mu.Lock()
				seen = append(seen, q.Get("cursor"))
				mu.Unlock()
				switch q.Get("cursor") {
				case "":
					writeV2Positions(t, w, []DataAPIPosition{{Title: "p1", ConditionID: "0x1", Asset: "a", Size: 1, CurPrice: 1}}, "page-2")
				case "page-2":
					if q.Has("offset") {
						t.Error("offset must never be sent")
					}
					writeV2Positions(t, w, []DataAPIPosition{{Title: "p2", ConditionID: "0x2", Asset: "b", Size: 1, CurPrice: 1}}, "")
				default:
					t.Errorf("unexpected cursor %q", q.Get("cursor"))
					writeV2Positions(t, w, nil, "")
				}
			}))
			defer server.Close()

			n, err := tt.fetch(NewPositionManagerWithDataAPI(server.URL))
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if n != 2 {
				t.Errorf("positions = %d, want 2 (one per page)", n)
			}
			mu.Lock()
			defer mu.Unlock()
			if strings.Join(seen, "|") != "|page-2" {
				t.Errorf("cursors requested = %q, want [\"\" \"page-2\"]", seen)
			}
		})
	}
}

func TestPositionsV2_StopsOnRunawayCursor(t *testing.T) {
	t.Parallel()

	// A server that always returns a next_cursor must not loop forever.
	var mu sync.Mutex
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		requests++
		mu.Unlock()
		writeV2Positions(t, w, []DataAPIPosition{{Title: "p", ConditionID: "0x1", Asset: "a", Size: 1, CurPrice: 1}}, "again")
	}))
	defer server.Close()

	pm := NewPositionManagerWithDataAPI(server.URL)
	if _, err := pm.GetUserPositionsFromAPI(context.Background(), common.HexToAddress("0x1234")); err == nil {
		t.Error("expected an error when pagination never ends")
	}
	mu.Lock()
	defer mu.Unlock()
	if requests > maxPositionPages {
		t.Errorf("requests = %d, want <= %d", requests, maxPositionPages)
	}
}

func TestPositionsV2_NullDataIsEmpty(t *testing.T) {
	t.Parallel()

	// v2 documents a miss as data: null, never an error.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":null,"pagination":{"has_more":false,"next_cursor":null}}`))
	}))
	defer server.Close()

	pm := NewPositionManagerWithDataAPI(server.URL)
	positions, err := pm.GetUserPositionsFromAPI(context.Background(), common.HexToAddress("0x1234"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(positions) != 0 {
		t.Errorf("positions = %d, want 0", len(positions))
	}
}
