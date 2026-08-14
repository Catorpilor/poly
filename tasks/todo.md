# Auto-arm full-position coverage + honest SL/TP list

Decisions (grilled 2026-08-15, MOUZ under-coverage episode):
- Bug: auto-arms snapshot fill shares only; TP sold 25% of 49.8 while the position held more (manual tranches outside the arm); SL/TP list marks the whole position "armed" without comparing SharesAtArm to actual shares
- Ratified: TP-only (auto) arms AUTO-EXTEND share coverage to the full position; AvgPrice/thresholds NEVER change (TP still keys off fill entry; sells 25% of everything; ceiling sells all)
- Manual TP+SL arms keep frozen-at-arm-time semantics untouched (deliberate user freeze)
- Mechanism: fire-time sizing from current holdings for TP-only arms (fallback to SharesAtArm on read failure) + sweep-cycle upward reconciliation of SharesAtArm so the list is truthful; never reconcile down (stale-size machinery already handles shrinkage at sell time)
- UI: SL/TP list shows covered/total with a mismatch marker when SharesAtArm < position shares
- v0.19.0; CONTEXT.md Arm/auto-arm notes; ledger note

## Plan

- [x] RED: failing tests (fire-time full-holding sizing TP-only only; manual arms unchanged; upward-only reconcile; list rendering with mismatch)
- [x] GREEN + REFACTOR, full suite + -race (verified independently, -count=2)
- [x] CONTEXT.md + ledger memory
- [ ] PR, merge, tag v0.19.0, deploy

## Review

- `HoldingReader` seam on the monitor (nil = disabled, closedChecker pattern); bot implements via the existing positions scanner — no new API path. Wired in main.go.
- Fire-time: TP/ceiling basis = max(snapshot, live holding) for SLArmed=false only; read failure falls back to snapshot; reactive shortfall clamp still caps over-requests. Manual arms regression-pinned byte-identical.
- Sweep reconciles SharesAtArm monotonically up (SQL-guarded `WHERE shares_at_arm < $3`), TP-only only, AvgPrice/HWM untouched.
- List row shows "⚠ 50/93 sh" coverage prefix on mismatch; manual arms never flagged.
