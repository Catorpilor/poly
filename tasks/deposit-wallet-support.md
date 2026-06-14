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

## MILESTONE 1 — Account-type detection + storage (DONE)
- [x] `migrations/006_account_type.sql` (+ down): `users.account_type`
      (`legacy_proxy` | `safe` | `deposit_wallet`), NOT NULL default `legacy_proxy`. Applied to dev DB.
- [x] `polymarket/account_type.go`: `AccountClassifier.Classify` (on-chain) —
      `factory()==depositWalletFactory` ⇒ deposit_wallet; `getOwners()` answers ⇒ safe; else
      legacy_proxy; empty code ⇒ unknown (caller defaults). `AccountType.SignatureType()` mapping.
- [x] Unit tests (`account_type_test.go`) with a mock eth client — all branches + mapping.
- [x] Model `User.AccountType` + repo Create/Update/3×SELECT plumbing (`accountTypeOrDefault`).
- [x] Wired into `/import` finalize (best-effort classify of resolved proxy).
- [x] Verified row fix: lastsaga set to `deposit_wallet` (proxy `0x4b2f…`); full suite + build green.
- [ ] DEFERRED to M3: the signer-type heuristic fix (proxy ⇒ Safe(2)) — belongs with the trading
      wiring that threads `account_type` into `ExecuteTrade`.

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

## MILESTONE 3 — Wire signer into trading (DONE for signing path)
NOTE (from M0): order submission is the SAME `POST /order` for all sig types; `POLY_ADDRESS` header
already = EOA. The deposit-wallet difference at trade time is purely the signature.
- [x] `resolveOrderSigner(eoa, proxy, accountType)` → (maker, signer, sigType): deposit_wallet ⇒
      maker==signer==proxy + POLY_1271 (routes to the M2 signer); legacy/Safe unchanged
      (maker=proxy, signer=EOA, POLY_GNOSIS_SAFE); no proxy ⇒ EOA. Unit-tested (6 cases).
- [x] `TradeRequest.AccountType` threaded into all 6 trade sites (buy/sell/sltp/webserver).
- [x] `ExecuteTrade` uses `resolveOrderSigner` instead of the proxy⇒Safe heuristic.
- [x] Minimal-impact: ONLY deposit_wallet gets new routing; legacy/Safe byte-identical to before.
- [x] Full suite + build green.
- [ ] GAP (only for a FRESH deposit wallet): one-time deploy (relayer-v2 WALLET-CREATE) + approvals
      (7 ERC-20 + 9 ERC-1155). lastsaga's wallet is already deployed+approved (traded on web), so M4
      can test without this. Implement before supporting brand-new deposit wallets.
- Note: legacy-vs-Safe distinction intentionally NOT changed (no evidence it's wrong; changing it
  risks regressing existing users). Only the new deposit_wallet branch was added.

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
