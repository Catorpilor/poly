# Fix #64 — buys never register a Held Watch

Decisions (grilled 2026-08-13/14):
- Scope: BOTH buy surfaces (web + Telegram), invariant "every successful BUY registers its buyer as a holder"
- Web seam: new `LiveTradeManager.RegisterHeldBuy(telegramID, eventSlug, tokenID, eventInfo)` — nil-safe, reuses `snipeMarketsFor`, calls `WatchHeld` with `SnipeHeldTTL`; called from `handleTrade` after trade success only, async, log-only failures
- Telegram seam: `go b.snipeRegisterHeldForUser(chatID, proxyAddr)` after successful buy (portfolio refetch — also rescues pre-fix positions)
- Bought latch: web buys NEVER call MarkBought (token-level latch would silence alerts for everyone)
- Recipient semantics: buy-sourced holders are FULL recipients (incl. $10 auto-buy / $5 deep) — identical to view-sourced holders
- Durability: in-memory + 6h TTL accepted (no boot re-seed)
- Implementation by opus subagent, TDD (red-green-refactor), branch + PR

## Plan

- [x] RED: failing tests (compile-failure red on all three seams, then green)
- [x] GREEN: minimal implementation (manager method + web handler call + telegram hooks)
- [x] REFACTOR + `go test ./...` + `go test -race` (verified independently, uncached)
- [x] Verify: no MarkBought on any new path; registration async off response/render paths
- [x] Open PR referencing #64

## Review

Shipped on branch `fix/snipe-held-on-buy` (+62 prod lines, 3 new test files):

- `internal/live/manager.go` — `RegisterHeldBuy` searches ALL event markets (sub-market buys included), registers via `WatchHeld`/`SnipeHeldTTL`, logs unknown tokens.
- `internal/live/webserver.go` — async hook after both trade-failure early returns; `tradeExecutor` field converted to a small `webTradeExecutor` interface (test seam, mirrors the existing `watches` seam).
- `internal/telegram/bot.go` — hooks in the two (and only) standard buy-success blocks: market buy (`handleBuyExecuteCallback`) and limit buy (`handleBuyLimitPriceInput`). Snipe-tap path deliberately not hooked: it MarkBoughts the token, so holder registration there is moot.
- `internal/telegram/snipe.go` — position scanner behind a nil-defaulting test seam.

Notes:
- A resting Telegram limit order only refreshes existing holdings until it fills (portfolio scan sees actual positions) — commented at the hook.
- Restart still clears held watches (accepted); the next buy or positions view re-registers.
