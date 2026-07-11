# 3. /redeem stays, right-sized around Polymarket auto-redemption

Status: accepted
Date: 2026-07-09

## Context

On-chain investigation of production redemptions (2026-07-09) established
three facts that contradict the code's own assumptions:

1. **Polymarket auto-redeems.** A keeper batch-redeems resolved positions
   for all holders (address-sorted sweeps, ~19 users per tx, ERC-4337
   bundler infra) and mints pUSD to each holder, typically same-day.
   Recent "user" redemptions on both production accounts were actually
   keeper transactions.
2. **CTF conditions still settle in USDC.e in the V2 era.** Markets
   created in July 2026 pay USDC.e at the CTF layer; an adapter wraps the
   payout and mints pUSD at the boundary. The fear in `redeem.go`'s
   comment — that V2-era markets would need per-market (pUSD) collateral —
   did not materialize. The USDC.e hardcode in `EncodeStandardRedemption`
   is *correct*, not a stopgap.
3. **The bot's own redeem path has two warts.** Standard redemption pays
   raw USDC.e to the trading wallet, which the V2 exchange cannot spend
   and the pUSD-denominated balance display does not show. And for
   deposit-wallet accounts the path builds a Gnosis SafeTx against a
   contract that is not a Safe — never exercised, presumed broken.

## Decision

Keep `/redeem` as a "collect now instead of waiting for the keeper"
convenience, right-sized:

- Document the USDC.e hardcode as deliberate and correct (fix the stale
  comment).
- Append a wrap-to-pUSD step (existing `EncodeWrapCollateral`) in the same
  MultiSend batch after standard redemption, so redeemed funds are
  immediately visible and tradeable.
- Refuse `/redeem` for deposit-wallet accounts with a "winnings arrive
  automatically" message.

Alternatives rejected: retiring `/redeem` entirely (bets on an
undocumented keeper with no SLA); building a deposit-wallet execution
route (real reverse-engineering, near-zero payoff while auto-redeem
covers it).

## Consequences

- If Polymarket ever creates natively-pUSD CTF conditions, standard
  redemption needs per-market collateral resolution at that point.
- Deposit-wallet users cannot force early collection through the bot.
