package polymarket

import (
	"fmt"
	"log"
	"math"
	"strconv"
	"strings"
)

// calcOrderAmounts derives the maker/taker amounts (raw 1e6 units) for an
// order from its tick-rounded limit price.
//
// The CLOB order carries no price field: the exchange recomputes the implied
// price from the amount ratio and rejects any ratio that is not an exact
// multiple of the market's tick. So the dependent amount (USDC for BUY,
// USDC for SELL) is derived in integer math — size × priceNum / tickDenom —
// and must NOT be rounded afterwards. The division is exact because size is
// floored to a multiple of 10^4 (2-decimal size cap) and every real tick
// denominator (10, 100, 1000, 10000) divides 10^4.
// See docs/adr/0001-integer-exact-order-amounts.md.
//
// BUY:  amount = USDC to spend (shares estimated via the fee-adjusted
//       effective price); returns maker = USDC, taker = shares.
// SELL: sharesRaw = exact position shares, or 0 to derive from amount;
//       returns maker = shares, taker = USDC.
func calcOrderAmounts(side string, amount float64, sharesRaw int64, price, tick float64, calcFeeBps int) (makerAmount, takerAmount string, err error) {
	if price <= 0 || price >= 1 {
		return "", "", fmt.Errorf("invalid price %.6f: must be strictly between 0 and 1", price)
	}

	var shares int64
	switch strings.ToUpper(side) {
	case "BUY":
		// Total cost = C × p × (1 + feeRate × (1-p)) — solve for shares C.
		calcFeeDecimal := float64(calcFeeBps) / 10000.0
		effectivePrice := price * (1 + calcFeeDecimal*(1-price))
		shares = int64((amount / effectivePrice) * 1e6)
	case "SELL":
		shares = sharesRaw
		if shares <= 0 {
			shares = int64((amount / price) * 1e6)
		}
	default:
		return "", "", fmt.Errorf("invalid side %q: must be BUY or SELL", side)
	}

	// Size cap: max 2 decimals, so floor to a multiple of 10^4 raw units.
	sharesRounded := (shares / 10000) * 10000
	if sharesRounded <= 0 {
		return "", "", fmt.Errorf("order too small: size %.6f shares floors to zero at price %.4f", float64(shares)/1e6, price)
	}

	usdc, exact := deriveUSDCOnGrid(sharesRounded, price, tick)
	if !exact {
		log.Printf("calcOrderAmounts WARN: tick=%g cannot place price=%.6f exactly on grid for size=%d — falling back to float rounding", tick, price, sharesRounded)
		usdc = int64(math.Round(float64(sharesRounded) * price))
	}
	if usdc <= 0 {
		return "", "", fmt.Errorf("order too small: %d shares at price %.4f rounds to zero USDC", sharesRounded, price)
	}

	sharesStr := strconv.FormatInt(sharesRounded, 10)
	usdcStr := strconv.FormatInt(usdc, 10)
	if strings.ToUpper(side) == "BUY" {
		return usdcStr, sharesStr, nil
	}
	return sharesStr, usdcStr, nil
}

// deriveUSDCOnGrid computes sharesRounded × price exactly, as
// sharesRounded × priceNum / tickDenom in integer math. It reports
// exact=false when price does not sit on the tick grid or the division
// would not be exact (bogus tick from the API) — callers must then fall
// back rather than mispricing the order on a wrong grid.
func deriveUSDCOnGrid(sharesRounded int64, price, tick float64) (usdc int64, exact bool) {
	if tick <= 0 {
		return 0, false
	}
	tickDenom := int64(math.Round(1 / tick))
	if tickDenom <= 0 {
		return 0, false
	}
	scaled := price * float64(tickDenom)
	priceNum := int64(math.Round(scaled))
	// Reject when price×tickDenom is not (float-fuzz close to) an integer:
	// e.g. price 0.5 with bogus tick 0.003 gives 166.5 — rounding it would
	// silently misprice the order at 167/333.
	if priceNum <= 0 || math.Abs(scaled-float64(priceNum)) > 1e-6 {
		return 0, false
	}
	product := sharesRounded * priceNum
	if product%tickDenom != 0 {
		return 0, false
	}
	return product / tickDenom, true
}
