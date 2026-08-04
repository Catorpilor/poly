# 6. Stop-loss escalates to market after one refused floor

Date: 2026-08-04

## Status

Accepted

## Context

The v0.11 trailing stop deliberately never sold below its floor
(trigger × 0.90): the exit was a floored FOK, retried indefinitely, with
market escalation explicitly rejected — the pre-v0.11 fixed stop had
market-sold wick breaches into panicked books at 0.04–0.13 against
0.18–0.49 triggers, realizing −48% of basis.

Production then produced three ride-to-zero episodes where the book
gapped through the floor and no retry ever found a legal fill
(PuckChamp 2026-07-21, FaZe 2026-08-01, and a $206 T1–HLE series stack
on 2026-08-04 whose entire fall from trigger-touch to $0.11 took under
two minutes — the breach's only sellable print predated the confirmation
debounce's expiry).

A 199-episode counterfactual over realized July–August price paths
(fidelity refined to 1-minute around every disagreement) compared floor
depths 0.10/0.15/0.20/0.25 and pure market exits:

- Deeper floors were statistically indistinguishable from the baseline
  (bootstrap P 0.20–0.82; point deltas carried by one or two episodes).
  The 2026-08-04 crash had no post-trigger print above **any** tested
  floor — depth does not catch real collapses, it only accepts worse
  prices on recoverable dips.
- Market exits improved the total by +$402 on ~$14.6k staked
  (bootstrap P = 0.99, fills all 47 triggered episodes, positive even
  after removing the three largest wins). In a true collapse the next
  print is already far below any floor; any fill beats zero.

## Decision

After the confirmation debounce, the floored FOK gets **exactly one
shot per breach episode**. If it is killed (not a balance-shortfall —
that path keeps its own handling), the monitor immediately sells the
remainder at market via the standard aggressive-GTC path, and every
retry while the episode persists goes straight to market. A recovery
above the trigger clears the episode and restores FOK-first.

The wick protection that made the old market-sells toxic is retained
upstream: activation gate (peak ≥ 1.2× entry), ratcheting trigger, and
the 30s debounce all still precede any sell. Escalation only ever runs
against a breach that survived all three *and* a refused floor.

## Consequences

- Collapse episodes exit near the crash's leading edge (~40s after
  breach start) instead of riding to zero; V-bounces that dip below the
  floor for longer than one attempt window are now sold at market —
  the counterfactual prices this trade-off as clearly net-positive for
  this account's in-play esports profile.
- The "never below floor" guarantee in user-facing copy is replaced by
  an honest escalation message naming the gapped floor.
- Escalated exits use GTC acceptance semantics (like TP fires), not the
  FOK fill-confirmation path; fill details surface on the tape.
- Simulation caveat: fills were modeled at print prices without depth;
  realized escalation prices may be worse in very thin books.
