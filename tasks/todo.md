# In-band holdings gate: no auto top-up of a token the recipient already holds

Grilled 2026-09-20 (grill-with-docs). Trigger: zeh manually bought an esports
token at 0.18, the buy registered a Held Watch, and the $10 in-band auto-buy
fired on top seconds later ("r107 pattern", 7 occurrences by 08-29).

## Evidence
- Ledger (scored 2026-09-20): in-band $10 autos that were top-ups of an
  already-held token = 2W/19L (9.5%), net −$85.86 (losses −$170.15; wins r29
  +$56.29, r113 +$28.00 — both cheap recent knife-buys) vs non-top-up in-band
  ≈19.7% W. 14 rows ambiguous on timing, unscored.
- Prior art: deep-tier holdings gate 24/24 correct (retired with the tier,
  `snipeBuyDeepHeld` is a dead enum); manual-armed gate 2/2 (#86, v0.21.2).
- Code fact: the $10 path has NO own-token holdings check; the per-session
  bought latch is snipe-path-only by design; `snipeHoldsSibling` checks only
  the OTHER side; Held Watch registers BOTH sides with no side info.

## Decisions (ratified)
1. Scope: any holding of the ALERTED token, any source (bot/web/tap/pre-restart),
   with or without an Arm ⇒ alert-only + tap buttons. Reverses v0.16.0 "deep
   only" and v0.21.2 "TP-only arms top up freely". No source-awareness, no
   size floor.
2. Precedence: holdings gate runs BEFORE case-3. Holding the crashed side
   (with or without the other side) = case 1 = alert-only, no boxed latch.
   Case 3 = holds ONLY the other side. Boxed rung fires (NotifySnipeBoxed)
   stay un-checked (rung 2 lands on the token rung 1 bought). Manual taps
   bypass, as every gate.
3. Evidence tiers, cheapest first, first hit wins, `via` names the tier:
   a. `bought`   — in-memory snipe bought record (`snipeBought.held`).
   b. `held`     — NEW lag-free bought-side mark on the watcher's holder entry,
                   set by every buy/positions path that registers Held Watch,
                   for the token actually bought/held; same TTL/lifecycle as
                   the holder entry (in-memory, janitor-swept, no restart
                   survival). Sibling-only registration NEVER sets it; walked
                   never sets it.
   c. `arm`      — any `sltp_arms` row for (chat, alerted token). SLArmed ⇒
                   existing `manual-armed` outcome/copy/log; TP-only ⇒
                   holdings-gated. Read error ⇒ log, fall through (fail-open).
   d. `positions`— Data API positions by token ID, Shares > 0 (dust counts).
                   Read error ⇒ log, buy proceeds (fail-open). The alert path
                   must fetch positions AT MOST ONCE (share with the sibling
                   check), never twice.
4. Lost free TP-arm is accepted: gate only; no arm button, no auto-arm of a
   position the machine never bought. Follow-up for October: count gated
   alerts whose held tranche later went unarmed and missed a harvest.
5. Naming: rename `snipeBuyDeepHeld` → `snipeBuyAlreadyHeld`, keep copy
   "you already hold this token — not topping up a held position" (template
   appends "— tap below if you still want it."). Log line:
   `Snipe auto-buy: holdings-gated chat=%d token=%.12s… via=%s`.
   Mark-set path gets one debug log line (lesson: silent-success paths).
6. No ADR. Records = CONTEXT.md (Comeback Snipe, Boxed Snipe, Held Watch —
   done, fix "Since 2026-09" to the deploy date at ship), GitHub issue with
   evidence + rejected options (source-aware, size floor, per-market mute,
   exposure cap, arm button), ledger regime-change annotation at deploy.

## Implementation map
- `internal/live/snipe_watcher.go`: `holderEntry` gains a bought-side flag;
  new `WatchBought(chatID, m, ttl)` (direct + flag) or `MarkHeld`; `Holds(chatID,
  tokenID) bool` (entry exists, not expired, flag set). `RenewHeldMarket`'s
  anchor token is a real holding ⇒ set the flag on the anchor only; group
  renewals never set it. Walked entries never set it.
- Registration call sites (set the flag on the bought/held token, siblings
  stay plain WatchHeld): `snipeRegisterBoughtToken` (bot.go:1369, bot.go:1539,
  snipe.go:1721 tap — idx names the token), `snipeWatchHeldMarket` /
  `registerSnipeHeld` positions refresh (the held token), `manager.RegisterHeldBuy`
  web path (tokenID param names the bought token).
- `internal/telegram/snipe.go` `snipeAutoBuy`: after wallet lookup, BEFORE
  case-3: `snipeHoldsAlerted(ctx, user, chatID, market) (via string, held bool)`.
  The post-case-3 manual-arm block is subsumed (tier c) — remove it, keep its
  outcome/class/copy/fail-open semantics and tests.
- Positions single-fetch: lazy fetch shared by tier d and `snipeHoldsSibling`
  (pass the fetched slice / a once-func into the sibling check).

## Tests (RED first, table-driven, `-race`)
- New `internal/telegram/snipe_holdings_test.go`:
  - tier a/b/c/d each gate with the right `via`; sibling-only held registration
    does NOT gate; arm read error falls through to positions; positions read
    error ⇒ buy proceeds; Shares>0 dust gates; SL-armed ⇒ `snipeBuyManualArmed`.
  - precedence: holds both sides ⇒ holdings-gated AND no boxed latch armed;
    holds only sibling ⇒ boxed path byte-identical to today.
  - exactly ONE positions fetch when both checks run (count scanner calls).
  - manual tap on a gated alert still buys; boxed rung fire with rung-1 held
    still fires.
  - gated alert text carries the copy + tap tail; cap untouched (no reserve).
- `snipe_manualarm_test.go`: "TP-only does not gate" flips to holdings-gated
  via=arm; SL-armed + fail-open cases unchanged.
- `internal/live/snipe_watcher_test.go` (or new `snipe_holds_test.go`):
  `Holds` true after bought registration, false for sibling-only, false after
  TTL/janitor, false for walked; renew anchor sets it, group renew doesn't.
- `manager` web path: RegisterHeldBuy sets the flag on tokenID only.

## Plan
- [x] File GitHub issue (evidence table, decisions, rejected options) → #111
- [x] RED: failing tests above (opus subagent, confirmed red on undefined symbols)
- [x] GREEN + REFACTOR; `go test ./...` and `go test -race ./...` green
- [x] Adversarial verify loop until SHIP (independent subagent, -count=2)
  - round 1: NO-SHIP. Real: (1) limit-order PLACEMENT path marked the token
    held (resting order ≠ holding); (2) mark never decayed — group renewals
    kept it alive after a sell, plain watches resurrected an expired mark
    → fix = mark gets its own expiry `holdsUntil`, only buys/anchor sightings
    extend it; (3) tier `arm` narrowed to SL||TP was wrong — ClearTP leaves
    (false,false) rows on a still-held 75% → any row gates. Coverage gaps:
    mark debug line, once-fetch on error path, zero-share positions marking.
    Accepted as pre-existing: per-recipient positions read on the alert path
    (sibling-watched recipients already paid it); arm-read failure log wording
    changed (note for the monitor).
  - round 2: NO-SHIP. F1/F3/F4/F6/F7 fixed. Blocker: `registerSnipeHeld` →
    `RenewHeldMarket` marks the anchor for EVERY position row, and the Data
    API keeps returning sold positions at 0 shares ⇒ sold token re-marked on
    every refresh, gated forever. Fix = renew path marks only when Shares>0
    (same predicate as the fallback branch). Also: `Holds` re-checks entry
    expiry (invariant holdsUntil ≤ expiry made explicit); limit-placement
    call site documented as deliberately non-marking (no handler harness —
    accepted unpinned).
  - round 3: SHIP. Renew path marks only on Shares>0 (one production caller);
    Holds re-checks entry expiry; all round-1/2 mutations re-run and bite.
- [ ] CONTEXT.md date fix; PR referencing the issue; merge; tag v0.26.0
- [ ] Deploy on the Pi (poly_deploy compose tag bump), verify startup logs
- [ ] Smoke: small manual buy on a watched market ⇒ mark log line present
- [ ] Ledger: regime-change annotation (date, tag, issue); monitor forwards
      `holdings-gated` lines; October review scores skips per `via`

## Review
(filled at ship)
