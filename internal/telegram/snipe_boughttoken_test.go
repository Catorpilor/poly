package telegram

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/Catorpilor/poly/internal/live"
	"github.com/Catorpilor/poly/internal/polymarket"
)

// capturingHeldWatch records the full WatchHeld call (chatID, market, ttl) and
// flags any MarkBought — the direct-register helper must never latch bought.
type capturingHeldWatch struct {
	mu     sync.Mutex
	calls  []heldWatchCall
	bought []string
}

type heldWatchCall struct {
	chatID int64
	market live.SnipeMarket
	ttl    time.Duration
}

func (c *capturingHeldWatch) WatchArmed(live.SnipeMarket) {}
func (c *capturingHeldWatch) UnwatchArmed(string)         {}
func (c *capturingHeldWatch) RenewHeldMarket(int64, string, time.Duration) bool {
	return false
}
func (x *capturingHeldWatch) EventSlugOf(string) string { return "" }
func (c *capturingHeldWatch) WatchHeld(chatID int64, m live.SnipeMarket, ttl time.Duration) {
	c.mu.Lock()
	c.calls = append(c.calls, heldWatchCall{chatID: chatID, market: m, ttl: ttl})
	c.mu.Unlock()
}
func (c *capturingHeldWatch) MarkBought(tokenID string) {
	c.mu.Lock()
	c.bought = append(c.bought, tokenID)
	c.mu.Unlock()
}
func (c *capturingHeldWatch) SiblingTokenIDs(_, _ string) []string { return nil }

func (c *capturingHeldWatch) heldCalls() []heldWatchCall {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]heldWatchCall(nil), c.calls...)
}

// boughtTokenMarket is a Gamma market with two CLOB tokens, outcomes, and a
// game start — the shape a numeric-ID fetch returns.
func boughtTokenMarket() *polymarket.GammaMarket {
	return &polymarket.GammaMarket{
		ID:               "157417",
		Question:         "LoL: T1 vs. Gen.G",
		OutcomesRaw:      `["T1","Gen.G"]`,
		ClobTokenIdsRaw:  `["tok-t1","tok-geng"]`,
		GameStartTimeRaw: "2026-08-14 09:00:00+00",
	}
}

// TestSnipeRegisterBoughtToken: the helper registers the bought token AND its
// flip sibling as Held Watches directly from the Gamma market, lag-free (issue
// #67) — the sibling watch (issue #78) means both sides enter the watcher.
func TestSnipeRegisterBoughtToken(t *testing.T) {
	t.Parallel()
	wantStart := boughtTokenMarket().GetGameStartTime()
	if wantStart.IsZero() {
		t.Fatal("fixture gameStartTime did not parse — test would be meaningless")
	}

	// Either bought index registers BOTH tokens of the market.
	for _, idx := range []int{0, 1} {
		idx := idx
		t.Run(fmt.Sprintf("bought idx %d registers both sides", idx), func(t *testing.T) {
			t.Parallel()
			watch := &capturingHeldWatch{}
			b := &Bot{snipeWatcher: watch}

			b.snipeRegisterBoughtToken(7, boughtTokenMarket(), idx)

			calls := watch.heldCalls()
			if len(calls) != 2 {
				t.Fatalf("WatchHeld calls = %d, want 2 (both sides)", len(calls))
			}
			byToken := map[string]heldWatchCall{}
			for _, c := range calls {
				byToken[c.market.TokenID] = c
			}
			for _, want := range []struct{ token, outcome string }{
				{"tok-t1", "T1"}, {"tok-geng", "Gen.G"},
			} {
				got, ok := byToken[want.token]
				if !ok {
					t.Fatalf("token %q not registered; got %v", want.token, calls)
				}
				if got.chatID != 7 {
					t.Errorf("%s chatID = %d, want 7", want.token, got.chatID)
				}
				if got.ttl != live.SnipeHeldTTL {
					t.Errorf("%s ttl = %v, want %v", want.token, got.ttl, live.SnipeHeldTTL)
				}
				if got.market.Outcome != want.outcome {
					t.Errorf("%s Outcome = %q, want %q", want.token, got.market.Outcome, want.outcome)
				}
				if got.market.MarketID != "157417" {
					t.Errorf("%s MarketID = %q, want 157417", want.token, got.market.MarketID)
				}
				if !got.market.GameStart.Equal(wantStart) {
					t.Errorf("%s GameStart = %v, want %v", want.token, got.market.GameStart, wantStart)
				}
			}
			if len(watch.bought) != 0 {
				t.Errorf("MarkBought called %d time(s), want 0 — a manual buy is not a snipe fill", len(watch.bought))
			}
		})
	}
}

// TestSnipeRegisterBoughtTokenNoOps: guarded conditions must not register
// anything (and must not panic).
func TestSnipeRegisterBoughtTokenNoOps(t *testing.T) {
	t.Parallel()

	t.Run("nil watcher is a no-op", func(t *testing.T) {
		t.Parallel()
		b := &Bot{} // no watcher wired
		b.snipeRegisterBoughtToken(7, boughtTokenMarket(), 0)
		// Reaching here without a panic is the assertion.
	})

	t.Run("nil market is a no-op", func(t *testing.T) {
		t.Parallel()
		watch := &capturingHeldWatch{}
		b := &Bot{snipeWatcher: watch}
		b.snipeRegisterBoughtToken(7, nil, 0)
		if len(watch.heldCalls()) != 0 {
			t.Error("nil market registered a watch")
		}
	})

	t.Run("out-of-range index is a no-op", func(t *testing.T) {
		t.Parallel()
		for _, idx := range []int{-1, 2, 99} {
			watch := &capturingHeldWatch{}
			b := &Bot{snipeWatcher: watch}
			b.snipeRegisterBoughtToken(7, boughtTokenMarket(), idx)
			if len(watch.heldCalls()) != 0 {
				t.Errorf("idx %d registered a watch, want no-op", idx)
			}
		}
	})

	t.Run("missing token IDs is a no-op", func(t *testing.T) {
		t.Parallel()
		watch := &capturingHeldWatch{}
		b := &Bot{snipeWatcher: watch}
		m := boughtTokenMarket()
		m.ClobTokenIdsRaw = "" // no clobTokenIds in the response
		b.snipeRegisterBoughtToken(7, m, 0)
		if len(watch.heldCalls()) != 0 {
			t.Error("missing token IDs registered a watch")
		}
	})

	t.Run("empty token at index is a no-op", func(t *testing.T) {
		t.Parallel()
		watch := &capturingHeldWatch{}
		b := &Bot{snipeWatcher: watch}
		m := boughtTokenMarket()
		m.ClobTokenIdsRaw = `["",""]`
		b.snipeRegisterBoughtToken(7, m, 0)
		if len(watch.heldCalls()) != 0 {
			t.Error("empty token id registered a watch")
		}
	})
}
