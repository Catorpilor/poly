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
		want     float64
	}{
		{"normal entry doubles", 0.20, 0.40},
		{"low entry doubles fine", 0.05, 0.10},
		{"mid entry caps at 0.99", 0.50, 0.99},
		{"high entry caps at 0.99", 0.80, 0.99},
		{"exactly half just below cap", 0.495, 0.99},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			a := &SLTPArm{AvgPrice: tt.avgPrice}
			got := a.TPTriggerPrice()
			if diff := got - tt.want; diff > 1e-9 || diff < -1e-9 {
				t.Errorf("TPTriggerPrice(avg=%v) = %v, want %v", tt.avgPrice, got, tt.want)
			}
		})
	}
}

func TestSLTPArm_SLTriggerPrice(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		avgPrice float64
		want     float64
	}{
		{"20c entry floor at 14c", 0.20, 0.14},
		{"50c entry floor at 35c", 0.50, 0.35},
		{"10c entry floor at 7c", 0.10, 0.07},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			a := &SLTPArm{AvgPrice: tt.avgPrice}
			got := a.SLTriggerPrice()
			// Floating-point tolerance
			if diff := got - tt.want; diff > 1e-9 || diff < -1e-9 {
				t.Errorf("SLTriggerPrice(avg=%v) = %v, want %v", tt.avgPrice, got, tt.want)
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
		{"bad outcome", func(a *SLTPArm) { a.Outcome = Outcome("MAYBE") }, "outcome"},
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
