package telegram

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"

	"github.com/Catorpilor/poly/internal/live"
	"github.com/Catorpilor/poly/internal/polymarket"
)

// recordingHeldWatch records the held-registration calls the buy-success hook
// drives. RenewHeld returns true so registerSnipeHeld takes the renew branch
// and never reaches the Gamma market fetch (no external call in tests).
type recordingHeldWatch struct {
	mu      sync.Mutex
	renewed []string
	held    []string
}

func (r *recordingHeldWatch) WatchArmed(live.SnipeMarket)   {}
func (r *recordingHeldWatch) UnwatchArmed(string)           {}
func (r *recordingHeldWatch) MarkBought(string)             {}
func (r *recordingHeldWatch) WatchHeld(_ int64, m live.SnipeMarket, _ time.Duration) {
	r.mu.Lock()
	r.held = append(r.held, m.TokenID)
	r.mu.Unlock()
}
func (r *recordingHeldWatch) RenewHeld(_ int64, tokenID string, _ time.Duration) bool {
	r.mu.Lock()
	r.renewed = append(r.renewed, tokenID)
	r.mu.Unlock()
	return true
}

func (r *recordingHeldWatch) renewedTokens() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.renewed...)
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
// Held Watch (issue #64), off the Data API by way of the injected scanner.
func TestSnipeRegisterHeldForUserRegistersPositions(t *testing.T) {
	t.Parallel()
	watch := &recordingHeldWatch{}
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
		t.Fatalf("held registration = %v, want [tok-1]", got)
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
