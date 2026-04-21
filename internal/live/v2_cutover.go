package live

import "time"

// Polymarket CTF Exchange V2 cutover window. During this interval the SL/TP
// monitor must not fire: the order book is wiped, bids during the hot-swap are
// not reliable signals, and the V2 CLOB endpoints/WS shape may differ.
//
// Bounds are inclusive of start, exclusive of end. Hardcoded for v1; if the
// cutover date shifts, update these constants and redeploy.
var (
	V2CutoverStart = time.Date(2026, 4, 28, 10, 30, 0, 0, time.UTC)
	V2CutoverEnd   = time.Date(2026, 4, 28, 12, 0, 0, 0, time.UTC)
)

// V2CutoverPause is a PauseWindow suitable for NewSLTPMonitor. Returns true while
// now is inside the cutover window.
func V2CutoverPause(now time.Time) bool {
	return !now.Before(V2CutoverStart) && now.Before(V2CutoverEnd)
}
