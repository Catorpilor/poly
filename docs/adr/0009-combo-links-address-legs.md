# 9. Combo Links address a Combo by its Legs

Date: 2026-10-22

## Status

Accepted

## Context

A Copy Signal for a combo trade needs a deep link into the bot, like the
`s_<slug>` link single-market signals carry. A Combo has nothing to put in
that link:

- **No slug and no page.** `polymarket.com/combo/<id>` and
  `/combos/<id>` are 404; Gamma has no combo object. Polymarket's own
  "Share combo" links encode the legs (Gamma market ID + side), not a
  combo identity.
- **Its on-chain identity is not resolvable alone.** Every Data API route
  that understands a Combo Condition (`/v2/activity/combos`,
  `/v2/positions/combos`) requires `user=<wallet>`; the wallet-free routes
  reject combo condition IDs.
- **Telegram caps the start parameter at 64 characters** of
  `[A-Za-z0-9_-]`. A wallet (40 hex) plus a condition ID (62 hex) is 102.

What the bot needs to price and buy a Combo is the leg position IDs.
Gamma's `/markets/<id>` returns them (`positionIds`, `[YES, NO]` —
distinct from `clobTokenIds`), so a market ID and a side are sufficient.

## Decision

A Combo Link carries only the Legs: `c_` followed, per leg in leg order,
by the Gamma market ID and `y` (the market's first outcome, index 0 —
"Yes", "Over") or `n` (the second, index 1), with no separators —
e.g. `c_5801197y5889785n5905290n`. The bot rebuilds the Combo from Gamma;
no wallet, Combo Condition or bot-side registry is involved. The format is
not meant to be human-readable.

The external notifier builds the link from
`/v2/activity/combos?user=<wallet>&condition=<id>` (`legs[].market.market_id`
and `leg_outcome_index`). A Combo whose link would exceed 64 characters is
signalled without a link.

## Considered options

- **`c_<wallet>_<condition prefix>`** — fits (59 chars), but puts a wallet
  in every link and makes the bot depend on the tracked wallet's history to
  find the legs. Rejected.
- **Leg slugs** — readable, but one slug (~40 chars) fills the budget.
- **Bot-side registry of tracked wallets with short aliases** — shorter
  links, but the bot and notifier must be kept in sync, and the bot would
  start knowing whom the user follows (ADR 0004 scope creep). Rejected.

## Consequences

- Links are self-contained: any Combo — including one nobody holds — can be
  opened from its legs, and old links in chat history keep resolving for as
  long as Gamma market IDs do.
- **Hard limit of about 7 legs** with 7-digit market IDs (8 chars per leg),
  shrinking as IDs grow. Larger Combos get alerts without a Trade link. If
  that becomes common, the encoding can be compacted (e.g. base36 IDs)
  under a new prefix; existing `c_` links must keep working.
- The link names legs, not the signalled Combo Condition. Two Combos with
  the same legs and sides are the same Combo, so this loses nothing.
- Leg order in the link follows the source's `leg_index`; the bot must not
  depend on it beyond display.
