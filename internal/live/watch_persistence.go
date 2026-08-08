package live

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/Catorpilor/poly/internal/database"
)

// watchRestoreDefaultPause spaces sequential Gamma re-resolves at boot so
// restoring a full set of Live Watches does not hammer Gamma. Applied between
// resolves, not before the first.
const watchRestoreDefaultPause = 200 * time.Millisecond

// LiveWatchStore is the durable record of Live Watches (ADR 0008). The
// registry in this package stays the runtime view; this store is what a
// restart re-reads. The interface lives here (the consumer side) so internal/
// live never depends on internal/database/repositories — the dependency points
// inward, mirroring SLTPArmStore. The concrete implementation
// (repositories.LiveWatchRepository) satisfies it structurally.
type LiveWatchStore interface {
	// Save upserts the watch for (chatID, eventSlug). tape may change on an
	// existing row (a re-subscribe switches feed mode), so it is written every
	// time.
	Save(ctx context.Context, chatID int64, eventSlug string, tape bool) error
	// Delete removes one watch. An absent row is not an error — the caller has
	// already dropped the in-memory subscription, and delete must be idempotent.
	Delete(ctx context.Context, chatID int64, eventSlug string) error
	// DeleteAll removes every watch for chatID and returns the removed slugs.
	DeleteAll(ctx context.Context, chatID int64) ([]string, error)
	// ListAll returns every stored watch, for boot re-registration.
	ListAll(ctx context.Context) ([]*database.LiveSubscription, error)
}

// SetLiveWatchStore wires the durable Live Watch store. Optional: leaving it
// unset keeps the pre-0008 in-memory-only behavior. Follows the manager's
// other setter-injected dependencies (SetSnipeWatcher, SetTelegramBot).
func (m *LiveTradeManager) SetLiveWatchStore(s LiveWatchStore) {
	m.watchStore = s
}

// RestoreWatches re-registers every durable Live Watch from the store so a
// restart is watch-neutral (ADR 0008): losing a watch silently costs coverage,
// and coverage is upstream of every dollar the strategy makes. Each row is
// re-resolved against Gamma and its assets + snipe watch re-registered, with
// the tape flag preserved.
//
// Failure semantics are deliberate and load-bearing: a resolve failure (Gamma
// error, an event not yet open, a closed event) LOGS and KEEPS the row, then
// continues with the next. A watch is only ever expired on positive
// closed-market evidence — that is Phase 4's job (the #40 sweeper doctrine).
// Deleting on a transient boot-time error would let one Gamma hiccup silently
// erase coverage. Resolves run sequentially with restorePause between them.
//
// Returns the restored and failed counts (also emitted as one summary log
// line). A nil store makes this a no-op.
func (m *LiveTradeManager) RestoreWatches(ctx context.Context) (restored, failed int, err error) {
	if m.watchStore == nil {
		return 0, 0, nil
	}

	watches, err := m.watchStore.ListAll(ctx)
	if err != nil {
		return 0, 0, fmt.Errorf("restore watches: list: %w", err)
	}

	for i, watch := range watches {
		if i > 0 && m.restorePause > 0 {
			select {
			case <-ctx.Done():
				log.Printf("LiveTradeManager: RestoreWatches cancelled after %d watch(es)", i)
				return restored, failed, ctx.Err()
			case <-time.After(m.restorePause):
			}
		}

		eventInfo, resolveErr := m.resolver.GetEventInfo(ctx, watch.EventSlug)
		if resolveErr != nil {
			// Keep the row: a transient Gamma failure or a not-yet-open event
			// must not cost coverage. Phase 4's closed-market sweep is the only
			// path that removes a watch.
			log.Printf("LiveTradeManager: restore watch failed (chat=%d slug=%s): %v — keeping row",
				watch.ChatID, watch.EventSlug, resolveErr)
			failed++
			continue
		}

		// Re-register the in-memory view only — the row already exists, so this
		// path must NOT re-persist. Mirrors SubscribeTelegram's registration,
		// preserving the tape flag.
		if isNew := m.subscriptions.SubscribeTelegram(watch.ChatID, watch.EventSlug, watch.Tape); isNew {
			m.trackEventAssets(watch.EventSlug, eventInfo)
			m.snipeWatchEvent(watch.EventSlug, eventInfo)
		}
		restored++
	}

	log.Printf("LiveTradeManager: restored=%d failed=%d live watch(es)", restored, failed)
	return restored, failed, nil
}
