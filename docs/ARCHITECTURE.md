# Polymarket Telegram Trading Bot - Technical Specification

## Executive Summary

A Telegram bot enabling direct Polymarket trading without web interface, featuring wallet management, position tracking, quick market access, and order execution. The bot abstracts Polymarket's dual-wallet architecture while maintaining security through encrypted key storage and user authentication.

## Architecture Overview

### System Components
```
┌─────────────────┐     ┌──────────────────┐     ┌─────────────────┐
│  Telegram Users │────▶│  Telegram Bot    │────▶│  Polymarket     │
└─────────────────┘     │  (Go Backend)    │     │  Infrastructure │
                        └──────────────────┘     └─────────────────┘
                                │                         │
                                ▼                         ▼
                        ┌──────────────────┐     ┌─────────────────┐
                        │  Database        │     │  Polygon Chain  │
                        │  (PostgreSQL)    │     │  (Smart Contracts)
                        └──────────────────┘     └─────────────────┘
```

### Data Flow
1. User sends command via Telegram
2. Bot authenticates user and retrieves encrypted wallet
3. Bot interacts with Polymarket CLOB API and/or Polygon contracts
4. Results formatted and returned to user via Telegram

## Feature Specifications

### 1. Wallet Management

#### 1.1 Wallet Creation Architecture

**Critical Finding**: Proxy wallets (Gnosis Safe) CANNOT be created without an EOA owner. The architecture requires:
```
EOA (has private key) ──owns──> Proxy Wallet (smart contract) ──holds──> Assets
```

**Implementation Strategy**:

**Option A: Full Wallet Generation** (Recommended)
```
1. Generate new EOA keypair
2. Store encrypted private key
3. Deploy Gnosis Safe proxy with EOA as owner
4. Use proxy for all trading operations
```

**Option B: Lightweight EOA-Only**
```
1. Generate EOA keypair
2. Use EOA directly for trading (less secure, simpler)
3. No proxy wallet (requires Polymarket API adjustments)
```

**Option C: Custodial Proxy Pool**
```
1. Bot maintains pool of pre-deployed proxies
2. Assigns proxy to user on registration
3. Bot remains technical owner (regulatory concerns)
```

#### 1.2 Wallet Import Flow
```yaml
Import Process:
  1. User provides private key via secure message
  2. Bot validates key format and derives address
  3. Bot checks for existing Polymarket proxy:
     - If exists: Link and verify ownership
     - If not: Offer to create proxy
  4. Encrypt and store credentials
  5. Delete original message for security
```

#### 1.3 Security Architecture
```yaml
Encryption:
  - Master key: Derived from bot token + salt
  - User keys: AES-256-GCM encrypted
  - Storage: Encrypted at rest in PostgreSQL
  - Memory: Keys decrypted only for signing

Authentication:
  - Telegram user ID as primary identifier
  - Optional 2FA via TOTP - Session management with TTL
  - Rate limiting per user
```

### 2. Position Balance Tracking

#### 2.1 ConditionalTokens Contract Analysis

**Contract**: `0x4D97DCd97eC945f40cF65F87097ACe5EA0476045`

**Key Functions for Balance Checking**:
```solidity
// Function #5: balanceOf(address owner, uint256 id)
// Returns balance of specific position token
balanceOf(owner, positionId) → uint256

// Position ID Calculation:
positionId = keccak256(
    collateralToken,  // USDC address
    collectionId      // keccak256(conditionId + indexSet)
)

// Where indexSet represents outcome:
// YES token: indexSet = 1 (0b01)
// NO token:  indexSet = 2 (0b10)
```

**Balance Query Strategy**:
```yaml
Full Position Scan:
  1. Get user's trade history from CLOB API
  2. Extract unique conditionIds
  3. For each condition:
     - Calculate YES position ID (indexSet=1)
     - Calculate NO position ID (indexSet=2)
     - Query balanceOf for each
  4. Filter non-zero balances
  5. Enrich with market metadata

Optimized Approach:
  1. Monitor Transfer events to/from user's proxy
  2. Cache active positionIds
  3. Batch query balances
  4. Update on trade execution
```

#### 2.2 Position Value Calculation
```yaml
Position Value Formula:
  Value = Shares * Current_Price

  Where:
    Shares = balanceOf(proxy, positionId) / 1e6
    Current_Price = from CLOB orderbook

  Unrealized P&L:
    P&L = (Current_Price - Avg_Buy_Price) * Shares
```

### 3. Quick Access Links

#### 3.1 Deep Link Structure Analysis

**URL Pattern**: `https://t.me/polymarketinsiderbot?start=a013f214-9b11-46b3-829c-8e680a8a338d`

**UUID Analysis** (`a013f214-9b11-46b3-829c-8e680a8a338d`):
```yaml
Hypothesis Testing:

  1. Market Identifier (Most Likely):
     - UUID maps to specific market/event
     - Enables direct market access without search
     - Format: Internal mapping or encoded market_id

  2. Referral Tracking:
     - UUID as affiliate/referral code
     - Tracks user acquisition source
     - Enables commission sharing

  3. Session Token:
     - Pre-authenticated session
     - Temporary access token
     - Expires after first use or timeout

  4. Composite Encoding:
     Structure: [market_id]-[user_ref]-[timestamp]-[action]
     Example: a013f214 (market) - 9b11 (source) - 46b3 (time) - 829c (buy/sell)
```

**Implementation Approach**:
```yaml
Quick Access System:
  1. Generate UUID for each market:
     uuid = sha256(market_id + salt)[:36]

  2. Store mapping:
     Database: uuid -> {market_id, created_at, click_count}

  3. Handle deep link:
     - Extract UUID from start parameter
     - Lookup market details
     - Present instant trade interface
     - Track analytics

  4. Dynamic link generation:
     /market [market_id] -> Returns deep link
     Share format: "Trade [MARKET] on Polymarket"
```

#### 3.2 Quick Action Buttons
```yaml
Instant Interface:
  On deep link open:
    - Show market title and current odds
    - Pre-populate trade amount (configurable)
    - Two buttons: [BUY YES] [BUY NO]
    - One-click execution with confirmation
```

### 4. Trading Implementation

#### 4.1 Order Types

**Market Orders**:
```yaml
Execution Logic:
  1. Fetch current orderbook
  2. Calculate slippage for requested size
  3. If slippage < threshold (e.g., 2%):
     - Execute as taker order
  4. Else:
     - Warn user about slippage
     - Offer to split order or use limit
```

**Limit Orders**:
```yaml
Order Management:
  1. Place maker order at specified price
  2. Store order ID in database
  3. Monitor fill status via webhook/polling
  4. Notify user on partial/full fill
  5. Auto-cancel after expiry
```

#### 4.2 Order Flow Architecture
```yaml
Trade Execution Pipeline:
  1. Command Parsing:
     /buy 100 YES @0.65 market_id

  2. Validation:
     - Check balance (USDC in proxy)
     - Verify market is open
     - Calculate fees

  3. Order Creation:
     - Generate nonce and expiry
     - Create EIP-712 typed data
     - Sign with user's EOA key

  4. Submission:
     - POST to CLOB API
     - Handle response/errors
     - Store in database

  5. Confirmation:
     - Format success message
     - Show order details
     - Update position cache
```

## Database Schema
```sql
-- Users table
CREATE TABLE users (
    telegram_id BIGINT PRIMARY KEY,
    username VARCHAR(255),
    eoa_address VARCHAR(42),
    proxy_address VARCHAR(42),
    encrypted_key TEXT,  -- AES-256-GCM encrypted
    settings JSONB,
    created_at TIMESTAMP DEFAULT NOW()
);

-- Markets cache
CREATE TABLE markets (
    market_id VARCHAR(66) PRIMARY KEY,
    quick_access_uuid UUID UNIQUE,
    title TEXT,
    condition_id VARCHAR(66),
    token_ids JSONB,  -- {yes: "0x...", no: "0x..."}
    cached_data JSONB,
    updated_at TIMESTAMP DEFAULT NOW()
);

-- Orders tracking
CREATE TABLE orders (
    order_id VARCHAR(66) PRIMARY KEY,
    telegram_id BIGINT REFERENCES users(telegram_id),
    market_id VARCHAR(66) REFERENCES markets(market_id),
    side VARCHAR(4),  -- BUY/SELL
    outcome VARCHAR(3),  -- YES/NO
    size DECIMAL(20,6),
    price DECIMAL(10,6),
    status VARCHAR(20),
    filled DECIMAL(20,6) DEFAULT 0,
    created_at TIMESTAMP DEFAULT NOW()
);

-- Positions cache
CREATE TABLE positions (
    telegram_id BIGINT REFERENCES users(telegram_id),
    market_id VARCHAR(66) REFERENCES markets(market_id),
    position_id VARCHAR(66),
    outcome VARCHAR(3),
    shares DECIMAL(20,6),
    avg_price DECIMAL(10,6),
    updated_at TIMESTAMP DEFAULT NOW(),
    PRIMARY KEY (telegram_id, position_id)
);
```

## API Integration Points

### Polymarket CLOB API
```yaml
Endpoints:
  - GET /markets/{id} - Market details
  - GET /orderbook - Current orderbook
  - POST /orders - Place order
  - DELETE /orders/{id} - Cancel order
  - GET /orders?maker={address} - User orders

Authentication:
  - Sign message with EOA
  - Include auth token in headers
```

### Polygon RPC
```yaml
Direct Contract Calls:
  - ConditionalTokens.balanceOf() - Position balances
  - USDC.balanceOf() - Collateral balance
  - CTFExchange.getOrderStatus() - On-chain order status

Event Monitoring:
  - Transfer events for position changes
  - OrderFilled events for execution tracking
```

## Bot Commands Structure
```yaml
Wallet Management:
  /start - Initialize bot, create/import wallet
  /wallet - Show addresses and balances
  /import - Import existing wallet
  /export - Export encrypted backup

Trading:
  /markets - List active markets
  /market [id] - Show market details + quick link
  /buy [amount] [YES/NO] [price?] [market_id]
  /sell [amount] [YES/NO] [price?] [market_id]
  /orders - Show open orders
  /cancel [order_id] - Cancel order

Portfolio:
  /positions - Show all positions
  /pnl - Calculate unrealized P&L
  /history - Trade history

Settings:
  /settings - Configure preferences
  /alerts - Set price alerts
  /gas - Check MATIC balance
```

## Security Considerations
```yaml
Critical Security Measures:
  1. Private keys never logged or transmitted plain
  2. Automatic message deletion for sensitive data
  3. Rate limiting: 60 requests/minute per user
  4. Encrypted database with key rotation
  5. Audit logging for all trades
  6. Maximum order size limits
  7. Withdrawal requires 2FA
  8. Session timeout after 30 minutes idle

Risk Mitigations:
  - SQL injection: Parameterized queries only
  - CSRF: Not applicable (Telegram auth)
  - Replay attacks: Nonce validation
  - Front-running: Randomized broadcast delay
```

## Performance Optimizations
```yaml
Caching Strategy:
  - Market data: 30 second TTL
  - Orderbook: 5 second TTL
  - Positions: Update on trade + 60 second poll
  - User settings: In-memory with Redis

Scaling Approach:
  - Connection pooling for RPC
  - Batch balance queries
  - Webhook for order updates vs polling
  - PostgreSQL read replicas
  - Rate limit at nginx level
```

## Deployment Architecture
```yaml
Infrastructure:
  - Bot Server: Go binary on Ubuntu VPS
  - Database: PostgreSQL 14+ with encryption
  - Cache: Redis for session management
  - Monitoring: Prometheus + Grafana
  - Logs: Vector -> ClickHouse

Environment Variables:
  TELEGRAM_BOT_TOKEN=
  DATABASE_URL=
  POLYGON_RPC_URL=
  CLOB_API_KEY=
  ENCRYPTION_KEY=
  REDIS_URL=
```

## Implementation Phases
```yaml
Phase 1 (MVP):
  - Wallet creation/import
  - Basic balance checking
  - Market buy/sell orders
  - Position display

Phase 2 (Enhanced):
  - Quick access links
  - Limit orders
  - P&L tracking
  - Price alerts

Phase 3 (Advanced):
  - Multi-wallet support
  - Automated strategies
  - Copy trading
  - Analytics dashboard
```

## Regulatory Compliance
```yaml
Considerations:
  - Non-custodial design (users control keys)
  - No fiat on/off ramps
  - Jurisdiction restrictions via IP
  - Terms of service acceptance
  - Trade history export for taxes
  - Maximum position limits
```
