package repositories_test

import (
	"testing"

	"github.com/Catorpilor/poly/internal/database/repositories"
	"github.com/Catorpilor/poly/internal/live"
)

// The concrete repository must satisfy the consumer-side store interface that
// internal/live defines and cmd/bot wires in. Asserting it here locks the
// structural seam so a signature drift fails the repository suite, not only the
// binary build.
//
// A live-database integration test is intentionally omitted: the repository
// mirrors sltp_arm_repository exactly (a direct *pgxpool.Pool, no mock seam),
// the repositories package has no DB test harness, and the project's TDD rules
// forbid calling a real database in tests. The repository's behavior is
// exercised through the manager's fake-store tests in internal/live.
var _ live.LiveWatchStore = repositories.NewLiveWatchRepository(nil)

func TestLiveWatchRepositoryImplementsStore(t *testing.T) {
	t.Parallel()
	if repositories.NewLiveWatchRepository(nil) == nil {
		t.Fatal("NewLiveWatchRepository returned nil")
	}
}
