# Polymarket Position Redemption (Claim All)

This document describes the position redemption implementation for the Polymarket Telegram bot, including the Builder Relayer integration, Gnosis Safe transaction signing, MultiSend batching, and the end-to-end claim flow.

## Table of Contents

1. [Overview](#overview)
2. [Background: How Polymarket Redemption Works](#background-how-polymarket-redemption-works)
3. [Architecture](#architecture)
4. [Builder Relayer Integration](#builder-relayer-integration)
5. [Safe Transaction Signing](#safe-transaction-signing)
6. [MultiSend Batching](#multisend-batching)
7. [Redemption Paths](#redemption-paths)
8. [User Flow](#user-flow)
9. [Code Structure](#code-structure)
10. [Configuration](#configuration)
11. [Data Types](#data-types)
12. [Error Handling](#error-handling)
13. [Security Considerations](#security-considerations)
14. [Troubleshooting](#troubleshooting)

---

## Overview

After a Polymarket market resolves, users with winning positions can redeem them to receive USDC. The `/redeem` command (or "🎁 Redeem" button from `/positions`) fetches all redeemable positions and claims them in a single atomic transaction via Polymarket's Builder Relayer.

### Key Features

- **Gasless redemption** — the relayer pays on-chain gas, no MATIC needed
- **Single transaction** — MultiSend batches all redemptions into one atomic tx
- **Both market types** — supports standard (binary) and negative-risk (multi-outcome) markets
- **Non-custodial** — the user's EOA only signs a message, never sends a transaction

---

## Background: How Polymarket Redemption Works

### On-Chain Mechanism

Polymarket positions are ERC-1155 tokens issued by the **Conditional Tokens Framework (CTF)** contract on Polygon. When a market resolves:

1. The UMA oracle reports the outcome via `reportPayouts(questionId, payouts)`
2. The CTF contract stores the payout vector (e.g., `[1, 0]` if YES wins)
3. Positions become redeemable — users can burn their tokens to receive USDC

### Two Redemption Paths

| Market Type | Contract | Address | Function |
|---|---|---|---|
| **Standard (binary)** | ConditionalTokens (CTF) | `0x4D97DCd97eC945f40cF65F87097ACe5EA0476045` | `redeemPositions(collateral, parentId, conditionId, indexSets)` |
| **Negative risk (multi-outcome)** | NegRiskAdapter | `0xd91E80cF2E7be2e162c6513ceD06f1dD0dA35296` | `redeemPositions(conditionId, amounts)` |

**Standard markets**: Burns the caller's *entire* token balance for a condition. `indexSets = [1, 2]` covers both YES and NO outcomes. Winning tokens pay $1 USDC each; losing tokens pay $0.

**Negative risk markets**: Takes explicit `[yesAmount, noAmount]` parameters. Uses wrapped collateral internally and unwraps to USDC. Requires CTF `setApprovalForAll(negRiskAdapter, true)` beforehand.

### The Proxy Wallet Complication

Users don't hold positions directly — a **Gnosis Safe (1-of-1 multisig)** proxy wallet holds the assets. The redemption call must execute *through* the Safe, not from the EOA directly.

Polymarket's web UI handles this via a **relayer service**: the user signs a message (Safe transaction hash) in MetaMask, and the relayer submits the actual on-chain `execTransaction` call on the Safe.

---

## Architecture

```
User taps "Claim All" in Telegram
  │
  ▼
Bot fetches redeemable positions
  │  GET data-api.polymarket.com/positions?user={proxy}&redeemable=true
  │
  ▼
Bot encodes redemption calldata per conditionID
  │  Standard: CTF.redeemPositions(USDC, 0x0, conditionId, [1,2])
  │  NegRisk:  NegRiskAdapter.redeemPositions(conditionId, [yesAmt, noAmt])
  │
  ▼
Bot packs into MultiSend (if multiple positions)
  │  multiSend(packed_sub_transactions) via DelegateCall
  │
  ▼
Bot computes EIP-712 SafeTx hash
  │  Domain: {chainId: 137, verifyingContract: safeAddress}
  │  Type: SafeTx(to, value, data, operation, ...)
  │
  ▼
Bot signs with personal_sign + adjusts v (+4)
  │  EOA private key → 65-byte signature (v=31 or 32)
  │
  ▼
Bot submits to Polymarket Builder Relayer
  │  POST relayer-v2.polymarket.com/submit
  │  Auth: HMAC-SHA256 Builder headers
  │
  ▼
Relayer calls execTransaction on Gnosis Safe
  │  On-chain tx, relayer pays gas
  │
  ▼
Bot polls for confirmation
  │  GET /transaction?id={id}
  │  STATE_NEW → STATE_MINED → STATE_CONFIRMED
  │
  ▼
Bot shows result with Polygonscan link
```

---

## Builder Relayer Integration

### Why a Relayer?

Direct on-chain `execTransaction` calls from the EOA don't work reliably on Polygon — transactions can get stuck in the mempool. Polymarket's web UI uses a relayer where the user only signs a message and the relayer handles on-chain execution and gas.

### Relayer API

**Base URL**: `https://relayer-v2.polymarket.com`

| Method | Endpoint | Auth | Purpose |
|---|---|---|---|
| `GET` | `/nonce?address={eoa}&type=SAFE` | No | Get Safe nonce for signing |
| `POST` | `/submit` | Yes (Builder HMAC) | Submit signed Safe transaction |
| `GET` | `/transaction?id={id}` | No | Poll transaction status |

### Authentication (Builder HMAC)

The relayer requires **Builder credentials** (separate from CLOB API credentials). These identify the bot as an authorized application.

**Headers:**
```
POLY_BUILDER_API_KEY       = Builder API key (UUID)
POLY_BUILDER_PASSPHRASE    = Builder passphrase
POLY_BUILDER_TIMESTAMP     = Unix timestamp (seconds)
POLY_BUILDER_SIGNATURE     = HMAC-SHA256 signature (URL-safe base64)
```

**Signature computation:**
```
message   = timestamp + method + requestPath + body
secret    = base64Decode(builderSecret)
signature = base64URLEncode(HMAC-SHA256(secret, message))
```

This is the same algorithm used for CLOB L2 authentication (`signL2Request` in `trading.go`), just with different header names and separate credentials.

### Submit Request Body

```json
{
  "type": "SAFE",
  "from": "0x<EOA_ADDRESS>",
  "to": "0x<TARGET_CONTRACT>",
  "proxyWallet": "0x<SAFE_ADDRESS>",
  "data": "0x<CALLDATA>",
  "nonce": "42",
  "signature": "0x<65_BYTE_SIGNATURE>",
  "signatureParams": {
    "gasPrice": "0",
    "operation": "0",
    "safeTxnGas": "0",
    "baseGas": "0",
    "gasToken": "0x0000000000000000000000000000000000000000",
    "refundReceiver": "0x0000000000000000000000000000000000000000"
  },
  "metadata": "redeem positions"
}
```

For MultiSend: `operation` is `"1"` (DelegateCall) and `to` is the MultiSend contract.

### Transaction States

```
STATE_NEW → STATE_EXECUTED → STATE_MINED → STATE_CONFIRMED (success)
                                         → STATE_FAILED    (failure)
```

The bot polls every 3 seconds with a 3-minute timeout.

---

## Safe Transaction Signing

The signing flow differs from standard Ethereum transaction signing:

### Step 1: Compute EIP-712 SafeTx Hash

```
Domain:     { chainId: 137, verifyingContract: safeAddress }
Type:       SafeTx(address to, uint256 value, bytes data, uint8 operation,
                   uint256 safeTxGas, uint256 baseGas, uint256 gasPrice,
                   address gasToken, address refundReceiver, uint256 nonce)
```

All gas-related fields are zero (relayer handles gas). `operation` is 0 for direct calls, 1 for DelegateCall (MultiSend).

### Step 2: personal_sign

The EIP-712 hash is wrapped with EIP-191 personal message prefix before signing:

```
messageToSign = keccak256("\x19Ethereum Signed Message:\n32" + safeTxHash)
signature     = ecSign(messageToSign, privateKey)
```

### Step 3: Adjust v-value

Gnosis Safe uses a modified `v` to distinguish `eth_sign` (personal_sign) from raw `ecrecover`:

- go-ethereum produces `v = 0 or 1`
- Add 31 → `v = 31 or 32`

This tells the Safe contract that the signature used `eth_sign` convention.

### Step 4: Pack as 65 bytes

```
signature = r[32 bytes] + s[32 bytes] + v[1 byte]
```

---

## MultiSend Batching

When multiple positions need redemption, they are packed into a single `multiSend(bytes)` call on the Gnosis Safe MultiSend contract (`0xA238CBeb142c10Ef7Ad8442C6D1f9E89e07e7761`).

### Sub-transaction Encoding

Each sub-transaction is tightly packed (no ABI padding):

```
uint8    operation   (1 byte)  — always 0 (Call)
address  to          (20 bytes) — target contract
uint256  value       (32 bytes) — always 0
uint256  dataLength  (32 bytes) — length of calldata
bytes    data        (variable) — the calldata
```

All sub-transactions are concatenated, then wrapped in:

```solidity
multiSend(bytes transactions)
```

The Safe executes this via `DelegateCall` (operation=1), so all sub-calls execute in the context of the Safe — atomically, in one on-chain transaction.

### Optimization

- **1 position**: Direct call (no MultiSend overhead)
- **2+ positions**: MultiSend batch

If NegRisk approval is needed, it's included as the first sub-transaction in the batch.

---

## Redemption Paths

### Standard (Binary) Markets

```solidity
// Target: CTF (0x4D97DCd97eC945f40cF65F87097ACe5EA0476045)
redeemPositions(
    address collateralToken,      // USDC: 0x2791Bca1f2de4661ED88A30C99A7a9449Aa84174
    bytes32 parentCollectionId,   // 0x0000...0000
    bytes32 conditionId,          // from position data
    uint256[] indexSets           // [1, 2] for binary markets
)
```

- `indexSets [1, 2]` covers both outcomes: YES (0b01) and NO (0b10)
- Burns the caller's entire balance for that condition
- Winning tokens pay $1 USDC each; losing tokens pay $0

### Negative Risk (Multi-Outcome) Markets

```solidity
// Target: NegRiskAdapter (0xd91E80cF2E7be2e162c6513ceD06f1dD0dA35296)
redeemPositions(
    bytes32 conditionId,
    uint256[] amounts             // [yesBalance, noBalance] from on-chain query
)
```

- Takes explicit amounts (queried from on-chain ERC-1155 balances)
- Requires prior CTF approval: `setApprovalForAll(negRiskAdapter, true)`
- Uses wrapped collateral internally, unwraps to USDC

---

## User Flow

### Entry Points

1. **`/redeem` command** — direct entry
2. **"🎁 Redeem" button** — from `/positions` view

### Flow

```
/redeem
  │
  ▼
"🎁 Checking redeemable positions..."
  │  Fetches from Data API (?redeemable=true)
  │  Filters: Size > 0 AND CurPrice > 0 (winning positions only)
  │
  ▼
Summary with "✅ Claim All" button:
  │  🎁 Redeemable Positions (3)
  │
  │  1. Market A — 50.0 YES
  │     Est. payout: ~$50.00
  │  2. Market B — 25.0 No
  │     Est. payout: ~$25.00
  │  ...
  │  Total est. payout: ~$75.00 USDC
  │
  │  [✅ Claim All]  [❌ Cancel]
  │
  ▼ (user taps Claim All)
  │
"⏳ Claiming 3 positions..."
  │  Encode → MultiSend → Sign → Submit → Poll
  │
  ▼
"✅ All Positions Claimed!"
  │  Claimed: 3/3 positions
  │  Est. payout: ~$75.00 USDC
  │  Tx: 0xabc... (Polygonscan link)
```

---

## Code Structure

### Files

| File | Purpose |
|---|---|
| `internal/blockchain/redeem.go` | Calldata encoding: `EncodeStandardRedemption`, `EncodeNegRiskRedemption`, `EncodeSetApprovalForAll`, `EncodeMultiSend` |
| `internal/blockchain/redeem_test.go` | Encoding tests (7 tests) |
| `internal/polymarket/relayer.go` | Relayer client: HMAC auth, EIP-712 hashing, Safe signing, submit, poll, MultiSend execution |
| `internal/polymarket/relayer_test.go` | Relayer tests: hash computation, signing, HMAC, HTTP mocks (7 tests) |
| `internal/polymarket/positions.go` | `RedeemablePositionInfo` type, `GetRedeemablePositions()` |
| `internal/polymarket/positions_test.go` | Position fetching tests (7 tests) |
| `internal/telegram/handlers_redeem.go` | Telegram UI: `/redeem` command, `handleRedeemPositions`, `handleRedeemAll` |
| `internal/telegram/state.go` | `StateRedeemingPositions` constant |

### Key Functions

**Calldata encoding** (`blockchain/redeem.go`):
- `EncodeStandardRedemption(conditionID) → (target, calldata, err)` — CTF redeemPositions
- `EncodeNegRiskRedemption(conditionID, amounts) → (target, calldata, err)` — NegRiskAdapter
- `EncodeSetApprovalForAll(operator, approved) → (calldata, err)` — CTF approval
- `EncodeMultiSend(txs) → (calldata, err)` — pack sub-txs for Safe MultiSend

**Relayer client** (`polymarket/relayer.go`):
- `ExecSafeTransaction(ctx, eoa, safe, to, data, key) → (txHash, err)` — single call
- `ExecMultiSendTransaction(ctx, eoa, safe, txs, key) → (txHash, err)` — batched calls
- `GetSafeNonce(ctx, eoa) → (nonce, err)` — from relayer API
- `SubmitSafeTransaction(ctx, req) → (resp, err)` — POST to relayer
- `WaitForConfirmation(ctx, txID, timeout) → (status, err)` — poll until terminal state

**Internal helpers** (`polymarket/relayer.go`):
- `computeSafeTxHash(chainID, safe, to, data, operation, nonce) → hash` — EIP-712
- `personalSignHash(hash) → hash` — EIP-191 prefix
- `signSafeTransaction(hash, key) → (signature, err)` — personal_sign + v adjustment
- `signBuilderRequest(timestamp, method, path, body) → signature` — HMAC-SHA256

---

## Configuration

### Required Environment Variables

```env
# Builder Relayer credentials (from polymarket.com/settings?tab=builder)
POLYMARKET_BUILDER_API_KEY=...
POLYMARKET_BUILDER_SECRET=...
POLYMARKET_BUILDER_PASSPHRASE=...

# Optional: custom relayer URL (default: https://relayer-v2.polymarket.com)
POLYMARKET_BUILDER_RELAYER_URL=...
```

These are **separate from** the CLOB API credentials used for trading. Builder credentials identify the bot as an authorized application to the relayer.

If Builder credentials are not configured, the bot starts normally but `/redeem` shows an error message.

### Existing Config (already required)

```env
POLYGON_RPC_URL=...          # Needed for neg-risk on-chain balance queries
ENCRYPTION_KEY=...           # Needed to decrypt user's private key for signing
```

---

## Data Types

### RedeemablePositionInfo

```go
type RedeemablePositionInfo struct {
    Title         string   // Market title
    Outcome       string   // "Yes" or "No"
    ConditionID   string   // Market condition ID (hex)
    Asset         string   // Token ID (YES or NO side)
    OppositeAsset string   // Complementary token ID
    Size          float64  // Number of shares
    NegativeRisk  bool     // Market type flag
    CurPrice      float64  // 1.0 for winners, 0.0 for losers
    EstPayout     float64  // Size * CurPrice
}
```

### MultiSendTx

```go
type MultiSendTx struct {
    To   common.Address  // Target contract
    Data []byte          // Encoded calldata
}
```

---

## Error Handling

| Scenario | Behavior |
|---|---|
| No Builder credentials | Bot starts, `/redeem` shows "Builder Relayer not configured" |
| No redeemable positions | Shows "No redeemable positions found" |
| Relayer nonce fetch fails | Shows error, user can retry |
| Relayer rejects submission | Shows relayer error message |
| Transaction fails on-chain | Shows `STATE_FAILED` with error |
| Confirmation timeout (3 min) | Shows timeout error |
| NegRisk approval needed | Included as first sub-tx in MultiSend batch |
| Blockchain client unavailable | Standard redemptions still work; neg-risk skipped (needs on-chain balance query) |
| Partial encoding failure | Skips failed positions, redeems the rest |

---

## Security Considerations

1. **Private keys** are never sent to the relayer — only the EIP-712 signature is transmitted
2. **Builder credentials** authenticate the application, not the user — they cannot move user funds
3. **Each redemption requires** the user's EOA signature on the specific Safe transaction hash
4. **Nonce management** is handled by the relayer — prevents replay attacks
5. **Keys are decrypted in memory** only during signing, then discarded (same pattern as trading)

---

## Troubleshooting

### "Builder Relayer not configured"

Missing `POLYMARKET_BUILDER_*` environment variables. Generate Builder credentials at `polymarket.com/settings?tab=builder`.

### "relayer nonce: status 401"

Builder credentials are invalid or expired. Regenerate from Polymarket settings.

### "relayer submit: status 400"

The transaction data may be malformed, or the market may not be resolved on-chain yet (Data API can be ahead of on-chain state).

### "transaction STATE_FAILED"

The on-chain execution reverted. Common causes:
- Market not yet resolved on-chain
- Position already redeemed
- NegRiskAdapter not approved (should be auto-handled)

### No redeemable positions showing

- Check that positions exist on Polymarket web UI under "Portfolio → Resolved"
- The Data API `?redeemable=true` filter only returns positions that are claimable
- Losing positions (CurPrice=0) are filtered out from display (they get burned automatically)

---

## Contract Addresses (Polygon Mainnet)

| Contract | Address |
|---|---|
| ConditionalTokens (CTF) | `0x4D97DCd97eC945f40cF65F87097ACe5EA0476045` |
| NegRiskAdapter | `0xd91E80cF2E7be2e162c6513ceD06f1dD0dA35296` |
| USDC.e (Bridged USDC) | `0x2791Bca1f2de4661ED88A30C99A7a9449Aa84174` |
| Gnosis Safe MultiSend | `0xA238CBeb142c10Ef7Ad8442C6D1f9E89e07e7761` |
| NegRisk CTF Exchange | `0xC5d563A36AE78145C45a50134d48A1215220f80a` |
| CTF Exchange | `0x4bFb41d5B3570DeFd03C39a9A4D8dE6Bd8B8982E` |

## API Endpoints

| API | Base URL | Purpose |
|---|---|---|
| Data API | `https://data-api.polymarket.com` | Position queries (`?redeemable=true`) |
| Builder Relayer | `https://relayer-v2.polymarket.com` | Safe transaction submission |
| Polygon RPC | Configured via `POLYGON_RPC_URL` | On-chain balance queries (neg-risk) |
