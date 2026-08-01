
## 2026-07-20 — CLOB `success:true` is NOT a fill
Polymarket /order returns `success:true, status:"delayed"` with empty
taking/makingAmount on in-play markets (bet delay); the order may still be
killed. Never treat submit-acceptance as execution — parse `status`, and
for delayed orders poll the order (or verify the position) before acting
on "sold". Found in production: first v0.11.0 FOK stop "fired", disarmed,
and notified while zero shares actually sold. Always smoke-test new order
paths on an in-play market specifically — that's where the exchange
semantics differ. (Issue #22)

## 2026-07-26 — API test fixtures must be captured bytes, not transcriptions
The v0.11.2 balance-shortfall regex shipped dead: production CLOB bodies
escape `>` as the six literal bytes `\u003e`, my spec transcribed the log
un-escaped, the agent's
fixture matched the spec, tests passed, production never matched. When
testing against an external API's error/response formats, paste the raw
bytes from a captured body (verify with `cat -A` / hexdump), never retype
from a log or from memory. Corollary: a fix for a production-only failure
isn't done until re-verified against production traffic. (Issue #24 reopen)

## 2026-07-26 — During the bet delay, orders are unqueryable; absence ≠ death
GET /data/order/{id} returns nothing for an in-play delayed order until it
matches or is reaped — "gone" 1s after submit is the *normal* pending state,
not a kill verdict. Twice a "dead" FOK filled 3–4s later. Absence of
evidence from a status endpoint is not evidence of a terminal state; gate
"gone → dead" behind a grace window sized to the async process. (Issue #27)

## 2026-08-01 — Fix the pattern, not the instance
The #33 condition-ID bug was fixed only at the call site visibly erroring
that day (held positions); the identical call in the armed-token path
shipped broken and cost a live missed snipe alert (NS Game 2, 0.395→0.03
unwatched). When the root cause is "wrong API form for this input", grep
every caller of that API before closing — the silent ones are the ones
that bite. Corollary: silent-success paths (the seeder) need one debug
log line, or production verification is guesswork.
