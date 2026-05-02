package database

import (
	"strings"
	"testing"
)

// Order.Validate and PriceAlert.Validate previously rejected any outcome
// other than YES/NO. Polymarket markets are binary at the contract level
// but the *display* outcome is the user-facing name — team or candidate
// names for sports/esports/elections. token_id is the canonical key for
// both orders and alerts; outcome is display metadata. Validation should
// only require it be non-empty. (Same fix that lifted the SL/TP arm bug
// for "WEIBO GAMING" / "DN SOOPERS" — applied here preventively before
// these code paths hit the same incompatibility in production.)

func TestOrder_Validate_AcceptsNonBinaryOutcomes(t *testing.T) {
	t.Parallel()
	valid := Order{
		Size:    1,
		Price:   0.5,
		Side:    OrderSideBuy,
		Outcome: OutcomeYes,
	}

	tests := []struct {
		name    string
		mutate  func(o *Order)
		wantErr string
	}{
		{"valid YES passes", func(o *Order) {}, ""},
		{"NO passes", func(o *Order) { o.Outcome = OutcomeNo }, ""},
		{"esports team accepted", func(o *Order) { o.Outcome = Outcome("WEIBO GAMING") }, ""},
		{"sports team accepted", func(o *Order) { o.Outcome = Outcome("KNICKS") }, ""},
		{"empty outcome rejected", func(o *Order) { o.Outcome = Outcome("") }, "outcome"},
		{"bad side still rejected", func(o *Order) { o.Side = OrderSide("MAYBE") }, "side"},
		{"bad price still rejected", func(o *Order) { o.Price = 2 }, "price"},
		{"bad size still rejected", func(o *Order) { o.Size = 0 }, "size"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			o := valid
			tt.mutate(&o)
			err := o.Validate()
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

func TestPriceAlert_Validate_AcceptsNonBinaryOutcomes(t *testing.T) {
	t.Parallel()
	valid := PriceAlert{
		PriceThreshold: 0.5,
		AlertType:      AlertTypeAbove,
		Outcome:        OutcomeYes,
	}

	tests := []struct {
		name    string
		mutate  func(p *PriceAlert)
		wantErr string
	}{
		{"valid YES passes", func(p *PriceAlert) {}, ""},
		{"NO passes", func(p *PriceAlert) { p.Outcome = OutcomeNo }, ""},
		{"esports team accepted", func(p *PriceAlert) { p.Outcome = Outcome("WEIBO GAMING") }, ""},
		{"candidate name accepted", func(p *PriceAlert) { p.Outcome = Outcome("DN SOOPERS") }, ""},
		{"empty outcome rejected", func(p *PriceAlert) { p.Outcome = Outcome("") }, "outcome"},
		{"bad alert type still rejected", func(p *PriceAlert) { p.AlertType = AlertType("MAYBE") }, "alert type"},
		{"bad price still rejected", func(p *PriceAlert) { p.PriceThreshold = 2 }, "price"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			p := valid
			tt.mutate(&p)
			err := p.Validate()
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
