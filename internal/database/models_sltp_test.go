package database

import (
	"strings"
	"testing"
)

func TestSLTPArm_TPTriggerPrice(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		avgPrice float64
		tickSize float64
		want     float64
	}{
		{"normal entry doubles", 0.20, 0.01, 0.40},
		{"low entry doubles fine", 0.05, 0.01, 0.10},
		{"mid entry caps at 0.99", 0.50, 0.01, 0.99},
		{"high entry caps at 0.99", 0.80, 0.01, 0.99},
		{"exactly half just below cap", 0.495, 0.01, 0.99},
		// issue #25: entry 0.2355 doubles to 0.471, which a 0.01-tick book
		// can never print — the bid peaked at exactly 0.47 in production and
		// the TP never fired. The trigger must floor to the tick grid.
		{"off-grid double floors to tick (production case)", 0.2355, 0.01, 0.47},
		{"on-grid double unchanged", 0.34, 0.01, 0.68},
		{"finer grid keeps 0.471", 0.2355, 0.001, 0.471},
		// 0.47/0.01 evaluates just below 47.0 in float64; the 1e-6 epsilon
		// must keep an exactly-on-grid trigger from flooring a full tick down.
		{"float artifact stays on grid", 0.235, 0.01, 0.47},
		{"cap applies before flooring", 0.60, 0.01, 0.99},
		{"tiny entry clamps to one tick", 0.003, 0.01, 0.01},
		{"zero tick falls back to 0.01 grid", 0.2355, 0, 0.47},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			a := &SLTPArm{AvgPrice: tt.avgPrice, TickSize: tt.tickSize}
			got := a.TPTriggerPrice()
			if diff := got - tt.want; diff > 1e-9 || diff < -1e-9 {
				t.Errorf("TPTriggerPrice(avg=%v, tick=%v) = %v, want %v", tt.avgPrice, tt.tickSize, got, tt.want)
			}
		})
	}
}

// TestSLTPArm_IsDeepEntry pins the ladder-membership boundary (issue #81):
// entry ≤ 0.05 qualifies (strict ≤ — 0.05 itself is in), 0.0501 doesn't, and a
// zero-value arm is never deep.
func TestSLTPArm_IsDeepEntry(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		avgPrice float64
		want     bool
	}{
		{"entry exactly 0.05 qualifies", 0.05, true},
		{"entry just above 0.05 does not", 0.0501, false},
		{"deep entry 0.02 qualifies", 0.02, true},
		{"one-cent entry qualifies", 0.01, true},
		{"typical entry 0.20 does not", 0.20, false},
		{"zero-value arm is not deep", 0, false},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			a := &SLTPArm{AvgPrice: tt.avgPrice}
			if got := a.IsDeepEntry(); got != tt.want {
				t.Errorf("IsDeepEntry(avg=%v) = %v, want %v", tt.avgPrice, got, tt.want)
			}
		})
	}
}

// TestSLTPArm_DeepEntryRungPrice pins the four rung triggers off a 0.05 entry,
// each entry × multiple floored to the tick grid.
func TestSLTPArm_DeepEntryRungPrice(t *testing.T) {
	t.Parallel()
	a := &SLTPArm{AvgPrice: 0.05, TickSize: 0.01}
	wants := []float64{0.10, 0.15, 0.20, 0.25} // 2×,3×,4×,5×
	for i, want := range wants {
		if got := a.DeepEntryRungPrice(i); diffTooBig(got, want) {
			t.Errorf("DeepEntryRungPrice(%d) = %v, want %v", i, got, want)
		}
	}
}

// TestSLTPArm_DeepEntryRungsCrossed covers the prefix-count semantics including
// a gap tick that crosses several rungs at once.
func TestSLTPArm_DeepEntryRungsCrossed(t *testing.T) {
	t.Parallel()
	a := &SLTPArm{AvgPrice: 0.05, TickSize: 0.01} // rungs at 0.10/0.15/0.20/0.25
	tests := []struct {
		name string
		bid  float64
		want int
	}{
		{"below first rung", 0.09, 0},
		{"exactly first rung", 0.10, 1},
		{"between rung 1 and 2", 0.14, 1},
		{"gap straight to rung 3 crosses 1-3", 0.20, 3},
		{"at/above top rung crosses all four", 0.25, 4},
		{"far above top rung still four", 0.80, 4},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := a.DeepEntryRungsCrossed(tt.bid); got != tt.want {
				t.Errorf("DeepEntryRungsCrossed(%v) = %d, want %d", tt.bid, got, tt.want)
			}
		})
	}
}

// TestDeepEntryLadderFractions pins the cumulative fraction math: the full
// ladder banks 75% and leaves ~25% to ride, and a between-rungs slice sums only
// the rungs it spans.
func TestDeepEntryLadderFractions(t *testing.T) {
	t.Parallel()
	if got := DeepEntryLadderSoldFraction(0); diffTooBig(got, 0) {
		t.Errorf("sold(0) = %v, want 0", got)
	}
	if got := DeepEntryLadderSoldFraction(1); diffTooBig(got, 0.25) {
		t.Errorf("sold(1) = %v, want 0.25", got)
	}
	if got := DeepEntryLadderSoldFraction(4); diffTooBig(got, 0.75) {
		t.Errorf("sold(4) = %v, want 0.75 (25%% rides to the ceiling)", got)
	}
	// A gap that fires rungs 2 and 3 (indices 1,2) sells 20%+15% = 35%.
	if got := DeepEntryLadderFractionBetween(1, 3); diffTooBig(got, 0.35) {
		t.Errorf("between(1,3) = %v, want 0.35", got)
	}
	// Out-of-range `to` is clamped to the ladder length.
	if got := DeepEntryLadderFractionBetween(0, 99); diffTooBig(got, 0.75) {
		t.Errorf("between(0,99) = %v, want 0.75", got)
	}
}

// TestSLTPArm_RemainderShares proves the frozen-basis remainder is byte-identical
// for non-deep arms and follows the ladder base for deep ones.
func TestSLTPArm_RemainderShares(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		arm  SLTPArm
		want float64
	}{
		{"non-deep TP not fired: full snapshot", SLTPArm{AvgPrice: 0.20, SharesAtArm: 100, TPArmed: true}, 100},
		{"non-deep TP fired: snapshot minus 25%", SLTPArm{AvgPrice: 0.20, SharesAtArm: 100, TPArmed: false}, 75},
		{"deep no rung fired: full snapshot", SLTPArm{AvgPrice: 0.05, SharesAtArm: 100, TPArmed: true}, 100},
		{"deep after 1 rung: base minus 25%", SLTPArm{AvgPrice: 0.05, SharesAtArm: 100, TPArmed: true, LadderRungsFired: 1, LadderBaseShares: 100}, 75},
		{"deep after 4 rungs: base minus 75%", SLTPArm{AvgPrice: 0.05, SharesAtArm: 100, TPArmed: true, LadderRungsFired: 4, LadderBaseShares: 100}, 25},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := tt.arm.RemainderShares(); diffTooBig(got, tt.want) {
				t.Errorf("RemainderShares() = %v, want %v", got, tt.want)
			}
		})
	}
}

func diffTooBig(got, want float64) bool {
	d := got - want
	return d > 1e-9 || d < -1e-9
}

func TestSLTPArm_SLActive(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		avgPrice float64
		hwm      float64
		want     bool
	}{
		{"dormant at seed (hwm == avg)", 0.50, 0.50, false},
		{"dormant just below activation", 0.50, 0.599, false},
		{"active exactly at avg*1.20", 0.50, 0.60, true},
		{"active well above activation", 0.50, 0.90, true},
		{"low entry activates at 0.24", 0.20, 0.24, true},
		{"low entry dormant at 0.23", 0.20, 0.23, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			a := &SLTPArm{AvgPrice: tt.avgPrice, HighWaterMark: tt.hwm}
			if got := a.SLActive(); got != tt.want {
				t.Errorf("SLActive(avg=%v, hwm=%v) = %v, want %v", tt.avgPrice, tt.hwm, got, tt.want)
			}
		})
	}
}

func TestSLTPArm_SLTriggerPrice(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		avgPrice float64
		hwm      float64
		want     float64
	}{
		{"hwm at entry floors at entry", 0.50, 0.50, 0.50},
		{"hwm*trail below entry floors at entry", 0.50, 0.60, 0.50},
		{"hwm*trail above entry trails", 0.50, 0.70, 0.56},
		{"high peak ratchets trigger up", 0.50, 0.90, 0.72},
		{"low entry trails from peak", 0.20, 0.30, 0.24},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			a := &SLTPArm{AvgPrice: tt.avgPrice, HighWaterMark: tt.hwm}
			got := a.SLTriggerPrice()
			// Floating-point tolerance
			if diff := got - tt.want; diff > 1e-9 || diff < -1e-9 {
				t.Errorf("SLTriggerPrice(avg=%v, hwm=%v) = %v, want %v", tt.avgPrice, tt.hwm, got, tt.want)
			}
		})
	}
}

func TestSLTPArm_SLFloorPrice(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		avgPrice float64
		hwm      float64
		want     float64
	}{
		{"floor is 90% of breakeven trigger", 0.50, 0.50, 0.45},
		{"floor is 90% of trailing trigger", 0.50, 0.90, 0.648},
		{"penny arm clamps at 0.001", 0.001, 0.001, 0.001},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			a := &SLTPArm{AvgPrice: tt.avgPrice, HighWaterMark: tt.hwm}
			got := a.SLFloorPrice()
			if diff := got - tt.want; diff > 1e-9 || diff < -1e-9 {
				t.Errorf("SLFloorPrice(avg=%v, hwm=%v) = %v, want %v", tt.avgPrice, tt.hwm, got, tt.want)
			}
		})
	}
}

func TestSLTPArm_Validate(t *testing.T) {
	t.Parallel()
	valid := SLTPArm{
		TelegramID:  123,
		TokenID:     "12345",
		ConditionID: "0xabc",
		Outcome:     OutcomeYes,
		AvgPrice:    0.25,
		SharesAtArm: 100,
	}

	tests := []struct {
		name    string
		mutate  func(a *SLTPArm)
		wantErr string
	}{
		{"valid passes", func(a *SLTPArm) {}, ""},
		{"missing telegram id", func(a *SLTPArm) { a.TelegramID = 0 }, "telegram_id"},
		{"missing token id", func(a *SLTPArm) { a.TokenID = "" }, "token_id"},
		{"missing condition id", func(a *SLTPArm) { a.ConditionID = "" }, "condition_id"},
		{"avg price zero", func(a *SLTPArm) { a.AvgPrice = 0 }, "avg_price"},
		{"avg price above 1", func(a *SLTPArm) { a.AvgPrice = 1.5 }, "avg_price"},
		{"shares zero", func(a *SLTPArm) { a.SharesAtArm = 0 }, "shares_at_arm"},
		{"shares negative", func(a *SLTPArm) { a.SharesAtArm = -1 }, "shares_at_arm"},
		{"empty outcome rejected", func(a *SLTPArm) { a.Outcome = Outcome("") }, "outcome"},
		// Sports/esports/election markets have team/candidate names as the
		// outcome string ("WEIBO GAMING", "DN SOOPERS", "Knicks"). These
		// must arm — token_id is the canonical key for SL/TP, outcome is
		// just display metadata. Strict YES/NO validation here masked
		// real positions on multi-outcome markets.
		{"non-binary outcome (esports team) accepted", func(a *SLTPArm) { a.Outcome = Outcome("WEIBO GAMING") }, ""},
		{"non-binary outcome (sports team) accepted", func(a *SLTPArm) { a.Outcome = Outcome("KNICKS") }, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			a := valid
			tt.mutate(&a)
			err := a.Validate()
			if tt.wantErr == "" {
				if err != nil {
					t.Errorf("unexpected error: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("error = %v, want containing %q", err, tt.wantErr)
			}
		})
	}
}
