package polymarket

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
)

// TestTradingClient_SellVWAP checks the depth-aware fire-confirm price source
// (issue #80): a FRESH order-book read, walked best-bid-first, returning the
// executable VWAP for a sell of N shares plus the total bid depth. Unlike
// GetBestPrice it does NOT error when the book can't cover the size — it returns
// the partial VWAP (an upper bound) so the confirm can apply its partial-depth
// rule. ok=false only when the bid side is empty.
func TestTradingClient_SellVWAP(t *testing.T) {
	t.Parallel()
	// Bids 0.52×20, 0.30×100, 0.20×500 (total depth 620). Deliberately
	// out-of-order to prove the walk sorts by price.
	book := OrderBook{Bids: []OrderBookEntry{
		{Price: 0.20, Size: 500}, {Price: 0.52, Size: 20}, {Price: 0.30, Size: 100},
	}}
	cases := []struct {
		name      string
		bids      []OrderBookEntry
		shares    float64
		wantVWAP  float64
		wantDepth float64
		wantOK    bool
	}{
		// 20@0.52 + 80@0.30 = 34.4 / 100 = 0.344 — well under the thin 0.52 top.
		{"phantom top into depth", book.Bids, 100, (0.52*20 + 0.30*80) / 100, 620, true},
		// Insufficient depth: consumes all 620, VWAP over the whole book, ok=true.
		{"insufficient depth", book.Bids, 700, (0.52*20 + 0.30*100 + 0.20*500) / 620, 620, true},
		{"empty book", nil, 100, 0, 0, false},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				json.NewEncoder(w).Encode(orderBookWire(tc.bids))
			}))
			defer srv.Close()
			tc2 := NewTradingClient(srv.URL, 137)
			vwap, depth, ok, err := tc2.SellVWAP(context.Background(), "tok", tc.shares)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tc.wantOK)
			}
			if math.Abs(vwap-tc.wantVWAP) > 1e-9 {
				t.Errorf("vwap = %v, want %v", vwap, tc.wantVWAP)
			}
			if math.Abs(depth-tc.wantDepth) > 1e-9 {
				t.Errorf("depth = %v, want %v", depth, tc.wantDepth)
			}
		})
	}
}

// TestTradingClient_SellVWAP_FetchError surfaces a transport/HTTP failure as an
// error (the caller fails open).
func TestTradingClient_SellVWAP_FetchError(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	tc := NewTradingClient(srv.URL, 137)
	if _, _, ok, err := tc.SellVWAP(context.Background(), "tok", 100); err == nil || ok {
		t.Fatalf("expected error and ok=false on HTTP 500, got ok=%v err=%v", ok, err)
	}
}

// orderBookWire serializes bids into the CLOB /book JSON shape (price/size are
// strings — matching OrderBookEntry's json:"...,string" tags).
func orderBookWire(bids []OrderBookEntry) map[string]any {
	out := make([]map[string]string, 0, len(bids))
	for _, b := range bids {
		out = append(out, map[string]string{
			"price": strconv.FormatFloat(b.Price, 'f', -1, 64),
			"size":  strconv.FormatFloat(b.Size, 'f', -1, 64),
		})
	}
	return map[string]any{"bids": out, "asks": []map[string]string{}}
}
