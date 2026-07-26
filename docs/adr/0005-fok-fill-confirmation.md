# 5. Confirm FOK fills — delayed orders are not executions

Status: accepted
Date: 2026-07-20

## Context

On 2026-07-20 the first live trailing-stop FOK exit fired on an in-play Dota 2
market and produced a false "stop fired" notification while zero shares sold.
In-play markets carry a bet delay: the CLOB *accepts* the order
(`success: true`, `status: "delayed"`, empty `takingAmount`/`makingAmount`) and
matches or kills it only after the delay elapses. The order — a FOK at limit
0.35 against a 0.28 book — was correctly killed, but `submitOrder` parsed only
`success`, so `TradeResult.Success = true` was read as a fill. The SL state
machine then disarmed the position and DM'd the user while 199.99 shares were
still held and unprotected.

Two latent hazards compounded it: `submitOrder`'s blanket-success fallback
returned `Success: true` whenever the 200-response body failed to parse, and
`FilledSize`/`AveragePrice` were never populated, so nothing downstream could
tell acceptance from execution.

## Decision

`Success` is defined per order type. For **FOK** it means a CONFIRMED FILL; for
**GTC/GTD** it keeps meaning "accepted" (a resting order is success) — those
paths are unchanged.

Fill confirmation is synchronous and lives inside the trading client:

- A FOK submit returning `matched` fills immediately from the response amounts
  (`takingAmount`/`makingAmount`, side-dependent).
- A FOK submit returning `delayed`/`live` (or an unknown status) blocks and
  polls `GET /data/order/{orderID}` (L2-authed) every 2s, up to 60s: `matched`
  → fill (fields approximated from `size_matched`/`price`); `unmatched`,
  canceled, or 404/gone → failure; timeout → failure. The poll respects context
  cancellation. No tri-state or pending result crosses the client boundary — the
  caller gets a plain `TradeResult` with the truth.
- The unparseable-200 fallback is flipped to `Success: false` for all order
  types.

Alternative considered — **async pending state in the monitor**: return a third
"pending" outcome from `ExecuteSell` and have the SL monitor track it, re-poll,
and disarm later. Rejected: it spreads exchange-delay semantics across the
monitor's already-subtle state machine (breach debounce, single-flight, sold
latch), and the monitor's existing `Success=false` behavior (keep the arm, send
one pending notice, retry ≥30s) is already exactly right for a killed delayed
FOK. Keeping the block inside the client leaves callers untouched.

## Consequences

- `ExecuteSell` can block up to 60s on an in-play market while a delayed FOK
  resolves. Acceptable: SL evaluation already runs off the WS read loop in a
  single-flight goroutine, so one slow confirmation stalls only that arm's next
  attempt, not the feed.
- Timeout is treated as failure — the safe direction. A false "failed" is
  recoverable (the arm survives and retries); a false "sold" silently drops
  protection.
- A fill that lands *after* the 60s timeout leaves a stale arm. Its retries
  bounce harmlessly (the position is gone, later sells no-op/reject), and the
  next evaluation reconciles — no double-sell, at worst a redundant attempt.
- Confirmed fills now carry `FilledSize`/`AveragePrice`, so the SL notification
  and the lottery message can report the actual average price.

## Amendment (2026-07-26): gone during the bet delay is pending, not dead

Production falsified the "404/gone → failure" rule for freshly delayed
orders: in-play delayed orders are not queryable via `GET /data/order/{id}`
during the bet delay at all, so the first poll always saw "gone" and the
loop returned dead ~1s after submission. Twice the order then FILLED 3–4
seconds after the premature verdict (2026-07-23 18:00:16 accept → 18:00:19
fill; 2026-07-26 15:50:56 accept → 15:51:00 fill), leaving a false "book too
thin" notice and a stale arm (issue #27).

Amended decision: within the first `fokGoneGraceWindow` (default 15s) of
polling, a gone/404 result is treated as still-pending and polling
continues; past the window, gone → dead as before. Real terminal statuses
(matched / unmatched / canceled) keep their immediate effect at any time,
and `fokConfirmTimeout` (60s) still bounds the whole confirmation — timeout
remains failure.

Deliberately NO Data-API cross-check (`/activity` / `/positions`) before
declaring failure: the tape carries no order ID, so matching a fill to this
order is fuzzy, and a wrong match produces a false "sold" report — exactly
the bug issue #22 removed. If an order fills but never becomes queryable (an
unproven case), the failure direction stays safe: the fill executed at ≥ the
FOK floor, and the next retry self-heals through the balance-shortfall path
(issue #24) — the stale arm is clamped to the real balance or auto-disarmed
instead of retrying forever.
