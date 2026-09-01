package live

import "testing"

// PeriodTotalProp (September review proposal #3, "corpse-by-clock"): period/total
// Over/Under props decay on the game clock and are structurally unrecoverable, so
// the snipe watcher gates them to a log-only line. Spread props are NOT in the
// class (a one-goal swing still flips a spread) and stay alertable. Word-boundary
// matched against the marker list — never Contains (the 2026-08-14 short-marker
// lesson: "over/under" must not match inside "Takeover/Underdog").
func TestPeriodTotalProp(t *testing.T) {
	t.Parallel()
	tests := []struct {
		question string
		want     bool
	}{
		// Real production O/U specimens → true.
		{"RC Celta de Vigo vs. CA Osasuna: 1st Half O/U 1.5", true},
		{"Mexico vs. Ecuador: O/U 2.5", true},
		{"Senegal vs. Iraq: O/U 3.5", true},
		{"Total Goals Over/Under 2.5?", true}, // spelled-out marker
		// Spread props stay alertable → false.
		{"Spread: FC Barcelona (-2.5)", false},
		{"Spread: Real Madrid CF (-2.5)", false},
		// Moneylines and game winners → false.
		{"Will Arsenal FC win on 2026-08-31?", false},
		{"LoL: Nongshim Red Force vs BNK FEARX - Game 2 Winner", false},
		{"Counter-Strike: G2 vs Aurora Gaming (BO3) - BLAST Open Porto Group A", false},
		// Word-boundary discipline: the harmful-direction false positives.
		// "over/under" is a substring of "takeover/underdog" but must NOT match.
		{"Massive Takeover/Underdog Storyline in Game 1?", false},
		// A "you"-style word must never trip the "o/u" marker (no slash token).
		{"Will Youssef En-Nesyri score first?", false},
	}
	for _, tt := range tests {
		if got := PeriodTotalProp(tt.question); got != tt.want {
			t.Errorf("PeriodTotalProp(%q) = %v, want %v", tt.question, got, tt.want)
		}
	}
}
