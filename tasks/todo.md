# Snipe auto-buy gates — stop catching falling knives

Decisions (grilled 2026-08-14, evidence: ledger + monitor tape Aug 4–14):
- Knife rate since Aug 13: 14/22 alert episodes collapsed to corpse zone; deep tier 0/11 fires, −$54.78
- NO dwell-delay: the biggest winners (HANJIN +$84, WE +$56) bounced within minutes — a delay + the ≤0.30 repricing guard would have missed them
- Gate 1 — **Sport gate**: auto-buy ($10 in-band AND $5 deep) only for esports markets; tennis (0/5), football (0/1), and unclassifiable markets become alert-only with tap buttons intact
- Gate 2 — **Spread-geometry gate** (in-band $10 only): at buy time fetch the fresh book; skip the auto-buy when fresh bid < ask/3 (Tempo corpse signature ≈0.22×, 2/2 on funded losers). Alert still sends, falls back to manual buttons
- Gate 3 — **Deep holdings gate** ($5 deep only): skip the $5 when the recipient already holds shares of the crashed side — positions API check OR the bot's own in-episode buy record (Data API can lag a just-filled buy). Deep alert + corpse warning still send. Deep buy remains as catch-up entry when the in-band buy never funded
- Alerts and manual tap buttons unchanged everywhere; gates apply to auto-buys only
- Ledger memory must record the policy-regime boundary (2026-08-14) for the September review
- Implementation by opus subagent, TDD, branch + PR + deploy

## Plan

- [x] RED: failing tests for the three gates + sport classifier
- [x] GREEN: minimal implementation
- [x] REFACTOR + `go test ./...` + `-race`
- [x] CONTEXT.md: update Comeback Snipe / Deep Crash entries with gate semantics
- [ ] PR, merge, tag, deploy
- [ ] Mark policy boundary in snipe ledger memory

## Review

All in `internal/telegram/snipe.go` (+ Bot field wiring, test seams):
- Gate 1 hooks at the top of `snipeAutoBuy` and `snipeDeepAutoBuy`; classifier is a word-boundary regex over a tunable marker allowlist (hardened post-subagent: bare substrings false-positived on "Lecce"/"Alec" — caught in review, fixed TDD).
- Gate 2 lives in `snipeGuardedBuyRefuse` behind a `corpseGuard` flag — only the in-band auto-buy passes true; deep and manual taps unaffected. `SnipeAskSource` gained `BestBid` (same fresh-book read as the ask guard). Missing/zero bid ⇒ treated as corpse ⇒ skip.
- Gate 3: `snipeBoughtRecord` (in-memory per-chat token set, written on in-band auto-buy + tap buy, never on deep) + `snipeHoldsPosition` (Data API via the existing `snipePositions` seam). Either showing exposure skips the $5; positions error falls back to the record alone.
- Distinct skip outcomes/log lines (`snipeBuyNotEsports`, `snipeBuyCorpseSpread`, `snipeBuyDeepHeld`) so the ledger can attribute skips.
- 18 new tests; full suite + `-race -count=1` green, verified independently.
