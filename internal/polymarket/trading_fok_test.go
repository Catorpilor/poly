package polymarket

import (
	"context"
	"crypto/ecdsa"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
)

// fokFakeCLOB is a CLOB stub for the FOK fill-confirmation path: it serves the
// order-build prerequisites (book, market tick), a configurable POST /order
// body, and a scripted sequence of GET /data/order/{id} poll responses.
type fokFakeCLOB struct {
	orderBody    string // raw JSON body returned by POST /order
	orderStatus  int    // HTTP status for POST /order; 0 = 200 OK
	ordersPlaced atomic.Int64

	// pollStatuses drives successive GET /data/order/{id} responses. Each
	// entry is a status string, except "404" (poll returns 404) and "gone"
	// (poll returns an empty object = order reaped). The final entry repeats.
	pollStatuses []string
	pollCount    atomic.Int64
	matchedSize  string // size_matched reported on a "matched" poll
	matchedPrice string // price reported on a "matched" poll
}

func (f *fokFakeCLOB) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/book":
			json.NewEncoder(w).Encode(map[string]any{
				"market": "cond-1",
				"asks":   []map[string]string{{"price": "0.50", "size": "1000"}},
				"bids":   []map[string]string{{"price": "0.34", "size": "1000"}},
			})
		case strings.HasPrefix(r.URL.Path, "/markets/"):
			json.NewEncoder(w).Encode(map[string]any{"minimum_tick_size": 0.01})
		case r.URL.Path == "/order":
			f.ordersPlaced.Add(1)
			if f.orderStatus != 0 {
				w.WriteHeader(f.orderStatus)
			}
			io.WriteString(w, f.orderBody)
		case strings.HasPrefix(r.URL.Path, "/data/order/"):
			n := int(f.pollCount.Add(1)) - 1
			if n >= len(f.pollStatuses) {
				n = len(f.pollStatuses) - 1
			}
			switch status := f.pollStatuses[n]; status {
			case "404":
				http.NotFound(w, r)
			case "gone":
				io.WriteString(w, "{}")
			default:
				json.NewEncoder(w).Encode(map[string]any{
					"id":           "ord-fok",
					"status":       status,
					"size_matched": f.matchedSize,
					"price":        f.matchedPrice,
				})
			}
		default:
			http.NotFound(w, r)
		}
	}
}

// newFOKFixture wires a TradingClient to the stub with shrunk poll timings so
// tests run in milliseconds. Credentials are supplied directly (the stub omits
// the credential endpoints, which ExecuteTrade doesn't touch).
func newFOKFixture(t *testing.T, clob *fokFakeCLOB) (*TradingClient, *ecdsa.PrivateKey, *APICredentials) {
	t.Helper()
	srv := httptest.NewServer(clob.handler())
	t.Cleanup(srv.Close)

	tc := NewTradingClient(srv.URL, 137)
	tc.fokPollInterval = 2 * time.Millisecond
	tc.fokConfirmTimeout = 60 * time.Millisecond

	key, err := crypto.GenerateKey()
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	creds := &APICredentials{
		APIKey: "test-key", Secret: "c2VjcmV0LXNlY3JldC1zZWNyZXQ=", Passphrase: "pass",
	}
	return tc, key, creds
}

func fokSellRequest() *TradeRequest {
	// 200 shares at 0.35 → maker 200000000 shares, taker 70000000 USDC.
	return &TradeRequest{
		MarketID:  "gm-1",
		TokenID:   "123456",
		Side:      "SELL",
		Outcome:   "Yes",
		SharesRaw: 200_000_000,
		Price:     0.35,
		OrderType: OrderTypeFOK,
	}
}

var fokProxy = common.HexToAddress("0x1111111111111111111111111111111111111111")

// TestFOKSubmitMatchedImmediately: a FOK that matches on submit is a confirmed
// fill, with FilledSize/AveragePrice derived from the response amounts.
func TestFOKSubmitMatchedImmediately(t *testing.T) {
	t.Parallel()
	clob := &fokFakeCLOB{
		orderBody: `{"success":true,"orderId":"ord-fok","status":"matched","takingAmount":"70000000","makingAmount":"200000000"}`,
	}
	tc, key, creds := newFOKFixture(t, clob)

	res, err := tc.ExecuteTrade(context.Background(), key, fokProxy, creds, fokSellRequest())
	if err != nil {
		t.Fatalf("ExecuteTrade: %v", err)
	}
	if !res.Success {
		t.Fatalf("Success = false, want true (matched): %+v", res)
	}
	if res.FilledSize != 200 {
		t.Errorf("FilledSize = %v, want 200", res.FilledSize)
	}
	if res.AveragePrice < 0.3499 || res.AveragePrice > 0.3501 {
		t.Errorf("AveragePrice = %v, want ~0.35", res.AveragePrice)
	}
	if clob.pollCount.Load() != 0 {
		t.Errorf("pollCount = %d, want 0 (immediate match must not poll)", clob.pollCount.Load())
	}
}

// TestFOKDelayedThenMatched: a delayed FOK polls until the order matches; fill
// fields come from the poll's size_matched/price.
func TestFOKDelayedThenMatched(t *testing.T) {
	t.Parallel()
	clob := &fokFakeCLOB{
		orderBody:    `{"success":true,"orderId":"ord-fok","status":"delayed"}`,
		pollStatuses: []string{"delayed", "delayed", "matched"},
		matchedSize:  "199.99",
		matchedPrice: "0.3610",
	}
	tc, key, creds := newFOKFixture(t, clob)

	res, err := tc.ExecuteTrade(context.Background(), key, fokProxy, creds, fokSellRequest())
	if err != nil {
		t.Fatalf("ExecuteTrade: %v", err)
	}
	if !res.Success {
		t.Fatalf("Success = false, want true (matched on poll): %+v", res)
	}
	if clob.pollCount.Load() != 3 {
		t.Errorf("pollCount = %d, want 3 (matched on 3rd poll)", clob.pollCount.Load())
	}
	if res.FilledSize != 199.99 {
		t.Errorf("FilledSize = %v, want 199.99", res.FilledSize)
	}
	if res.AveragePrice != 0.3610 {
		t.Errorf("AveragePrice = %v, want 0.3610", res.AveragePrice)
	}
}

// TestFOKDelayedThenUnmatched: an accepted-but-killed FOK is a failure, and the
// poll loop stops as soon as it sees the terminal "unmatched".
func TestFOKDelayedThenUnmatched(t *testing.T) {
	t.Parallel()
	clob := &fokFakeCLOB{
		orderBody:    `{"success":true,"orderId":"ord-fok","status":"delayed"}`,
		pollStatuses: []string{"unmatched", "matched"}, // 2nd would match — must never be reached
	}
	tc, key, creds := newFOKFixture(t, clob)

	res, err := tc.ExecuteTrade(context.Background(), key, fokProxy, creds, fokSellRequest())
	if err != nil {
		t.Fatalf("ExecuteTrade: %v", err)
	}
	if res.Success {
		t.Fatalf("Success = true, want false (killed FOK): %+v", res)
	}
	if clob.pollCount.Load() != 1 {
		t.Errorf("pollCount = %d, want 1 (stop at terminal unmatched)", clob.pollCount.Load())
	}
}

// TestFOKDelayedThenGone: a delayed FOK whose order 404s (killed and reaped) is
// a failure.
func TestFOKDelayedThenGone(t *testing.T) {
	t.Parallel()
	for _, tc404 := range []string{"404", "gone"} {
		tc404 := tc404
		t.Run(tc404, func(t *testing.T) {
			t.Parallel()
			clob := &fokFakeCLOB{
				orderBody:    `{"success":true,"orderId":"ord-fok","status":"delayed"}`,
				pollStatuses: []string{tc404},
			}
			tc, key, creds := newFOKFixture(t, clob)

			res, err := tc.ExecuteTrade(context.Background(), key, fokProxy, creds, fokSellRequest())
			if err != nil {
				t.Fatalf("ExecuteTrade: %v", err)
			}
			if res.Success {
				t.Fatalf("Success = true, want false (order gone): %+v", res)
			}
			if clob.pollCount.Load() != 1 {
				t.Errorf("pollCount = %d, want 1", clob.pollCount.Load())
			}
		})
	}
}

// TestFOKDelayedTimesOut: a FOK stuck delayed past the confirm timeout fails
// (the safe direction — a caller that keeps the arm and retries beats a false
// "sold").
func TestFOKDelayedTimesOut(t *testing.T) {
	t.Parallel()
	clob := &fokFakeCLOB{
		orderBody:    `{"success":true,"orderId":"ord-fok","status":"delayed"}`,
		pollStatuses: []string{"delayed"},
	}
	tc, key, creds := newFOKFixture(t, clob)

	res, err := tc.ExecuteTrade(context.Background(), key, fokProxy, creds, fokSellRequest())
	if err != nil {
		t.Fatalf("ExecuteTrade: %v", err)
	}
	if res.Success {
		t.Fatalf("Success = true, want false (timeout): %+v", res)
	}
	if clob.pollCount.Load() < 2 {
		t.Errorf("pollCount = %d, want >= 2 (polled repeatedly before timeout)", clob.pollCount.Load())
	}
}

// TestFOKPollRespectsContextCancel: cancelling the context short-circuits the
// poll loop promptly, well before the confirm timeout.
func TestFOKPollRespectsContextCancel(t *testing.T) {
	t.Parallel()
	clob := &fokFakeCLOB{
		orderBody:    `{"success":true,"orderId":"ord-fok","status":"delayed"}`,
		pollStatuses: []string{"delayed"},
	}
	tc, key, creds := newFOKFixture(t, clob)
	tc.fokConfirmTimeout = 10 * time.Second // long — the ctx must win, not this
	tc.fokPollInterval = 5 * time.Millisecond

	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
	defer cancel()

	start := time.Now()
	res, err := tc.ExecuteTrade(ctx, key, fokProxy, creds, fokSellRequest())
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("ExecuteTrade: %v", err)
	}
	if res.Success {
		t.Fatalf("Success = true, want false (ctx canceled): %+v", res)
	}
	if elapsed > 2*time.Second {
		t.Errorf("elapsed = %v, want prompt return (< 2s) on ctx cancel", elapsed)
	}
}

// TestGTCDelayedIsAccepted: GTC/GTD keep acceptance semantics — a resting order
// (status "live"/"delayed") is success and never triggers fill polling.
func TestGTCDelayedIsAccepted(t *testing.T) {
	t.Parallel()
	for _, status := range []string{"live", "delayed"} {
		status := status
		t.Run(status, func(t *testing.T) {
			t.Parallel()
			clob := &fokFakeCLOB{
				orderBody: `{"success":true,"orderId":"ord-gtc","status":"` + status + `"}`,
			}
			tc, key, creds := newFOKFixture(t, clob)

			req := fokSellRequest()
			req.OrderType = OrderTypeGTC
			res, err := tc.ExecuteTrade(context.Background(), key, fokProxy, creds, req)
			if err != nil {
				t.Fatalf("ExecuteTrade: %v", err)
			}
			if !res.Success {
				t.Fatalf("Success = false, want true (GTC acceptance): %+v", res)
			}
			if clob.pollCount.Load() != 0 {
				t.Errorf("pollCount = %d, want 0 (GTC never polls)", clob.pollCount.Load())
			}
		})
	}
}

// TestOrderBalanceShortfallAnnotated: the CLOB's balance-shortfall rejection
// must be classified on the TradeResult so the SL/TP monitor can clamp the
// sell to the wallet's actual balance instead of retrying a doomed order
// forever (issue #24). The raw HTTP body JSON-escapes the arrow as \u003e —
// the backtick literals below keep those six bytes verbatim, byte-exact with
// production (issue #24 reopened: a literal-"->" fixture passed while the
// escaped production body never matched). The decoded "->" form must also
// classify, belt and braces.
func TestOrderBalanceShortfallAnnotated(t *testing.T) {
	t.Parallel()
	const escapedArrowBody = `{"error":"not enough balance / allowance: the balance is not enough -\u003e balance: 16922, order amount: 49990000"}`
	const escapedZeroBody = `{"error":"not enough balance / allowance: the balance is not enough -\u003e balance: 0, order amount: 24990000"}`
	const literalArrowBody = `{"error":"not enough balance / allowance: the balance is not enough -> balance: 225000000, order amount: 450000000"}`

	for _, tt := range []struct {
		name      string
		body      string
		status    int
		wantAvail int64
	}{
		{"escaped arrow rejected with 400", escapedArrowBody, http.StatusBadRequest, 16_922},
		{"escaped arrow rejected with 200 error body", escapedArrowBody, http.StatusOK, 16_922},
		{"escaped arrow zero balance", escapedZeroBody, http.StatusBadRequest, 0},
		{"literal arrow rejected with 400", literalArrowBody, http.StatusBadRequest, 225_000_000},
		{"literal arrow rejected with 200 error body", literalArrowBody, http.StatusOK, 225_000_000},
	} {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			clob := &fokFakeCLOB{orderBody: tt.body, orderStatus: tt.status}
			tc, key, creds := newFOKFixture(t, clob)

			res, err := tc.ExecuteTrade(context.Background(), key, fokProxy, creds, fokSellRequest())
			if err != nil {
				t.Fatalf("ExecuteTrade: %v", err)
			}
			if res.Success {
				t.Fatalf("Success = true, want false: %+v", res)
			}
			if !res.InsufficientBalance {
				t.Error("InsufficientBalance = false, want true")
			}
			if res.AvailableSharesRaw != tt.wantAvail {
				t.Errorf("AvailableSharesRaw = %d, want %d", res.AvailableSharesRaw, tt.wantAvail)
			}
		})
	}

	t.Run("other rejections are not annotated", func(t *testing.T) {
		t.Parallel()
		clob := &fokFakeCLOB{
			orderBody:   `{"error":"order is invalid. Price breaks minimum tick size rules"}`,
			orderStatus: http.StatusBadRequest,
		}
		tc, key, creds := newFOKFixture(t, clob)

		res, err := tc.ExecuteTrade(context.Background(), key, fokProxy, creds, fokSellRequest())
		if err != nil {
			t.Fatalf("ExecuteTrade: %v", err)
		}
		if res.Success {
			t.Fatalf("Success = true, want false: %+v", res)
		}
		if res.InsufficientBalance || res.AvailableSharesRaw != 0 {
			t.Errorf("unrelated rejection annotated: InsufficientBalance=%v AvailableSharesRaw=%d",
				res.InsufficientBalance, res.AvailableSharesRaw)
		}
	})
}

// TestUnparseableResponseFailsClosed: a 200 whose body doesn't parse is not a
// confirmed anything — the old blanket-success fallback is flipped to failure,
// for every order type.
func TestUnparseableResponseFailsClosed(t *testing.T) {
	t.Parallel()
	for _, ot := range []OrderType{OrderTypeFOK, OrderTypeGTC} {
		ot := ot
		t.Run(string(ot), func(t *testing.T) {
			t.Parallel()
			clob := &fokFakeCLOB{orderBody: `this is not json {`}
			tc, key, creds := newFOKFixture(t, clob)

			req := fokSellRequest()
			req.OrderType = ot
			res, err := tc.ExecuteTrade(context.Background(), key, fokProxy, creds, req)
			if err != nil {
				t.Fatalf("ExecuteTrade: %v", err)
			}
			if res.Success {
				t.Fatalf("Success = true, want false (unparseable body): %+v", res)
			}
			if res.ErrorMsg == "" {
				t.Error("ErrorMsg empty, want a descriptive message")
			}
			if clob.pollCount.Load() != 0 {
				t.Errorf("pollCount = %d, want 0 (unparseable → no polling)", clob.pollCount.Load())
			}
		})
	}
}
