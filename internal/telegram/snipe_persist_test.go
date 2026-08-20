package telegram

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Catorpilor/poly/internal/database"
)

// --- fake durable buy store (issue #84) ---

type savedSnipeBuy struct {
	chatID  int64
	tokenID string
	amount  float64
	pool    string
}

// fakeSnipeBuyStore records Save calls and serves ListSince from an in-memory
// row set. now stamps a Save's bought_at (defaults to real time); saveErr makes
// every Save fail (the write-failure path). Safe for concurrent use.
type fakeSnipeBuyStore struct {
	mu      sync.Mutex
	saved   []savedSnipeBuy
	rows    []*database.SnipeBuy
	saveErr error
	now     func() time.Time
}

func (s *fakeSnipeBuyStore) stamp() time.Time {
	if s.now != nil {
		return s.now().UTC()
	}
	return time.Now().UTC()
}

func (s *fakeSnipeBuyStore) Save(_ context.Context, chatID int64, tokenID string, amountUSD float64, pool string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.saveErr != nil {
		return s.saveErr
	}
	s.saved = append(s.saved, savedSnipeBuy{chatID, tokenID, amountUSD, pool})
	s.rows = append(s.rows, &database.SnipeBuy{
		ID: int64(len(s.rows) + 1), ChatID: chatID, TokenID: tokenID,
		AmountUSD: amountUSD, Pool: pool, BoughtAt: s.stamp(),
	})
	return nil
}

// ListSince mirrors the SQL `WHERE bought_at >= since`.
func (s *fakeSnipeBuyStore) ListSince(_ context.Context, since time.Time) ([]*database.SnipeBuy, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []*database.SnipeBuy
	for _, r := range s.rows {
		if !r.BoughtAt.Before(since) {
			out = append(out, r)
		}
	}
	return out, nil
}

func (s *fakeSnipeBuyStore) savedCalls() []savedSnipeBuy {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]savedSnipeBuy(nil), s.saved...)
}

// --- write-through: the seam ---

// TestSnipeBoughtRecordWriteThrough: mark writes ONE 'main' row with the stake
// and latches the in-memory record; logDeepBuy writes ONE 'deep' row WITHOUT
// touching the in-memory record (Gate 3a semantics).
func TestSnipeBoughtRecordWriteThrough(t *testing.T) {
	t.Parallel()
	store := &fakeSnipeBuyStore{}
	r := newSnipeBoughtRecord()
	r.SetStore(store)

	r.mark(7, "tokMain", snipeAutoBuyUSD)
	r.logDeepBuy(7, "tokDeep", snipeDeepBuyUSD)

	if !r.held(7, "tokMain") {
		t.Error("mark did not latch the in-memory record")
	}
	if r.held(7, "tokDeep") {
		t.Error("logDeepBuy must NOT latch the in-memory record (deep never marks it)")
	}
	got := store.savedCalls()
	want := []savedSnipeBuy{
		{7, "tokMain", snipeAutoBuyUSD, database.SnipeBuyPoolMain},
		{7, "tokDeep", snipeDeepBuyUSD, database.SnipeBuyPoolDeep},
	}
	if len(got) != len(want) {
		t.Fatalf("saved %d rows, want %d: %+v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("saved[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}
}

// TestSnipeBoughtRecordNilStoreUnchanged: with no store, mark/logDeepBuy are
// byte-identical to pre-#84 — the in-memory record still latches, nothing
// persists, and nothing panics.
func TestSnipeBoughtRecordNilStoreUnchanged(t *testing.T) {
	t.Parallel()
	r := newSnipeBoughtRecord() // no store

	r.mark(7, "tok", snipeAutoBuyUSD)
	r.logDeepBuy(7, "tokD", snipeDeepBuyUSD)

	if !r.held(7, "tok") {
		t.Error("mark with nil store must still latch the in-memory record")
	}
	if r.held(7, "tokD") {
		t.Error("logDeepBuy must never latch the record")
	}
}

// TestSnipeAutoBuyPersistsMainRow: the in-band $10 auto-buy writes exactly one
// 'main' row for the stake.
func TestSnipeAutoBuyPersistsMainRow(t *testing.T) {
	t.Parallel()
	h := newSnipeAutoBuyHarness(t, snipeHarnessConfig{ask: 0.17, askOK: true, user: snipeWalletUser()})
	store := &fakeSnipeBuyStore{}
	h.bot.SetSnipeBuyStore(store)
	m := testSnipeMarket()

	h.bot.NotifySnipeAlert(7, m, 0.45, 0.17)

	got := store.savedCalls()
	if len(got) != 1 {
		t.Fatalf("saved %d rows, want 1: %+v", len(got), got)
	}
	if got[0] != (savedSnipeBuy{7, m.TokenID, snipeAutoBuyUSD, database.SnipeBuyPoolMain}) {
		t.Errorf("in-band row = %+v, want {7, token, $10, main}", got[0])
	}
}

// TestSnipeManualTapPersistsMainRow: a manual $25 tap writes one 'main' row for
// $25.
func TestSnipeManualTapPersistsMainRow(t *testing.T) {
	t.Parallel()
	h := newSnipeAutoBuyHarness(t, snipeHarnessConfig{ask: 0.17, askOK: true, user: snipeWalletUser()})
	store := &fakeSnipeBuyStore{}
	h.bot.SetSnipeBuyStore(store)
	m := testSnipeMarket()

	// Register an alert, then tap Add-$25 (a manual buy that does not draw the cap).
	alertID := h.bot.snipeAlerts.add(m)
	h.bot.handleSnipeCallback(context.Background(), snipeTapUpdate(7, "snipe:"+alertID+":25"))

	got := store.savedCalls()
	if len(got) != 1 {
		t.Fatalf("saved %d rows, want 1: %+v", len(got), got)
	}
	if got[0] != (savedSnipeBuy{7, m.TokenID, 25, database.SnipeBuyPoolMain}) {
		t.Errorf("manual tap row = %+v, want {7, token, $25, main}", got[0])
	}
}

// TestSnipeBoxedTranchePersistsMainRow: a boxed rung writes one 'main' row for
// the $5 tranche stake (it shares the snipeAutoBuyExec seam).
func TestSnipeBoxedTranchePersistsMainRow(t *testing.T) {
	t.Parallel()
	h := newSnipeAutoBuyHarness(t, snipeHarnessConfig{ask: 0.09, askOK: true, user: snipeWalletUser()})
	store := &fakeSnipeBuyStore{}
	h.bot.SetSnipeBuyStore(store)
	m := testSnipeMarket()
	h.bot.snipeBoxedLatch.arm(7, m.TokenID, true, true) // latched case-3

	h.bot.NotifySnipeBoxed(7, m, 0.45, 0.09, 1)

	got := store.savedCalls()
	if len(got) != 1 {
		t.Fatalf("saved %d rows, want 1: %+v", len(got), got)
	}
	if got[0] != (savedSnipeBuy{7, m.TokenID, snipeBoxedTrancheUSD, database.SnipeBuyPoolMain}) {
		t.Errorf("boxed row = %+v, want {7, token, $5, main}", got[0])
	}
}

// TestSnipeDeepCrashPersistsDeepRow: a Deep Crash fire writes one 'deep' row for
// the $5 stake.
func TestSnipeDeepCrashPersistsDeepRow(t *testing.T) {
	t.Parallel()
	h := newSnipeAutoBuyHarness(t, snipeHarnessConfig{ask: 0.02, askOK: true, user: snipeWalletUser()})
	store := &fakeSnipeBuyStore{}
	h.bot.SetSnipeBuyStore(store)
	m := testSnipeMarket()

	h.bot.NotifySnipeDeepCrash(7, m, 0.45, 0.02, 0.17, time.Minute)

	got := store.savedCalls()
	if len(got) != 1 {
		t.Fatalf("saved %d rows, want 1: %+v", len(got), got)
	}
	if got[0] != (savedSnipeBuy{7, m.TokenID, snipeDeepBuyUSD, database.SnipeBuyPoolDeep}) {
		t.Errorf("deep row = %+v, want {7, token, $5, deep}", got[0])
	}
}

// TestSnipeWriteFailureDoesNotBlockBuy: a store that errors on every Save must
// not fail the buy — the fill stands, the in-memory record latches, the alert
// is delivered, and the watcher is marked.
func TestSnipeWriteFailureDoesNotBlockBuy(t *testing.T) {
	t.Parallel()
	h := newSnipeAutoBuyHarness(t, snipeHarnessConfig{ask: 0.17, askOK: true, user: snipeWalletUser()})
	store := &fakeSnipeBuyStore{saveErr: errors.New("db down")}
	h.bot.SetSnipeBuyStore(store)
	m := testSnipeMarket()

	h.bot.NotifySnipeAlert(7, m, 0.45, 0.17)

	if got := h.buys.count(); got != 1 {
		t.Fatalf("buy calls = %d, want 1 — a persist failure must not block the buy", got)
	}
	if !h.bot.snipeBought.held(7, m.TokenID) {
		t.Error("in-memory record must stay authoritative when the persist fails")
	}
	if got := h.watch.boughtCount(); got != 1 {
		t.Errorf("MarkBought calls = %d, want 1", got)
	}
	if len(store.savedCalls()) != 0 {
		t.Error("errored Save must record nothing")
	}
	sent := h.tg.sentAt(t, 0)
	if !strings.Contains(sent.text, "Auto-sniped") || !strings.Contains(sent.text, "ord-auto") {
		t.Errorf("alert must still be delivered on persist failure:\n%s", sent.text)
	}
}

// --- boot restore ---

// restoreTestBot builds a minimal bot with a durable store and both spend
// ledgers pinned to the given clock — enough to exercise restoreSnipeBuysAt
// without the Telegram/Gamma harness.
func restoreTestBot(store *fakeSnipeBuyStore, now time.Time) (*Bot, *fakeSnipeWatch) {
	watch := &fakeSnipeWatch{}
	b := &Bot{
		snipeBought:    newSnipeBoughtRecord(),
		snipeSpend:     newSnipeSpendLedger(snipeAutoBuyDailyCapUSD),
		snipeDeepSpend: newSnipeSpendLedger(snipeDeepDailyCapUSD),
		snipeWatcher:   watch,
	}
	b.SetSnipeBuyStore(store)
	b.snipeSpend.now = func() time.Time { return now }
	b.snipeDeepSpend.now = func() time.Time { return now }
	return b, watch
}

// TestRestoreSnipeBuysWindowAndPools pins the whole restore contract in one
// scenario: the 24h window filters old rows; the bought record rebuilds from
// MAIN rows only; MarkBought re-latches EVERY row (main + deep); the spend
// ledgers seed per pool for the CURRENT UTC day only — with the deliberate
// asymmetry that a yesterday-23:50 row (inside 24h) restores the bought latch
// but NOT the already-rolled-over cap.
func TestRestoreSnipeBuysWindowAndPools(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 8, 20, 8, 46, 0, 0, time.UTC)
	yesterday2350 := time.Date(2026, 8, 19, 23, 50, 0, 0, time.UTC) // inside 24h, prior UTC day
	old := time.Date(2026, 8, 19, 8, 0, 0, 0, time.UTC)             // > 24h before now
	store := &fakeSnipeBuyStore{rows: []*database.SnipeBuy{
		{ID: 1, ChatID: 7, TokenID: "tokMain", AmountUSD: 10, Pool: database.SnipeBuyPoolMain, BoughtAt: now.Add(-6 * time.Minute)},
		{ID: 2, ChatID: 7, TokenID: "tokDeep", AmountUSD: 5, Pool: database.SnipeBuyPoolDeep, BoughtAt: now.Add(-4 * time.Minute)},
		{ID: 3, ChatID: 7, TokenID: "tokYesterday", AmountUSD: 10, Pool: database.SnipeBuyPoolMain, BoughtAt: yesterday2350},
		{ID: 4, ChatID: 7, TokenID: "tokOld", AmountUSD: 10, Pool: database.SnipeBuyPoolMain, BoughtAt: old},
	}}
	b, watch := restoreTestBot(store, now)

	restored, err := b.restoreSnipeBuysAt(context.Background(), now)
	if err != nil {
		t.Fatalf("restore: %v", err)
	}
	if restored != 3 {
		t.Fatalf("restored = %d, want 3 (tokOld filtered by the 24h window)", restored)
	}

	// Bought record: main rows within 24h only.
	if !b.snipeBought.held(7, "tokMain") {
		t.Error("tokMain (main, today) missing from bought record")
	}
	if !b.snipeBought.held(7, "tokYesterday") {
		t.Error("tokYesterday (main, 23:50 yesterday, within 24h) missing from bought record")
	}
	if b.snipeBought.held(7, "tokDeep") {
		t.Error("tokDeep (deep) must NOT be in the bought record")
	}
	if b.snipeBought.held(7, "tokOld") {
		t.Error("tokOld (>24h) must be filtered out")
	}

	// Watcher latch: MarkBought for every restored row (main + deep), not tokOld.
	marked := map[string]bool{}
	for _, tok := range watch.bought {
		marked[tok] = true
	}
	for _, tok := range []string{"tokMain", "tokDeep", "tokYesterday"} {
		if !marked[tok] {
			t.Errorf("watcher MarkBought missing %q", tok)
		}
	}
	if marked["tokOld"] {
		t.Error("watcher MarkBought must not include the windowed-out tokOld")
	}

	// Main ledger seeded $10 (tokMain only; tokYesterday excluded — prior UTC day).
	if left, ok := b.snipeSpend.reserve(7, 41); ok || left != 40 {
		t.Errorf("main reserve(41) = (%.0f, %v), want (40, false) — $10 seeded for today only", left, ok)
	}
	if left, ok := b.snipeSpend.reserve(7, 40); !ok || left != 0 {
		t.Errorf("main reserve(40) = (%.0f, %v), want (0, true) — seed survives the first reserve's roll", left, ok)
	}

	// Deep ledger seeded $5.
	if left, ok := b.snipeDeepSpend.reserve(7, 16); ok || left != 15 {
		t.Errorf("deep reserve(16) = (%.0f, %v), want (15, false) — $5 seeded", left, ok)
	}
}

// TestRestoreSnipeBuysPostMidnightNoResurrection: a boot minutes after UTC
// midnight must not seed yesterday's spend into the fresh day, though it still
// restores the bought latch (within 24h).
func TestRestoreSnipeBuysPostMidnightNoResurrection(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 8, 20, 0, 5, 0, 0, time.UTC)
	store := &fakeSnipeBuyStore{rows: []*database.SnipeBuy{
		{ID: 1, ChatID: 7, TokenID: "tok", AmountUSD: 10, Pool: database.SnipeBuyPoolMain,
			BoughtAt: time.Date(2026, 8, 19, 23, 50, 0, 0, time.UTC)},
	}}
	b, watch := restoreTestBot(store, now)

	if _, err := b.restoreSnipeBuysAt(context.Background(), now); err != nil {
		t.Fatalf("restore: %v", err)
	}

	// Bought latch restored (within 24h)...
	if !b.snipeBought.held(7, "tok") {
		t.Error("pre-midnight buy must still restore the bought record")
	}
	if len(watch.bought) != 1 || watch.bought[0] != "tok" {
		t.Errorf("watcher bought = %v, want [tok]", watch.bought)
	}
	// ...but the cap is a fresh, full $50: yesterday's $10 did not resurrect.
	if left, ok := b.snipeSpend.reserve(7, 50); !ok || left != 0 {
		t.Errorf("post-midnight reserve(50) = (%.0f, %v), want (0, true) — no resurrected spend", left, ok)
	}
}

// TestRestoreSnipeBuysNilStore: a bot without a durable store restores nothing
// and never errors (pre-#84 behavior).
func TestRestoreSnipeBuysNilStore(t *testing.T) {
	t.Parallel()
	b := &Bot{
		snipeBought:    newSnipeBoughtRecord(),
		snipeSpend:     newSnipeSpendLedger(snipeAutoBuyDailyCapUSD),
		snipeDeepSpend: newSnipeSpendLedger(snipeDeepDailyCapUSD),
		snipeWatcher:   &fakeSnipeWatch{},
	}
	restored, err := b.RestoreSnipeBuys(context.Background())
	if err != nil || restored != 0 {
		t.Fatalf("nil-store restore = (%d, %v), want (0, nil)", restored, err)
	}
}

// TestSnipeRestartAmnesiaRegression is THE 08:46 replay (issue #84): a token
// auto-bought before a restart must not re-buy or re-alert after it, and the
// daily cap must carry the pre-restart spend forward.
//
// Bot A accepts the $10 in-band buy (write-through persists the row). A restart
// is simulated by a fresh Bot B — new bought record, new spend ledgers, new
// watcher — wired to the SAME store, then RestoreSnipeBuys. The re-alert
// suppression itself lives in the watcher's lazy MarkBought (proven in
// internal/live); here we prove the restore drives it and replays the cap.
func TestSnipeRestartAmnesiaRegression(t *testing.T) {
	t.Parallel()
	fixed := time.Date(2026, 8, 20, 8, 46, 0, 0, time.UTC)
	store := &fakeSnipeBuyStore{now: func() time.Time { return fixed }}

	// --- before the restart: Bot A auto-buys $10 and persists it ---
	h := newSnipeAutoBuyHarness(t, snipeHarnessConfig{ask: 0.17, askOK: true, user: snipeWalletUser()})
	h.bot.SetSnipeBuyStore(store)
	m := testSnipeMarket()
	h.bot.NotifySnipeAlert(7, m, 0.45, 0.17)
	if got := h.buys.count(); got != 1 {
		t.Fatalf("pre-restart buys = %d, want 1", got)
	}
	if len(store.savedCalls()) != 1 {
		t.Fatalf("pre-restart persisted rows = %d, want 1", len(store.savedCalls()))
	}

	// --- the restart: fresh state, same durable store ---
	b, watch := restoreTestBot(store, fixed)
	restored, err := b.restoreSnipeBuysAt(context.Background(), fixed)
	if err != nil || restored != 1 {
		t.Fatalf("restore = (%d, %v), want (1, nil)", restored, err)
	}

	// No re-alert: the watcher is re-marked bought for the token (the lazy latch
	// applies when the token is re-watched — see the live package tests).
	if len(watch.bought) != 1 || watch.bought[0] != m.TokenID {
		t.Errorf("post-restart watcher bought = %v, want [%s]", watch.bought, m.TokenID)
	}
	// No duplicate buy: the bought record holds the token (Gate 3a / sibling gate).
	if !b.snipeBought.held(7, m.TokenID) {
		t.Error("post-restart bought record missing the token — Gate 3a would allow a re-buy")
	}
	// Cap replay: $10 already spent, so a $45 reserve is refused ($50 − $10 = $40).
	if left, ok := b.snipeSpend.reserve(7, 45); ok || left != 40 {
		t.Errorf("post-restart reserve(45) = (%.0f, %v), want (40, false) — pre-restart $10 must persist", left, ok)
	}
	if _, ok := b.snipeSpend.reserve(7, 40); !ok {
		t.Error("post-restart reserve(40) refused — the remaining $40 headroom must survive")
	}
}
