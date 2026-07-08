package polymarket

import (
	"strconv"
	"testing"
)

func TestCalcOrderAmounts(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		side       string
		amount     float64 // BUY: USDC to spend; SELL fallback: USDC target
		sharesRaw  int64   // SELL: exact position shares (0 = derive from amount)
		price      float64 // tick-rounded limit price
		tick       float64
		calcFeeBps int
		wantMaker  string
		wantTaker  string
		wantErr    bool
	}{
		{
			// Prod incident 2026-07-08: $150 France WC buy, VWAP 0.33 +2%
			// slippage → 0.337 on a 0.001-tick market. Old code rounded the
			// exact maker 149702140 to 149702100, implied price
			// 0.336999909954527 → CLOB rejection.
			name:       "BUY regression: sub-cent tick stays exactly on grid",
			side:       "BUY",
			amount:     150,
			price:      0.337,
			tick:       0.001,
			calcFeeBps: 30,
			wantMaker:  "149702140",
			wantTaker:  "444220000",
		},
		{
			name:       "BUY 1-cent tick market unchanged",
			side:       "BUY",
			amount:     60,
			price:      0.57,
			tick:       0.01,
			calcFeeBps: 30,
			wantMaker:  "59918400",
			wantTaker:  "105120000",
		},
		{
			name:       "BUY slippage-capped price 0.99 on 0.001 tick",
			side:       "BUY",
			amount:     150,
			price:      0.99,
			tick:       0.001,
			calcFeeBps: 30,
			wantMaker:  "149994900",
			wantTaker:  "151510000",
		},
		{
			name:      "BUY coarse 0.1 tick",
			side:      "BUY",
			amount:    100,
			price:     0.5,
			tick:      0.1,
			wantMaker: "100000000",
			wantTaker: "200000000",
		},
		{
			name:      "BUY finest 0.0001 tick",
			side:      "BUY",
			amount:    10,
			price:     0.0503,
			tick:      0.0001,
			wantMaker: "9999640",
			wantTaker: "198800000",
		},
		{
			// Prod incident 2026-06-23: sell rejected with implied price
			// 0.9479994714587738 on a 0.001-tick market.
			name:      "SELL regression: exact position shares on sub-cent tick",
			side:      "SELL",
			sharesRaw: 444220303,
			price:     0.948,
			tick:      0.001,
			wantMaker: "444220000",
			wantTaker: "421120560",
		},
		{
			name:      "SELL derived from USDC amount when sharesRaw absent",
			side:      "SELL",
			amount:    100,
			price:     0.5,
			tick:      0.01,
			wantMaker: "200000000",
			wantTaker: "100000000",
		},
		{
			// tick 0.003 has no denominator dividing 10^4; the fallback
			// float-rounds instead of silently mispricing on a wrong grid.
			name:      "bogus tick falls back to float rounding",
			side:      "BUY",
			amount:    100,
			price:     0.5,
			tick:      0.003,
			wantMaker: "100000000",
			wantTaker: "200000000",
		},
		{
			name:    "order too small: size floors to zero shares",
			side:    "BUY",
			amount:  0.004,
			price:   0.99,
			tick:    0.001,
			wantErr: true,
		},
		{
			name:    "price zero rejected",
			side:    "BUY",
			amount:  100,
			price:   0,
			tick:    0.01,
			wantErr: true,
		},
		{
			name:      "price one rejected",
			side:      "SELL",
			sharesRaw: 10000000,
			price:     1,
			tick:      0.01,
			wantErr:   true,
		},
		{
			name:    "unknown side rejected",
			side:    "HOLD",
			amount:  100,
			price:   0.5,
			tick:    0.01,
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			maker, taker, err := calcOrderAmounts(tt.side, tt.amount, tt.sharesRaw, tt.price, tt.tick, tt.calcFeeBps)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("calcOrderAmounts() = (%s, %s, nil), want error", maker, taker)
				}
				return
			}
			if err != nil {
				t.Fatalf("calcOrderAmounts() unexpected error: %v", err)
			}
			if maker != tt.wantMaker || taker != tt.wantTaker {
				t.Errorf("calcOrderAmounts() = (%s, %s), want (%s, %s)", maker, taker, tt.wantMaker, tt.wantTaker)
			}
			assertOnTickGrid(t, tt.side, maker, taker, tt.price, tt.tick)
		})
	}
}

// assertOnTickGrid verifies the invariant the CLOB enforces: the implied
// price (USDC amount / share amount) must be an exact multiple of the tick.
// Checked in integer math: usdc * (1/tick) == shares * (price/tick).
func assertOnTickGrid(t *testing.T, side, maker, taker string, price, tick float64) {
	t.Helper()

	tickDenom := int64(1/tick + 0.5)
	priceNum := int64(price*float64(tickDenom) + 0.5)
	if float64(priceNum)/float64(tickDenom) != price {
		return // price not representable on this tick grid (fallback cases)
	}

	makerInt, err := strconv.ParseInt(maker, 10, 64)
	if err != nil {
		t.Fatalf("maker %q not an integer: %v", maker, err)
	}
	takerInt, err := strconv.ParseInt(taker, 10, 64)
	if err != nil {
		t.Fatalf("taker %q not an integer: %v", taker, err)
	}

	usdc, shares := makerInt, takerInt // BUY: maker = USDC, taker = shares
	if side == "SELL" {
		usdc, shares = takerInt, makerInt
	}
	if shares%10000 != 0 {
		t.Errorf("share amount %d not a multiple of 10^4 (size max 2 decimals)", shares)
	}
	if usdc*tickDenom != shares*priceNum {
		t.Errorf("implied price off tick grid: %d/%d = %v, want exactly %v",
			usdc, shares, float64(usdc)/float64(shares), price)
	}
}
