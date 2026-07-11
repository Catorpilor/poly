# Poly — Architecture

A personal Telegram trading bot for Polymarket ([ADR 0004](adr/0004-personal-bot-scope.md)).
The bot is a **stateless trading terminal**: Polymarket's APIs are the sole
source of truth for orders, positions, and markets
([ADR 0002](adr/0002-polymarket-api-as-source-of-truth.md)); local persistence
is limited to identity and standing instructions.

Domain vocabulary lives in [CONTEXT.md](../CONTEXT.md) — terms used here
(Trading Wallet, Arm, Auto-Redeem, …) mean exactly what the glossary says.

## System components

```
┌──────────────┐    ┌─────────────────────┐    ┌──────────────────────────────┐
│ Telegram user │──▶│  Poly bot (Go)      │──▶│  Polymarket                   │
└──────────────┘    │  Docker on RPi      │    │   CLOB API    (orders, auth)  │
                    │                     │    │   Gamma API   (market meta)   │
┌──────────────┐    │  internal/telegram  │    │   Data API    (positions)     │
│ Web client    │──▶│  internal/polymarket│    │   RTDS WS     (live tape)     │
│ (live feed,   │    │  internal/blockchain│   │   Market WS   (price feed)    │
│  web login)   │    │  internal/live      │    │   Builder Relayer (gasless)  │
└──────────────┘    │  internal/database  │    └──────────────────────────────┘
                    └────────┬────────────┘    ┌──────────────────────────────┐
                             │                 │  Polygon RPC                  │
                    ┌────────▼────────┐        │  (balances, approvals,        │
                    │  PostgreSQL     │        │   ERC-1155 reads)             │
                    │  3 tables       │        └──────────────────────────────┘
                    └─────────────────┘
```

`cmd/bot/main.go` wires: DB connection → Telegram bot → price feed manager →
SL/TP monitor → live trade manager → web server.

## Local state (PostgreSQL)

Exactly three tables are read/written (migrations in `migrations/`, applied
manually):

| Table          | Holds                                                        |
|----------------|--------------------------------------------------------------|
| `users`        | Telegram ↔ wallet binding, AES-256-GCM encrypted EOA key, `account_type`, settings |
| `login_tokens` | Web-auth handshake state machine (pending → authenticated → used/expired) |
| `sltp_arms`    | Armed SL/TP standing instructions with arm-time snapshots    |

Everything else (orders, positions, markets, history, P&L) is fetched live
from Polymarket per request, with short-TTL in-memory caching only.
Migration 007 dropped the dormant order/position/market/session/audit/alert
tables (ADR 0002).

## Wallet architecture

Every user is an **EOA** (signs, holds nothing) controlling a **Trading
Wallet** (holds collateral and shares, is `maker` on every order). Three
variants, classified on-chain once at import and persisted as
`users.account_type`:

| Account type     | Signature type          | Notes                          |
|------------------|-------------------------|--------------------------------|
| `legacy_proxy`   | POLY_GNOSIS_SAFE (2)    | Deliberately routed same as `safe` — validated in production |
| `safe`           | POLY_GNOSIS_SAFE (2)    | Gnosis Safe                    |
| `deposit_wallet` | POLY_1271 (3)           | V2-era contract account; ERC-7739 signing — see [deposit-wallet-flow.md](deposit-wallet-flow.md) |

Import flow (`internal/telegram/handlers.go`): validate key → delete the
user's message → resolve the existing Trading Wallet (deterministic registry
→ API → Gnosis detection) → classify account type → encrypt and persist.

**Known limitation** (accepted, ADR 0004): the resolver cannot derive
deposit-wallet addresses minted by the new factory for email/third-party
Polymarket signups; such imports would bind a wrong, empty Trading Wallet.
Personal-bot scope makes this a non-issue — new users are verified by hand.

## Trading pipeline

`/buy` / `/sell` (and the SL/TP monitor) all funnel into
`TradingClient.ExecuteTrade` (`internal/polymarket/trading.go`):

1. Resolve the outcome's Token ID from Gamma market metadata.
2. Fetch fee rates (Gamma `feeSchedule` for share estimation, CLOB
   `/fee-rate` for submission).
3. Price the order: VWAP walk over the book, plus a slippage allowance
   (buys ×1.02 capped 0.99, sells ×0.98), rounded to the market tick.
4. Derive maker/taker amounts integer-exactly on the tick grid
   ([ADR 0001](adr/0001-integer-exact-order-amounts.md)) — all order paths
   must go through `calcOrderAmounts`.
5. Pick maker/signer/signature type from the account type
   (`resolveOrderSigner`).
6. Sign the 11-field V2 EIP-712 order in `internal/polymarket/orderv2/`
   (local reimplementation of the CLOB V2 client, golden-tested against the
   TS SDK byte-for-byte; `expiration` ships in JSON but is NOT signed).
7. POST to `/order` with L2 HMAC headers.

Sells from positions check CTF exchange approval first and use the
position's Token ID directly (no Gamma lookup).

## Positions

Data API `/positions` (limit 500) via `UnifiedPositionScanner` →
`PositionManager`. No on-chain scanning, no local cache. Outcome labels are
display-only; Token ID is identity (see CONTEXT.md).

## Redemption

`/redeem` is a collect-now convenience
([ADR 0003](adr/0003-redeem-strategy-post-auto-redeem.md)) — Polymarket's
keeper auto-redeems winners anyway, paying pUSD, typically same-day.

- Positions grouped by condition; standard markets encode
  `CTF.redeemPositions` with **USDC.e collateral (deliberate — CTF
  conditions still settle in USDC.e in the V2 era; pUSD is a boundary
  wrapper)**; neg-risk markets go through the NegRisk adapter with amounts
  ordered by **outcome index**, from on-chain ERC-1155 balances.
- Executed gaslessly via the Builder Relayer (single SafeTx or MultiSend
  batch); the relayer blocks until on-chain confirmation.
- A follow-up sweep then wraps the wallet's entire (measured) USDC.e
  balance into pUSD so the payout is visible and tradeable. Non-fatal on
  failure — `/migrate` wraps leftovers.
- Deposit-wallet accounts are refused ("winnings arrive automatically"):
  the SafeTx path cannot sign for them.

## SL/TP (standing instructions)

Arm via the positions UI → snapshot `avg_price` + `shares` into
`sltp_arms`. `SLTPMonitor` (`internal/live/sltp_monitor.go`) evaluates the
**best bid** from the CLOB market WebSocket (plus a 20s backstop poll), in
precedence order: Ceiling TP (0.95, sell all) → TP (2×, sell half) → SL
(0.70×, sell all), optionally followed by a Lottery Ticket FOK buy of the
opposite token (≤$0.05/share, ≤$5). Thresholds are product policy — global
constants on the model, defined in CONTEXT.md. Fires re-use the standard
sell path. A cutover pause window can suppress firing.

## Live Feed

`internal/live` connects to Polymarket RTDS (`activity/trades`), filters by
event slug, and broadcasts matching trades to subscribed Telegram chats
(`/live`, `/stoplive`, `/subs`) and web clients. It is a **spectator tape**
— there is no automated copy trading.

## Web login

`login_` deep links + `login_tokens` implement a Telegram-authenticated web
session handshake — see [telegram-web-login.md](telegram-web-login.md).

## Deep links

Slug-based `start` parameters: `m_<slug>` (market), `s_<slug>` (share/live),
`login_<uuid>` (web auth). There is no UUID market-mapping scheme.

## Commands

`/start /wallet /import /export /markets /market /event /buy /sell /orders
/cancel /positions /pnl /history /redeem /migrate /live /stoplive /subs
/settings /alerts /gas /refresh /help`

## Deployment

Docker image `cheshire42/poly` on a Raspberry Pi; PostgreSQL on the host;
config via `.env`. Release flow and runbook: [CLAUDE.md](../CLAUDE.md) and
[DEPLOYMENT.md](DEPLOYMENT.md). (`REDIS_URL` exists in config but no code
currently uses Redis.)

## Decision log

- [ADR 0001](adr/0001-integer-exact-order-amounts.md) — integer-exact order
  amounts on the tick grid
- [ADR 0002](adr/0002-polymarket-api-as-source-of-truth.md) — APIs are the
  source of truth; no local trading state
- [ADR 0003](adr/0003-redeem-strategy-post-auto-redeem.md) — /redeem
  right-sized around Polymarket auto-redemption
- [ADR 0004](adr/0004-personal-bot-scope.md) — personal bot, not a
  multi-user product
