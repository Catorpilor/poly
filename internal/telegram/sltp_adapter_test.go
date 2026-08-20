package telegram

import (
	"context"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"

	"github.com/Catorpilor/poly/internal/database"
	"github.com/Catorpilor/poly/internal/database/repositories"
	"github.com/Catorpilor/poly/internal/polymarket"
)

// fakeArmRepo satisfies repositories.SLTPArmRepository via an embedded nil
// interface; only GetByID is exercised here, so the rest stay unimplemented.
type fakeArmRepo struct {
	repositories.SLTPArmRepository
	wantUser int64
	wantID   int
	arm      *database.SLTPArm
	err      error
}

func (f *fakeArmRepo) GetByID(_ context.Context, telegramID int64, id int) (*database.SLTPArm, error) {
	f.wantUser, f.wantID = telegramID, id
	return f.arm, f.err
}

func cbUpdate(userID int64, data string) *tgbotapi.Update {
	return &tgbotapi.Update{
		CallbackQuery: &tgbotapi.CallbackQuery{
			From: &tgbotapi.User{ID: userID},
			Data: data,
		},
	}
}

// TestResolveSLTPArm proves disarm/lottery resolution keys off the stable arm
// DB ID carried in the callback and loads straight from the repo — with no
// dependency on the cached position list. This is the fix for disarms silently
// failing as "Session expired" when the UI state had expired or the tap arrived
// late.
func TestResolveSLTPArm(t *testing.T) {
	t.Parallel()

	t.Run("valid id loads arm from repo, ignoring UI state", func(t *testing.T) {
		t.Parallel()
		want := &database.SLTPArm{ID: 42, TelegramID: 7, TokenID: "tokZ", Outcome: "YES"}
		repo := &fakeArmRepo{arm: want}
		b := &Bot{sltpArmRepo: repo}

		got, err := b.resolveSLTPArm(context.Background(), cbUpdate(7, "sltp:off:42"))
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != want {
			t.Fatalf("got %v, want %v", got, want)
		}
		if repo.wantUser != 7 || repo.wantID != 42 {
			t.Errorf("repo queried with user=%d id=%d, want user=7 id=42", repo.wantUser, repo.wantID)
		}
	})

	t.Run("already disarmed returns nil,nil", func(t *testing.T) {
		t.Parallel()
		b := &Bot{sltpArmRepo: &fakeArmRepo{arm: nil}}
		got, err := b.resolveSLTPArm(context.Background(), cbUpdate(7, "sltp:off:99"))
		if err != nil || got != nil {
			t.Fatalf("got (%v, %v), want (nil, nil)", got, err)
		}
	})

	for _, bad := range []string{"sltp:off", "sltp:off:abc", "sltp:off:1:2"} {
		bad := bad
		t.Run("malformed callback "+bad, func(t *testing.T) {
			t.Parallel()
			b := &Bot{sltpArmRepo: &fakeArmRepo{}}
			if _, err := b.resolveSLTPArm(context.Background(), cbUpdate(7, bad)); err == nil {
				t.Errorf("expected error for %q, got nil", bad)
			}
		})
	}
}

// TestArmTickSize covers the arm-time tick capture (issue #25): the market's
// minimum_tick_size flows from the CLOB into the arm; any failure falls back
// to 0.01 and must never block arming.
func TestArmTickSize(t *testing.T) {
	t.Parallel()

	t.Run("nil trading client defaults to 0.01", func(t *testing.T) {
		t.Parallel()
		b := &Bot{}
		if got := b.armTickSize(context.Background(), "tok"); got != 0.01 {
			t.Errorf("armTickSize = %v, want 0.01", got)
		}
	})

	t.Run("fetches minimum tick from the CLOB", func(t *testing.T) {
		t.Parallel()
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch {
			case r.URL.Path == "/book":
				io.WriteString(w, `{"market":"cond-1"}`)
			case strings.HasPrefix(r.URL.Path, "/markets/"):
				io.WriteString(w, `{"minimum_tick_size":0.001}`)
			default:
				http.NotFound(w, r)
			}
		}))
		defer srv.Close()
		b := &Bot{tradingClient: polymarket.NewTradingClient(srv.URL, 137)}
		if got := b.armTickSize(context.Background(), "tok"); got != 0.001 {
			t.Errorf("armTickSize = %v, want 0.001", got)
		}
	})

	t.Run("CLOB error defaults to 0.01", func(t *testing.T) {
		t.Parallel()
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "boom", http.StatusInternalServerError)
		}))
		defer srv.Close()
		b := &Bot{tradingClient: polymarket.NewTradingClient(srv.URL, 137)}
		if got := b.armTickSize(context.Background(), "tok"); got != 0.01 {
			t.Errorf("armTickSize = %v, want 0.01", got)
		}
	})
}

func TestSharesBigIntToFloat(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		in   *big.Int
		want float64
	}{
		{"nil returns zero", nil, 0},
		{"one share", big.NewInt(1_000_000), 1.0},
		{"half share", big.NewInt(500_000), 0.5},
		{"100 shares", big.NewInt(100_000_000), 100.0},
		{"fractional 1.234567 shares", big.NewInt(1_234_567), 1.234567},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := sharesBigIntToFloat(tt.in)
			if diff := got - tt.want; diff > 1e-9 || diff < -1e-9 {
				t.Errorf("sharesBigIntToFloat = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestSLTPRowForPosition_ArmedShowsDisarm(t *testing.T) {
	t.Parallel()
	pos := &polymarket.Position{
		MarketTitle: "Will X happen?",
		TokenID:     "tokX",
		Outcome:     "YES",
		Shares:      big.NewInt(10_000_000),
	}
	// ID deliberately differs from the position index (2) to prove the disarm
	// button is keyed on the stable arm DB ID, not the volatile UI index.
	arm := &database.SLTPArm{ID: 42, TPArmed: true, SLArmed: true}
	row := sltpRowForPosition(2, pos, arm)
	if len(row) != 1 {
		t.Fatalf("expected 1 button, got %d", len(row))
	}
	if row[0].CallbackData == nil || *row[0].CallbackData != "sltp:off:42" {
		t.Errorf("expected callback sltp:off:42, got %v", row[0].CallbackData)
	}
	if !strings.Contains(row[0].Text, "Disarm") {
		t.Errorf("expected button text to mention Disarm, got %q", row[0].Text)
	}
}

// TestSLTPRowForPosition_Coverage covers feat/auto-arm-full-coverage's honest
// list: a TP-only arm whose SharesAtArm lags the position's shares shows a
// coverage marker; a covered TP-only arm and a manual TP+SL arm do not.
func TestSLTPRowForPosition_Coverage(t *testing.T) {
	t.Parallel()
	pos := &polymarket.Position{
		MarketTitle: "LoL final",
		TokenID:     "tokX",
		Outcome:     "T1",
		Shares:      big.NewInt(93_000_000), // 93 shares
	}

	t.Run("TP-only under-covered shows coverage warning", func(t *testing.T) {
		t.Parallel()
		arm := &database.SLTPArm{ID: 7, TPArmed: true, SLArmed: false, SharesAtArm: 50}
		row := sltpRowForPosition(0, pos, arm)
		text := row[0].Text
		if !strings.Contains(text, "⚠") || !strings.Contains(text, "50/93") {
			t.Errorf("under-covered TP-only row missing coverage marker: %q", text)
		}
		if *row[0].CallbackData != "sltp:off:7" {
			t.Errorf("callback = %v, want sltp:off:7", row[0].CallbackData)
		}
	})

	t.Run("TP-only fully covered shows no warning", func(t *testing.T) {
		t.Parallel()
		arm := &database.SLTPArm{ID: 8, TPArmed: true, SLArmed: false, SharesAtArm: 93}
		text := sltpRowForPosition(0, pos, arm)[0].Text
		if strings.Contains(text, "⚠") {
			t.Errorf("covered TP-only row must not warn: %q", text)
		}
	})

	t.Run("manual TP+SL under-covered is NOT flagged (deliberate freeze)", func(t *testing.T) {
		t.Parallel()
		arm := &database.SLTPArm{ID: 9, TPArmed: true, SLArmed: true, SharesAtArm: 50}
		text := sltpRowForPosition(0, pos, arm)[0].Text
		if strings.Contains(text, "⚠") {
			t.Errorf("manual arm coverage is a deliberate freeze, must not warn: %q", text)
		}
	})
}

func TestSLTPRowForPosition_UnarmedShowsArm(t *testing.T) {
	t.Parallel()
	pos := &polymarket.Position{
		MarketTitle: "Will X happen?",
		TokenID:     "tokX",
		Outcome:     "YES",
		Shares:      big.NewInt(10_000_000),
	}
	row := sltpRowForPosition(0, pos, nil)
	if len(row) != 1 {
		t.Fatalf("expected 1 button, got %d", len(row))
	}
	if row[0].CallbackData == nil || *row[0].CallbackData != "sltp:arm:0" {
		t.Errorf("expected callback sltp:arm:0, got %v", row[0].CallbackData)
	}
	if !strings.Contains(row[0].Text, "Arm") {
		t.Errorf("expected button text to mention Arm, got %q", row[0].Text)
	}
}

func TestSLTPRowForPosition_CallbackDataUnder64Bytes(t *testing.T) {
	t.Parallel()
	// Telegram caps callback_data at 64 bytes. Position index up to 8 (per handleSLTPList
	// cap) yields callback like "sltp:arm:8" — trivially under, but guard against regressions.
	pos := &polymarket.Position{
		MarketTitle: "An extremely long market title that must not break the button",
		TokenID:     "some_long_token_id_value_here",
		Outcome:     "YES",
		Shares:      big.NewInt(1_234_567_890),
	}
	row := sltpRowForPosition(7, pos, nil)
	if cb := row[0].CallbackData; cb == nil || len(*cb) > 64 {
		t.Errorf("callback data %q exceeds 64 bytes (or nil)", cbString(cb))
	}
}

func cbString(p *string) string {
	if p == nil {
		return "<nil>"
	}
	return *p
}

// TestNormalizeOutcome covers the boundary between the position scanner
// (returns "Yes"/"No" for display) and SLTPArm.Validate (requires "YES"/"NO").
// Without this normalization, arming a position fails with
// `invalid arm: invalid outcome: Yes`.
func TestNormalizeOutcome(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in   string
		want database.Outcome
	}{
		{"Yes", database.OutcomeYes},
		{"yes", database.OutcomeYes},
		{"YES", database.OutcomeYes},
		{"No", database.OutcomeNo},
		{"NO", database.OutcomeNo},
	}
	for _, c := range cases {
		if got := normalizeOutcome(c.in); got != c.want {
			t.Errorf("normalizeOutcome(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestSLTPFiredText_TPOnlyNoStop covers the v0.17.0 TP-only auto-arm
// (SLArmed=false): a TP fire must NOT promise a trailing stop that will never
// run (the monitor gates SL on SLArmed). The remainder rides to the ceiling.
func TestSLTPFiredText_TPOnlyNoStop(t *testing.T) {
	t.Parallel()
	arm := &database.SLTPArm{AvgPrice: 0.50, HighWaterMark: 1.00, Outcome: "KNICKS", TPArmed: true, SLArmed: false}
	got := sltpFiredText("TP", arm, 1.00, &polymarket.TradeResult{Success: true})

	for _, w := range []string{"TP hit", "KNICKS", "0.95", "ceiling", "max loss"} {
		if !strings.Contains(got, w) {
			t.Errorf("TP-only text missing %q:\n%s", w, got)
		}
	}
	for _, notWant := range []string{"Trailing stop", "follows the peak", "watching the remainder", "stop $"} {
		if strings.Contains(got, notWant) {
			t.Errorf("TP-only text must not promise a stop (%q):\n%s", notWant, got)
		}
	}
}

func TestSLTPFiredText(t *testing.T) {
	t.Parallel()
	// Post-TP-fire TP+SL arm (SLArmed=true): entry 0.50, peak ratcheted to
	// 1.00 → stop 0.80. The TP-only (SLArmed=false) TP message is covered
	// separately by TestSLTPFiredText_TPOnlyNoStop.
	arm := &database.SLTPArm{AvgPrice: 0.50, HighWaterMark: 1.00, Outcome: "KNICKS", SLArmed: true}
	ok := &polymarket.TradeResult{Success: true}
	fail := &polymarket.TradeResult{Success: false, ErrorMsg: "boom"}

	tests := []struct {
		name    string
		kind    string
		result  *polymarket.TradeResult
		want    []string
		wantNot []string
	}{
		{
			name:   "TP mentions trailing stop on remainder",
			kind:   "TP",
			result: ok,
			want:   []string{"TP hit", "Trailing stop", "$0.8000", "KNICKS"},
		},
		{
			name:   "ceiling unchanged",
			kind:   "TP-ceiling",
			result: ok,
			want:   []string{"TP ceiling", "fully disarmed"},
		},
		{
			name:   "SL describes peak, stop and floor",
			kind:   "SL",
			result: ok,
			want:   []string{"Trailing stop hit", "$1.0000", "$0.8000", "$0.7200", "fully disarmed"},
		},
		{
			name:    "SL-market escalation names the gapped floor honestly",
			kind:    "SL-market",
			result:  ok,
			want:    []string{"Trailing stop hit", "gapped through the floor", "$0.7200", "sold at market", "fully disarmed"},
			wantNot: []string{"FOK floor"},
		},
		{
			name:   "SL with confirmed fill shows avg price",
			kind:   "SL",
			result: &polymarket.TradeResult{Success: true, FilledSize: 199.99, AveragePrice: 0.3610},
			want:   []string{"Trailing stop hit", "199.99", "avg", "$0.3610"},
		},
		{
			name:   "failure branch for TP kinds",
			kind:   "TP",
			result: fail,
			want:   []string{"sell failed", "boom"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := sltpFiredText(tt.kind, arm, 0.80, tt.result)
			for _, w := range tt.want {
				if !strings.Contains(got, w) {
					t.Errorf("text missing %q:\n%s", w, got)
				}
			}
			for _, w := range tt.wantNot {
				if strings.Contains(got, w) {
					t.Errorf("text should not contain %q:\n%s", w, got)
				}
			}
		})
	}
}

// TestSLTPFiredText_Ladder covers the deep-entry ladder fire notice (issue
// #81): it states the rung multiple reached and the cumulative fraction banked,
// and its remainder line matches the arm source (ceiling for TP-only, trailing
// stop for a manual TP+SL arm).
func TestSLTPFiredText_Ladder(t *testing.T) {
	t.Parallel()
	ok := &polymarket.TradeResult{Success: true}

	t.Run("TP-only ladder rung states multiple and cumulative bank", func(t *testing.T) {
		t.Parallel()
		// After the 3× rung (2 rungs fired): 25%+20% = 45% banked.
		arm := &database.SLTPArm{AvgPrice: 0.05, TickSize: 0.01, Outcome: "T1",
			TPArmed: true, SLArmed: false, LadderRungsFired: 2, LadderBaseShares: 100}
		got := sltpFiredText("TP-ladder", arm, 0.15, ok)
		for _, w := range []string{"TP ladder", "3× entry", "45%", "T1", "$0.95", "ceiling"} {
			if !strings.Contains(got, w) {
				t.Errorf("ladder fire text missing %q:\n%s", w, got)
			}
		}
		for _, nw := range []string{"Trailing stop"} {
			if strings.Contains(got, nw) {
				t.Errorf("TP-only ladder must not promise a stop (%q):\n%s", nw, got)
			}
		}
	})

	t.Run("manual ladder rung promises the trailing stop on the remainder", func(t *testing.T) {
		t.Parallel()
		// All four rungs fired: 75% banked; SLArmed ⇒ the remainder is under the stop.
		arm := &database.SLTPArm{AvgPrice: 0.05, TickSize: 0.01, HighWaterMark: 0.30, Outcome: "T1",
			TPArmed: true, SLArmed: true, LadderRungsFired: 4, LadderBaseShares: 100}
		got := sltpFiredText("TP-ladder", arm, 0.25, ok)
		for _, w := range []string{"TP ladder", "5× entry", "75%", "Trailing stop"} {
			if !strings.Contains(got, w) {
				t.Errorf("manual ladder fire text missing %q:\n%s", w, got)
			}
		}
		if strings.Contains(got, "ceiling") {
			t.Errorf("manual ladder remainder is under the stop, not the ceiling:\n%s", got)
		}
	})
}

// TestSLTPListHeader covers the /sltp list legend (issue #81 F4): it stays
// byte-identical when no listed arm is deep, and appends the ladder summary when
// any armed position is a deep entry.
func TestSLTPListHeader(t *testing.T) {
	t.Parallel()

	t.Run("no deep arm: standard legend, no ladder line", func(t *testing.T) {
		t.Parallel()
		armed := map[string]*database.SLTPArm{
			"A": {AvgPrice: 0.20, TPArmed: true, SLArmed: true},
		}
		got := sltpListHeader(3, armed)
		for _, w := range []string{"SL/TP Auto-Sell", "entry × 2.0 → sell 25%", "trailing"} {
			if !strings.Contains(got, w) {
				t.Errorf("standard header missing %q:\n%s", w, got)
			}
		}
		if strings.Contains(got, "exit ladder") {
			t.Errorf("non-deep list must not mention the ladder:\n%s", got)
		}
	})

	t.Run("a deep arm present: appends the ladder summary", func(t *testing.T) {
		t.Parallel()
		armed := map[string]*database.SLTPArm{
			"A": {AvgPrice: 0.20, TPArmed: true, SLArmed: true},
			"B": {AvgPrice: 0.03, TPArmed: true, SLArmed: false}, // deep
		}
		got := sltpListHeader(2, armed)
		for _, w := range []string{"exit ladder", "25%@2×", "20%@3×", "15%@4×", "15%@5×", "ceiling"} {
			if !strings.Contains(got, w) {
				t.Errorf("deep-aware header missing %q:\n%s", w, got)
			}
		}
	})
}

// TestSLTPArmedText_Ladder covers the manual arm-confirmation copy for a
// deep-entry position (issue #81): it lists every rung (fraction, multiple, and
// tick-floored price) and the ceiling remainder, and never the single-partial
// wording.
func TestSLTPArmedText_Ladder(t *testing.T) {
	t.Parallel()
	arm := &database.SLTPArm{AvgPrice: 0.05, HighWaterMark: 0.05, TickSize: 0.01, Outcome: "T1"}
	got := sltpArmedText("LoL: T1 vs GEN", "T1", arm)
	for _, w := range []string{
		"TP ladder", "deep entry",
		"25% @ 2× → bid ≥ $0.1000",
		"20% @ 3× → bid ≥ $0.1500",
		"15% @ 4× → bid ≥ $0.2000",
		"15% @ 5× → bid ≥ $0.2500",
		"remainder", "$0.95 ceiling",
	} {
		if !strings.Contains(got, w) {
			t.Errorf("deep armed text missing %q:\n%s", w, got)
		}
	}
	if strings.Contains(got, "→ sell 25%") {
		t.Errorf("deep armed text must not show the single-partial line:\n%s", got)
	}
}

// TestSLTPStaleSizeText covers the issue #24 notices: a balance-shortfall
// rejection means the arm's snapshot is stale — either the position is gone
// entirely (auto-disarm) or the stop now covers fewer shares.
func TestSLTPStaleSizeText(t *testing.T) {
	t.Parallel()
	arm := &database.SLTPArm{SharesAtArm: 450, Outcome: "NONGSHIM RED FORCE"}

	tests := []struct {
		name         string
		availableRaw int64
		want         []string
		wantNot      []string
	}{
		{
			name:         "zero balance renders auto-disarm",
			availableRaw: 0,
			want:         []string{"closed outside the bot", "auto-disarmed", "NONGSHIM RED FORCE"},
			wantNot:      []string{"smaller"},
		},
		{
			name:         "partial balance renders clamped coverage",
			availableRaw: 225_000_000,
			want: []string{"smaller than when armed", "225.00", "450.00",
				"NONGSHIM RED FORCE", "re-arm"},
			wantNot: []string{"auto-disarmed"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := sltpStaleSizeText(arm, tt.availableRaw)
			for _, w := range tt.want {
				if !strings.Contains(got, w) {
					t.Errorf("text missing %q:\n%s", w, got)
				}
			}
			for _, w := range tt.wantNot {
				if strings.Contains(got, w) {
					t.Errorf("text should not contain %q:\n%s", w, got)
				}
			}
		})
	}
}

// TestSLTPSweptText covers the issue #39 cleanup notice: one message per user
// per sweep summarizing every arm auto-disarmed because its market closed.
func TestSLTPSweptText(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		outcomes []string
		want     []string
	}{
		{
			name:     "single outcome",
			outcomes: []string{"YES"},
			want: []string{
				"🧹 *Cleaned up 1 finished position(s)*",
				"YES",
				"don't need SL/TP",
			},
		},
		{
			name:     "multiple outcomes comma-joined",
			outcomes: []string{"NONGSHIM RED FORCE", "NO", "KNICKS"},
			want: []string{
				"🧹 *Cleaned up 3 finished position(s)*",
				"NONGSHIM RED FORCE, NO, KNICKS",
				"don't need SL/TP",
			},
		},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := sltpSweptText(tt.outcomes)
			for _, w := range tt.want {
				if !strings.Contains(got, w) {
					t.Errorf("text missing %q:\n%s", w, got)
				}
			}
		})
	}
}

func TestSLTPArmedText(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		arm     *database.SLTPArm
		want    []string
		notWant []string
	}{
		{
			// Fresh arm: HWM == entry (dormant). TP 2× = 0.60 < ceiling.
			name: "reachable TP shows the partial",
			arm:  &database.SLTPArm{AvgPrice: 0.30, HighWaterMark: 0.30, Outcome: "KNICKS"},
			want: []string{
				"$0.3000",  // entry
				"$0.3600",  // activation = entry × 1.20
				"20%",      // trail distance below peak
				"max loss", // explicit no-stop-below-activation caveat
				"$0.6000",  // TP trigger
				"sell 25%", // the partial promise, honest here
			},
			notWant: []string{"ceiling"},
		},
		{
			// Issue #74: entry > 0.475 caps the 2× trigger above the 0.95
			// ceiling, which the monitor checks first — the partial can never
			// fire, so the confirmation must promise the ceiling, not the TP.
			name: "unreachable TP shows the ceiling instead",
			arm:  &database.SLTPArm{AvgPrice: 0.50, HighWaterMark: 0.50, Outcome: "KNICKS"},
			want: []string{
				"$0.5000", // entry
				"$0.6000", // activation = entry × 1.20
				"max loss",
				"$0.95",    // ceiling threshold
				"sell 100", // ceiling sells everything
				"ceiling",
			},
			notWant: []string{"sell 25%", "$0.9900"},
		},
		{
			// Boundary: trigger == ceiling exactly — ceiling still fires first.
			name:    "trigger equal to ceiling is unreachable",
			arm:     &database.SLTPArm{AvgPrice: 0.475, HighWaterMark: 0.475, Outcome: "KNICKS"},
			want:    []string{"$0.95", "sell 100", "ceiling"},
			notWant: []string{"sell 25%"},
		},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := sltpArmedText("Knicks vs. Spurs", "KNICKS", tt.arm)
			for _, w := range tt.want {
				if !strings.Contains(got, w) {
					t.Errorf("armed text missing %q:\n%s", w, got)
				}
			}
			for _, nw := range tt.notWant {
				if strings.Contains(got, nw) {
					t.Errorf("armed text must not contain %q:\n%s", nw, got)
				}
			}
		})
	}
}
