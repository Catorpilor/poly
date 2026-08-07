package live

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/Catorpilor/poly/internal/database"
)

// fakeWatchStore is an in-memory LiveWatchStore for manager tests. It records
// call counts so the persistence wiring (upsert on subscribe, delete on
// unsubscribe, no re-persist on restore) can be asserted without a database.
type fakeWatchStore struct {
	mu         sync.Mutex
	rows       map[string]*database.LiveSubscription
	order      []string // insertion order → deterministic ListAll
	saves      int
	deletes    int
	deleteAlls int
	listErr    error
}

func newFakeWatchStore() *fakeWatchStore {
	return &fakeWatchStore{rows: make(map[string]*database.LiveSubscription)}
}

func watchStoreKey(chatID int64, slug string) string {
	return fmt.Sprintf("%d\x00%s", chatID, slug)
}

func (s *fakeWatchStore) Save(_ context.Context, chatID int64, eventSlug string, tape bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.saves++
	s.putLocked(chatID, eventSlug, tape)
	return nil
}

func (s *fakeWatchStore) Delete(_ context.Context, chatID int64, eventSlug string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.deletes++
	s.removeLocked(watchStoreKey(chatID, eventSlug))
	return nil
}

func (s *fakeWatchStore) DeleteAll(_ context.Context, chatID int64) ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.deleteAlls++
	var slugs []string
	for _, k := range append([]string(nil), s.order...) {
		if row, ok := s.rows[k]; ok && row.ChatID == chatID {
			slugs = append(slugs, row.EventSlug)
			s.removeLocked(k)
		}
	}
	return slugs, nil
}

func (s *fakeWatchStore) ListAll(_ context.Context) ([]*database.LiveSubscription, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.listErr != nil {
		return nil, s.listErr
	}
	out := make([]*database.LiveSubscription, 0, len(s.order))
	for _, k := range s.order {
		if row, ok := s.rows[k]; ok {
			cp := *row
			out = append(out, &cp)
		}
	}
	return out, nil
}

// seed inserts a row as a restart would find it — not counted as a Save.
func (s *fakeWatchStore) seed(chatID int64, eventSlug string, tape bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.putLocked(chatID, eventSlug, tape)
}

func (s *fakeWatchStore) get(chatID int64, eventSlug string) *database.LiveSubscription {
	s.mu.Lock()
	defer s.mu.Unlock()
	if row, ok := s.rows[watchStoreKey(chatID, eventSlug)]; ok {
		cp := *row
		return &cp
	}
	return nil
}

func (s *fakeWatchStore) rowCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.rows)
}

func (s *fakeWatchStore) putLocked(chatID int64, eventSlug string, tape bool) {
	k := watchStoreKey(chatID, eventSlug)
	if _, ok := s.rows[k]; !ok {
		s.order = append(s.order, k)
	}
	s.rows[k] = &database.LiveSubscription{ChatID: chatID, EventSlug: eventSlug, Tape: tape}
}

func (s *fakeWatchStore) removeLocked(k string) {
	if _, ok := s.rows[k]; !ok {
		return
	}
	delete(s.rows, k)
	for i, kk := range s.order {
		if kk == k {
			s.order = append(s.order[:i], s.order[i+1:]...)
			break
		}
	}
}

// newPersistedManager extends the snipe-wired harness with a fake watch store,
// so persistence wiring can be exercised alongside the snipe-watch lifecycle.
func newPersistedManager(t *testing.T) (*LiveTradeManager, *SnipeWatcher, *fakeWatchStore) {
	t.Helper()
	m, w, _ := newSnipeWiredManager(t)
	store := newFakeWatchStore()
	m.SetLiveWatchStore(store)
	return m, w, store
}

func TestSubscribeTelegramPersistsWatch(t *testing.T) {
	t.Parallel()
	m, _, store := newPersistedManager(t)
	ctx := context.Background()

	if _, err := m.SubscribeTelegram(ctx, 7, pinnedFeedEventSlug, false); err != nil {
		t.Fatalf("SubscribeTelegram: %v", err)
	}
	row := store.get(7, pinnedFeedEventSlug)
	if row == nil {
		t.Fatal("subscribe did not persist the watch")
	}
	if row.Tape {
		t.Error("persisted tape = true, want false")
	}
	if store.saves != 1 {
		t.Errorf("saves = %d, want 1", store.saves)
	}

	// Re-subscribing flips the tape flag: the upsert must update the row in
	// place, never create a duplicate.
	if _, err := m.SubscribeTelegram(ctx, 7, pinnedFeedEventSlug, true); err != nil {
		t.Fatalf("SubscribeTelegram(tape): %v", err)
	}
	row = store.get(7, pinnedFeedEventSlug)
	if row == nil || !row.Tape {
		t.Errorf("re-subscribe did not update tape flag: %+v", row)
	}
	if store.saves != 2 {
		t.Errorf("saves = %d, want 2 (upsert on every subscribe)", store.saves)
	}
	if store.rowCount() != 1 {
		t.Errorf("row count = %d, want 1 (upsert, not duplicate)", store.rowCount())
	}
}

func TestUnsubscribeTelegramDeletesWatch(t *testing.T) {
	t.Parallel()
	m, _, store := newPersistedManager(t)
	ctx := context.Background()

	if _, err := m.SubscribeTelegram(ctx, 7, pinnedFeedEventSlug, false); err != nil {
		t.Fatalf("SubscribeTelegram: %v", err)
	}
	if !m.UnsubscribeTelegram(7, pinnedFeedEventSlug) {
		t.Fatal("UnsubscribeTelegram returned false")
	}
	if store.get(7, pinnedFeedEventSlug) != nil {
		t.Error("unsubscribe did not delete the persisted watch")
	}
	if store.deletes != 1 {
		t.Errorf("deletes = %d, want 1", store.deletes)
	}
}

func TestUnsubscribeAllTelegramDeletesAll(t *testing.T) {
	t.Parallel()
	m, _, store := newPersistedManager(t)
	ctx := context.Background()

	for _, slug := range []string{pinnedFeedEventSlug, pinnedFeedGame3Slug} {
		if _, err := m.SubscribeTelegram(ctx, 7, slug, false); err != nil {
			t.Fatalf("SubscribeTelegram(%s): %v", slug, err)
		}
	}
	got := m.UnsubscribeAllTelegram(7)
	if len(got) != 2 {
		t.Fatalf("UnsubscribeAllTelegram returned %v, want 2 slugs", got)
	}
	if store.rowCount() != 0 {
		t.Errorf("rows remain after unsubscribe-all: %d", store.rowCount())
	}
	if store.deleteAlls != 1 {
		t.Errorf("deleteAll calls = %d, want 1", store.deleteAlls)
	}
}

func TestRestoreWatchesRegistersStoredRows(t *testing.T) {
	t.Parallel()
	m, w, store := newPersistedManager(t)
	store.seed(7, pinnedFeedEventSlug, false)
	store.seed(8, pinnedFeedGame3Slug, true)

	restored, failed, err := m.RestoreWatches(context.Background())
	if err != nil {
		t.Fatalf("RestoreWatches: %v", err)
	}
	if restored != 2 || failed != 0 {
		t.Errorf("restored=%d failed=%d, want 2/0", restored, failed)
	}
	// Moneyline event → ML tokens watched; pinned game3 slug → game3 tokens.
	for _, tok := range []string{"ml-blg", "ml-hle", "g3-blg", "g3-hle"} {
		if !w.isWatched(tok) {
			t.Errorf("token %s not watched after restore", tok)
		}
	}
	// Tape flag is preserved per row.
	if m.IsTapeSubscription(7, pinnedFeedEventSlug) {
		t.Error("chat 7 tape flag should be false")
	}
	if !m.IsTapeSubscription(8, pinnedFeedGame3Slug) {
		t.Error("chat 8 tape flag should be preserved as true")
	}
	// Restore re-registers the in-memory view only — it must not write back.
	if store.saves != 0 || store.deletes != 0 || store.deleteAlls != 0 {
		t.Errorf("restore mutated store: saves=%d deletes=%d deleteAlls=%d",
			store.saves, store.deletes, store.deleteAlls)
	}
}

func TestRestoreWatchesResolveFailureKeepsRow(t *testing.T) {
	t.Parallel()
	m, w, store := newPersistedManager(t)

	// A Gamma that 404s every lookup makes an uncached slug fail
	// deterministically, without touching the real network.
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, _ *http.Request) {
		rw.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()
	m.resolver.gammaAPIURL = srv.URL

	store.seed(7, pinnedFeedEventSlug, false)       // cached → resolves
	store.seed(9, "closed-or-missing-event", false) // uncached → 404

	restored, failed, err := m.RestoreWatches(context.Background())
	if err != nil {
		t.Fatalf("RestoreWatches: %v", err)
	}
	if restored != 1 || failed != 1 {
		t.Errorf("restored=%d failed=%d, want 1/1", restored, failed)
	}
	if !w.isWatched("ml-blg") {
		t.Error("resolvable watch was not registered")
	}
	if len(m.GetUserSubscriptions(9)) != 0 {
		t.Error("failed watch must not register in the runtime registry")
	}
	// Critical (ADR 0008): a resolve failure keeps the DB row — expiry is
	// Phase 4's job (positive closed evidence only), never a boot-time error.
	if store.get(9, "closed-or-missing-event") == nil {
		t.Error("resolve failure deleted the row — must keep it")
	}
	if store.deletes != 0 || store.deleteAlls != 0 {
		t.Errorf("restore deleted rows on failure: deletes=%d deleteAlls=%d",
			store.deletes, store.deleteAlls)
	}
}

func TestNilStorePathsNoPanic(t *testing.T) {
	t.Parallel()
	m, _, _ := newSnipeWiredManager(t) // no store wired → watchStore is nil
	ctx := context.Background()

	if _, err := m.SubscribeTelegram(ctx, 7, pinnedFeedEventSlug, false); err != nil {
		t.Fatalf("SubscribeTelegram: %v", err)
	}
	if !m.UnsubscribeTelegram(7, pinnedFeedEventSlug) {
		t.Fatal("UnsubscribeTelegram returned false")
	}
	if _, err := m.SubscribeTelegram(ctx, 8, pinnedFeedGame3Slug, true); err != nil {
		t.Fatalf("SubscribeTelegram: %v", err)
	}
	if got := m.UnsubscribeAllTelegram(8); len(got) != 1 {
		t.Fatalf("UnsubscribeAllTelegram = %v, want 1", got)
	}
	// RestoreWatches with a nil store is a no-op, never a panic.
	restored, failed, err := m.RestoreWatches(ctx)
	if err != nil || restored != 0 || failed != 0 {
		t.Errorf("nil-store RestoreWatches = (%d,%d,%v), want (0,0,nil)", restored, failed, err)
	}
}
