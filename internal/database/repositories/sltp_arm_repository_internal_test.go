package repositories

import "testing"

// TestArmUpsertResetsLadderState is the issue #81 F3 guard: a re-arm over a
// mid-ladder deep arm must restart the ladder, so both upsert queries' ON
// CONFLICT clauses reset ladder_rungs_fired and ladder_base_shares to 0 (like
// high_water_mark's re-seed). Without a DB harness the SQL text is the testable
// surface; asserting on it fails the suite the moment the reset is dropped.
func TestArmUpsertResetsLadderState(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name  string
		query string
	}{
		{"Arm (TP+SL)", armUpsertQuery},
		{"ArmTPOnly", armTPOnlyUpsertQuery},
	} {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			for _, want := range []string{
				"ON CONFLICT",
				"ladder_rungs_fired = 0",
				"ladder_base_shares = 0",
			} {
				if !contains(tt.query, want) {
					t.Errorf("%s upsert missing %q:\n%s", tt.name, want, tt.query)
				}
			}
		})
	}
}

// contains is a tiny substring check kept local so the test needs no imports
// beyond testing.
func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
