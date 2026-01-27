# Telegram Login for Live Web Interface

This feature enables users to authenticate the live web interface using their Telegram account.

## Overview

The login flow allows users to:
1. Click "Login with Telegram" on the web interface
2. Get redirected to Telegram bot with a login token
3. Bot authenticates user and creates wallet if needed
4. User returns to web with an authenticated session

## Architecture

```
┌─────────────┐     ┌─────────────┐     ┌─────────────┐     ┌─────────────┐
│   Browser   │────▶│  Web Server │────▶│  Database   │◀────│  Telegram   │
│  (Frontend) │     │  (Backend)  │     │ (Postgres)  │     │    Bot      │
└─────────────┘     └─────────────┘     └─────────────┘     └─────────────┘
       │                   │                   │                   │
       │  1. Click Login   │                   │                   │
       │──────────────────▶│                   │                   │
       │                   │  2. Create Token  │                   │
       │                   │──────────────────▶│                   │
       │  3. Open Telegram │                   │                   │
       │◀──────────────────│                   │                   │
       │                   │                   │                   │
       │                   │                   │  4. Deep Link     │
       │───────────────────│───────────────────│──────────────────▶│
       │                   │                   │                   │
       │                   │                   │  5. Authenticate  │
       │                   │                   │◀──────────────────│
       │                   │                   │                   │
       │  6. Poll Status   │                   │                   │
       │──────────────────▶│──────────────────▶│                   │
       │                   │                   │                   │
       │  7. Complete      │                   │                   │
       │──────────────────▶│──────────────────▶│                   │
       │                   │                   │                   │
       │  8. Session       │                   │                   │
       │◀──────────────────│                   │                   │
```

## Database Schema

The feature uses a `login_tokens` table:

```sql
CREATE TABLE login_tokens (
    token UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    status VARCHAR(20) NOT NULL DEFAULT 'pending'
        CHECK (status IN ('pending', 'authenticated', 'used', 'expired')),
    telegram_id BIGINT REFERENCES users(telegram_id) ON DELETE SET NULL,
    wallet_address VARCHAR(42),
    proxy_address VARCHAR(42),
    created_at TIMESTAMPTZ DEFAULT NOW(),
    authenticated_at TIMESTAMPTZ,
    expires_at TIMESTAMPTZ NOT NULL,
    used_at TIMESTAMPTZ
);
```

Token status transitions:
- `pending` → `authenticated` (when user authenticates via Telegram)
- `authenticated` → `used` (when web completes the login)
- `pending` → `expired` (after 5 minutes)

## API Endpoints

### POST `/api/auth/init`

Creates a new login token and returns Telegram deep link.

**Response:**
```json
{
    "token": "uuid-string",
    "telegramUrl": "https://t.me/botname?start=login_uuid",
    "expiresAt": 1234567890
}
```

### GET `/api/auth/status?token=<uuid>`

Checks the status of a login token.

**Response:**
```json
{
    "status": "pending|authenticated|used|expired",
    "walletAddress": "0x...",
    "proxyAddress": "0x..."
}
```

### POST `/api/auth/complete`

Completes the login and marks the token as used.

**Request:**
```json
{
    "token": "uuid-string"
}
```

**Response:**
```json
{
    "success": true,
    "telegramId": 123456789,
    "walletAddress": "0x...",
    "proxyAddress": "0x..."
}
```

## Telegram Bot Deep Link

The bot handles deep links with the format:
```
/start login_<uuid>
```

When received, the bot:
1. Validates the token exists and is pending
2. Gets or creates the user (generates wallet if new)
3. Marks the token as authenticated with user details
4. Sends success message to user

## Configuration

Add these environment variables to `.env`:

```bash
# Bot username for generating Telegram deep links (without @)
TELEGRAM_BOT_USERNAME=your_bot_username

# URL for the live web interface (used in success message for HTTPS URLs)
LIVE_WEB_URL=http://localhost:8081
```

## Frontend Flow

The web interface JavaScript:

1. **initiateLogin()** - Creates token via `/api/auth/init`, opens Telegram
2. **startLoginPolling()** - Polls `/api/auth/status` every 3 seconds
3. **completeLogin()** - Calls `/api/auth/complete`, saves session to localStorage
4. **checkExistingSession()** - Checks localStorage and URL params on page load
5. **logout()** - Clears localStorage session

Session is stored in localStorage:
```json
{
    "telegramId": 123456789,
    "walletAddress": "0x...",
    "proxyAddress": "0x...",
    "authenticatedAt": 1234567890
}
```

## Files Modified/Created

| File | Description |
|------|-------------|
| `migrations/002_login_tokens.sql` | Database migration |
| `internal/config/config.go` | Added BotUsername and LiveWebURL config |
| `internal/database/models.go` | Added LoginToken model |
| `internal/database/repositories/login_token_repository.go` | Token CRUD operations |
| `internal/telegram/bot.go` | Added handleLoginToken handler |
| `internal/live/webserver.go` | Added auth API endpoints |
| `internal/live/static/index.html` | Added login UI and JavaScript |
| `cmd/bot/main.go` | Updated WebServer initialization |

## Running the Migration

```bash
psql $DATABASE_URL < migrations/002_login_tokens.sql
```

## Security Considerations

- Tokens expire after 5 minutes
- Tokens can only be used once (marked as `used` after completion)
- Private keys are never exposed - only wallet addresses are shared
- Session is stored client-side in localStorage
- Telegram bot validates token ownership before authentication

## Notes

- For localhost development, Telegram doesn't allow URL buttons (requires HTTPS)
- The web interface auto-detects login via polling, so no button needed for localhost
- For production with HTTPS, an "Open Live Web" button is shown in Telegram
