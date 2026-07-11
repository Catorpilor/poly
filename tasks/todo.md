# Web Live-Trading Refactor (grilled & confirmed 2026-07-12)

Review findings F1–F11: see git history of this file / PR description.
Grilled decisions:
- Q1 **(a)** LAN-only stands; cheap hardening only (JSON content type,
  Host/Origin checks). Bearer sessions deferred until the page leaves the LAN.
- Q2 **(a)** Web endpoint is buy-only; SELL rejected with "sell via Telegram".
- Q3 **(b)** Unified wire protocol `{marketIndex, outcomeIndex}`; `yesNoIndex`
  deleted; pure `resolveWebTrade()`; glossary: Market Index vs Outcome Index.
- Q4 **(a)** Per-conn write mutex + write deadline + drop-on-error; mutex on
  upstream RTDS conn writes. No hub/goroutine-per-client.
- Q5 **(a)** Shared trade executor; rewire web only. Telegram executors and
  SL/TP untouched.
- Q6 Polish cut: method-scoped routes, http.Server timeouts, drop hardcoded
  bot-username fallback, server-side max amount (1000, matches UI),
  CheckOrigin shares the same origin predicate. Skipped: auth-init rate
  limit, confirm dialog, Encode-error plumbing.
- Shipping: one branch, one commit per phase, TDD each phase, single PR,
  tag v0.9.0 on merge, deploy on user's go. No new ADRs (all reversible).

## Phase 1 — Request hardening (F1) ✅
- [x] TDD: httptest tests — wrong Content-Type 415; evil Origin 403;
      non-IP/localhost Host 403; legit LAN/localhost requests pass
- [x] JSON Content-Type required on all /api/ POSTs (frontend already
      sends it everywhere, incl. body-less auth/init — no frontend change)
- [x] Host must be localhost/IP-literal/configured host; Origin (when
      present) must match request Host — applied to /api/* and /ws upgrade
      (CheckOrigin now shares the same predicate, Q6 item landed early)
- Note: prod .env has no LIVE_WEB_URL; browsing by IP/localhost works
  as-is, browsing by mDNS hostname would need LIVE_WEB_URL set

## Phase 2 — Protocol & correctness (F2–F5) ✅
- [x] TDD: table tests for pure resolveWebTrade() + validateWebTrade() —
      2-way, 3-way (F2 regression: marketIndex 2 tradeable), bad indexes
      (F3 regression: negative index errors instead of panicking), missing
      markets/tokens; handler-level tests for buy-only and required slug
- [x] Wire protocol: {marketIndex, outcomeIndex}; yesNoIndex deleted
      (server + index.html together); dead outcomeName block removed
- [x] Buy-only: side != BUY rejected with pointer to Telegram
- [x] Dead marketId branch removed; eventSlug required; validation runs
      before dependency checks; docs/web-trade-feature.md API spec updated

## Phase 3 — WebSocket write safety (F6–F8)
- [ ] Register per-conn write mutex on connect; sendResponse uses it
- [ ] Write deadlines in broadcastToWeb; drop conn on write error
- [ ] Mutex for upstream RTDS conn (pingLoop vs subscribe writes)
- [ ] go test -race with concurrent ack/broadcast test

## Phase 4 — Shared trade executor (F9, F10)
- [ ] Extract helper: user → decrypt → creds → TestL2Auth → fees →
      TradeRequest → ExecuteTrade
- [ ] Rewire handleTrade onto it; handleTrade = parse/validate/resolve/execute

## Phase 5 — Polish (F11 cut)
- [ ] Method-scoped routes ("POST /api/trade" etc.)
- [ ] http.Server ReadHeaderTimeout/ReadTimeout/IdleTimeout
- [ ] Remove hardcoded poly_trade_test_bot fallback (disable auth-init +
      log error when username unconfigured)
- [ ] Server-side max amount 1000 on /api/trade

## Review

(filled in as phases complete)
