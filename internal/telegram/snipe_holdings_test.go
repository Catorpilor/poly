package telegram

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Catorpilor/poly/internal/database"
	"github.com/Catorpilor/poly/internal/live"
	"github.com/Catorpilor/poly/internal/polymarket"
)

// heldPosition is a Data API position with shares — what tier `positions` reads
// as a holding. Dust counts: the gate is Shares > 0, not a size floor.
func heldPosition(tokenID string, shares int64) *polymarket.Position {
	return &polymarket.Position{TokenID: tokenID, Shares: big.NewInt(shares)}
}

// tpOnlyArm is an auto-arm with no stop (sl_armed = FALSE) — still evidence of
// a holding under issue #111, where it used to top up freely (#86 scope).
func tpOnlyArm(tokenID string) *database.SLTPArm {
	return &database.SLTPArm{TokenID: tokenID, SLArmed: false, TPArmed: true}
}

// holdsGateBot wires the minimum Bot the holdings gate reads: the in-memory
// bought record, a fake watcher, and a scripted positions source. The arm repo
// starts nil (tier `arm` inert) — cases that need it wire their own.
func holdsGateBot(positions []*polymarket.Position, posErr error) (*Bot, *fakeSnipeWatch, *fakePositionSource) {
	watch := &fakeSnipeWatch{}
	scanner := &fakePositionSource{positions: positions, err: posErr}
	b := &Bot{snipeWatcher: watch, snipeBought: newSnipeBoughtRecord(), snipePositions: scanner}
	return b, watch, scanner
}

// TestSnipeHoldsAlerted covers the four evidence tiers (cheapest first, first
// hit wins), the `via` each reports, and the fail-open/fall-through edges
// (issue #111).
func TestSnipeHoldsAlerted(t *testing.T) {
	t.Parallel()
	market := testSnipeMarket()
	user := snipeWalletUserWithProxy()

	tests := []struct {
		name      string
		positions []*polymarket.Position
		posErr    error
		setup     func(b *Bot, watch *fakeSnipeWatch)
		wantVia   string
		wantHeld  bool
	}{
		{
			name:     "tier a: in-memory snipe bought record",
			setup:    func(b *Bot, _ *fakeSnipeWatch) { b.snipeBought.mark(7, market.TokenID, snipeAutoBuyUSD) },
			wantVia:  snipeHoldViaBought,
			wantHeld: true,
		},
		{
			name:     "tier b: watcher bought-side mark",
			setup:    func(_ *Bot, w *fakeSnipeWatch) { w.markHolds(market.TokenID) },
			wantVia:  snipeHoldViaHeld,
			wantHeld: true,
		},
		{
			name: "tier c: TP-only arm gates (reverses the #86 free top-up)",
			setup: func(b *Bot, _ *fakeSnipeWatch) {
				b.sltpArmRepo = &armGateRepo{arms: map[string]*database.SLTPArm{market.TokenID: tpOnlyArm(market.TokenID)}}
			},
			wantVia:  snipeHoldViaArm,
			wantHeld: true,
		},
		{
			name: "tier c: post-ClearTP row (tp+sl both false) still gates",
			setup: func(b *Bot, _ *fakeSnipeWatch) {
				// ClearTP flips tp_armed → false after the TP-only arm sells 25%;
				// ~75% of the position is still held (sltp_monitor.go ClearTP).
				b.sltpArmRepo = &armGateRepo{arms: map[string]*database.SLTPArm{
					market.TokenID: {TokenID: market.TokenID, SLArmed: false, TPArmed: false},
				}}
			},
			wantVia:  snipeHoldViaArm,
			wantHeld: true,
		},
		{
			name: "tier c: SL-armed keeps the manual-armed class",
			setup: func(b *Bot, _ *fakeSnipeWatch) {
				b.sltpArmRepo = &armGateRepo{arms: map[string]*database.SLTPArm{market.TokenID: manualArm(market.TokenID)}}
			},
			wantVia:  snipeHoldViaArmSL,
			wantHeld: true,
		},
		{
			name:      "tier d: positions, dust counts",
			positions: []*polymarket.Position{heldPosition(market.TokenID, 1)},
			wantVia:   snipeHoldViaPositions,
			wantHeld:  true,
		},
		{
			name:      "no evidence anywhere ⇒ buy proceeds",
			positions: []*polymarket.Position{heldPosition("unrelated", 50_000_000)},
			wantHeld:  false,
		},
		{
			name:      "zero-share position is not a holding",
			positions: []*polymarket.Position{{TokenID: market.TokenID, Shares: big.NewInt(0)}},
			wantHeld:  false,
		},
		{
			name:      "sibling evidence never gates (that is case 3)",
			positions: []*polymarket.Position{heldPosition("sibB", 50_000_000)},
			setup: func(b *Bot, w *fakeSnipeWatch) {
				w.siblings = []string{"sibB"}
				w.markHolds("sibB")
				b.snipeBought.mark(7, "sibB", snipeAutoBuyUSD)
				b.sltpArmRepo = &armGateRepo{arms: map[string]*database.SLTPArm{"sibB": manualArm("sibB")}}
			},
			wantHeld: false,
		},
		{
			name:      "first hit wins: bought record beats a live arm",
			positions: []*polymarket.Position{heldPosition(market.TokenID, 50_000_000)},
			setup: func(b *Bot, _ *fakeSnipeWatch) {
				b.snipeBought.mark(7, market.TokenID, snipeAutoBuyUSD)
				b.sltpArmRepo = &armGateRepo{arms: map[string]*database.SLTPArm{market.TokenID: manualArm(market.TokenID)}}
			},
			wantVia:  snipeHoldViaBought,
			wantHeld: true,
		},
		{
			name:      "arm read error falls through to positions",
			positions: []*polymarket.Position{heldPosition(market.TokenID, 50_000_000)},
			setup: func(b *Bot, _ *fakeSnipeWatch) {
				b.sltpArmRepo = &armGateRepo{err: errors.New("db down")}
			},
			wantVia:  snipeHoldViaPositions,
			wantHeld: true,
		},
		{
			name:     "positions read error fails open",
			posErr:   errors.New("data api down"),
			wantHeld: false,
		},
		{
			name:     "no trading wallet ⇒ nothing to read, buy proceeds",
			wantHeld: false,
		},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			b, watch, _ := holdsGateBot(tt.positions, tt.posErr)
			if tt.setup != nil {
				tt.setup(b, watch)
			}
			ctx := context.Background()
			via, held := b.snipeHoldsAlerted(ctx, 7, market, b.snipePositionsOnce(ctx, user))
			if held != tt.wantHeld {
				t.Fatalf("held = %v, want %v (via=%q)", held, tt.wantHeld, via)
			}
			if held && via != tt.wantVia {
				t.Errorf("via = %q, want %q", via, tt.wantVia)
			}
		})
	}
}

// TestSnipeSkipNoteAlreadyHeld: the holdings-gated class carries the ratified
// copy plus the standing tap tail (issue #111 keeps the retired deep tier's
// wording).
func TestSnipeSkipNoteAlreadyHeld(t *testing.T) {
	t.Parallel()
	got := snipeSkipNote(snipeBuyResult{outcome: snipeBuyAlreadyHeld})
	for _, want := range []string{
		"Auto-buy skipped",
		"you already hold this token — not topping up a held position",
		"tap below if you still want it",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("already-held skip note missing %q in:\n%s", want, got)
		}
	}
}

// TestNotifySnipeAlertHoldingsGated is the r107 pattern end to end: the
// recipient already holds the alerted token, so the $10 in-band top-up never
// runs — no buy, no cap reserve, no bought latch — while the alert delivers
// with live tap buttons and the honest note.
func TestNotifySnipeAlertHoldingsGated(t *testing.T) {
	t.Parallel()
	h := newSnipeAutoBuyHarness(t, snipeHarnessConfig{ask: 0.18, askOK: true, user: snipeWalletUserWithProxy()})
	m := testSnipeMarket()
	h.watch.markHolds(m.TokenID) // a manual web buy registered a Held Watch

	h.bot.NotifySnipeAlert(7, m, 0.45, 0.18)

	if got := h.buys.count(); got != 0 {
		t.Fatalf("buy calls = %d, want 0 (holdings gate)", got)
	}
	if got := h.watch.boughtCount(); got != 0 {
		t.Errorf("MarkBought calls = %d, want 0", got)
	}
	sent := h.tg.sentAt(t, 0)
	if !strings.Contains(sent.text, "Comeback Snipe") || strings.Contains(sent.text, "Auto-sniped") {
		t.Errorf("holdings-gated alert wrong:\n%s", sent.text)
	}
	if !strings.Contains(sent.text, "you already hold this token") || !strings.Contains(sent.text, "tap below if you still want it") {
		t.Errorf("holdings-gated alert must carry the already-held note:\n%s", sent.text)
	}
	callbackData(t, sent.markup, "⚡ Snipe $10")
	callbackData(t, sent.markup, "⚡ Snipe $25")
	if _, ok := h.bot.snipeSpend.reserve(7, snipeAutoBuyDailyCapUSD); !ok {
		t.Error("cap consumed by a holdings-gated alert")
	}
}

// Precedence (ratified): holding the CRASHED side — with or without the other
// side — is case 1, alert-only. The gate runs before case 3, so no boxed latch
// is armed and the watcher's later ≤$0.10 / ≤$0.05 rungs cannot fire.
func TestNotifySnipeAlertHoldsBothSidesGatesWithoutBoxedLatch(t *testing.T) {
	t.Parallel()
	h := newSnipeAutoBuyHarness(t, snipeHarnessConfig{ask: 0.18, askOK: true, user: snipeWalletUserWithProxy()})
	m := testSnipeMarket()
	h.watch.siblings = []string{"sibB"}
	h.bot.snipeBought.mark(7, "sibB", snipeAutoBuyUSD)    // the other side
	h.bot.snipeBought.mark(7, m.TokenID, snipeAutoBuyUSD) // and the crashed side

	h.bot.NotifySnipeAlert(7, m, 0.45, 0.18)

	if got := h.buys.count(); got != 0 {
		t.Fatalf("buy calls = %d, want 0", got)
	}
	if h.bot.snipeBoxedLatch.eligible(7, m.TokenID) {
		t.Error("holdings gate must run BEFORE case 3 — no boxed latch on a held crashed side")
	}
	sent := h.tg.sentAt(t, 0)
	if !strings.Contains(sent.text, "you already hold this token") {
		t.Errorf("expected the already-held note, not the boxed-wait note:\n%s", sent.text)
	}
}

// The gate reads the ALERTED token only: a recipient holding just the other
// side is still case 3 and the boxed ladder path is untouched.
func TestNotifySnipeAlertHoldsOnlySiblingStillBoxed(t *testing.T) {
	t.Parallel()
	h := newSnipeAutoBuyHarness(t, snipeHarnessConfig{
		ask:       0.18,
		askOK:     true,
		user:      snipeWalletUserWithProxy(),
		positions: []*polymarket.Position{heldPosition("sibB", 50_000_000)},
	})
	m := testSnipeMarket()
	h.watch.siblings = []string{"sibB"}

	h.bot.NotifySnipeAlert(7, m, 0.45, 0.18)

	if got := h.buys.count(); got != 0 {
		t.Fatalf("buy calls = %d, want 0 (boxed-wait)", got)
	}
	if !h.bot.snipeBoxedLatch.eligible(7, m.TokenID) {
		t.Error("sibling-only holder must still latch boxed-eligible")
	}
	sent := h.tg.sentAt(t, 0)
	if !strings.Contains(sent.text, "other side") || strings.Contains(sent.text, "you already hold this token") {
		t.Errorf("expected the boxed-wait note:\n%s", sent.text)
	}
}

// The gate is a guard, not a dependency: a Data API failure fails OPEN and the
// buy proceeds exactly as today.
func TestNotifySnipeAlertPositionsErrorFailsOpen(t *testing.T) {
	t.Parallel()
	h := newSnipeAutoBuyHarness(t, snipeHarnessConfig{
		ask:    0.18,
		askOK:  true,
		user:   snipeWalletUserWithProxy(),
		posErr: errors.New("data api down"),
	})

	h.bot.NotifySnipeAlert(7, testSnipeMarket(), 0.45, 0.18)

	if got := h.buys.count(); got != 1 {
		t.Fatalf("buy calls = %d, want 1 (positions error must fail open)", got)
	}
	if !h.tg.hasSendContaining("Auto-sniped") {
		t.Error("fail-open path must still get the Auto-sniped alert")
	}
}

// One alert, at most one Data API positions read (ratified): the holdings gate
// and the case-3 sibling check share a single lazy fetch.
func TestNotifySnipeAlertFetchesPositionsOnce(t *testing.T) {
	t.Parallel()
	h := newSnipeAutoBuyHarness(t, snipeHarnessConfig{
		ask:       0.18,
		askOK:     true,
		user:      snipeWalletUserWithProxy(),
		positions: []*polymarket.Position{heldPosition("unrelated", 50_000_000)},
	})
	h.watch.siblings = []string{"sibB"} // both checks reach the positions tier

	h.bot.NotifySnipeAlert(7, testSnipeMarket(), 0.45, 0.18)

	scanner, ok := h.bot.snipePositions.(*fakePositionSource)
	if !ok {
		t.Fatalf("harness positions source = %T, want *fakePositionSource", h.bot.snipePositions)
	}
	if scanner.calls != 1 {
		t.Errorf("GetPositions calls = %d, want exactly 1 per alert", scanner.calls)
	}
	if got := h.buys.count(); got != 1 {
		t.Errorf("buy calls = %d, want 1 (no holding anywhere)", got)
	}
}

// Manual taps bypass every gate: the ⚡ button on a holdings-gated alert buys.
func TestSnipeTapOnHoldingsGatedAlertStillBuys(t *testing.T) {
	t.Parallel()
	h := newSnipeAutoBuyHarness(t, snipeHarnessConfig{ask: 0.18, askOK: true, user: snipeWalletUserWithProxy()})
	m := testSnipeMarket()
	h.watch.markHolds(m.TokenID)

	h.bot.NotifySnipeAlert(7, m, 0.45, 0.18)
	if got := h.buys.count(); got != 0 {
		t.Fatalf("pre-tap buys = %d, want 0 (holdings gate)", got)
	}
	tapData := callbackData(t, h.tg.sentAt(t, 0).markup, "⚡ Snipe $10")

	h.bot.handleSnipeCallback(context.Background(), snipeTapUpdate(7, tapData))

	if got := h.buys.count(); got != 1 {
		t.Fatalf("post-tap buys = %d, want 1 — taps are never gated", got)
	}
}

// The boxed rungs are un-checked by design (ratified): rung 2 lands on the very
// token rung 1 just bought, so the holdings gate must not reach NotifySnipeBoxed.
func TestNotifySnipeBoxedUnaffectedByHoldingsGate(t *testing.T) {
	t.Parallel()
	h := newSnipeAutoBuyHarness(t, snipeHarnessConfig{ask: 0.04, askOK: true, user: snipeWalletUserWithProxy()})
	m := testSnipeMarket()
	h.bot.snipeBoxedLatch.arm(7, m.TokenID, false, true) // rung 1 taken, rung 2 live
	h.bot.snipeBought.mark(7, m.TokenID, snipeBoxedTrancheUSD)
	h.watch.markHolds(m.TokenID)

	h.bot.NotifySnipeBoxed(7, m, 0.45, 0.04, 2)

	if got := h.buys.count(); got != 1 {
		t.Fatalf("boxed rung-2 buys = %d, want 1 — the ladder is un-gated", got)
	}
}

// Registration wiring (issue #111): a buy names its token, so only that token
// gets the bought-side mark — the flip sibling stays a plain watch.
func TestSnipeRegisterBoughtTokenMarksBoughtSideOnly(t *testing.T) {
	t.Parallel()
	tokens := []string{"tok-t1", "tok-geng"}
	for idx, want := range tokens {
		idx, want := idx, want
		t.Run(fmt.Sprintf("bought idx %d marks %s", idx, want), func(t *testing.T) {
			t.Parallel()
			watch := &capturingHeldWatch{}
			b := &Bot{snipeWatcher: watch, snipeMarkets: noEventsGammaClient(t)}

			b.snipeRegisterBoughtToken(7, boughtTokenMarket(), idx)

			got := watch.boughtSideTokens()
			if len(got) != 1 || got[0] != want {
				t.Fatalf("bought-side marks = %v, want exactly [%s]", got, want)
			}
			if len(watch.heldCalls()) != 2 {
				t.Errorf("direct registrations = %d, want 2 (both sides still watched)", len(watch.heldCalls()))
			}
		})
	}
}

// The positions-refresh path knows which token the position is in: that token
// gets the mark, its sibling does not.
func TestRegisterSnipeHeldMarksHeldTokenOnly(t *testing.T) {
	t.Parallel()
	gamma := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/markets/157417" {
			fmt.Fprint(w,
				`{"id":"157417","question":"LoL: T1 vs. Gen.G","conditionId":"cond-1",`+
					`"outcomes":"[\"T1\",\"Gen.G\"]","clobTokenIds":"[\"tok-t1\",\"tok-geng\"]",`+
					`"gameStartTime":"2026-08-14T09:00:00Z"}`)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(gamma.Close)

	tests := []struct {
		name      string
		shares    *big.Int
		wantMarks []string
	}{
		{"held position marks its own token", big.NewInt(50_000_000), []string{"tok-t1"}},
		{"dust still counts", big.NewInt(1), []string{"tok-t1"}},
		{"zero-share position registers plain", big.NewInt(0), nil},
		{"missing shares registers plain", nil, nil},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			watch := &recordingHeldWatch{renewResult: false} // unwatched ⇒ fetch + fan out
			b := &Bot{snipeWatcher: watch, snipeMarkets: polymarket.NewMarketClientWithURL(gamma.URL)}

			b.registerSnipeHeld(7, []*polymarket.Position{
				{TokenID: "tok-t1", MarketID: "157417", Outcome: "T1", Shares: tt.shares},
			})

			got := watch.boughtSideTokens()
			if len(got) != len(tt.wantMarks) || (len(got) == 1 && got[0] != tt.wantMarks[0]) {
				t.Fatalf("bought-side marks = %v, want %v", got, tt.wantMarks)
			}
			if held := watch.heldTokens(); len(held) != 2 {
				t.Errorf("direct registrations = %v, want both sides watched either way", held)
			}
		})
	}
}

// Item 1 of the ratified scope: a RESTING limit order is not a position. The
// limit-placement path watches both sides (a fill-then-crash must still alert)
// but marks nothing — otherwise a never-filled order gates the $10 for 6h on a
// token the user does not own.
func TestSnipeRegisterRestingOrderNeverMarks(t *testing.T) {
	t.Parallel()
	watch := &capturingHeldWatch{}
	b := &Bot{snipeWatcher: watch, snipeMarkets: noEventsGammaClient(t)}

	b.snipeRegisterRestingOrder(7, boughtTokenMarket(), 0)

	if got := watch.boughtSideTokens(); len(got) != 0 {
		t.Errorf("bought-side marks = %v, want none — a resting limit is not a holding", got)
	}
	if got := watch.heldCalls(); len(got) != 2 {
		t.Errorf("direct registrations = %d, want 2 (both sides still watched)", len(got))
	}
}

// The single-fetch guarantee holds on the ERROR path too: one failed read
// serves both the holdings gate and the sibling check, and the buy fails open.
func TestNotifySnipeAlertFetchesPositionsOnceOnError(t *testing.T) {
	t.Parallel()
	h := newSnipeAutoBuyHarness(t, snipeHarnessConfig{
		ask:    0.18,
		askOK:  true,
		user:   snipeWalletUserWithProxy(),
		posErr: errors.New("data api down"),
	})
	h.watch.siblings = []string{"sibB"} // both checks reach the positions tier

	h.bot.NotifySnipeAlert(7, testSnipeMarket(), 0.45, 0.18)

	scanner, ok := h.bot.snipePositions.(*fakePositionSource)
	if !ok {
		t.Fatalf("harness positions source = %T, want *fakePositionSource", h.bot.snipePositions)
	}
	if scanner.calls != 1 {
		t.Errorf("GetPositions calls = %d, want exactly 1 (the error is memoized too)", scanner.calls)
	}
	if got := h.buys.count(); got != 1 {
		t.Errorf("buy calls = %d, want 1 (fail open)", got)
	}
}

// A held watch registered with no named holding (a pure sibling/metadata
// refresh) marks nothing — the gate must never read a watch as a holding.
func TestSnipeWatchHeldMarketWithoutHeldTokenMarksNothing(t *testing.T) {
	t.Parallel()
	watch := &fakeSnipeWatch{}
	b := &Bot{snipeWatcher: watch, snipeMarkets: noEventsGammaClient(t)}

	b.snipeWatchHeldMarket(7, boughtTokenMarket(), "", live.SnipeHeldTTL)

	if got := watch.boughtSideTokens(); len(got) != 0 {
		t.Errorf("bought-side marks = %v, want none", got)
	}
	if got := watch.heldTokens(); len(got) != 2 {
		t.Errorf("direct registrations = %v, want both sides", got)
	}
}

// The positions-refresh RENEW branch (token already watched) must apply the
// same holding predicate as the fallback: only a position with Shares > 0 is a
// holding. The Data API keeps listing sold positions at zero shares, so marking
// on every refresh would gate a sold token forever (issue #111).
func TestRegisterSnipeHeldRenewMarksOnlyHeldPositions(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		shares *big.Int
		want   bool
	}{
		{"held position renews as a holding", big.NewInt(50_000_000), true},
		{"dust still counts", big.NewInt(1), true},
		{"zero-share position renews the watch only", big.NewInt(0), false},
		{"missing shares renews the watch only", nil, false},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			watch := &recordingHeldWatch{renewResult: true} // already watched ⇒ renew path
			b := &Bot{snipeWatcher: watch}

			b.registerSnipeHeld(7, []*polymarket.Position{
				{TokenID: "tok-t1", MarketID: "157417", Outcome: "T1", Shares: tt.shares},
			})

			got := watch.renewCalls()
			if len(got) != 1 || got[0].tokenID != "tok-t1" {
				t.Fatalf("renewals = %+v, want one for tok-t1", got)
			}
			if got[0].held != tt.want {
				t.Errorf("renew held flag = %v, want %v", got[0].held, tt.want)
			}
		})
	}
}
