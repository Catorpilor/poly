# Polymarket CTF Exchange V2 Migration Plan

**Date**: 2026-04-07 (last updated 2026-04-30)
**Migration cutover**: **2026-04-28 ~11:00 UTC**, ~1 hour downtime
**Status**: ✅ **Cutover complete; bot migrated to V2 on `feat/v2-migration`** (2026-04-30)

## Outcome

Cutover happened on schedule. As of 2026-04-30 there is still no upstream Go SDK
(`github.com/Polymarket/go-order-utils` latest tag remains `v1.22.6` from 2025-08-19),
so the bot reimplements V2 order signing locally in `internal/polymarket/orderv2/`,
pinned byte-for-byte to `@polymarket/clob-client-v2@1.0.2` via golden tests.

**Key implementation notes captured during migration:**
- The V2 `Order` EIP-712 struct is **11 fields**, not 12. `expiration` is in the API
  JSON payload but is NOT in the signed message — including it would silently break
  signing.
- The V2 EIP-712 domain string `name` is unchanged ("Polymarket CTF Exchange") —
  only `version` bumps `"1"` → `"2"`. Both V2 exchanges share the same `chainId 137`
  on Polygon mainnet.
- The V2 API JSON payload drops `nonce`/`feeRateBps`/`taker` and adds
  `timestamp`/`metadata`/`builder`. `salt` is still serialized as an integer
  (parsed via `parseInt(order.salt, 10)` in the SDK).
- V2 bootstrapping requires: wrap any USDC.e → pUSD via Onramp.wrap(), approve
  pUSD for both V2 exchanges, and `setApprovalForAll` on CTF for both V2 exchanges.
  Implemented as `/migrate` Telegram command (`handleMigrate` in `handlers_migrate.go`).

**Code landmarks:**
- `internal/polymarket/orderv2/` — V2 order construction + EIP-712 signing
- `internal/polymarket/trading.go` — V2 order JSON shape in `submitOrder`
- `internal/blockchain/v2_bootstrap.go` — wallet-bootstrap planner
- `internal/telegram/handlers_migrate.go` — `/migrate` command
- `internal/config/config.go` — V2 addresses as defaults, env-overridable

The historical pre-cutover plan is preserved below.

---

## Executive Summary

Polymarket is upgrading its entire exchange stack on **2026-04-28 at ~11:00 UTC**: new CTF Exchange V2 contracts, a new collateral token (pUSD replacing USDC.e), updated CLOB-Client SDKs, and a **changed order struct / EIP-712 domain version**. The system uses a "hot-swap" mechanism — SDKs on the latest version auto-switch at cutover. All open orders are wiped during the ~1 hour maintenance.

**Our bot is directly impacted.** The order struct itself changed (fields added and removed), the EIP-712 domain version bumped `1`→`2`, the fee model moved from signed-in-order to protocol-computed at match time, and legacy SDKs stop working after cutover (no backward compatibility). Polymarket has shipped V2 SDKs only for TypeScript (`@polymarket/clob-client-v2@1.0.0`) and Python (`py-clob-client-v2==1.0.0`) — **a V2 `go-order-utils` / Go CLOB client has not been announced**, which is our highest-risk gap.

Pre-cutover testing URL: `https://clob-v2.polymarket.com` (becomes production after Apr 28).

---

## What's Changing

### 1. CTF Exchange V2 Contract
- **CTF Exchange V2**: `0xE111180000d2663C0091e4f400237545B87B996B`
- **Neg Risk CTF Exchange V2**: `0xe2222d279d744050d28e00520010520000310F59`
- **Order struct CHANGED** (contradicts earlier "unchanged" assumption):
  - **Removed fields**: `nonce`, `feeRateBps`, `taker`, `expiration`
  - **Added fields**: `timestamp` (ms — replaces nonce for uniqueness), `metadata` (bytes32), `builder` (bytes32), `builderCode` (optional)
- **EIP-712 domain version bumped** `"1"` → `"2"` (new typehash/domain separator — all signed orders from V1 signers will be rejected)
- Fees now determined at protocol match-time, **not embedded in signed orders**
- Makers never pay fees
- Solidity upgraded 0.8.15 → 0.8.30; Solady replaces OpenZeppelin
- `POLY_1271 = 3` signature type added (EIP-1271 smart-contract wallet sigs)

### 2. pUSD (New Collateral Token)
- USDC.e (`0x2791Bca1f2de4661ED88A30C99A7a9449Aa84174`) replaced by pUSD, a standard ERC-20 backed 1:1 by USDC with on-chain enforcement
- **pUSD (proxy)**: `0xC011a7E12a19f7B1f670d46F03B03f3342E82DFB`
- **pUSD (implementation)**: `0x6bBCef9f7ef3B6C592c99e0f206a0DE94Ad0925f`
- **Collateral Onramp**: `0x93070a847efEf7F70739046A929D47a521F5B8ee`
- **Collateral Offramp**: `0x2957922Eb93258b93368531d39fAcCA3B4dC5854`
- **CtfCollateralAdapter**: `0xADa100874d00e3331D00F2007a9c336a65009718`
- **NegRiskCtfCollateralAdapter**: `0xAdA200001000ef00D07553cEE7006808F895c6F1`
- API traders must call `wrap()` on the Collateral Onramp to convert USDC/USDC.e → pUSD

### 3. New CLOB-Client SDK
- **TypeScript**: `@polymarket/clob-client-v2@1.0.0` (replaces `@polymarket/clob-client`) — released
- **Python**: `py-clob-client-v2==1.0.0` (replaces `py-clob-client`) — released
- **Go SDK**: **not yet announced** — highest-risk gap for our bot
- Legacy packages **stop functioning after cutover** (no backward compatibility)
- Hot-swap: SDKs on the latest V2 version auto-detect the cutover and switch without manual intervention
- Constructor changes: positional args → options object; `chainId` → `chain`; remove `tickSizeTtlMs`

### 4. Fee Model Changes
- Fee formula `fee = C × feeRate × p × (1 - p)` is unchanged, but **application moved to protocol match-time**
- Clients no longer sign `feeRateBps` into orders — remove manual fee config
- Dynamic parameters now queried via `getClobMarketInfo(conditionID)` (replaces the old `/fee-rate` + Gamma `feeSchedule` dance)
- Makers never pay fees
- WebSocket `fee_rate_bps` field on `last_trade_price` events persists
- Market-buy orders: pass `userUSDCBalance` for accurate fee-adjusted fill calculation

### 5. Builder Program (Not currently used, but simplified)
- `@polymarket/builder-signing-sdk` eliminated; `POLY_BUILDER_*` HMAC headers removed
- Single `builderCode` (bytes32) attached to orders or set once at client construction
- Obtain code from Builder Profile settings if we ever opt in

### 6. Order Book Clearing
- All open orders cancelled during the ~1h window
- Bot must handle graceful shutdown and re-submission after migration
- Re-place orders immediately after cutover

### 7. API Authentication Unchanged
- L1 (EIP-712 `ClobAuthDomain` stays `"1"`) and L2 HMAC auth unchanged
- Existing `POLY_ADDRESS`, `POLY_SIGNATURE`, `POLY_TIMESTAMP`, `POLY_API_KEY`, `POLY_PASSPHRASE` headers remain valid
- No need to re-derive API credentials

---

## Impact Assessment

### P0 — Critical (Blocks Trading)

| Component | Files Affected | What Changes |
|-----------|---------------|--------------|
| **Collateral token** | `balance.go:30`, `redeem.go:16`, `positions_v2.go:40`, `config.go:172` | USDC.e → pUSD `0xC011a7E12a19f7B1f670d46F03B03f3342E82DFB` |
| **go-order-utils SDK** | `go.mod:12`, `vendor/github.com/polymarket/go-order-utils/` | **No V2 Go SDK announced** — may need to fork & patch (addresses, domain version `"2"`, new order struct) |
| **Order struct** | `vendor/.../pkg/model/`, `trading.go:708-719` | Fields changed: drop `nonce`/`feeRateBps`/`taker`/`expiration`; add `timestamp`/`metadata`/`builder`/`builderCode`. New EIP-712 typehash |
| **EIP-712 domain version** | Vendored SDK | Bump `"1"` → `"2"` — orders signed with V1 domain will be rejected by V2 exchange |
| **CTF Exchange address** | `ctf_exchange.go:26`, `balance.go:166`, `config.go:173` | V2: `0xE111180000d2663C0091e4f400237545B87B996B` |
| **NegRisk Exchange address** | `balance.go:167` | V2: `0xe2222d279d744050d28e00520010520000310F59` |
| **Fee handling** | `trading.go` fee-lookup path, `OrderData` | Stop signing `feeRateBps` into orders; query `getClobMarketInfo(conditionID)` instead of `/fee-rate` + Gamma `feeSchedule` |

### P1 — High (Affects Functionality)

| Component | Files Affected | What Changes |
|-----------|---------------|--------------|
| **Redemption collateral param** | `redeem.go:61-78` | `redeemPositions()` collateral address → pUSD |
| **Collateral wrapping** | New code needed | `wrap()` on Collateral Onramp `0x93070a847efEf7F70739046A929D47a521F5B8ee` to convert USDC/USDC.e → pUSD |
| **ERC-20 approvals on V2 exchange** | New code | `approve()` pUSD for both V2 exchanges; `setApprovalForAll` CTF for V2 exchanges |
| **Testing endpoint** | env override | Point to `https://clob-v2.polymarket.com` pre-cutover for validation |
| **Relayer** | `relayer.go` | Check for V2-compatible Safe module addresses; relayer URL likely unchanged |
| **Market-buy `userUSDCBalance`** | `trading.go` market-buy path | Pass current pUSD balance for fee-adjusted fill calc |

### P2 — Medium (Should Update)

| Component | Files Affected | What Changes |
|-----------|---------------|--------------|
| **WebSocket URL** | `manager.go:20` | URL may change; monitor announcements |
| **Balance display** | `balance.go`, Telegram handlers | Show "PUSD" instead of "USDC" in user-facing text |
| **ERC-20 approval flow** | New code | One-time approval for Polymarket USD spending by exchange |

### P3 — Low (Nice to Have)

| Component | Files Affected | What Changes |
|-----------|---------------|--------------|
| **EIP-1271 support** | Not needed unless we add smart contract wallet signing | `POLY_1271 = 3` signature type |
| **Builder codes** | Not currently used | On-chain order attribution via builder API |

---

## Affected Files — Full Inventory

### Contract Addresses (Hardcoded)

```
internal/blockchain/balance.go:30    — USDC address in BalanceChecker
internal/blockchain/balance.go:166   — CTFExchangeAddress constant
internal/blockchain/balance.go:167   — NegRiskExchangeAddress constant
internal/blockchain/redeem.go:16     — USDCAddress package var
internal/polymarket/ctf_exchange.go:26 — ctfExchange in CancelClient
internal/polymarket/positions_v2.go:40 — usdcAddress in PositionManagerV2
internal/config/config.go:172        — cfg.Polymarket.USDCAddress default
internal/config/config.go:173        — cfg.Polymarket.CTFExchangeAddress default
```

### SDK / Order Signing

```
go.mod:12                            — go-order-utils v1.22.6 dependency
vendor/github.com/polymarket/go-order-utils/  — vendored SDK (contract addrs, domain separator, order builder)
internal/polymarket/trading.go:708-736 — OrderData construction + BuildSignedOrder call
internal/polymarket/trading.go:727-732 — model.CTFExchange / model.NegRiskCTFExchange selection
```

### API URLs (Configurable via env, lower risk)

```
internal/config/config.go:167    — CLOB API URL (env override available)
internal/config/config.go:168    — Data API URL (env override available)
internal/config/config.go:199    — Relayer URL (env override available)
internal/polymarket/markets.go:106   — Gamma API URL (hardcoded)
internal/polymarket/trading.go:1122  — Gamma API URL (hardcoded)
internal/polymarket/trading.go:1168  — Gamma API URL (hardcoded)
internal/live/resolver.go:104        — Gamma API URL (hardcoded)
internal/live/manager.go:20          — WebSocket URL (hardcoded)
```

### Documentation

```
docs/trading.md     — Contract addresses table, API URLs, code examples
docs/redeem.md      — Redemption flow, contract addresses, collateral references
```

---

## Implementation Plan

### Phase 1: Preparation (Now — Before V2 SDK Release)

**1.1 Make contract addresses configurable**

Currently, contract addresses are hardcoded in multiple files. Extract them into `config.go` so they can be overridden via environment variables without code changes.

```go
// config.go additions
cfg.Polymarket.CollateralAddress     = getEnv("POLYMARKET_COLLATERAL_ADDRESS", "<current USDC.e>")
cfg.Polymarket.CTFExchangeAddress    = getEnv("POLYMARKET_CTF_EXCHANGE_ADDRESS", "<current>")
cfg.Polymarket.NegRiskExchangeAddress = getEnv("POLYMARKET_NEGRISK_EXCHANGE_ADDRESS", "<current>")
cfg.Polymarket.CollateralOnrampAddress = getEnv("POLYMARKET_COLLATERAL_ONRAMP_ADDRESS", "")
```

Pass these through to `BalanceChecker`, `RedeemEncoder`, `CancelClient`, etc. instead of hardcoding.

**Files to change:**
- `internal/config/config.go` — add new config fields
- `internal/blockchain/balance.go` — accept addresses from config
- `internal/blockchain/redeem.go` — accept collateral address from config
- `internal/polymarket/ctf_exchange.go` — accept exchange address from config
- `internal/polymarket/positions_v2.go` — accept collateral address from config

**1.2 Make Gamma API URL configurable**

Three files hardcode `https://gamma-api.polymarket.com`. Add an env var override.

**Files to change:**
- `internal/polymarket/markets.go:106`
- `internal/polymarket/trading.go:1122, 1168`
- `internal/live/resolver.go:104`

**1.3 Add collateral wrapping support**

Implement a `WrapCollateral()` function that calls the Collateral Onramp contract's `wrap()` function. This converts USDC/USDC.e → Polymarket USD.

```go
// internal/blockchain/collateral.go (new file)
func EncodeWrapCollateral(amount *big.Int) ([]byte, error) {
    // ABI-encode wrap(uint256 amount) call
}
```

This can be submitted via the relayer (gas-less) or as a direct transaction.

**1.4 Add ERC-20 approval for new exchange**

The new CTF Exchange V2 contract address will need `approve()` or `setApprovalForAll()` from the user's proxy wallet. Implement a helper and integrate it into the first-trade flow.

### Phase 2: SDK Update (Go V2 SDK — status unconfirmed)

**2.1 Determine Go SDK path**

Polymarket has shipped V2 SDKs for TypeScript and Python but **not Go**. Options:

1. **Wait**: monitor `Polymarket/go-order-utils` for a v2 tag
2. **Fork & patch** (recommended fallback): fork `go-order-utils` and update:
   - `pkg/config/config.go` — V2 contract addresses (see Quick Reference table)
   - `pkg/model/order.go` — new order struct fields
   - EIP-712 domain `version` field → `"2"`
   - Order typehash recomputed to match V2 exchange
3. **Reimplement locally**: drop the vendored SDK and inline the minimum EIP-712 / order-signing logic in `internal/polymarket/`

```bash
# once a V2 tag exists:
go get github.com/polymarket/go-order-utils@v2.x.x
go mod vendor
```

**2.2 Update `OrderData` construction**

The V2 order struct (from official docs):
- **Drop**: `nonce`, `feeRateBps`, `taker`, `expiration`
- **Add**: `timestamp` (ms), `metadata` (bytes32), `builder` (bytes32), `builderCode` (optional)

Rework `trading.go:708-719`:
- Replace `nonce` with current millisecond `timestamp`
- Remove `feeRateBps` from the signed payload (fees are protocol-computed now)
- Set `builder` and `metadata` to zero bytes32 unless we opt into the builder program
- Remove `expiration` handling (or verify if SDK absorbs it into metadata)

**2.3 Update fee handling**

Drop the current two-layer lookup (`CalcFeeBps` from Gamma `feeSchedule` + `TakerFeeBps` from CLOB `/fee-rate`) — V2 computes fees at match-time. Migration steps:
- Stop signing any fee into the order
- Add `getClobMarketInfo(conditionID)` lookup for display-only fee preview
- Retain Gamma fee-schedule polling only if still needed for UX projections
- WebSocket `fee_rate_bps` on `last_trade_price` is still emitted — useful for accurate post-trade accounting

**2.4 Pre-cutover validation against `https://clob-v2.polymarket.com`**

Add a config flag (e.g. `POLYMARKET_CLOB_URL`) and point it at the V2 staging URL. Place a dust order, verify signing + acceptance, cancel, then revert. Do this well before Apr 28.

### Phase 3: Migration Day — 2026-04-28 ~11:00 UTC

**3.1 Pre-migration checklist (by 2026-04-27)**
- [ ] Go SDK path chosen (wait vs fork vs reimplement) and working
- [ ] V2 addresses configured (see Quick Reference)
- [ ] pUSD + Collateral Onramp wiring tested
- [ ] Collateral wrapping flow tested against `clob-v2.polymarket.com`
- [ ] ERC-20 approvals planned for proxy wallet: pUSD → V2 CTF Exchange, pUSD → V2 NegRisk Exchange, CTF `setApprovalForAll` for both V2 exchanges
- [ ] New order struct signs successfully against the V2 staging CLOB
- [ ] Fee path reworked (no `feeRateBps` in signed payload)
- [ ] SL/TP pause window (`internal/live/v2_cutover.go`) verified: `V2CutoverStart`/`V2CutoverEnd` bracket the announced downtime; `SLTPMonitor.evaluate` skips firing + sends one-time notice during the window
- [ ] CLOB market WebSocket (`wss://ws-subscriptions-clob.polymarket.com/ws/market`, consumed by `PriceFeedManager`) endpoint + message schema survive the cutover — if not, update `clobMarketWSURL` in `internal/live/pricefeed.go` and re-validate `event_type=book`/`price_change` parsing

**3.2 During the ~1h maintenance window**
- Bot should detect 503 / order rejection and back off
- Log maintenance state clearly; don't burn retries
- API auth headers remain valid — no re-auth churn

**3.3 Post-migration**
- Confirm orders land at production `https://clob.polymarket.com` (V2 takes over this URL)
- Wrap USDC.e balance → pUSD via Collateral Onramp
- Execute the approval set above from the proxy wallet
- Submit a dust order to validate end-to-end signing + matching
- Monitor WebSocket feed — payloads should be mostly unchanged, `fee_rate_bps` still present

### Phase 4: Cleanup

- Remove V1 contract address fallbacks once V2 is stable
- Update `docs/trading.md` and `docs/redeem.md` with V2 addresses
- Update `docs/ARCHITECTURE.md` if contract interaction patterns changed
- Update user-facing messages (USDC → PUSD/Polymarket USD labeling)

---

## Risk Assessment

| Risk | Likelihood | Impact | Mitigation |
|------|-----------|--------|------------|
| No V2 Go SDK before 2026-04-28 | **High** | **Critical** — can't sign V2 orders | Plan to fork `go-order-utils` and patch: V2 addresses, new order struct, domain version `"2"`, new typehash. Validate against `clob-v2.polymarket.com` |
| Order struct mismatch (missed field add/remove) | Medium | **Critical** — orders silently rejected | Cross-reference V2 TS/Python SDK source for the canonical struct; golden-test EIP-712 hash |
| EIP-712 domain version not bumped to `"2"` | Medium | **Critical** — all orders rejected | Explicit test against staging URL before cutover |
| Fee still signed into order post-migration | Medium | High — order rejection or incorrect fills | Remove `feeRateBps` from `OrderData` entirely; rely on protocol-side fee calc |
| Approvals missing on V2 exchange | Medium | High — first trades revert | Execute approval set immediately post-cutover as part of startup check |
| Relayer Safe-module format drift | Low | Medium — gas-less txs break | Relayer URL env-configurable; watch announcements |
| WebSocket URL drift | Low | Medium | Manager reconnection logic already handles disconnects |

---

## Monitoring Checklist

Migration date confirmed: **2026-04-28 ~11:00 UTC**. Watch daily:

- [ ] **GitHub**: [polymarket/go-order-utils releases](https://github.com/Polymarket/go-order-utils/releases) — V2 SDK tag (highest priority — still absent as of 2026-04-19)
- [ ] **TS SDK reference**: [@polymarket/clob-client-v2 source](https://www.npmjs.com/package/@polymarket/clob-client-v2) — canonical order struct + EIP-712 encoding to mirror in Go
- [ ] **Python SDK reference**: `py-clob-client-v2` source — second reference implementation
- [ ] **Docs**: [docs.polymarket.com/v2-migration](https://docs.polymarket.com/v2-migration) — migration guide changes
- [ ] **Docs**: [docs.polymarket.com/resources/contracts](https://docs.polymarket.com/resources/contracts) — address updates
- [ ] **Staging**: `https://clob-v2.polymarket.com` — reachable and accepting orders
- [ ] **Twitter/X**: [@PolymarketDevs](https://x.com/PolymarketDevs) — late-breaking announcements
- [ ] **Discord**: Polymarket developer channel — SDK release notes

---

## Quick Reference: Current vs V2

| Component | Current (V1) | V2 (Confirmed) |
|-----------|-------------|----------------|
| Collateral | USDC.e `0x2791Bca1...84174` | pUSD `0xC011a7E12a19f7B1f670d46F03B03f3342E82DFB` |
| CTF Exchange | `0x4bFb41d5...8982E` | `0xE111180000d2663C0091e4f400237545B87B996B` |
| NegRisk Exchange | `0xC5d563A3...76045` | `0xe2222d279d744050d28e00520010520000310F59` |
| Collateral Onramp | n/a | `0x93070a847efEf7F70739046A929D47a521F5B8ee` |
| Collateral Offramp | n/a | `0x2957922Eb93258b93368531d39fAcCA3B4dC5854` |
| CtfCollateralAdapter | n/a | `0xADa100874d00e3331D00F2007a9c336a65009718` |
| NegRiskCtfCollateralAdapter | n/a | `0xAdA200001000ef00D07553cEE7006808F895c6F1` |
| NegRisk Adapter | — | `0xd91E80cF2E7be2e162c6513ceD06f1dD0dA35296` |
| ConditionalTokens | `0x4D97DCd9...76045` | `0x4D97DCd97eC945f40cF65F87097ACe5EA0476045` (unchanged) |
| go-order-utils | v1.22.6 | **No V2 tag announced** |
| TS SDK | `@polymarket/clob-client` | `@polymarket/clob-client-v2@1.0.0` |
| Python SDK | `py-clob-client` | `py-clob-client-v2==1.0.0` |
| Order Struct | 12-field EIP-712 (`nonce`, `feeRateBps`, `taker`, `expiration`, …) | **Changed**: drop `nonce`/`feeRateBps`/`taker`/`expiration`; add `timestamp`/`metadata`/`builder`/`builderCode` |
| EIP-712 domain version | `"1"` | **`"2"`** |
| ClobAuth domain version | `"1"` | `"1"` (unchanged) |
| Signature Types | EOA(0), POLY_PROXY(1), POLY_GNOSIS_SAFE(2) | + POLY_1271(3) |
| Fee Formula | `C × feeRate × p × (1-p)` signed into order | Same formula, **computed at match-time, not signed** |
| Fee Lookup | CLOB `/fee-rate` + Gamma `feeSchedule` | `getClobMarketInfo(conditionID)` |
| CLOB API (prod) | `https://clob.polymarket.com` | Same URL post-cutover |
| CLOB API (staging) | — | `https://clob-v2.polymarket.com` |
| Relayer | `https://relayer-v2.polymarket.com` | Likely unchanged |
| WebSocket | `wss://ws-live-data.polymarket.com` | Unchanged; payloads mostly unchanged |
| Solidity | 0.8.15 (OpenZeppelin) | 0.8.30 (Solady) |
