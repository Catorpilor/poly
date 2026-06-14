# Deposit-Wallet Flow — Milestone 0 Spec

> Authoritative implementation spec for the Polymarket **deposit-wallet** (smart-contract wallet, `signatureType = 3 / POLY_1271`) trading path.
> The bot is Go and there is **no Go SDK**, so the Go implementation follows this document.
> Source of truth: the acquired TypeScript (`clob-client-v2`) and Python (`py-clob-client-v2`) SDKs, plus the beta `@polymarket/client` bundle (with source maps). Where a claim is **not** in source it is marked **INFERRED** or listed as an **open question**. Do not treat anything here as protocol truth unless it is tagged CONFIRMED.

---

## 1. Overview

A **deposit wallet** is a Polymarket smart-contract wallet (EIP-1271 / EIP-7702-style account) that holds the user's assets and authorizes orders via its `isValidSignature` method. It is the V2/V3-era replacement for the legacy Gnosis-Safe / proxy wallet flow.

How it differs from the legacy flow:

| Aspect | Legacy (sigType 0/1/2) | Deposit wallet (sigType 3 / POLY_1271) |
|---|---|---|
| `order.signer` | the **EOA** | the **deposit-wallet contract address** (skips EOA-equality check) |
| `order.maker` | EOA or proxy/safe (funds source) | the **deposit-wallet contract address** (funds source) |
| Signature bytes | plain **65-byte ECDSA** EIP-712 sig over the Order | **packed ERC-7739 blob** (`innerSig ‖ appDomainSep ‖ contentsHash ‖ contentsType ‖ len`) |
| On-chain verification | `ecrecover` against EOA | contract `isValidSignature` (EIP-1271 magic `0x1626ba7e`) reconstructing the 7739 digest |
| Wallet deployment | proxyFactory / safeFactory | `depositWalletFactory` (CREATE2 beacon proxy) |
| Order submission | `POST /order` on CLOB | **same** `POST /order` on CLOB (NOT a relayer) |
| Approvals / deploy | EOA sends txs | **gasless** via relayer-v2 |

Key correction to common assumptions (CONFIRMED, facet "submission path"): deposit-wallet **orders are NOT relayed**. They are POSTed to the same CLOB `/order` endpoint as every other order, with `order.signatureType = 3`. The relayer-v2 is used only for **deploying the wallet** and **gasless approvals**, not for order matching.

---

## 2. Addresses — signer vs maker vs deposit wallet

### Roles (CONFIRMED from `createOrder.ts:22-25`, `buildOrderCreationArgs.ts:82-89`, `exchangeOrderBuilderV2.ts:96-102`)

- **EOA** — the externally-owned account whose private key actually produces the ECDSA `innerSig`. It does **not** appear in `order.maker`/`order.signer` for a deposit wallet. It is carried in the `POLY_ADDRESS` auth header (`headers/index.ts:35`) and as L2 HMAC credentials.
- **`order.maker`** — the source of funds = the **deposit-wallet contract address**.
- **`order.signer`** — for `POLY_1271`, `signerForOrder = (signatureType === POLY_1271) ? maker : eoaSignerAddress`, i.e. **also the deposit-wallet contract address**. The verifying contract that runs `isValidSignature`.
- So for a deposit wallet: **`maker === signer === deposit-wallet contract address`**, and they differ from the controlling EOA.

### Factory / implementation registry (CONFIRMED, beta client `production` env, `index.js`)

| Contract | Address |
|---|---|
| `depositWalletFactory` | `0x00000000000Fb5C9ADea0298D729A0CB3823Cc07` |
| `depositWalletBeacon` | `0x7A18EDfe055488A3128f01F563e5B479D92ffc3a` |
| `depositWalletImplementation` | `0x58CA52ebe0DadfdF531Cde7062e76746de4Db1eB` |
| `proxyFactory` (legacy proxy) | `0xaB45c5A4B0c941a2F231C04C3f49182e1A254052` |
| `proxyImplementation` (legacy) | `0x44e999d5c2F66Ef0861317f9A4805AC2e90aEB4f` |
| `safeFactory` (legacy) | `0xaacFeEa03eb1561C4e67d661e40682Bd20E3541b` |
| `protocolV2Router` | `0x12121212006e4CD160D18e3f00711DA5c3372600` |
| `relayHub` | `0xD216153c06E857cD7f72665E0aF1d7D82172F494` |

`WalletType.DEPOSIT_WALLET → SignatureType.POLY_1271` (CONFIRMED, `chunk-2ZZDFOKL.js`).

### The "lastsaga" worked example (CORRECTED with on-chain evidence)

The discovery agents could not find these addresses in source and guessed the mapping (calling `0x4b2f` a "legacy proxy"). That guess was **wrong**. Corrected here from direct on-chain reads (Polygon, via the dev proxy) cross-checked against the CONFIRMED factory registry above:

| Role | Value | Evidence |
|---|---|---|
| EOA (controlling key) | `0xf6C73cB22eEA470d479a3e40a1BaD1292f318B01` | imported key; `0x4b2f.owner()` returns it |
| **Deposit wallet** (`maker == signer`, holds positions) | `0x4b2f0b7B91319419d52f01cAf7D73D453770318b` | `0x4b2f.factory()` on-chain = `0x00000000000Fb5C9ADea0298D729A0CB3823Cc07` = **`depositWalletFactory`** (registry above); 125-byte contract holding the FIFA positions per data-api |
| Funding/deposit address (EIP-7702) | `0xB8bd619f86eA921b2811bb9D5063922Dd36AbEb1` | EOA delegated via 7702 → impl `0xe6Cae83BdE06E4c305530e199D7217f42808555B`; **empty** of positions — a separate funding address, NOT the trading wallet |

So `0x4b2f` **is** the deposit wallet (deployed by `depositWalletFactory`), not a legacy proxy. This reconciles the failed buy: the bot sent `maker=0x4b2f` (correct) but `signer=0xf6C7` (should be `0x4b2f`) and `sigType=2` (should be `3` + ERC-7739) → CLOB rejected with *"use the deposit wallet flow"*.

Note: we can sidestep the unknown CREATE2 derivation entirely — the deposit-wallet address is readable from the proxy's `owner()` reverse-link / the user's Polymarket profile (already done). Still worth confirming the delegate/impl `0xe6Cae8…555B` vs beacon/impl (`0x7A18ED…`/`0x58CA52…`) before locking a golden vector.

### Deposit-wallet address derivation (PARTIAL / open)

The deposit wallet is derived via CREATE2 from `depositWalletFactory` (beacon proxy). The beta bundle is minified; the **exact init-code hash and salt were not extracted** (facet open question). Two derivation paths `it()`/`st()` (beacon vs direct) exist in `chunk-2ZZDFOKL.js`. **This is an implementation blocker** — see §6.

---

## 3. Signing — ERC-7739 nested `TypedDataSign` + ERC-1271 (sigType 3)

CONFIRMED from `exchangeOrderBuilderV2.ts` (TS, viem-based) and `exchange_order_builder_v2.py` (Python, hand-rolled — the explicit reference for the digest viem hides). Both produce **byte-identical** signatures, which cross-validates the digest semantics.

This path is taken **only** when `message.signatureType == POLY_1271 (3)` (`ts:154`, `py:154`). Non-1271 types (0/1/2) sign the plain Order typed data with no wrapping.

### Constants (CONFIRMED)

- `SignatureTypeV2.POLY_1271 = 3` (`signatureTypeV2.ts:20`).
- `ORDER_TYPE_STRING` (`exchangeOrderBuilderV2.ts:17-18`, `py:26-30`):
  ```
  Order(uint256 salt,address maker,address signer,uint256 tokenId,uint256 makerAmount,uint256 takerAmount,uint8 side,uint8 signatureType,uint256 timestamp,bytes32 metadata,bytes32 builder)
  ```
- `ORDER_TYPE_HASH = keccak256(ORDER_TYPE_STRING)`.
- `SOLADY_TYPE_STRING` (`py:31-35`):
  ```
  TypedDataSign(Order contents,string name,string version,uint256 chainId,address verifyingContract,bytes32 salt)
  ```
  with `ORDER_TYPE_STRING` **appended** (Solady requires the nested struct type to follow the outer struct). `SOLADY_TYPE_HASH = keccak256(SOLADY_TYPE_STRING ‖ ORDER_TYPE_STRING)` (`py:42`).
- `DOMAIN_TYPE_HASH = keccak256("EIP712Domain(string name,string version,uint256 chainId,address verifyingContract)")`.
- Deposit-wallet sub-domain constants (`py:43-47`, TS inline `ts:212-216`): `name = "DepositWallet"`, `version = "1"`, `salt = bytes32(0)`, `verifyingContract = order.signer` (the deposit-wallet contract).
- Exchange (app) domain: `name = "Polymarket CTF Exchange"`, `version = "2"` (V2) or `"3"` (V3), `chainId`, `verifyingContract = exchange contract address`.

### Algorithm (step by step)

**STEP 1 — `appDomainSep`** (the **Exchange's** EIP-712 domain separator; computed once, `ts:47-65`, `py:73-84`):
```
appDomainSep = keccak256( abi.encode(
    DOMAIN_TYPE_HASH,                          // bytes32
    keccak256("Polymarket CTF Exchange"),      // bytes32
    keccak256(domainVersion "2" or "3"),       // bytes32
    chainId,                                    // uint256
    exchangeContractAddress                     // address
))
```
Note: 7739 folds in the **verifying contract's own** (Exchange) domain separator here — NOT the DepositWallet domain.

**STEP 2 — `contentsHash`** = struct hash of the Order ("contents") (`ts:164-195`, `py:163-194`):
```
contentsHash = keccak256( abi.encode(
    ORDER_TYPE_HASH,   // bytes32
    salt,              // uint256
    maker,             // address
    signer,            // address
    tokenId,           // uint256
    makerAmount,       // uint256
    takerAmount,       // uint256
    side,              // uint8   (BUY=0, SELL=1)
    signatureType,     // uint8   (=3)
    timestamp,         // uint256
    metadata,          // bytes32
    builder            // bytes32
))
```
Field order is exactly `CTF_EXCHANGE_V2_ORDER_STRUCT`. `side` encoding BUY=0/SELL=1 is set in `buildOrderTypedData` (`ts:141`).

**STEP 3 — `typedDataSignStructHash`** (`py:195-216` explicit; TS delegates to viem `primaryType "TypedDataSign"`):
```
typedDataSignStructHash = keccak256( abi.encode(
    SOLADY_TYPE_HASH,             // bytes32
    contentsHash,                 // bytes32
    keccak256("DepositWallet"),   // bytes32  (DEPOSIT_WALLET_NAME_HASH)
    keccak256("1"),               // bytes32  (DEPOSIT_WALLET_VERSION_HASH)
    chainId,                      // uint256
    order.signer,                 // address  (deposit-wallet contract)
    bytes32(0)                    // bytes32  (DEPOSIT_WALLET_DOMAIN_SALT)
))
```
The deposit wallet's own domain is folded in as **plain fields of the TypedDataSign struct**, not as a separate EIP-712 domain.

**STEP 4 — final digest the EOA signs** (`py:217-221`):
```
digest   = keccak256( 0x1901 ‖ appDomainSep ‖ typedDataSignStructHash )
innerSig = ecdsa_sign(digest, EOA_privkey)   // 65 bytes = r ‖ s ‖ v
```

**STEP 5 — assemble the on-wire `order.signature`** (`ts:220-223`, `py:227-237`):
```
signature = 0x
    ‖ innerSig        (65 bytes)
    ‖ appDomainSep    (32 bytes)
    ‖ contentsHash    (32 bytes)
    ‖ contentsType    (UTF-8 bytes of ORDER_TYPE_STRING — NOT the appended TypedDataSign part)
    ‖ uint16_BE( len(ORDER_TYPE_STRING) )   (2 bytes, big-endian)
```
TS: `lenHex = length.toString(16).padStart(4,'0')`. Python: `len.to_bytes(2,'big')`. This trailer is the ERC-7739 "contents type" the verifier reuses to reconstruct and validate via `isValidSignature`.

### ✅ RESOLVED — `ORDER_TYPE_STRING` length / trailer

The two signing facets disagreed (261/`0x0105` vs 248/`0x00f8`); **both were wrong**. Measured directly from the acquired source (`gh-py-clob-client-v2/.../exchange_order_builder_v2.py:26-29`, the 3-line concatenation quoted in §3), the canonical `ORDER_TYPE_STRING` is **exactly 186 bytes → trailer `0x00ba`** (all-ASCII, so `len(str) == byte len`). Python computes `len(ORDER_TYPE_STRING).to_bytes(2,"big")` at runtime (`py:228`).

Guidance for Go: still **compute the length at runtime** from the canonical bytes (don't hardcode `0x00ba`), and pin it with a golden vector — but the expected value is now known: `0x00ba`.

### EOA-equality skip (CONFIRMED)

For `POLY_1271`, the builder skips the `signer === getSignerAddress()` check (`ts:96-102`, `py:100-104`) because `order.signer` is the contract, not the EOA.

---

## 4. Submission

### Endpoint (CONFIRMED)

- **`POST /order`** at `clob.polymarket.com` for single orders (`POST_ORDER = "/order"`, `endpoints.ts:38`). `POST /orders` for batch (`endpoints.ts:39`). The same endpoint is used for **all** signature types including `POLY_1271` (`client.ts:1125-1216`). There is **no relayer branch** for order submission (grep for relayer/depositWallet in `gh-clob-client-v2/src` returns nothing).
- Auth: **L2 HMAC headers**; `POLY_ADDRESS` header = the **EOA** address (not the wallet) (`headers/index.ts:35`).

### Request body shape (CONFIRMED, `orderToJsonV2`, `types/ordersV2.ts:5-34`)
```jsonc
{
  "deferExec": <bool>,
  "postOnly": <bool>,
  "owner": "<creds.key>",
  "orderType": "<GTC|FOK|...>",
  "order": {
    "salt": "...",
    "maker": "<deposit-wallet addr>",
    "signer": "<deposit-wallet addr>",   // == maker for POLY_1271
    "taker": "...",
    "tokenId": "...",
    "makerAmount": "...",
    "takerAmount": "...",
    "side": "BUY|SELL",
    "signatureType": 3,                  // POLY_1271
    "timestamp": "...",
    "expiration": "...",
    "metadata": "0x...32",
    "builder": "0x...32",
    "signature": "0x<packed 7739 blob from §3 STEP 5>"
  }
}
```

### Order-domain verifying contract (CONFIRMED, `createOrder.ts:36-56`, `config.ts`, chainId 137)

The EIP-712 verifying contract in the order domain is the **CTF Exchange**, NOT the deposit wallet:

| Version | Selector | Address |
|---|---|---|
| V2 (standard) | `exchangeV2` | `0xE111180000d2663C0091e4f400237545B87B996B` |
| V2 (neg-risk) | `negRiskExchangeV2` | `0xe2222d279d744050d28e00520010520000310F59` |
| V3 | `exchangeV3` | `0xe3333700cA9d93003F00f0F71f8515005F6c00Aa` |
| Legacy V1 (rejects POLY_1271) | `exchange` | `0x4bFb41d5B3570DeFd03C39a9A4D8dE6Bd8B8982E` |
| Legacy V1 neg-risk | `negRiskExchange` | `0xC5d563A36AE78145C45a50134d48A1215220f80a` |

Selection: `version 2 → negRisk ? negRiskExchangeV2 : exchangeV2`; `version 3 → exchangeV3`. POLY_1271 is **rejected on v1** (`createOrder.ts:39-41`).

> **Open / server-driven:** `getVersion()` defaults to 2, and the server can force a version-mismatch retry. Which version (v2 vs v3) is currently live for deposit-wallet orders is **not fixed in source**. The Go client must handle a version-mismatch response and retry with the server-indicated version.

### Relayer-v2 (SEPARATE service — deploy + gasless approvals only, NOT orders) (CONFIRMED)

- Base: `https://relayer-v2.polymarket.com` (preprod `relayer-v2-preprod-int.polymarket.com`).
- `POST /submit` with `RelayerExecuteRequest`. Deposit-wallet variant (`bindings relayer/index.d.ts:73-90`):
  ```jsonc
  { "type": "WALLET", "from": "...", "to": "...", "nonce": "...", "signature": "...",
    "metadata": "...?",
    "depositWalletParams": { "calls": [{"target":"...","data":"...","value":"..."}],
                             "deadline": "...", "depositWallet": "..." } }
  ```
- `WALLET-CREATE` variant (`type:"WALLET-CREATE", from, to, metadata`) deploys the wallet (`index.d.ts:91-97`).
- `GET /deployed?address=&type=` → `{deployed: bool}` (`index.d.ts:164-167`).
- `GET /v1/account/transactions/params?address=&type=` → `{address, nonce}`.

> **Open:** whether the CLOB `/order` endpoint accepts a `POLY_1271` order before the wallet is deployed, or requires `WALLET-CREATE` first. The `/deployed` checks **imply** deploy-then-trade ordering but it is **not proven** from the order endpoint.

---

## 5. Approvals / setup

CONFIRMED from beta `@polymarket/client` `src/actions/approvals.ts` + `src/abis.ts` (recovered via source maps) and the `production` env in `index.js`. The legacy `clob-client-v2` does **not** contain this orchestration.

### Required ERC-20 approvals (`approve(spender, MAX_UINT256)`)
`token = collateralToken = 0xC011a7E12a19f7B1f670d46F03B03f3342E82DFB` (USDC.e legacy address), `amount = (1<<256)-1`, for spenders:
1. `standardExchange` `0xE111180000d2663C0091e4f400237545B87B996B`
2. `negRiskExchange` `0xe2222d279d744050d28e00520010520000310F59`
3. `negRiskAdapter` `0xd91E80cF2E7be2e162c6513ceD06f1dD0dA35296`
4. `collateralAdapter` `0xAdA100Db00Ca00073811820692005400218FcE1f`
5. `negRiskCollateralAdapter` `0xadA2005600Dec949baf300f4C6120000bDB6eAab`
6. `protocolV2Router` `0x12121212006e4CD160D18e3f00711DA5c3372600`
7. `exchangeV3` `0xe3333700cA9d93003F00f0F71f8515005F6c00Aa`

### Required ERC-1155 `setApprovalForAll(operator, true)` — pairs `(token, operator)`
- `conditionalTokens` `0x4D97DCd97eC945f40cF65F87097ACe5EA0476045`:
  1. `standardExchange`
  2. `negRiskExchange`
  3. `negRiskAdapter`
  4. `collateralAdapter`
  5. `negRiskCollateralAdapter`
  6. `autoRedeemOperator` `0xa1200000d0002264C9a1698e001292D00E1b00af`
- `positionManager` `0x006F54F7f9A22e0000CC2AB60031000000ae9fEF`:
  7. `protocolV2Router`
  8. `exchangeV3`
  9. `autoRedeemOperator`

### Precheck (skip already-set)
`resolveMissingTradingApprovals()` batches `allowance(owner, spender)` and `isApprovedForAll(owner, operator)` via `rpc.ethCallBatch`. Include an ERC-20 only if `allowance < MAX_UINT256`; an ERC-1155 only if `!isApprovedForAll`. **`owner = the deposit-wallet address`, not the EOA.**

### Routing (CONFIRMED, `prepareTradingApprovals`)
- `walletType === EOA` → send each `approve`/`setApprovalForAll` tx directly on-chain, sequentially, awaiting each (EOA pays gas).
- **else (deposit wallet / Safe / proxy)** → bundle ALL calls into **one** `prepareGaslessTransaction({ calls:[...erc20, ...erc1155], metadata:"Trading setup approvals" })` and submit through the **relayer-v2** (gasless). This is the deposit-wallet path: approvals are executed by the wallet via the relayer, in one bundled gasless tx, signed with the same 7739/1271 scheme.

---

## 6. Confidence & open questions

### Overall confidence: **MEDIUM-HIGH**
The signing/submission/approval mechanics are CONFIRMED from two independent SDK implementations that produce identical signatures. Confidence is held below "high" because (a) the on-chain verifier is absent from all source, (b) the two facets disagree on the `contentsType` length trailer, and (c) the deposit-wallet address derivation was not fully decoded.

### CONFIRMED from source
- `POLY_1271 = 3`; this is a V2+ addition only (no 1271 in legacy order-utils).
- Order struct (11 fields) and `ORDER_TYPE_STRING` field set/order.
- ERC-7739 nested `TypedDataSign` signing: the 5-step algorithm, including `appDomainSep = the Exchange domain separator` and the DepositWallet sub-domain folded as plain TypedDataSign fields with `verifyingContract = order.signer`, `salt = 0`.
- Final packed signature layout: `innerSig(65) ‖ appDomainSep(32) ‖ contentsHash(32) ‖ contentsType ‖ uint16_BE(len)`.
- For POLY_1271: `maker == signer == deposit-wallet contract address`; EOA-equality check skipped; EOA carried in auth header only.
- Orders go to CLOB `POST /order`, never the relayer; verifying contract = CTF Exchange (V2/V3 addresses above), not the wallet.
- Relayer-v2 endpoints + `WALLET`/`WALLET-CREATE` shapes for deploy + gasless approvals.
- Full approval spender/operator lists and contract addresses; precheck batching; EOA-vs-relayer routing.
- Factory/beacon/implementation addresses (`...Cc07` / `0x7A18ED…` / `0x58CA52…`).
- TS and Python produce byte-identical signatures (cross-validation).

### INFERRED (structurally consistent, not labeled in source)
- The scheme is "ERC-7739 / Solady TypedDataSign" — the literal strings "7739"/"ERC-7739" do **not** appear in source; identified by structure.
- The packed blob is the EIP-1271-compatible signature the wallet's `isValidSignature` is expected to validate (the contract recomputes the digest from `appDomainSep`, `contentsHash`, `contentsType` + its own domain and `ecrecover`s `innerSig`).
- "Deploy-then-trade" ordering (implied by `/deployed` checks, not proven).

### UNVERIFIED (prompt-supplied, not in source)
- The "lastsaga" example addresses (`0xf6C73cB2…`, `0x4b2f…`, `0xB8bd…`, `0xe6Cae8…555B`). Only the factory `…Cc07` cross-checks. **Must be confirmed on-chain before use.**

### UNKNOWNS that still BLOCK implementation
1. **`contentsType` length trailer conflict** (`0x0105` vs `0x00f8`). Load-bearing. Compute at runtime; resolve via a golden vector. **(blocker for signature correctness)**
2. **On-chain verifier** (`isValidSignature`, magic `0x1626ba7e`, Solady ERC-1271 impl) is NOT in any acquired source. Needed to confirm exact reconstruction & the trailer encoding the contract reads. **Source from on-chain bytecode / contracts repo.** **(blocker for verification confidence)**
3. **Deposit-wallet address derivation** — exact CREATE2 init-code hash + salt and the beacon-vs-direct (`it()`/`st()`) choice were not decoded from the minified bundle. **(blocker if Go must derive the wallet address itself)**
4. **TS↔Python digest equivalence proof** — both are claimed identical, but the exact equivalence (TS passes the DepositWallet domain to viem while Python signs the Exchange-separated digest) should be proven against a known-good vector. **(verify, not necessarily a blocker)**
5. **Collateral token post-migration** — beta source still uses USDC.e `0xC011…`; whether this is re-pointed to pUSD / Polymarket USD, or pUSD needs separate approvals, is unknown. (See MEMORY: V2 Exchange Migration, USDC.e→Polymarket USD.) **(blocker for approvals correctness post-migration)**
6. **Live exchange version** — v2 vs v3 for deposit-wallet orders is server-driven; client must handle version-mismatch retry.
7. **Order acceptance precondition** — whether `/order` requires the wallet already deployed, and whether any extra `deferExec`/gasless flag is needed for deposit-wallet orders.
8. **Exact `prepareGaslessTransaction` → relayer `/submit` mapping** for the bundled approval tx (which fields, how the 7739 sig is attached, gas sponsorship) — not fully verified.

---

## 7. Suggested golden-vector tests for the Go signer

Build these as table-driven tests; mock all network. The first priority is a **known-good signature vector** to resolve the trailer-length conflict (§3) and the TS/Python equivalence (§6).

1. **`ORDER_TYPE_STRING` canonical bytes** — assert the exact string and that `len()` matches whatever the on-chain verifier expects. Emit both candidate trailers (`0x0105`, `0x00f8`) and FAIL loudly until reconciled with a real vector. (Resolves blocker #1.)
2. **`ORDER_TYPE_HASH` / `DOMAIN_TYPE_HASH` / `SOLADY_TYPE_HASH`** — assert each keccak256 against hashes captured from running the TS/Python SDK on the same inputs.
3. **`appDomainSep`** — given a fixed `(chainId=137, version "2", exchangeV2 addr)`, assert the exchange domain separator equals the SDK output. Repeat for version "3" / exchangeV3 and for negRisk.
4. **`contentsHash`** — for a fixed Order (with `side=BUY=0` and `side=SELL=1` cases), assert the struct hash matches the SDK.
5. **`typedDataSignStructHash`** — assert with `verifyingContract = order.signer`, `salt = 0`, DepositWallet name/version hashes.
6. **Final digest** — assert `keccak256(0x1901 ‖ appDomainSep ‖ typedDataSignStructHash)` matches the Python `py:217-221` value for a fixed input.
7. **End-to-end packed signature** — using a fixed test private key, deterministically produce `innerSig` and assert the full blob byte-for-byte equals the SDK output (this is the master vector; covers ordering, lengths, low-s/v normalization).
8. **`innerSig` v/low-s normalization** — confirm the Go ECDSA output (`r ‖ s ‖ v`, v ∈ {27,28} or {0,1}) matches the SDK convention; add a vector that would differ under the wrong convention.
9. **EOA-equality skip** — assert the builder does NOT reject when `order.signer != EOA` for `POLY_1271`, but DOES reject for types 0/1/2.
10. **maker == signer invariant** — assert that for `POLY_1271` both fields equal the deposit-wallet address.
11. **Request JSON shape** — assert `orderToJsonV2` output matches the CONFIRMED shape in §4, with `signatureType: 3` inside `order`.
12. **Approvals diff** — given mocked `allowance`/`isApprovedForAll` responses, assert exactly the missing approvals are emitted, with `owner = deposit-wallet`, and that the deposit-wallet path bundles them into one gasless call.
13. **(Once verifier is sourced)** Negative test: a blob with the wrong trailer length / wrong `contentsType` must be rejected by a local re-implementation of `isValidSignature`.

> The single most valuable artifact is **one real, known-good `(order, signature)` pair** produced by the TS or Python SDK with a fixed key. Capture it before writing the Go signer; it collapses blockers #1 and #4 immediately.
