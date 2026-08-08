# 8. Durable, web-managed Live Watches with Event Refresh

Date: 2026-08-08

## Status

Accepted

## Context

Live subscriptions are in-memory and Telegram-only: `/live <slug>`
resolves an event once, registers its markets with the snipe watcher,
and is forgotten on restart. Three costs materialized in one week of
production:

- **Watch latency** (issue #55): series games are created on Gamma
  mid-series; a subscription resolved once never sees them. Team WE's
  Game-2 market registered at 15:19:31 — after the token had crashed
  from $0.125 to $0.0065 and rebounded. The system's best-ever setups
  (150× at the observed bottom) are invisible in exactly the window
  they exist.
- **Restart amnesia**: every deploy or crash drops all watches until
  each is manually re-subscribed. Three restarts this week each zeroed
  coverage silently.
- **Subscription churn**: the web page can view and even trade, but
  subscribing still round-trips through Telegram deep links per game
  slug; the friction pushes manual unsubscribes that later prove wrong
  (the BLG–TES unsubscribe preceded the biggest favorite-collapse in
  the ledger by minutes).

The in-memory philosophy (documented for the snipe caps: "a restart
resets it — soft rail") was being applied to two different kinds of
state. Caps are *bounds on harm* — self-healing daily, cheap to lose.
Watches are *user intent* — losing one silently costs coverage, and
coverage is upstream of every dollar the strategy makes.

## Decision

1. **Persistence**: Live Watches move to Postgres (migration 010:
   `live_subscriptions(chat_id, event_slug, tape, created_at)`,
   PK `(chat_id, event_slug)`). Boot re-registers and re-resolves every
   stored watch. The registry remains the runtime view; the table is
   the durable record. Caps and episode latches stay in-memory —
   the soft-rail doctrine is narrowed, not abandoned.
2. **Event Refresh**: a loop re-resolves each subscribed event every
   ~2 minutes (batched, identity-validated Gamma lookups per the #33
   lessons) and idempotently registers newly appeared markets. Scoped
   to subscribed events; refresh errors log and skip — only positive
   closed evidence (the #40 sweeper doctrine) expires a watch, with the
   grouped 🧹 notice.
3. **Web management**: the authenticated web page (whose session
   already carries the Telegram identity and already exposes trading)
   gains `PUT/DELETE /api/events/{slug}/subscription` and
   `GET /api/subscriptions` behind the existing session guard, plus a
   subscribe toggle and watch list in the UI. Web-created and
   Telegram-created watches are the same object. Per-connection web
   *viewing* subscriptions are unchanged and remain ephemeral.
4. **Guardrails**: 30 active watches per user; per-watch tape flag
   (quiet by default); in-play unsubscribes get a UI confirmation
   (quiet watches cost nothing to keep — the asymmetry favors staying
   subscribed).

Rollout: v0.15.0 in four TDD phases — persistence, refresh loop, web
API/UI, expiry sweep — ordered so the watch-latency fix (phases 1–2)
ships even if the UI slips.

## Consequences

- Issue #55's class of miss is closed for subscribed events: a game
  market created mid-series is watched within ~2 minutes, and session
  highs seed from the 2h history window, so even a crash-in-progress
  is covered.
- Restarts stop costing coverage; deploys become watch-neutral.
- The bot gains a steady, bounded Gamma polling load (subscribed
  events × 1 lookup / 2 min, batched). Unbounded discovery crawling
  remains explicitly out of scope.
- A second write path (web) to subscription state exists; both paths
  funnel through one manager method, and the DB constraint dedupes.
- The glossary's Live Watch / Event Refresh terms (CONTEXT.md) become
  the canonical vocabulary; "subscription" without qualification is
  ambiguous between durable watches and WS viewing and should be
  avoided in new code.
