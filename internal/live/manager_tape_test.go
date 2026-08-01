package live

import (
	"context"
	"strings"
	"testing"
)

// A Telegram subscription always feeds the snipe watcher and the web tape;
// the batched Telegram trade feed is delivered only to subscriptions created
// (or switched) with the tape flag.

func TestRegistryTapeFlagSemantics(t *testing.T) {
	t.Parallel()
	r := NewSubscriptionRegistry()

	// A default (quiet) subscription is a full telegram subscription — it
	// routes snipe alerts — but not a tape subscription.
	if !r.SubscribeTelegram(7, "evt", false) {
		t.Fatal("first subscribe reported already subscribed")
	}
	if got := r.GetTelegramSubscribers("evt"); len(got) != 1 || got[0] != 7 {
		t.Fatalf("GetTelegramSubscribers = %v, want [7]", got)
	}
	if got := r.TapeSubscribers("evt"); len(got) != 0 {
		t.Fatalf("TapeSubscribers = %v, want none for a quiet subscription", got)
	}
	if r.IsTapeSubscribed(7, "evt") {
		t.Fatal("IsTapeSubscribed = true for a quiet subscription")
	}

	// Re-subscribing with tape keeps the "newly subscribed" contract (false)
	// but switches the flag on.
	if r.SubscribeTelegram(7, "evt", true) {
		t.Fatal("re-subscribe (tape) reported newly subscribed")
	}
	if got := r.TapeSubscribers("evt"); len(got) != 1 || got[0] != 7 {
		t.Fatalf("TapeSubscribers after tape upgrade = %v, want [7]", got)
	}
	if !r.IsTapeSubscribed(7, "evt") {
		t.Fatal("IsTapeSubscribed = false after tape upgrade")
	}

	// ... and back to quiet, same contract.
	if r.SubscribeTelegram(7, "evt", false) {
		t.Fatal("re-subscribe (quiet) reported newly subscribed")
	}
	if got := r.TapeSubscribers("evt"); len(got) != 0 {
		t.Fatalf("TapeSubscribers after downgrade = %v, want none", got)
	}
	if got := r.GetTelegramSubscribers("evt"); len(got) != 1 || got[0] != 7 {
		t.Fatalf("GetTelegramSubscribers after downgrade = %v, want [7] (still subscribed)", got)
	}

	// Unsubscribing a quiet subscription must still report success — the
	// stored bool is a mode, not membership.
	if !r.UnsubscribeTelegram(7, "evt") {
		t.Fatal("UnsubscribeTelegram = false for a quiet subscription")
	}
	if r.HasTelegramSubscribers("evt") {
		t.Fatal("subscriber remains after unsubscribe")
	}
}

func TestRegistryTapeSubscribersMixedModes(t *testing.T) {
	t.Parallel()
	r := NewSubscriptionRegistry()
	r.SubscribeTelegram(7, "evt", false)
	r.SubscribeTelegram(8, "evt", true)

	if got := r.GetTelegramSubscribers("evt"); len(got) != 2 {
		t.Fatalf("GetTelegramSubscribers = %v, want both modes", got)
	}
	if got := r.TapeSubscribers("evt"); len(got) != 1 || got[0] != 8 {
		t.Fatalf("TapeSubscribers = %v, want [8]", got)
	}
}

// A quiet subscription must never feed the Telegram batcher, whatever the
// trade size, while the web tape keeps receiving the same trade.
func TestBroadcastToTelegramQuietSubscriptionGetsNoTape(t *testing.T) {
	t.Parallel()

	m, sender, timers := newFeedWiredManager(t)
	server, client := newWSPair(t)
	m.RegisterWebConn(server)
	if err := m.SubscribeWeb(server, pinnedFeedEventSlug, false); err != nil {
		t.Fatalf("SubscribeWeb: %v", err)
	}
	if _, err := m.SubscribeTelegram(context.Background(), feedTestChat, pinnedFeedEventSlug, false); err != nil {
		t.Fatalf("SubscribeTelegram: %v", err)
	}

	if !m.handleTrade(feedWiringTrade(FeedMinTradeUSD + 5)) {
		t.Fatal("trade should match the subscription")
	}
	if got := timers.count(); got != 0 {
		t.Fatalf("quiet subscription armed %d flush timers, want 0", got)
	}
	if got := sender.messages(); len(got) != 0 {
		t.Fatalf("quiet subscription relayed trade prints: %v", got)
	}
	if got := readTradeFrame(t, client); got != pinnedFeedEventSlug {
		t.Fatalf("web frame routed to %q, want %q", got, pinnedFeedEventSlug)
	}
}

// Re-subscribing with tape switches a live quiet subscription to delivery.
func TestSubscribeTelegramTapeUpgradeStartsDelivery(t *testing.T) {
	t.Parallel()

	m, sender, timers := newFeedWiredManager(t)
	ctx := context.Background()
	if _, err := m.SubscribeTelegram(ctx, feedTestChat, pinnedFeedEventSlug, false); err != nil {
		t.Fatalf("SubscribeTelegram(quiet): %v", err)
	}

	m.handleTrade(feedWiringTrade(30))
	if got := sender.messages(); len(got) != 0 {
		t.Fatalf("quiet subscription relayed trade prints: %v", got)
	}

	if _, err := m.SubscribeTelegram(ctx, feedTestChat, pinnedFeedEventSlug, true); err != nil {
		t.Fatalf("SubscribeTelegram(tape): %v", err)
	}
	if !m.IsTapeSubscription(feedTestChat, pinnedFeedEventSlug) {
		t.Fatal("IsTapeSubscription = false after tape re-subscribe")
	}

	m.handleTrade(feedWiringTrade(30))
	timers.timer(0).fire()
	msgs := sender.messages()
	if len(msgs) != 1 {
		t.Fatalf("tape subscription delivered %d messages, want 1", len(msgs))
	}
	if !strings.Contains(msgs[0].text, "Whale123") {
		t.Errorf("flushed message missing trade line: %q", msgs[0].text)
	}
}
