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

---

# Pinned feed filter (v0.10.4)

User report: pinned game3/game4 panels showed the whole event's
activity ("all the event slug activities were flushed in").

Root cause: SubscribeWeb always called trackEventAssets → pinned
subscriptions mapped the parent Moneyline's token IDs to the pinned
slug. So (a) every series-winner trade flushed into a pinned panel,
(b) two pinned panels from one event overwrote each other's mapping
(last subscriber received all ML trades), (c) the pinned market's own
trades never matched — assets untracked, prefix fallback compares the
parent event slug against the longer pinned slug.

Fix (TDD, manager_pinned_feed_test.go):
- [x] SubscribeWeb: pinned subscription → trackMarketAssets (pinned
      clobTokenIds → subscription slug); event slug → ML as before
- [x] broadcastToWeb gains matchedByAsset: asset-matched trades bypass
      the allMarkets sub-market gate (they ARE the selected market)
- [x] Tests: pinned asset tracking, ML-not-tracked, per-panel routing
      (game3/game4 on one conn), ML trade matches nothing, indicator-
      slug pinned market delivered without allMarkets
- [x] Docs: CONTEXT.md Pinned Market (feed follows pin),
      web-trade-feature.md (single-destination note: pinned markets'
      trades no longer reach event-slug allMarkets panels)
- [x] Full suite + -race clean

Known edge (accepted): a Telegram subscription made with a market slug
still tracks ML assets under that slug (Telegram has no pinning) — if
combined with a web pin on the same slug, ML trades reach that panel.

---

# Breakeven-Trailing Stop-Loss Rework (SL/TP v2)

Plan: ~/.claude/plans/atomic-singing-petal.md
Why: 92-day data — fixed -30% SL realized -48% of basis over 62 fires,
29% of stops were comebacks; holding beat stopping by ~$1,470.
Params (user-chosen): activation = entry×1.20, trail = 20% below peak
floored at entry, FOK floor = trigger×0.90, retry-only (no market
escalation), no deep stop. TP side unchanged.

- [x] Stage 0 — Migration 008: high_water_mark column + backfill (+down)
- [x] Stage 1 — models.go: HWM field, SLActivationMult/SLTrailMult/
      SLMaxSlip, SLActive/SLTriggerPrice/SLFloorPrice (table tests first)
- [x] Stage 2 — repository: column plumbing, Arm upsert seeds HWM=avg,
      UpdateHWM monotonic
- [x] Stage 3 — executor plumbing: ExecuteSell(limitPrice, orderType),
      drop GTC hardcode in executeSellOrderFromPosition
- [x] Stage 4 — monitor: HWM ratchet + fakeStore.UpdateHWM
- [x] Stage 5a — dormant gate + evaluateSL restructure
- [x] Stage 5b — 30s confirmation debounce (fakeClock via m.now)
- [x] Stage 5c — FOK floor exec, single-flight, retry,
      disarm-after-success, pending notice
- [x] Stage 5d — interaction tests (TP/ceiling mid-breach, re-arm reset)
- [x] Stage 6 — telegram: NotifySLExitPending, fire/header/confirm text
- [x] Final — go test -race -count=2 ./... (green, 2 runs)

## Review

All stages TDD RED→GREEN; full suite + `-race -count=2` clean; `go vet`
clean. ~1,350 lines added across 9 files + migration 008 (+down) +
CONTEXT.md glossary (SL rewritten, HWM added).

Key mechanics as built:
- Trigger math on SLTPArm: SLActive (hwm ≥ avg×1.20), SLTriggerPrice
  (max(avg, hwm×0.80)), SLFloorPrice (trigger×0.90, clamp ≥0.001).
- Monitor: ratchetHWM on every evaluation (monotonic SQL guard);
  per-arm in-memory slArmState {breachStart, lastAttempt, inFlight,
  sold, pendingNotified} keyed by arm ID; 30s confirm window (20s tick
  guarantees coverage); FOK exit at floor; retry ≥30s; disarm only
  after a successful sell (sold flag makes disarm-retry re-sell-proof);
  TP/ceiling/dormant/recovery all wipe breach state.
- TP side untouched (still market-style GTC, ClearTP-first).
- 16 new/rewritten monitor tests (incl. fakeClock determinism,
  concurrency, restart, sold-but-disarm-failed), 3 new model tables,
  2 telegram text tables.

NOT deployed yet — deploy steps when user gives the go:
1. psql "$DATABASE_URL" -f migrations/008_sl_trailing_stop.sql
2. tag v0.0.XX → CI → bump tag in ~/workspace/poly_deploy → compose
   down/up; existing arms restart DORMANT (hwm=avg).
3. Smoke: arm a small position; first production FOK SELL — verify one
   live fire before trusting broadly.

# FOK Fill Confirmation — delayed orders are not fills (issue #22, 2026-07-20)

First live v0.11.0 FOK stop-loss fired on an in-play Dota 2 market: CLOB
returned `success:true, status:"delayed"`, the bot read it as a fill,
disarmed the position, and DM'd a false "stop fired" while zero shares
sold. Root cause: `submitOrder` parsed only `success` and never `status`.

- [x] TDD: FOK fill-confirmation tests (fake CLOB via httptest) — matched
      immediately; delayed→matched on poll; delayed→unmatched (no further
      polls); delayed→404/gone; delayed→timeout; ctx-cancel mid-poll;
      GTC delayed/live stays accepted (never polls); unparseable 200 →
      fail-closed (FOK + GTC)
- [x] `submitOrder`: parse `status`/amounts; FOK Success = confirmed fill,
      GTC/GTD unchanged (acceptance); flip unparseable-200 fallback to
      failure for all order types
- [x] Block-and-poll `GET /data/order/{id}` (L2-authed) every 2s / 60s
      timeout inside the trading client; poll interval + timeout are
      struct fields so tests shrink them; case-insensitive status;
      respects ctx cancellation; timeout = failure (safe direction)
- [x] Populate FilledSize/AveragePrice on every FOK fill (submit amounts,
      or poll size_matched/price approximation); SL fired text shows the
      avg fill price when known
- [x] ADR 0005; CONTEXT.md glossary (Acceptance / Fill / Bet Delay);
      lessons.md; `go test ./...`, `-race` (polymarket/live/telegram),
      `go vet` all green

## Review

Fix is contained to the trading client: `TradeResult.Success` is now
per order type — a killed delayed FOK returns `Success=false`, which the
SL monitor already handles correctly (keep arm, one pending notice,
retry ≥30s). No monitor change needed. TP/ceiling-TP untouched (still
GTC acceptance). Known accepted edge: a resting unfilled TP sell can make
a later SL under-sell by half — documented on issue #22, not fixed.

NOT deployed — user gates tag/deploy. Smoke: first production FOK SELL on
an in-play market must confirm one real fill before trusting broadly.

---

# SL/TP: tick-grid TP trigger (#25) + stale-size shortfall handling (#24) — 2026-07-21

Branch `fix/sltp-stale-size-and-tick-grid`, TDD red→green per stage,
one commit per issue.

## Issue #25 — TP trigger lands off the tick grid
- [x] TDD: TPTriggerPrice table extended — production case 0.2355→0.47
      (tick 0.01), 0.34→0.68, 0.2355→0.471 (tick 0.001), float-artifact
      0.235→0.47 (1e-6 epsilon), cap 0.60→0.99, tiny 0.003→0.01 clamp,
      TickSize 0 → 0.01 fallback; monitor regression: avg 0.2355 +
      bid 0.47 must fire TP (did not fire in production)
- [x] migrations/009: sltp_arms.tick_size DECIMAL(6,4) NOT NULL
      DEFAULT 0.01 CHECK (0, 0.1] (+ .down.sql)
- [x] SLTPArm.TickSize; TPTriggerPrice caps at 0.99 then floors to the
      grid; extra finding: n×tick can float above the book's price
      (47×0.01 > float64(0.47)) — snapped to 6-decimal precision so the
      monitor's bid >= trigger comparison is exact
- [x] Repository: tick_size in columns/scan/INSERT + DO UPDATE SET;
      repo normalizes tick <= 0 → 0.01 (column CHECK requires > 0)
- [x] Arm flow: armTickSize() fetches CLOB minimum_tick_size at arm
      time, defaults to 0.01 on any error (never blocks arming);
      table-tested against httptest fake CLOB
- [x] Display: armed/list text interpolates TPTriggerPrice() — shows
      the effective grid price automatically; no expected-text changes
      needed (0.99 cap case still renders $0.9900)

## Issue #24 — doomed retries after manual partial sell
- [x] TDD: exact production rejection body classified (400 and
      200-error-body paths); unrelated rejections not annotated
- [x] TradeResult.InsufficientBalance + AvailableSharesRaw (regexp on
      "balance is not enough -> balance: (\d+), order amount: (\d+)")
- [x] SL: shortfall > 0 → clamp later attempts to min(snapshot,
      balance), one stale-size notice per episode INSTEAD of thin-book
      pending; shortfall == 0 → latch sold-state (never resell),
      auto-disarm, notify, unsubscribe-if-last
- [x] TP + ceiling-TP: shared retryTPShortfall — one immediate clamped
      retry (> 0) or full disarm + auto-disarm notice, fired notice
      skipped (== 0)
- [x] Notifier.NotifySLTPStaleSize + telegram impl + pure
      sltpStaleSizeText (table-tested)
- [x] Monitor tests: SL>0 (one stale, zero pendings, clamped retry),
      SL=0 (one disarm, no further sells), TP>0 (exactly two executor
      calls, second clamped, GTC), TP=0 (disarm, no fired notice);
      regression: plain FOK kill still keeps arm + thin-book pending,
      zero stale notices

## Verification
- [x] go test ./... green
- [x] go test -race on live, polymarket, database, telegram (run
      per-package — RPi)
- [x] gofmt -l clean on all touched files

## Review
TP shortfall > 0 does not send the stale-size notice (spec only wires
it into the SL episode path; the TP retry outcome is reported via the
normal fired notice). SL trigger/floor deliberately unchanged — off-grid
SL triggers fire earlier (safe), floor is a limit not a trigger.
NOT deployed; migration 009 must be applied manually before rollout.

---

# FOK gone-grace (#27) + shortfall redux (#24 reopened) — 2026-07-27

Branch `fix/fok-grace-and-shortfall-redux`, TDD red→green, one commit
per issue.

## Issue #24 reopened — shortfall classification inert in production
- [x] TDD: byte-exact escaped-arrow production bodies (the CLOB
      JSON-escapes ">" as a u003e escape in the raw HTTP body) served
      by the fake CLOB (400 and 200-error-body paths); literal "->"
      kept as belt and braces; negative case unchanged
- [x] balanceShortfallRe tolerates both arrow forms
- [x] Sellability rule shortfallGone(availableRaw, price): gone when
      below 10_000 raw (0.01-share size precision) or value at price
      under the $1 CLOB minimum (price = FOK floor on the SL path,
      current bid on the TP/ceiling path)
- [x] handleSLShortfall takes floor; retryTPShortfall takes bid;
      gone → sold-latch + disarm + stale(0) notice (SL) or
      disarmGonePosition (TP/ceiling); sellable → clamp unchanged
- [x] Monitor tests: dust 16922 raw → auto-disarm, one stale notice,
      no further sells; 1.5 shares at $0.702 floor ($1.05) → clamp,
      retry sells 1_500_000; 1.4 shares at $0.702 ($0.98) → gone;
      TP 2026-07-26 production regression (escaped zero-balance body →
      disarm + stale notice, NO TP-failed fired notice)

## Issue #27 — gone verdict premature during bet delay
- [x] TDD: gone, gone, matched → Success with fill details (production
      case, both 404 and empty-body forms); gone for the whole grace
      window → dead, message mentions unfilled; existing gone subtests
      updated to the new semantics (gone only terminal after grace)
- [x] fokGoneGraceWindow field (default 15s, test-overridable like
      fokPollInterval): gone within the grace = still pending; real
      terminal statuses (matched/unmatched/canceled) unaffected; 60s
      fokConfirmTimeout still bounds everything, timeout = failure
- [x] Deliberately NO Data-API cross-check (fuzzy activity match risks
      false "sold" — the exact bug #22 removed); rationale + unproven
      fills-but-never-queryable case documented in ADR 0005 amendment

## Docs
- [x] ADR 0005 "Amendment (2026-07-26)": gone-during-grace is pending
- [x] CONTEXT.md Bet Delay: order not queryable during the delay,
      Acceptance is the only signal

## Verification
- [x] go test ./... green
- [x] go test -race -count=1 on live + polymarket (per-package — RPi);
      one unreproducible polymarket -race failure before widening the
      test grace windows to 150ms, stable across 16 runs after
- [x] gofmt -l clean on all touched files

---

# Comeback Snipe v1 (#29) — 2026-08-01

Branch `feat/comeback-snipe`, TDD red→green per stage, spec + staged plan
in issue #29; glossary semantics (Comeback Snipe, Session High) binding
from CONTEXT.md.

## Stage 1 — SnipeWatcher core (internal/live/snipe_watcher.go)
- [x] Constants SnipeCompetitiveBid=0.40, SnipeCrashAsk=0.18;
      snipeResetAsk() derived midpoint (expression, not a 0.29 literal)
- [x] Per-token {sessionHigh, alerted, boughtThisMatch} under a mutex,
      in-memory, restart-resettable; MarkBought latches for the match
- [x] evaluate(tokenID, bid, ask): ratchet high on bid; alert on
      high ≥ 0.40 ∧ 0 < ask ≤ 0.18 ∧ !alerted ∧ !bought ∧ in-play;
      episode reset when ask > midpoint
- [x] In-play gate: Gamma market-level gameStartTime (startDate is the
      listing date; acceptingOrders is true pre-game — neither works);
      unknown start never alerts
- [x] Recipient resolution injected (SnipeRecipientResolver: event
      subscribers + arm owners); holders tracked internally with TTL
- [x] Tests: exact boundaries (0.40/0.18), high 0.39 / ask 0.19 no-fire,
      flap 0.17↔0.19↔0.17 → ONE alert, Kudermetova replay (recover >0.29,
      re-crash → second alert), MarkBought silence, fresh-watcher restart,
      pre-start gate, concurrent evaluate fires once

## Stage 2 — Universe + feed wiring (internal/live)
- [x] Subscribed events: tokens watched while any telegram/web subscriber
      exists (new HasAnySubscribers), released with the last; pinned
      subscriptions watch the pinned market, others the ML markets —
      mirroring the trade feed's resolution
- [x] Armed tokens ride the SL/TP monitor's existing feed subscription
      (no watcher ref); held tokens Subscribe with SnipeHeldTTL=6h,
      lazily pruned + 10-min janitor for quiet tokens
- [x] polymarket.ParseGameStartTime (space-offset + RFC3339 layouts);
      GammaMarket gains gameStartTime + clobTokenIds parsing; MarketInfo
      gains GetGameStartTime
- [x] NewSnipeRecipientResolver adapter (SubscriptionRegistry + arm store)
- [x] Fixed in review: releasing an armed-only token must not Unsubscribe
      the SL/TP monitor's feed ref — unsubscribes strictly by feedRef

## Stage 3 — Alert + tap (internal/telegram/snipe.go)
- [x] Pure builders (table-tested): alert text (was $high / now $ask,
      payout multiple), repriced text, filled text; [Snipe $10][Snipe $25]
- [x] In-memory alert registry: base36 sequential alertID → token/market
      info, 12h TTL, lazy pruning; callback `snipe:<alertID>:<10|25>`
      (token IDs ~78 digits can't ride 64-byte callback data)
- [x] Tap: atomic claim (used/expired IDs answer and never buy twice);
      repricing guard re-checks BestAsk, refuses > SnipeCrashAsk*1.5 or
      unavailable with a "repriced — not buying" edit; buy through the
      existing executeBuyOrderByIndex market path; MarkBought on fill;
      Arm SL/TP offered via the existing sltp_list flow on the fill message
- [x] Universe hooks: arm handler + boot seed register armed (Gamma
      metadata off-path), disarm unwatches; positions fetches (/positions,
      refresh, sell list, SL/TP list) register held with TTL
- [x] Wired in cmd/bot/main.go

## Verification
- [x] go test ./... green
- [x] go test -race -count=1 ./internal/live/ ./internal/telegram/ green
      (per-package — RPi)
- [x] gofmt -l clean on all touched files
- [x] go vet ./... clean

## Review
Armed tokens auto-disarmed by the monitor (SL/TP fire, gone position)
keep a stale armed-source entry in the watcher until restart — recipients
resolve live via the arm store, so no wrong alerts, just idle state.
Guard refusal consumes the alert (terminal "repriced" state by design);
a fresh alert requires recovery above the midpoint and a re-crash.
Web-panel subscribers keep tokens watched but alerts are Telegram DMs
only (web has no chat identity) — matches issue #29's v1 scope.
NOT deployed; no DB migration (all state in-memory by design).

---

# Telegram Feed Batching (issue #31, 2026-08-01)

Live feed flooded Telegram (13 msgs/min of dust prints on the tennis
sub) and shared the send path's rate budget with SL/TP + snipe alerts.

## Batcher core (internal/live/feed_batcher.go)
- [x] Policy constants: feedBatchWindow=5s, FeedMinTradeUSD=20.0,
      feedBatchMaxLines=10 — package-level product policy, no config
- [x] FeedBatcher: buffers per (chatID, eventSlug); first buffered trade
      arms a one-shot flush timer (no idle ticks); flush sends ONE
      message: "[SHORT-SLUG] Live trades" header + up to 10 lines newest
      last + "+N more trades" tail; Flush (unsubscribe) delivers pending
      immediately and stops the timer; FlushAll = shutdown drain
- [x] feedTimer interface over time.AfterFunc — tests fire flushes
      deterministically, no sleeps

## Wiring (internal/live/manager.go, formatter.go)
- [x] broadcastToTelegram → batcher.Add per subscriber; filters on
      trade.Size (RTDS "size" is the USDC value both feeds show as $)
- [x] FormatForTelegram → FormatTelegramLine (event prefix moved to the
      batch header; per-line body unchanged)
- [x] UnsubscribeTelegram / UnsubscribeAllTelegram flush pending;
      Stop() drains best-effort
- [x] Web path untouched: broadcastToWeb unfiltered, unbatched
- [x] SL/TP + snipe stay direct: Notifier/SnipeNotifier implementations
      call b.sendMessage / b.sendMessageWithKeyboard, never the batcher

## Tests (feed_batcher_test.go, feed_batcher_wiring_test.go)
- [x] Floor: 19.99 dropped (no timer), 20.00 kept
- [x] Single flush keeps buffered line order; window value asserted
- [x] Cap at 10 lines + "+3 more trades" count
- [x] Separate chats / separate subscriptions never share a buffer
- [x] Unsubscribe flushes immediately; stopped timer can't double-send
- [x] Empty buffer sends nothing (Flush + FlushAll)
- [x] New window after flush excludes old lines
- [x] Concurrent adds during flush race-safe, every add accounted once
- [x] Manager wiring: batched relay, sub-floor drop, unsubscribe flush,
      web frame still immediate for a sub-floor trade

## Verification
- [x] go test ./... green
- [x] go test -race -count=1 ./internal/live/ ./internal/telegram/ green
      (per-package — RPi)
- [x] gofmt -l clean on touched files; go vet ./internal/live/ clean

## Review
Flush sends outside the batcher mutex, so a slow Telegram send can't
block new windows. Kept lines are the FIRST 10 of the window ("+N more"
counts later overflow) — bounded memory during a flood. 429
retry/backoff in sendMessage noted out of scope per the issue; not
trivial from the batcher's side, left alone. NOT deployed.

---

# v0.12.2 — Snipe threshold, quiet subscriptions, Session High seeding (2026-08-01)

Branch feat/snipe-v0.12.2, three commits (one per change).

## Change 1 — SnipeCrashAsk 0.18 → 0.20 (product policy)
- [x] SnipeCrashAsk = 0.20; snipeResetAsk() midpoint 0.30 and telegram
      guard SnipeCrashAsk*1.5 = 0.30 stay derived, never literals
- [x] Boundary tests fire at ask 0.20, not at 0.21; reset above 0.30;
      flap case moved to 0.19/0.21/0.19 around the new bar
- [x] No stale 0.18/0.27/0.29 literals (remaining 0.18s are an
      unrelated fee expectation and a history-series data point)
- [x] CONTEXT.md "Comeback Snipe": crash ≤ 0.20, midpoint 0.30 today

## Change 2 — Quiet-by-default subscriptions, /live <slug> tape opt-in
- [x] Registry: telegramSubs/userEvents values = per-(chat, slug) tape
      flag; SubscribeTelegram(chatID, slug, tape) applies the flag
      unconditionally on re-subscribe (both directions) while keeping
      the newly-subscribed bool; membership checks comma-ok
- [x] New TapeSubscribers + IsTapeSubscribed; GetTelegramSubscribers
      still returns ALL telegram subscribers (routes snipe alerts)
- [x] broadcastToTelegram → TapeSubscribers only; unsubscribe paths,
      batcher flushes, web path unchanged
- [x] /live <slug> [tape]: case-insensitive keyword, usage on anything
      else (pure parseLiveArgs, table-tested); confirmation states the
      mode; /subs marks each subscription quiet/tape
- [x] CONTEXT.md "Live Feed": telegram tape opt-in per subscription;
      default = quiet (snipe watch + web tape only)
- [x] Tests: flag toggle both directions; quiet + ≥$20 trade → no
      batcher entry while web frame delivers; tape upgrade starts
      delivery; quiet unsubscribe still reports success

## Change 3 — Seed Session High from CLOB price history
- [x] polymarket: TradingClient.MaxTradePriceSince — public GET
      /prices-history?market=&interval=1d&fidelity=5, max p with
      t >= since; (0,false) on error/empty/malformed/out-of-range;
      httptest table tests incl. since cutoff + unreachable server
- [x] live: SnipeHistorySeeder interface, SetHistorySeeder (nil = old
      behavior); one seeding goroutine per NEW token state via
      ensureStateLocked (event/armed/held all funnel through it);
      since = GameStart−2h, else now−6h; seedSessionHigh raises only,
      mutex-guarded, never touches episode latches
- [x] cmd/bot/main.go: existing TradingClient wired as seeder
- [x] CONTEXT.md "Session High": seeded at watch-start, ratcheted
      live, restart re-seeds ("restart cannot alert" caveat removed)
- [x] Tests: Nongshim late-watch replay (seed 0.495 → bid 0.10 /
      ask 0.20 alerts); lower seed never lowers ratcheted high; late
      seed after alert+MarkBought keeps latches; at most one seed per
      token state; failed fetch leaves high untouched; nil seeder
      unchanged; concurrent seed+evaluate under -race

## Verification
- [x] go test -count=1 ./... green
- [x] go test -race -count=1 ./internal/live/ ./internal/telegram/
      ./internal/polymarket/ green (per-package — RPi)
- [x] gofmt -l clean on all touched files; go vet clean on touched pkgs

## Review
Registry bool values changed meaning (membership → tape flag): every
membership check now uses comma-ok; UnsubscribeTelegram had relied on
the value and would have broken for quiet subs — covered by test.
Snipe alert routing (GetTelegramSubscribers) and HasAnySubscribers are
mode-blind by design. Seeder field is read unguarded — documented
"set before Start", matching SetSnipeWatcher's wiring pattern.
NOT deployed; NOT pushed (branch local, PR to follow as v0.12.2).
