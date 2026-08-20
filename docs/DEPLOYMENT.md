# Polymarket Telegram Bot - Deployment Guide

This guide covers deploying the bot on a Raspberry Pi with Docker, connecting to existing PostgreSQL and Redis services.

## Prerequisites

- Raspberry Pi (3B+ or newer recommended, 4 preferred for ARM64)
- Raspberry Pi OS (64-bit recommended for better performance)
- Docker and Docker Compose installed
- PostgreSQL installed and running
- Redis installed and running
- A Telegram Bot Token (from @BotFather)
- A Polygon RPC URL (from Alchemy, Infura, or QuickNode)

## 1. Prepare the Raspberry Pi

### Install Docker (if not already installed)

```bash
# Update system
sudo apt update && sudo apt upgrade -y

# Install Docker
curl -fsSL https://get.docker.com -o get-docker.sh
sudo sh get-docker.sh

# Add your user to docker group (logout/login required after)
sudo usermod -aG docker $USER

# Install Docker Compose plugin
sudo apt install docker-compose-plugin -y

# Verify installation
docker --version
docker compose version
```

### Configure PostgreSQL

```bash
# Connect to PostgreSQL
sudo -u postgres psql

# Create database and user
CREATE DATABASE polybot;
CREATE USER polybot WITH ENCRYPTED PASSWORD 'your_secure_password';
GRANT ALL PRIVILEGES ON DATABASE polybot TO polybot;
\c polybot
GRANT ALL ON SCHEMA public TO polybot;
\q
```

Edit PostgreSQL config to allow connections from Docker:

```bash
# Find your PostgreSQL config
sudo nano /etc/postgresql/*/main/postgresql.conf
# Set: listen_addresses = '*'

# Edit pg_hba.conf
sudo nano /etc/postgresql/*/main/pg_hba.conf
# Add line for Docker network:
# host    polybot    polybot    172.17.0.0/16    md5

# Restart PostgreSQL
sudo systemctl restart postgresql
```

### Configure Redis

```bash
# Edit Redis config to allow external connections
sudo nano /etc/redis/redis.conf
# Set: bind 0.0.0.0
# Set: protected-mode no (or set a password)

# Restart Redis
sudo systemctl restart redis
```

## 2. Clone and Configure the Bot

```bash
# Clone repository
git clone https://github.com/Catorpilor/poly.git
cd poly

# Copy and edit environment file
cp .env.example .env
nano .env
```

### Required Environment Variables

Edit `.env` with your values:

```bash
# Telegram Bot Configuration (REQUIRED)
TELEGRAM_BOT_TOKEN=your_bot_token_from_botfather

# Database Configuration (REQUIRED)
# Use your Raspberry Pi's IP or 'host.docker.internal' for Docker
DATABASE_URL=postgresql://polybot:your_secure_password@host.docker.internal:5432/polybot
DATABASE_MAX_CONNECTIONS=10
DATABASE_MAX_IDLE_CONNECTIONS=5

# Polygon Network Configuration (REQUIRED)
# Get from: https://www.alchemy.com/ or https://www.infura.io/
POLYGON_RPC_URL=https://polygon-mainnet.g.alchemy.com/v2/YOUR_API_KEY

# Encryption Configuration (REQUIRED)
# Generate with: openssl rand -hex 32
ENCRYPTION_KEY=your_64_character_hex_string_here

# Redis Configuration
REDIS_URL=redis://host.docker.internal:6379/0

# Application Settings
ENVIRONMENT=production
LOG_LEVEL=info

# Trading Configuration
DEFAULT_SLIPPAGE_PERCENT=2.0
MAX_ORDER_SIZE_USDC=1000
MIN_ORDER_SIZE_USDC=1
```

### Generate Encryption Key

```bash
# Generate a secure 32-byte hex key
openssl rand -hex 32
# Example output: a1b2c3d4e5f6...64 characters total
```

## 3. Run Database Migrations

Before starting the bot, initialize the database schema:

```bash
# Connect to PostgreSQL and run migrations
psql -h localhost -U polybot -d polybot -f migrations/001_initial_schema.sql

# Or from the Pi directly:
sudo -u postgres psql -d polybot -f migrations/001_initial_schema.sql
```

### Per-release migrations (apply BEFORE swapping the image)

Migrations ship as numbered `migrations/NNN_*.sql` files applied by hand — there
is no in-app runner (`cmd/bot/main.go`). The bot's arm reader (`scanArm`)
`SELECT`s every column unconditionally, so a new arm-table column must exist
before the new image starts or **every armed-position read crashes**. Apply the
pending file(s) against the live DB first, then swap the image:

```bash
# v0.21.x — deep-entry exit ladder (issue #81): adds sltp_arms.ladder_rungs_fired
# and ladder_base_shares. Apply this BEFORE `docker compose up -d` with the new tag.
psql -h localhost -U polybot -d polybot -f migrations/011_deep_entry_ladder.sql

# v0.21.1 — snipe-buy persistence (issue #84): creates snipe_buys. Missing table
# degrades loudly to pre-#84 amnesia (the fix silently isn't there) — apply first.
psql -h localhost -U polybot -d polybot -f migrations/012_snipe_buys.sql
```

Before `docker compose down`, check the session monitor for tokens currently
inside the snipe alert band: a restart during a live episode re-alerts and,
without #84's table populated, re-buys (ledger r81). Prefer deploying between
episodes.

## 4. Build and Run with Docker

### Option A: Using Docker Compose (Recommended)

```bash
# Build and start the bot
docker compose up -d --build

# View logs
docker compose logs -f polybot

# Stop the bot
docker compose down

# Rebuild after code changes
docker compose up -d --build
```

### Option B: Using Docker directly

```bash
# Build the image
docker build -t polybot:latest .

# Run the container
docker run -d \
  --name polybot \
  --restart unless-stopped \
  --env-file .env \
  --add-host=host.docker.internal:host-gateway \
  polybot:latest

# View logs
docker logs -f polybot

# Stop and remove
docker stop polybot && docker rm polybot
```

### Option C: Cross-compile on a faster machine

If building on the Pi is too slow, cross-compile on your development machine:

```bash
# On your Mac/Linux development machine
# Build for ARM64 (Raspberry Pi 4, Pi 3 64-bit OS)
docker buildx build --platform linux/arm64 -t polybot:latest --load .

# Save the image
docker save polybot:latest | gzip > polybot-arm64.tar.gz

# Transfer to Raspberry Pi
scp polybot-arm64.tar.gz pi@raspberrypi:~/

# On Raspberry Pi - load the image
gunzip -c polybot-arm64.tar.gz | docker load

# Run with docker compose
docker compose up -d
```

## 5. Verify Deployment

```bash
# Check if container is running
docker ps

# Check logs for startup messages
docker compose logs polybot

# Test the bot
# Send /start to your bot on Telegram
```

Expected startup logs:
```
Starting Polymarket Telegram Bot...
Connected to database
Connected to Redis
Bot started successfully
```

## 6. Maintenance

### Update the Bot

```bash
cd poly
git pull
docker compose up -d --build
```

### View Logs

```bash
# Live logs
docker compose logs -f polybot

# Last 100 lines
docker compose logs --tail 100 polybot
```

### Restart the Bot

```bash
docker compose restart polybot
```

### Backup Database

```bash
# Create backup
pg_dump -h localhost -U polybot polybot > backup_$(date +%Y%m%d).sql

# Restore backup
psql -h localhost -U polybot polybot < backup_20241211.sql
```

### Monitor Resources

```bash
# Check container resource usage
docker stats polybot

# Check system resources
htop
```

## 7. Troubleshooting

### Bot won't connect to database

```bash
# Check PostgreSQL is running
sudo systemctl status postgresql

# Test connection from host
psql -h localhost -U polybot -d polybot -c "SELECT 1;"

# Check Docker can reach host
docker run --rm --add-host=host.docker.internal:host-gateway alpine ping -c 3 host.docker.internal
```

### Bot won't connect to Redis

```bash
# Check Redis is running
sudo systemctl status redis

# Test connection
redis-cli ping
```

### Container keeps restarting

```bash
# Check logs for errors
docker compose logs polybot

# Common issues:
# - Missing or invalid TELEGRAM_BOT_TOKEN
# - Invalid DATABASE_URL format
# - ENCRYPTION_KEY not 64 characters
# - PostgreSQL/Redis not accessible
```

### Slow builds on Raspberry Pi

The Go compiler can be slow on Raspberry Pi. Options:
1. Cross-compile on a faster machine (see Option C above)
2. Use pre-built binary instead of Docker
3. Increase swap space temporarily:
   ```bash
   sudo dphys-swapfile swapoff
   sudo nano /etc/dphys-swapfile  # Set CONF_SWAPSIZE=2048
   sudo dphys-swapfile setup
   sudo dphys-swapfile swapon
   ```

## 8. Security Recommendations

1. **Firewall**: Only expose PostgreSQL/Redis to localhost or Docker network
   ```bash
   sudo ufw allow ssh
   sudo ufw enable
   ```

2. **Database**: Use strong passwords and limit permissions

3. **Encryption Key**: Store securely, back up separately from database

4. **Updates**: Keep system and Docker images updated
   ```bash
   sudo apt update && sudo apt upgrade -y
   docker compose pull
   ```

5. **Monitoring**: Set up alerts for container health
   ```bash
   # Simple health check cron job
   */5 * * * * docker ps | grep -q polybot || docker compose -f /home/pi/poly/docker-compose.yml up -d
   ```

## 9. Optional: Run as systemd Service

For automatic startup on boot without Docker:

```bash
# Build binary directly
go build -o polybot ./cmd/bot

# Create systemd service
sudo nano /etc/systemd/system/polybot.service
```

```ini
[Unit]
Description=Polymarket Telegram Bot
After=network.target postgresql.service redis.service

[Service]
Type=simple
User=pi
WorkingDirectory=/home/pi/poly
EnvironmentFile=/home/pi/poly/.env
ExecStart=/home/pi/poly/polybot
Restart=always
RestartSec=10

[Install]
WantedBy=multi-user.target
```

```bash
# Enable and start
sudo systemctl daemon-reload
sudo systemctl enable polybot
sudo systemctl start polybot
sudo systemctl status polybot
```

## 10. Network Path: Proxy Environment (GFW)

The production Pi sits behind the GFW. **There is no working direct path from
this network to Telegram or to ANY polymarket.com host** — earlier "CLOB works
direct" observations were the LAN's transparent SNI proxy silently rescuing
that traffic. All external traffic must ride the explicit proxy.

### Compose environment (production values)

```yaml
environment:
  - HTTP_PROXY=http://192.168.1.108:7890
  - HTTPS_PROXY=http://192.168.1.108:7890
  # ONLY local addresses — every external host needs the proxy
  - NO_PROXY=host.docker.internal,localhost,127.0.0.1
```

And in `.env`:

```bash
# The proxy tunnels kill ~60s idle connections; short polls survive.
# Default is 60 (fine on unproxied networks). Clamped to [1,90].
TELEGRAM_POLL_TIMEOUT_SECONDS=15
```

### The proxy box (mihomo on rasp1)

- `ssh zeh@192.168.1.108` (passwordless sudo). Service: `mihomo.service`,
  binary `/opt/mihomo/mihomo`, config `/etc/mihomo/config.yaml`,
  API `http://192.168.1.108:9090` (unauthenticated), explicit port `:7890`.
- The `Telegram` selector defaults to **TG-Auto** — a `url-test` group probing
  `https://api.telegram.org` every 120s over HK/JP/SG/TW/US with automatic
  failover. `profile.store-selected: true` persists selections across restarts.
- After editing the config: validate with
  `sudo /opt/mihomo/mihomo -t -d /etc/mihomo`, then hot-reload with
  `curl -X PUT "http://192.168.1.108:9090/configs?force=true" -d '{"path": "/etc/mihomo/config.yaml"}'`.

### Failure signatures

| Symptom in bot logs | Meaning | Fix |
|---|---|---|
| `getUpdates ... context deadline exceeded` repeatedly, offset frozen | proxy's Telegram route down or long-polls exceed the tunnel's idle-kill threshold | check TG-Auto via the :9090 API; confirm `TELEGRAM_POLL_TIMEOUT_SECONDS` is short |
| replies sent with EMPTY text; `data-api`/`gamma` timeouts | a Polymarket host forced onto the (nonexistent) direct path | check NO_PROXY only lists local addresses |
| orders rejected `Trading restricted in your region` | proxy exit country is Polymarket-restricted (US/SG/UK/FR…) | repoint the `Proxies` group at an allowed-country exit |
| `answerCallbackQuery ... query is too old` | updates delivered late (path degraded); taps DID process | same as row 1 — latency, not loss |

## 11. EC2 Warm Standby & Cutover Runbook

An AWS m5.large (ap-southeast-1) holds a ready deployment, reachable only via
Tailscale: `ssh ec2-user@100.77.31.77`, dir `~/poly_deploy/` (note: the
hyphenated `docker-compose` binary there). Stack: polybot + `postgres:17`
(`poly_postgres`) + `redis:7-alpine` on an internal network, web UI bound to
the Tailscale IP only (`:8083`).

**⚠️ Polymarket geoblocks ORDER PLACEMENT from Singapore AWS IPs** — data
APIs, WS, and alerts all work, so reachability checks pass while trading is
dead. Before any future cutover to a new egress, verify an actual order from
there first. Tokyo (ap-northeast-1) is the vetted relocation candidate.

Cutover sequence (proven 2026-08-16, 43s bot-down window):
1. Prep everything while the old host runs: compose up db containers, copy
   `.env`, rehearsal `pg_dump`/`pg_restore`, compare row counts.
2. Old host `docker compose down` → final dump → restore → new host up.
   Two instances must NEVER poll `getUpdates` concurrently.
3. Verify: `Authorized`, getUpdates responses, `SLTPMonitor: Started with N
   armed token(s)` matching the arms table, SnipeWatcher, RTDS, web UI probe.
4. Old host stays down but intact — it is the rollback path (proven same-day:
   stop new bot, `docker compose up -d` on old).
