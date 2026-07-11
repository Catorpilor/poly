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

## Orders, trades, positions

**Order** — A signed instruction to the CLOB to buy or sell shares at an
implied price. Orders live on the exchange, never locally. An **Open Order**
is the resting, unmatched remainder of one.

**Trade** — An executed match (a fill). On a market's live tape, a Trade is
anyone's fill; in our own results, ours. A Trade results from an Order and
is never a synonym for one.

**Position** — A user's current holdings of one outcome token: share count
plus average price. Lives on Polymarket, keyed by Token ID.

**Token ID** — The ERC-1155 token identifier of one outcome of one market.
The canonical identity for outcomes and positions.

**Outcome** — One possible result of a market, identified by its Token ID.
The outcome *label* ("YES", "KNICKS") is display metadata only: its casing
varies across Polymarket's APIs, and it must never be used as a key or
compared case-sensitively.

## Wallets & accounts

**EOA** — The externally-owned account whose private key the bot holds
encrypted. It signs orders and Safe transactions on behalf of the Trading
Wallet. It never holds assets.

**Trading Wallet** — The on-chain contract account that holds a user's
collateral and outcome shares and appears as `maker` on every order. Each
user has exactly one. Comes in three variants: Legacy Proxy, Safe, Deposit
Wallet. Historically called the "proxy" throughout code and schema (e.g. the
stored "proxy address" is really the Trading Wallet address, whatever its
variant); as an umbrella term, "Trading Wallet" supersedes "proxy" — "proxy"
now refers only to the Legacy Proxy variant.

**Legacy Proxy** — Trading Wallet variant: Polymarket's original
minimal-proxy contract account.

**Safe** — Trading Wallet variant: a Gnosis Safe.

**Deposit Wallet** — Trading Wallet variant: the V2-era contract account
whose order signatures are validated via EIP-1271/ERC-7739. Its address is
both `maker` and `signer` on orders; the EOA appears only in the inner
signature.

**Account Type** — Which Trading Wallet variant a user has. Classified once
from on-chain evidence when the wallet is imported, then persisted. Legacy
Proxy vs Safe is a *classification* distinction only: at order-signing time
both deliberately use the Safe signature type, which is validated in
production for every affected account.

**Funding Address** — *Not part of this domain.* Polymarket's EIP-7702
per-user deposit-routing address mentioned in their docs; the bot never
touches it.

## Collateral & redemption

**USDC.e** — Bridged USDC on Polygon. The collateral every CTF condition
actually settles in — V1 and V2 era alike.

**pUSD (Polymarket USD)** — Polymarket's boundary wrapper over USDC.e: the
token users hold and the V2 exchanges trade. Minted 1:1 from USDC.e at the
edges (deposits, redemption payouts). *Not* the CTF-layer collateral.

**Redeem** — Exchanging a resolved market's outcome tokens for collateral.
Winning tokens pay out; losing tokens burn for zero.

**Auto-Redeem** — Polymarket's keeper service that batch-redeems resolved
positions for all holders and pays pUSD, typically same-day. Outside the
bot's control; no published SLA.

## Standing instructions (SL/TP)

**Arm** — A standing instruction on one position: watch its price and
auto-sell when a trigger fires. One Arm per user per Token ID. Thresholds
are frozen from the position's average price and share count *at arm time*;
buying more shares later does not move them until re-armed. All triggers
evaluate the **best bid** — the price a sell would actually hit — never the
last trade or the midpoint.

**Take-Profit (TP)** — Trigger: bid reaches 2× the arm-time average price →
sell half the arm-time shares.

**Stop-Loss (SL)** — Trigger: bid falls to 0.70× the arm-time average
price → sell everything.

**Ceiling Take-Profit** — Trigger: bid reaches 0.95 → sell everything,
taking precedence over the 2× rule.

**Lottery Ticket** — Optional follow-on to a Ceiling Take-Profit: a
fill-or-kill buy of the *opposite* outcome token, only if it costs at most
$0.05 per share, spending at most $5 — a cheap bet on a reversal.

These thresholds are product policy, deliberately global constants — not
per-user configuration.
