package telegram

import (
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
func (c *capturingHeldWatch) RenewHeld(int64, string, time.Duration) bool {
	return false
}
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

// TestSnipeRegisterBoughtToken: the helper registers the bought token as a Held
// Watch directly from the Gamma market, lag-free (issue #67).
func TestSnipeRegisterBoughtToken(t *testing.T) {
	t.Parallel()
	wantStart := boughtTokenMarket().GetGameStartTime()
	if wantStart.IsZero() {
		t.Fatal("fixture gameStartTime did not parse — test would be meaningless")
	}

	tests := []struct {
		name        string
		idx         int
		wantToken   string
		wantOutcome string
	}{
		{"index 0 registers T1", 0, "tok-t1", "T1"},
		{"index 1 registers Gen.G", 1, "tok-geng", "Gen.G"},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			watch := &capturingHeldWatch{}
			b := &Bot{snipeWatcher: watch}

			b.snipeRegisterBoughtToken(7, boughtTokenMarket(), tt.idx)

			calls := watch.heldCalls()
			if len(calls) != 1 {
				t.Fatalf("WatchHeld calls = %d, want 1", len(calls))
			}
			got := calls[0]
			if got.chatID != 7 {
				t.Errorf("chatID = %d, want 7", got.chatID)
			}
			if got.ttl != live.SnipeHeldTTL {
				t.Errorf("ttl = %v, want %v", got.ttl, live.SnipeHeldTTL)
			}
			if got.market.TokenID != tt.wantToken {
				t.Errorf("TokenID = %q, want %q", got.market.TokenID, tt.wantToken)
			}
			if got.market.Outcome != tt.wantOutcome {
				t.Errorf("Outcome = %q, want %q", got.market.Outcome, tt.wantOutcome)
			}
			if got.market.MarketID != "157417" {
				t.Errorf("MarketID = %q, want 157417", got.market.MarketID)
			}
			if got.market.Question != "LoL: T1 vs. Gen.G" {
				t.Errorf("Question = %q, want the market question", got.market.Question)
			}
			if !got.market.GameStart.Equal(wantStart) {
				t.Errorf("GameStart = %v, want %v", got.market.GameStart, wantStart)
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
