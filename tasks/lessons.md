
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

## 2026-08-15 — Relaxing an invariant means auditing every consumer of it, prose included
v0.17.0 made SLArmed=false arms possible, relaxing "every arm has a
stop" — but the TP-fire message template still said "Trailing stop
watching the remainder" (its comment even asserted the stop was
"necessarily active"), and it lied to the user at the worst possible
moment: while their unprotected remainder rode a peak back to zero.
The compiler verifies struct consumers; nothing verifies prose,
comments, or UI copy that bake in the old invariant. Rule: when a
change relaxes an invariant, grep every consumer of the TYPE (messages,
views, docs, log templates) for wording that assumes the old world —
list them in the PR as audited-and-kept or changed. The v0.19.0 list
view had the sibling bug (rendered "armed" without coverage) for the
same root cause.

## 2026-08-15 — Changing a field's data source can silently change its meaning
The #67 fix deliberately built auto-arms from FILL data (right call —
lag). But SharesAtArm thereby quietly changed meaning from "the
position at arm time" (the manual flow's semantics, which sell sizing
and the UI were written against) to "the fill tranche" — and every
consumer kept its old interpretation: TP sold 25% of the fill while
manual tranches sat outside the arm, and the list showed the whole
position as armed. Rule: when a field's value starts coming from a new
source, re-derive what each consumer believes the field MEANS — a
data-source substitution is a semantics change until proven otherwise.

## 2026-08-15 — Rebuild a guard process by diffing against its captured predecessor
Re-arming the log monitor to add the boxed-tier lines, I retyped the
loop and dropped both the `sleep 5` and the closing `done` — exit 2,
dead monitor, and had the syntax happened to parse it would have been a
tight CPU spin on the Pi with the container down. TaskStop echoes the
previous command verbatim: the correct move is to edit THAT text, not
re-type from memory, then diff old vs new before launching, then
confirm the replacement is actually running (TaskOutput non-error).
A guard that fails to start is the silent loss of everything it guards.

## 2026-08-15 — A bet against your own data must instrument both counterfactuals
Boxed Snipe deliberately trades the ledger's best subclass (case-3 taps
at ~0.20, +$92/4) for deep flip tickets, on the thesis that TP-only
auto-arm ceilings now do the box-completion job. That may be wrong —
and the only way the September review can falsify it is because every
boxed-wait skip logs distinctly AND the re-offer logs its own fire, so
both branches (what the skipped 0.20 buy would have done; what the
0.10 buy captured) are scoreable. Rule: when a policy change knowingly
contradicts historical performance, ship the instrumentation that can
prove it wrong in the same release — a regime bet without counterfactual
logging is just hope.

## 2026-08-14 — Short-token substring allowlists false-positive in the harmful direction
The esports classifier's bare `lec`/`lol` markers matched football club
"Lecce" and name fragments — and a classifier false positive here
auto-BUYS a non-esports corpse, the exact loss the gate exists to stop.
Rules: (1) match short markers with word boundaries (regex `\b`, markers
QuoteMeta'd), never Contains; (2) write the harmful-direction false-
positive cases as explicit tests (Lecce, Alec) before fixing; (3) when a
subagent flags a residual risk in its report, resolve it in review —
relaying the flag to the user unfixed is not review.

## 2026-08-16 — Behind a transparent proxy, "direct-reachable" is an illusion
Two days of debugging rested on "Polymarket works direct from China; only
Telegram needs the proxy" — false. The transparent SNI proxy had been
silently rescuing every polymarket.com host all along, so plain curls
"proved" a direct path that didn't exist, and a NO_PROXY built on that
model forced genuinely-blocked hosts direct: /wallet replies went out
with EMPTY text (upstream fetches dead, Telegram fine). Rules: (1) to
test the true direct path, bypass the proxy explicitly (`curl --noproxy
"*"`) — an unqualified probe tests the interception, not the route;
(2) empty outbound message text = the handler's upstream data call died,
not the messaging layer; (3) when a reachability belief becomes load-
bearing for config, re-verify it at the moment of use — the 08-15
"CLOB is fine" observation was true-but-misattributed and silently
poisoned the 08-16 rollback config.

## 2026-08-16 — Validate the traffic pattern, not the endpoint
Quick probes through a node scored 8/8 while the bot's getUpdates failed
on that same node at the same moment: the tunnels pass sub-second
requests but kill connections held idle ~60s, and a 60s long-poll is
precisely a held-idle connection. Node-shopping couldn't fix it (every
exit dropped holds); shortening the poll to 15s fixed it everywhere
(0 failures thereafter). Rules: (1) a path is only validated by
reproducing the client's actual pattern — hold duration, streaming,
WS — a fast 200 validates nothing about held connections; (2) protocol
timing knobs that interact with infrastructure tolerance belong in
config (TELEGRAM_POLL_TIMEOUT_SECONDS), not constants; (3) a low-rate
version of the failure had been in the logs for days (isolated
"Failed to get updates" retries) — a retry loop that usually absorbs an
error is also hiding its rate; alert on rate, not occurrence.

## 2026-08-16 — "APIs reachable" ≠ "trading permitted": verify geoblocks from the new egress
The Pi→EC2 migration was mechanically flawless (43s window, byte-faithful
DB) and dead on arrival: Polymarket geoblocks ORDER PLACEMENT from
Singapore IPs while serving data/WS/alerts normally, so every
reachability check passed and only live orders failed ("Trading
restricted in your region"). Rules: (1) before relocating execution
infra, place a real (minimal) order from the new egress — read paths
prove nothing about write permission; (2) the migration discipline that
made this survivable is the keeper: prep slow, cut fast, never two
pollers, old host preserved intact = same-day free rollback; (3) region
choice for trading infra is a compliance surface — check the venue's
restricted list before choosing the datacenter.

## 2026-08-16 — Shared boxes and runtime state: enumerate first, persist deliberately
Two near-misses in one migration: (a) the "new" EC2 was already running a
forgotten April polybot container — 3 months up, holding a live bot
token, the target container name, AND a publicly-exposed 0.0.0.0:8081 —
discovered only because compose ps showed a stranger; (b) the mihomo
node switch applied via API was runtime-only (`store-selected` absent),
so any service restart would have silently reverted Telegram to the
broken default. Rules: (1) on shared/long-lived hosts, enumerate
running containers/ports/services BEFORE deploying — name collisions
and forgotten token-holders are found by looking, not by luck;
(2) an operational fix applied through a runtime API isn't done until
the equivalent lives in config (or persistence is verified) — restart
amnesia turns today's fix into next month's mystery regression.

## 2026-08-16 — Timeouts are a stack; tuning one leaves its coupled twins ruling the worst case
v0.19.2 shortened the getUpdates hold to 15s and was declared working —
but the lag persisted, because the HTTP client's hardcoded 75s timeout
(sized as 60s poll + 15s grace) still governed how long a proxy-killed
connection hung before the retry loop could act. The tell was in the
failure SPACING: dead polls landed exactly ~78s apart, the old constant's
fingerprint, visible in the logs even while success counts looked
improved. Rules: (1) a timeout that encodes an assumption about another
timeout (75 = 60 + 15) must be a derivation, not a literal — when the
source value became configurable, the derived one silently became a bug
(fixed: pollHTTPTimeout = poll + 10s); (2) when tuning any timing knob,
enumerate the full chain around it (server hold, client timeout, dial,
retry delay) and re-derive each from the same source; (3) verify latency
fixes by measuring the WORST case (spacing/duration of failures), not
the success rate — "more polls succeed" and "dead polls still cost 78s"
were both true at once.

## 2026-08-19 — Search for the existing feature before designing its replacement
Designing the #78 fix (buy the flip side cheap), I proposed new
mechanisms through two grill rounds before the user pointed at the
existing Lottery toggle ("other side @ ≤$0.05") — which I had even SEEN
in that morning's logs (the sltp:lot button) without connecting it. Its
mechanics (ceiling-triggered, $5, ≤0.05, opt-in) changed the whole
option space: the final ladder design deliberately absorbed its unique
coverage row. Rules: (1) before proposing any new mechanism, grep the
domain vocabulary for prior art (the feature was one `grep -rn lottery`
away) and read what surfaced in today's own logs; (2) present overlap
as a coverage matrix (which scenario does each mechanism catch) — it
converts "new feature vs old feature" into a factual gap analysis;
(3) when a fix targets a dispatch/registration gap, enumerate ALL call
sites of the behavior, not the one function named in the diagnosis —
round 1 fixed only the web path because the spec said "RegisterHeldBuy"
while telegram buys registered elsewhere (caught by independent verify,
would have shipped the bug unfixed for the primary path).
