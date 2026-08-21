package telegram

import (
	"context"
	"fmt"
	"log"
	"strconv"
	"strings"

	"github.com/Catorpilor/poly/internal/live"
	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

// liveSubscriptions is the slice of the live trade manager the /live, /stoplive
// and /subs handlers drive. *live.LiveTradeManager satisfies it in production;
// tests inject a fake (see fakeLiveSubs) so fan-out logic can be exercised
// without a Polymarket resolve or a live WebSocket.
type liveSubscriptions interface {
	SubscribeTelegram(ctx context.Context, chatID int64, eventSlug string, tape bool) (*live.EventInfo, error)
	UnsubscribeTelegram(chatID int64, eventSlug string) bool
	UnsubscribeAllTelegram(chatID int64) []string
	GetUserSubscriptions(chatID int64) []string
	IsTapeSubscription(chatID int64, eventSlug string) bool
}

// liveSubscriptionMgr returns the subscription backend: the injected test seam
// when present, else the production manager. Returns a nil interface only when
// live monitoring is genuinely unavailable, so callers can keep the existing
// "not available" guard.
func (b *Bot) liveSubscriptionMgr() liveSubscriptions {
	if b.liveSubs != nil {
		return b.liveSubs
	}
	if b.liveManager == nil {
		return nil
	}
	return b.liveManager
}

// household returns the linked-account chat IDs (LINKED_CHAT_IDS). Empty when
// the feature is off or config is absent.
func (b *Bot) household() []int64 {
	if b.config == nil {
		return nil
	}
	return b.config.Telegram.LinkedChatIDs
}

// isHouseholdMember reports whether chatID belongs to the linked household.
func isHouseholdMember(chatID int64, members []int64) bool {
	for _, m := range members {
		if m == chatID {
			return true
		}
	}
	return false
}

// otherMembers returns the household members that are not the issuer,
// de-duplicated and in config order. The issuer is fanned out separately
// because its personal tape flag differs from the quiet-on-create default.
func otherMembers(issuer int64, members []int64) []int64 {
	out := make([]int64, 0, len(members))
	seen := make(map[int64]bool, len(members))
	for _, m := range members {
		if m == issuer || seen[m] {
			continue
		}
		seen[m] = true
		out = append(out, m)
	}
	return out
}

// issuerLabel renders the fan-out issuer for the "never silent" member DMs:
// @username if set, else a first/last name, else a numeric chat id. Cosmetic.
func issuerLabel(from *tgbotapi.User, chatID int64) string {
	if from != nil {
		if from.UserName != "" {
			return "@" + from.UserName
		}
		if name := strings.TrimSpace(from.FirstName + " " + from.LastName); name != "" {
			return name
		}
	}
	return fmt.Sprintf("account %d", chatID)
}

// fanoutSubscribeNotice is the DM every OTHER household member receives when a
// member issues /live — the "never silent" guarantee (#90). It names the
// issuer, states full recipiency (each member's own snipe auto-buy fires), and
// gives the per-event opt-out.
func fanoutSubscribeNotice(issuer, slug string) string {
	return fmt.Sprintf("%s subscribed this household to %s — you are a full recipient; snipe auto-buy applies. /stoplive %s to opt out.",
		issuer, slug, slug)
}

// fanoutUnsubscribeNotice mirrors a member's /stoplive <slug> to every other
// member.
func fanoutUnsubscribeNotice(issuer, slug string) string {
	return fmt.Sprintf("%s unsubscribed this household from %s. You are no longer a recipient. /live %s to re-subscribe.",
		issuer, slug, slug)
}

// fanoutStopAllNotice mirrors a member's /stoplive all to every other member.
// It spells out the sharp edge: 'all' is literal (#90) — it clears each
// member's ENTIRE subscription set, including events that were never
// household-subscribed. slugs is what was actually cleared for that member.
func fanoutStopAllNotice(issuer string, slugs []string) string {
	return fmt.Sprintf("%s ran /stoplive all for this household — ALL %d of your live subscription(s) were cleared (%s). /live <event-slug> to re-subscribe.",
		issuer, len(slugs), strings.Join(slugs, ", "))
}

// fanoutConfirmSuffix is appended to the issuer's own confirmation. n is the
// number of OTHER members the action reached.
func fanoutConfirmSuffix(n int) string {
	if n == 0 {
		return ""
	}
	return fmt.Sprintf("\n\nFanned out to %d linked account(s).", n)
}

// fanoutFailureNote lists members a fan-out could not reach so a partial
// failure is never silent (#90).
func fanoutFailureNote(failed []int64) string {
	if len(failed) == 0 {
		return ""
	}
	parts := make([]string, len(failed))
	for i, id := range failed {
		parts[i] = strconv.FormatInt(id, 10)
	}
	return fmt.Sprintf("\n\n⚠️ Could not fan out to %d account(s): %s. Re-run the command to retry.",
		len(failed), strings.Join(parts, ", "))
}

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
	mgr := b.liveSubscriptionMgr()
	if mgr == nil {
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

	// Subscribe the issuer first: this both resolves the event (the shared
	// resolve every member relies on) and applies the issuer's personal tape
	// flag. A resolve failure here means NO member could subscribe (same slug,
	// same resolver), so fail exactly as the single-account path always has.
	eventInfo, err := mgr.SubscribeTelegram(ctx, chatID, eventSlug, tape)
	if err != nil {
		b.sendMessage(chatID, fmt.Sprintf("Failed to subscribe: %v", err))
		return nil
	}

	subs := mgr.GetUserSubscriptions(chatID)

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

	// Household fan-out (#90): only when the issuer is a linked member.
	// Non-members and the feature-off empty list keep bit-identical
	// single-account behavior.
	members := b.household()
	if isHouseholdMember(chatID, members) {
		issuer := issuerLabel(update.Message.From, chatID)
		var reached int
		var failed []int64
		for _, m := range otherMembers(chatID, members) {
			// Tape is personal: a member that already chose tape keeps it, a new
			// or quiet member stays quiet. IsTapeSubscription is false for both
			// "not subscribed" and "subscribed quiet", so it yields exactly the
			// preserve-or-quiet flag with no extra lookup — and never downgrades
			// a member who opted into tape.
			memberTape := mgr.IsTapeSubscription(m, eventSlug)
			if _, err := mgr.SubscribeTelegram(ctx, m, eventSlug, memberTape); err != nil {
				log.Printf("handleLive: fan-out subscribe failed (issuer=%d member=%d slug=%s): %v", chatID, m, eventSlug, err)
				failed = append(failed, m)
				continue
			}
			reached++
			b.sendMessage(m, fanoutSubscribeNotice(issuer, eventSlug))
		}
		message += fanoutConfirmSuffix(reached) + fanoutFailureNote(failed)
	}

	b.sendMessage(chatID, message)
	return nil
}

// handleStopLive handles the /stoplive command
func (b *Bot) handleStopLive(ctx context.Context, bot *Bot, update *tgbotapi.Update) error {
	mgr := b.liveSubscriptionMgr()
	if mgr == nil {
		b.sendMessage(update.Message.Chat.ID, "Live monitoring is not available.")
		return nil
	}

	args := strings.Fields(update.Message.CommandArguments())
	chatID := update.Message.Chat.ID

	if len(args) == 0 {
		// Show usage
		subs := mgr.GetUserSubscriptions(chatID)
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

	members := b.household()
	isMember := isHouseholdMember(chatID, members)
	issuer := issuerLabel(update.Message.From, chatID)

	if strings.ToLower(args[0]) == "all" {
		// Issuer's own clear first.
		unsubscribed := mgr.UnsubscribeAllTelegram(chatID)

		if !isMember {
			// Single-account behavior — bit-identical to the pre-#90 output.
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

		// Household fan-out: 'all' is literal (#90) — clear each OTHER member's
		// ENTIRE subscription set, including events never household-subscribed.
		// A member whose state actually changed gets the mirror DM; one with
		// nothing to clear is left alone (no state change ⇒ nothing to voice).
		var reached int
		for _, m := range otherMembers(chatID, members) {
			cleared := mgr.UnsubscribeAllTelegram(m)
			if len(cleared) == 0 {
				continue
			}
			reached++
			b.sendMessage(m, fanoutStopAllNotice(issuer, cleared))
		}

		if len(unsubscribed) == 0 && reached == 0 {
			b.sendMessage(chatID, "You don't have any active subscriptions.")
			return nil
		}
		b.sendMessage(chatID, stopAllSummary(unsubscribed, reached))
		return nil
	}

	// Unsubscribe from specific event.
	eventSlug := args[0]
	issuerUnsub := mgr.UnsubscribeTelegram(chatID, eventSlug)

	if !isMember {
		// Single-account behavior, unchanged.
		if !issuerUnsub {
			b.sendMessage(chatID, fmt.Sprintf("You're not subscribed to: %s", eventSlug))
			return nil
		}
		remaining := mgr.GetUserSubscriptions(chatID)
		b.sendMessage(chatID, fmt.Sprintf("Stopped monitoring: %s\n\nYou have %d active subscription(s) remaining.",
			eventSlug, len(remaining)))
		return nil
	}

	// Household fan-out: unsubscribe every other member from this slug. Only
	// members that were actually subscribed change state, so only they get the
	// mirror DM.
	var reached int
	for _, m := range otherMembers(chatID, members) {
		if mgr.UnsubscribeTelegram(m, eventSlug) {
			reached++
			b.sendMessage(m, fanoutUnsubscribeNotice(issuer, eventSlug))
		}
	}

	// If neither the issuer nor any member was subscribed, report as unchanged.
	if !issuerUnsub && reached == 0 {
		b.sendMessage(chatID, fmt.Sprintf("You're not subscribed to: %s", eventSlug))
		return nil
	}

	remaining := mgr.GetUserSubscriptions(chatID)
	message := fmt.Sprintf("Stopped monitoring: %s\n\nYou have %d active subscription(s) remaining.",
		eventSlug, len(remaining)) + fanoutConfirmSuffix(reached)
	b.sendMessage(chatID, message)
	return nil
}

// stopAllSummary renders the issuer's /stoplive all confirmation: the issuer's
// own cleared events plus, when part of a household, the fan-out count.
func stopAllSummary(unsubscribed []string, reached int) string {
	message := fmt.Sprintf("Stopped monitoring %d of your event(s):\n", len(unsubscribed))
	for _, slug := range unsubscribed {
		message += fmt.Sprintf("- %s\n", slug)
	}
	return message + fanoutConfirmSuffix(reached)
}

// handleSubs handles the /subs command
func (b *Bot) handleSubs(ctx context.Context, bot *Bot, update *tgbotapi.Update) error {
	mgr := b.liveSubscriptionMgr()
	if mgr == nil {
		b.sendMessage(update.Message.Chat.ID, "Live monitoring is not available.")
		return nil
	}

	chatID := update.Message.Chat.ID
	subs := mgr.GetUserSubscriptions(chatID)

	if len(subs) == 0 {
		b.sendMessage(chatID, `You don't have any active subscriptions.

Use /live <event-slug> to start monitoring an event.
Example: /live nba-lal-por-2026-01-17`)
		return nil
	}

	message := fmt.Sprintf("*Your Active Subscriptions (%d):*\n\n", len(subs))
	for i, slug := range subs {
		mode := "quiet"
		if mgr.IsTapeSubscription(chatID, slug) {
			mode = "tape"
		}
		message += fmt.Sprintf("%d. %s (%s)\n", i+1, slug, mode)
	}
	message += "\nUse /live <event-slug> tape to switch a subscription to trade prints.\nUse /stoplive <event-slug> to stop a specific subscription.\nUse /stoplive all to stop all subscriptions."

	b.sendMessage(chatID, message)
	return nil
}
