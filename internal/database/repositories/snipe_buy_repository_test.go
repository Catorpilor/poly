package repositories_test

import (
	"testing"

	"github.com/Catorpilor/poly/internal/database/repositories"
	"github.com/Catorpilor/poly/internal/telegram"
)

// The concrete repository must satisfy the consumer-side store interface that
// internal/telegram defines and cmd/bot wires in (issue #84). Asserting it here
// locks the structural seam — the same discipline as LiveWatchRepository — so a
// signature drift fails the repository suite, not only the binary build.
//
// A live-database integration test is intentionally omitted: the repository is a
// direct *pgxpool.Pool with no mock seam, the repositories package has no DB
// test harness, and the project's TDD rules forbid calling a real database in
// tests. The repository's behavior is exercised through the write-through and
// restore fake-store tests in internal/telegram.
var _ telegram.SnipeBuyStore = repositories.NewSnipeBuyRepository(nil)

func TestSnipeBuyRepositoryImplementsStore(t *testing.T) {
	t.Parallel()
	if repositories.NewSnipeBuyRepository(nil) == nil {
		t.Fatal("NewSnipeBuyRepository returned nil")
	}
}
