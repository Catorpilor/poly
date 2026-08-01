package telegram

import (
	"context"
	"fmt"
	"strings"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

// liveUsageText is shown for /live without a slug or with an unrecognized
// keyword.
const liveUsageText = `Usage: /live <event-slug> [tape]

Example: /live nba-lal-por-2026-01-17

Subscribes you to an event: quiet by default — snipe watch armed, no
trade prints. Add 'tape' for batched trade prints ≥ $20 every 5s.
Re-run with or without 'tape' to switch an existing subscription's mode.
You can subscribe to multiple events at once.

Use /subs to see your active subscriptions.
Use /stoplive <event-slug> to stop monitoring a specific event.
Use /stoplive all to stop all monitoring.`

// parseLiveArgs parses /live arguments: an event slug plus an optional
// case-insensitive 'tape' keyword. ok=false (missing slug, unknown keyword,
// extra args) sends the caller to the usage text. Pure — table-tested.
func parseLiveArgs(args []string) (eventSlug string, tape bool, ok bool) {
	switch len(args) {
	case 1:
		return args[0], false, true
	case 2:
		if strings.EqualFold(args[1], "tape") {
			return args[0], true, true
		}
	}
	return "", false, false
}

// liveModeText describes a subscription's delivery mode in the /live
// confirmation. Pure — table-tested.
func liveModeText(tape bool) string {
	if tape {
		return "tape on — batched trade prints ≥ $20 every 5s"
	}
	return "quiet — snipe watch armed; add 'tape' for trade prints"
}

// handleLive handles the /live <event-slug> [tape] command
func (b *Bot) handleLive(ctx context.Context, bot *Bot, update *tgbotapi.Update) error {
	if b.liveManager == nil {
		b.sendMessage(update.Message.Chat.ID, "Live monitoring is not available.")
		return nil
	}

	args := strings.Fields(update.Message.CommandArguments())
	eventSlug, tape, ok := parseLiveArgs(args)
	if !ok {
		b.sendMessage(update.Message.Chat.ID, liveUsageText)
		return nil
	}

	chatID := update.Message.Chat.ID

	// Subscribe to the event
	eventInfo, err := b.liveManager.SubscribeTelegram(ctx, chatID, eventSlug, tape)
	if err != nil {
		b.sendMessage(chatID, fmt.Sprintf("Failed to subscribe: %v", err))
		return nil
	}

	// Get current subscription count
	subs := b.liveManager.GetUserSubscriptions(chatID)

	message := fmt.Sprintf(`Subscribed to live trades for:
*%s*

Mode: %s.
Monitoring %d market(s).
You now have %d active subscription(s).

Use /subs to see all subscriptions.
Use /stoplive %s to stop this subscription.`,
		eventInfo.Title,
		liveModeText(tape),
		len(eventInfo.Markets),
		len(subs),
		eventSlug,
	)

	b.sendMessage(chatID, message)
	return nil
}

// handleStopLive handles the /stoplive command
func (b *Bot) handleStopLive(ctx context.Context, bot *Bot, update *tgbotapi.Update) error {
	if b.liveManager == nil {
		b.sendMessage(update.Message.Chat.ID, "Live monitoring is not available.")
		return nil
	}

	args := strings.Fields(update.Message.CommandArguments())
	chatID := update.Message.Chat.ID

	if len(args) == 0 {
		// Show usage
		subs := b.liveManager.GetUserSubscriptions(chatID)
		if len(subs) == 0 {
			b.sendMessage(chatID, "You don't have any active subscriptions.")
			return nil
		}

		message := `Usage: /stoplive <event-slug> or /stoplive all

Your active subscriptions:
`
		for _, sub := range subs {
			message += fmt.Sprintf("- %s\n", sub)
		}

		b.sendMessage(chatID, message)
		return nil
	}

	arg := strings.ToLower(args[0])

	if arg == "all" {
		// Unsubscribe from all events
		unsubscribed := b.liveManager.UnsubscribeAllTelegram(chatID)
		if len(unsubscribed) == 0 {
			b.sendMessage(chatID, "You don't have any active subscriptions.")
			return nil
		}

		message := fmt.Sprintf("Stopped monitoring %d event(s):\n", len(unsubscribed))
		for _, slug := range unsubscribed {
			message += fmt.Sprintf("- %s\n", slug)
		}

		b.sendMessage(chatID, message)
		return nil
	}

	// Unsubscribe from specific event
	eventSlug := args[0]
	if !b.liveManager.UnsubscribeTelegram(chatID, eventSlug) {
		b.sendMessage(chatID, fmt.Sprintf("You're not subscribed to: %s", eventSlug))
		return nil
	}

	// Get remaining subscriptions
	remaining := b.liveManager.GetUserSubscriptions(chatID)
	message := fmt.Sprintf("Stopped monitoring: %s\n\nYou have %d active subscription(s) remaining.",
		eventSlug, len(remaining))

	b.sendMessage(chatID, message)
	return nil
}

// handleSubs handles the /subs command
func (b *Bot) handleSubs(ctx context.Context, bot *Bot, update *tgbotapi.Update) error {
	if b.liveManager == nil {
		b.sendMessage(update.Message.Chat.ID, "Live monitoring is not available.")
		return nil
	}

	chatID := update.Message.Chat.ID
	subs := b.liveManager.GetUserSubscriptions(chatID)

	if len(subs) == 0 {
		b.sendMessage(chatID, `You don't have any active subscriptions.

Use /live <event-slug> to start monitoring an event.
Example: /live nba-lal-por-2026-01-17`)
		return nil
	}

	message := fmt.Sprintf("*Your Active Subscriptions (%d):*\n\n", len(subs))
	for i, slug := range subs {
		mode := "quiet"
		if b.liveManager.IsTapeSubscription(chatID, slug) {
			mode = "tape"
		}
		message += fmt.Sprintf("%d. %s (%s)\n", i+1, slug, mode)
	}
	message += "\nUse /live <event-slug> tape to switch a subscription to trade prints.\nUse /stoplive <event-slug> to stop a specific subscription.\nUse /stoplive all to stop all subscriptions."

	b.sendMessage(chatID, message)
	return nil
}
