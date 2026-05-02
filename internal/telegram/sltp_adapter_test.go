package telegram

import (
	"math/big"
	"strings"
	"testing"

	"github.com/Catorpilor/poly/internal/database"
	"github.com/Catorpilor/poly/internal/polymarket"
)

func TestSharesBigIntToFloat(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		in   *big.Int
		want float64
	}{
		{"nil returns zero", nil, 0},
		{"one share", big.NewInt(1_000_000), 1.0},
		{"half share", big.NewInt(500_000), 0.5},
		{"100 shares", big.NewInt(100_000_000), 100.0},
		{"fractional 1.234567 shares", big.NewInt(1_234_567), 1.234567},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := sharesBigIntToFloat(tt.in)
			if diff := got - tt.want; diff > 1e-9 || diff < -1e-9 {
				t.Errorf("sharesBigIntToFloat = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestSLTPRowForPosition_ArmedShowsDisarm(t *testing.T) {
	t.Parallel()
	pos := &polymarket.Position{
		MarketTitle: "Will X happen?",
		TokenID:     "tokX",
		Outcome:     "YES",
		Shares:      big.NewInt(10_000_000),
	}
	arm := &database.SLTPArm{TPArmed: true, SLArmed: true}
	row := sltpRowForPosition(2, pos, arm)
	if len(row) != 1 {
		t.Fatalf("expected 1 button, got %d", len(row))
	}
	if row[0].CallbackData == nil || *row[0].CallbackData != "sltp:off:2" {
		t.Errorf("expected callback sltp:off:2, got %v", row[0].CallbackData)
	}
	if !strings.Contains(row[0].Text, "Disarm") {
		t.Errorf("expected button text to mention Disarm, got %q", row[0].Text)
	}
}

func TestSLTPRowForPosition_UnarmedShowsArm(t *testing.T) {
	t.Parallel()
	pos := &polymarket.Position{
		MarketTitle: "Will X happen?",
		TokenID:     "tokX",
		Outcome:     "YES",
		Shares:      big.NewInt(10_000_000),
	}
	row := sltpRowForPosition(0, pos, nil)
	if len(row) != 1 {
		t.Fatalf("expected 1 button, got %d", len(row))
	}
	if row[0].CallbackData == nil || *row[0].CallbackData != "sltp:arm:0" {
		t.Errorf("expected callback sltp:arm:0, got %v", row[0].CallbackData)
	}
	if !strings.Contains(row[0].Text, "Arm") {
		t.Errorf("expected button text to mention Arm, got %q", row[0].Text)
	}
}

func TestSLTPRowForPosition_CallbackDataUnder64Bytes(t *testing.T) {
	t.Parallel()
	// Telegram caps callback_data at 64 bytes. Position index up to 8 (per handleSLTPList
	// cap) yields callback like "sltp:arm:8" — trivially under, but guard against regressions.
	pos := &polymarket.Position{
		MarketTitle: "An extremely long market title that must not break the button",
		TokenID:     "some_long_token_id_value_here",
		Outcome:     "YES",
		Shares:      big.NewInt(1_234_567_890),
	}
	row := sltpRowForPosition(7, pos, nil)
	if cb := row[0].CallbackData; cb == nil || len(*cb) > 64 {
		t.Errorf("callback data %q exceeds 64 bytes (or nil)", cbString(cb))
	}
}

func cbString(p *string) string {
	if p == nil {
		return "<nil>"
	}
	return *p
}

// TestNormalizeOutcome covers the boundary between the position scanner
// (returns "Yes"/"No" for display) and SLTPArm.Validate (requires "YES"/"NO").
// Without this normalization, arming a position fails with
// `invalid arm: invalid outcome: Yes`.
func TestNormalizeOutcome(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in   string
		want database.Outcome
	}{
		{"Yes", database.OutcomeYes},
		{"yes", database.OutcomeYes},
		{"YES", database.OutcomeYes},
		{"No", database.OutcomeNo},
		{"NO", database.OutcomeNo},
	}
	for _, c := range cases {
		if got := normalizeOutcome(c.in); got != c.want {
			t.Errorf("normalizeOutcome(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
