package live

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/shopspring/decimal"
)

// The manager's Telegram relay must go through the feed batcher: sub-floor
// trades dropped, survivors coalesced per (chat, subscription), pending
// batches flushed on unsubscribe. The web broadcast path stays direct.

// newFeedWiredManager builds a manager with a cached event, a fake Telegram
// sender behind the batcher, and manual flush timers.
func newFeedWiredManager(t *testing.T) (*LiveTradeManager, *fakeFeedSender, *fakeFeedTimerFactory) {
	t.Helper()

	m := &LiveTradeManager{
		subscriptions:     NewSubscriptionRegistry(),
		resolver:          NewEventSlugResolver(),
		formatter:         NewTradeFormatter(),
		assetToEvent:      make(map[string]string),
		assetToMarketName: make(map[string]string),
		subscribed:        true, // skip the RTDS dial in asset tracking
	}
	m.resolver.cacheEvent(pinnedFeedEventSlug, pinnedFeedEvent())

	sender := &fakeFeedSender{}
	m.SetTelegramBot(sender)
	timers := &fakeFeedTimerFactory{}
	m.feedBatcher.newTimer = timers.newTimer
	return m, sender, timers
}

// feedWiringTrade is an asset-matched Moneyline trade with a USD size.
func feedWiringTrade(usd float64) *rtdsTradePayload {
	trade := pinnedFeedTrade("ml-blg", pinnedFeedEventSlug, "BLG")
	trade.Name = "Whale123"
	trade.Size = decimal.NewFromFloat(usd)
	trade.Price = decimal.NewFromFloat(0.86)
	return trade
}

func TestBroadcastToTelegramBatchesTrades(t *testing.T) {
	t.Parallel()

	m, sender, timers := newFeedWiredManager(t)
	if _, err := m.SubscribeTelegram(context.Background(), feedTestChat, pinnedFeedEventSlug, true); err != nil {
		t.Fatalf("SubscribeTelegram: %v", err)
	}

	if !m.handleTrade(feedWiringTrade(25)) {
		t.Fatal("trade should match the telegram subscription")
	}
	if got := sender.messages(); len(got) != 0 {
		t.Fatalf("trade relayed immediately, want buffered until the window flushes: %v", got)
	}

	timers.timer(0).fire()
	msgs := sender.messages()
	if len(msgs) != 1 {
		t.Fatalf("window flush sent %d messages, want 1", len(msgs))
	}
	if msgs[0].chatID != feedTestChat {
		t.Errorf("flush sent to chat %d, want %d", msgs[0].chatID, feedTestChat)
	}
	if !strings.Contains(msgs[0].text, "Whale123 BUY BLG $25.00 @ $0.86") {
		t.Errorf("flushed message missing trade line: %q", msgs[0].text)
	}
	if !strings.HasPrefix(msgs[0].text, "["+ShortenEventSlug(pinnedFeedEventSlug)+"] Live trades\n") {
		t.Errorf("flushed message missing subscription header: %q", msgs[0].text)
	}
}

func TestBroadcastToTelegramDropsSubFloorTrades(t *testing.T) {
	t.Parallel()

	m, sender, timers := newFeedWiredManager(t)
	if _, err := m.SubscribeTelegram(context.Background(), feedTestChat, pinnedFeedEventSlug, true); err != nil {
		t.Fatalf("SubscribeTelegram: %v", err)
	}

	if !m.handleTrade(feedWiringTrade(FeedMinTradeUSD - 0.01)) {
		t.Fatal("sub-floor trade still matches the subscription (only the relay drops it)")
	}
	if got := timers.count(); got != 0 {
		t.Fatalf("sub-floor trade armed %d timers, want 0", got)
	}
	if got := sender.messages(); len(got) != 0 {
		t.Fatalf("sub-floor trade was relayed: %v", got)
	}
}

func TestUnsubscribeTelegramFlushesPendingBatch(t *testing.T) {
	t.Parallel()

	m, sender, timers := newFeedWiredManager(t)
	if _, err := m.SubscribeTelegram(context.Background(), feedTestChat, pinnedFeedEventSlug, true); err != nil {
		t.Fatalf("SubscribeTelegram: %v", err)
	}

	m.handleTrade(feedWiringTrade(30))
	if !m.UnsubscribeTelegram(feedTestChat, pinnedFeedEventSlug) {
		t.Fatal("UnsubscribeTelegram: not subscribed")
	}

	msgs := sender.messages()
	if len(msgs) != 1 {
		t.Fatalf("unsubscribe flushed %d messages, want 1 immediately", len(msgs))
	}
	timers.timer(0).fire()
	if got := sender.messages(); len(got) != 1 {
		t.Fatalf("stopped timer double-sent: %d messages, want 1", len(got))
	}
}

func TestUnsubscribeAllTelegramFlushesPendingBatches(t *testing.T) {
	t.Parallel()

	m, sender, _ := newFeedWiredManager(t)
	if _, err := m.SubscribeTelegram(context.Background(), feedTestChat, pinnedFeedEventSlug, true); err != nil {
		t.Fatalf("SubscribeTelegram: %v", err)
	}

	m.handleTrade(feedWiringTrade(30))
	if got := m.UnsubscribeAllTelegram(feedTestChat); len(got) != 1 {
		t.Fatalf("UnsubscribeAllTelegram removed %v, want 1 event", got)
	}
	if got := sender.messages(); len(got) != 1 {
		t.Fatalf("unsubscribe-all flushed %d messages, want 1 immediately", len(got))
	}
}

// The web panel path is unbatched and unfiltered: a sub-floor trade still
// reaches the web subscriber immediately while Telegram drops it.
func TestBroadcastToWebUnaffectedByBatcher(t *testing.T) {
	t.Parallel()

	m, sender, _ := newFeedWiredManager(t)
	server, client := newWSPair(t)
	m.RegisterWebConn(server)
	if err := m.SubscribeWeb(server, pinnedFeedEventSlug, false); err != nil {
		t.Fatalf("SubscribeWeb: %v", err)
	}
	if _, err := m.SubscribeTelegram(context.Background(), feedTestChat, pinnedFeedEventSlug, true); err != nil {
		t.Fatalf("SubscribeTelegram: %v", err)
	}

	if !m.handleTrade(feedWiringTrade(FeedMinTradeUSD - 0.01)) {
		t.Fatal("trade should match")
	}
	if got := readTradeFrame(t, client); got != pinnedFeedEventSlug {
		t.Fatalf("web frame routed to %q, want %q", got, pinnedFeedEventSlug)
	}
	if got := sender.messages(); len(got) != 0 {
		t.Fatalf("telegram relayed a sub-floor trade: %v", got)
	}
}

func TestFormatTelegramLine(t *testing.T) {
	t.Parallel()

	f := NewTradeFormatter()
	tests := []struct {
		name  string
		trade *TradeInfo
		want  string
	}{
		{
			name: "team outcome",
			trade: &TradeInfo{
				EventSlug: "atp-norrie-shapova-2026-08-01",
				Pseudonym: "Whale123",
				Side:      "BUY",
				Outcome:   "Denis Shapovalov",
				Size:      decimal.NewFromFloat(136),
				Price:     decimal.NewFromFloat(0.86),
			},
			want: "Whale123 BUY Denis Shapovalov $136.00 @ $0.86",
		},
		{
			name: "3-way ML yes/no",
			trade: &TradeInfo{
				EventSlug:  "epl-ast-eve-2026-01-18",
				Pseudonym:  "Bob",
				Side:       "sell",
				Outcome:    "Yes",
				MarketName: "DRAW",
				Size:       decimal.NewFromFloat(50),
				Price:      decimal.NewFromFloat(0.25),
			},
			want: "Bob SELL DRAW Yes $50.00 @ $0.25",
		},
		{
			name: "sub-market",
			trade: &TradeInfo{
				EventSlug:   "epl-ast-eve-2026-01-18",
				Pseudonym:   "Carol",
				Side:        "BUY",
				Outcome:     "Yes",
				MarketName:  "Over 2.5",
				IsSubMarket: true,
				Size:        decimal.NewFromFloat(20),
				Price:       decimal.NewFromFloat(0.60),
			},
			want: "Carol BUY Over 2.5 $20.00 @ $0.60",
		},
		{
			name: "anonymous wallet",
			trade: &TradeInfo{
				EventSlug:   "atp-norrie-shapova-2026-08-01",
				ProxyWallet: "0x1234567890abcdef1234567890abcdef12345678",
				Side:        "BUY",
				Outcome:     "Norrie",
				Size:        decimal.NewFromFloat(21),
				Price:       decimal.NewFromFloat(0.14),
			},
			want: "0x1234...5678 BUY Norrie $21.00 @ $0.14",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := f.FormatTelegramLine(tt.trade); got != tt.want {
				t.Errorf("FormatTelegramLine() = %q, want %q", got, tt.want)
			}
		})
	}
}

// Guard against flakiness in feedWiringTrade timestamps: pinnedFeedTrade
// stamps time.Now, keep it fresh relative to the stale filter.
func TestFeedWiringTradeIsFresh(t *testing.T) {
	t.Parallel()
	trade := feedWiringTrade(25)
	age := time.Now().UnixMilli() - trade.Timestamp
	if age > time.Minute.Milliseconds() {
		t.Fatalf("fixture trade is %dms old, would trip the stale filter", age)
	}
}
