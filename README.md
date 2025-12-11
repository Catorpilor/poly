# Polymarket Telegram Trading Bot

A powerful Telegram bot for trading on Polymarket directly through Telegram, featuring secure wallet management, real-time position tracking, and automated order execution.

## Features

### Phase 1 (MVP) - Currently Implemented
- ✅ **Wallet Management**: Generate new EOA wallets with Gnosis Safe proxy support
- ✅ **Secure Key Storage**: AES-256-GCM encryption for private keys
- ✅ **Database Schema**: PostgreSQL with full migration support
- ✅ **Telegram Bot Interface**: Command-based interaction with rate limiting
- ✅ **Configuration Management**: Environment-based configuration
- 🚧 **Basic Trading**: Market buy/sell orders (in development)
- 🚧 **Position Tracking**: Real-time balance checking (in development)

### Planned Features (Phase 2-3)
- 📅 Quick access links for instant trading
- 📅 Limit orders with price targets
- 📅 P&L tracking and analytics
- 📅 Price alerts and notifications
- 📅 Multi-wallet support
- 📅 Automated trading strategies

## Architecture

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

## Prerequisites

- Go 1.21 or higher
- PostgreSQL 14 or higher
- Redis (for session management)
- Telegram Bot Token (from [@BotFather](https://t.me/botfather))
- Polygon RPC URL (e.g., Alchemy, Infura)
- Polymarket API access

## Installation

### 1. Clone the Repository

```bash
git clone https://github.com/Catorpilor/poly.git
cd poly
```

### 2. Install Dependencies

```bash
go mod download
```

### 3. Set Up Environment Variables

Copy the example environment file and configure it:

```bash
cp .env.example .env
```

Edit `.env` with your configuration:

```env
# Required Configuration
TELEGRAM_BOT_TOKEN=your_telegram_bot_token
DATABASE_URL=postgresql://user:pass@localhost:5432/polymarket_bot
POLYGON_RPC_URL=https://polygon-rpc.com
ENCRYPTION_KEY=your_64_char_hex_key  # Generate with: openssl rand -hex 32
POLYMARKET_API_KEY=your_api_key
```

### 4. Set Up Database

#### Install PostgreSQL

If PostgreSQL is not installed:

```bash
# macOS
brew install postgresql@14
brew services start postgresql@14

# Ubuntu/Debian
sudo apt update
sudo apt install postgresql postgresql-contrib
sudo systemctl start postgresql

# CentOS/RHEL
sudo yum install postgresql14-server postgresql14
sudo postgresql-14-setup initdb
sudo systemctl start postgresql-14
```

#### Create Database User and Database

1. **Connect to PostgreSQL as superuser:**

```bash
# macOS/Linux
sudo -u postgres psql

# Or if you have a postgres user
psql -U postgres
```

2. **Create a dedicated user for the bot:**

```sql
-- Create user with password
CREATE USER polymarket_bot_user WITH PASSWORD 'your_secure_password_here';

-- Grant necessary privileges
ALTER USER polymarket_bot_user CREATEDB;
```

3. **Create the database:**

```sql
-- Create database owned by the bot user
CREATE DATABASE polymarket_bot OWNER polymarket_bot_user;

-- Connect to the new database
\c polymarket_bot

-- Grant all privileges on the database to the user
GRANT ALL PRIVILEGES ON DATABASE polymarket_bot TO polymarket_bot_user;

-- Ensure the user can create schemas and tables
GRANT CREATE ON DATABASE polymarket_bot TO polymarket_bot_user;
```

4. **Configure PostgreSQL for password authentication:**

Edit `pg_hba.conf` (location varies by system):

```bash
# Find pg_hba.conf location
sudo -u postgres psql -c "SHOW hba_file"

# Edit the file
sudo nano /etc/postgresql/14/main/pg_hba.conf
```

Ensure these lines are present:
```
# TYPE  DATABASE        USER                    ADDRESS                 METHOD
local   all             polymarket_bot_user                             md5
host    all             polymarket_bot_user     127.0.0.1/32           md5
host    all             polymarket_bot_user     ::1/128                md5
```

Restart PostgreSQL:
```bash
# Ubuntu/Debian
sudo systemctl restart postgresql

# macOS
brew services restart postgresql@14
```

#### Run Database Migrations

1. **Test the connection first:**

```bash
psql -U polymarket_bot_user -d polymarket_bot -h localhost -W
# Enter password when prompted
# Type \q to exit
```

2. **Run the initial migration:**

```bash
# Method 1: Using psql
psql -U polymarket_bot_user -d polymarket_bot -h localhost -f migrations/001_initial_schema.sql

# Method 2: Using psql with password in environment variable
PGPASSWORD='your_secure_password_here' psql -U polymarket_bot_user -d polymarket_bot -h localhost -f migrations/001_initial_schema.sql

# Method 3: Using .pgpass file (more secure)
echo "localhost:5432:polymarket_bot:polymarket_bot_user:your_secure_password_here" >> ~/.pgpass
chmod 600 ~/.pgpass
psql -U polymarket_bot_user -d polymarket_bot -h localhost -f migrations/001_initial_schema.sql
```

3. **Verify the migration:**

```sql
-- Connect to the database
psql -U polymarket_bot_user -d polymarket_bot -h localhost

-- List all tables
\dt

-- You should see:
--  public | audit_logs    | table | polymarket_bot_user
--  public | markets       | table | polymarket_bot_user
--  public | orders        | table | polymarket_bot_user
--  public | positions     | table | polymarket_bot_user
--  public | price_alerts  | table | polymarket_bot_user
--  public | sessions      | table | polymarket_bot_user
--  public | users         | table | polymarket_bot_user

-- Check indexes
\di

-- Exit
\q
```

#### Configure Database URL

Update your `.env` file with the correct database URL:

```env
# Format: postgresql://username:password@host:port/database
DATABASE_URL=postgresql://polymarket_bot_user:your_secure_password_here@localhost:5432/polymarket_bot

# With SSL (for production)
DATABASE_URL=postgresql://polymarket_bot_user:your_secure_password_here@localhost:5432/polymarket_bot?sslmode=require

# With connection pool settings
DATABASE_URL=postgresql://polymarket_bot_user:your_secure_password_here@localhost:5432/polymarket_bot?pool_max_conns=25&pool_min_conns=5
```

#### Database Backup and Restore

**Create a backup:**

```bash
# Full database backup
pg_dump -U polymarket_bot_user -d polymarket_bot -h localhost -F c -b -v -f polymarket_bot_backup.dump

# SQL format backup (readable)
pg_dump -U polymarket_bot_user -d polymarket_bot -h localhost --clean --if-exists -f polymarket_bot_backup.sql
```

**Restore from backup:**

```bash
# From custom format
pg_restore -U polymarket_bot_user -d polymarket_bot -h localhost -v polymarket_bot_backup.dump

# From SQL format
psql -U polymarket_bot_user -d polymarket_bot -h localhost -f polymarket_bot_backup.sql
```

#### Troubleshooting

**Common Issues:**

1. **"FATAL: password authentication failed"**
   - Check password is correct
   - Ensure pg_hba.conf is configured for md5 authentication
   - Restart PostgreSQL after configuration changes

2. **"FATAL: database does not exist"**
   - Ensure you created the database: `CREATE DATABASE polymarket_bot;`
   - Check you're connecting to the right database name

3. **"FATAL: role does not exist"**
   - Create the user: `CREATE USER polymarket_bot_user WITH PASSWORD 'password';`
   - Check username spelling in connection string

4. **"could not connect to server"**
   - Ensure PostgreSQL is running: `sudo systemctl status postgresql`
   - Check the host and port in connection string
   - Verify PostgreSQL is listening on the correct interface

**Reset Database (Development Only):**

```bash
# Drop and recreate everything
psql -U postgres << EOF
DROP DATABASE IF EXISTS polymarket_bot;
DROP USER IF EXISTS polymarket_bot_user;
CREATE USER polymarket_bot_user WITH PASSWORD 'your_secure_password_here';
CREATE DATABASE polymarket_bot OWNER polymarket_bot_user;
GRANT ALL PRIVILEGES ON DATABASE polymarket_bot TO polymarket_bot_user;
EOF

# Re-run migrations
psql -U polymarket_bot_user -d polymarket_bot -h localhost -f migrations/001_initial_schema.sql
```

#### Security Best Practices

1. **Use strong passwords:** Generate with `openssl rand -base64 32`
2. **Limit connections:** Configure pg_hba.conf to only allow necessary hosts
3. **Use SSL in production:** Set `sslmode=require` in connection string
4. **Regular backups:** Set up automated daily backups with retention policy
5. **Monitor logs:** Check PostgreSQL logs for suspicious activity
6. **Separate users:** Don't use the postgres superuser for the application
7. **Network security:** Use firewall rules to restrict database access

### 5. Run the Bot

```bash
go run cmd/bot/main.go
```

For production, build the binary:

```bash
go build -o polymarket-bot cmd/bot/main.go
./polymarket-bot
```

## Usage

### Bot Commands

#### Wallet Management
- `/start` - Initialize bot and create/import wallet
- `/wallet` - Show wallet addresses and balances
- `/import` - Import existing wallet
- `/export` - Export wallet backup

#### Trading
- `/markets` - List active markets
- `/market <id>` - Show market details
- `/buy <amount> <YES/NO> <market_id> [price]` - Buy tokens
- `/sell <amount> <YES/NO> <market_id> [price]` - Sell tokens
- `/orders` - Show open orders
- `/cancel <order_id>` - Cancel an order

#### Portfolio
- `/positions` - Show all positions
- `/pnl` - Calculate unrealized P&L
- `/history` - View trade history

#### Settings & Utilities
- `/settings` - Configure preferences
- `/alerts` - Set price alerts
- `/gas` - Check MATIC balance for gas
- `/help` - Show help message

### Example Usage

1. **Start the bot and create a wallet:**
   ```
   /start
   [Choose "Create New Wallet"]
   ```

2. **Check your wallet balance:**
   ```
   /wallet
   ```

3. **Buy YES tokens in a market:**
   ```
   /buy 100 YES 0x123abc... 0.65
   ```
   This buys 100 USDC worth of YES tokens at 0.65 (65%) price

4. **Check your positions:**
   ```
   /positions
   ```

## Project Structure

```
poly/
├── cmd/
│   └── bot/              # Application entry point
├── internal/
│   ├── config/           # Configuration management
│   ├── database/         # Database connection and models
│   ├── telegram/         # Telegram bot handlers
│   ├── wallet/           # Wallet generation and management
│   ├── polymarket/       # Polymarket API integration
│   └── blockchain/       # Polygon blockchain interaction
├── pkg/
│   └── encryption/       # Encryption utilities
├── migrations/           # Database migrations
├── tests/                # Test files
├── .env.example          # Environment variables example
├── go.mod                # Go module file
└── README.md            # This file
```

## Security

### Key Security Features
- **Private Key Encryption**: All private keys are encrypted using AES-256-GCM
- **Secure Storage**: Keys are stored encrypted in the database
- **Message Auto-deletion**: Sensitive messages are automatically deleted
- **Rate Limiting**: 60 requests per minute per user
- **Session Management**: 30-minute timeout for idle sessions
- **Audit Logging**: All trading operations are logged

### Best Practices
1. Never share your private keys
2. Use a strong encryption key (generate with `openssl rand -hex 32`)
3. Keep your bot token secret
4. Use environment variables, never hardcode credentials
5. Regularly backup your database
6. Monitor the audit logs for suspicious activity

## Development

### Running Tests

```bash
go test ./...
```

### Building for Production

```bash
# Linux
GOOS=linux GOARCH=amd64 go build -o polymarket-bot cmd/bot/main.go

# With optimizations
go build -ldflags="-s -w" -o polymarket-bot cmd/bot/main.go
```

### Docker Support

Docker configuration coming soon. Will include:
- Multi-stage build for smaller images
- Docker Compose for full stack deployment
- Health checks and auto-restart

## Monitoring

### Health Check

The bot exposes a health endpoint (coming soon):
```bash
curl http://localhost:8080/health
```

### Logs

Logs are output to stdout and can be redirected:
```bash
./polymarket-bot 2>&1 | tee bot.log
```

## Contributing

Contributions are welcome! Please follow these steps:

1. Fork the repository
2. Create a feature branch (`git checkout -b feature/amazing-feature`)
3. Commit your changes (`git commit -m 'Add amazing feature'`)
4. Push to the branch (`git push origin feature/amazing-feature`)
5. Open a Pull Request

## Roadmap

### Phase 1 (MVP) ✅
- [x] Wallet creation/import
- [x] Basic balance checking
- [ ] Market buy/sell orders
- [ ] Position display

### Phase 2 (Enhanced)
- [ ] Quick access links
- [ ] Limit orders
- [ ] P&L tracking
- [ ] Price alerts

### Phase 3 (Advanced)
- [ ] Multi-wallet support
- [ ] Automated strategies
- [ ] Copy trading
- [ ] Analytics dashboard

## Support

For issues and feature requests, please use the [GitHub Issues](https://github.com/Catorpilor/poly/issues) page.

## License

This project is licensed under the MIT License - see the LICENSE file for details.

## Disclaimer

This bot is for educational purposes. Trading involves risk. Always:
- Do your own research
- Never invest more than you can afford to lose
- Understand the risks of prediction markets
- Keep your private keys secure

## Acknowledgments

- Polymarket for the API and infrastructure
- Telegram Bot API for the messaging platform
- Go community for excellent libraries and tools