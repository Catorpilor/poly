package telegram

import (
	"context"

	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"

	"github.com/Catorpilor/poly/internal/polymarket"
)

// fakeComboGateway scripts quotes, takes and status polls, and records every
// screen edit and side message the buy flow produces.
type fakeComboGateway struct {
	mu        sync.Mutex
	balanceE6 int64
	quotes    []*polymarket.ComboQuote // returned in order; last one repeats
	quoteErr  error
	takes     []*polymarket.ComboTakeResult
	takeErr   error
	statuses  []*polymarket.ComboTakeResult // polled in order; last one repeats
	nQuote    int
	nTake     int
	nStatus   int
	stakes    []float64
	edits     []string
	notes     []string
	lastKB    *tgbotapi.InlineKeyboardMarkup
	statusHit chan struct{} // optional: signalled on every status poll
}

func (g *fakeComboGateway) pick(list []*polymarket.ComboTakeResult, n int) *polymarket.ComboTakeResult {
	if len(list) == 0 {
		return nil
	}
	if n >= len(list) {
		n = len(list) - 1
	}
	return list[n]
}

func (g *fakeComboGateway) install(b *Bot) {
	b.comboMarketFn = nil
	b.comboAccountFn = fakeComboAccount(false)
	b.comboBalanceFn = func(context.Context, *comboAccount) (int64, error) { return g.balanceE6, nil }
	b.comboQuoteFn = func(_ context.Context, _ *comboAccount, _ []string, stake float64, _ int64) (*polymarket.ComboQuote, error) {
		g.mu.Lock()
		defer g.mu.Unlock()
		g.stakes = append(g.stakes, stake)
		i := g.nQuote
		g.nQuote++
		if g.quoteErr != nil {
			return nil, g.quoteErr
		}
		if i >= len(g.quotes) {
			i = len(g.quotes) - 1
		}
		q := *g.quotes[i]
		q.RFQID = fmt.Sprintf("rfq-%d", i+1)
		return &q, nil
	}
	b.comboTakeFn = func(context.Context, *comboAccount, *polymarket.ComboQuote) (*polymarket.ComboTakeResult, error) {
		g.mu.Lock()
		defer g.mu.Unlock()
		r := g.pick(g.takes, g.nTake)
		g.nTake++
		return r, g.takeErr
	}
	b.comboStatusFn = func(context.Context, *comboAccount, string) (*polymarket.ComboTakeResult, error) {
		g.mu.Lock()
		r := g.pick(g.statuses, g.nStatus)
		g.nStatus++
		hit := g.statusHit
		g.mu.Unlock()
		if hit != nil {
			select {
			case hit <- struct{}{}:
			default:
			}
		}
		return r, nil
	}
	b.comboEditFn = func(_ int64, _ int, text string, kb *tgbotapi.InlineKeyboardMarkup) {
		g.mu.Lock()
		defer g.mu.Unlock()
		g.edits = append(g.edits, text)
		g.lastKB = kb
	}
	b.comboNotifyFn = func(_ int64, text string) {
		g.mu.Lock()
		defer g.mu.Unlock()
		g.notes = append(g.notes, text)
	}
	b.comboPollEvery = time.Millisecond
	b.comboInlineWait = 20 * time.Millisecond
	b.comboBackgroundWait = 200 * time.Millisecond
}

func (g *fakeComboGateway) lastEdit() string {
	g.mu.Lock()
	defer g.mu.Unlock()
	if len(g.edits) == 0 {
		return ""
	}
	return g.edits[len(g.edits)-1]
}

func (g *fakeComboGateway) counts() (quotes, takes int) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.nQuote, g.nTake
}

// quoteAt builds a quote paying mult per dollar at stake (fees included).
func quoteAt(stake, mult float64) *polymarket.ComboQuote {
	total := int64(stake * 1e6)
	return &polymarket.ComboQuote{
		QuoteID: "q", Status: "AWAITING_REQUESTER_ACCEPTANCE", YesPositionID: "999",
		TotalRequiredE6: total, NetReceiveE6: int64(float64(total) * mult),
		MakerAmountE6: total, TakerAmountE6: int64(float64(total) * mult),
	}
}

var (
	filled    = &polymarket.ComboTakeResult{Status: "FILLED", TxHash: "0xfeed"}
	executing = &polymarket.ComboTakeResult{Status: "EXECUTING"}
	mined     = &polymarket.ComboTakeResult{Status: "MINED"}
	declined  = &polymarket.ComboTakeResult{Status: "FAILED", ErrorCode: "MAKER_DECLINED", ErrorMessage: "declined"}
	expired   = &polymarket.ComboTakeResult{Status: "EXPIRED", ErrorCode: "ACCEPTANCE_EXPIRED"}
)

const buyableLink = "c_700001y700002n"

// newBuyHarness opens the buyable combo once (so a multiplier is displayed,
// from the first scripted quote) and returns the bot and its refresh key.
func newBuyHarness(t *testing.T, g *fakeComboGateway) (*Bot, string) {
	t.Helper()
	b := &Bot{}
	g.install(b)
	b.comboMarketFn = fakeComboMarket(t)
	b.comboScreen(context.Background(), 7, buyableLink)
	return b, b.rememberComboLink(buyableLink)
}

func TestComboBuy_TakesWithinToleranceAndReportsFill(t *testing.T) {
	t.Parallel()

	g := &fakeComboGateway{balanceE6: 100e6, quotes: []*polymarket.ComboQuote{quoteAt(10, 2.75), quoteAt(25, 2.65)}, takes: []*polymarket.ComboTakeResult{filled}}
	b, key := newBuyHarness(t, g)

	b.comboBuy(context.Background(), 7, 1, 1, key, 25)

	if q, tk := g.counts(); q != 2 || tk != 1 {
		t.Fatalf("quotes/takes = %d/%d, want 2/1 (x2.65 is within 5%% of x2.75)", q, tk)
	}
	if g.stakes[1] != 25 {
		t.Errorf("buy quoted at $%v, want $25", g.stakes[1])
	}
	out := g.lastEdit()
	for _, s := range []string{"✅ *Combo bought*", "Paid $25.00", "66.25 shares", "x2.65", "Pays $66.25 if every leg wins", "*Yes*", "*Under*", "https://polygonscan.com/tx/0xfeed"} {
		if !strings.Contains(out, s) {
			t.Errorf("fill message missing %q:\n%s", s, out)
		}
	}
}

func TestComboBuy_WorseThanToleranceShowsQuoteAndBuysNothing(t *testing.T) {
	t.Parallel()

	g := &fakeComboGateway{balanceE6: 1000e6, quotes: []*polymarket.ComboQuote{quoteAt(10, 2.75), quoteAt(250, 2.50)}, takes: []*polymarket.ComboTakeResult{filled}}
	b, key := newBuyHarness(t, g)

	b.comboBuy(context.Background(), 7, 1, 1, key, 250)

	if _, tk := g.counts(); tk != 0 {
		t.Fatalf("took %d quotes; x2.50 is more than 5%% below x2.75", tk)
	}
	out := g.lastEdit()
	if !strings.Contains(out, "x2.50") || !strings.Contains(out, "Tap $250 again") {
		t.Errorf("want the new quote and a tap-again prompt:\n%s", out)
	}
	if len(comboStakeButtons(g.lastKB)) != 6 {
		t.Errorf("stake buttons must stay so the user can tap again")
	}

	// The shown quote is now the displayed multiplier: the next tap buys.
	b.comboBuy(context.Background(), 7, 1, 1, key, 250)
	if _, tk := g.counts(); tk != 1 {
		t.Errorf("second tap took %d, want 1", tk)
	}
}

func TestComboBuy_NothingDisplayedFirstTapOnlyQuotes(t *testing.T) {
	t.Parallel()

	noQuote := &polymarket.ComboQuote{Status: "FAILED", ErrorCode: "NO_QUOTES"}
	g := &fakeComboGateway{balanceE6: 100e6, quotes: []*polymarket.ComboQuote{noQuote, quoteAt(25, 2.7)}, takes: []*polymarket.ComboTakeResult{filled}}
	b, key := newBuyHarness(t, g)

	b.comboBuy(context.Background(), 7, 1, 1, key, 25)
	if _, tk := g.counts(); tk != 0 {
		t.Fatal("with no multiplier on screen the bot must never take")
	}
	if !strings.Contains(g.lastEdit(), "Tap $25 again") {
		t.Errorf("want a tap-again prompt:\n%s", g.lastEdit())
	}
	b.comboBuy(context.Background(), 7, 1, 1, key, 25)
	if _, tk := g.counts(); tk != 1 {
		t.Errorf("second tap took %d, want 1", tk)
	}
}

func TestComboBuy_InsufficientBalanceStopsBeforeQuoting(t *testing.T) {
	t.Parallel()

	g := &fakeComboGateway{balanceE6: 12_340_000, quotes: []*polymarket.ComboQuote{quoteAt(10, 2.75)}}
	b, key := newBuyHarness(t, g)

	b.comboBuy(context.Background(), 7, 1, 1, key, 50)

	if q, _ := g.counts(); q != 1 {
		t.Errorf("quotes = %d, want only the opening one", q)
	}
	if out := g.lastEdit(); !strings.Contains(out, "Not enough pUSD") || !strings.Contains(out, "$12.34") {
		t.Errorf("want the balance in the refusal:\n%s", out)
	}
}

func TestComboBuy_RetriesOnceAfterARefusal(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		quotes     []*polymarket.ComboQuote
		takes      []*polymarket.ComboTakeResult
		wantQuotes int
		wantTakes  int
		wantText   string
	}{
		{
			name:       "declined then filled",
			quotes:     []*polymarket.ComboQuote{quoteAt(10, 2.75), quoteAt(25, 2.7), quoteAt(25, 2.68)},
			takes:      []*polymarket.ComboTakeResult{declined, filled},
			wantQuotes: 3, wantTakes: 2, wantText: "Combo bought",
		},
		{
			name:       "expired then filled",
			quotes:     []*polymarket.ComboQuote{quoteAt(10, 2.75), quoteAt(25, 2.7)},
			takes:      []*polymarket.ComboTakeResult{expired, filled},
			wantQuotes: 3, wantTakes: 2, wantText: "Combo bought",
		},
		{
			name:       "refused twice",
			quotes:     []*polymarket.ComboQuote{quoteAt(10, 2.75), quoteAt(25, 2.7)},
			takes:      []*polymarket.ComboTakeResult{declined, declined},
			wantQuotes: 3, wantTakes: 2, wantText: "Nothing was bought",
		},
		{
			name:       "retry quote out of tolerance is shown, not taken",
			quotes:     []*polymarket.ComboQuote{quoteAt(10, 2.75), quoteAt(25, 2.7), quoteAt(25, 2.4)},
			takes:      []*polymarket.ComboTakeResult{declined, filled},
			wantQuotes: 3, wantTakes: 1, wantText: "x2.40",
		},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			g := &fakeComboGateway{balanceE6: 100e6, quotes: tt.quotes, takes: tt.takes}
			b, key := newBuyHarness(t, g)
			b.comboBuy(context.Background(), 7, 1, 1, key, 25)
			if q, tk := g.counts(); q != tt.wantQuotes || tk != tt.wantTakes {
				t.Errorf("quotes/takes = %d/%d, want %d/%d", q, tk, tt.wantQuotes, tt.wantTakes)
			}
			if out := g.lastEdit(); !strings.Contains(out, tt.wantText) {
				t.Errorf("want %q in:\n%s", tt.wantText, out)
			}
		})
	}
}

func TestComboBuy_PendingResolvesInline(t *testing.T) {
	t.Parallel()

	g := &fakeComboGateway{balanceE6: 100e6, quotes: []*polymarket.ComboQuote{quoteAt(10, 2.75)},
		takes: []*polymarket.ComboTakeResult{executing}, statuses: []*polymarket.ComboTakeResult{mined, filled}}
	b, key := newBuyHarness(t, g)
	b.comboInlineWait = time.Second

	b.comboBuy(context.Background(), 7, 1, 1, key, 10)

	if out := g.lastEdit(); !strings.Contains(out, "Combo bought") {
		t.Errorf("want the fill:\n%s", out)
	}
}

func TestComboBuy_PendingGoesBackgroundAndBlocksTheSameCombo(t *testing.T) {
	t.Parallel()

	hit := make(chan struct{}, 1)
	g := &fakeComboGateway{balanceE6: 100e6, quotes: []*polymarket.ComboQuote{quoteAt(10, 2.75)},
		takes: []*polymarket.ComboTakeResult{executing}, statuses: []*polymarket.ComboTakeResult{executing}, statusHit: hit}
	b, key := newBuyHarness(t, g)

	b.comboBuy(context.Background(), 7, 1, 1, key, 10)

	out := g.lastEdit()
	if !strings.Contains(out, "⏳ *Pending*") || strings.Contains(out, "failed") {
		t.Fatalf("after the inline wait the message must say Pending, never failed:\n%s", out)
	}

	// A second tap on the same combo is refused without quoting.
	q0, _ := g.counts()
	b.comboBuy(context.Background(), 7, 1, 1, key, 10)
	if q, _ := g.counts(); q != q0 {
		t.Error("a pending combo must not be quoted again")
	}
	g.mu.Lock()
	notes := strings.Join(g.notes, "\n")
	g.mu.Unlock()
	if !strings.Contains(notes, "pending buy on this combo") {
		t.Errorf("want a pending refusal, got notes %q", notes)
	}

	// Background polling then sees the fill and edits the message.
	g.mu.Lock()
	g.statuses = []*polymarket.ComboTakeResult{filled}
	g.nStatus = 0
	g.mu.Unlock()
	waitFor(t, func() bool { return strings.Contains(g.lastEdit(), "Combo bought") })
	waitFor(t, func() bool { return !b.comboIsPending(7, key) })
}

func TestComboBuy_NeverResolvedIsUnknownNotFailed(t *testing.T) {
	t.Parallel()

	g := &fakeComboGateway{balanceE6: 100e6, quotes: []*polymarket.ComboQuote{quoteAt(10, 2.75)},
		takes: []*polymarket.ComboTakeResult{executing}, statuses: []*polymarket.ComboTakeResult{executing}}
	b, key := newBuyHarness(t, g)

	b.comboBuy(context.Background(), 7, 1, 1, key, 10)
	waitFor(t, func() bool { return strings.Contains(g.lastEdit(), "unknown") })
	out := g.lastEdit()
	if strings.Contains(strings.ToLower(out), "failed") || !strings.Contains(out, "/positions") {
		t.Errorf("an unresolved take is unknown — check /positions — never failed:\n%s", out)
	}
}

func TestComboBuy_TerminalFailureAfterPendingIsFailed(t *testing.T) {
	t.Parallel()

	g := &fakeComboGateway{balanceE6: 100e6, quotes: []*polymarket.ComboQuote{quoteAt(10, 2.75)},
		takes: []*polymarket.ComboTakeResult{executing}, statuses: []*polymarket.ComboTakeResult{mined, {Status: "FAILED", ErrorCode: "EXECUTION_FAILED"}}}
	b, key := newBuyHarness(t, g)
	b.comboInlineWait = time.Second

	b.comboBuy(context.Background(), 7, 1, 1, key, 10)

	if _, tk := g.counts(); tk != 1 {
		t.Errorf("takes = %d: no automatic retry after a pending take", tk)
	}
	if out := g.lastEdit(); !strings.Contains(out, "EXECUTION\\_FAILED") || !strings.Contains(out, "Nothing was bought") {
		t.Errorf("want a terminal failure:\n%s", out)
	}
}

func TestComboBuy_RateLimitedQuote(t *testing.T) {
	t.Parallel()

	g := &fakeComboGateway{balanceE6: 100e6, quotes: []*polymarket.ComboQuote{quoteAt(10, 2.75)}}
	b, key := newBuyHarness(t, g)
	g.quoteErr = errComboRateLimited

	b.comboBuy(context.Background(), 7, 1, 1, key, 10)

	if out := g.lastEdit(); !strings.Contains(out, "Quote limit reached") {
		t.Errorf("want the rate-limit notice:\n%s", out)
	}
	if _, tk := g.counts(); tk != 0 {
		t.Error("nothing to take")
	}
}

func TestComboBuy_ExpiredScreen(t *testing.T) {
	t.Parallel()

	g := &fakeComboGateway{}
	b := &Bot{}
	g.install(b)
	b.comboBuy(context.Background(), 7, 1, 1, "deadbeef0000", 10)
	if !strings.Contains(g.lastEdit(), "expired") {
		t.Errorf("unknown key must say the screen expired: %q", g.lastEdit())
	}
}

func TestComboStakeCallbacksFitTelegram(t *testing.T) {
	t.Parallel()

	g := &fakeComboGateway{balanceE6: 100e6, quotes: []*polymarket.ComboQuote{quoteAt(10, 2.75)}}
	b := &Bot{}
	g.install(b)
	b.comboMarketFn = fakeComboMarket(t)
	_, kb := b.comboScreen(context.Background(), 7, buyableLink)
	stakes := comboStakeButtons(kb)
	want := []string{"10", "25", "50", "100", "250"}
	if len(stakes) != 6 {
		t.Fatalf("stake buttons = %v", stakes)
	}
	for i, s := range stakes {
		if len(s) > 64 {
			t.Errorf("callback %q is %d bytes > 64", s, len(s))
		}
		if i < 5 && !strings.HasSuffix(s, ":"+want[i]) {
			t.Errorf("button %d = %q, want stake %s", i, s, want[i])
		}
	}
	if !strings.HasPrefix(stakes[5], comboCustomPrefix) {
		t.Errorf("last button = %q, want Custom", stakes[5])
	}
}

func TestParseComboStakeInput(t *testing.T) {
	t.Parallel()
	for in, want := range map[string]float64{"75": 75, "$12.5": 12.5, " 1000 ": 1000} {
		if got, err := parseComboStake(in); err != nil || got != want {
			t.Errorf("parseComboStake(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "0", "-5", "abc", "1e400", "NaN"} {
		if _, err := parseComboStake(bad); err == nil {
			t.Errorf("parseComboStake(%q) must fail", bad)
		}
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatal("condition not met within 2s")
}

// Regression (2026-10-22 live test): a Custom stake's result edited the
// original combo screen — by then above the "Enter your stake" prompt and the
// user's reply — so "nothing happened" on screen while the buy filled. The
// result must land in a NEW message below the reply.
func TestComboStakeInput_ReportsInANewMessage(t *testing.T) {
	t.Parallel()

	g := &fakeComboGateway{balanceE6: 100e6, quotes: []*polymarket.ComboQuote{quoteAt(10, 2.75), quoteAt(5, 2.76)}, takes: []*polymarket.ComboTakeResult{filled}}
	b, key := newBuyHarness(t, g)
	b.stateManager = NewStateManager()
	const newMsg = 13099
	var sent []string
	b.comboSendFn = func(_ int64, text string) int { sent = append(sent, text); return newMsg }
	var editedIDs []int
	inner := b.comboEditFn
	b.comboEditFn = func(chatID int64, msgID int, text string, kb *tgbotapi.InlineKeyboardMarkup) {
		editedIDs = append(editedIDs, msgID)
		inner(chatID, msgID, text, kb)
	}

	userCtx := &UserContext{State: StateWaitingForComboStake, Data: map[string]interface{}{"combo_key": key}}
	upd := &tgbotapi.Update{Message: &tgbotapi.Message{Text: "5", Chat: &tgbotapi.Chat{ID: 1}, From: &tgbotapi.User{ID: 7}}}
	b.handleComboStakeInput(context.Background(), upd, userCtx)

	if len(sent) != 1 {
		t.Fatalf("new messages = %d, want 1", len(sent))
	}
	for _, id := range editedIDs {
		if id != newMsg {
			t.Errorf("edited message %d; the result must go to the new message %d, not the old screen", id, newMsg)
		}
	}
	if !strings.Contains(g.lastEdit(), "Combo bought") {
		t.Errorf("want the fill in the new message:\n%s", g.lastEdit())
	}
}
