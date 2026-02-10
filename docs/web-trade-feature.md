# Web Trade Feature

## Overview

This feature adds trade execution capability to the live monitoring web interface. Users can buy positions directly from event panels after logging in via Telegram authentication.

## Architecture

```
Web UI → Trade Button Click → POST /api/trade → Auth Check → Execute Trade → Return Result
```

**Key Components:**
- Port: 8081 (existing web server)
- Auth: Session token from localStorage (via Telegram login)
- UI: Buy buttons on each event panel
- Orders: Market orders with VWAP pricing and 2% slippage buffer

## API Specification

### POST `/api/trade`

**Request:**
```json
{
    "session": {
        "telegramId": 123456789,
        "walletAddress": "0x...",
        "proxyAddress": "0x..."
    },
    "trade": {
        "eventSlug": "nba-lac-uta-2026-01-27",
        "outcomeIndex": 0,
        "side": "BUY",
        "amount": 10.0
    }
}
```

**Response (Success):**
```json
{
    "success": true,
    "orderId": "abc123",
    "message": "Trade executed successfully"
}
```

**Response (Error):**
```json
{
    "success": false,
    "error": "Insufficient balance"
}
```

**Status Codes:**
- 200: Success
- 400: Validation error or trade failed
- 401: Not authenticated
- 405: Method not allowed
- 503: Trading not configured

## Implementation Details

### Files Modified

| File | Changes |
|------|---------|
| `internal/live/webserver.go` | Added trade endpoint, dependencies, handler, market selection logic |
| `internal/telegram/bot.go` | Added `GetWalletManager()`, `GetTradingClient()` getters |
| `internal/live/static/index.html` | Added trade UI and JavaScript |
| `cmd/bot/main.go` | Pass walletManager and tradingClient to WebServer |
| `internal/polymarket/trading.go` | Added `GetMarketInfo()`, changed to `TakerFeeBps` (basis points) |

### WebServer Dependencies

```go
type WebServer struct {
    liveManager    *LiveTradeManager
    // ... existing fields
    userRepo       repositories.UserRepository
    walletManager  *wallet.Manager
    tradingClient  *polymarket.TradingClient
}
```

### Trade Execution Flow

1. **Authentication**: Validate session has telegramId, fetch user from DB, verify proxyAddress matches
2. **Wallet Decryption**: Decrypt user's private key via walletManager
3. **API Credentials**: Get or create Polymarket API credentials
4. **Market Resolution**: Resolve event slug to market ID and token ID (with Moneyline selection for sports)
5. **Taker Fee Fetch**: Get taker fee from CLOB API (in basis points, e.g., 1000 = 10%)
6. **Trade Execution**: Build TradeRequest and call `tradingClient.ExecuteTrade()`

### Moneyline Market Selection

For sports/esports events with multiple markets (spreads, totals, props), the system automatically selects the Moneyline market using `GetPrimaryMarket()` from the resolver. Markets are filtered by checking (case-insensitive) for these sub-market keywords:

**General:**
- `spread` - spread betting
- `over`, `under`, `o/u` - over/under markets
- `score` - correct score markets
- `handicap` - handicap betting
- `total games`, `total goals`, `total kills`, `total sets`, `total points`, `total maps`, `total rounds` - totals markets (specific patterns to avoid matching tournament names like "Qatar Total Open")
- `: total`, `: o/u` - colon-prefixed totals patterns

**Sports (NBA, NFL, etc.):**
- `points`, `rebounds`, `assists` - player props
- `1h `, `1q ` - half/quarter markets
- `(-`, `(+` - spread notation like "(-10.5)"
- `goals` - soccer totals

**Tennis:**
- `1st `, `2nd `, `3rd ` - set winner markets (e.g., "1st Set Winner")
- `set ` - set-related markets (e.g., "Set Handicap")

**Esports (LoL, etc.):**
- `first` - first blood, first tower, etc.
- `blood` - first blood
- `tower` - first tower
- `dragon` - first dragon
- `baron` - first baron
- `inhibitor` - first inhibitor
- `kills` - total kills
- `map `, `maps` - map-specific markets
- `series:` - series markets

**Fallback logic:**
1. First pass: select markets with NO sub-market keywords (these are ML markets)
2. Second pass: look for markets with "win" in question, but still skip sub-markets (prevents "Set 1 Winner" from being selected via "win" → "Winner" substring match)
3. Last resort: return first active market

**Examples:**
| Market Question | Filtered? | Reason |
|-----------------|-----------|--------|
| "Pistons vs. Nuggets" | No | ML market |
| "Spread: Pistons (-7.5)" | Yes | contains "spread", "(-" |
| "Pistons vs. Nuggets: O/U 219.5" | Yes | contains "o/u" |
| "LeBron James: Points O/U 27.5" | Yes | contains "points", "o/u" |
| "First Blood in Game 1?" | Yes | contains "first", "blood" |
| "T1 vs. Gen.G" | No | ML market |
| "1H Moneyline: Heat vs. Bulls" | Yes | contains "1h " |
| "1st Set Winner" | Yes | contains "1st ", "set " |
| "Set Handicap: Djokovic (-1.5)" | Yes | contains "set ", "handicap" |
| "Set 1 Winner: Parks vs Zheng" | Yes | contains "set ", "1st " |
| "Qatar Total Open: Parks vs Zheng" | No | ML market ("Total" is in tournament name, not a totals pattern) |
| "Zheng vs. Sabalenka" | No | ML market |

### Frontend Integration

**Trade Section HTML:**
```html
<div id="trade-${slug}" class="trade-section hidden">
    <input type="number" id="amount-${slug}" value="10" min="1" max="1000">
    <button onclick="executeTrade('${slug}', 0, 'BUY')">Buy ${outcome0}</button>
    <button onclick="executeTrade('${slug}', 1, 'BUY')">Buy ${outcome1}</button>
</div>
```

**JavaScript:**
```javascript
async function executeTrade(eventSlug, outcomeIndex, side) {
    const session = getSession();
    if (!session?.telegramId) {
        alert('Please login first');
        return;
    }

    const amount = parseFloat(document.getElementById(`amount-${eventSlug}`)?.value || 10);

    const response = await fetch('/api/trade', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({
            session: session,
            trade: { eventSlug, outcomeIndex, side, amount }
        })
    });

    const result = await response.json();
    alert(result.success ? `Order placed: ${result.orderId}` : `Error: ${result.error}`);
}
```

## Taker Fee Handling

Markets may have taker fees (e.g., 10% for crypto markets). The system:

1. Fetches market info from CLOB API using the token ID
2. Extracts `taker_base_fee` (returned as basis points: 1000 = 10%)
3. Includes fee in the trade request for proper order signing

**Note:** The CLOB API requires the condition ID (not token ID) for market lookup. The system first fetches the order book to get the condition ID, then fetches market info.

## Usage

1. Start the bot with live web server enabled
2. Navigate to `http://localhost:8081`
3. Login via Telegram (click Login button, use Telegram bot)
4. Subscribe to an event (e.g., NBA game)
5. Trade section appears with Buy buttons showing team names
6. Enter amount (default: 10 USDC), click Buy button
7. Trade executes immediately as market order

## Limitations

- Buy only (sell via Telegram bot)
- Market orders only (no limit orders)
- Single outcome per trade
- Requires prior Telegram login and wallet setup
