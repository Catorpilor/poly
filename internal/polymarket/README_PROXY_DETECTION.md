# Polymarket Proxy Detection Issue

## Problem
Users who have traded on Polymarket have proxy wallets, but the bot cannot detect them.

## What We Know
1. Polymarket uses a dual-wallet system: EOA (user controls) → Proxy (holds funds, executes trades)
2. Every user who has traded has a proxy wallet
3. The proxy is deterministically generated from the EOA address

## Detection Methods Implemented

### 1. Deterministic Calculation (CREATE2)
- Calculates expected proxy address using CREATE2 formula
- Tries multiple salt patterns that Polymarket might use
- Checks if calculated address has contract code

### 2. Registry Query
- Attempts to call potential registry contracts
- Tries various method names: `getProxy()`, `userToProxy()`, `wallets()`, etc.
- Queries known Polymarket factory addresses

### 3. API Query
- Queries Polymarket's CLOB API (if endpoints are available)
- Tries `/user/{address}` and `/derive-proxy` endpoints

### 4. Gnosis Safe Detection
- Scans ProxyCreation events from Gnosis Safe factories
- Checks if EOA is owner of any Safe contracts

## Known Issues

### 1. Unknown Factory Address
The actual Polymarket proxy factory address might be different from what we're using:
- Current guess: `0xaacfeea03eb1561c4e67d661e40682bd20e3541b`
- May need to find the actual factory through transaction analysis

### 2. Unknown Salt Pattern
Polymarket's salt calculation for CREATE2 might be:
- Simple: `keccak256(eoaAddress)`
- Complex: `keccak256("POLYMARKET", eoaAddress, nonce)`
- Custom: Some other pattern

### 3. Unknown Init Code Hash
The init code hash for proxy contracts is crucial for CREATE2 calculation.
Current guess is based on standard Gnosis Safe proxies.

## How to Find Your Proxy Manually

### Option 1: Check Polygonscan
1. Go to https://polygonscan.com/address/YOUR_EOA_ADDRESS
2. Look for transactions to/from proxy contracts
3. Check "Internal Transactions" tab
4. Look for contract creations

### Option 2: Check Polymarket UI
1. Log into Polymarket with your wallet
2. Go to portfolio/positions
3. Check the network tab in browser DevTools
4. Look for API calls that return your proxy address

### Option 3: Use Polymarket's API Directly
```bash
# If you have API access
curl -X GET "https://clob.polymarket.com/user/YOUR_EOA_ADDRESS"
```

## Debugging Steps

1. **Enable Debug Logging**: The bot will log all detection attempts
2. **Check Logs**: Look for "Debugging Proxy Detection" messages
3. **Manual Override**: You can manually set proxy address in database if needed

## Workaround

Until automatic detection works, you can:
1. Find your proxy address manually (see above)
2. Update the database directly:
```sql
UPDATE users
SET proxy_address = 'YOUR_PROXY_ADDRESS'
WHERE eoa_address = 'YOUR_EOA_ADDRESS';
```

## Next Steps

To properly fix this, we need:
1. The actual Polymarket proxy factory contract address
2. The exact salt calculation method
3. The correct init code hash
4. Or access to Polymarket's internal API/registry

The best approach would be to:
1. Analyze a known EOA→Proxy pair on Polygonscan
2. Find the factory contract that deployed it
3. Reverse engineer the salt pattern
4. Update the detection logic accordingly