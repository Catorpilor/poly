package polymarket

import (
	"context"
	"crypto/ecdsa"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
)

// fakeCLOB is a minimal CLOB API covering the endpoints one Execute() call
// touches: credential derivation, the L2 auth check, the order book (price
// and condition ID), market info (tick size), fee rate, and order intake.
type fakeCLOB struct {
	l2Status     int // status for GET /auth/api-keys (200 = auth ok)
	feeStatus    int // status for GET /fee-rate
	credsStatus  int // status for GET /auth/derive-api-key AND POST /auth/api-key
	ordersPlaced atomic.Int64
}

func (f *fakeCLOB) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/auth/derive-api-key"), r.URL.Path == "/auth/api-key":
			if f.credsStatus != http.StatusOK {
				w.WriteHeader(f.credsStatus)
				return
			}
			// Secret must be valid urlsafe base64 — L2 signing decodes it.
			json.NewEncoder(w).Encode(map[string]string{
				"apiKey": "test-key", "secret": "c2VjcmV0LXNlY3JldC1zZWNyZXQ=", "passphrase": "pass",
			})
		case r.URL.Path == "/auth/api-keys":
			w.WriteHeader(f.l2Status)
		case r.URL.Path == "/book":
			json.NewEncoder(w).Encode(map[string]interface{}{
				"market": "cond-1",
				"asks":   []map[string]string{{"price": "0.50", "size": "1000"}},
				"bids":   []map[string]string{{"price": "0.48", "size": "1000"}},
			})
		case strings.HasPrefix(r.URL.Path, "/markets/"):
			json.NewEncoder(w).Encode(map[string]interface{}{"minimum_tick_size": 0.01})
		case r.URL.Path == "/fee-rate":
			if f.feeStatus != http.StatusOK {
				w.WriteHeader(f.feeStatus)
				return
			}
			json.NewEncoder(w).Encode(map[string]int{"base_fee": 30})
		case r.URL.Path == "/order":
			f.ordersPlaced.Add(1)
			json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "orderId": "ord-123"})
		default:
			http.NotFound(w, r)
		}
	}
}

// newExecutorFixture wires a TradeExecutor to fake CLOB and Gamma servers.
// gammaStatus controls the Gamma market lookup (fee schedule + negRisk).
func newExecutorFixture(t *testing.T, clob *fakeCLOB, gammaStatus int) (*TradeExecutor, *ecdsa.PrivateKey) {
	t.Helper()

	clobSrv := httptest.NewServer(clob.handler())
	t.Cleanup(clobSrv.Close)

	gammaSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if gammaStatus != http.StatusOK {
			w.WriteHeader(gammaStatus)
			return
		}
		json.NewEncoder(w).Encode(map[string]interface{}{
			"id":          "gm-1",
			"negRisk":     true,
			"feeSchedule": map[string]float64{"rate": 0.03}, // 30 bps
		})
	}))
	t.Cleanup(gammaSrv.Close)

	key, err := crypto.GenerateKey()
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}

	executor := NewTradeExecutor(
		NewTradingClient(clobSrv.URL, 137),
		NewMarketClientWithURL(gammaSrv.URL),
	)
	return executor, key
}

func buyRequest() *TradeRequest {
	return &TradeRequest{
		MarketID:  "gm-1",
		TokenID:   "123456",
		Side:      "BUY",
		Outcome:   "Yes",
		Amount:    10,
		Price:     0, // market order
		OrderType: OrderTypeGTC,
	}
}

func TestTradeExecutorHappyPath(t *testing.T) {
	t.Parallel()

	clob := &fakeCLOB{l2Status: 200, feeStatus: 200, credsStatus: 200}
	executor, key := newExecutorFixture(t, clob, http.StatusOK)

	req := buyRequest()
	result, err := executor.Execute(context.Background(), key, common.HexToAddress("0x1111111111111111111111111111111111111111"), req)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !result.Success || result.OrderID != "ord-123" {
		t.Errorf("result = %+v, want success with orderId ord-123", result)
	}
	if clob.ordersPlaced.Load() != 1 {
		t.Errorf("orders placed = %d, want 1", clob.ordersPlaced.Load())
	}

	// Fee discovery must have populated the request before submission:
	// Gamma feeSchedule (0.03 → 30 bps) for amount calculation plus
	// negRisk, CLOB fee-rate for the order.
	if req.CalcFeeBps != 30 {
		t.Errorf("CalcFeeBps = %d, want 30", req.CalcFeeBps)
	}
	if req.TakerFeeBps != 30 {
		t.Errorf("TakerFeeBps = %d, want 30", req.TakerFeeBps)
	}
	if !req.NegativeRisk {
		t.Error("NegativeRisk = false, want true (from Gamma)")
	}
}

// F10: the L2 auth pre-check must abort the trade before any order reaches
// the CLOB — a stale credential should fail loudly, not as a rejected order.
func TestTradeExecutorL2AuthFailureAbortsBeforeOrder(t *testing.T) {
	t.Parallel()

	clob := &fakeCLOB{l2Status: 401, feeStatus: 200, credsStatus: 200}
	executor, key := newExecutorFixture(t, clob, http.StatusOK)

	_, err := executor.Execute(context.Background(), key, common.HexToAddress("0x1111111111111111111111111111111111111111"), buyRequest())
	if err == nil {
		t.Fatal("Execute with failing L2 auth = nil error, want error")
	}
	if clob.ordersPlaced.Load() != 0 {
		t.Errorf("orders placed = %d, want 0 — L2 failure must abort before submission", clob.ordersPlaced.Load())
	}
}

func TestTradeExecutorCredentialFailureAborts(t *testing.T) {
	t.Parallel()

	clob := &fakeCLOB{l2Status: 200, feeStatus: 200, credsStatus: 500}
	executor, key := newExecutorFixture(t, clob, http.StatusOK)

	_, err := executor.Execute(context.Background(), key, common.HexToAddress("0x1111111111111111111111111111111111111111"), buyRequest())
	if err == nil {
		t.Fatal("Execute with failing credentials = nil error, want error")
	}
	if clob.ordersPlaced.Load() != 0 {
		t.Errorf("orders placed = %d, want 0", clob.ordersPlaced.Load())
	}
}

// Fee discovery is best-effort: a missing Gamma fee schedule or CLOB
// fee-rate must not block the trade (both existing entry points log and
// proceed with defaults).
func TestTradeExecutorFeeLookupsAreBestEffort(t *testing.T) {
	t.Parallel()

	t.Run("gamma down", func(t *testing.T) {
		t.Parallel()
		clob := &fakeCLOB{l2Status: 200, feeStatus: 200, credsStatus: 200}
		executor, key := newExecutorFixture(t, clob, http.StatusInternalServerError)

		req := buyRequest()
		result, err := executor.Execute(context.Background(), key, common.HexToAddress("0x1111111111111111111111111111111111111111"), req)
		if err != nil || !result.Success {
			t.Fatalf("Execute = (%+v, %v), want success despite Gamma outage", result, err)
		}
		if req.CalcFeeBps != 0 || req.NegativeRisk {
			t.Errorf("CalcFeeBps = %d, NegativeRisk = %v — want zero-value defaults", req.CalcFeeBps, req.NegativeRisk)
		}
		if req.TakerFeeBps != 30 {
			t.Errorf("TakerFeeBps = %d, want 30 from CLOB", req.TakerFeeBps)
		}
	})

	t.Run("clob fee-rate down", func(t *testing.T) {
		t.Parallel()
		clob := &fakeCLOB{l2Status: 200, feeStatus: 500, credsStatus: 200}
		executor, key := newExecutorFixture(t, clob, http.StatusOK)

		req := buyRequest()
		result, err := executor.Execute(context.Background(), key, common.HexToAddress("0x1111111111111111111111111111111111111111"), req)
		if err != nil || !result.Success {
			t.Fatalf("Execute = (%+v, %v), want success despite fee-rate outage", result, err)
		}
		if req.TakerFeeBps != 0 {
			t.Errorf("TakerFeeBps = %d, want 0 default", req.TakerFeeBps)
		}
		if req.CalcFeeBps != 30 {
			t.Errorf("CalcFeeBps = %d, want 30 from Gamma", req.CalcFeeBps)
		}
	})
}
