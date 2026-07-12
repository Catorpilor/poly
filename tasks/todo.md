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

## Phase 3 — WebSocket write safety (F6–F8) ✅
- [x] RegisterConn on connect; WriteConn = single serialized write path
      with 5s deadline; sendResponse and broadcastToWeb both use it
- [x] broadcastToWeb drops conn on write error (dead client can't stall
      the feed); old GetConnWriteMutex API removed
- [x] rtdsWriteMu for upstream RTDS conn (pingLoop vs subscribe writes)
- [x] go test -race green: concurrent-writer serialization test + direct
      ack-vs-broadcast race test + drop/deliver behavioral tests

## Phase 4 — Shared trade executor (F9, F10) ✅
- [x] polymarket.TradeExecutor: creds → TestL2Auth → fee discovery
      (best-effort) → ExecuteTrade; httptest-tested against fake CLOB +
      Gamma (happy path, L2 abort-before-order, creds abort, fee outages)
- [x] handleTrade rewired: parse → validate → auth → resolve → Execute;
      web path gains the L2 auth pre-check (F10)

## Phase 5 — Polish (F11 cut) ✅
- [x] Method-scoped routes (GET/POST patterns) + /api/ JSON fallback
      (known path wrong method → 405, unknown path → 404; never the
      file server's HTML 404); four manual method checks deleted
- [x] http.Server timeouts (header 5s, read/write 30s, idle 120s;
      gorilla clears deadlines on /ws hijack so WebSocket unaffected)
- [x] Hardcoded poly_trade_test_bot fallback removed — auth-init 503s
      with a log line when TELEGRAM_BOT_USERNAME unset (prod .env sets it)
- [x] Server-side max amount 1000 USDC in validateWebTrade

## Review

All five phases landed on refactor/web-live-trading, one commit each,
TDD RED→GREEN throughout; `go test ./...`, `-race`, and `go vet` clean
after every phase.

- Phase 1 (a30b66d): CSRF/DNS-rebinding guard on /api/* and /ws —
  Host allowlist, Origin==Host, JSON content type. 13 httptest cases.
- Phase 2 (d018b4c): {marketIndex, outcomeIndex} protocol; soccer's
  third outcome tradeable (F2), negative-index panic fixed (F3), web
  is buy-only (F4), dead marketId branch gone (F5). Pure
  resolveWebTrade()/validateWebTrade() with 26 table cases.
- Phase 3 (3a8daa7): single serialized write path per web conn with 5s
  deadline + drop-on-error (F6/F7); upstream RTDS write mutex (F8).
  Real-WebSocket concurrency tests under -race.
- Phase 4 (7a89dfe): polymarket.TradeExecutor (creds → L2 pre-check →
  best-effort fees → ExecuteTrade); web rewired, gains TestL2Auth
  (F10); Telegram executors untouched per Q5. Fake CLOB/Gamma tests.
- Phase 5: method-scoped routes, server timeouts, no hardcoded bot
  username, 1000 USDC cap.

Deploy notes:
- Refresh any open browser tab after deploying — the embedded frontend
  and the wire protocol changed together (stale JS would send the old
  field names; server ignores unknown fields, so a stale 3-way trade
  would silently target market 0).
- Prod .env: TELEGRAM_BOT_USERNAME already set; LIVE_WEB_URL unset is
  fine while browsing by IP/localhost (set it if using an mDNS name).
- Deferred by decision: bearer-token sessions (until the page leaves
  the LAN), Telegram executor migration onto TradeExecutor.

---

# Sub-Market Trading via Market Picker (grilled & confirmed 2026-07-12)

Goal: trade any active market in a subscribed event (e.g. LoL
`lol-hle1-ly-2026-07-11-game1` game winner), not just the Moneyline.
UI shape confirmed: market picker in the event panel (not
trade-what-you-see feed buttons).

Grilled decisions:
- Q1 **(a)** v0.9.0 merged/tagged/deployed first ✅; picker ships as its
  own release for isolated blast radius.
- Q2 **(a)** Picker lists sub-markets only (event markets minus
  GetAllMLMarkets, active && !closed) — ML keeps its dedicated buttons.
- Q3 **(a)** Rows show indicative outcomePrices (resolver cache ≤5 min
  stale; fills still price off the live book, VWAP + 2% slippage).

Branch: feat/web-submarket-trading off main (post-#14).

Design decisions:
- Sub-markets are addressed by **market slug** (stable identity), not
  by index into the full market list (ordering-fragile). ML buttons
  keep using marketIndex.
- Picker data comes from a REST endpoint fetched when the picker is
  opened (fresh active/closed state), not baked into the subscribe
  response. Freshness bound: resolver caches events 5 min — a market
  that closes mid-game may linger in the picker briefly; the CLOB
  rejects orders on closed markets, so worst case is a clean error.
- Same buy-only rule, same 1000 USDC cap, same amount input per panel.
- Glossary: add **Sub-market** to CONTEXT.md (term already lives in
  code as isSubMarketSlug and the "All Markets" toggle).

## Phase A — Backend: list + resolve + trade by slug (TDD) ✅
- [x] GET /api/events/{slug}/markets (guardAPI): active non-closed
      sub-markets with slug/question/outcomes/prices; 404 unknown event
- [x] webTradeData gains optional marketSlug; marketIndex ignored when
      set; outcomeIndex still 0/1
- [x] resolveWebTradeBySlug: slug match over all event markets;
      closed/inactive/unknown/bounds rejected. Table-tested.
- [x] **Classifier fix found en route**: "Game N Winner" counted as ML
      ("map " keyword existed, "game " didn't) — LoL Bo3 rendered as a
      fake 3-way in prod. "game " added + Bo3 regression case; existing
      NBA/soccer/tennis/esports tables unchanged.

## Phase B — Frontend: picker UI ✅
- [x] "Markets ▾" toggle in each panel's trade section; fetch on every
      open (fresh closed-state); loading / error / empty states
- [x] Rows: question + per-outcome buy buttons with indicative cent
      prices; click → executeTrade(eventSlug, 0, idx, marketSlug)
- [x] Reuses panel amount input and button-disable loading state;
      inline JS syntax-checked with node --check

## Phase C — Docs & wrap-up ✅
- [x] docs/web-trade-feature.md: marketSlug protocol, picker endpoint,
      "game " keyword in the classification table, thin-book caution
- [x] CONTEXT.md: Sub-market entry
- [x] Full suite + -race clean; PR opened
- [ ] Manual smoke on a live esports event (picker lists game markets;
      buy on a game-winner market) — user, post-deploy

Caution (told to user): sub-market books are thin; VWAP + 2% slippage
walks them — the 1000 cap bounds damage but spreads cost more than ML.
