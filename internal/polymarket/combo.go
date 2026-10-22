package polymarket

import (
	"fmt"
	"strconv"
)

// ComboLegState is a Leg's availability for a new Combo.
type ComboLegState int

const (
	// LegOpen: the market is live, accepting orders and combo-enabled.
	LegOpen ComboLegState = iota
	// LegWon / LegLost: the market resolved; the leg's outcome paid 1 / 0.
	LegWon
	LegLost
	// LegClosed: closed without a definite 0/1 price (e.g. resolving).
	LegClosed
	// LegPaused: open but not accepting orders right now.
	LegPaused
	// LegNotComboable: the market is not enabled for Combos.
	LegNotComboable
)

func (s ComboLegState) String() string {
	switch s {
	case LegOpen:
		return "open"
	case LegWon:
		return "won"
	case LegLost:
		return "lost"
	case LegClosed:
		return "closed"
	case LegPaused:
		return "paused"
	case LegNotComboable:
		return "not combo-enabled"
	}
	return fmt.Sprintf("ComboLegState(%d)", int(s))
}

// ComboLeg is one resolved Leg of a Combo Link.
type ComboLeg struct {
	Ref          ComboLegRef
	Question     string
	MarketSlug   string
	EventSlug    string
	OutcomeLabel string  // the market's own label for Ref.OutcomeIndex ("Over", "Yes", a team)
	PositionID   string  // leg position id for RFQ (positionIds[idx]); not a CLOB token id
	Price        float64 // Gamma outcome price of the taken outcome
	State        ComboLegState
}

// BuildComboLeg resolves a Combo Link leg against its Gamma market.
//
// Availability is comboStatus == "enabled" && !closed && acceptingOrders.
// rfqEnabled is deliberately ignored: it is false on open combo-catalog
// markets too (issue #118).
func BuildComboLeg(ref ComboLegRef, m *GammaMarket) (ComboLeg, error) {
	if m == nil {
		return ComboLeg{}, fmt.Errorf("market %s not found", ref.MarketID)
	}
	if ref.OutcomeIndex < 0 || ref.OutcomeIndex > 1 {
		return ComboLeg{}, fmt.Errorf("market %s: outcome index %d out of range", ref.MarketID, ref.OutcomeIndex)
	}
	if len(m.PositionIDs) != 2 {
		return ComboLeg{}, fmt.Errorf("market %s has no combo position ids", ref.MarketID)
	}
	outcomes := m.GetOutcomes()
	if len(outcomes) != 2 {
		return ComboLeg{}, fmt.Errorf("market %s is not a two-outcome market", ref.MarketID)
	}

	price := 0.0
	if prices := m.GetOutcomePrices(); len(prices) == 2 {
		price, _ = strconv.ParseFloat(prices[ref.OutcomeIndex], 64)
	}

	leg := ComboLeg{
		Ref:          ref,
		Question:     m.Question,
		MarketSlug:   m.Slug,
		EventSlug:    parentEventSlug(m),
		OutcomeLabel: outcomes[ref.OutcomeIndex],
		PositionID:   m.PositionIDs[ref.OutcomeIndex],
		Price:        price,
	}

	switch {
	case m.Closed && price == 1:
		leg.State = LegWon
	case m.Closed && price == 0:
		leg.State = LegLost
	case m.Closed:
		leg.State = LegClosed
	case m.ComboStatus != "enabled":
		leg.State = LegNotComboable
	case !m.AcceptingOrders:
		leg.State = LegPaused
	default:
		leg.State = LegOpen
	}
	return leg, nil
}

// parentEventSlug is the market's real parent event slug, or "". Unlike
// GetEventSlug it never falls back to the market slug, which would make a
// broken /event/<market-slug> link.
func parentEventSlug(m *GammaMarket) string {
	if len(m.Events) > 0 {
		return m.Events[0].Slug
	}
	return ""
}

// ComboVerdict is whether a Combo Link can be bought, and if not, why.
type ComboVerdict int

const (
	// ComboBuyable: every leg is open.
	ComboBuyable ComboVerdict = iota
	// ComboLost: a leg already lost — this combo can never pay.
	ComboLost
	// ComboWon: every leg already won — nothing left to bet on.
	ComboWon
	// ComboUnavailable: some leg is finished, paused or not combo-enabled.
	ComboUnavailable
)

func (v ComboVerdict) String() string {
	switch v {
	case ComboBuyable:
		return "buyable"
	case ComboLost:
		return "lost"
	case ComboWon:
		return "won"
	case ComboUnavailable:
		return "unavailable"
	}
	return fmt.Sprintf("ComboVerdict(%d)", int(v))
}

// ComboView is a Combo Link's legs, resolved.
type ComboView struct {
	Legs []ComboLeg
}

// Verdict reports whether the combo can be bought. For a non-buyable combo
// it also names the first blocking leg (1-based): the first lost leg for
// ComboLost, the first non-open leg for ComboUnavailable, 0 otherwise.
func (v ComboView) Verdict() (ComboVerdict, int) {
	for i, l := range v.Legs {
		if l.State == LegLost {
			return ComboLost, i + 1
		}
	}
	allWon := len(v.Legs) > 0
	for _, l := range v.Legs {
		if l.State != LegWon {
			allWon = false
			break
		}
	}
	if allWon {
		return ComboWon, 0
	}
	for i, l := range v.Legs {
		if l.State != LegOpen {
			return ComboUnavailable, i + 1
		}
	}
	return ComboBuyable, 0
}

// LegPositionIDs returns the leg position ids in link order, for an RFQ.
func (v ComboView) LegPositionIDs() []string {
	ids := make([]string, len(v.Legs))
	for i, l := range v.Legs {
		ids[i] = l.PositionID
	}
	return ids
}

// Estimate is the Combo Estimate: the product of the legs' prices and its
// Multiplier (1/product). Indicative only — before fees and maker margin.
// ok is false when any leg price is not in (0, 1].
func (v ComboView) Estimate() (price, multiplier float64, ok bool) {
	if len(v.Legs) == 0 {
		return 0, 0, false
	}
	price = 1
	for _, l := range v.Legs {
		if l.Price <= 0 || l.Price > 1 {
			return 0, 0, false
		}
		price *= l.Price
	}
	return price, 1 / price, true
}
