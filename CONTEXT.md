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

**Live Feed** — A spectator view of a market's tape: third-party Trades
streamed to subscribed chats and web clients. Watch-only — it never places
orders and is not copy trading. Telegram delivery of the tape is opt-in per
subscription (`/live <slug> tape`); a default subscription is quiet — snipe
watch and the web tape only.

**Position** — A user's current holdings of one outcome token: share count
plus average price. Lives on Polymarket, keyed by Token ID.

**Token ID** — The ERC-1155 token identifier of one outcome of one market.
The canonical identity for outcomes and positions.

**Outcome** — One possible result of a market, identified by its Token ID.
The outcome *label* ("YES", "KNICKS") is display metadata only: its casing
varies across Polymarket's APIs, and it must never be used as a key or
compared case-sensitively.

**Outcome Index** — The position of an Outcome within its market's token
list (0 or 1 for binary markets). Positional identity: it selects which
Token ID a trade or redemption targets, and must never be inferred from
the outcome label.

**Market Index** — The position of a market within an event's Moneyline
market list. Always 0 for events with a single Moneyline market (2-way);
0–2 for events with one market per side (3-way soccer: home/draw/away).
Distinct from Outcome Index: Market Index picks the market, Outcome Index
picks the side within it.

**Sub-market** — Any market in an event other than its Moneyline
market(s): game/map winners, totals, spreads, props, first objectives.
Classified by question keywords, not slug shape. Addressed by its market
slug — a stable identity — never by position in the event's market list,
which reorders.

**Pinned Market** — The Sub-market a web subscription addressed directly
(the user subscribed with its market slug rather than the event slug).
The panel's primary buy buttons and its live trade feed target the pinned
market; without a pin they target the Moneyline.

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
last trade or the midpoint. Arms whose market has closed are swept
automatically — auto-disarmed with a cleanup notice, since a finished
market can never fire a trigger.

**Take-Profit (TP)** — Trigger: bid reaches 2× the arm-time average price →
sell a quarter of the arm-time shares. Lightened from half in v0.12.8:
counterfactual replay of realized price paths showed early banking mostly
sells winners cheap once the trailing stop is insuring the position.

**Stop-Loss (SL)** — Breakeven-trailing. Dormant until the High-Water Mark
reaches 1.20× the arm-time average price (until then there is no stop —
max loss is the stake). Once active: trigger = max(entry, HWM × 0.80),
ratcheting up only. A breach must hold for 30s straight (confirmation
debounce), then the exit is a fill-or-kill limit at trigger × 0.90 — never
a market order into a gapped book; a no-fill keeps the arm and retries
every ≥30s while the breach persists. Replaced the fixed 0.70× stop in
v0.11: 92 days of fills showed that stop realizing −48% of basis and
selling 29% eventual winners.

**High-Water Mark (HWM)** — The highest best bid observed on an armed
position since arm/re-arm, persisted per arm (`high_water_mark`). Seeded
to the entry price; only ever raised. Drives SL activation and the
trailing trigger.

**Acceptance** — The exchange took an order onto its books (or into its
bet-delay queue). Says nothing about execution. The natural success
criterion for resting orders (GTC/GTD): "your limit order is placed."

**Fill** — Shares actually traded. The only success criterion for
fill-or-kill orders: an accepted-but-killed FOK is a failure, not a
partial anything. Auto-sell fires, disarms, and "sold" notifications may
only follow a Fill, never mere Acceptance.

**Bet Delay** — Polymarket's in-play matching delay: on live sports
markets the CLOB *accepts* an order (`status: delayed`) and matches or
kills it only after the delay elapses. During the delay an order is
neither filled nor dead — treating Acceptance as a Fill here is exactly
the false-fire bug of issue #22. During the delay the order is also not
queryable on the order-status endpoint — Acceptance is the only signal
the exchange gives until the delay elapses (issue #27).

**Ceiling Take-Profit** — Trigger: bid reaches 0.95 → sell everything,
taking precedence over the 2× rule.

**Lottery Ticket** — Optional follow-on to a Ceiling Take-Profit: a
fill-or-kill buy of the *opposite* outcome token, only if it costs at most
$0.05 per share, spending at most $5 — a cheap bet on a reversal.

**Comeback Snipe** — An auto-buy-then-top-up on a token that was
*formerly competitive, now priced as dead*: an in-play market whose
token's Session High bid reached ≥ 0.40 and whose ask has since fallen
into the 0.03–0.20 crash band. Below 0.03 the market is declaring death
or settlement rather than overreacting — no game-end signal exists in
the metadata, so the floor is what keeps a finished game's loser token
from alerting. v2 (issue #45): on every genuine alert the bot instantly
auto-buys a fixed $10 for the recipient through the guarded buy path
(fresh-ask repricing guard ≤ 0.30), bounded by a $50-per-user-per-UTC-day
in-memory cap (a restart resets it — soft rail). The alert then offers a
one-tap Add $25 top-up riding the same alert entry; any auto-buy failure
(cap, guard, no wallet, CLOB) falls back to the unchanged manual alert
with one-tap buy buttons — delivery is never blocked by execution. The
human judgment layer thus moved from gatekeeper to top-up: the bot takes
the small stake on its own, the human decides whether to press. Alerts
are episode-based: after alerting, a token re-alerts only once its ask
has recovered above the midpoint of the crash threshold and the
competitiveness bar (0.30 today) and then crashes again — a real
recovery, not spread noise. Buying via the alert (auto or tap) silences
that token for the rest of the match. Distinct from a Lottery Ticket
(which reacts to *our own* ceiling exit on the other side) and from
pre-game value buys (not in-play, out of scope).

**Session High** — The highest trade-or-bid price the bot knows for a
token in the current session: seeded at watch-start from the CLOB's
recent trade history (since ~game start), then ratcheted live by
observed bids — raised only, never lowered. In-memory: a restart
re-seeds from history, so a watcher that joins (or rejoins) mid-game
still knows a token was formerly competitive.

These thresholds are product policy, deliberately global constants — not
per-user configuration.
