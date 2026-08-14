
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

## 2026-08-01 — An API that ignores unknown params returns plausible garbage
Gamma /markets silently ignores unrecognized query params and serves the
default market list: `?condition_id=` (wrong, singular) "worked" for weeks
while resolving a LoL condition ID to a politics market. Two rules: (1)
when adding an API call, prove the filter actually filters (query a known
id, assert identity on the response); (2) lookup helpers must validate
response identity against the request, so a filter regression fails loudly
instead of propagating someone else's market. Corollary of the fixture
lesson: a fixture that honors your parameter name can't catch you using
the wrong one — model the API's ignore-and-default behavior in the fake.

## 2026-08-05: Never settle P&L by structural inference — verify each market

Mis-settled a ledger row +$89.59 (actual: −$9.96) by inferring "a Game 3
market exists, therefore Falcons won Game 2" — false, the series was longer
than BO3. The wrong outcome then contaminated an issue report (#52 claimed
a missed TP during a winning ride that never happened) before the user
corrected it. Rules: (1) an outcome is settled ONLY by direct evidence for
THAT market — Gamma `closed=true` outcomePrices, or the position's own
curPrice/redeem record — never by series topology, sibling markets, or
what "must have" happened; (2) when a settlement propagates into other
artifacts (issues, memory, running totals), correcting it means correcting
every downstream artifact, so verify before writing, not after; (3) the
user watching the game outranks any inference chain.

## 2026-08-14 — A monitor's filter is part of every feature's definition of done
The log monitor forwarded `seeded` init noise (8 useless wakes in one
night, risking the harness auto-stopping it — which kills ALL alerting
silently) while NOT matching `deep-crash`, so a whole alert tier ran for
a week invisible except by manual log reads. Two rules: (1) when a
feature adds a log line class (deep-crash, gate skips), extending the
monitor filter ships WITH the feature; (2) filters earn their keep in
both directions — every line class you would act on, no line class you
wouldn't. Seed/startup/init lines are never actionable.

## 2026-08-14 — Before filtering a losing pattern, run the filter over past winners
"Falling knives" (14/22 alert episodes corpsed since 08-13) pattern-matched
to an obvious fix — delay the auto-buy until the price stabilizes. The
ledger killed it: the two biggest winners (HANJIN +$84, WE +$56) bounced
inside any plausible delay window, and the ≤0.30 repricing guard would
then have refused the recovered price — the delay filters winners at
least as hard as knives. The variables that actually separated corpses
from comebacks were sport (tennis 0/5, all winners esports) and fresh
own-side spread, neither of which is time. Rule: a proposed filter is
evaluated by replaying it over the ledger's WINNERS first; symptom-shaped
fixes get rejected by data, not by intuition.

## 2026-08-14 — Gate mechanisms with conditions, don't switch them off
Recommended suspending the 0/11 deep-crash auto-buy; user redirected to a
holdings check instead (skip the $5 only when the recipient already holds
the side) — which kills 11/11 of the observed losses while preserving the
tier's remaining valid case (catch-up entry when the in-band buy never
funded). The pattern generalizes and matches zeh's standing preference:
find the condition that separates the mechanism's losses from its wins
and gate on that; blanket off-switches destroy the option value and the
September-review data stream. Alert delivery itself is NEVER gated.

## 2026-08-14 — A missing log line only proves absence if THAT path logs
Diagnosing #67 I inferred "the buy hook never ran" because no "Fetching
positions" line followed the fill — but that line belongs to
ScanAllStrategies (the display path); the hook's GetPositions is silent
on success. The false inference cost a detour through three wrong
hypotheses (stale binary, nil watcher, phantom third handler) before
reading the callee settled it. Rules: (1) before treating a missing log
line as proof a code path didn't execute, open the exact callee and
confirm it emits that line — sibling functions logging similar things
don't count; (2) corollary to the 08-01 silent-success lesson: a silent
helper on a decision path (the positions read that gates registration)
earns one debug line, because its silence is indistinguishable from its
absence.

## 2026-08-14 — Score the exit, the entry, and the settlement separately
The AL stop-loss at $0.01 read as "a terrible loss" — but replaying the
price log showed the exit BEAT holding by ~$1 (the game was over;
settlement was $0). The loss was decided by the entries (knife-buy at
0.20, add at 0.216, arm at the bounce top). Scoring the whole episode as
"the SL failed" would have produced the wrong fix (soften the stop);
separating entry/exit/settlement produced the right one (drop the stop
from lottery tranches entirely, keep the TP harvest — validated within
the hour when Yandex's TP bank cut a full-loss leg to −$1.66). Rule: in
any trading incident review, replay the price path and score each
decision against its own counterfactual — the component that hurts most
emotionally is often not the one that lost the money.

## 2026-08-14 — Short-token substring allowlists false-positive in the harmful direction
The esports classifier's bare `lec`/`lol` markers matched football club
"Lecce" and name fragments — and a classifier false positive here
auto-BUYS a non-esports corpse, the exact loss the gate exists to stop.
Rules: (1) match short markers with word boundaries (regex `\b`, markers
QuoteMeta'd), never Contains; (2) write the harmful-direction false-
positive cases as explicit tests (Lecce, Alec) before fixing; (3) when a
subagent flags a residual risk in its report, resolve it in review —
relaying the flag to the user unfixed is not review.
