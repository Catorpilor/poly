package telegram

import (
	"context"
	"fmt"
	"strings"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

// handleLive handles the /live <event-slug> command
func (b *Bot) handleLive(ctx context.Context, bot *Bot, update *tgbotapi.Update) error {
	if b.liveManager == nil {
		b.sendMessage(update.Message.Chat.ID, "Live monitoring is not available.")
		return nil
	}

	args := strings.Fields(update.Message.CommandArguments())
	if len(args) == 0 {
		b.sendMessage(update.Message.Chat.ID, `Usage: /live <event-slug>

Example: /live nba-lal-por-2026-01-17

This will subscribe you to live trade updates for the specified event.
You can subscribe to multiple events at once.

Use /subs to see your active subscriptions.
Use /stoplive <event-slug> to stop monitoring a specific event.
Use /stoplive all to stop all monitoring.`)
		return nil
	}

	eventSlug := args[0]
	chatID := update.Message.Chat.ID

	// Subscribe to the event
	eventInfo, err := b.liveManager.SubscribeTelegram(ctx, chatID, eventSlug)
	if err != nil {
		b.sendMessage(chatID, fmt.Sprintf("Failed to subscribe: %v", err))
		return nil
	}

	// Get current subscription count
	subs := b.liveManager.GetUserSubscriptions(chatID)

	message := fmt.Sprintf(`Subscribed to live trades for:
*%s*

Monitoring %d market(s).
You now have %d active subscription(s).

Use /subs to see all subscriptions.
Use /stoplive %s to stop this subscription.`,
		eventInfo.Title,
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
		message += fmt.Sprintf("%d. %s\n", i+1, slug)
	}
	message += "\nUse /stoplive <event-slug> to stop a specific subscription.\nUse /stoplive all to stop all subscriptions."

	b.sendMessage(chatID, message)
	return nil
}
