package telegram

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/Catorpilor/poly/internal/database"
	"github.com/Catorpilor/poly/internal/database/repositories"
)

// armGateRepo is a fake SLTPArmRepository for the manual-arm gate (issue #86):
// GetByUserAndToken answers from a fixed per-token map, or returns a wired error
// to exercise the fail-open path. Keying by token ID (not chatID) is enough for
// these single-user tests. Every other method is inherited from the embedded nil
// interface — the gate must never call them.
type armGateRepo struct {
	repositories.SLTPArmRepository
	arms map[string]*database.SLTPArm
	err  error
}

func (r *armGateRepo) GetByUserAndToken(_ context.Context, _ int64, tokenID string) (*database.SLTPArm, error) {
	if r.err != nil {
		return nil, r.err
	}
	return r.arms[tokenID], nil
}

// ArmTPOnly is a no-op returning the arm unchanged: when the gate lets a buy
// through, the fill's async TP-only auto-arm (snipeAutoArmTPOnly) calls this. The
// harness leaves sltpArmRepo nil to short-circuit it, but the gate tests must
// wire the repo, so the fake has to answer this method too rather than panic on
// the embedded nil interface.
func (r *armGateRepo) ArmTPOnly(_ context.Context, arm *database.SLTPArm) (*database.SLTPArm, error) {
	return arm, nil
}

// manualArm is an active manual stop (sl_armed = TRUE) — the shape the #86 gate
// must catch.
func manualArm(tokenID string) *database.SLTPArm {
	return &database.SLTPArm{TokenID: tokenID, SLArmed: true, TPArmed: true}
}

// hasSendContaining reports whether any recorded sendMessage body contains sub.
// The gate tests that let a buy THROUGH also spawn the fill's async TP-only
// auto-arm (a second, "Auto-armed" send that races the synchronous alert), so a
// positional sentAt(0) check on those is non-deterministic. The alert itself is
// always sent synchronously before NotifySnipeAlert returns, so scanning all
// sends for its "Auto-sniped" marker is race-free.
func (rec *tgRecorder) hasSendContaining(sub string) bool {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	for _, m := range rec.sends {
		if strings.Contains(m.text, sub) {
			return true
		}
	}
	return false
}

// TestSnipeSkipNoteManualArmed: the manual-arm skip class carries the ratified
// copy — an honest reason plus the standing "tap below if you still want it".
func TestSnipeSkipNoteManualArmed(t *testing.T) {
	t.Parallel()
	got := snipeSkipNote(snipeBuyResult{outcome: snipeBuyManualArmed})
	for _, want := range []string{
		"Auto-buy skipped",
		"your stop is already managing this token",
		"tap below if you still want it",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("manual-armed skip note missing %q in:\n%s", want, got)
		}
	}
}

// TestNotifySnipeAlertManualArmedGates: a recipient with an active sl_armed stop
// on the CRASHED token gets no in-band buy — no reserve (cap untouched), no
// MarkBought — but the alert still delivers with tap buttons and the honest
// stop-managing note.
func TestNotifySnipeAlertManualArmedGates(t *testing.T) {
	t.Parallel()
	h := newSnipeAutoBuyHarness(t, snipeHarnessConfig{ask: 0.17, askOK: true, user: snipeWalletUser()})
	m := testSnipeMarket()
	h.bot.sltpArmRepo = &armGateRepo{arms: map[string]*database.SLTPArm{m.TokenID: manualArm(m.TokenID)}}

	h.bot.NotifySnipeAlert(7, m, 0.45, 0.17)

	if got := h.buys.count(); got != 0 {
		t.Fatalf("buy calls = %d, want 0 (manual-arm gate)", got)
	}
	if got := h.watch.boughtCount(); got != 0 {
		t.Errorf("MarkBought calls = %d, want 0", got)
	}
	sent := h.tg.sentAt(t, 0)
	if !strings.Contains(sent.text, "Comeback Snipe") || strings.Contains(sent.text, "Auto-sniped") {
		t.Errorf("manual-armed alert wrong:\n%s", sent.text)
	}
	if !strings.Contains(sent.text, "Auto-buy skipped") || !strings.Contains(sent.text, "your stop is already managing this token") {
		t.Errorf("manual-armed alert must carry the stop-managing skip note:\n%s", sent.text)
	}
	// Tap buttons stay live — the user judges it.
	callbackData(t, sent.markup, "⚡ Snipe $10")
	callbackData(t, sent.markup, "⚡ Snipe $25")
	// The cap was never reserved.
	if _, ok := h.bot.snipeSpend.reserve(7, snipeAutoBuyDailyCapUSD); !ok {
		t.Error("cap consumed by a manual-armed alert")
	}
}

// TestNotifySnipeAlertTPOnlyArmDoesNotGate: a TP-only auto-arm (sl_armed = FALSE)
// carries whole-position TP coverage, so a top-up is orphan-safe — it must buy
// exactly as today.
func TestNotifySnipeAlertTPOnlyArmDoesNotGate(t *testing.T) {
	t.Parallel()
	h := newSnipeAutoBuyHarness(t, snipeHarnessConfig{ask: 0.17, askOK: true, user: snipeWalletUser()})
	m := testSnipeMarket()
	h.bot.sltpArmRepo = &armGateRepo{arms: map[string]*database.SLTPArm{
		m.TokenID: {TokenID: m.TokenID, SLArmed: false, TPArmed: true},
	}}

	h.bot.NotifySnipeAlert(7, m, 0.45, 0.17)

	if got := h.buys.count(); got != 1 {
		t.Fatalf("buy calls = %d, want 1 (TP-only never gates)", got)
	}
	if !h.tg.hasSendContaining("Auto-sniped") {
		t.Error("TP-only-armed recipient must still get the Auto-sniped alert")
	}
}

// TestNotifySnipeAlertNoOrSweptArmDoesNotGate: no arm row, and a fully
// disarmed/swept row (tp_armed = FALSE AND sl_armed = FALSE), both fall through
// to the normal auto-buy.
func TestNotifySnipeAlertNoOrSweptArmDoesNotGate(t *testing.T) {
	t.Parallel()
	m := testSnipeMarket()
	tests := []struct {
		name string
		arm  *database.SLTPArm // nil ⇒ no row for the token
	}{
		{"no arm row", nil},
		{"disarmed/swept row (tp+sl both false)", &database.SLTPArm{TokenID: m.TokenID, SLArmed: false, TPArmed: false}},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			h := newSnipeAutoBuyHarness(t, snipeHarnessConfig{ask: 0.17, askOK: true, user: snipeWalletUser()})
			arms := map[string]*database.SLTPArm{}
			if tt.arm != nil {
				arms[m.TokenID] = tt.arm
			}
			h.bot.sltpArmRepo = &armGateRepo{arms: arms}

			h.bot.NotifySnipeAlert(7, m, 0.45, 0.17)

			if got := h.buys.count(); got != 1 {
				t.Fatalf("buy calls = %d, want 1 (%s must not gate)", got, tt.name)
			}
			if !h.tg.hasSendContaining("Auto-sniped") {
				t.Errorf("%s must still get the Auto-sniped alert", tt.name)
			}
		})
	}
}

// TestNotifySnipeAlertCase3WithManualArmOnSiblingStillLatches (precedence pin):
// the recipient holds the OTHER side via a manual arm on the SIBLING — which is
// exactly what makes them case-3. The alerted token has no arm, so the #86 gate
// has nothing to fire on; case-3 classification runs first and boxed-wait must
// latch as today. Case-3 wins; the gate reads the ALERTED token only.
func TestNotifySnipeAlertCase3WithManualArmOnSiblingStillLatches(t *testing.T) {
	t.Parallel()
	h := newSnipeAutoBuyHarness(t, snipeHarnessConfig{ask: 0.18, askOK: true, user: snipeWalletUser()})
	m := testSnipeMarket()
	h.watch.siblings = []string{"sibB"}
	h.bot.sltpArmRepo = &armGateRepo{arms: map[string]*database.SLTPArm{"sibB": manualArm("sibB")}}

	h.bot.NotifySnipeAlert(7, m, 0.45, 0.18)

	if got := h.buys.count(); got != 0 {
		t.Fatalf("buy calls = %d, want 0 (boxed-wait)", got)
	}
	if !h.bot.snipeBoxedLatch.eligible(7, m.TokenID) {
		t.Error("case-3 with a manual arm on the sibling must still latch boxed-eligible")
	}
	sent := h.tg.sentAt(t, 0)
	if !strings.Contains(sent.text, "other side") || strings.Contains(sent.text, "your stop is already managing") {
		t.Errorf("expected the boxed-wait note, not the manual-arm skip note:\n%s", sent.text)
	}
	if _, ok := h.bot.snipeSpend.reserve(7, snipeAutoBuyDailyCapUSD); !ok {
		t.Error("cap consumed by a boxed-wait alert")
	}
}

// TestNotifySnipeAlertArmLookupErrorFailsOpen: the gate is a guard, not a
// dependency — a DB-read failure on the arm lookup fails OPEN, so the buy
// proceeds exactly as today.
func TestNotifySnipeAlertArmLookupErrorFailsOpen(t *testing.T) {
	t.Parallel()
	h := newSnipeAutoBuyHarness(t, snipeHarnessConfig{ask: 0.17, askOK: true, user: snipeWalletUser()})
	m := testSnipeMarket()
	h.bot.sltpArmRepo = &armGateRepo{err: errors.New("db down")}

	h.bot.NotifySnipeAlert(7, m, 0.45, 0.17)

	if got := h.buys.count(); got != 1 {
		t.Fatalf("buy calls = %d, want 1 (arm-lookup error must fail open)", got)
	}
	if !h.tg.hasSendContaining("Auto-sniped") {
		t.Error("fail-open path must still get the Auto-sniped alert")
	}
}

// TestNotifySnipeAlertDKG2ManualArmNoOrphan replays the DK G2 exhibit: a manual
// trailing stop (sl_armed) stood at 0.464 while the in-band alert fired near
// 0.26 — a fully buyable ask (below the 0.30 repricing guard, healthy book), so
// only the #86 gate can stop it. It must spend $0, leaving no orphan.
func TestNotifySnipeAlertDKG2ManualArmNoOrphan(t *testing.T) {
	t.Parallel()
	h := newSnipeAutoBuyHarness(t, snipeHarnessConfig{ask: 0.26, askOK: true, user: snipeWalletUser()})
	m := testSnipeMarket()
	h.bot.sltpArmRepo = &armGateRepo{arms: map[string]*database.SLTPArm{
		m.TokenID: {TokenID: m.TokenID, SLArmed: true, TPArmed: true, HighWaterMark: 0.464},
	}}

	h.bot.NotifySnipeAlert(7, m, 0.464, 0.26)

	if got := h.buys.count(); got != 0 {
		t.Fatalf("buy calls = %d, want 0 — the DK G2 orphan must be impossible", got)
	}
	if got := h.watch.boughtCount(); got != 0 {
		t.Errorf("MarkBought calls = %d, want 0", got)
	}
	// $0 spent: the whole daily cap is still reservable (0 already spent ⇒ $50 in,
	// $0 left, ok).
	if left, ok := h.bot.snipeSpend.reserve(7, snipeAutoBuyDailyCapUSD); !ok || left != 0 {
		t.Errorf("cap after a gated DK G2 alert = (%.0f left, ok=%v), want the full $50 reservable", left, ok)
	}
}
