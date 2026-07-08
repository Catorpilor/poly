# Glossary

Canonical vocabulary for the Poly trading bot. Terms here mean exactly this,
everywhere in the codebase, docs, and conversation.

## Order pricing

**Tick Size** — The per-market minimum price increment on the Polymarket CLOB
(e.g. `0.01` for most markets, `0.001` for long-shot markets). Every price the
CLOB accepts must sit exactly on this grid. Comes from the market, not from us.

**Implied Price** — The price the CLOB *derives* from an order's maker/taker
amount ratio. An order carries no price field; the implied price is the only
price the exchange sees, and it must land exactly on the tick grid.

**Size** — The number of outcome shares in an order. The CLOB caps size
precision at 2 decimal places regardless of tick size.

**Maker Amount / Taker Amount** — What the order's signer gives / receives.
For a BUY: maker = USDC spent, taker = shares received. For a SELL: maker =
shares sold, taker = USDC received. Their ratio *is* the implied price.

**Effective Price** — The per-share cost including the dynamic taker fee;
used to estimate how many shares a USDC budget buys. Distinct from the order's
implied price, which is fee-exclusive.

**Slippage Allowance** — The deliberate worsening of a market order's limit
price (buys up, sells down) so it still crosses the book if the market moves
between quote and submission.
