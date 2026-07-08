package polymarket

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestGetFeeRate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		tokenID    string
		response   map[string]interface{}
		statusCode int
		wantBps    int
		wantErr    bool
	}{
		{
			name:       "Sports market (30 bps)",
			tokenID:    "token-sports-123",
			response:   map[string]interface{}{"base_fee": 30},
			statusCode: http.StatusOK,
			wantBps:    30,
		},
		{
			name:       "Crypto market (72 bps)",
			tokenID:    "token-crypto-456",
			response:   map[string]interface{}{"base_fee": 72},
			statusCode: http.StatusOK,
			wantBps:    72,
		},
		{
			name:       "Geopolitics (0 bps / free)",
			tokenID:    "token-geo-789",
			response:   map[string]interface{}{"base_fee": 0},
			statusCode: http.StatusOK,
			wantBps:    0,
		},
		{
			name:       "Server error returns error",
			tokenID:    "token-err",
			statusCode: http.StatusInternalServerError,
			wantErr:    true,
		},
		{
			name:       "Missing base_fee defaults to 0",
			tokenID:    "token-missing",
			response:   map[string]interface{}{},
			statusCode: http.StatusOK,
			wantBps:    0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				// Verify endpoint and query param
				if r.URL.Path != "/fee-rate" {
					t.Errorf("unexpected path: %s", r.URL.Path)
				}
				if got := r.URL.Query().Get("token_id"); got != tt.tokenID {
					t.Errorf("token_id = %q, want %q", got, tt.tokenID)
				}

				w.WriteHeader(tt.statusCode)
				if tt.response != nil {
					json.NewEncoder(w).Encode(tt.response)
				}
			}))
			defer server.Close()

			tc := NewTradingClient(server.URL, 137)
			got, err := tc.GetFeeRate(context.Background(), tt.tokenID)

			if tt.wantErr {
				if err == nil {
					t.Fatal("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tt.wantBps {
				t.Errorf("GetFeeRate() = %d, want %d", got, tt.wantBps)
			}
		})
	}
}

func TestCalculateFee(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		shares     float64
		feeRateBps int
		price      float64
		wantFee    float64
		tolerance  float64
	}{
		{
			name:       "Clippers trade (Sports 30 bps, p=0.57)",
			shares:     95.69,
			feeRateBps: 30,
			price:      0.57,
			wantFee:    0.07,
			tolerance:  0.01,
		},
		{
			name:       "Crypto market at 50% (72 bps)",
			shares:     100,
			feeRateBps: 72,
			price:      0.50,
			wantFee:    0.18, // 100 × 0.0072 × 0.50 × 0.50
			tolerance:  0.01,
		},
		{
			name:       "Geopolitics (0 bps) = zero fee",
			shares:     1000,
			feeRateBps: 0,
			price:      0.50,
			wantFee:    0.0,
			tolerance:  0.0001,
		},
		{
			name:       "Near-certain outcome (p=0.99) = near-zero fee",
			shares:     100,
			feeRateBps: 50,
			price:      0.99,
			wantFee:    0.00495, // 100 × 0.005 × 0.99 × 0.01
			tolerance:  0.0001,
		},
		{
			name:       "Near-zero probability (p=0.01) = near-zero fee",
			shares:     100,
			feeRateBps: 50,
			price:      0.01,
			wantFee:    0.00495,
			tolerance:  0.0001,
		},
		{
			name:       "Peak fee at p=0.50 (Economics 50 bps)",
			shares:     100,
			feeRateBps: 50,
			price:      0.50,
			wantFee:    0.125, // 100 × 0.005 × 0.50 × 0.50
			tolerance:  0.001,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := CalculateFee(tt.shares, tt.feeRateBps, tt.price)
			if math.Abs(got-tt.wantFee) > tt.tolerance {
				t.Errorf("CalculateFee(%v, %d, %v) = %v, want %v (±%v)",
					tt.shares, tt.feeRateBps, tt.price, got, tt.wantFee, tt.tolerance)
			}
		})
	}
}

func TestBuyShareCalculation(t *testing.T) {
	t.Parallel()

	// Drives the real order-amount seam (calcOrderAmounts) rather than a
	// copy of the fee formula, so it fails if ExecuteTrade's math changes.
	tests := []struct {
		name       string
		amount     float64 // USDC to spend
		price      float64
		feeRateBps int
		wantShares string // taker amount, raw 1e6 units, floored to 2 decimals
		wantUSDC   string // maker amount, raw 1e6 units, exact shares×price
	}{
		{
			name:       "Clippers $60 at p=0.57, Sports 30 bps",
			amount:     60.0,
			price:      0.57,
			feeRateBps: 30,
			wantShares: "105120000", // ~60 / (0.57 * (1 + 0.003 * 0.43)) ≈ 105.12
			wantUSDC:   "59918400",  // 105.12 × 0.57 exactly
		},
		{
			name:       "Crypto $100 at p=0.50, 72 bps",
			amount:     100.0,
			price:      0.50,
			feeRateBps: 72,
			wantShares: "199280000", // ~100 / (0.50 * (1 + 0.0072 * 0.50)) ≈ 199.28
			wantUSDC:   "99640000",  // 199.28 × 0.50 exactly
		},
		{
			name:       "Geopolitics $100 at p=0.50, 0 bps (free)",
			amount:     100.0,
			price:      0.50,
			feeRateBps: 0,
			wantShares: "200000000", // 100 / 0.50 exactly
			wantUSDC:   "100000000",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			maker, taker, err := calcOrderAmounts("BUY", tt.amount, 0, tt.price, 0.01, tt.feeRateBps)
			if err != nil {
				t.Fatalf("calcOrderAmounts() unexpected error: %v", err)
			}
			if taker != tt.wantShares || maker != tt.wantUSDC {
				t.Errorf("calcOrderAmounts() = (maker %s, taker %s), want (maker %s, taker %s)",
					maker, taker, tt.wantUSDC, tt.wantShares)
			}
		})
	}
}
