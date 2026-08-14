# Auto-arm TP+ceiling on snipe fills (no trailing SL)

Decisions (grilled 2026-08-14 PM, after the AL episode):
- AL revisit verdict: SL exit was correct (game over, salvaged ~$1) — but the ledger says trailing SLs on snipe tranches truncate the 5× tail the band needs (r22 −$31 counterfactual; wick-amputations r12/r17). TP+ceiling exits made every big winner.
- Every snipe fill (in-band $10 auto, deep $5 auto, one-tap $10/$25) auto-arms **TP + ceiling only** (`TPArmed=true, SLArmed=false`) — max loss stays the stake; winners get harvested mechanically.
- Existing arm for (user, token) ⇒ skip (never clobber; a later manual arm re-arms normally and wins).
- Arm data must not race the Data API (the #67 lesson): prefer fill data (VWAP price, fill shares); position read only as enrichment when fresh.
- Auto-arm failure is log-only — never blocks the buy or the alert.
- User gets the standard "Armed" confirmation, worded TP-only.
- Version v0.17.0; CONTEXT.md Comeback Snipe + Arm entries updated; ledger memory gets the second same-day regime note.

## Plan

- [x] RED: failing tests (arm created TP-only from fill data; skip when arm exists; failure is log-only; watcher registration)
- [x] GREEN + REFACTOR, full suite + -race (verified independently)
- [x] CONTEXT.md + ledger memory regime note
- [ ] PR, merge, tag v0.17.0, deploy

## Review

- New repo method `ArmTPOnly` (sl_armed=FALSE; upsert mirrors `Arm`, so manual re-arm restores full TP+SL) — required because `Arm`'s SQL hardcodes both flags TRUE.
- `snipeAutoArmTPOnly` helper wired after all three snipe fill sites (in-band, deep, tap), async, log-only failures, no-clobber via GetByUserAndToken.
- Arm data from the fill itself (#67 lesson): filledPrice or fresh-ask fallback (delayed orders report no fill fields — the 07-20 lesson), shares = stake/price fallback; `WatchArmed` reuses the in-hand Gamma market, no refetch.
- 6 new tests incl. no-clobber, repo-error isolation, and TP-only message wording; one test-side race fixed (await terminal DM, not intermediate effect).
