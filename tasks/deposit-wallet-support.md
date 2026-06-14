# Deposit Wallet Support (Polymarket new-architecture accounts)

**Goal:** let the bot trade for accounts on Polymarket's new "deposit wallet" flow,
**without breaking** legacy proxy/Safe accounts. Both account types coexist behind
strategy interfaces selected per-user.

## Why (evidence)
Legacy buy from a new-architecture account is rejected by the CLOB:
```
POST /order -> {"error":"maker address not allowed, please use the deposit wallet flow"}
```
The order itself was correct (maker = real proxy `0x4b2f…`, signer = imported EOA `0xf6C7…`),
so this is an account-architecture gate, not a derivation bug. The current bot signs a
classic EIP-712 order (`signatureType: 2`) and submits `POST /order`; that path is gated
off for new accounts.

## Confirmed facts (lastsaga case, 2026-06-14)
- Imported EOA (signer): `0xf6C73cB22eEA470d479a3e40a1BaD1292f318B01`
- Trading proxy (maker, holds positions): `0x4b2f0b7B91319419d52f01cAf7D73D453770318b`
  - minimal-clone proxy, `owner()` == imported EOA, deployed by NEW factory
    `0x00000000000Fb5C9ADea0298D729A0CB3823Cc07` (old-factory resolver derives the WRONG addr —
    see [[project_new_proxy_factory]]).
- Deposit wallet: `0xB8bd619f86eA921b2811bb9D5063922Dd36AbEb1`
  - EIP-7702 delegated EOA, code = `0xef0100` + impl `0xe6Cae83BdE06E4c305530e199D7217f42808555B`.
- Required signing: **signature type 3 = ERC-7739-wrapped ERC-1271** (per combos/RFQ docs;
  the deposit-wallet flow uses `relayer-v2`, possibly a different exchange).

## Reuse vs net-new
| Area | Reuse | Net-new |
|---|---|---|
| EIP-712 | `orderv2` domain/struct hashing, order model, `SignatureType` enum (add type 3) | ERC-7739 nesting + ERC-1271 signer |
| Submission | L2 auth (HMAC), HTTP plumbing | deposit-wallet submit path (relayer-v2 / new endpoint) |
| Wallet | `wallet.Manager`, key handling | account-type detection on import |
| DB | repo + migration convention | `account_type` column |
| Telegram | buy/sell UX unchanged | none (routing is below the UI) |

This is the SAME machinery the combos/RFQ taker feature needs (relayer-v2, sig type 3),
so the discovery spike and the type-3 signer are shared with [[tasks/todo.md]] (combos).

---

## MILESTONE 0 — Discovery spike (MOSTLY DONE; see docs/deposit-wallet-flow.md)
Reverse-engineered from real SDK source (npm pack + git clone of clob-client-v2 / py-clob-client-v2).
- [x] ERC-7739 wrapping — full 5-step `TypedDataSign` algorithm captured (field order, sub-domain folding).
- [x] signatureType=3 (POLY_1271) payload — packed `signature` = innerSig(65)‖appDomainSep(32)‖
      contentsHash(32)‖contentsType‖uint16 len. `maker == signer == deposit-wallet contract`.
- [x] **maker resolved**: `0x4b2f` IS the deposit wallet (deployed by `depositWalletFactory 0x…Cc07`,
      `owner()`=EOA, holds positions). `0xB8bd` (EIP-7702) is a separate funding address.
- [x] **Submission CORRECTED**: same `POST /order` CLOB endpoint for ALL sig types — NOT a relayer.
      relayer-v2 is only wallet-deploy + gasless approvals.
- [x] Approvals: 7 ERC-20 max + 9 ERC-1155 setApprovalForAll; non-EOA bundles via relayer.
- [x] **ORDER_TYPE_STRING resolved**: 186 bytes → trailer `0x00ba` (measured from source; both agents wrong).
- [x] Verifying contract = CTF Exchange V2/V3 (NOT the deposit wallet).
- [x] Output written: `docs/deposit-wallet-flow.md`.
- [x] **Golden vector captured + reproduced in Go** — independent go-ethereum impl matches the SDK's
      EXPECTED_POLY_1271_SIGNATURE byte-for-byte (appDomainSep/contentsHash/digest all match). §3
      algorithm confirmed end-to-end; trailer = 186/0x00ba. Vector recorded in docs §7.
- [ ] REMAINING BLOCKERS before build (confidence now MEDIUM-HIGH):
      - confirm on-chain `isValidSignature` reconstruction (Solady 1271 verifier not in source) — the
        signer is proven, but acceptance by the live wallet contract is still unverified;
      - resolve collateral token for V2 (USDC.e `0xC011…` vs pUSD/Polymarket USD) for approvals;
      - confirm delegate/impl `0xe6Cae8…555B` vs beacon/impl; one real end-to-end live order.

## MILESTONE 1 — Account-type detection + storage
- [ ] `migrations/00X_account_type.sql` (+ down): add `users.account_type`
      (`legacy_proxy` | `safe` | `deposit_wallet`), default `legacy_proxy`, backfill existing rows.
- [ ] Detector (on-chain, reuses RPC): inspect proxy/account code —
      EIP-7702 prefix `0xef0100`, new-factory clone, or Safe `getOwners()` — to classify.
      Fall back to a Polymarket lookup if needed.
- [ ] Set `account_type` at `/import`; expose for routing. TDD with recorded bytecode fixtures.
- [ ] FIX existing bug: signer-type heuristic assumes proxy ⇒ Safe(2); make it derive from
      `account_type`/on-chain type instead (`0x4b2f` is a clone, not a Safe).

## MILESTONE 2 — Signer (DONE)
- [x] `orderv2/deposit_wallet.go`: `BuildSignedDepositWalletOrder` + ERC-7739 helpers
      (exchangeDomainSeparator / orderStructHash / typedDataSignStructHash); produces the packed
      `innerSig(65)‖appDomainSep(32)‖contentsHash(32)‖contentsType‖uint16` blob.
- [x] Coexistence: `BuildSignedOrder` dispatches POLY_1271 → deposit-wallet path; legacy 0/1/2
      ECDSA path unchanged (regression test `TestLegacyPathUnchangedForEOA`).
- [x] Golden test `deposit_wallet_test.go`: full packed sig + intermediates (appDomainSep,
      contentsHash) match the SDK vector byte-for-byte; trailer/maker==signer invariants.
- [x] `go test -race ./internal/polymarket/orderv2/` green; full suite green; build clean.
- Note: signer strategy kept as a Builder method + dispatch (simpler than a separate interface);
  revisit interface extraction only if a second submitter path is needed (it isn't — same /order).

## MILESTONE 3 — Submission + approvals + fix proxy derivation
NOTE (from M0): order submission is the SAME `POST /order` for all sig types — no submitter split needed.
The deposit-wallet difference is (a) the signature (M2) and (b) one-time setup via relayer-v2.
- [ ] One-time deposit-wallet setup: ensure wallet deployed (relayer-v2 WALLET-CREATE) + approvals
      (7 ERC-20 + 9 ERC-1155, bundled gasless via relayer-v2) before first order.
- [ ] `POLY_ADDRESS` auth header = EOA (not the wallet); reuse existing L2 HMAC creds.
- [ ] Resolve proxy correctly for new-factory accounts (close [[project_new_proxy_factory]]):
      on-chain `owner()` reverse-check or Polymarket signer→proxy lookup; store on user.
- [ ] `ExecuteTrade` becomes a thin orchestrator: pick Signer + Submitter by `account_type`.

## MILESTONE 4 — Wire-through + verification
- [ ] Route buy/sell (and SL/TP auto-sell) through the strategies; legacy path byte-for-byte unchanged.
- [ ] Live verification: a real $1 buy + sell on a deposit-wallet account succeeds; legacy account
      still works (regression check). Diff payloads vs main.
- [ ] `go test ./... -race` green.

---

## Coexistence design (summary)
- `users.account_type` chosen at import via on-chain detection.
- `ExecuteTrade(user, ...)` selects `OrderSigner` + `OrderSubmitter` off `account_type`.
- Legacy accounts keep their exact current code path → near-zero regression risk.
- `SignatureType` enum already multiplexes (0/1/2); type 3 is the natural extension.

## Top risks
1. **ERC-7739 nesting** is the make-or-break detail; the docs omit it → spike-gated, golden-vector tested.
2. **No Go SDK** — protocol reverse-engineered from TS; isolate behind the signer/submitter interfaces.
3. **Unknown maker address + endpoint** for the deposit-wallet flow — Milestone 0 must pin both.
4. **Beta/V2 churn** — new exchange/relayer contracts may change; keep them config-driven.
5. **Regression on legacy** — mitigate by extracting the legacy path unchanged + a regression trade.

## Notes
- Legacy wallets work today; this is additive, not urgent.
- Shares the Milestone 0 spike with the combos plan ([[tasks/todo.md]]); do once, use twice.
- Related: [[project_v2_migration]], [[project_new_proxy_factory]].

## Review
(to be filled in as milestones land)
