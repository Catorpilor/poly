# 10. Combo buys never report failure without a terminal status

Date: 2026-10-22

## Status

Accepted

## Context

ADR 0005 makes FOK fill confirmation block for up to 60s and treat a timeout
as failure — the safe direction for an *exit*: a false "failed" just keeps the
stop-loss armed and it retries.

A Combo buy (a **Take** of a Combo Quote) runs the other way. Polymarket's
Requester docs: "A local polling timeout does not mean the trade failed;
resume polling the same `rfq_id`" — a Take can sit in
`AWAITING_MAKER_CONFIRMATION` / `EXECUTING` / `MINED` / `RETRYING`. A false
"failed" on a *buy* invites the user to tap Buy again, and if the first Take
then fills they hold the Combo twice — the double-exposure class fixed in
#113.

## Decision

A Combo buy has three outcomes: **Filled**, **Failed** (only on a terminal
`FAILED` / `EXPIRED` / `CANCELED`, or a rejection before anything executed),
and **Pending**.

- The bot polls the RFQ status for ~30s inline, then edits the message to
  "⏳ Pending" and keeps polling in the background (every few seconds, up to
  ~10 minutes), editing the message again on a terminal status.
- While a Take is Pending on a Combo, that user's stake buttons for the same
  Combo refuse with "you already have a pending buy on this combo".
- If polling ends unresolved, the message says the outcome is **unknown**
  (check `/positions`) — never "failed".

Pending Takes are in-memory (soft rail, like the resting-order gate): a
restart loses the background poll and the block; `/positions` remains the
truth.

## Consequences

- Deliberately inconsistent with ADR 0005: exits fail on timeout, combo buys
  do not. The asymmetry follows which mistake is recoverable.
- A restart during a Pending Take can leave a stale "⏳ Pending" message; the
  combo still shows in `/positions` if it filled.

## Addendum (2026-10-22): sells

Combo sells (RFQ `SELL`, priced in shares) follow the same three outcomes —
**Sold**, **Failed** only on a terminal status, **Pending** with background
polling and an *unknown* (never "failed") ending — and block further sells
of the same Combo position while one is Pending. A double sell is less
harmful than a double buy (the wallet cannot sell shares it no longer holds),
but one rule for every Combo Take keeps the flow predictable and reuses the
same machinery. The Multiplier Tolerance applies to proceeds per share.
