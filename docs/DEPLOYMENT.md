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
