# September Review — Comeback Snipe (held 2026-09-01)

Standing task from 2026-08-02: review every alert/tap/outcome, judge whether the
0.03–0.20 ask band earns its keep, score gated skips as counterfactuals, split
samples at regime changes. Ledger: memory `project_snipe_ledger.md` (rows 1–117,
ten regime changes). Aggregation recomputed row-by-row by subagent, arithmetic
run twice; the one load-bearing estimate (r80) tape-verified during the review.

## Headline

**System money (the bot's own spend), all-time settled: −$63.02 on ≈ $1,130
deployed (−5.6%).**

| layer | tranches | winners | P&L |
|---|---|---|---|
| in-band $10 taps | 97 | 17 (17.5%) | −$160.29 |
| deep tier $5 | 13 | 0 | −$64.73 |
| boxed ladder rungs | 10 episodes | 1 | **+$162.00** |

The boxed line is carried entirely by r80 (DK–HLE G1, 08-20), now
**tape-verified**: rungs bought 299.08 HLE for $9.51 (0.09/0.02), sold $243.26
(76.02 @ 0.23 + 228.06 @ 0.99) → **+$233.75 real**, not an estimate. Without it
the system would be −$296; with it verified, −$63 stands.

Era split of the in-band layer: v2 era ≈ breakeven (−$9.68 / 49 tranches, 9 W);
the corpse-heavy 08-14→08-29 stretch bled −$186 / 45; the alert-only era opened
2-for-2 (+$44.80). Winners are 100% esports fast-collapse competitive crashes.

**The edge has moved from entries to exits.** The alert band is a trigger at
roughly breakeven; the money is made by the machinery it feeds: TP-only
auto-arms, the P4 ladder, ceilings, and depth-confirm. Ten settled rows now
show machine exits converting a LOSING side into banked profit (r52 +23.32,
r86 +20, r116 +16.8 headline the class). Depth-refusals are **12+/12+ correct**
across TP/SL/ceiling since v0.20.1 — zero bad refusals, several
refuse-then-fire-higher sequences (r113's SL-refusal→TP-0.67 the exemplar).

## Verdict on the band (the core question)

**KEEP, esports-only, as-is.** 17.5% winner rate sits just under the ~20%
breakeven at 0.20 asks — on its own the flat entry doesn't quite pay, but it is
within noise, the bleed is bounded by the gates, and the band is the intake for
the harvest machine that IS positive. The "wished for lower entries"
counterfactual study is deferred: the P4 ladder and boxed rungs already
implement buy-lower, and r80 (+$233 from 0.02–0.09 entries vs ~$40 had it
bought at 0.20) is the strongest possible datapoint that deep entries where
they matter are already captured.

## Gate scorecard (counterfactuals)

| gate | record | verdict |
|---|---|---|
| holdings (deep) | 24/24 correct, ≈ +$120 saved, 0 missed | unambiguous KEEP |
| corpse-spread | 1/1 (+$14.90) | KEEP |
| manual-armed | 2/2 (+$20) | KEEP |
| sport | ≈ +$70 saved vs $79.80 missed (Paul, Rayo No) — net ≈ −$10 | KEEP: a wash on counterfactuals, but tennis+football tap record is 0/8 and the gate is variance control, not EV harvesting |
| future-game | no live fire yet | keep collecting |
| series-walked (v0.23.0) | ≈ +$60 saved vs ≈ $80 missed (both on one monster see-saw map) | KEEP, sample insufficient — October re-review |
| boxed-wait postponement | rungs +$162 vs ≈ +$90 counterfactual old case-3-at-0.20 on the same episodes | KEEP — the deep-rung structure captured 6× more on the one big hit, exactly its thesis; 3 unfilled-rung misses (+$119.80 forgone) are the known cost branch |

## Signal corrections (ledger hygiene — prose vs recomputation)

- **Crossed-bid**: honest full-enumeration rate **6/24 (25%)** vs 17.5%
  baseline — mildly positive, NOT the curated "3-for-5 / 2-for-3" the running
  prose tracked. Downgraded to instrumentation.
- **pairAlerted≠never chase taps: 1 W / 5 L** — anti-signal. Don't chase the
  second crash of a see-saw; the boxed ladder already owns case-3.
- **Football alerted-crash recovery corrected to 1/7** (Rayo "No" won; prose
  said 0/7). The fade thesis survives: complements keep winning (Osasuna +76,
  Man City +163 judgment fades). Recommend paper-trade fade instrumentation
  (log complement price at alert + settlement), no auto-fade money.
- **Favorite-collapse anti-signal**: session-high ≥ 0.90 auto-taps are 0-for-3+
  (r35 .945, r72 .960, r94 .955; r16 at .880 WON, so the cut is above .88).
  Sample too small for a hard gate — recommend shadow log-split first.
- **corpse-by-clock** (period/total props decaying on the clock): structural,
  1/1. Recommend excluding period/total props from alerting entirely; spread
  props stay (Ferencvárosi: comeback-able on one goal).

## Proposals coming out of the review (each needs its own grill)

1. **Deep tier → alert-only** (kill the $5 auto-pool). 0/13, −$64.73; the
   holdings gate already strangles it; remaining fires target unheld corpses,
   the worst class. Keep the 100× DM taps for judgment.
2. **Shadow-instrument a favorite-collapse split** (log-only, high ≥ 0.90) to
   decide a future gate on data.
3. **Exclude period/total props from alerts** (spread props stay).
4. Ledger memory corrections from this review: applied 2026-09-01.

## Infrastructure record

All three named missed-winner case studies (≈ $180 forgone: Enterprise, WE–TES,
DNS) were infra/design misses, not band misses — all fixed (v0.16.1, v0.20.0).
The final week closed #100 (phantom episode resets — both live resets since are
genuinely bid-corroborated), #102 (continuations alert-only — specimens on both
tiers within 20 minutes of deploy), #99 (series walk revived — recipients
widen, auto money doesn't). Three grill-ship runs, every one SHIP after
independent adversarial verify; the verify loop caught a misidentified seam, a
latency regression, an amplification bug, and an arm64 data race pre-merge.

## Stragglers to settle (carried forward)

r54, r61, r63, r68, r79, r81 (restart-amnesia double-buy), r80's DK
second-ladder residual (≈ −$1.50), Millwall/Nürnberg/Málaga/Arsenal/Barcelona
football alert-quality rows, Osasuna/Carabelli/Gomez counterfactuals, Fery
lottery.
