package telegram

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"

	"github.com/Catorpilor/poly/internal/database"
	"github.com/Catorpilor/poly/internal/database/repositories"
	"github.com/Catorpilor/poly/internal/live"
	"github.com/Catorpilor/poly/internal/polymarket"
)

func TestSnipeAutoBoughtText(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		question    string
		outcome     string
		sessionHigh float64
		ask         float64
		orderID     string
		capLeft     float64
		want        []string
	}{
		{
			name:        "typical auto-buy",
			question:    "Will X win?",
			outcome:     "X",
			sessionHigh: 0.45,
			ask:         0.17,
			orderID:     "ord-1",
			capLeft:     40,
			want:        []string{"Auto-sniped $10", "Will X win?", "X", "was $0.45", "now $0.17", "ord-1", "$40"},
		},
		{
			name:        "cap exhausted shows zero left",
			question:    "Lakers vs. Trail Blazers",
			outcome:     "Lakers",
			sessionHigh: 0.40,
			ask:         0.20,
			orderID:     "ord-5",
			capLeft:     0,
			want:        []string{"Auto-sniped $10", "Lakers", "was $0.40", "now $0.20", "ord-5", "$0"},
		},
		{
			name:        "non-ascii title survives truncation",
			question:    "Кудерметова wins? " + strings.Repeat("好", 80),
			outcome:     "Да",
			sessionHigh: 0.52,
			ask:         0.05,
			orderID:     "ord-9",
			capLeft:     30,
			want:        []string{"Кудерметова", "Да", "was $0.52", "now $0.05", "ord-9", "$30"},
		},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := snipeAutoBoughtText(tt.question, tt.outcome, tt.sessionHigh, tt.ask, snipeAutoBuyUSD, tt.orderID, tt.capLeft)
			for _, want := range tt.want {
				if !strings.Contains(got, want) {
					t.Errorf("snipeAutoBoughtText missing %q in:\n%s", want, got)
				}
			}
		})
	}
}

func TestSnipeSpendLedgerCapAndRollover(t *testing.T) {
	t.Parallel()
	l := newSnipeSpendLedger(snipeAutoBuyDailyCapUSD)
	now := time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)
	l.now = func() time.Time { return now }

	// Five $10 reservations walk the cap down to zero.
	for i, wantLeft := range []float64{40, 30, 20, 10, 0} {
		left, ok := l.reserve(7, snipeAutoBuyUSD)
		if !ok || left != wantLeft {
			t.Fatalf("reserve #%d = (%.0f, %v), want (%.0f, true)", i+1, left, ok, wantLeft)
		}
	}
	// The sixth is refused whole.
	if left, ok := l.reserve(7, snipeAutoBuyUSD); ok || left != 0 {
		t.Fatalf("reserve past cap = (%.0f, %v), want (0, false)", left, ok)
	}
	// Another chat is unaffected.
	if _, ok := l.reserve(8, snipeAutoBuyUSD); !ok {
		t.Fatal("reserve for a different chat refused, want allowed")
	}
	// A released reservation frees headroom (failed buys don't consume the cap).
	l.release(7, snipeAutoBuyUSD)
	if left, ok := l.reserve(7, snipeAutoBuyUSD); !ok || left != 0 {
		t.Fatalf("reserve after release = (%.0f, %v), want (0, true)", left, ok)
	}

	// UTC-day rollover resets the accumulator.
	now = now.Add(24 * time.Hour)
	if left, ok := l.reserve(7, snipeAutoBuyUSD); !ok || left != 40 {
		t.Fatalf("reserve after rollover = (%.0f, %v), want (40, true)", left, ok)
	}

	// A release that lands after a rollover must not push spend negative:
	// the next reservation still sees a fresh day, not extra headroom.
	now = now.Add(24 * time.Hour)
	l.release(7, snipeAutoBuyUSD)
	if left, ok := l.reserve(7, snipeAutoBuyUSD); !ok || left != 40 {
		t.Fatalf("reserve after post-rollover release = (%.0f, %v), want (40, true)", left, ok)
	}
}

// tgMessage is one recorded Telegram API payload.
type tgMessage struct {
	chatID string
	text   string
	markup string
}

// tgRecorder is a fake Telegram API server handler recording sendMessage and
// editMessageText payloads. Safe for concurrent requests.
type tgRecorder struct {
	mu    sync.Mutex
	sends []tgMessage
	edits []tgMessage
}

func (rec *tgRecorder) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	msg := tgMessage{
		chatID: r.PostForm.Get("chat_id"),
		text:   r.PostForm.Get("text"),
		markup: r.PostForm.Get("reply_markup"),
	}
	rec.mu.Lock()
	switch {
	case strings.HasSuffix(r.URL.Path, "/sendMessage"):
		rec.sends = append(rec.sends, msg)
	case strings.HasSuffix(r.URL.Path, "/editMessageText"):
		rec.edits = append(rec.edits, msg)
	}
	rec.mu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	if strings.HasSuffix(r.URL.Path, "/getMe") {
		fmt.Fprint(w, `{"ok":true,"result":{"id":1,"is_bot":true,"first_name":"T","username":"test_bot"}}`)
		return
	}
	fmt.Fprint(w, `{"ok":true,"result":{"message_id":1,"chat":{"id":1}}}`)
}

func (rec *tgRecorder) sendCount() int {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	return len(rec.sends)
}

func (rec *tgRecorder) sentAt(t *testing.T, i int) tgMessage {
	t.Helper()
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if len(rec.sends) <= i {
		t.Fatalf("want at least %d sendMessage call(s), got %d", i+1, len(rec.sends))
	}
	return rec.sends[i]
}

func (rec *tgRecorder) lastEdit(t *testing.T) tgMessage {
	t.Helper()
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if len(rec.edits) == 0 {
		t.Fatal("want at least one editMessageText call, got none")
	}
	return rec.edits[len(rec.edits)-1]
}

// buyRecorder records snipe buy executor calls and serves a configurable
// result. Safe for concurrent calls.
type buyRecorder struct {
	mu     sync.Mutex
	calls  []buyCall
	result *polymarket.TradeResult
}

type buyCall struct {
	userID int64
	idx    int
	amount float64
}

func (br *buyRecorder) record(user *database.User, idx int, amount float64) *polymarket.TradeResult {
	br.mu.Lock()
	defer br.mu.Unlock()
	br.calls = append(br.calls, buyCall{userID: user.TelegramID, idx: idx, amount: amount})
	return br.result
}

func (br *buyRecorder) setResult(r *polymarket.TradeResult) {
	br.mu.Lock()
	defer br.mu.Unlock()
	br.result = r
}

func (br *buyRecorder) count() int {
	br.mu.Lock()
	defer br.mu.Unlock()
	return len(br.calls)
}

func (br *buyRecorder) call(t *testing.T, i int) buyCall {
	t.Helper()
	br.mu.Lock()
	defer br.mu.Unlock()
	if len(br.calls) <= i {
		t.Fatalf("want at least %d buy call(s), got %d", i+1, len(br.calls))
	}
	return br.calls[i]
}

// fakeSnipeWatch records MarkBought and WatchArmed calls; the other
// watch-registration methods are no-ops.
type fakeSnipeWatch struct {
	mu         sync.Mutex
	bought     []string
	armed      []live.SnipeMarket
	held       []live.SnipeMarket // WatchHeld (direct) registrations
	walked     []live.SnipeMarket // WatchWalked (series-walked) registrations (issue #102)
	walkedOnly map[string]bool    // tokenIDs for which WalkedOnlyHolder returns true
	siblings   []string           // returned by SiblingTokenIDs (boxed case-3 tests)
}

func (f *fakeSnipeWatch) WatchArmed(m live.SnipeMarket) {
	f.mu.Lock()
	f.armed = append(f.armed, m)
	f.mu.Unlock()
}
func (f *fakeSnipeWatch) UnwatchArmed(string) {}
func (f *fakeSnipeWatch) WatchHeld(_ int64, m live.SnipeMarket, _ time.Duration) {
	f.mu.Lock()
	f.held = append(f.held, m)
	delete(f.walkedOnly, m.TokenID) // direct always wins: a direct register upgrades a walked entry
	f.mu.Unlock()
}
func (f *fakeSnipeWatch) WatchWalked(_ int64, m live.SnipeMarket, _ time.Duration) {
	f.mu.Lock()
	f.walked = append(f.walked, m)
	f.mu.Unlock()
}
func (f *fakeSnipeWatch) WalkedOnlyHolder(_ int64, tokenID string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.walkedOnly[tokenID]
}

// markWalkedOnly makes WalkedOnlyHolder report tokenID as walked-only — the seam
// the series-walked gate tests drive (issue #102).
func (f *fakeSnipeWatch) markWalkedOnly(tokenID string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.walkedOnly == nil {
		f.walkedOnly = make(map[string]bool)
	}
	f.walkedOnly[tokenID] = true
}
func (f *fakeSnipeWatch) heldTokens() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, 0, len(f.held))
	for _, m := range f.held {
		out = append(out, m.TokenID)
	}
	return out
}
func (f *fakeSnipeWatch) walkedTokens() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, 0, len(f.walked))
	for _, m := range f.walked {
		out = append(out, m.TokenID)
	}
	return out
}
func (f *fakeSnipeWatch) RenewHeldMarket(int64, string, time.Duration) bool { return true }
func (f *fakeSnipeWatch) EventSlugOf(string) string                         { return "" }

func (f *fakeSnipeWatch) MarkBought(tokenID string) {
	f.mu.Lock()
	f.bought = append(f.bought, tokenID)
	f.mu.Unlock()
}

// siblings, when set, is returned by SiblingTokenIDs for any query — enough for
// the boxed-tier case-3 tests.
func (f *fakeSnipeWatch) SiblingTokenIDs(_, _ string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.siblings...)
}

func (f *fakeSnipeWatch) boughtCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.bought)
}

func (f *fakeSnipeWatch) armedTokens() []live.SnipeMarket {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]live.SnipeMarket(nil), f.armed...)
}

// fakeSnipeUserRepo serves one fixed user for every telegram ID; nil means
// "no wallet".
type fakeSnipeUserRepo struct {
	repositories.UserRepository
	user *database.User
}

func (f *fakeSnipeUserRepo) GetByTelegramID(context.Context, int64) (*database.User, error) {
	return f.user, nil
}

// fakeAskSource is a fixed-answer SnipeAskSource.
type fakeAskSource struct {
	ask   float64
	askOK bool
	bid   float64
	bidOK bool
}

func (f *fakeAskSource) BestAsk(string) (float64, bool) { return f.ask, f.askOK }
func (f *fakeAskSource) BestBid(string) (float64, bool) { return f.bid, f.bidOK }

type snipeHarnessConfig struct {
	ask       float64
	askOK     bool
	bid       float64 // fresh best bid for Gate 2; used only when bidSet
	bidOK     bool
	bidSet    bool                    // false ⇒ harness defaults to a healthy bid == ask
	user      *database.User          // nil = no wallet
	buyResult *polymarket.TradeResult // nil = success with OrderID ord-auto
	positions []*polymarket.Position  // Gate 3 positions-check seam (empty by default)
	posErr    error                   // Gate 3 positions-check API error
}

// snipeAutoBuyHarness wires a Bot for NotifySnipeAlert / handleSnipeCallback
// tests: recording fake Telegram server, httptest Gamma server for
// testSnipeMarket, fake user repo / ask source / watcher, recording buy
// executor.
type snipeAutoBuyHarness struct {
	bot   *Bot
	tg    *tgRecorder
	watch *fakeSnipeWatch
	buys  *buyRecorder
}

func newSnipeAutoBuyHarness(t *testing.T, cfg snipeHarnessConfig) *snipeAutoBuyHarness {
	t.Helper()

	tg := &tgRecorder{}
	tgSrv := httptest.NewServer(tg)
	t.Cleanup(tgSrv.Close)
	api, err := tgbotapi.NewBotAPIWithClient("test-token", tgSrv.URL+"/bot%s/%s", tgSrv.Client())
	if err != nil {
		t.Fatalf("NewBotAPIWithClient: %v", err)
	}

	m := testSnipeMarket()
	gamma := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/markets/"+m.MarketID {
			fmt.Fprintf(w,
				`{"id":%q,"question":%q,"conditionId":"cond-1","outcomes":"[\"Lakers\",\"Trail Blazers\"]","clobTokenIds":"[\"%s\",\"%s\"]"}`,
				m.MarketID, m.Question, m.TokenID, strings.Repeat("8", 78))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(gamma.Close)

	buys := &buyRecorder{result: cfg.buyResult}
	if buys.result == nil {
		buys.result = &polymarket.TradeResult{Success: true, OrderID: "ord-auto"}
	}
	watch := &fakeSnipeWatch{}
	// Fresh bid for Gate 2 (corpse-spread). Default: a healthy tight book
	// (bid == ask) so in-band auto-buys clear the gate; corpse tests set bidSet.
	bid, bidOK := cfg.ask, true
	if cfg.bidSet {
		bid, bidOK = cfg.bid, cfg.bidOK
	}
	b := &Bot{
		api:             api,
		userRepo:        &fakeSnipeUserRepo{user: cfg.user},
		snipeFeed:       &fakeAskSource{ask: cfg.ask, askOK: cfg.askOK, bid: bid, bidOK: bidOK},
		snipeAlerts:     newSnipeAlertRegistry(),
		snipeSpend:      newSnipeSpendLedger(snipeAutoBuyDailyCapUSD),
		snipeDeepSpend:  newSnipeSpendLedger(snipeDeepDailyCapUSD),
		snipeBought:     newSnipeBoughtRecord(),
		snipeBoxedLatch: newSnipeBoxedLatch(),
		snipePositions:  &fakePositionSource{positions: cfg.positions, err: cfg.posErr},
		snipeWatcher:    watch,
		snipeMarkets:    polymarket.NewMarketClientWithURL(gamma.URL),
	}
	// The default fixture reports an executor-confirmed fill at the guard ask
	// (size = amount/ask matches the stake-derivation the arm tests assert), so
	// arm-behavior tests exercise the immediate post-fill ceremony. Tests that
	// target the #92 confirm path pass an explicit buyResult, which is served
	// verbatim.
	defaultFixture := cfg.buyResult == nil
	b.snipeBuyExec = func(_ context.Context, user *database.User, _ *polymarket.GammaMarket, idx int, amount float64) *polymarket.TradeResult {
		r := buys.record(user, idx, amount)
		if defaultFixture && r != nil && r.Success && r.FilledSize == 0 && cfg.ask > 0 {
			c := *r
			c.FilledSize = amount / cfg.ask
			c.AveragePrice = cfg.ask
			return &c
		}
		return r
	}
	return &snipeAutoBuyHarness{bot: b, tg: tg, watch: watch, buys: buys}
}

func snipeWalletUser() *database.User {
	return &database.User{TelegramID: 7, EOAAddress: "0xabc", EncryptedKey: "enc"}
}

// callbackData extracts a button's callback data from a recorded reply_markup
// JSON by its label.
func callbackData(t *testing.T, markup, label string) string {
	t.Helper()
	var kb struct {
		InlineKeyboard [][]struct {
			Text string `json:"text"`
			Data string `json:"callback_data"`
		} `json:"inline_keyboard"`
	}
	if err := json.Unmarshal([]byte(markup), &kb); err != nil {
		t.Fatalf("parse reply_markup %q: %v", markup, err)
	}
	for _, row := range kb.InlineKeyboard {
		for _, btn := range row {
			if btn.Text == label {
				return btn.Data
			}
		}
	}
	t.Fatalf("button %q not in markup %s", label, markup)
	return ""
}

func snipeTapUpdate(userID int64, data string) *tgbotapi.Update {
	return &tgbotapi.Update{
		CallbackQuery: &tgbotapi.CallbackQuery{
			From:    &tgbotapi.User{ID: userID},
			Message: &tgbotapi.Message{MessageID: 42, Chat: &tgbotapi.Chat{ID: userID}},
			Data:    data,
		},
	}
}

// TestNotifySnipeAlertAutoBuys covers the v2 happy path: one $10 buy,
// MarkBought, the auto-sniped message with both buttons — and the registry
// entry stays claimable, so the Add-$25 tap buys and only the tap after THAT
// reports used.
func TestNotifySnipeAlertAutoBuys(t *testing.T) {
	t.Parallel()
	h := newSnipeAutoBuyHarness(t, snipeHarnessConfig{ask: 0.17, askOK: true, user: snipeWalletUser()})
	m := testSnipeMarket()

	h.bot.NotifySnipeAlert(7, m, 0.45, 0.17)

	if got := h.buys.count(); got != 1 {
		t.Fatalf("buy calls = %d, want 1", got)
	}
	if c := h.buys.call(t, 0); c.amount != snipeAutoBuyUSD || c.idx != 0 || c.userID != 7 {
		t.Errorf("auto-buy call = %+v, want $10 at index 0 for user 7", c)
	}
	if got := h.watch.boughtCount(); got != 1 {
		t.Errorf("MarkBought calls = %d, want 1", got)
	}
	sent := h.tg.sentAt(t, 0)
	for _, want := range []string{"Auto-sniped", "ord-auto", "was $0.45", "now $0.17", "$40"} {
		if !strings.Contains(sent.text, want) {
			t.Errorf("auto-sniped message missing %q in:\n%s", want, sent.text)
		}
	}
	if callbackData(t, sent.markup, "🎯 Arm SL/TP") != "sltp_list" {
		t.Error("Arm SL/TP button missing or wrong callback")
	}

	// The Add-$25 button rides the SAME registry entry — the auto-buy did not
	// claim it.
	addData := callbackData(t, sent.markup, "⚡ Add $25")
	if !strings.HasPrefix(addData, "snipe:") || !strings.HasSuffix(addData, ":25") {
		t.Fatalf("Add $25 callback = %q, want snipe:<id>:25", addData)
	}
	h.bot.handleSnipeCallback(context.Background(), snipeTapUpdate(7, addData))
	if got := h.buys.count(); got != 2 {
		t.Fatalf("buy calls after Add $25 = %d, want 2", got)
	}
	if c := h.buys.call(t, 1); c.amount != 25 {
		t.Errorf("Add $25 buy amount = %.2f, want 25", c.amount)
	}
	if edit := h.tg.lastEdit(t); !strings.Contains(edit.text, "Sniped!") || !strings.Contains(edit.text, "$25.00") {
		t.Errorf("Add $25 fill message = %q, want Sniped! for $25.00", edit.text)
	}

	// Double-tap after the top-up refuses: the claim is spent now.
	h.bot.handleSnipeCallback(context.Background(), snipeTapUpdate(7, addData))
	if got := h.buys.count(); got != 2 {
		t.Fatalf("buy calls after double-tap = %d, want still 2", got)
	}
	if msg := h.tg.sentAt(t, 1); !strings.Contains(msg.text, "Already handled") {
		t.Errorf("double-tap reply = %q, want Already handled", msg.text)
	}
}

// TestNotifySnipeAlertGuardRefusalFallsBack: a repriced ask skips the auto-buy
// and delivers the unchanged manual alert; the cap reservation is refunded.
func TestNotifySnipeAlertGuardRefusalFallsBack(t *testing.T) {
	t.Parallel()
	h := newSnipeAutoBuyHarness(t, snipeHarnessConfig{ask: 0.31, askOK: true, user: snipeWalletUser()})

	h.bot.NotifySnipeAlert(7, testSnipeMarket(), 0.45, 0.31)

	if got := h.buys.count(); got != 0 {
		t.Fatalf("buy calls = %d, want 0", got)
	}
	if got := h.watch.boughtCount(); got != 0 {
		t.Errorf("MarkBought calls = %d, want 0", got)
	}
	sent := h.tg.sentAt(t, 0)
	if !strings.Contains(sent.text, "Comeback Snipe") || strings.Contains(sent.text, "Auto-sniped") {
		t.Errorf("fallback alert wrong:\n%s", sent.text)
	}
	if strings.Contains(sent.text, "cap reached") {
		t.Errorf("guard fallback must not carry the cap note:\n%s", sent.text)
	}
	if !strings.Contains(sent.text, "Auto-buy skipped") || !strings.Contains(sent.text, "moved past the snipe guard") {
		t.Errorf("guard fallback must say why the auto-buy was skipped (issue #50):\n%s", sent.text)
	}
	callbackData(t, sent.markup, "⚡ Snipe $10")
	callbackData(t, sent.markup, "⚡ Snipe $25")
	// The failed attempt released its reservation: the full cap is available.
	if _, ok := h.bot.snipeSpend.reserve(7, snipeAutoBuyDailyCapUSD); !ok {
		t.Error("cap not refunded after guard refusal")
	}
}

// TestNotifySnipeAlertNoWalletFallsBack: recipients without a wallet get the
// manual alert and never reach the buy path.
func TestNotifySnipeAlertNoWalletFallsBack(t *testing.T) {
	t.Parallel()
	h := newSnipeAutoBuyHarness(t, snipeHarnessConfig{ask: 0.17, askOK: true, user: nil})

	h.bot.NotifySnipeAlert(7, testSnipeMarket(), 0.45, 0.17)

	if got := h.buys.count(); got != 0 {
		t.Fatalf("buy calls = %d, want 0", got)
	}
	sent := h.tg.sentAt(t, 0)
	if !strings.Contains(sent.text, "Comeback Snipe") || strings.Contains(sent.text, "Auto-sniped") {
		t.Errorf("fallback alert wrong:\n%s", sent.text)
	}
	if !strings.Contains(sent.text, "no trading wallet") {
		t.Errorf("no-wallet fallback must say why the auto-buy was skipped (issue #50):\n%s", sent.text)
	}
	callbackData(t, sent.markup, "⚡ Snipe $10")
	callbackData(t, sent.markup, "⚡ Snipe $25")
	if _, ok := h.bot.snipeSpend.reserve(7, snipeAutoBuyDailyCapUSD); !ok {
		t.Error("cap consumed with no wallet")
	}
}

// TestNotifySnipeAlertBuyFailureFallsBack: an executor rejection falls back to
// the manual alert without MarkBought, and refunds the cap reservation.
func TestNotifySnipeAlertBuyFailureFallsBack(t *testing.T) {
	t.Parallel()
	h := newSnipeAutoBuyHarness(t, snipeHarnessConfig{
		ask: 0.17, askOK: true, user: snipeWalletUser(),
		buyResult: &polymarket.TradeResult{Success: false, ErrorMsg: "not enough balance"},
	})
	m := testSnipeMarket()

	h.bot.NotifySnipeAlert(7, m, 0.45, 0.17)

	if got := h.buys.count(); got != 1 {
		t.Fatalf("buy attempts = %d, want 1", got)
	}
	if got := h.watch.boughtCount(); got != 0 {
		t.Errorf("MarkBought calls = %d, want 0 after failed buy", got)
	}
	sent := h.tg.sentAt(t, 0)
	if !strings.Contains(sent.text, "Comeback Snipe") || strings.Contains(sent.text, "Auto-sniped") {
		t.Errorf("fallback alert wrong:\n%s", sent.text)
	}
	if !strings.Contains(sent.text, "order was rejected") {
		t.Errorf("buy-failure fallback must say why the auto-buy was skipped (issue #50):\n%s", sent.text)
	}
	callbackData(t, sent.markup, "⚡ Snipe $10")

	// The failed buy refunded its reservation: the next success reports a full
	// $40 left, not $30.
	h.buys.setResult(&polymarket.TradeResult{Success: true, OrderID: "ord-2"})
	h.bot.NotifySnipeAlert(7, m, 0.45, 0.17)
	if next := h.tg.sentAt(t, 1); !strings.Contains(next.text, "Auto-sniped") || !strings.Contains(next.text, "$40") {
		t.Errorf("post-failure success message = %q, want Auto-sniped with $40 cap left", next.text)
	}
}

// TestSnipeRefuseDeepBuy: the Deep Crash guard only buys inside
// [SnipeDeepFloor, SnipeMinAsk) — ADR 0007's strict zone check.
func TestSnipeRefuseDeepBuy(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		ask    float64
		ok     bool
		refuse bool
	}{
		{"mid-zone buys", 0.02, true, false},
		{"exactly at the floor buys", live.SnipeDeepFloor, true, false},
		{"just above the zone refuses (in-band territory)", live.SnipeMinAsk, true, true},
		{"bounced out refuses", 0.08, true, true},
		{"dust below the floor refuses", 0.004, true, true},
		{"no ask refuses", 0, false, true},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := snipeRefuseDeepBuy(tt.ask, tt.ok); got != tt.refuse {
				t.Errorf("snipeRefuseDeepBuy(%v, %v) = %v, want %v", tt.ask, tt.ok, got, tt.refuse)
			}
		})
	}
}

// TestSnipeDeepText: the Deep Crash body carries the term, both prices, the
// multiple, and the corpse warning.
func TestSnipeDeepText(t *testing.T) {
	t.Parallel()
	got := snipeDeepText("Will X win?", "X", 0.09, 0.02)
	for _, want := range []string{"Deep Crash", "Will X win?", "$0.09", "$0.020", "50×", "Corpse territory"} {
		if !strings.Contains(got, want) {
			t.Errorf("snipeDeepText missing %q in:\n%s", want, got)
		}
	}
	bought := snipeDeepBoughtText("Will X win?", "X", 0.09, 0.02, snipeDeepBuyUSD, "ord-d1", 15)
	for _, want := range []string{"$5 auto-bought", "ord-d1", "Deep pool left today: $15"} {
		if !strings.Contains(bought, want) {
			t.Errorf("snipeDeepBoughtText missing %q in:\n%s", want, bought)
		}
	}
}

// TestNotifySnipeDeepCrash: the $5 executes from the deep pool behind the
// strict zone guard, and each failure mode degrades to the manual Deep Crash
// alert with a reason.
func TestNotifySnipeDeepCrash(t *testing.T) {
	t.Parallel()

	t.Run("in-zone ask buys $5 from the deep pool", func(t *testing.T) {
		t.Parallel()
		h := newSnipeAutoBuyHarness(t, snipeHarnessConfig{ask: 0.02, askOK: true, user: snipeWalletUser()})
		h.bot.NotifySnipeDeepCrash(7, testSnipeMarket(), 0.45, 0.02, 0.09, 3*time.Minute)

		if got := h.buys.count(); got != 1 {
			t.Fatalf("buy calls = %d, want 1", got)
		}
		if c := h.buys.call(t, 0); c.amount != snipeDeepBuyUSD {
			t.Errorf("deep buy amount = %v, want %v", c.amount, snipeDeepBuyUSD)
		}
		sent := h.tg.sentAt(t, 0)
		if !strings.Contains(sent.text, "Deep Crash") || !strings.Contains(sent.text, "$5 auto-bought") {
			t.Errorf("deep bought message wrong:\n%s", sent.text)
		}
		// The MAIN pool is untouched — full $50 still reservable.
		if _, ok := h.bot.snipeSpend.reserve(7, snipeAutoBuyDailyCapUSD); !ok {
			t.Error("deep buy consumed the main pool")
		}
	})

	t.Run("bounced-out ask skips the buy with a reason", func(t *testing.T) {
		t.Parallel()
		h := newSnipeAutoBuyHarness(t, snipeHarnessConfig{ask: 0.08, askOK: true, user: snipeWalletUser()})
		h.bot.NotifySnipeDeepCrash(7, testSnipeMarket(), 0.45, 0.02, 0.09, time.Minute)

		if got := h.buys.count(); got != 0 {
			t.Fatalf("buy calls = %d, want 0 (zone guard)", got)
		}
		sent := h.tg.sentAt(t, 0)
		if !strings.Contains(sent.text, "Deep Crash") || !strings.Contains(sent.text, "Auto-buy skipped") {
			t.Errorf("deep skip message wrong:\n%s", sent.text)
		}
		// Refunded: the full deep pool remains.
		if _, ok := h.bot.snipeDeepSpend.reserve(7, snipeDeepDailyCapUSD); !ok {
			t.Error("deep pool not refunded after guard refusal")
		}
	})

	t.Run("deep pool exhausts after four $5s and degrades with the cap note", func(t *testing.T) {
		t.Parallel()
		h := newSnipeAutoBuyHarness(t, snipeHarnessConfig{ask: 0.02, askOK: true, user: snipeWalletUser()})
		m := testSnipeMarket()
		for i := 0; i < 4; i++ {
			h.bot.NotifySnipeDeepCrash(7, m, 0.45, 0.02, 0.09, time.Minute)
		}
		if got := h.buys.count(); got != 4 {
			t.Fatalf("buys before exhaustion = %d, want 4", got)
		}
		h.bot.NotifySnipeDeepCrash(7, m, 0.45, 0.02, 0.09, time.Minute)
		if got := h.buys.count(); got != 4 {
			t.Fatalf("buy past the deep pool executed")
		}
		sent := h.tg.sentAt(t, 4)
		if !strings.Contains(sent.text, "Deep pool exhausted") {
			t.Errorf("exhaustion message missing the deep cap note:\n%s", sent.text)
		}
	})
}

// TestNotifySnipeAlertDailyCap: the fifth $10 auto-buy lands exactly on the
// $50 boundary and executes; the sixth falls back to the manual alert with the
// cap note; the UTC-day rollover re-arms the auto-buy.
func TestNotifySnipeAlertDailyCap(t *testing.T) {
	t.Parallel()
	h := newSnipeAutoBuyHarness(t, snipeHarnessConfig{ask: 0.17, askOK: true, user: snipeWalletUser()})
	now := time.Date(2026, 8, 4, 23, 30, 0, 0, time.UTC)
	h.bot.snipeSpend.now = func() time.Time { return now }
	m := testSnipeMarket()

	for i := 0; i < 5; i++ {
		h.bot.NotifySnipeAlert(7, m, 0.45, 0.17)
	}
	if got := h.buys.count(); got != 5 {
		t.Fatalf("buy calls after five alerts = %d, want 5", got)
	}
	if fifth := h.tg.sentAt(t, 4); !strings.Contains(fifth.text, "Auto-sniped") || !strings.Contains(fifth.text, "$0") {
		t.Errorf("fifth alert = %q, want Auto-sniped with $0 cap left", fifth.text)
	}

	// Sixth: cap reached — manual alert with the one-line note, no buy.
	h.bot.NotifySnipeAlert(7, m, 0.45, 0.17)
	if got := h.buys.count(); got != 5 {
		t.Fatalf("buy calls after capped alert = %d, want still 5", got)
	}
	sixth := h.tg.sentAt(t, 5)
	if !strings.Contains(sixth.text, "Comeback Snipe") || !strings.Contains(sixth.text, "auto-snipe cap reached") {
		t.Errorf("capped alert = %q, want manual alert with cap note", sixth.text)
	}
	callbackData(t, sixth.markup, "⚡ Snipe $10")

	// Past UTC midnight the cap resets and the auto-buy fires again.
	now = now.Add(time.Hour)
	h.bot.NotifySnipeAlert(7, m, 0.45, 0.17)
	if got := h.buys.count(); got != 6 {
		t.Fatalf("buy calls after rollover = %d, want 6", got)
	}
	if seventh := h.tg.sentAt(t, 6); !strings.Contains(seventh.text, "Auto-sniped") || !strings.Contains(seventh.text, "$40") {
		t.Errorf("post-rollover alert = %q, want Auto-sniped with $40 cap left", seventh.text)
	}
}

// TestNotifySnipeAlertCapConcurrencySafe: racing alerts never overshoot the
// daily cap and every alert is delivered. Run with -race.
func TestNotifySnipeAlertCapConcurrencySafe(t *testing.T) {
	t.Parallel()
	h := newSnipeAutoBuyHarness(t, snipeHarnessConfig{ask: 0.17, askOK: true, user: snipeWalletUser()})
	m := testSnipeMarket()

	const alerts = 10
	var wg sync.WaitGroup
	for i := 0; i < alerts; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			h.bot.NotifySnipeAlert(7, m, 0.45, 0.17)
		}()
	}
	wg.Wait()

	if got := h.buys.count(); got != 5 {
		t.Errorf("buy calls = %d, want exactly 5 ($50 cap / $10)", got)
	}
	if got := h.tg.sendCount(); got != alerts {
		t.Errorf("alerts delivered = %d, want %d — delivery must never be blocked", got, alerts)
	}
	var auto, capped int
	h.tg.mu.Lock()
	for _, msg := range h.tg.sends {
		switch {
		case strings.Contains(msg.text, "Auto-sniped"):
			auto++
		case strings.Contains(msg.text, "auto-snipe cap reached"):
			capped++
		}
	}
	h.tg.mu.Unlock()
	if auto != 5 || capped != 5 {
		t.Errorf("messages = %d auto-sniped + %d capped, want 5 + 5", auto, capped)
	}
}
