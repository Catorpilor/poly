# Polymarket Trading Implementation

This document describes the trading implementation for the Polymarket Telegram bot, including API authentication, order signing, and execution flow.

## Table of Contents

1. [Overview](#overview)
2. [Architecture](#architecture)
3. [Authentication](#authentication)
4. [Order Flow](#order-flow)
5. [Code Structure](#code-structure)
6. [Data Types](#data-types)
7. [Token ID Resolution](#token-id-resolution)
8. [Order Types](#order-types)
9. [API Reference](#api-reference)
10. [Dependencies](#dependencies)
11. [Security Considerations](#security-considerations)
12. [Troubleshooting](#troubleshooting)

---

## Overview

The trading system enables users to buy and sell outcome tokens on Polymarket directly through Telegram. It integrates with:

- **Polymarket CLOB API** (`https://clob.polymarket.com`) - Order submission and management
- **Polymarket Gamma API** (`https://gamma-api.polymarket.com`) - Market data and token IDs
- **Polymarket Data API** (`https://data-api.polymarket.com`) - Position tracking

### Key Features

- Non-custodial trading (user controls private keys)
- EIP-712 order signing using Polymarket's official Go library
- Support for EOA and Proxy wallet signatures
- GTC (Good-til-cancelled) orders
- Automatic best price calculation with slippage protection

---

## Architecture

```
┌─────────────────┐     ┌──────────────────┐     ┌─────────────────┐
│  Telegram Bot   │────▶│  TradingClient   │────▶│  CLOB API       │
│  (handlers)     │     │  (trading.go)    │     │  (Polymarket)   │
└─────────────────┘     └──────────────────┘     └─────────────────┘
        │                       │                        │
        │                       ▼                        │
        │               ┌──────────────────┐             │
        │               │  go-order-utils  │             │
        │               │  (EIP-712 sign)  │             │
        │               └──────────────────┘             │
        │                       │                        │
        ▼                       ▼                        ▼
┌─────────────────┐     ┌──────────────────┐     ┌─────────────────┐
│  Wallet Manager │     │  Order Builder   │     │  CTF Exchange   │
│  (decrypt keys) │     │  (sign orders)   │     │  (on-chain)     │
└─────────────────┘     └──────────────────┘     └─────────────────┘
```

### Component Responsibilities

| Component | File | Responsibility |
|-----------|------|----------------|
| TradingClient | `internal/polymarket/trading.go` | API auth, order execution |
| Bot | `internal/telegram/bot.go` | User interaction, trade dispatch |
| WalletManager | `internal/wallet/wallet.go` | Key encryption/decryption |
| MarketClient | `internal/polymarket/markets.go` | Market data fetching |

---

## Authentication

Polymarket uses a **two-level authentication system**:

### L1 Authentication (Private Key)

Used for:
- Creating API credentials
- Deriving existing API credentials
- Signing orders

**EIP-712 Domain:**
```go
Domain: {
    Name:    "ClobAuthDomain"
    Version: "1"
    ChainId: 137 (Polygon mainnet)
}
```

**Message Structure:**
```go
ClobAuth: {
    address:   signer's wallet address
    timestamp: UNIX timestamp
    nonce:     default 0
    message:   "This message attests that I control the given wallet"
}
```

**L1 Headers:**
```
POLY_ADDRESS:   Your Polygon signer address
POLY_SIGNATURE: EIP-712 signature (0x prefixed hex)
POLY_TIMESTAMP: Current UNIX timestamp
POLY_NONCE:     Nonce value (default 0)
```

### L2 Authentication (API Credentials)

Used for:
- Posting signed orders
- Canceling orders
- Viewing positions

**Credentials (obtained from L1):**
- `apiKey`: UUID format
- `secret`: Base64-encoded secret
- `passphrase`: Random string

**L2 Headers:**
```
POLY_ADDRESS:    Your Polygon address
POLY_SIGNATURE:  HMAC-SHA256 signature
POLY_TIMESTAMP:  Current UNIX timestamp
POLY_API_KEY:    Your API key
POLY_PASSPHRASE: Your passphrase
```

**HMAC Signature Calculation:**
```go
message := timestamp + method + path + body
// Secret is base64 encoded (standard or URL-safe format)
secretBytes := base64Decode(secret)
// Output is URL-safe base64 with padding
signature := base64URLEncode(HMAC-SHA256(secretBytes, message))
```

**Important Notes:**
- The secret from Polymarket API may be standard or URL-safe base64
- Python's `base64.urlsafe_b64decode` accepts both formats
- The output signature should be URL-safe base64 with padding preserved

### API Credential Endpoints

```
POST /auth/api-key        - Create new credentials
GET  /auth/derive-api-key - Retrieve existing credentials
```

---

## Order Flow

### Complete Buy Order Flow

```
1. User clicks "Buy Yes $50" in Telegram
                    │
                    ▼
2. Bot validates user has wallet
   └─▶ userRepo.GetByTelegramID()
                    │
                    ▼
3. Fetch market details from Gamma API
   └─▶ marketClient.GetMarketByID()
                    │
                    ▼
4. Decrypt user's private key
   └─▶ walletManager.DecryptPrivateKey()
                    │
                    ▼
5. Get/create API credentials (L1 auth)
   └─▶ tradingClient.GetOrCreateAPICredentials()
       ├─▶ Try: deriveAPICredentials()
       └─▶ Fallback: createAPICredentials()
                    │
                    ▼
6. Get token ID for outcome (YES/NO)
   └─▶ tradingClient.GetTokenIDForOutcome()
       └─▶ Fetch clobTokenIds from Gamma API
                    │
                    ▼
7. Get best price from order book (optional)
   └─▶ tradingClient.GetBestPrice()
       └─▶ Fetch order book, calculate VWAP
                    │
                    ▼
8. Build order data
   └─▶ Calculate makerAmount, takerAmount
       Set side, expiration, feeRateBps
                    │
                    ▼
9. Sign order using go-order-utils
   └─▶ builder.BuildSignedOrder()
       ├─▶ BuildOrder() - create Order struct
       ├─▶ BuildOrderHash() - EIP-712 hash
       └─▶ BuildOrderSignature() - sign hash
                    │
                    ▼
10. Submit to CLOB API
    └─▶ POST /order with L2 headers
                    │
                    ▼
11. Return result to user
    └─▶ Show success/error message
```

### Order Amount Calculation

For **BUY** orders:
```go
// User wants to spend $50 USDC to buy YES tokens
makerAmount = 50 * 1e6        // USDC to spend (6 decimals)
takerAmount = (50 / price) * 1e6  // Expected shares to receive
```

For **SELL** orders:
```go
// User wants to sell shares worth $50
makerAmount = (50 / price) * 1e6  // Shares to sell
takerAmount = 50 * 1e6            // USDC to receive
```

---

## Code Structure

### Key Files

```
internal/
├── polymarket/
│   ├── trading.go         # Trading client, auth, order execution
│   ├── markets.go         # Market data from Gamma API
│   └── positions.go       # Position data from Data API
├── telegram/
│   ├── bot.go            # Bot struct, executeBuyOrder/executeSellOrder
│   └── handlers.go       # Command handlers, handleAmountCallback
└── wallet/
    └── wallet.go         # Key encryption/decryption
```

### Key Functions

**TradingClient (trading.go):**
```go
// Create trading client
NewTradingClient(clobURL string, chainID int64) *TradingClient

// Authentication
GetOrCreateAPICredentials(ctx, privateKey) (*APICredentials, error)
createAPICredentials(ctx, privateKey) (*APICredentials, error)
deriveAPICredentials(ctx, privateKey, nonce) (*APICredentials, error)
signL1Auth(privateKey, timestamp, nonce) (string, error)
signL2Request(secret, timestamp, method, path, body) string

// Order execution
ExecuteTrade(ctx, privateKey, proxyAddress, creds, trade) (*TradeResult, error)
submitOrder(ctx, creds, address, signedOrder, orderType) (*TradeResult, error)

// Market data
GetOrderBook(ctx, tokenID) (*OrderBook, error)
GetBestPrice(ctx, tokenID, side, amount) (float64, error)
GetTokenIDForOutcome(ctx, market, outcome) (string, error)
```

**Bot (bot.go):**
```go
// Execute trades
executeBuyOrder(ctx, user, market, outcome, amount) *TradeResult
executeSellOrder(ctx, user, market, outcome, amount) *TradeResult
```

---

## Data Types

### TradeRequest
```go
type TradeRequest struct {
    MarketID     string    // Gamma market ID
    TokenID      string    // ERC1155 token ID for the outcome
    Side         string    // "BUY" or "SELL"
    Outcome      string    // "YES" or "NO"
    Amount       float64   // Amount in USDC
    Price        float64   // Price per share (0-1), 0 = market order
    OrderType    OrderType // GTC, GTD, FOK
    Expiration   int64     // Unix timestamp for GTD orders
    NegativeRisk bool      // Whether this is a negative risk market
}
```

### TradeResult
```go
type TradeResult struct {
    Success      bool
    OrderID      string
    OrderHash    string
    ErrorMsg     string
    FilledSize   float64
    AveragePrice float64
}
```

### Order (from go-order-utils)
```go
type Order struct {
    Salt          *big.Int       // Unique salt for entropy
    TokenId       *big.Int       // ERC1155 token ID
    MakerAmount   *big.Int       // Max tokens to sell
    TakerAmount   *big.Int       // Min tokens to receive
    Side          *big.Int       // 0=BUY, 1=SELL
    Expiration    *big.Int       // Unix timestamp
    Nonce         *big.Int       // For on-chain cancellations
    FeeRateBps    *big.Int       // Fee in basis points
    SignatureType *big.Int       // 0=EOA, 1=POLY_PROXY, 2=GNOSIS_SAFE
    Maker         common.Address // Funder address
    Taker         common.Address // Zero address for public orders
    Signer        common.Address // Signing address
}
```

### Signature Types
```go
const (
    EOA             SignatureType = 0  // Direct EOA signing
    POLY_PROXY      SignatureType = 1  // EOA owns Polymarket proxy
    POLY_GNOSIS_SAFE SignatureType = 2 // EOA owns Gnosis Safe
)
```

### Order Types
```go
const (
    OrderTypeGTC OrderType = "GTC"  // Good-til-cancelled
    OrderTypeGTD OrderType = "GTD"  // Good-til-date
    OrderTypeFOK OrderType = "FOK"  // Fill-or-kill
)
```

---

## Token ID Resolution

Each market outcome (YES/NO) has a unique ERC1155 token ID.

### Token ID Structure

The Gamma API returns token IDs in the `clobTokenIds` field as a JSON array string:
```json
{
    "id": "0x1234...",
    "clobTokenIds": "[\"12345678901234567890\", \"09876543210987654321\"]"
}
```

- Index 0 = YES token ID
- Index 1 = NO token ID

### Fetching Token IDs

```go
func (tc *TradingClient) GetTokenIDForOutcome(ctx context.Context, market *GammaMarket, outcome string) (string, error) {
    // Fetch market details
    url := fmt.Sprintf("https://gamma-api.polymarket.com/markets/%s", market.ID)

    // Parse clobTokenIds JSON array
    var tokenIDs []string
    json.Unmarshal([]byte(marketDetail.ClobTokenIds), &tokenIDs)

    // Return appropriate token ID
    if outcome == "YES" {
        return tokenIDs[0], nil
    } else {
        return tokenIDs[1], nil
    }
}
```

---

## Order Types

### GTC (Good-til-Cancelled)
- Default order type
- Remains active until filled or manually cancelled
- No expiration time

### GTD (Good-til-Date)
- Expires at specified UTC timestamp
- Must have at least 1-minute security threshold
- Use for time-limited offers

### FOK (Fill-or-Kill)
- Must be filled immediately in entirety
- If cannot fill completely, entire order is cancelled
- Use for guaranteed execution or nothing

---

## API Reference

### CLOB API Endpoints

| Endpoint | Method | Auth | Description |
|----------|--------|------|-------------|
| `/auth/api-key` | POST | L1 | Create API credentials |
| `/auth/derive-api-key` | GET | L1 | Derive existing credentials |
| `/order` | POST | L2 | Submit order |
| `/orders` | GET | L2 | Get user's orders |
| `/order/{id}` | DELETE | L2 | Cancel order |
| `/book` | GET | None | Get order book |

### Order Submission Request

```json
POST /order
Content-Type: application/json

{
    "order": {
        "salt": "1234567890",
        "maker": "0x...",
        "signer": "0x...",
        "taker": "0x0000000000000000000000000000000000000000",
        "tokenId": "12345678901234567890",
        "makerAmount": "50000000",
        "takerAmount": "100000000",
        "expiration": "0",
        "nonce": "0",
        "feeRateBps": "0",
        "side": 0,
        "signatureType": 1,
        "signature": "0x..."
    },
    "owner": "api-key-uuid",
    "orderType": "GTC"
}
```

### Order Response

```json
{
    "success": true,
    "orderId": "order-uuid",
    "orderHashes": ["0x..."]
}
```

### Gamma API Endpoints

| Endpoint | Method | Description |
|----------|--------|-------------|
| `/markets` | GET | List markets |
| `/markets/{id}` | GET | Get market by ID |
| `/markets/slug/{slug}` | GET | Get market by slug |

### Contract Addresses (Polygon Mainnet)

| Contract | Address |
|----------|---------|
| CTF Exchange | `0x4bFb41d5B3570DeFd03C39a9A4D8dE6Bd8B8982E` |
| NegRisk CTF Exchange | `0xC5d563A36AE78145C45a50134d48A1215220f80a` |
| Conditional Tokens | `0x4D97DCd97eC945f40cF65F87097ACe5EA0476045` |
| USDC | `0x2791Bca1f2de4661ED88A30C99A7a9449Aa84174` |

---

## Dependencies

### Go Packages

```go
// Polymarket official order utilities
github.com/polymarket/go-order-utils v1.22.6

// Sub-packages used:
github.com/polymarket/go-order-utils/pkg/builder  // Order building
github.com/polymarket/go-order-utils/pkg/model    // Data types
github.com/polymarket/go-order-utils/pkg/signer   // Signing utilities
github.com/polymarket/go-order-utils/pkg/eip712   // EIP-712 encoding
```

### External APIs

- **CLOB API**: `https://clob.polymarket.com` - Order management
- **Gamma API**: `https://gamma-api.polymarket.com` - Market data
- **Data API**: `https://data-api.polymarket.com` - Positions

---

## Security Considerations

### Key Management

1. **Private keys are never stored in plaintext**
   - Encrypted with AES-256-GCM before database storage
   - Decrypted only when needed for signing

2. **API credentials are ephemeral**
   - Created/derived on demand
   - Not persisted (could be cached with TTL)

3. **Signatures are non-replayable**
   - Orders include random salt
   - L1 auth includes timestamp

### Best Practices

```go
// DO: Decrypt key only when needed
wallet, err := walletManager.DecryptPrivateKey(user.EncryptedKey)
defer zeroMemory(wallet.PrivateKey) // Clear from memory

// DON'T: Log private keys or signatures
log.Printf("Signing with key: %s", privateKey) // NEVER DO THIS
```

### Trading Safety

1. **Slippage protection**: Default 2% buffer on market orders
2. **Order validation**: Check balance before submission
3. **Rate limiting**: Prevent spam orders

---

## Troubleshooting

### Common Errors

| Error | Cause | Solution |
|-------|-------|----------|
| `Failed to get API credentials` | L1 auth failed | Check timestamp sync, signature format |
| `Token ID not found` | Market missing clobTokenIds | Fetch fresh market data |
| `Insufficient liquidity` | Order book empty | Reduce order size or use limit order |
| `signature error` | Invalid EIP-712 signature | Check signer matches maker/proxy |
| `Order failed: 400` | Invalid order parameters | Check amounts, token ID |

### Debug Logging

Enable verbose logging:
```go
log.Printf("Executing trade: %+v", trade)
log.Printf("Order response: %s", string(respBody))
```

### Testing Orders

1. **Use small amounts first** ($1-10) to verify flow
2. **Check order book** before placing orders
3. **Monitor positions** after trades via `/positions`

### API Response Debugging

```bash
# Test CLOB API connectivity
curl -X GET "https://clob.polymarket.com/markets"

# Test Gamma API
curl -X GET "https://gamma-api.polymarket.com/markets?limit=1"

# Check order book for a token
curl -X GET "https://clob.polymarket.com/book?token_id=TOKEN_ID"
```

---

## Buy Order Flow (Telegram UI)

### User Interface Flow

```
/markets → List trending markets
    │
    └─▶ Click market name (deep link)
        │
        ├─▶ Market details displayed
        │   ├── Current prices (Yes/No)
        │   ├── Volume, liquidity
        │   └── [📈 Buy Yes] [📉 Buy No] buttons
        │
        └─▶ Click "Buy Yes" or "Buy No"
            │
            ├─▶ Amount selection
            │   ├── [💵 $10] [💰 $25] [💎 $50]
            │   ├── [🚀 $100] [🌟 $250]
            │   ├── [✏️ Custom Amount]
            │   └── [❌ Cancel]
            │
            └─▶ Select amount → Order executes
                │
                └─▶ Result displayed (success/error)
```

### Buy Order Execution

```go
// In bot.go
func (b *Bot) executeBuyOrder(ctx context.Context, user *database.User,
    market *polymarket.GammaMarket, outcome string, amount float64) *polymarket.TradeResult {

    // 1. Decrypt wallet
    userWallet, err := b.walletManager.DecryptPrivateKey(user.EncryptedKey)

    // 2. Get API credentials
    creds, err := b.tradingClient.GetOrCreateAPICredentials(ctx, userWallet.PrivateKey)

    // 3. Get token ID for outcome
    tokenID, err := b.tradingClient.GetTokenIDForOutcome(ctx, market, outcome)

    // 4. Build trade request
    tradeReq := &polymarket.TradeRequest{
        MarketID:     market.ID,
        TokenID:      tokenID,
        Side:         "BUY",
        Outcome:      outcome,
        Amount:       amount,
        OrderType:    polymarket.OrderTypeGTC,
        NegativeRisk: market.NegRisk,
    }

    // 5. Execute trade
    return b.tradingClient.ExecuteTrade(ctx, userWallet.PrivateKey, proxyAddress, creds, tradeReq)
}
```

---

## Sell Order Flow (Telegram UI)

### User Interface Flow

```
/positions → Show all positions
    │
    ├─▶ [🔄 Refresh] [💰 Sell] buttons
    │
    └─▶ Click "Sell"
        │
        ├─▶ Position list (clickable buttons)
        │   ├── [Market Title - 10.5 YES]
        │   ├── [Market Title - 5.2 NO]
        │   └── [← Back to Positions]
        │
        └─▶ Click position
            │
            ├─▶ Position details displayed
            │   ├── Market name, outcome
            │   ├── Shares, current price, value
            │   └── Quantity selection:
            │       [25%] [50%] [75%] [💯 Sell All]
            │
            └─▶ Select quantity
                │
                ├─▶ Order type selection
                │   ├── [📊 Market Order] - Immediate at best bid
                │   └── [📝 Limit Order] - Set minimum price
                │
                ├─▶ Market Order → Execute immediately
                │   └─▶ Result displayed
                │
                └─▶ Limit Order
                    │
                    ├─▶ Prompt: "Enter price (0.01 - 0.99)"
                    │
                    └─▶ User types price → Order placed
                        └─▶ Result displayed
```

### Sell Order Implementation

#### Position-Based Selling

Unlike buy orders (which calculate shares from USD amount), sell orders use the **exact share count** from the position to avoid balance mismatches:

```go
// In bot.go - handleSellAmountCallback
// Calculate exact shares to sell based on percentage of position
posSharesRaw := pos.Shares.Int64()
sellSharesRaw := (posSharesRaw * int64(percentage)) / 100
```

#### TradeRequest with SharesRaw

```go
type TradeRequest struct {
    MarketID     string    // Gamma market ID
    TokenID      string    // ERC1155 token ID (from position.TokenID)
    Side         string    // "BUY" or "SELL"
    Outcome      string    // "YES" or "NO"
    Amount       float64   // Amount in USDC (estimate for SELL)
    SharesRaw    int64     // Raw shares with 6 decimals (for SELL)
    Price        float64   // 0 = market order, >0 = limit order
    OrderType    OrderType // GTC, GTD, FOK
    NegativeRisk bool      // Use NegRiskCTFExchange if true
}
```

#### Sell Order Execution

```go
// In bot.go
func (b *Bot) executeSellOrderFromPosition(ctx context.Context, user *database.User,
    pos *polymarket.Position, amount float64, sharesRaw int64, limitPrice float64) *polymarket.TradeResult {

    // 1. Decrypt wallet
    userWallet, err := b.walletManager.DecryptPrivateKey(user.EncryptedKey)

    // 2. Get API credentials
    creds, err := b.tradingClient.GetOrCreateAPICredentials(ctx, userWallet.PrivateKey)

    // 3. Build trade request using position data directly
    tradeReq := &polymarket.TradeRequest{
        MarketID:     pos.ConditionID,
        TokenID:      pos.TokenID,      // Use token ID from position
        Side:         "SELL",
        Outcome:      pos.Outcome,
        Amount:       amount,
        SharesRaw:    sharesRaw,        // Use exact shares from position
        Price:        limitPrice,        // 0 = market, >0 = limit
        OrderType:    polymarket.OrderTypeGTC,
        NegativeRisk: pos.NegativeRisk,
    }

    // 4. Check exchange approval (for ERC-1155 transfers)
    balanceChecker := blockchain.NewBalanceChecker(b.blockchain.GetClient())
    approved, exchangeAddr, err := balanceChecker.CheckExchangeApproval(ctx, proxyAddress, pos.NegativeRisk)
    if !approved {
        return &polymarket.TradeResult{
            Success: false,
            ErrorMsg: "Exchange not approved. Please sell once on Polymarket.com to enable approval.",
        }
    }

    // 5. Execute trade
    return b.tradingClient.ExecuteTrade(ctx, userWallet.PrivateKey, proxyAddress, creds, tradeReq)
}
```

### Amount Calculation for SELL Orders

```go
// In trading.go - ExecuteTrade
if trade.SharesRaw > 0 {
    // Use exact shares from position (preferred)
    shares = trade.SharesRaw
} else {
    // Fallback: calculate from USD amount
    shares = int64((trade.Amount / price) * 1e6)
}

// Round to 2 decimals (divisible by 10000)
sharesRounded := (shares / 10000) * 10000
makerAmount = sharesRounded

// Calculate USDC to receive from shares * price
takerAmountRaw := int64(float64(sharesRounded) * price)
takerAmountRaw = (takerAmountRaw / 10) * 10  // 5 decimals
takerAmount = takerAmountRaw
```

### Market vs Limit Orders

**Market Order (Price = 0)**:
```go
// Fetch best bid and apply 2% slippage
price = orderbook.Bids[0].Price
price *= 0.98  // Accept 2% less
if price < 0.01 {
    price = 0.01  // Floor at $0.01
}
```

**Limit Order (Price > 0)**:
```go
// Use user-specified price directly
// Order will only fill at this price or higher
price = trade.Price
```

---

## Exchange Approval Check

For selling ERC-1155 position tokens, the exchange contract must have approval to transfer tokens on behalf of the user's proxy wallet.

### Contract Addresses

```go
var (
    ConditionalTokensAddress = "0x4D97DCd97eC945f40cF65F87097ACe5EA0476045"
    CTFExchangeAddress       = "0x4bFb41d5B3570DeFd03C39a9A4D8dE6Bd8B8982E"
    NegRiskExchangeAddress   = "0xC5d563A36AE78145C45a50134d48A1215220f80a"
)
```

### Approval Check

```go
// In blockchain/balance.go
func (bc *BalanceChecker) IsApprovedForAll(ctx context.Context, owner, operator common.Address) (bool, error) {
    // Call ConditionalTokens.isApprovedForAll(owner, operator)
    // Method ID: 0xe985e9c5
    methodID := common.FromHex("0xe985e9c5")
    // ... encode and call
}

func (bc *BalanceChecker) CheckExchangeApproval(ctx context.Context, proxyAddress common.Address, isNegRisk bool) (bool, common.Address, error) {
    var exchangeAddress common.Address
    if isNegRisk {
        exchangeAddress = NegRiskExchangeAddress
    } else {
        exchangeAddress = CTFExchangeAddress
    }
    approved, err := bc.IsApprovedForAll(ctx, proxyAddress, exchangeAddress)
    return approved, exchangeAddress, err
}
```

### Setting Approval

If approval is not set, users need to either:
1. Sell a position once via Polymarket.com (triggers automatic approval)
2. Or manually call `setApprovalForAll(exchangeAddress, true)` on ConditionalTokens

---

## Position Data Structure

### Position struct

```go
type Position struct {
    MarketID     string   `json:"market_id"`
    MarketTitle  string   `json:"market_title"`
    ConditionID  string   `json:"condition_id"`
    TokenID      string   `json:"token_id"`      // ERC-1155 token ID
    Outcome      string   `json:"outcome"`       // YES or NO
    Shares       *big.Int `json:"shares"`
    AveragePrice float64  `json:"average_price"`
    CurrentPrice float64  `json:"current_price"`
    Value        float64  `json:"value"`         // Current value in USDC
    PnL          float64  `json:"pnl"`
    PnLPercent   float64  `json:"pnl_percent"`
    NegativeRisk bool     `json:"negative_risk"` // Use NegRiskCTFExchange
}
```

### Data API Response

Positions are fetched from `https://data-api.polymarket.com/positions?user=PROXY_ADDRESS`:

```json
{
    "proxyWallet": "0x...",
    "asset": "123456789...",        // Token ID
    "conditionId": "0xabc...",
    "size": 100.5,                   // Shares
    "avgPrice": 0.45,
    "curPrice": 0.52,
    "currentValue": 52.26,
    "cashPnl": 7.26,
    "percentPnl": 15.5,
    "title": "Will X happen?",
    "outcome": "Yes",
    "negativeRisk": true
}
```

---

## Order Management

### View Open Orders (`/orders`)

The `/orders` command displays all open orders using L2 authentication.

```
/orders → Fetch and display open orders
    │
    ├─▶ Loading: "📋 Fetching your open orders..."
    │
    ├─▶ Display orders list
    │   ├── Order side (📈 BUY / 📉 SELL)
    │   ├── Price, Size, Filled amount
    │   ├── Order type (GTC, GTD, FOK)
    │   ├── Creation time
    │   └── Truncated order ID
    │
    └─▶ [🔄 Refresh] [❌ Cancel All] buttons
```

### OpenOrder Data Structure

```go
type OpenOrder struct {
    ID              string   `json:"id"`
    Status          string   `json:"status"`
    Owner           string   `json:"owner"`
    MakerAddress    string   `json:"maker_address"`
    Market          string   `json:"market"`
    AssetID         string   `json:"asset_id"`
    Side            string   `json:"side"`
    OriginalSize    string   `json:"original_size"`
    SizeMatched     string   `json:"size_matched"`
    Price           string   `json:"price"`
    AssociateTrades []string `json:"associate_trades"`
    Outcome         string   `json:"outcome"`
    CreatedAt       int64    `json:"created_at"`
    Expiration      string   `json:"expiration"`
    OrderType       string   `json:"order_type"`
}
```

### Fetching Open Orders

```go
// GetOpenOrders fetches all open orders using L2 authentication
func (tc *TradingClient) GetOpenOrders(ctx context.Context, address common.Address, creds *APICredentials) ([]*OpenOrder, error) {
    // GET /data/orders with pagination
    // Uses L2 headers: POLY_ADDRESS, POLY_SIGNATURE, POLY_TIMESTAMP, POLY_API_KEY, POLY_PASSPHRASE
    // Returns paginated results until next_cursor == "LTE=" (end marker)
}
```

### Cancel Single Order (`/cancel`)

```
/cancel <order_id> → Cancel specific order
    │
    ├─▶ Loading: "🚫 Cancelling order..."
    │
    ├─▶ DELETE /order/{order_id} with L2 auth
    │
    └─▶ Result: Success or error message
```

### Cancel Order Implementation

```go
// CancelOrder cancels a single order by ID using L2 authentication
func (tc *TradingClient) CancelOrder(ctx context.Context, address common.Address, creds *APICredentials, orderID string) error {
    // DELETE /order/{orderID}
    // Uses L2 headers for authentication
    // Returns nil on success, error on failure
}
```

### Cancel All Orders

The "Cancel All" button cancels all open orders sequentially:

```go
// CancelAllOrders cancels all open orders
func (tc *TradingClient) CancelAllOrders(ctx context.Context, address common.Address, creds *APICredentials) (int, error) {
    // 1. Get all open orders
    // 2. Cancel each order individually
    // 3. Return count of successfully cancelled orders
}
```

### L2 Authentication for Orders

All order management endpoints require L2 authentication:

```go
// L2 Headers required:
req.Header.Set("POLY_ADDRESS", address.Hex())
req.Header.Set("POLY_SIGNATURE", signature)     // HMAC-SHA256
req.Header.Set("POLY_TIMESTAMP", timestamp)
req.Header.Set("POLY_API_KEY", creds.APIKey)
req.Header.Set("POLY_PASSPHRASE", creds.Passphrase)

// Signature calculation:
message := timestamp + method + path + body
signature := base64URLEncode(HMAC-SHA256(base64Decode(secret), message))
```

---

## Future Improvements

1. **Order caching**: Cache API credentials with TTL
2. ~~**Limit orders**: Support user-specified prices~~ ✅ Implemented
3. **Order tracking**: Store orders in database for history
4. **Partial fills**: Handle partial fill notifications
5. ~~**Cancel functionality**: Implement order cancellation~~ ✅ Implemented
6. ~~**Position-based sells**: Sell based on current position size~~ ✅ Implemented
7. **Automatic approval**: Send setApprovalForAll transaction from bot
8. ~~**View open orders**: Fetch and display open orders~~ ✅ Implemented

---

## References

- [Polymarket CLOB Documentation](https://docs.polymarket.com/developers/CLOB)
- [go-order-utils GitHub](https://github.com/Polymarket/go-order-utils)
- [EIP-712 Specification](https://eips.ethereum.org/EIPS/eip-712)
- [Polymarket TypeScript Client](https://github.com/Polymarket/clob-client)
