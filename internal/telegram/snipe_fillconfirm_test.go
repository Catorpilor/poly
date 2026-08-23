package telegram

import (
	"context"
	"errors"
	"fmt"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"

	"github.com/Catorpilor/poly/internal/database"
	"github.com/Catorpilor/poly/internal/polymarket"
)

// fillReading is one scripted answer from the fake order-status probe.
type fillReading struct {
	matched, price float64
	open, found    bool
	err            error
}

// fillProbe scripts the order-status/cancel seams for snipeConfirmFillThenArm
// (issue #92). Readings are served sequentially (the last repeats); once the
// order is killed, afterKill is served instead when set.
type fillProbe struct {
	mu        sync.Mutex
	calls     int
	readings  []fillReading
	afterKill *fillReading
	killed    []string
	killErr   error
}

func (p *fillProbe) check(_ context.Context, _ *database.User, orderID string) (float64, float64, bool, bool, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls++
	if p.afterKill != nil && len(p.killed) > 0 {
		r := *p.afterKill
		return r.matched, r.price, r.open, r.found, r.err
	}
	i := p.calls - 1
	if i >= len(p.readings) {
		i = len(p.readings) - 1
	}
	r := p.readings[i]
	return r.matched, r.price, r.open, r.found, r.err
}

func (p *fillProbe) kill(_ context.Context, _ *database.User, orderID string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.killErr != nil {
		return p.killErr
	}
	p.killed = append(p.killed, orderID)
	return nil
}

func (p *fillProbe) killCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.killed)
}

func (p *fillProbe) checkCalls() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls
}

// releaseRecorder captures stake refunds issued by the confirm path.
type releaseRecorder struct {
	mu       sync.Mutex
	released []float64
}

func (r *releaseRecorder) release(amount float64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.released = append(r.released, amount)
}

func (r *releaseRecorder) total() float64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	sum := 0.0
	for _, a := range r.released {
		sum += a
	}
	return sum
}

func newFillConfirmBot(t *testing.T, probe *fillProbe) (*Bot, *tgRecorder, *recordingArmRepo) {
	t.Helper()
	tg := &tgRecorder{}
	tgSrv := httptest.NewServer(tg)
	t.Cleanup(tgSrv.Close)
	api, err := tgbotapi.NewBotAPIWithClient("test-token", tgSrv.URL+"/bot%s/%s", tgSrv.Client())
	if err != nil {
		t.Fatalf("NewBotAPIWithClient: %v", err)
	}
	repo := &recordingArmRepo{}
	b := &Bot{api: api, sltpArmRepo: repo}
	b.snipeFillPoll = 2 * time.Millisecond
	b.snipeFillWindow = 120 * time.Millisecond
	if probe != nil {
		b.snipeOrderFill = probe.check
		b.snipeOrderKill = probe.kill
	}
	return b, tg, repo
}

func fillConfirmResult(orderID string, ask float64) snipeBuyResult {
	return snipeBuyResult{
		outcome: snipeBuyFilled,
		ask:     ask,
		orderID: orderID,
		market:  &polymarket.GammaMarket{ID: "m1", ConditionID: "cond-1"},
		idx:     0,
	}
}

func fillConfirmUser() *database.User {
	return &database.User{TelegramID: 7, EOAAddress: "0xabc", EncryptedKey: "enc"}
}

// An executor-confirmed fill (immediate match) arms directly with the
// executor's size/price and never touches the order-status probe.
func TestSnipeConfirmFill_ImmediateMatch_ArmsWithoutProbe(t *testing.T) {
	t.Parallel()
	probe := &fillProbe{readings: []fillReading{{err: errors.New("must not be called")}}}
	b, _, repo := newFillConfirmBot(t, probe)
	rel := &releaseRecorder{}

	res := fillConfirmResult("ord-1", 0.07)
	res.filledSize = 71.43
	res.filledPrice = 0.07
	b.snipeConfirmFillThenArm(7, fillConfirmUser(), "tok-1", "Q?", "Lakers", res, 5, rel.release)

	arms := repo.armedCalls()
	if len(arms) != 1 {
		t.Fatalf("want 1 arm, got %d", len(arms))
	}
	if arms[0].SharesAtArm != 71.43 || arms[0].AvgPrice != 0.07 {
		t.Errorf("arm = %.2f @ %.3f, want 71.43 @ 0.070", arms[0].SharesAtArm, arms[0].AvgPrice)
	}
	if probe.checkCalls() != 0 {
		t.Errorf("probe called %d times for a confirmed fill, want 0", probe.checkCalls())
	}
	if rel.total() != 0 {
		t.Errorf("released %.2f for a confirmed fill, want 0", rel.total())
	}
}

// A resting order that matches during the window arms with the probe's
// confirmed size and price — no cancel, no refund.
func TestSnipeConfirmFill_RestingThenMatched_Arms(t *testing.T) {
	t.Parallel()
	probe := &fillProbe{readings: []fillReading{
		{open: true, found: true},
		{matched: 71.43, price: 0.07, open: false, found: true},
	}}
	b, _, repo := newFillConfirmBot(t, probe)
	rel := &releaseRecorder{}

	b.snipeConfirmFillThenArm(7, fillConfirmUser(), "tok-1", "Q?", "Lakers", fillConfirmResult("ord-2", 0.10), 5, rel.release)

	arms := repo.armedCalls()
	if len(arms) != 1 {
		t.Fatalf("want 1 arm, got %d", len(arms))
	}
	if arms[0].SharesAtArm != 71.43 || arms[0].AvgPrice != 0.07 {
		t.Errorf("arm = %.2f @ %.3f, want the probe-confirmed 71.43 @ 0.070", arms[0].SharesAtArm, arms[0].AvgPrice)
	}
	if probe.killCount() != 0 {
		t.Errorf("cancelled a filled order")
	}
	if rel.total() != 0 {
		t.Errorf("released %.2f for a filled order, want 0", rel.total())
	}
}

// An order that never fills inside the window is cancelled; the stake is
// released and the user gets the refund note. No arm is created.
func TestSnipeConfirmFill_NeverFilled_CancelsAndRefunds(t *testing.T) {
	t.Parallel()
	probe := &fillProbe{
		readings:  []fillReading{{open: true, found: true}},
		afterKill: &fillReading{open: false, found: true},
	}
	b, tg, repo := newFillConfirmBot(t, probe)
	rel := &releaseRecorder{}

	b.snipeConfirmFillThenArm(7, fillConfirmUser(), "tok-1", "Q?", "Lakers", fillConfirmResult("ord-3", 0.10), 5, rel.release)

	if arms := repo.armedCalls(); len(arms) != 0 {
		t.Fatalf("armed an unfilled order: %+v", arms[0])
	}
	if probe.killCount() != 1 {
		t.Errorf("kill calls = %d, want 1", probe.killCount())
	}
	if got := rel.total(); got != 5 {
		t.Errorf("released %.2f, want the full $5 stake", got)
	}
	assertSentContains(t, tg, "didn't fill")
}

// A cancel that races a fill (the post-cancel reading shows matched shares)
// arms with the matched size instead of refunding.
func TestSnipeConfirmFill_CancelRacesFill_Arms(t *testing.T) {
	t.Parallel()
	probe := &fillProbe{
		readings:  []fillReading{{open: true, found: true}},
		afterKill: &fillReading{matched: 71.43, price: 0.07, open: false, found: true},
	}
	b, _, repo := newFillConfirmBot(t, probe)
	rel := &releaseRecorder{}

	b.snipeConfirmFillThenArm(7, fillConfirmUser(), "tok-1", "Q?", "Lakers", fillConfirmResult("ord-4", 0.10), 5, rel.release)

	arms := repo.armedCalls()
	if len(arms) != 1 {
		t.Fatalf("want 1 arm after cancel-races-fill, got %d", len(arms))
	}
	if arms[0].SharesAtArm != 71.43 {
		t.Errorf("arm shares = %.2f, want 71.43", arms[0].SharesAtArm)
	}
	if got := rel.total(); got != 0 {
		t.Errorf("released %.2f for a fully filled stake ($71.43*0.07≈$5), want 0", got)
	}
}

// A partial fill at the window edge cancels the remainder, arms the matched
// shares, and refunds only the unspent slice of the stake.
func TestSnipeConfirmFill_PartialAtWindow_ArmsMatchedRefundsRest(t *testing.T) {
	t.Parallel()
	probe := &fillProbe{
		readings:  []fillReading{{matched: 30, price: 0.07, open: true, found: true}},
		afterKill: &fillReading{matched: 30, price: 0.07, open: false, found: true},
	}
	b, _, repo := newFillConfirmBot(t, probe)
	rel := &releaseRecorder{}

	b.snipeConfirmFillThenArm(7, fillConfirmUser(), "tok-1", "Q?", "Lakers", fillConfirmResult("ord-5", 0.10), 5, rel.release)

	arms := repo.armedCalls()
	if len(arms) != 1 {
		t.Fatalf("want 1 arm for the partial fill, got %d", len(arms))
	}
	if arms[0].SharesAtArm != 30 || arms[0].AvgPrice != 0.07 {
		t.Errorf("arm = %.2f @ %.3f, want 30 @ 0.070", arms[0].SharesAtArm, arms[0].AvgPrice)
	}
	if probe.killCount() != 1 {
		t.Errorf("kill calls = %d, want 1 (cancel the resting remainder)", probe.killCount())
	}
	want := 5 - 30*0.07 // $2.90 unspent
	if got := rel.total(); got < want-0.001 || got > want+0.001 {
		t.Errorf("released %.2f, want %.2f", got, want)
	}
}

// When the order status can never be read (probe and cancel both erroring),
// the path fails closed: no arm, no refund, and a warning DM — never a silent
// guess in either direction.
func TestSnipeConfirmFill_UnknownStatus_FailsClosed(t *testing.T) {
	t.Parallel()
	probe := &fillProbe{
		readings: []fillReading{{err: errors.New("clob down")}},
		killErr:  errors.New("clob down"),
	}
	b, tg, repo := newFillConfirmBot(t, probe)
	rel := &releaseRecorder{}

	b.snipeConfirmFillThenArm(7, fillConfirmUser(), "tok-1", "Q?", "Lakers", fillConfirmResult("ord-6", 0.10), 5, rel.release)

	if arms := repo.armedCalls(); len(arms) != 0 {
		t.Fatalf("armed on unknown status: %+v", arms[0])
	}
	if got := rel.total(); got != 0 {
		t.Errorf("released %.2f on unknown status, want 0 (fail closed)", got)
	}
	assertSentContains(t, tg, "stays counted")
}

// A matched probe reading with no usable price falls back to the guard ask.
func TestSnipeConfirmFill_MatchedWithoutPrice_UsesGuardAsk(t *testing.T) {
	t.Parallel()
	probe := &fillProbe{readings: []fillReading{{matched: 50, open: false, found: true}}}
	b, _, repo := newFillConfirmBot(t, probe)
	rel := &releaseRecorder{}

	b.snipeConfirmFillThenArm(7, fillConfirmUser(), "tok-1", "Q?", "Lakers", fillConfirmResult("ord-7", 0.10), 5, rel.release)

	arms := repo.armedCalls()
	if len(arms) != 1 {
		t.Fatalf("want 1 arm, got %d", len(arms))
	}
	if arms[0].AvgPrice != 0.10 {
		t.Errorf("arm price = %.3f, want the 0.100 guard ask fallback", arms[0].AvgPrice)
	}
}

// assertSentContains fails unless some recorded DM contains substr.
func assertSentContains(t *testing.T, tg *tgRecorder, substr string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		tg.mu.Lock()
		for _, m := range tg.sends {
			if strings.Contains(m.text, substr) {
				tg.mu.Unlock()
				return
			}
		}
		tg.mu.Unlock()
		time.Sleep(5 * time.Millisecond)
	}
	tg.mu.Lock()
	defer tg.mu.Unlock()
	texts := make([]string, 0, len(tg.sends))
	for _, m := range tg.sends {
		texts = append(texts, m.text)
	}
	t.Fatalf("no DM contains %q; sent: %s", substr, fmt.Sprint(texts))
}

// The auto-sniped DM must disclose an unconfirmed fill (issue #92 defect 1):
// executor Success with FilledSize 0 means the order may be resting.
func TestSnipeAutoBuyDM_UnconfirmedFill_DisclosesPending(t *testing.T) {
	t.Parallel()
	h := newSnipeAutoBuyHarness(t, snipeHarnessConfig{
		ask: 0.17, askOK: true, user: snipeWalletUser(),
		buyResult: &polymarket.TradeResult{Success: true, OrderID: "ord-rest"},
	})
	h.bot.snipeOrderFill = (&fillProbe{readings: []fillReading{{open: true, found: true}}}).check
	h.bot.snipeOrderKill = (&fillProbe{}).kill
	h.bot.snipeFillPoll = time.Millisecond
	h.bot.snipeFillWindow = 5 * time.Millisecond
	h.bot.snipeOrderKill = (&fillProbe{}).kill

	h.bot.NotifySnipeAlert(7, testSnipeMarket(), 0.45, 0.17)

	assertSentContains(t, h.tg, "Fill not confirmed yet")
}

// A confirmed immediate fill keeps the classic auto-sniped copy — no pending
// disclaimer.
func TestSnipeAutoBuyDM_ConfirmedFill_NoPendingNote(t *testing.T) {
	t.Parallel()
	h := newSnipeAutoBuyHarness(t, snipeHarnessConfig{
		ask: 0.17, askOK: true, user: snipeWalletUser(),
		buyResult: &polymarket.TradeResult{Success: true, OrderID: "ord-hit", FilledSize: 58.8, AveragePrice: 0.17},
	})

	h.bot.NotifySnipeAlert(7, testSnipeMarket(), 0.45, 0.17)

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		h.tg.mu.Lock()
		n := len(h.tg.sends)
		h.tg.mu.Unlock()
		if n > 0 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	h.tg.mu.Lock()
	defer h.tg.mu.Unlock()
	for _, m := range h.tg.sends {
		if strings.Contains(m.text, "Fill not confirmed yet") {
			t.Errorf("confirmed fill DM carries the pending note: %s", m.text)
		}
	}
}

// A found=false blip right after submit (the bet-delay window, issue #27)
// must be inconclusive: the loop keeps polling and arms when the order
// becomes queryable and matched. Default gone-grace far exceeds the window.
func TestSnipeConfirmFill_GoneBlipEarly_KeepsPolling(t *testing.T) {
	t.Parallel()
	probe := &fillProbe{readings: []fillReading{
		{open: false, found: false},
		{open: false, found: false},
		{matched: 71.43, price: 0.07, open: false, found: true},
	}}
	b, _, repo := newFillConfirmBot(t, probe)
	rel := &releaseRecorder{}

	b.snipeConfirmFillThenArm(7, fillConfirmUser(), "tok-1", "Q?", "Lakers", fillConfirmResult("ord-8", 0.10), 5, rel.release)

	arms := repo.armedCalls()
	if len(arms) != 1 {
		t.Fatalf("want 1 arm after the gone blip resolves, got %d", len(arms))
	}
	if arms[0].SharesAtArm != 71.43 {
		t.Errorf("arm shares = %.2f, want 71.43", arms[0].SharesAtArm)
	}
	if got := rel.total(); got != 0 {
		t.Errorf("released %.2f on a bet-delay blip that later filled, want 0", got)
	}
}

// found=false past the gone-grace means the order was reaped: terminal, and a
// previously observed partial fill is preserved, not wiped (review F3).
func TestSnipeConfirmFill_ReapedAfterGrace_KeepsPartial(t *testing.T) {
	t.Parallel()
	probe := &fillProbe{readings: []fillReading{
		{matched: 30, price: 0.07, open: true, found: true},
		{open: false, found: false},
	}}
	b, _, repo := newFillConfirmBot(t, probe)
	b.snipeFillGoneGrace = time.Nanosecond
	rel := &releaseRecorder{}

	b.snipeConfirmFillThenArm(7, fillConfirmUser(), "tok-1", "Q?", "Lakers", fillConfirmResult("ord-9", 0.10), 5, rel.release)

	arms := repo.armedCalls()
	if len(arms) != 1 {
		t.Fatalf("want 1 arm for the preserved partial, got %d", len(arms))
	}
	if arms[0].SharesAtArm != 30 {
		t.Errorf("arm shares = %.2f, want the preserved 30 (a reap blip must not wipe a partial)", arms[0].SharesAtArm)
	}
	if probe.killCount() != 0 {
		t.Errorf("killed a reaped order")
	}
	want := 5 - 30*0.07
	if got := rel.total(); got < want-0.001 || got > want+0.001 {
		t.Errorf("released %.2f, want %.2f", got, want)
	}
}

// A cancel that fails while the order still reads open must NOT refund — the
// order may still fill. Fail closed with a warning (review F2).
func TestSnipeConfirmFill_KillFailsOrderStillOpen_NoRefund(t *testing.T) {
	t.Parallel()
	probe := &fillProbe{
		readings: []fillReading{{open: true, found: true}},
		killErr:  errors.New("cancel rejected"),
	}
	b, tg, repo := newFillConfirmBot(t, probe)
	rel := &releaseRecorder{}

	b.snipeConfirmFillThenArm(7, fillConfirmUser(), "tok-1", "Q?", "Lakers", fillConfirmResult("ord-10", 0.10), 5, rel.release)

	if arms := repo.armedCalls(); len(arms) != 0 {
		t.Fatalf("armed an order with zero matched shares: %+v", arms[0])
	}
	if got := rel.total(); got != 0 {
		t.Errorf("released %.2f while the order may still be live, want 0", got)
	}
	assertSentContains(t, tg, "stays counted")
}
