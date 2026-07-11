package polymarket

import (
	"math/big"
	"strings"
	"testing"
	"unicode/utf8"
)

// Market titles may contain non-ASCII (CJK titles, accented team names).
// Truncation must never byte-slice mid-rune — that emits invalid UTF-8,
// which Telegram rejects with a 400 for the whole message.
func TestFormatPositionsFromAPI_TruncatesTitlesRuneSafe(t *testing.T) {
	t.Parallel()

	longCJK := strings.Repeat("市", 60) // 60 runes, 180 bytes — must truncate
	positions := []*Position{
		{
			MarketTitle:  longCJK,
			Outcome:      "Yes",
			Shares:       big.NewInt(1_000_000),
			AveragePrice: 0.5,
			CurrentPrice: 0.6,
			Value:        0.6,
			PnL:          0.1,
			PnLPercent:   20,
		},
	}

	out := NewUnifiedPositionScanner().formatPositionsFromAPI(positions)

	if !utf8.ValidString(out) {
		t.Fatalf("output contains invalid UTF-8 (title was byte-sliced mid-rune): %q", out)
	}
	if !strings.Contains(out, "...") {
		t.Errorf("long title was not truncated: %q", out)
	}
	if strings.Contains(out, longCJK) {
		t.Errorf("60-rune title should have been shortened")
	}
}

func TestFormatPositionsFromAPI_ShortTitleUntouched(t *testing.T) {
	t.Parallel()

	positions := []*Position{
		{
			MarketTitle: "Will X happen?",
			Outcome:     "No",
			Shares:      big.NewInt(2_000_000),
		},
	}

	out := NewUnifiedPositionScanner().formatPositionsFromAPI(positions)

	if !strings.Contains(out, "Will X happen?") {
		t.Errorf("short title must appear unmodified, got %q", out)
	}
}
