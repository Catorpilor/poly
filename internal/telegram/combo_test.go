package telegram

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"

	"github.com/Catorpilor/poly/internal/polymarket"
)

// Gamma /markets/<id> fixtures: the first real Combo Links' legs (all
// finished on 2026-10-22) plus two open markets.
var comboTestMarkets = map[string]string{
	"5437373": `{"id":"5437373","question":"FC Bayern München vs. Arsenal FC: O/U 0.5","slug":"ucl-bay-ars-2026-10-21-total-0pt5","closed":true,"acceptingOrders":false,"comboStatus":"enabled","outcomes":"[\"Over\", \"Under\"]","outcomePrices":"[\"1\", \"0\"]","positionIds":["84047013385200","84047013385201"]}`,
	"5436769": `{"id":"5436769","question":"Exact Score: Aston Villa FC 3 - 1 Viking FK?","slug":"ucl-ast-vf-2026-10-21-exact-score-3-1","closed":true,"acceptingOrders":false,"comboStatus":"enabled","outcomes":"[\"Yes\", \"No\"]","outcomePrices":"[\"1\", \"0\"]","positionIds":["12186271078900","12186271078901"]}`,
	"5436671": `{"id":"5436671","question":"Exact Score: Real Madrid CF 3 - 1 RB Leipzig?","slug":"ucl-rma-rbl-2026-10-21-exact-score-3-1","closed":true,"acceptingOrders":false,"comboStatus":"enabled","outcomes":"[\"Yes\", \"No\"]","outcomePrices":"[\"0\", \"1\"]","positionIds":["91899799307000","91899799307001"]}`,
	"700001":  `{"id":"700001","question":"Will Team_A win on 2026-10-25?","slug":"nba-a-b-2026-10-25","closed":false,"acceptingOrders":true,"comboStatus":"enabled","outcomes":"[\"Yes\", \"No\"]","outcomePrices":"[\"0.5\", \"0.5\"]","positionIds":["70000100","70000101"]}`,
	"700002":  `{"id":"700002","question":"Lakers vs. Suns: O/U 221.5","slug":"nba-lal-phx-2026-10-25-total-221pt5","closed":false,"acceptingOrders":true,"comboStatus":"enabled","outcomes":"[\"Over\", \"Under\"]","outcomePrices":"[\"0.4\", \"0.6\"]","positionIds":["70000200","70000201"]}`,
}

func fakeComboMarket(t *testing.T) func(context.Context, string) (*polymarket.GammaMarket, error) {
	t.Helper()
	return func(_ context.Context, id string) (*polymarket.GammaMarket, error) {
		raw, ok := comboTestMarkets[id]
		if !ok {
			return nil, polymarket.ErrMarketNotFound
		}
		var m polymarket.GammaMarket
		if err := json.Unmarshal([]byte(raw), &m); err != nil {
			t.Errorf("fixture %s: %v", id, err)
		}
		return &m, nil
	}
}

type quoteCall struct {
	userID int64
	legs   []string
	stake  float64
}

type fakeComboQuoter struct {
	mu    sync.Mutex
	calls []quoteCall
	quote *polymarket.ComboQuote
	err   error
}

func (f *fakeComboQuoter) fn(_ context.Context, acct *comboAccount, legs []string, stake float64) (*polymarket.ComboQuote, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, quoteCall{acct.userID, append([]string(nil), legs...), stake})
	return f.quote, f.err
}

// fakeComboAccount loads a stub account; noWallet makes it fail like a user
// without a wallet.
func fakeComboAccount(noWallet bool) func(context.Context, int64) (*comboAccount, error) {
	return func(_ context.Context, userID int64) (*comboAccount, error) {
		if noWallet {
			return nil, errComboNoWallet
		}
		return &comboAccount{userID: userID}, nil
	}
}

// docsQuote: pay $10.00 (fees incl.), receive 19.32381 shares -> x1.93.
var docsQuote = &polymarket.ComboQuote{
	RFQID: "rfq-1", Status: "AWAITING_REQUESTER_ACCEPTANCE",
	TotalRequiredE6: 10_000_000, NetReceiveE6: 19_323_810, BlendedPrice: 0.5,
}

func TestComboScreen(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		param       string
		quote       *polymarket.ComboQuote
		quoteErr    error
		noWallet    bool
		wantText    []string
		notText     []string
		wantQuoted  bool
		wantRefresh bool
	}{
		{
			name:     "malformed link",
			param:    "c_5437373y",
			wantText: []string{"❌ Invalid combo link", "at least 2 legs"},
		},
		{
			name:     "unknown market",
			param:    "c_5437373y999999y",
			wantText: []string{"❌ Invalid combo link", "999999", "not found"},
		},
		{
			name:     "real link 1: both legs won",
			param:    "c_5437373y5436769y",
			wantText: []string{"🧩 *Combo* · 2 legs", "*Over*", "✅ won", "Every leg already won"},
			notText:  []string{"Live quote", "Estimate"},
		},
		{
			name:     "real link 2: Real leg lost",
			param:    "c_5437373y5436671y",
			wantText: []string{"❌ lost", "Leg 2 already lost"},
			notText:  []string{"Live quote"},
		},
		{
			name:     "open + finished leg is unavailable",
			param:    "c_700001y5437373y",
			wantText: []string{"⛔ Leg 2", "can't be bought right now"},
			notText:  []string{"Live quote"},
		},
		{
			name:        "buyable with a live quote",
			param:       "c_700001y700002n",
			quote:       docsQuote,
			wantText:    []string{"*Under* @ 0.60", "*Yes* @ 0.50", "Live quote", "*x1.93*", "$19.32", "Estimate: x3.33", "Team\\_A"},
			wantQuoted:  true,
			wantRefresh: true,
		},
		{
			name:        "no quotes right now",
			param:       "c_700001y700002y",
			quote:       &polymarket.ComboQuote{Status: "FAILED", ErrorCode: "NO_QUOTES", ErrorMessage: "no quotes"},
			wantText:    []string{"No quotes right now", "Estimate: x5.00"},
			wantQuoted:  true,
			wantRefresh: true,
		},
		{
			name:        "gateway rejects the legs",
			param:       "c_700001y700002y",
			quote:       &polymarket.ComboQuote{Status: "FAILED", ErrorCode: "INCOMPATIBLE_LEGS", ErrorMessage: "legs conflict"},
			wantText:    []string{"Quote failed", "INCOMPATIBLE\\_LEGS"},
			wantQuoted:  true,
			wantRefresh: true,
		},
		{
			name:        "no wallet yet",
			param:       "c_700001y700002y",
			noWallet:    true,
			wantText:    []string{"/start", "Estimate: x5.00"},
			wantRefresh: true,
		},
		{
			name:        "quote transport error keeps the estimate",
			param:       "c_700001y700002y",
			quoteErr:    errors.New("dial tcp: i/o timeout"),
			wantText:    []string{"Live quote unavailable", "Estimate: x5.00"},
			wantQuoted:  true,
			wantRefresh: true,
		},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			q := &fakeComboQuoter{quote: tt.quote, err: tt.quoteErr}
			b := &Bot{comboMarketFn: fakeComboMarket(t), comboAccountFn: fakeComboAccount(tt.noWallet), comboQuoteFn: q.fn}

			text, kb := b.comboScreen(context.Background(), 42, tt.param)

			for _, s := range tt.wantText {
				if !strings.Contains(text, s) {
					t.Errorf("text missing %q:\n%s", s, text)
				}
			}
			for _, s := range tt.notText {
				if strings.Contains(text, s) {
					t.Errorf("text must not contain %q:\n%s", s, text)
				}
			}
			if got := len(q.calls) > 0; got != tt.wantQuoted {
				t.Errorf("quoted = %v, want %v", got, tt.wantQuoted)
			}
			if tt.wantQuoted && len(q.calls) == 1 {
				c := q.calls[0]
				if c.userID != 42 || c.stake != comboReferenceStakeUSD || len(c.legs) != 2 {
					t.Errorf("quote call = %+v, want user 42, $%v, 2 legs", c, comboReferenceStakeUSD)
				}
			}
			hasRefresh := false
			if kb != nil {
				for _, row := range kb.InlineKeyboard {
					for _, btn := range row {
						if btn.CallbackData != nil && strings.HasPrefix(*btn.CallbackData, "cr:") {
							hasRefresh = true
							if len(*btn.CallbackData) > 64 {
								t.Errorf("callback data %d bytes > 64", len(*btn.CallbackData))
							}
							if got, ok := b.lookupComboLink(strings.TrimPrefix(*btn.CallbackData, "cr:")); !ok || got != tt.param {
								t.Errorf("refresh key resolves to %q/%v, want %q", got, ok, tt.param)
							}
						}
					}
				}
			}
			if hasRefresh != tt.wantRefresh {
				t.Errorf("refresh button = %v, want %v", hasRefresh, tt.wantRefresh)
			}
			// Every buyable screen (all legs open) offers the stake buttons,
			// whatever happened to the quote; others never do.
			stakes := comboStakeButtons(kb)
			if want := tt.wantRefresh; (len(stakes) == 6) != want {
				t.Errorf("stake buttons = %v, want 6 iff buyable", stakes)
			}
			if strings.Contains(text, "next release") {
				t.Error("buying is live: drop the 'next release' footer")
			}
		})
	}
}

func TestComboScreen_LegsCarryTheirMarketLink(t *testing.T) {
	t.Parallel()

	b := &Bot{comboMarketFn: fakeComboMarket(t), comboAccountFn: fakeComboAccount(false), comboQuoteFn: (&fakeComboQuoter{quote: docsQuote}).fn}
	text, _ := b.comboScreen(context.Background(), 1, "c_5437373y5436769y")
	for _, slug := range []string{"ucl-bay-ars-2026-10-21-total-0pt5", "ucl-ast-vf-2026-10-21-exact-score-3-1"} {
		if !strings.Contains(text, fmt.Sprintf("(https://polymarket.com/market/%s)", slug)) {
			t.Errorf("missing market link for %s:\n%s", slug, text)
		}
	}
}

func TestComboScreen_QuotesTheLegPositionIDsInLinkOrder(t *testing.T) {
	t.Parallel()

	q := &fakeComboQuoter{quote: docsQuote}
	b := &Bot{comboMarketFn: fakeComboMarket(t), comboAccountFn: fakeComboAccount(false), comboQuoteFn: q.fn}
	b.comboScreen(context.Background(), 1, "c_700002n700001y")
	if len(q.calls) != 1 {
		t.Fatalf("quote calls = %d, want 1", len(q.calls))
	}
	// 700002 n -> positionIds[1]; 700001 y -> positionIds[0]; link order kept.
	if got := strings.Join(q.calls[0].legs, ","); got != "70000201,70000100" {
		t.Errorf("legs = %s, want 70000201,70000100", got)
	}
}

// comboStakeButtons lists the stake callbacks on a keyboard, in order.
func comboStakeButtons(kb *tgbotapi.InlineKeyboardMarkup) []string {
	var out []string
	if kb == nil {
		return nil
	}
	for _, row := range kb.InlineKeyboard {
		for _, btn := range row {
			if btn.CallbackData != nil && (strings.HasPrefix(*btn.CallbackData, comboBuyPrefix) || strings.HasPrefix(*btn.CallbackData, comboCustomPrefix)) {
				out = append(out, *btn.CallbackData)
			}
		}
	}
	return out
}
