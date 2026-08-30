package telegram

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"

	"github.com/Catorpilor/poly/internal/live"
	"github.com/Catorpilor/poly/internal/polymarket"
)

// recordingHeldWatch records the held-registration calls the buy-success hook
// drives. renewResult decides the RenewHeldMarket branch: true takes the renew
// path (no Gamma fetch), false forces the WatchHeld fan-out path.
type recordingHeldWatch struct {
	mu          sync.Mutex
	renewed     []string
	held        []string // WatchHeld (direct) token IDs
	walked      []string // WatchWalked (series-walked) token IDs (issue #102)
	renewResult bool
	eventSlug   string // served by EventSlugOf (series-walk tests, issue #94)
}

func (r *recordingHeldWatch) WatchArmed(live.SnipeMarket) {}
func (r *recordingHeldWatch) UnwatchArmed(string)         {}
func (r *recordingHeldWatch) MarkBought(string)           {}
func (r *recordingHeldWatch) WatchHeld(_ int64, m live.SnipeMarket, _ time.Duration) {
	r.mu.Lock()
	r.held = append(r.held, m.TokenID)
	r.mu.Unlock()
}
func (r *recordingHeldWatch) WatchWalked(_ int64, m live.SnipeMarket, _ time.Duration) {
	r.mu.Lock()
	r.walked = append(r.walked, m.TokenID)
	r.mu.Unlock()
}
func (r *recordingHeldWatch) WalkedOnlyHolder(int64, string) bool { return false }
func (r *recordingHeldWatch) RenewHeldMarket(_ int64, tokenID string, _ time.Duration) bool {
	r.mu.Lock()
	r.renewed = append(r.renewed, tokenID)
	result := r.renewResult
	r.mu.Unlock()
	return result
}
func (x *recordingHeldWatch) EventSlugOf(string) string {
	x.mu.Lock()
	defer x.mu.Unlock()
	return x.eventSlug
}
func (r *recordingHeldWatch) SiblingTokenIDs(_, _ string) []string { return nil }

func (r *recordingHeldWatch) renewedTokens() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.renewed...)
}

func (r *recordingHeldWatch) heldTokens() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.held...)
}

func (r *recordingHeldWatch) walkedTokens() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.walked...)
}

// fakePositionSource is a scripted snipePositionSource.
type fakePositionSource struct {
	positions []*polymarket.Position
	err       error
	calls     int
}

func (f *fakePositionSource) GetPositions(context.Context, common.Address) ([]*polymarket.Position, error) {
	f.calls++
	return f.positions, f.err
}

// A successful buy hook fetches the buyer's positions and registers each as a
// Held Watch (issue #64), off the Data API by way of the injected scanner. A
// token the watcher already knows renews via RenewHeldMarket (the whole market,
// issue #78) with no Gamma fetch.
func TestSnipeRegisterHeldForUserRegistersPositions(t *testing.T) {
	t.Parallel()
	watch := &recordingHeldWatch{renewResult: true} // already watched ⇒ renew path
	scanner := &fakePositionSource{positions: []*polymarket.Position{
		{TokenID: "tok-1", MarketID: "m-1", Outcome: "Yes"},
	}}
	b := &Bot{snipeWatcher: watch, snipePositions: scanner}

	b.snipeRegisterHeldForUser(7, common.HexToAddress("0xproxy"))

	if scanner.calls != 1 {
		t.Fatalf("scanner GetPositions calls = %d, want 1", scanner.calls)
	}
	got := watch.renewedTokens()
	if len(got) != 1 || got[0] != "tok-1" {
		t.Fatalf("held registration = %v, want [tok-1] (RenewHeldMarket renews the market)", got)
	}
}

// When the held token is NOT yet watched, registerSnipeHeld fetches its market
// and registers BOTH sides — the held token and its flip sibling (issue #78) —
// so a crash on the side the user does not hold still reaches them.
func TestSnipeRegisterHeldFansOutSiblings(t *testing.T) {
	t.Parallel()
	gamma := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/markets/157417" {
			fmt.Fprint(w,
				`{"id":"157417","question":"LoL: T1 vs. Gen.G","conditionId":"cond-1",`+
					`"outcomes":"[\"T1\",\"Gen.G\"]","clobTokenIds":"[\"tok-t1\",\"tok-geng\"]",`+
					`"gameStartTime":"2026-08-14T09:00:00Z"}`)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(gamma.Close)

	watch := &recordingHeldWatch{renewResult: false} // unwatched ⇒ fetch + fan out
	b := &Bot{snipeWatcher: watch, snipeMarkets: polymarket.NewMarketClientWithURL(gamma.URL)}

	// The user holds only tok-t1; the flip side tok-geng must ride along.
	b.registerSnipeHeld(7, []*polymarket.Position{{TokenID: "tok-t1", MarketID: "157417", Outcome: "T1"}})

	held := watch.heldTokens()
	if len(held) != 2 {
		t.Fatalf("WatchHeld tokens = %v, want both tok-t1 and tok-geng", held)
	}
	seen := map[string]bool{}
	for _, tok := range held {
		seen[tok] = true
	}
	if !seen["tok-t1"] || !seen["tok-geng"] {
		t.Errorf("held tokens = %v, want tok-t1 + sibling tok-geng", held)
	}
}

// With no watcher wired the hook is inert — it must not even fetch positions.
func TestSnipeRegisterHeldForUserNilWatcher(t *testing.T) {
	t.Parallel()
	scanner := &fakePositionSource{}
	b := &Bot{snipePositions: scanner}

	b.snipeRegisterHeldForUser(7, common.HexToAddress("0xproxy"))

	if scanner.calls != 0 {
		t.Errorf("scanner called with nil watcher (calls=%d)", scanner.calls)
	}
}
