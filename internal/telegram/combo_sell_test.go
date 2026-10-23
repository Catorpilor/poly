package telegram

import (
	"context"
	"strings"
	"testing"

	"github.com/Catorpilor/poly/internal/polymarket"
)

const sellCond = "0x03136a66e19d923f1296a3291daea378620000000000000000000000000000"

func heldCombo(sharesE6 int64, legStatuses ...string) *polymarket.ComboPosition {
	c := &polymarket.ComboPosition{
		ComboConditionID: sellCond, Status: "OPEN", SharesE6: sharesE6, Shares: float64(sharesE6) / 1e6, Paid: 5,
		LegsTotal: len(legStatuses),
	}
	names := []string{"Chiefs vs. Seahawks", "Will FC Barcelona win on 2026-10-25?"}
	labels := []string{"Seahawks", "Yes"}
	for i, s := range legStatuses {
		c.Legs = append(c.Legs, polymarket.ComboPositionLeg{PositionID: "L" + labels[i], Question: names[i], OutcomeLabel: labels[i], Status: s, Price: 0.58})
	}
	return c
}

// sellQuoteAt: selling sharesE6 shares, receiving perShare each after fees.
func sellQuoteAt(sharesE6 int64, perShare float64) *polymarket.ComboQuote {
	return &polymarket.ComboQuote{
		QuoteID: "qs", Direction: "SELL", Status: "AWAITING_REQUESTER_ACCEPTANCE", YesPositionID: "999",
		TotalRequiredE6: sharesE6, NetReceiveE6: int64(float64(sharesE6) * perShare),
		MakerAmountE6: sharesE6, TakerAmountE6: int64(float64(sharesE6) * perShare),
	}
}

// newSellHarness opens the sell screen once (displaying the first scripted
// quote's proceeds per share) and returns the bot and its sell key.
func newSellHarness(t *testing.T, g *fakeComboGateway, held *polymarket.ComboPosition) (*Bot, string) {
	t.Helper()
	b := &Bot{}
	g.install(b)
	b.comboPositionFn = func(context.Context, *comboAccount, string) (*polymarket.ComboPosition, error) { return held, nil }
	key := b.rememberComboSell(sellCond)
	text, _ := b.comboSellScreen(context.Background(), 7, key)
	if strings.Contains(text, "❌") {
		t.Fatalf("sell screen failed to open:\n%s", text)
	}
	return b, key
}

func TestComboSellScreen(t *testing.T) {
	t.Parallel()

	g := &fakeComboGateway{quotes: []*polymarket.ComboQuote{sellQuoteAt(13_798_319, 0.3276)}}
	b := &Bot{}
	g.install(b)
	b.comboPositionFn = func(context.Context, *comboAccount, string) (*polymarket.ComboPosition, error) {
		return heldCombo(13_798_319, "OPEN", "OPEN"), nil
	}
	text, kb := b.comboSellScreen(context.Background(), 7, b.rememberComboSell(sellCond))

	for _, s := range []string{"💰 *Sell combo*", "Chiefs vs. Seahawks", "*Seahawks*", "13.80 shares", "paid $5.00", "Live sell quote", "*$4.52*", "0.328/share"} {
		if !strings.Contains(text, s) {
			t.Errorf("missing %q in:\n%s", s, text)
		}
	}
	if g.stakes[0] != 0 {
		t.Errorf("a sell must not quote a BUY stake")
	}
	var buttons []string
	for _, row := range kb.InlineKeyboard {
		for _, btn := range row {
			buttons = append(buttons, *btn.CallbackData)
			if len(*btn.CallbackData) > 64 {
				t.Errorf("callback %q > 64 bytes", *btn.CallbackData)
			}
		}
	}
	joined := strings.Join(buttons, " ")
	for _, want := range []string{":25", ":50", ":75", ":100", comboSellRefreshPrefix} {
		if !strings.Contains(joined, want) {
			t.Errorf("buttons %v missing %q", buttons, want)
		}
	}
}

func TestComboSellScreen_NotSellable(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		held *polymarket.ComboPosition
		want string
	}{
		{"no longer held", nil, "no longer hold"},
		{"a leg lost", heldCombo(10e6, "RESOLVED_LOSS", "OPEN"), "a leg already lost"},
		{"won", func() *polymarket.ComboPosition {
			c := heldCombo(10e6, "RESOLVED_WIN", "RESOLVED_WIN")
			c.Status, c.Redeemable = "REDEEMABLE", true
			return c
		}(), "redeem it on polymarket.com"},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			g := &fakeComboGateway{quotes: []*polymarket.ComboQuote{sellQuoteAt(1, 0.3)}}
			b := &Bot{}
			g.install(b)
			b.comboPositionFn = func(context.Context, *comboAccount, string) (*polymarket.ComboPosition, error) { return tt.held, nil }
			text, kb := b.comboSellScreen(context.Background(), 7, b.rememberComboSell(sellCond))
			if !strings.Contains(text, tt.want) || kb != nil {
				t.Errorf("want %q and no buttons, got kb=%v:\n%s", tt.want, kb != nil, text)
			}
			if q, _ := g.counts(); q != 0 {
				t.Error("nothing to quote")
			}
		})
	}
}

func TestComboSell_TakesWithinToleranceAndReportsSold(t *testing.T) {
	t.Parallel()

	g := &fakeComboGateway{quotes: []*polymarket.ComboQuote{sellQuoteAt(13_790_000, 0.3276), sellQuoteAt(6_890_000, 0.33)}, takes: []*polymarket.ComboTakeResult{filled}}
	b, key := newSellHarness(t, g, heldCombo(13_798_319, "OPEN", "OPEN"))

	b.comboSell(context.Background(), 7, 1, 1, key, 50)

	if q, tk := g.counts(); q != 2 || tk != 1 {
		t.Fatalf("quotes/takes = %d/%d, want 2/1", q, tk)
	}
	out := g.lastEdit()
	for _, s := range []string{"✅ *Combo sold*", "6.89 shares", "received $2.27", "0.330/share", "https://polygonscan.com/tx/0xfeed", "/positions"} {
		if !strings.Contains(out, s) {
			t.Errorf("missing %q in:\n%s", s, out)
		}
	}
}

func TestComboSell_SharesPerPercent(t *testing.T) {
	t.Parallel()

	// The live gateway quotes sells in steps of 0.01 share (13.798319 -> 13.79,
	// 6.899159 -> 6.89; verified 2026-10-22), so the bot rounds down to the
	// step itself and the quote shows exactly what will be sold.
	for pct, want := range map[int]int64{25: 3_440_000, 50: 6_890_000, 75: 10_340_000, 100: 13_790_000} {
		if got := comboSellShares(13_798_319, pct); got != want {
			t.Errorf("%d%% of 13.798319 = %d, want %d (down to 0.01 share)", pct, got, want)
		}
	}
	if got := comboSellShares(8_319, 100); got != 0 {
		t.Errorf("dust below one 0.01 step must sell nothing, got %d", got)
	}
}

func TestComboSell_WorseThanToleranceShowsQuoteAndSellsNothing(t *testing.T) {
	t.Parallel()

	g := &fakeComboGateway{quotes: []*polymarket.ComboQuote{sellQuoteAt(13_798_319, 0.33), sellQuoteAt(13_798_319, 0.30)}, takes: []*polymarket.ComboTakeResult{filled}}
	b, key := newSellHarness(t, g, heldCombo(13_798_319, "OPEN", "OPEN"))

	b.comboSell(context.Background(), 7, 1, 1, key, 100)
	if _, tk := g.counts(); tk != 0 {
		t.Fatal("0.300/share is more than 5% below the displayed 0.330")
	}
	if out := g.lastEdit(); !strings.Contains(out, "Tap 💯 again") || !strings.Contains(out, "$4.14") {
		t.Errorf("want the new quote and a tap-again prompt:\n%s", out)
	}
	b.comboSell(context.Background(), 7, 1, 1, key, 100)
	if _, tk := g.counts(); tk != 1 {
		t.Error("the shown quote becomes the reference: the next tap sells")
	}
}

func TestComboSell_RetryAndFailure(t *testing.T) {
	t.Parallel()

	g := &fakeComboGateway{quotes: []*polymarket.ComboQuote{sellQuoteAt(10e6, 0.33)}, takes: []*polymarket.ComboTakeResult{declined, declined}}
	b, key := newSellHarness(t, g, heldCombo(10e6, "OPEN", "OPEN"))
	b.comboSell(context.Background(), 7, 1, 1, key, 100)
	if _, tk := g.counts(); tk != 2 {
		t.Errorf("takes = %d, want 2 (one automatic retry)", tk)
	}
	if out := g.lastEdit(); !strings.Contains(out, "Nothing was sold") {
		t.Errorf("want a terminal failure:\n%s", out)
	}
}

func TestComboSell_PendingBlocksFurtherSellsAndNeverSaysFailed(t *testing.T) {
	t.Parallel()

	g := &fakeComboGateway{quotes: []*polymarket.ComboQuote{sellQuoteAt(10e6, 0.33)},
		takes: []*polymarket.ComboTakeResult{executing}, statuses: []*polymarket.ComboTakeResult{executing}}
	b, key := newSellHarness(t, g, heldCombo(10e6, "OPEN", "OPEN"))

	b.comboSell(context.Background(), 7, 1, 1, key, 50)
	if out := g.lastEdit(); !strings.Contains(out, "⏳ *Pending*") || strings.Contains(out, "failed") {
		t.Fatalf("want Pending:\n%s", out)
	}
	q0, _ := g.counts()
	b.comboSell(context.Background(), 7, 1, 1, key, 50)
	if q, _ := g.counts(); q != q0 {
		t.Error("a pending sell must block further sells of the same combo")
	}
	waitFor(t, func() bool { return strings.Contains(g.lastEdit(), "unknown") })
	if out := g.lastEdit(); strings.Contains(strings.ToLower(out), "failed") || !strings.Contains(out, "/positions") {
		t.Errorf("unresolved = unknown, never failed:\n%s", out)
	}
}

func TestComboSell_DoesNotBlockBuys(t *testing.T) {
	t.Parallel()

	// Pending state is per direction: the sell key never collides with a
	// Combo Link key, so a pending sell leaves buying alone.
	b := &Bot{}
	sellKey := b.rememberComboSell(sellCond)
	if !b.markComboPending(7, comboSellPendingKey(sellKey)) {
		t.Fatal("mark sell pending")
	}
	if b.comboIsPending(7, b.rememberComboLink(buyableLink)) {
		t.Error("a pending sell must not block buying")
	}
}

func TestComboSellRows(t *testing.T) {
	t.Parallel()

	lost := heldCombo(1e6, "RESOLVED_LOSS", "OPEN")
	rows := (&Bot{}).comboSellRows([]*polymarket.ComboPosition{heldCombo(1e6, "OPEN", "OPEN"), lost, heldCombo(2e6, "RESOLVED_WIN", "OPEN")})
	var labels []string
	for _, r := range rows {
		for _, btn := range r {
			labels = append(labels, btn.Text)
			if !strings.HasPrefix(*btn.CallbackData, comboSellPrefix) || len(*btn.CallbackData) > 64 {
				t.Errorf("callback %q", *btn.CallbackData)
			}
		}
	}
	// Numbers match the Combos section; the lost combo #2 gets no button.
	if strings.Join(labels, ",") != "💰 Sell combo 1,💰 Sell combo 3" {
		t.Errorf("buttons = %v", labels)
	}
}
