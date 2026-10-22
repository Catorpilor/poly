package polymarket

import (
	"encoding/json"
	"math"
	"strings"
	"testing"
)

// Trimmed verbatim Gamma /markets/<id> rows (2026-10-22) for the legs of the
// first three real Combo Links.
const (
	gammaOver05  = `{"id":"5437373","question":"FC Bayern München vs. Arsenal FC: O/U 0.5","slug":"ucl-bay-ars-2026-10-21-total-0pt5","closed":true,"active":true,"acceptingOrders":false,"rfqEnabled":false,"comboStatus":"enabled","outcomes":"[\"Over\", \"Under\"]","outcomePrices":"[\"1\", \"0\"]","positionIds":["8404701338520000000000000000000000000000000000000000000000000000000000000000","8404701338520000000000000000000000000000000000000000000000000000000000000001"]}`
	gammaVilla31 = `{"id":"5436769","question":"Exact Score: Aston Villa FC 3 - 1 Viking FK?","slug":"ucl-ast-vf-2026-10-21-exact-score-3-1","closed":true,"active":true,"acceptingOrders":false,"rfqEnabled":false,"comboStatus":"enabled","outcomes":"[\"Yes\", \"No\"]","outcomePrices":"[\"1\", \"0\"]","positionIds":["1218627107890000000000000000000000000000000000000000000000000000000000000000","1218627107890000000000000000000000000000000000000000000000000000000000000001"]}`
	gammaReal31  = `{"id":"5436671","question":"Exact Score: Real Madrid CF 3 - 1 RB Leipzig?","slug":"ucl-rma-rbl-2026-10-21-exact-score-3-1","closed":true,"active":true,"acceptingOrders":false,"rfqEnabled":false,"comboStatus":"enabled","outcomes":"[\"Yes\", \"No\"]","outcomePrices":"[\"0\", \"1\"]","positionIds":["9189979930700000000000000000000000000000000000000000000000000000000000000000","9189979930700000000000000000000000000000000000000000000000000000000000000001"]}`
	// An open catalog market: rfqEnabled is false even though it is combo-able.
	gammaIranOpen = `{"id":"665374","question":"Will the U.S. invade Iran before 2027?","slug":"will-the-us-invade-iran-before-2027","closed":false,"active":true,"acceptingOrders":true,"rfqEnabled":false,"comboStatus":"enabled","outcomes":"[\"Yes\", \"No\"]","outcomePrices":"[\"0.155\", \"0.845\"]","positionIds":["7985599515345184796400000000000000000000000000000000000000000000000000000000","7985599515345184796400000000000000000000000000000000000000000000000000000001"]}`
)

func gammaFixture(t *testing.T, raw string) *GammaMarket {
	t.Helper()
	var m GammaMarket
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		t.Fatalf("fixture: %v", err)
	}
	return &m
}

func TestBuildComboLeg(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		raw         string
		outcome     int
		wantState   ComboLegState
		wantLabel   string
		wantPosSfx  string
		wantPrice   float64
		wantErrPart string
	}{
		{name: "y on O/U is Over, resolved won", raw: gammaOver05, outcome: 0, wantState: LegWon, wantLabel: "Over", wantPosSfx: "0", wantPrice: 1},
		{name: "n on O/U is Under, resolved lost", raw: gammaOver05, outcome: 1, wantState: LegLost, wantLabel: "Under", wantPosSfx: "1", wantPrice: 0},
		{name: "exact score yes lost", raw: gammaReal31, outcome: 0, wantState: LegLost, wantLabel: "Yes", wantPrice: 0},
		{name: "open catalog market despite rfqEnabled=false", raw: gammaIranOpen, outcome: 0, wantState: LegOpen, wantLabel: "Yes", wantPrice: 0.155},
		{name: "open market, second outcome", raw: gammaIranOpen, outcome: 1, wantState: LegOpen, wantLabel: "No", wantPosSfx: "1", wantPrice: 0.845},
		{
			name:      "combos disabled on an open market",
			raw:       strings.Replace(gammaIranOpen, `"comboStatus":"enabled"`, `"comboStatus":"disabled"`, 1),
			wantState: LegNotComboable, wantLabel: "Yes", wantPrice: 0.155,
		},
		{
			name:      "open but not accepting orders",
			raw:       strings.Replace(gammaIranOpen, `"acceptingOrders":true`, `"acceptingOrders":false`, 1),
			wantState: LegPaused, wantLabel: "Yes", wantPrice: 0.155,
		},
		{
			name:      "closed without a 0/1 price is closed, not won or lost",
			raw:       strings.Replace(strings.Replace(gammaIranOpen, `"closed":false`, `"closed":true`, 1), `\"0.155\", \"0.845\"`, `\"0.5\", \"0.5\"`, 1),
			wantState: LegClosed, wantLabel: "Yes", wantPrice: 0.5,
		},
		{
			name:        "no position ids",
			raw:         strings.Replace(gammaIranOpen, `"positionIds":["7985599515345184796400000000000000000000000000000000000000000000000000000000","7985599515345184796400000000000000000000000000000000000000000000000000000001"]`, `"positionIds":[]`, 1),
			wantErrPart: "no combo position ids",
		},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			m := gammaFixture(t, tt.raw)
			leg, err := BuildComboLeg(ComboLegRef{MarketID: m.ID, OutcomeIndex: tt.outcome}, m)
			if tt.wantErrPart != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErrPart) {
					t.Fatalf("err = %v, want containing %q", err, tt.wantErrPart)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if leg.State != tt.wantState {
				t.Errorf("state = %v, want %v", leg.State, tt.wantState)
			}
			if leg.OutcomeLabel != tt.wantLabel {
				t.Errorf("label = %q, want %q", leg.OutcomeLabel, tt.wantLabel)
			}
			if leg.Price != tt.wantPrice {
				t.Errorf("price = %v, want %v", leg.Price, tt.wantPrice)
			}
			if want := m.PositionIDs[tt.outcome]; leg.PositionID != want {
				t.Errorf("position id = %q, want positionIds[%d] = %q", leg.PositionID, tt.outcome, want)
			}
			if leg.Question != m.Question || leg.MarketSlug != m.Slug {
				t.Errorf("question/slug not carried: %+v", leg)
			}
		})
	}
}

func TestComboViewVerdicts(t *testing.T) {
	t.Parallel()

	leg := func(raw string, outcome int) ComboLeg {
		m := gammaFixture(t, raw)
		l, err := BuildComboLeg(ComboLegRef{MarketID: m.ID, OutcomeIndex: outcome}, m)
		if err != nil {
			t.Fatal(err)
		}
		return l
	}

	tests := []struct {
		name        string
		legs        []ComboLeg
		wantVerdict ComboVerdict
		wantBlocker int // 1-based leg number named by the verdict, 0 = none
	}{
		{name: "real link 1: both legs won", legs: []ComboLeg{leg(gammaOver05, 0), leg(gammaVilla31, 0)}, wantVerdict: ComboWon},
		{name: "real link 2: Real leg lost", legs: []ComboLeg{leg(gammaOver05, 0), leg(gammaReal31, 0)}, wantVerdict: ComboLost, wantBlocker: 2},
		{name: "one open, one finished won: not buyable", legs: []ComboLeg{leg(gammaIranOpen, 0), leg(gammaOver05, 0)}, wantVerdict: ComboUnavailable, wantBlocker: 2},
		{name: "lost beats unavailable", legs: []ComboLeg{leg(gammaIranOpen, 0), leg(gammaReal31, 0)}, wantVerdict: ComboLost, wantBlocker: 2},
		{name: "all open", legs: []ComboLeg{leg(gammaIranOpen, 0), leg(gammaIranOpen, 1)}, wantVerdict: ComboBuyable},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			v := ComboView{Legs: tt.legs}
			verdict, blocker := v.Verdict()
			if verdict != tt.wantVerdict || blocker != tt.wantBlocker {
				t.Errorf("verdict = %v (leg %d), want %v (leg %d)", verdict, blocker, tt.wantVerdict, tt.wantBlocker)
			}
		})
	}
}

func TestComboEstimate(t *testing.T) {
	t.Parallel()

	v := ComboView{Legs: []ComboLeg{
		{State: LegOpen, Price: 0.5},
		{State: LegOpen, Price: 0.4},
	}}
	price, mult, ok := v.Estimate()
	if !ok || math.Abs(price-0.2) > 1e-12 || math.Abs(mult-5) > 1e-9 {
		t.Errorf("estimate = %v x%v ok=%v, want 0.2 x5 true", price, mult, ok)
	}

	v.Legs[1].Price = 0
	if _, _, ok := v.Estimate(); ok {
		t.Error("a zero-priced leg must give no estimate")
	}
}
