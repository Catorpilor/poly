package telegram

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"

	"github.com/Catorpilor/poly/internal/database"
	"github.com/Catorpilor/poly/internal/live"
	"github.com/Catorpilor/poly/internal/polymarket"
)

// Issue #97: per-game markets carry the SERIES gameStartTime, so a
// not-yet-played game passes the in-play gate the moment the series starts.
// The future-game gate withholds the AUTO buy when an earlier game of the
// same event is demonstrably still live; taps stay ungated; everything
// ambiguous fails open.

func TestGameNumber(t *testing.T) {
	t.Parallel()
	tests := []struct {
		question string
		want     int
	}{
		{"Dota 2: TEAM VISION vs Team Spirit - Game 3 Winner", 3},
		{"Counter-Strike: FUT Esports vs Spirit - Map 4 Winner", 4},
		{"LoL: ThunderTalk Gaming vs LGD Gaming - Game 1 Winner", 1},
		{"Dota 2: TEAM VISION vs Team Spirit (BO5) - The International Playoffs", 0},
		{"Will CA Osasuna win on 2026-08-24?", 0},
		{"Game 1: Both Teams Beat Roshan?", 0},
		{"Total Kills Over/Under 50.5 in Game 1?", 0},
		{"First Blood in Game 2?", 0},
	}
	for _, tt := range tests {
		if got := live.GameNumber(tt.question); got != tt.want {
			t.Errorf("GameNumber(%q) = %d, want %d", tt.question, got, tt.want)
		}
	}
}

func TestSnipeGameLive(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		prices []string
		want   bool
	}{
		{"mid-game", []string{"0.6", "0.4"}, true},
		{"decided high", []string{"0.9995", "0.0005"}, false},
		{"decided low", []string{"0.02", "0.98"}, false},
		{"empty", nil, false},
		{"unparseable", []string{"", "x"}, false},
	}
	for _, tt := range tests {
		if got := snipeGameLive(tt.prices); got != tt.want {
			t.Errorf("%s: snipeGameLive(%v) = %v, want %v", tt.name, tt.prices, got, tt.want)
		}
	}
}

// futureGameHarness wires a Gamma stub serving the alerted Game 3 market AND
// its event, whose Game 2 state is scripted per test.
func futureGameHarness(t *testing.T, game2Prices string, game2Closed bool, eventStatus int) (*Bot, *tgRecorder, *buyRecorder) {
	t.Helper()
	tg := &tgRecorder{}
	tgSrv := httptest.NewServer(tg)
	t.Cleanup(tgSrv.Close)
	api, err := tgbotapi.NewBotAPIWithClient("test-token", tgSrv.URL+"/bot%s/%s", tgSrv.Client())
	if err != nil {
		t.Fatalf("bot api: %v", err)
	}

	tok := strings.Repeat("3", 78)
	sib := strings.Repeat("4", 78)
	tokList := fmt.Sprintf(`["%s","%s"]`, tok, sib)
	gamma := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/markets/g3":
			fmt.Fprintf(w, `{"id":"g3","question":"Dota 2: A vs B - Game 3 Winner","conditionId":"c3","outcomes":"[\"A\",\"B\"]","clobTokenIds":%q,"gameStartTime":"2026-08-26 10:00:00+00","active":true,"closed":false,"events":[{"slug":"dota2-a-b-2026"}]}`, tokList)
		case r.URL.Path == "/events" && r.URL.Query().Get("slug") == "dota2-a-b-2026":
			if eventStatus != 0 {
				http.Error(w, "boom", eventStatus)
				return
			}
			fmt.Fprintf(w, `[{"slug":"dota2-a-b-2026","markets":[
				{"id":"g2","question":"Dota 2: A vs B - Game 2 Winner","outcomes":"[\"A\",\"B\"]","outcomePrices":%q,"clobTokenIds":"[\"x1\",\"x2\"]","gameStartTime":"2026-08-26 10:00:00+00","active":true,"closed":%v},
				{"id":"g3","question":"Dota 2: A vs B - Game 3 Winner","outcomes":"[\"A\",\"B\"]","outcomePrices":"[\"0.2\",\"0.8\"]","clobTokenIds":%q,"gameStartTime":"2026-08-26 10:00:00+00","active":true,"closed":false}
			]}]`, game2Prices, game2Closed, tokList)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(gamma.Close)

	buys := &buyRecorder{result: &polymarket.TradeResult{Success: true, OrderID: "ord-fg", FilledSize: 49.8, AveragePrice: 0.2}}
	b := &Bot{
		api:             api,
		userRepo:        &fakeSnipeUserRepo{user: &database.User{TelegramID: 7, EOAAddress: "0xabc", EncryptedKey: "enc"}},
		snipeFeed:       &fakeAskSource{ask: 0.20, askOK: true, bid: 0.20, bidOK: true},
		snipeAlerts:     newSnipeAlertRegistry(),
		snipeSpend:      newSnipeSpendLedger(snipeAutoBuyDailyCapUSD),
		snipeDeepSpend:  newSnipeSpendLedger(snipeDeepDailyCapUSD),
		snipeBought:     newSnipeBoughtRecord(),
		snipeBoxedLatch: newSnipeBoxedLatch(),
		snipePositions:  &fakePositionSource{},
		snipeWatcher:    &fakeSnipeWatch{},
		snipeMarkets:    polymarket.NewMarketClientWithURL(gamma.URL),
	}
	b.snipeBuyExec = func(_ context.Context, user *database.User, _ *polymarket.GammaMarket, idx int, amount float64) *polymarket.TradeResult {
		return buys.record(user, idx, amount)
	}
	return b, tg, buys
}

func futureGameMarket() live.SnipeMarket {
	return live.SnipeMarket{
		TokenID:  strings.Repeat("3", 78),
		MarketID: "g3",
		Question: "Dota 2: A vs B - Game 3 Winner",
		Outcome:  "A",
	}
}

// Game 2 mid-play (0.6/0.4, open) → the Game 3 auto-buy is future-gated:
// alert-only DM with the distinct note, no buy executed.
func TestFutureGameGate_BlocksAutoBuy(t *testing.T) {
	t.Parallel()
	b, tg, buys := futureGameHarness(t, `["0.6","0.4"]`, false, 0)

	b.NotifySnipeAlert(7, futureGameMarket(), 0.55, 0.20)

	if got := buys.count(); got != 0 {
		t.Fatalf("auto-buy executed %d times on a future game, want 0", got)
	}
	assertSentContains(t, tg, "hasn't started")
}

// All earlier games decided (0.9995/0.0005, even if not yet closed) → the
// between-games gap fails open and the buy proceeds.
func TestFutureGameGate_FailsOpenBetweenGames(t *testing.T) {
	t.Parallel()
	b, _, buys := futureGameHarness(t, `["0.9995","0.0005"]`, false, 0)

	b.NotifySnipeAlert(7, futureGameMarket(), 0.55, 0.20)

	if got := buys.count(); got != 1 {
		t.Fatalf("auto-buy count = %d, want 1 (decided earlier game fails open)", got)
	}
}

// Event fetch failure → fail open: the buy proceeds.
func TestFutureGameGate_FailsOpenOnFetchError(t *testing.T) {
	t.Parallel()
	b, _, buys := futureGameHarness(t, `["0.6","0.4"]`, false, http.StatusInternalServerError)

	b.NotifySnipeAlert(7, futureGameMarket(), 0.55, 0.20)

	if got := buys.count(); got != 1 {
		t.Fatalf("auto-buy count = %d, want 1 (fetch error fails open)", got)
	}
}

// The manual tap is never future-gated: the same mid-game-2 state buys via
// the tap path.
func TestFutureGameGate_TapUngated(t *testing.T) {
	t.Parallel()
	b, tg, buys := futureGameHarness(t, `["0.6","0.4"]`, false, 0)

	b.NotifySnipeAlert(7, futureGameMarket(), 0.55, 0.20)
	if got := buys.count(); got != 0 {
		t.Fatalf("precondition: auto-buy gated, got %d buys", got)
	}
	tg.mu.Lock()
	markup := tg.sends[len(tg.sends)-1].markup
	tg.mu.Unlock()
	data := callbackData(t, markup, "⚡ Snipe $10")

	b.handleSnipeCallback(context.Background(), snipeTapUpdate(7, data))

	if got := buys.count(); got != 1 {
		t.Fatalf("tap buy count = %d, want 1 (taps are never gated)", got)
	}
}
