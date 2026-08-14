# Boxed Snipe — case-3 postponement to ≤ $0.10

Decisions (grilled 2026-08-14 evening):
- Position-state cases at alert: (1) holds crashed side → unchanged; (2) no position → unchanged; (3) holds the OPPOSITE side → in-band $10 postponed until ask ≤ $0.10 (user proposed 0.05; 0.10 compromise ratified — preserves HANJIN-class bottoms, accepts skipping shallow boxed bounces like r42)
- Rationale: with TP-only auto-arms the held side A harvests at the $0.95 ceiling; the B-side flip ticket is better bought deep (mirror symmetry B≤0.10 ↔ A≥0.90) — generalizes the Lottery Ticket concept into the snipe path
- "Postpone" = within-episode re-dispatch: new watcher tier (like Deep Crash) fires once per episode when st.alerted && ask ≤ 0.10 && ask ≥ deep floor && in-play; bot buys $10 for case-3 recipients who were boxed-waited at alert time
- Case-3 detection: sibling-token exposure = in-memory bought record ∪ SL/TP arm exists ∪ positions API (in that order; watcher sibling lookup by MarketID acceptable as first pass)
- Sport + corpse-spread gates still apply to the postponed buy; deep tier unchanged (case-3 gets its <$0.03 chance; holdings gate already handles case-1 there)
- Alert message for boxed-waiters says the auto-buy is waiting for ≤ $0.10; manual tap buttons unchanged
- Ledger counter-evidence recorded: case-3 taps at 0.15–0.25 were the best subclass (+$92/4) — this change is a deliberate regime bet that auto-arm ceilings replace the box-completion rescue role; September scores boxed-wait skips as counterfactuals
- v0.18.0; CONTEXT.md + ledger regime note #3

## Plan

- [x] RED: failing tests (boxed tier fires once per episode at the cross; case-3 classified via record/arm/positions; case-1/2 unchanged at ≤0.20; gates apply; message wording)
- [x] GREEN + REFACTOR, full suite + -race (verified independently, -count=2)
- [x] CONTEXT.md (Boxed Snipe glossary term) + ledger memory (regime #3)
- [ ] PR, merge, tag v0.18.0, deploy

## Review

- Watcher: `SnipeBoxedMaxAsk=0.10`, `boxedAlerted` latch resetting with the episode, boxed dispatch after fire/deep in evaluate; zone deliberately overlaps deep so straight-to-corpse crashes still re-offer; `SiblingTokenIDs` in-memory index (no Gamma in the alert path).
- Bot: case-3 = sibling exposure via bought-record → arm store → positions API (error ⇒ not-case-3, conservative); `snipeAutoBuyExec` extracted so boxed reuses the full cap/guard/auto-arm ceremony; boxed-wait alert note + boxed-bought confirmation; non-case-3 recipients silent on the boxed dispatch.
- Accepted edge (documented): discontinuous crash can give case-3 both the $5 deep and $10 boxed — both capped; suppressing would couple the tiers.
- 12 new tests across watcher + bot; agent's watcher test initially mis-asserted the deep/boxed boundary and the RUN corrected it — red-first surfaced a real ordering behavior.
