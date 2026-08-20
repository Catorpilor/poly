package repositories_test

import (
	"testing"

	"github.com/Catorpilor/poly/internal/database/repositories"
	"github.com/Catorpilor/poly/internal/live"
)

// The concrete SL/TP arm repository must satisfy the consumer-side store
// interface the monitor defines and cmd/bot wires in. Asserting it here locks
// the structural seam so a signature drift — e.g. the new AdvanceLadder method
// for the deep-entry ladder (issue #81) — fails the repository suite, not only
// the binary build.
//
// A live-database integration test is intentionally omitted, matching
// live_watch_repository_test: the repository is a direct *pgxpool.Pool with no
// mock seam, the package has no DB test harness, and the project's TDD rules
// forbid calling a real database in tests. AdvanceLadder's persistence semantics
// (monotonic fired-rung advance, frozen base surviving a restart, non-qualifying
// arms untouched) are exercised through the monitor's fake-store restart test in
// internal/live (TestSLTPMonitor_Ladder_SurvivesRestart).
var _ live.SLTPArmStore = repositories.NewSLTPArmRepository(nil)

func TestSLTPArmRepositoryImplementsStore(t *testing.T) {
	t.Parallel()
	if repositories.NewSLTPArmRepository(nil) == nil {
		t.Fatal("NewSLTPArmRepository returned nil")
	}
}
