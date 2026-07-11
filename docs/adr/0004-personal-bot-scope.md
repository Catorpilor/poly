# 4. Poly is a personal bot, not a multi-user product

Status: accepted
Date: 2026-07-09

## Context

The codebase carries multi-user infrastructure (users table, web login
handshake, per-user account classification), which implies open
onboarding. Production reality is a personal deployment: the operator's
own accounts, on the operator's own hardware. The distinction gates real
work: wallet import mis-resolves the trading wallet for email/third-party
Polymarket signups (the new deposit-wallet factory's addresses cannot be
derived deterministically), which is release-blocking for a product and
irrelevant for a personal bot.

## Decision

Poly is a personal/small-circle bot. New users are onboarded by hand and
can be verified by hand. Consequences:

- The email-signup resolver defect is a documented known limitation, not
  scheduled work.
- SL/TP thresholds stay global constants (policy, not per-user config).
- No sessions, audit logs, or admin pagination; multi-user hardening
  (key-custody threat model for strangers' keys) is out of scope.

Revisit this ADR before ever inviting users outside the trusted circle.
