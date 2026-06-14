# Combo Positions (Polymarket RFQ) — Taker Side

**Role decision:** Bot acts as **requester / taker** (user builds & takes combos), not market maker.
**Docs:** https://docs.polymarket.com/market-makers/combos
**First goal (user-chosen):** ship a **read-only combo positions view** before any RFQ/trading.

## Mental model (taker)
A combo is one YES/NO position synthesized from multiple market legs, traded via RFQ
(request-for-quote over WebSocket) instead of the CLOB order book.

```
build legs -> RFQ_REQUEST (WS) -> maker quotes (400ms) -> show best quote
-> user accepts (10s) -> [Last Look 1s] -> RFQ_EXECUTION_UPDATE -> notify
```
- Direction: BUY/SELL. Side: YES-only (current API limit).
- Exit a position via SELL YES (another RFQ). At resolution: redeem.

## Key endpoints / infra
- RFQ WS:          `wss://combos-rfq-gateway-quoter.polymarket.com/ws/rfq`
- Combo markets:   `GET https://combos-rfq-api.polymarket.com/v1/rfq/combo-markets`
- Combo positions: `GET https://data-api.polymarket.com/v1/positions/combos`
- Relayer:         `https://relayer-v2.polymarket.com/`
- Split contract:  `0x30000034706c7d8e12009dab006be20000c031a8`
- Merge contract:  `0x12121212006e4CD160D18e3f00711DA5c3372600`

## Reuse vs net-new
| Area | Reuse | Net-new |
|---|---|---|
| Data API | `internal/polymarket/positions.go` (`DataAPIPosition`, `/positions?user=`) | `/v1/positions/combos` reader |
| EIP-712 | `orderv2.Builder.BuildSignedOrder`, sig-type enums, salt | taker-order build (if taker signs) |
| WebSocket | `live/pricefeed.go` reconnect/backoff/heartbeat patterns | RFQ gateway client + auth |
| CLOB auth | `GetOrCreateAPICredentials`, L2 HMAC | RFQ-channel auth |
| Wallet | `wallet.Manager`, proxy/Safe detection | approvals to split/merge/relayer-v2 |
| DB | repo + pgx + migration convention | combo tables + repo |
| Telegram | router, `prefix:arg` callbacks, `StateManager`, keyboards | `/combos` view, `/combo` builder |

---

## MILESTONE 1 — Read-only combo positions view  (DONE — pending manual TG check)
Goal: `/combos` Telegram command lists the user's existing combo positions. No signing, no WS.

- [x] Spike: captured real `GET /v1/positions/combos` response into
      `internal/polymarket/testdata/combos_positions.json` (4-leg FIFA WC parlay, RESOLVED_LOSS).
- [x] TDD: `internal/polymarket/combos_test.go` — fixture-backed decode + format tests.
- [x] `internal/polymarket/combos.go`: `ComboPosition`/`ComboLeg`, `CombosClient.GetComboPositions`,
      `FormatComboPositions` (config-driven base URL, 500 limit, lowercased addr).
- [x] Config: reused `DataAPIUrl` (covers it). `CombosRFQApiUrl` deferred to trading milestones.
- [x] Telegram: `internal/telegram/combos.go` `handleCombos` + `handleRefreshCombos`;
      registered `/combos` in `bot.go` (handlers, command menu) and `/help`.
- [x] Handle empty/no-combos gracefully ("No combo positions found.").
- [x] `go test ./...` green; build clean.
- [ ] Manual check in Telegram against the proxy wallet that holds the combo.

## MILESTONE 2 — Protocol discovery spike (gate for all trading)
Goal: nail the wire protocol the docs leave unspecified, esp. the taker accept/sign step.

- [ ] Pull `@polymarket/client@beta` source from npm; extract: WS auth handshake, accept message
      shape, **whether the taker signs**, combo-market list response, yes/no_position_id derivation.
- [ ] Confirm combo YES payout/resolution semantics and condition_id<->legs mapping.
- [ ] Read-only WS smoke test against the RFQ gateway with CLOB creds.
- [ ] Finalize `docs/combos-rfq-protocol.md` as the authoritative spec.

## MILESTONE 3 — RFQ WebSocket client  (`internal/polymarket/rfq/`)
- [ ] Connect + auth (CLOB creds); 30s ping/pong; reconnect/backoff (port from pricefeed.go).
- [ ] Codec: RFQ_REQUEST (out), RFQ_QUOTE / ACK_RFQ_QUOTE / RFQ_EXECUTION_UPDATE /
      RFQ_CONFIRMATION_REQUEST (in), accept (out). Table-driven codec tests.
- [ ] Session manager: one in-flight RFQ/user, 10s accept timer, best-price selection.

## MILESTONE 4 — Combo markets + pricing
- [ ] `GET /v1/rfq/combo-markets` client w/ short-TTL cache (mirror market-data caching).
- [ ] e6 base-unit math verbatim from docs (BUY/SELL YES x collateral/inventory, ceil/floor);
      table-driven tests.
- [ ] Pre-trade checks: pUSD balance + approvals to split/merge/relayer-v2 contracts.
- [ ] Taker signing path ONLY if Milestone 2 shows it's required.

## MILESTONE 5 — Database  (`migrations/006_combos.sql` + `.down.sql`)
- [ ] Tables: combo_requests, combo_legs (or JSONB), combo_quotes, combo_positions.
- [ ] `ComboRepository` following `repositories/sltp_arm_repository.go`.
- [ ] Lifecycle: pending -> quoted -> accepted -> executing -> filled|failed|expired.

## MILESTONE 6 — Telegram combo builder  (`internal/telegram/combo.go`)
- [ ] `/combo`: search/add legs -> pick YES + BUY/SELL -> size (notional|shares) -> review.
- [ ] Request -> live quote card w/ countdown -> Accept / Cancel.
- [ ] Callbacks: combo:add: combo:dir: combo:size: combo:req: combo:accept: combo:cancel:
- [ ] Push async exec updates (MATCHED/MINED/CONFIRMED/FAILED) to the user.

## MILESTONE 7 — Settlement
- [ ] `redeem(condition_id, outcome_index, amount)` via relayer-v2.
- [ ] Detect resolved combos; surface a Redeem button (mirror handlers_redeem.go).

---

## Cross-cutting
- [ ] Config: RFQ gateway URL, combos-rfq-api URL, relayer-v2 URL, split/merge contract addrs.
- [ ] Risk guards: slippage limit, max legs, single in-flight RFQ/user, idempotency on rfq_id.
- [ ] TDD throughout (mock WS server, codec tests, price-math tables) per CLAUDE.md.

## Top risks
1. Taker signing semantics unspecified in docs — Milestone 2 must resolve before any trading.
2. Beta API (separate gateways, relayer-v2) churn — isolate behind `rfq` package.
3. New on-chain approvals to split/merge/relayer contracts — easy to miss.
4. V2 migration interplay — verify domain/version assumptions don't collide with new contracts.
5. No Go SDK — protocol is hand-rolled/reverse-engineered, fragile to upstream change.

## Review
### Milestone 1 (read-only /combos) — landed
- Files: `internal/polymarket/combos.go`, `combos_test.go`, `testdata/combos_positions.json`,
  `internal/telegram/combos.go`; wired in `bot.go` (handler + menu + callback) and `handlers.go` (help).
- Combo semantics confirmed from live data: AND-parlay — one RESOLVED_LOSS leg loses the whole combo.
- Data API numerics are JSON **strings** (unlike `/positions` which returns numbers) — parsed via `parseFloat`.
- Network note: dev box has no direct outbound; reached the API via `http_proxy=http://192.168.1.108:7890`.
  The bot itself runs where data-api is reachable, so no proxy wiring added to the client.
- Reused `truncateUTF8`-style rune-safe truncation (local `truncateComboTitle`) per UTF-8 guideline.
- Not done: manual Telegram smoke test (needs the running bot + the user's account).
