# 7. Deep Crash: prior-alert-gated auto-buy below the corpse floor

Date: 2026-08-07

## Status

Accepted

## Context

SnipeMinAsk (0.03) exists because the first live snipe alert fired on a
finished game's loser token at $0.001 (MOUZ, 2026-08-01): with no
game-end signal in the market metadata, a bare cheap price cannot be
distinguished from settlement, so the band's floor treats everything
below 3¢ as a corpse.

Two production comebacks then showed the floor also amputates the best
tail the strategy has ever seen. Team WE (2026-08-06) crashed through
the band to $0.0065 and resolved at $1.00 within minutes (the bot,
registered late, only alerted on the rebound leg at $0.15). HANJIN
BRION (2026-08-07) alerted in-band at $0.09, was auto-bought at $0.10,
kept collapsing to $0.015, and exploded to $0.99 sixty seconds later —
the sub-floor prints paid 40–66× and the bot, by design, could neither
notify nor buy them. The counter-case remains real: Tempo (2026-08-05)
also printed ~$0.02 after its in-band alert and resolved at zero. Price
alone does not separate the two.

One observable does separate MOUZ-type corpses from HANJIN-type panics:
whether the token crashed down *through* the band while watched in the
current episode. A corpse is first sighted already cheap; a live panic
was alerted moments earlier at 3–20¢. The user also rejected a
notification-only tier — the sub-floor windows last one to seven
minutes, frequently while asleep, so a tier that cannot act alone
catches nothing.

## Decision

A second alert tier, **Deep Crash**, fires when a token whose current
episode already produced a genuine in-band alert prints an ask in
[0.005, 0.03). It fires once per episode, deliberately bypasses the
bought latch (the in-band $10 has usually already bought), notifies all
resolved recipients with an explicit corpse warning, and auto-buys a
fixed $5 gated by a strict zone guard — the buy executes only if a
fresh ask is still below 0.03, so the deep tranche's worst case is
exactly −$5 against a minimum 33× payoff. Deep spend draws from its own
$20/user/UTC-day in-memory pool, isolating corpse false-positives from
the main band's $50 budget. Below $0.005 nothing fires: settlement
dust. Episode state is in-memory; a restart between the in-band alert
and the dip silences that episode's deep tier (accepted, consistent
with every other soft rail).

## Consequences

- The glossary's "below 0.03 the market is declaring death" becomes a
  rebuttable presumption rather than an axiom; CONTEXT.md now carries
  the exception and the Deep Crash term (distinct from Lottery Ticket,
  which buys the opposite side after our own ceiling exit).
- A Tempo-type false positive costs $5, bounded at $20/day; a
  HANJIN-type hit pays ≥33×. At the observed base rate (2 recoveries vs
  1 corpse among sub-floor prints following genuine alerts) the tier is
  strongly positive, but n=3 — the sinceAlert/spread instrumentation on
  every deep fire feeds the September review, which decides whether the
  tier keeps its budget.
- The corpse floor's protective role is unchanged for tokens first
  sighted cheap (late registration, finished games): with no in-band
  alert on record, the deep tier stays silent. The Team WE miss is
  therefore NOT fixed by this tier — that was watch latency (tracked
  separately), and no threshold change would have caught it.
