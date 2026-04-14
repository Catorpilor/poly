# Polymarket Fee Structure Report

**Date:** 2026-04-01
**Effective Date of New Fees:** March 30, 2026

---

## Overview

Polymarket overhauled its fee structure on March 30, 2026, replacing the old flat taker fee model (up to 1000 bps) with a new dynamic, probability-based fee curve across nearly all market categories. Geopolitics remains the sole fee-free category.

---

## New Fee Formula

```
fee = C × feeRate × p × (1 - p)
```

| Variable | Description |
|----------|-------------|
| `C` | Shares traded |
| `feeRate` | Category-specific rate (see table below) |
| `p` | Share price (probability) |

Fees peak when probability is near 50% and decline symmetrically toward 0% and 100%.

---

## Taker Fee Rates by Category

| Category | Fee Rate (bps) | Peak Effective Fee (%) |
|---|---|---|
| Crypto | 72 | ~1.80% |
| Economics | 50 | ~1.50% |
| Culture | 50 | ~1.25% |
| Weather | 50 | ~1.25% |
| Other/General | 50 | ~1.25% |
| Finance | 40 | ~1.00% |
| Politics | 40 | ~1.00% |
| Tech | 40 | ~1.00% |
| Mentions | 40 | ~1.00% |
| Sports | 30 | ~0.75% |
| Geopolitics | 0 | 0% (free) |

---

## Maker Rebates

- Makers are **never charged fees** — only takers pay.
- Makers receive daily USDC rebates funded by taker fees:
  - **Crypto markets:** 20% rebate
  - **All other categories:** 25% rebate

---

## Fee Collection Mechanics

- **Buy orders:** fees collected in shares
- **Sell orders:** fees collected in USDC
- Minimum fee: 0.00001 USDC (anything smaller rounds to zero)
- No Polymarket fees on USDC deposits or withdrawals (third-party fees may apply)

---

## Impact Analysis: Old vs New Fee Structure

### Case Study — Clippers Trade (2026-04-01 03:16:55 UTC)

| Metric | Old Fee Model | New Fee Model |
|---|---|---|
| Market | Trail Blazers vs. Clippers | Same |
| Side | BUY Clippers | Same |
| Amount | $60.00 | $60.00 |
| Share Price | $0.57 | $0.57 |
| Shares | 95.69 | ~95.69 |
| Fee Rate | 1000 bps (flat) | 30 bps (Sports) |
| **Fee Paid** | **$5.46** | **~$0.07** |
| Effective Price | $0.627/share | ~$0.571/share |

**Fee reduction: ~98.7%**

### Formula Breakdown (New Model)

```
fee = 95.69 × 0.003 × 0.57 × (1 - 0.57)
    = 95.69 × 0.003 × 0.57 × 0.43
    = 95.69 × 0.000735
    ≈ $0.07
```

---

## Bot Configuration Note

The current bot (`cheshire42/poly:0.4.0-event-listing`) is using `TakerFeeBps=1000` from the market API. With the new fee structure, the `feeSchedule` object within each market should be used to calculate correct fees. Per the Polymarket changelog (March 31, 2026):

> "Fees should now be calculated using the `feeSchedule` object within a market."

Verify that the bot is reading the updated `feeSchedule` from the API to ensure accurate cost/P&L calculations going forward.

---

## Sources

- [Polymarket Fees Documentation](https://docs.polymarket.com/trading/fees)
- [Polymarket Changelog](https://docs.polymarket.com/changelog)
- [Parameter — Polymarket Fee Overhaul](https://parameter.io/polymarket-introduces-new-trading-fees-and-referral-program-in-2026-overhaul/)
- [MEXC — Polymarket Expands Taker Fees](https://www.mexc.com/news/976171)
- Docker logs: `polybot` container (2026-04-01)
