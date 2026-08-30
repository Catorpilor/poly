
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

## 2026-08-20 — A data source's API shape is not its production behavior
The v0.20.1 depth confirm was built (correctly, per spec) against the
local WS order book — which production logs then proved is a one-shot
HTTP snapshot frozen at subscribe time: Polymarket's market channel
sends trade prints, not book updates, so the "live book" was fossilized
by design in this deployment. Shipped, it would have vetoed genuine
stop-losses indefinitely against stale-healthy liquidity — the exact
inverse of the fix's purpose. The repo even documented the staleness in
a comment; nobody connected it to the new consumer. Rules: (1) before
wiring a guard to a data source, verify what actually flows into it IN
PRODUCTION (grep the deploy logs for the update events; zero events in
34 minutes was the proof), not what its accessors promise; (2) a
verifier brief must include "trace the data source's refresh mechanics
end-to-end" whenever a decision consumes cached state; (3) comments
documenting a limitation are load-bearing evidence — grep for them when
adding consumers.

## 2026-08-20 — Restart amnesia composes; deploys are trades
The v0.21.0 deploy landed while a token sat inside the snipe alert
band. On boot the watcher re-seeded, re-alerted, and re-bought $10 for
a recipient who already held the episode — because the episode latch,
the bought record, AND the daily cap are all in-memory (each documented
individually as a "soft rail"; their composition was not). Issue #84.
Rules: (1) before `docker compose down`, check the monitor for tokens
currently in-band — a deploy during a live episode IS a trade decision;
(2) when N pieces of state share the same loss mode (process restart),
document and test the composed failure, not just each piece; (3) after
re-arming a log monitor, verify the follower process attached (ps for
the `logs -f` pipeline) — the 08-14 syntax-fumble lesson repeated today
before being caught by exactly that check (build the grep pattern in a
shell variable; never inline a quote-heavy pattern into a one-liner).

## 2026-08-26 — Verify the fix by replaying the incident through it
Two of this session's four fixes were disqualified in review round 1
because the patched code never ran on the path that caused the incident:
the #94 series walk lived only in the held-registration helper while the
actual recipients=0 incident flowed through snipe auto-buy → WatchArmed
(which never called it), and a renewal short-circuit defeated the
/positions rescue on top; the #92 fill-confirm's first draft re-created
the original orphan on every in-play order via the bet-delay 404 — a
failure mode the repo's own #27 lesson already documented for the FOK
path. Rules: (1) the highest-value verification question is "trace the
ORIGINAL incident, event by event, through the new code" — green tests
prove the new paths work, not that the incident's path reaches them;
(2) when a fix touches a flow with siblings (buy paths, registration
paths), enumerate every entry point and say explicitly which are covered
and which are not; (3) before designing around an external system's
async behavior, grep lessons.md and the adjacent subsystem for the same
failure signature — the bet-delay grace existed 30 lines from where it
was needed.

## 2026-08-26 — The bot's own messages are not tape
Two message-truth defects in one day: the SL-fire DM reported "filled at
$0.63" while the data-api tape showed the actual fill at 0.71 (the DM
renders the floor, not the execution — a ledger row settled off it was
wrong by $4), and "auto-sniped $5" went out for a GTC order that never
filled (it rested, then filled 24 minutes later unarmed). Extends the
08-05 settle-by-evidence rule: the bot's own notifications are claims,
not evidence — settle P&L, sizes, and prices ONLY from data-api activity
and prices-history, and treat any DM copy that asserts an execution fact
as a defect unless the code verifiably has that fact at send time
(otherwise disclose: "fill not confirmed yet"). When a message and the
tape disagree, the tape wins and the message becomes a bug to file.

## 2026-08-26 — Commit-then-I/O needs an undo, keyed by identity that survives
The fire paths committed their destructive step before selling (ClearTP /
AdvanceLadder / Disarm — deliberate double-fire guards), so a sell that
sold NOTHING (phantom print into an empty book, r106) permanently
consumed protection: flag dead, rungs spent, row deleted. Rules: (1) any
"commit state, then do I/O" sequence must answer "what if the I/O does
nothing?" — the answer is a CAS-guarded undo + retry backoff + notify,
with the undo yielding in the no-double-execution direction on every
race; (2) key the operational state (backoffs, streaks) by natural
identity (chat, token), never by a serial row id — the ceiling restore
REINSERTS the row and an id-keyed backoff dies with the old id, turning
the guard into a per-tick spam loop; (3) when a test fake models a DB
write, its IDENTITY semantics must match the SQL (the fake's
id-preserving copy made the suite structurally blind to the churn) —
fake/SQL parity means serial allocation and conflict behavior, not just
the happy-path data.

## 2026-08-26 — Escape sequences don't survive tooling heredocs
A Go regex written through a python heredoc shipped with literal
backspace bytes: python silently turned `\b` into \x08 inside the
'''string''' (only `\s` warned), the pattern compiled fine and matched
nothing, and the future-game gate was a silent no-op until the red test
caught it. Rules: (1) never route escape-bearing source lines (regexes,
format strings) through an interpreter's string literals — use the Edit
tool or bytes-mode patches for those lines; (2) when a pattern "compiles
but never matches", hexdump the source line (od -c) before debugging the
pattern's logic; (3) a table test on the pure function is the detector
that makes this a 2-minute fix instead of a shipped no-op — classifiers
get table tests, always.

## 2026-08-26 — A filter encodes its consumer's purpose; audit before reuse
GetAllMLMarkets deliberately classifies game/map-winner markets as
sub-markets — correct for its consumer (the feed renderer, which must
not show a BO3 as a fake 3-way market), and exactly wrong for #94's
recipiency walk, whose entire point was those game-winner markets. Blind
reuse would have shipped a filter excluding precisely the targets, with
every test green. Rules: (1) before reusing a classifier/filter, read
its exclusion list against YOUR purpose, not its name — "moneyline" meant
"what the renderer treats as primary", not "what a watcher should watch";
(2) when purposes diverge, write a new named predicate beside the old one
with a comment stating both purposes and why they differ (SeriesWatchMarket
vs isSubMarketQuestion); (3) the same applies in reverse: changing a
shared filter for your consumer silently re-scopes every other caller —
grep them first.

## 2026-08-26 — Stub the exact route form, and curl it for the exact field first

**Mechanism:** v0.22.2's series walk shipped verified (TDD green, two adversarial
rounds) and was structurally dead in production for three days: the walk reads
`market.Events[0].Slug`, the test stub served `/markets/{id}` WITH an `events`
field, but real Gamma omits `events[]` on the path form — only the list form
`/markets?id=` carries it. Every Telegram path uses `GetMarketByID` (path form),
so all three walk triggers silently no-opped (issue #99, ledger r107/r108: two
holders in one BO3 each alerted only on the game they personally bought).
**Incident:** the production-facts check for that fix verified `/events?slug=`
carried `outcomePrices` — the field the NEW code read — but never verified the
EXISTING fetch the fix piggybacked on carried what the new code needed from it.
**Rules:** (1) Before stubbing an external endpoint, curl the production
response for the exact route form the code hits and diff the fields the change
depends on — sibling route forms of the same API are not interchangeable
(`/markets/{id}` vs `/markets?id=` vs `?condition_ids=` each drop different
fields; gameStartTime already taught this once). (2) A silent early-return on
missing data (`if slug == "" { return }`) turns a wrong assumption into an
unobservable dead feature — when a guard encodes "this field is expected
present", log the bail at least once so production tape can falsify the
assumption. (3) Adversarial verify rounds inherit the fixture's worldview;
only a production read breaks the loop.

## 2026-08-28: Settled a ledger row from a search-result video title (self-caught)

**Mechanism:** Scoring the Ferencvárosi–Trabzonspor spread alert, I settled
"final 3–2, +1.5 covered, untapped +$48.8 winner" from a UEFA highlights
video TITLE in web-search results. The video was the same fixture from the
2023-24 UECL group stage — the actual 2026-08-27 match ended 2-0 (margin 2,
opposite resolution). Caught one event later only because Gamma's 0.835/0.165
lean contradicted the story; the ESPN box score for the dated gameId settled it.
**Aggravator:** the CLOB tape had already falsified my version — 0.03 asks
sitting unfilled for minutes is impossible on a side heading to $1 — and I
explained it away ("thin market apathy") instead of treating it as evidence.
**Rules:** (1) Settle outcomes only from a source pinned to the exact date/
match-id (ESPN gameId, UEFA match page, resolved market) — never from titles
or summaries in search listings; recurring fixtures make title collisions
likely. (2) When the order book contradicts the narrative (free money not
taken), the book wins — stop and re-verify before writing the outcome.
(3) Same class as ledger r21 (user-corrected mis-settlement by inference);
this one was self-caught but only after publishing a wrong row — verify
BEFORE the first write, not after.

## 2026-08-30: Spec named a mechanism where the ratified intent was behavioral (#102)

**Mechanism:** The grilled decision was "markets the user never touched are
alert-only." The issue spec I wrote translated that to "walked=true:
registrations made by snipeWalkEventSlug only" — a mechanism map. The builder
followed the letter and correctly flagged as an "observation" that
RegisterHeldBuy (web path) inlines its OWN continuation registrations outside
that function — which, per #99, is the only continuation path LIVE in
production and the one that produced the r114/r115 specimen motivating the
change. Shipped as specced, the fix would have gated the dead path and left
the live one auto-buying.
**Saves:** (1) the builder prompt's standing "enumerate ALL call sites and
classify each" instruction surfaced the path; (2) reading the builder's
"observations" section adversarially — it had labeled the asymmetry
"intended scope" citing my own spec line back at me.
**Rules:** (1) Spec the INTENT plus an acceptance predicate over behavior
("no path may auto-spend on a market lacking a direct-touch registration"),
and let call-site enumeration serve the predicate — never enumerate mechanisms
in the spec as if exhaustive. (2) Builder observations that cite the spec to
justify an asymmetry are the highest-value lines in the report — each one is
either a real scope decision or a spec bug; classify explicitly before verify.
(3) The round-2 verify also caught a renewal path ADDING holder entries with a
defaulted class — when introducing a classification on existing state, grep
every WRITE site of that state, not just the registration functions named in
the design.
