package live

import (
	"math"
	"testing"
)

func TestBookState_ApplyBook(t *testing.T) {
	t.Parallel()
	b := newBookState()
	b.ApplyBook(
		[]BookLevel{{Price: 0.50, Size: 100}, {Price: 0.48, Size: 200}, {Price: 0.45, Size: 0}},
		[]BookLevel{{Price: 0.52, Size: 150}, {Price: 0.55, Size: 300}},
	)
	if got, ok := b.BestBid(); !ok || got != 0.50 {
		t.Errorf("BestBid = %v ok=%v, want 0.50 true", got, ok)
	}
	if got, ok := b.BestAsk(); !ok || got != 0.52 {
		t.Errorf("BestAsk = %v ok=%v, want 0.52 true", got, ok)
	}
	// Zero-size level should be dropped
	if _, exists := b.bids[0.45]; exists {
		t.Errorf("expected zero-size bid at 0.45 to be dropped")
	}
}

func TestBookState_ApplyBook_ReplacesPrevious(t *testing.T) {
	t.Parallel()
	b := newBookState()
	b.ApplyBook([]BookLevel{{Price: 0.3, Size: 100}}, []BookLevel{{Price: 0.4, Size: 100}})
	b.ApplyBook([]BookLevel{{Price: 0.2, Size: 50}}, []BookLevel{{Price: 0.5, Size: 50}})
	if got, _ := b.BestBid(); got != 0.2 {
		t.Errorf("after replace, BestBid = %v want 0.2", got)
	}
	if got, _ := b.BestAsk(); got != 0.5 {
		t.Errorf("after replace, BestAsk = %v want 0.5", got)
	}
}

func TestBookState_ApplyPriceChange(t *testing.T) {
	t.Parallel()
	b := newBookState()
	b.ApplyBook(
		[]BookLevel{{Price: 0.50, Size: 100}, {Price: 0.48, Size: 200}},
		[]BookLevel{{Price: 0.52, Size: 150}},
	)
	// Add a higher bid
	b.ApplyPriceChange([]PriceChange{{Price: 0.51, Size: 75, Side: "BUY"}})
	if got, _ := b.BestBid(); got != 0.51 {
		t.Errorf("after add, BestBid = %v want 0.51", got)
	}
	// Remove the new top bid via size=0
	b.ApplyPriceChange([]PriceChange{{Price: 0.51, Size: 0, Side: "BUY"}})
	if got, _ := b.BestBid(); got != 0.50 {
		t.Errorf("after remove, BestBid = %v want 0.50", got)
	}
	// Update an existing bid size
	b.ApplyPriceChange([]PriceChange{{Price: 0.50, Size: 10, Side: "BUY"}})
	if b.bids[0.50] != 10 {
		t.Errorf("expected bid 0.50 resized to 10, got %v", b.bids[0.50])
	}
	// Ask side delta
	b.ApplyPriceChange([]PriceChange{{Price: 0.51, Size: 50, Side: "SELL"}})
	if got, _ := b.BestAsk(); got != 0.51 {
		t.Errorf("after sell delta, BestAsk = %v want 0.51", got)
	}
}

func TestBookState_Empty(t *testing.T) {
	t.Parallel()
	b := newBookState()
	if _, ok := b.BestBid(); ok {
		t.Error("empty book should return ok=false for BestBid")
	}
	if _, ok := b.BestAsk(); ok {
		t.Error("empty book should return ok=false for BestAsk")
	}
}

func TestBookState_ApplyBook_SkipsNegativeSize(t *testing.T) {
	t.Parallel()
	b := newBookState()
	b.ApplyBook([]BookLevel{{Price: 0.5, Size: -1}, {Price: 0.4, Size: 100}}, nil)
	if got, _ := b.BestBid(); got != 0.4 {
		t.Errorf("negative size should be dropped, BestBid = %v want 0.4", got)
	}
}

// TestBookState_SellVWAP is the issue #80 book-walking accessor: the executable
// VWAP of selling N shares into the bid side, best (highest) bid first, plus the
// total bid depth. ok=false ONLY when the bid side is empty — a partial fill
// (depth < shares) still returns ok=true with a VWAP over the available top
// levels, which is an upper bound on the true full-size VWAP.
func TestBookState_SellVWAP(t *testing.T) {
	t.Parallel()
	// Bids 0.50×100, 0.48×200, 0.45×300 (total depth 600). Map-order shuffled to
	// prove the walk sorts by price, not insertion order.
	full := []BookLevel{{Price: 0.45, Size: 300}, {Price: 0.50, Size: 100}, {Price: 0.48, Size: 200}}
	cases := []struct {
		name      string
		bids      []BookLevel
		shares    float64
		wantVWAP  float64
		wantDepth float64
		wantOK    bool
	}{
		{"single top level", full, 100, 0.50, 600, true},
		{"spans two levels", full, 300, (0.50*100 + 0.48*200) / 300, 600, true},
		{"exact-depth boundary", full, 600, (0.50*100 + 0.48*200 + 0.45*300) / 600, 600, true},
		// Insufficient depth: consumes the whole book, VWAP over all 600 shares,
		// ok=true (upper bound), depth reports the 600 actually available.
		{"insufficient depth", full, 700, (0.50*100 + 0.48*200 + 0.45*300) / 600, 600, true},
		{"empty book", nil, 100, 0, 0, false},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			b := newBookState()
			b.ApplyBook(tc.bids, nil)
			vwap, depth, ok := b.SellVWAP(tc.shares)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tc.wantOK)
			}
			if math.Abs(vwap-tc.wantVWAP) > 1e-9 {
				t.Errorf("vwap = %v, want %v", vwap, tc.wantVWAP)
			}
			if math.Abs(depth-tc.wantDepth) > 1e-9 {
				t.Errorf("depth = %v, want %v", depth, tc.wantDepth)
			}
		})
	}
}
