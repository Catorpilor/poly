package telegram

import (
	"bytes"
	"context"
	"errors"
	"log"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Catorpilor/poly/internal/database"
	"github.com/Catorpilor/poly/internal/database/repositories"
)

// recordingArmRepo is a fake SLTPArmRepository for the TP-only auto-arm: it
// serves a fixed "existing" arm from GetByUserAndToken and records ArmTPOnly
// calls. Only the two methods the auto-arm path touches are implemented; the
// rest come from the embedded (nil) interface.
type recordingArmRepo struct {
	repositories.SLTPArmRepository
	mu       sync.Mutex
	existing *database.SLTPArm // returned by GetByUserAndToken (nil = none)
	getErr   error             // forced GetByUserAndToken error (fail-closed path, issue #87)
	armErr   error             // forced ArmTPOnly error
	armed    []*database.SLTPArm
}

func (r *recordingArmRepo) GetByUserAndToken(_ context.Context, _ int64, _ string) (*database.SLTPArm, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.getErr != nil {
		return nil, r.getErr
	}
	return r.existing, nil
}

func (r *recordingArmRepo) ArmTPOnly(_ context.Context, arm *database.SLTPArm) (*database.SLTPArm, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.armErr != nil {
		return nil, r.armErr
	}
	saved := *arm
	saved.ID = len(r.armed) + 1
	saved.HighWaterMark = arm.AvgPrice // mirror the SQL's high_water_mark = avg_price seed
	r.armed = append(r.armed, &saved)
	return &saved, nil
}

func (r *recordingArmRepo) armedCalls() []*database.SLTPArm {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]*database.SLTPArm(nil), r.armed...)
}

// waitArmed polls until exactly one arm has been persisted (the auto-arm runs
// asynchronously so it never blocks alert delivery) or the deadline passes.
// ArmTPOnly runs before the WatchArmed/confirmation steps, so this is only safe
// for assertions on the persisted arm itself; use waitForSend to await the
// helper's terminal effect (the confirmation DM) before asserting those.
func waitArmed(t *testing.T, repo *recordingArmRepo) *database.SLTPArm {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if calls := repo.armedCalls(); len(calls) == 1 {
			return calls[0]
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for one auto-arm; got %d", len(repo.armedCalls()))
	return nil
}

// waitForSend polls the recorder until a sent message contains substr. The
// confirmation DM is the LAST step of the async auto-arm, so its arrival proves
// ArmTPOnly and WatchArmed already ran.
func waitForSend(t *testing.T, tg *tgRecorder, substr string) {
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
	t.Fatalf("timed out waiting for a sent message containing %q", substr)
}

// TestSnipeAutoArmedText: the TP-only confirmation carries entry, TP trigger,
// and a max-loss-is-your-stake caveat, and omits any trailing-stop line.
func TestSnipeAutoArmedText(t *testing.T) {
	t.Parallel()
	arm := &database.SLTPArm{AvgPrice: 0.20, HighWaterMark: 0.20, SharesAtArm: 50, Outcome: "LAKERS"}
	got := snipeAutoArmedText("LoL: T1 vs. Gen.G", "LAKERS", arm)
	for _, w := range []string{"Auto-armed (TP only)", "$0.2000", "$0.4000", "max loss", "$10.00"} {
		if !strings.Contains(got, w) {
			t.Errorf("auto-armed text missing %q:\n%s", w, got)
		}
	}
	for _, notWant := range []string{"trailing", "Trailing", "activation", "wakes"} {
		if strings.Contains(got, notWant) {
			t.Errorf("TP-only text must not mention a stop-loss (%q):\n%s", notWant, got)
		}
	}
}

// TestSnipeAutoArmedTextUnreachableTP: issue #74 — an entry high enough that
// the capped 2× trigger sits at/above the 0.95 ceiling must not promise the
// 25% partial; the ceiling (sell 100%) is what actually fires.
func TestSnipeAutoArmedTextUnreachableTP(t *testing.T) {
	t.Parallel()
	arm := &database.SLTPArm{AvgPrice: 0.50, HighWaterMark: 0.50, SharesAtArm: 20, Outcome: "LAKERS"}
	got := snipeAutoArmedText("LoL: T1 vs. Gen.G", "LAKERS", arm)
	for _, w := range []string{"Auto-armed (TP only)", "$0.5000", "$0.95", "sell 100", "ceiling", "max loss", "$10.00"} {
		if !strings.Contains(got, w) {
			t.Errorf("auto-armed text missing %q:\n%s", w, got)
		}
	}
	for _, notWant := range []string{"sell 25%", "$0.9900", "trailing", "wakes"} {
		if strings.Contains(got, notWant) {
			t.Errorf("auto-armed text must not contain %q:\n%s", notWant, got)
		}
	}
}

// TestSnipeAutoArmedTextDeepEntry: a deep-entry (≤ $0.05) TP-only auto-arm lists
// the full exit ladder (issue #81) — every rung and the ceiling remainder — and
// still omits any trailing-stop wording.
func TestSnipeAutoArmedTextDeepEntry(t *testing.T) {
	t.Parallel()
	arm := &database.SLTPArm{AvgPrice: 0.02, HighWaterMark: 0.02, TickSize: 0.01, SharesAtArm: 250, Outcome: "T1"}
	got := snipeAutoArmedText("LoL: T1 vs GEN", "T1", arm)
	for _, w := range []string{
		"Auto-armed (TP only)", "$0.0200", "TP ladder", "deep entry",
		"25% @ 2×", "20% @ 3×", "15% @ 4×", "15% @ 5×", "ceiling", "max loss",
	} {
		if !strings.Contains(got, w) {
			t.Errorf("deep auto-armed text missing %q:\n%s", w, got)
		}
	}
	for _, notWant := range []string{"trailing", "Trailing", "wakes"} {
		if strings.Contains(got, notWant) {
			t.Errorf("deep TP-only text must not mention a stop (%q):\n%s", notWant, got)
		}
	}
}

// TestSnipeAutoArmInBand: a successful in-band auto-buy TP-only-arms from the
// fill — TPArmed true, SLArmed false, price = guard ask, shares = stake/price,
// HWM = AvgPrice — informs the snipe watcher, and DMs the confirmation.
func TestSnipeAutoArmInBand(t *testing.T) {
	t.Parallel()
	h := newSnipeAutoBuyHarness(t, snipeHarnessConfig{ask: 0.20, askOK: true, user: snipeWalletUser()})
	repo := &recordingArmRepo{}
	h.bot.sltpArmRepo = repo
	m := testSnipeMarket()

	h.bot.NotifySnipeAlert(7, m, 0.45, 0.20)

	// The confirmation DM is the helper's last step — awaiting it proves the
	// arm was persisted AND WatchArmed ran, so the assertions below don't race
	// the async goroutine.
	waitForSend(t, h.tg, "Auto-armed (TP only)")
	calls := repo.armedCalls()
	if len(calls) != 1 {
		t.Fatalf("ArmTPOnly calls = %d, want 1", len(calls))
	}
	arm := calls[0]
	if !arm.TPArmed || arm.SLArmed {
		t.Errorf("arm flags = TP:%v SL:%v, want TP:true SL:false", arm.TPArmed, arm.SLArmed)
	}
	if arm.TelegramID != 7 || arm.TokenID != m.TokenID {
		t.Errorf("arm identity = %d/%s, want 7/%s", arm.TelegramID, arm.TokenID, m.TokenID)
	}
	if arm.ConditionID != "cond-1" {
		t.Errorf("ConditionID = %q, want cond-1 (from the fetched market)", arm.ConditionID)
	}
	if arm.AvgPrice != 0.20 {
		t.Errorf("AvgPrice = %v, want 0.20 (guard ask, delayed fill has no VWAP)", arm.AvgPrice)
	}
	if want := snipeAutoBuyUSD / 0.20; arm.SharesAtArm != want {
		t.Errorf("SharesAtArm = %v, want %v (stake/price)", arm.SharesAtArm, want)
	}
	if arm.HighWaterMark != arm.AvgPrice {
		t.Errorf("HighWaterMark = %v, want %v (seed to AvgPrice)", arm.HighWaterMark, arm.AvgPrice)
	}
	// Snipe watcher gained the armed source for this token.
	armedFound := false
	for _, sm := range h.watch.armedTokens() {
		if sm.TokenID == m.TokenID {
			armedFound = true
		}
	}
	if !armedFound {
		t.Errorf("WatchArmed not called for %s", m.TokenID)
	}
}

// TestSnipeAutoArmNoClobber: an existing arm is never overwritten by the
// auto-arm — no ArmTPOnly call, no error, buy unaffected.
func TestSnipeAutoArmNoClobber(t *testing.T) {
	t.Parallel()
	h := newSnipeAutoBuyHarness(t, snipeHarnessConfig{ask: 0.20, askOK: true, user: snipeWalletUser()})
	repo := &recordingArmRepo{existing: &database.SLTPArm{ID: 99, TelegramID: 7, TokenID: testSnipeMarket().TokenID}}
	h.bot.sltpArmRepo = repo

	h.bot.NotifySnipeAlert(7, testSnipeMarket(), 0.45, 0.20)

	if got := h.buys.count(); got != 1 {
		t.Fatalf("buy calls = %d, want 1", got)
	}
	// Give the async auto-arm a chance to (not) fire.
	time.Sleep(150 * time.Millisecond)
	if got := len(repo.armedCalls()); got != 0 {
		t.Errorf("ArmTPOnly calls = %d, want 0 (existing arm must not be clobbered)", got)
	}
	// A found arm is a silent skip — no auto-arm DM (neither the confirmation nor
	// the issue #87 fail-closed warning); only the alert itself went out.
	h.tg.mu.Lock()
	for _, m := range h.tg.sends {
		if strings.Contains(m.text, "Auto-armed") || strings.Contains(m.text, "Couldn't verify") {
			t.Errorf("existing arm must not trigger an auto-arm DM; got:\n%s", m.text)
		}
	}
	h.tg.mu.Unlock()
}

// syncBuffer is a mutex-guarded log sink so the async auto-arm goroutine's
// output can be captured and read from the test goroutine under -race.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// TestSnipeAutoArmReadErrorFailsClosed: issue #87 — a non-nil error from the
// existing-arm read must FAIL CLOSED: no ArmTPOnly call, a fail-closed log, and
// a DM telling the recipient to arm manually. The buy itself is unaffected.
// Not parallel: it swaps the process-wide log output to capture the log line.
func TestSnipeAutoArmReadErrorFailsClosed(t *testing.T) {
	var logs syncBuffer
	prevOut, prevFlags := log.Writer(), log.Flags()
	log.SetOutput(&logs)
	log.SetFlags(0)
	t.Cleanup(func() {
		log.SetOutput(prevOut)
		log.SetFlags(prevFlags)
	})

	h := newSnipeAutoBuyHarness(t, snipeHarnessConfig{ask: 0.20, askOK: true, user: snipeWalletUser()})
	repo := &recordingArmRepo{getErr: errors.New("db read timeout")}
	h.bot.sltpArmRepo = repo

	h.bot.NotifySnipeAlert(7, testSnipeMarket(), 0.45, 0.20)

	if got := h.buys.count(); got != 1 {
		t.Fatalf("buy calls = %d, want 1 (read error must not block the buy)", got)
	}
	// The fail-closed warning DM is the branch's terminal effect; awaiting it
	// proves the read-error path ran to completion (log + DM + return).
	waitForSend(t, h.tg, "Couldn't verify this token's existing protection — auto-arm skipped. Tap 🎯 SL/TP to arm manually.")

	if got := len(repo.armedCalls()); got != 0 {
		t.Errorf("ArmTPOnly calls = %d, want 0 (must fail closed on read error)", got)
	}
	if out := logs.String(); !strings.Contains(out, "existing-arm read FAILED") || !strings.Contains(out, "failing closed, no auto-arm") {
		t.Errorf("missing fail-closed log line; got:\n%s", out)
	}
}

// TestSnipeAutoArmRepoErrorDoesNotBlockBuy: an ArmTPOnly failure is log-only —
// the buy still filled and MarkBought still latched.
func TestSnipeAutoArmRepoErrorDoesNotBlockBuy(t *testing.T) {
	t.Parallel()
	h := newSnipeAutoBuyHarness(t, snipeHarnessConfig{ask: 0.20, askOK: true, user: snipeWalletUser()})
	repo := &recordingArmRepo{armErr: errors.New("db down")}
	h.bot.sltpArmRepo = repo

	h.bot.NotifySnipeAlert(7, testSnipeMarket(), 0.45, 0.20)

	if got := h.buys.count(); got != 1 {
		t.Fatalf("buy calls = %d, want 1 (arm failure must not block the buy)", got)
	}
	if got := h.watch.boughtCount(); got != 1 {
		t.Errorf("MarkBought calls = %d, want 1", got)
	}
	// The alert itself was still delivered.
	sent := h.tg.sentAt(t, 0)
	if !strings.Contains(sent.text, "Auto-sniped") {
		t.Errorf("alert not delivered on arm failure:\n%s", sent.text)
	}
}

// TestSnipeAutoArmDeep: a successful $5 deep auto-buy TP-only-arms with the deep
// stake and its guard ask.
func TestSnipeAutoArmDeep(t *testing.T) {
	t.Parallel()
	h := newSnipeAutoBuyHarness(t, snipeHarnessConfig{ask: 0.02, askOK: true, user: snipeWalletUser()})
	repo := &recordingArmRepo{}
	h.bot.sltpArmRepo = repo

	h.bot.NotifySnipeDeepCrash(7, testSnipeMarket(), 0.45, 0.02, 0.09, time.Minute)

	arm := waitArmed(t, repo)
	if !arm.TPArmed || arm.SLArmed {
		t.Errorf("deep arm flags = TP:%v SL:%v, want TP:true SL:false", arm.TPArmed, arm.SLArmed)
	}
	if arm.AvgPrice != 0.02 {
		t.Errorf("deep arm AvgPrice = %v, want 0.02", arm.AvgPrice)
	}
	if want := snipeDeepBuyUSD / 0.02; arm.SharesAtArm != want {
		t.Errorf("deep arm SharesAtArm = %v, want %v (deep stake/price)", arm.SharesAtArm, want)
	}
}

// TestSnipeAutoArmTap: a one-tap buy in handleSnipeCallback TP-only-arms with
// the tapped amount as the stake.
func TestSnipeAutoArmTap(t *testing.T) {
	t.Parallel()
	h := newSnipeAutoBuyHarness(t, snipeHarnessConfig{ask: 0.20, askOK: true, user: snipeWalletUser()})
	// Disable the in-band auto-buy's arm so only the tap arms: use an existing
	// arm returned once, then cleared. Simpler: fresh repo, and register the
	// alert manually so the tap is the sole fill.
	repo := &recordingArmRepo{}
	h.bot.sltpArmRepo = repo
	m := testSnipeMarket()
	alertID := h.bot.snipeAlerts.add(m)

	h.bot.handleSnipeCallback(context.Background(), snipeTapUpdate(7, "snipe:"+alertID+":25"))

	arm := waitArmed(t, repo)
	if !arm.TPArmed || arm.SLArmed {
		t.Errorf("tap arm flags = TP:%v SL:%v, want TP:true SL:false", arm.TPArmed, arm.SLArmed)
	}
	if want := 25.0 / 0.20; arm.SharesAtArm != want {
		t.Errorf("tap arm SharesAtArm = %v, want %v ($25 stake/price)", arm.SharesAtArm, want)
	}
}
