package live

import (
	"sort"
	"sync"
)

// BookLevel represents a single (price, size) entry in a book side.
type BookLevel struct {
	Price float64
	Size  float64
}

// PriceChange is a single-level book delta. Size == 0 removes the level.
type PriceChange struct {
	Price float64
	Size  float64
	Side  string // "BUY" for bids, "SELL" for asks
}

// bookState maintains bid/ask maps for one tokenID.
// Safe for concurrent use.
type bookState struct {
	mu   sync.RWMutex
	bids map[float64]float64 // price -> size
	asks map[float64]float64
}

func newBookState() *bookState {
	return &bookState{
		bids: make(map[float64]float64),
		asks: make(map[float64]float64),
	}
}

// ApplyBook replaces the full book with a snapshot. Zero-size levels are dropped.
func (b *bookState) ApplyBook(bids, asks []BookLevel) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.bids = make(map[float64]float64, len(bids))
	for _, l := range bids {
		if l.Size > 0 {
			b.bids[l.Price] = l.Size
		}
	}
	b.asks = make(map[float64]float64, len(asks))
	for _, l := range asks {
		if l.Size > 0 {
			b.asks[l.Price] = l.Size
		}
	}
}

// ApplyPriceChange applies delta updates. Size <= 0 removes the level.
// Side is case-sensitive: expect "BUY" (bids) or "SELL" (asks).
func (b *bookState) ApplyPriceChange(changes []PriceChange) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, c := range changes {
		m := b.bids
		if c.Side == "SELL" {
			m = b.asks
		}
		if c.Size <= 0 {
			delete(m, c.Price)
		} else {
			m[c.Price] = c.Size
		}
	}
}

// BestBid returns the highest bid price with positive size.
func (b *bookState) BestBid() (float64, bool) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	var best float64
	found := false
	for price := range b.bids {
		if !found || price > best {
			best = price
			found = true
		}
	}
	return best, found
}

// SellVWAP returns the volume-weighted average price of selling `shares` into
// the bid side (a sell lifts the highest bids first) and the total bid depth
// available. It underpins the depth-aware fire confirm (issue #80): the fire
// triggers on one best-bid print, but the executable price is the VWAP down the
// book for the whole size being sold.
//
// ok=false ONLY when there is nothing to walk (empty bid side) — the confirm
// treats that as fail-open. When the book cannot cover `shares` (depth < shares)
// ok stays true and vwap is the average over the available TOP levels: an UPPER
// bound on the true full-size VWAP, since the unfilled remainder would only sell
// into still-lower bids. Callers that need a full fill compare depth to shares.
func (b *bookState) SellVWAP(shares float64) (vwap, depth float64, ok bool) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	if len(b.bids) == 0 {
		return 0, 0, false
	}
	prices := make([]float64, 0, len(b.bids))
	for p := range b.bids {
		prices = append(prices, p)
		depth += b.bids[p]
	}
	// Highest bid first — that is the order a market sell consumes liquidity.
	sort.Sort(sort.Reverse(sort.Float64Slice(prices)))
	var cost, filled, remaining float64
	remaining = shares
	for _, p := range prices {
		take := b.bids[p]
		if take > remaining {
			take = remaining
		}
		cost += take * p
		filled += take
		remaining -= take
		if remaining <= 0 {
			break
		}
	}
	if filled <= 0 {
		// shares <= 0, or nothing consumable — no meaningful VWAP.
		return 0, depth, false
	}
	return cost / filled, depth, true
}

// BestAsk returns the lowest ask price with positive size.
func (b *bookState) BestAsk() (float64, bool) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	var best float64
	found := false
	for price := range b.asks {
		if !found || price < best {
			best = price
			found = true
		}
	}
	return best, found
}
