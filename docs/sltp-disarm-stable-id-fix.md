# SL/TP Disarm Reliability Fix — Stable Arm ID Resolution

**Date:** 2026-06-06
**Release:** `v0.6.17-sltp-disarm-id` (image `cheshire42/poly:0.6.17-sltp-disarm-id`)
**Status:** Deployed
**Affected area:** Telegram SL/TP auto-sell — disarm and lottery-toggle callbacks

---

## Summary

A user disarmed a Knicks moneyline position in the SL/TP menu, but the position
auto-sold anyway when its take-profit condition was met. Log forensics showed the
disarm tap was received by the bot yet never took effect — it silently returned
"Session expired" while the monitor kept the position armed and fired the TP.

Root cause: the disarm and lottery callbacks identified the target position by its
**index into a cached position list** that has a 10-minute TTL. When that cache had
expired (or the tap was delivered late), the index could not be resolved and the
disarm aborted without disarming.

The fix re-keys those callbacks on the **stable `SLTPArm` database ID** and resolves
them directly from the database, independent of any cached UI state. A disarm now
takes effect whenever the tap is processed, regardless of how stale the UI is.

---

## Timeline of the Incident

All times UTC, 2026-06-06. Position: *Knicks vs. Spurs* / Outcome **Knicks**,
entry $0.2647, 1000.43 shares.

| Time | Event |
|------|-------|
| 01:35:21 | `🎯 Armed` — TP: bid ≥ $0.5294 → sell 50%; SL: bid ≤ $0.1853 → sell 100% |
| 01:35:23 | Menu re-rendered showing the `⏹ Disarm` button; user tapped **Disarm** |
| 01:35:23 → 02:02:56 | Bot `getUpdates` long-poll stalled — offset frozen at `302959893`, empty batches returned for ~27 min |
| 02:02:56 | Telegram flushed the buffered taps; bot processed `sltp:off:0` and replied **`❌ Session expired. Tap 🎯 SL/TP again.`** — disarm did **not** run |
| 02:16:31 | `✅ TP hit at $0.5300` — the still-armed position auto-sold 50% |

The monitor logged `arms=1` continuously throughout the window and never observed a
disarm. No `⏹ Disarmed` confirmation was ever emitted for this position.

---

## Root Cause

### Two contributing factors

1. **Transport delay (environmental).** The bot runs on a Raspberry Pi with flaky
   connectivity. Telegram's `getUpdates` long-poll stalled for ~27 minutes, buffering
   the user's taps and delivering them in a single late batch. This is a network-layer
   problem, not a bot-logic bug, but it is what exposed the latent code defect.

2. **Stale-state dependency (the code bug).** The disarm/lottery callbacks encoded only
   a **position index**:

   ```
   sltp:off:<positionIndex>     // disarm
   sltp:lot:<positionIndex>     // lottery toggle
   ```

   `handleSLTPDisarmCallback` resolved that index against a per-user cached position list
   stored in `stateManager` with a **10-minute TTL** (`handleSLTPList`,
   `SetState(..., 10*time.Minute)`). When the late tap was finally processed at 02:02:56,
   that cache had expired ~17 minutes earlier, so `resolveSLTPPosition` returned
   `(nil, false)` and the handler bailed with "Session expired".

### Why this was dangerous

The monitor reads armed rows from the database on every evaluation
(`ListArmedByToken`). A disarm only stops auto-selling if it **deletes the database
row**. Because the disarm aborted before reaching the database, the row survived and
the monitor fired the TP. The failure was silent — the user received a "Session
expired" message they reasonably ignored, having already tapped Disarm.

A safety toggle must resolve from durable state, never from a transient UI snapshot.

---

## The Fix

Re-key the disarm and lottery callbacks on the arm's **stable database primary key**
(`SLTPArm.ID`) and resolve them straight from the repository. The arm row already exists
in the database when these buttons are rendered, so its ID is always available and never
expires.

```
sltp:off:<armID>     // disarm  — resolves via SLTPArmRepository.GetByID
sltp:lot:<armID>     // lottery — resolves via SLTPArmRepository.GetByID
sltp:arm:<index>     // arm     — UNCHANGED (see note below)
```

The **Arm** button still uses the position index, because arming legitimately requires
fresh data from the position scanner (average price, share count, condition ID). Arming
against a stale snapshot is guarded separately and is not safety-critical; the dangerous
direction is a disarm that silently no-ops.

### Behavioral changes

- A disarm tap takes effect whenever it is processed, even if the UI cache is gone or the
  tap arrived minutes late.
- Disarming an arm that is already gone (double-tap, or an arm that fired and cleared in
  the meantime) is now **idempotent**: the bot replies `⏹ Already disarmed` instead of an
  error.
- `GetByID` is **scoped to the calling user**, so a crafted callback cannot act on another
  user's arm.
- Disarm/lottery confirmations no longer depend on the position cache. A small best-effort
  helper (`sltpArmDisplay`) still enriches the message with the market title when the cache
  happens to be present, falling back to the outcome label otherwise.

---

## Code Changes

### `internal/database/repositories/sltp_arm_repository.go`

Added a user-scoped lookup by primary key to the `SLTPArmRepository` interface and its
implementation:

```go
// GetByID returns the arm with the given primary key, scoped to telegramID,
// or nil if no such arm exists for that user.
GetByID(ctx context.Context, telegramID int64, id int) (*database.SLTPArm, error)
```

```go
func (r *sltpArmRepo) GetByID(ctx context.Context, telegramID int64, id int) (*database.SLTPArm, error) {
	query := `SELECT ` + sltpArmColumns + ` FROM sltp_arms WHERE id = $1 AND telegram_id = $2`
	row := r.db.Pool.QueryRow(ctx, query, id, telegramID)
	arm, err := scanArm(row)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("failed to get sltp arm by id: %w", err)
	}
	return arm, nil
}
```

### `internal/telegram/sltp.go`

- `sltpRowForPosition` — disarm button callback now `sltp:off:<existing.ID>`.
- `sltpLotteryRow` — lottery button callback now `sltp:lot:<arm.ID>`; dropped its unused
  index parameter.
- New `resolveSLTPArm(ctx, update)` — parses the arm ID from callback data and loads the
  arm via `GetByID`, with no dependency on `stateManager`. Returns `(nil, nil)` when no
  matching armed row exists.
- New `sltpArmDisplay(userID, arm)` — best-effort human label (market title if cached,
  else outcome).
- `handleSLTPDisarmCallback` — rewritten to resolve by arm ID, treat a missing arm as
  idempotent success, then disarm + unsubscribe.
- `handleSLTPLotteryCallback` — rewritten to resolve by arm ID.

`resolveSLTPPosition` (index-based) is retained and still used by the Arm path.

### `internal/telegram/sltp_adapter_test.go`

- Updated `TestSLTPRowForPosition_ArmedShowsDisarm` to assert the disarm button is keyed
  on the arm ID (uses ID `42` with index `2` to prove the two are independent).
- Added `TestResolveSLTPArm` with a fake repository (interface embedding): verifies a valid
  ID loads the arm and queries the repo with the correct user/ID; an absent arm returns
  `(nil, nil)`; malformed callbacks (`sltp:off`, `sltp:off:abc`, `sltp:off:1:2`) return
  errors. The fake never touches `stateManager`, proving resolution is state-independent.

---

## Verification

- `go build ./...` — passes
- `go vet ./...` — clean
- `go test ./...` — all packages pass, including the new `TestResolveSLTPArm`
- CI (`docker-release.yml`) built and pushed multi-arch (`linux/amd64,linux/arm64`) image
  `cheshire42/poly:0.6.17-sltp-disarm-id` in 6m54s
- Deployed to the Pi via `docker compose pull && up -d`; startup clean — `Connected to
  RTDS`, `SLTPMonitor: Started`, no panics

**Counterfactual:** had this fix been live during the incident, the disarm processed at
02:02:56 would have deleted the arm row, and the 02:16 TP would not have fired.

---

## Residual Risk / Follow-ups

- **Transport stall is unaddressed by this change.** The ~27-minute `getUpdates` stall is
  a Pi connectivity issue. This fix makes disarm correct regardless of delivery timing,
  which is the right layer to solve the data-loss problem, but the underlying network
  flakiness may still delay *all* bot interactions. If it recurs, investigate the Pi's
  network or relocate the bot.
- **Late-arriving taps still execute.** A buffered tap is applied whenever it lands. For
  disarm this is the safe direction (it stops auto-selling). A late-arriving *arm* would
  also execute; arming is lower-risk but worth noting if stricter freshness is ever needed.
- **Verification was static + startup-level.** No armed positions existed at deploy time,
  so the end-to-end disarm path should be exercised live (arm → disarm → confirm the
  `⏹ Disarmed` reply) at the next opportunity.
