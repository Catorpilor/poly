package live

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"
)

// watchSweepInitialDelay is how long after boot the first watch expiry sweep
// runs: soon enough that a deploy purges finished watches promptly, late enough
// to stay off the startup path. Mirrors the SL/TP resolved-arm sweeper (#39).
const watchSweepInitialDelay = 2 * time.Minute

// watchSweepInterval is the cadence of watch expiry sweeps after the first.
// Event resolution lags the final whistle by minutes-to-hours, so hourly is
// plenty — matching the SL/TP sweeper's cadence.
const watchSweepInterval = 1 * time.Hour

// ClosedEventChecker resolves an event slug to its event ONLY when Gamma
// affirmatively reports the event closed (ADR 0008 phase 4). The common
// negative — an active/open event — is ErrEventNotClosed. *EventSlugResolver
// satisfies it.
type ClosedEventChecker interface {
	ClosedEventBySlug(ctx context.Context, slug string) (*EventInfo, error)
}

// SetClosedEventChecker wires the Gamma closed-event lookup used by the watch
// expiry sweep. nil (the default) keeps the sweep disabled. Must be called
// before StartWatchExpirySweep.
func (m *LiveTradeManager) SetClosedEventChecker(c ClosedEventChecker) {
	m.closedEventChecker = c
}

// StartWatchExpirySweep launches the watch expiry sweep bound to the manager's
// lifecycle: it runs on m.ctx, so Stop() (which cancels m.ctx) also stops it —
// exactly like StartEventRefresh and the SL/TP sweepLoop. No-op when no
// closed-event checker is wired. Call once, after the snipe watcher, the watch
// store, and the Telegram sender are wired.
func (m *LiveTradeManager) StartWatchExpirySweep() {
	if m.closedEventChecker == nil {
		log.Printf("LiveTradeManager: watch expiry sweep disabled (no closed-event checker)")
		return
	}
	go m.runWatchExpirySweep(m.ctx)
}

// runWatchExpirySweep runs one sweep shortly after start (so a deploy purges
// finished watches immediately), then every sweepInterval until ctx is
// cancelled. Separated from StartWatchExpirySweep so tests can drive it with
// their own context and tiny delays. Mirrors the SL/TP sweepLoop.
func (m *LiveTradeManager) runWatchExpirySweep(ctx context.Context) {
	first := time.NewTimer(m.sweepInitialDelay)
	defer first.Stop()
	select {
	case <-ctx.Done():
		return
	case <-first.C:
	}
	m.sweepExpiredWatches(ctx)

	t := time.NewTicker(m.sweepInterval)
	defer t.Stop()
	log.Printf("LiveTradeManager: watch expiry sweep started (interval=%v)", m.sweepInterval)
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			m.sweepExpiredWatches(ctx)
		}
	}
}

// sweepExpiredWatches removes a Live Watch only on positive evidence that its
// event has finished: Gamma returns the event under the closed=true filter, its
// slug matches (identity), and EVERY market is closed=true. Fail-safe by
// construction (the #40 sweeper doctrine, #33 Gamma trap): a not-closed
// response, an identity mismatch, a partial event (any still-open market), or a
// lookup error KEEPS the watch for the next sweep — only closed:true evidence
// expires one. A resolve FAILURE is never closed-evidence.
//
// For a swept event, every subscriber is removed through the same
// UnsubscribeTelegram path a manual /stoplive uses (registry unsubscribe, feed
// flush, snipe release on the last subscriber, durable store Delete), so no
// feed/snipe/DB resource leaks. Each affected user then gets ONE grouped 🧹
// notice listing every event swept for them this pass — never one message per
// watch. Returns the per-sweep counts (also emitted as one summary line).
func (m *LiveTradeManager) sweepExpiredWatches(ctx context.Context) (swept, kept, errCount int) {
	if m.closedEventChecker == nil {
		return 0, 0, 0
	}

	slugs := m.subscriptions.TelegramSubscribedEvents()

	// Grouped notice accumulator: chatID -> event titles swept for that user
	// this pass, in first-seen order.
	expiredByUser := make(map[int64][]string)
	var notifyOrder []int64

	for _, slug := range slugs {
		select {
		case <-ctx.Done():
			return swept, kept, errCount
		default:
		}

		event, err := m.closedEventChecker.ClosedEventBySlug(ctx, slug)
		switch {
		case err == nil && event != nil && allMarketsClosed(event):
			// Positive closed:true evidence for every market — expire below.
		case err == nil || errors.Is(err, ErrEventNotClosed):
			// Still active/open, not found, or returned without every market
			// closed: the common negative. Keep quietly, retry next sweep.
			kept++
			continue
		default:
			// Real lookup error (network, non-200, decode, identity mismatch):
			// keep and log. NEVER delete on error.
			log.Printf("WatchExpiry sweep: closed lookup for %s: %v (keeping watch)", slug, err)
			errCount++
			kept++
			continue
		}

		title := eventExpiryLabel(event, slug)
		// Snapshot the subscribers before removal — UnsubscribeTelegram mutates
		// the registry as we go.
		for _, chatID := range m.subscriptions.GetTelegramSubscribers(slug) {
			if !m.UnsubscribeTelegram(chatID, slug) {
				continue
			}
			if _, seen := expiredByUser[chatID]; !seen {
				notifyOrder = append(notifyOrder, chatID)
			}
			expiredByUser[chatID] = append(expiredByUser[chatID], title)
		}
		swept++
	}

	for _, chatID := range notifyOrder {
		m.sendWatchExpiredNotice(chatID, expiredByUser[chatID])
	}

	// A quiet no-op sweep (nothing closed, no errors) logs nothing.
	if swept > 0 || errCount > 0 {
		log.Printf("WatchExpiry sweep: swept=%d kept=%d errors=%d", swept, kept, errCount)
	}
	return swept, kept, errCount
}

// allMarketsClosed reports whether event has at least one market and every
// market is closed=true — the sweep's positive-evidence rule. An event with no
// markets is treated as NOT closed (nothing to prove finished), guarding
// against a stripped/partial response expiring a watch.
func allMarketsClosed(event *EventInfo) bool {
	if event == nil || len(event.Markets) == 0 {
		return false
	}
	for i := range event.Markets {
		if !event.Markets[i].Closed {
			return false
		}
	}
	return true
}

// eventExpiryLabel is the human-readable name used in the expiry notice: the
// event title when Gamma gave one, else the slug.
func eventExpiryLabel(event *EventInfo, slug string) string {
	if event != nil && event.Title != "" {
		return event.Title
	}
	return slug
}

// sendWatchExpiredNotice delivers the grouped 🧹 notice for one user. Nil-safe:
// with no Telegram sender wired (or nothing to report) it is a no-op.
func (m *LiveTradeManager) sendWatchExpiredNotice(chatID int64, titles []string) {
	if m.telegramSender == nil || len(titles) == 0 {
		return
	}
	m.telegramSender.SendMessage(chatID, watchExpiredText(titles))
}

// watchExpiredText builds the grouped watch-expiry notice body. Pure —
// table-tested. titles are every event swept for this user in one pass,
// comma-joined so the sweep never spams one message per watch. Mirrors the
// SL/TP sweep notice's 🧹 wording and "nothing was traded" reassurance.
func watchExpiredText(titles []string) string {
	return fmt.Sprintf(
		"🧹 *Watch expired: %d finished event(s)*\n\n"+
			"%s\n\n"+
			"These events have finished, so their Live Watches were removed automatically. "+
			"Nothing was traded.",
		len(titles), strings.Join(titles, ", "))
}
