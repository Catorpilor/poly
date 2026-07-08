# 1. Integer-exact order amount derivation

Status: accepted
Date: 2026-07-08

## Context

A CLOB order carries no price field. The exchange derives the implied price
from the maker/taker amount ratio and rejects any order whose ratio is not an
exact multiple of the market's tick size.

`ExecuteTrade` used to round the dependent USDC amount to the nearest 100 raw
units under a "max 4 decimals" assumption inherited from 1¢-tick markets. On
sub-cent-tick markets this destroyed an already-exact ratio: a $150 buy at
0.337 on a 0.001-tick market produced maker `149702100` instead of the exact
`149702140`, an implied price of `0.336999909954527`, and the rejection
*"price breaks minimum tick size rule 0.001"* (prod incidents 2026-06-23 SELL,
2026-07-08 BUY). Polymarket's own client caps amount decimals at price
decimals + size decimals (tick 0.001 → 5 decimals), so the 4-decimal rounding
was never a real requirement.

## Decision

Derive the dependent amount in integer math from the tick-rounded price:

    tickDenom = round(1 / tick)          e.g. 1000
    priceNum  = round(price × tickDenom) e.g. 337
    usdc      = size × priceNum / tickDenom

with no rounding afterwards. The division is exact by construction: size is
floored to a multiple of 10^4 raw units (the CLOB's 2-decimal size cap) and
every real tick denominator (10, 100, 1000, 10000) divides 10^4.

Degenerate inputs error out before an order is built (size floors to zero,
price outside (0,1), unknown side). A tick whose denominator does not divide
10^4 — impossible from Polymarket today — logs a warning and falls back to
float rounding of size × price rather than mispricing on a wrong grid.

Alternatives considered:

- **Port py-clob-client's ROUNDING_CONFIG table** (per-tick price/size/amount
  decimals) — matches the reference SDK but keeps float math in the ratio
  path and is a larger change for the same behavior.
- **Skip the round-to-100 only when tick < 0.01** — smallest diff, but leaves
  float products as the source of truth for the ratio and keeps the magic
  rounding for 1¢ markets.

## Consequences

- The implied price is exact for every valid tick; the float-fuzz bug class
  is eliminated from the ratio path.
- Amount decimal count now varies by market tick (4 decimals on 1¢ markets,
  5 on 0.001 markets) — matching what the exchange actually accepts.
- Amounts must never be re-rounded after `calcOrderAmounts`; any new order
  path must go through it.
