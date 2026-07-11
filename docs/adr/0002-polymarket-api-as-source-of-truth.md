# 2. Polymarket APIs are the source of truth; no local trading state

Status: accepted
Date: 2026-07-09

## Context

The initial schema (migration 001) and ARCHITECTURE.md describe a bot that
persists orders, positions, markets, sessions, audit logs, and price alerts
locally. The code never grew into that model: orders, positions, and market
data are fetched live from the Polymarket CLOB/Gamma/Data APIs on every
request, and only `users`, `login_tokens`, and `sltp_arms` are read or
written by Go. The dormant tables and their Go models (`database.Order`,
`database.Position`, `database.Market`, …) occupy the domain's best names
and force the live types into collisions (`polymarket.Position` vs
`database.Position`) without buying anything.

## Decision

The bot is a stateless trading terminal over Polymarket's state. Polymarket's
APIs are the sole source of truth for orders, positions, and markets. Local
persistence is limited to what Polymarket cannot know:

- **identity** — `users` (Telegram ↔ wallet binding, encrypted key,
  account type), `login_tokens` (web-auth handshake)
- **standing instructions** — `sltp_arms` (armed SL/TP with arm-time
  snapshots)

The dormant tables (`orders`, `positions`, `markets`, `sessions`,
`audit_logs`, `price_alerts`), their Go models, and the dead position
scanners are to be deleted, and ARCHITECTURE.md updated to match.

Alternative considered: keep the schema and grow into it (local order
tracking, P&L history, audit logs). Rejected because no roadmap feature
needs it, and if history is ever needed it should be re-added as an
append-only event log rather than this CRUD mirror of Polymarket state.

## Consequences

- Any future feature needing trade history or time-series P&L requires a
  new migration — that cost is accepted and deliberate.
- No local cache means every position/market view costs API calls;
  short-TTL in-memory caching remains the mitigation.
- The `Position`/`Order`/`Market` names belong to the live API types;
  new code must not reintroduce DB models under those names.
