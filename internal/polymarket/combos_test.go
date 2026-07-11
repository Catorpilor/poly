package polymarket

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// loadComboFixture reads the recorded Data API response captured from
// GET /v1/positions/combos?user=0x3eac...67cb (a 4-leg FIFA World Cup parlay).
func loadComboFixture(t *testing.T) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "combos_positions.json"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	return data
}

func TestParseComboPositions(t *testing.T) {
	combos, err := parseComboPositions(loadComboFixture(t))
	if err != nil {
		t.Fatalf("parseComboPositions: %v", err)
	}

	if len(combos) != 1 {
		t.Fatalf("want 1 combo, got %d", len(combos))
	}

	c := combos[0]
	tests := []struct {
		name string
		got  any
		want any
	}{
		{"condition id", c.ConditionID, "0x036cf52d666a434b45f6d78174e183aba30000000000000000000000000000"},
		{"position id", c.PositionID, "1549450180583996140930639221814853829808264037419948333637106877764611342336"},
		{"status", c.Status, "RESOLVED_LOSS"},
		{"legs total", c.LegsTotal, 4},
		{"legs resolved", c.LegsResolved, 1},
		{"legs pending", c.LegsPending, 3},
		{"leg slice len", len(c.Legs), 4},
	}
	for _, tt := range tests {
		if tt.got != tt.want {
			t.Errorf("%s = %v, want %v", tt.name, tt.got, tt.want)
		}
	}

	// String-encoded numerics must be parsed into floats.
	if c.Shares < 124.76 || c.Shares > 124.77 {
		t.Errorf("shares = %v, want ~124.766063", c.Shares)
	}
	if c.EntryAvgPrice < 0.16 || c.EntryAvgPrice > 0.1606 {
		t.Errorf("entry avg price = %v, want ~0.1603", c.EntryAvgPrice)
	}
	if c.TotalCost != 20.0 {
		t.Errorf("total cost = %v, want 20.00", c.TotalCost)
	}

	// Legs preserve order and decode market metadata.
	leg0 := c.Legs[0]
	if leg0.MarketTitle != "Korea Republic" || leg0.OutcomeLabel != "No" {
		t.Errorf("leg0 = %q/%q, want Korea Republic/No", leg0.MarketTitle, leg0.OutcomeLabel)
	}
	if leg0.Status != "RESOLVED_LOSS" {
		t.Errorf("leg0 status = %q, want RESOLVED_LOSS", leg0.Status)
	}
	leg1 := c.Legs[1]
	if leg1.MarketTitle != "Brazil" || leg1.Status != "OPEN" {
		t.Errorf("leg1 = %q/%q, want Brazil/OPEN", leg1.MarketTitle, leg1.Status)
	}
	if leg1.CurrentPrice < 0.57 || leg1.CurrentPrice > 0.58 {
		t.Errorf("leg1 current price = %v, want ~0.575", leg1.CurrentPrice)
	}
}

func TestFormatComboPositions(t *testing.T) {
	combos, err := parseComboPositions(loadComboFixture(t))
	if err != nil {
		t.Fatalf("parseComboPositions: %v", err)
	}

	out := FormatComboPositions(combos)
	for _, want := range []string{
		"Combo Position",  // header
		"RESOLVED_LOSS",   // combo status
		"Korea Republic",  // leg market
		"Brazil",          // leg market
		"4 legs",          // leg count
		"124.77",          // shares rounded
	} {
		if !strings.Contains(out, want) {
			t.Errorf("formatted output missing %q\n---\n%s", want, out)
		}
	}
}

func TestFormatComboPositionsEmpty(t *testing.T) {
	out := FormatComboPositions(nil)
	if !strings.Contains(out, "No combo positions") {
		t.Errorf("empty output = %q, want a 'No combo positions' message", out)
	}
}
