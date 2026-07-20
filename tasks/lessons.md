
## 2026-07-20 — CLOB `success:true` is NOT a fill
Polymarket /order returns `success:true, status:"delayed"` with empty
taking/makingAmount on in-play markets (bet delay); the order may still be
killed. Never treat submit-acceptance as execution — parse `status`, and
for delayed orders poll the order (or verify the position) before acting
on "sold". Found in production: first v0.11.0 FOK stop "fired", disarmed,
and notified while zero shares actually sold. Always smoke-test new order
paths on an in-play market specifically — that's where the exchange
semantics differ. (Issue #22)
