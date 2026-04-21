package live

import (
	"context"
	"errors"
	"log"
	"sync"
	"time"

	"github.com/Catorpilor/poly/internal/database"
	"github.com/Catorpilor/poly/internal/database/repositories"
	"github.com/Catorpilor/poly/internal/polymarket"
)

// sltpMonitorTickSource is how the monitor gets "now" — overridable in tests.
type sltpMonitorTickSource func() time.Time

// SLTPArmStore is the subset of SLTPArmRepository the monitor needs.
type SLTPArmStore interface {
	ListArmedTokenIDs(ctx context.Context) ([]string, error)
	ListArmedByToken(ctx context.Context, tokenID string) ([]*database.SLTPArm, error)
	ClearTP(ctx context.Context, telegramID int64, tokenID string) error
	Disarm(ctx context.Context, telegramID int64, tokenID string) error
}

// PriceFeedSubscriber is the subset of PriceFeedManager the monitor needs.
// Implementations must also invoke registered listeners for tokenID updates.
type PriceFeedSubscriber interface {
	Subscribe(tokenID string)
	Unsubscribe(tokenID string)
	BestBid(tokenID string) (float64, bool)
	OnUpdate(PriceUpdateListener)
}

// TradeExecutor performs a SELL on behalf of a user. Implementations resolve
// wallet, proxy address, API credentials, and fee parameters from the arm.
type TradeExecutor interface {
	ExecuteSell(ctx context.Context, arm *database.SLTPArm, sharesRaw int64) *polymarket.TradeResult
}

// Notifier sends SL/TP fire and pause notifications to users.
type Notifier interface {
	NotifySLTPFired(telegramID int64, kind string, arm *database.SLTPArm, bid float64, result *polymarket.TradeResult)
	// NotifySLTPPaused is sent at most once per user while the pause window is
	// active, so users understand why their arms aren't firing.
	NotifySLTPPaused(telegramID int64, arm *database.SLTPArm)
}

// PauseWindow returns true when the monitor must skip evaluation (e.g., V2 cutover).
type PauseWindow func(now time.Time) bool

// SLTPMonitor evaluates armed TP/SL thresholds on each price update and fires
// SELL orders when thresholds are crossed. Safe to call Start once per process.
type SLTPMonitor struct {
	ctx      context.Context
	cancel   context.CancelFunc
	store    SLTPArmStore
	feed     PriceFeedSubscriber
	executor TradeExecutor
	notifier Notifier
	paused   PauseWindow
	now      sltpMonitorTickSource

	mu           sync.Mutex
	pauseNotified map[int64]bool // telegramID -> notified at window start
}

// NewSLTPMonitor builds the monitor. paused may be nil (no pause window).
func NewSLTPMonitor(
	store SLTPArmStore,
	feed PriceFeedSubscriber,
	executor TradeExecutor,
	notifier Notifier,
	paused PauseWindow,
) *SLTPMonitor {
	ctx, cancel := context.WithCancel(context.Background())
	return &SLTPMonitor{
		ctx:           ctx,
		cancel:        cancel,
		store:         store,
		feed:          feed,
		executor:      executor,
		notifier:      notifier,
		paused:        paused,
		now:           time.Now,
		pauseNotified: make(map[int64]bool),
	}
}

// Start seeds WS subscriptions from the DB and registers the update handler.
func (m *SLTPMonitor) Start() error {
	tokenIDs, err := m.store.ListArmedTokenIDs(m.ctx)
	if err != nil {
		return err
	}
	for _, id := range tokenIDs {
		m.feed.Subscribe(id)
	}
	m.feed.OnUpdate(m.handleUpdate)
	log.Printf("SLTPMonitor: Started with %d armed token(s)", len(tokenIDs))
	return nil
}

// Stop cancels the monitor context, which in turn stops any in-flight evaluations.
func (m *SLTPMonitor) Stop() {
	m.cancel()
}

// SubscribeFor is invoked by callers (e.g., Telegram handler) when a new arm is created
// so the price feed starts receiving updates for tokenID.
func (m *SLTPMonitor) SubscribeFor(tokenID string) {
	m.feed.Subscribe(tokenID)
}

// UnsubscribeFor is invoked by callers when an arm is removed manually.
func (m *SLTPMonitor) UnsubscribeFor(tokenID string) {
	m.feed.Unsubscribe(tokenID)
}

// handleUpdate is registered with the price feed. It dispatches evaluation to
// a background goroutine so the WS read loop never blocks on DB or sell calls.
func (m *SLTPMonitor) handleUpdate(tokenID string) {
	go m.evaluate(tokenID)
}

// evaluate loads armed rows for tokenID and checks each against the current best bid.
// Skipped if the pause window is active; users are notified at most once.
func (m *SLTPMonitor) evaluate(tokenID string) {
	if m.paused != nil && m.paused(m.now()) {
		m.notifyPauseOnce(tokenID)
		return
	}

	arms, err := m.store.ListArmedByToken(m.ctx, tokenID)
	if err != nil {
		log.Printf("SLTPMonitor: list armed for %s: %v", tokenID, err)
		return
	}
	if len(arms) == 0 {
		return
	}

	bid, ok := m.feed.BestBid(tokenID)
	if !ok || bid <= 0 {
		return
	}

	for _, arm := range arms {
		m.evaluateArm(arm, bid)
	}
}

// evaluateArm checks TP then SL. At most one fires per call.
func (m *SLTPMonitor) evaluateArm(arm *database.SLTPArm, bid float64) {
	if arm.TPArmed && bid >= arm.TPTriggerPrice() {
		m.fireTP(arm, bid)
		return
	}
	if arm.SLArmed && bid <= arm.SLTriggerPrice() {
		m.fireSL(arm, bid)
	}
}

// notifyPauseOnce sends one pause message per (user) for the lifetime of the
// monitor process. Safe to call on every update — per-user dedup via pauseNotified.
func (m *SLTPMonitor) notifyPauseOnce(tokenID string) {
	arms, err := m.store.ListArmedByToken(m.ctx, tokenID)
	if err != nil || len(arms) == 0 {
		return
	}

	m.mu.Lock()
	var pending []*database.SLTPArm
	for _, arm := range arms {
		if m.pauseNotified[arm.TelegramID] {
			continue
		}
		m.pauseNotified[arm.TelegramID] = true
		pending = append(pending, arm)
	}
	m.mu.Unlock()

	for _, arm := range pending {
		m.notifier.NotifySLTPPaused(arm.TelegramID, arm)
	}
}

// fireTP clears the tp_armed flag (double-fire guard), sells 50% of the
// snapshot shares, and notifies the user. SL stays armed on the remainder.
func (m *SLTPMonitor) fireTP(arm *database.SLTPArm, bid float64) {
	if err := m.store.ClearTP(m.ctx, arm.TelegramID, arm.TokenID); err != nil {
		if !errors.Is(err, repositories.ErrSLTPArmNotFound) {
			log.Printf("SLTPMonitor: clear tp for %d/%s: %v", arm.TelegramID, arm.TokenID, err)
		}
		return
	}

	sharesRaw := int64(arm.SharesAtArm * database.TPSellFraction * 1e6)
	if sharesRaw <= 0 {
		return
	}

	log.Printf("SLTPMonitor: TP fire user=%d token=%s bid=%.4f sharesRaw=%d",
		arm.TelegramID, arm.TokenID, bid, sharesRaw)
	result := m.executor.ExecuteSell(m.ctx, arm, sharesRaw)
	m.notifier.NotifySLTPFired(arm.TelegramID, "TP", arm, bid, result)
}

// fireSL deletes the arm row (double-fire guard), sells 100% of remaining shares,
// notifies the user, and unsubscribes from the feed if no other users remain
// armed on this token.
func (m *SLTPMonitor) fireSL(arm *database.SLTPArm, bid float64) {
	if err := m.store.Disarm(m.ctx, arm.TelegramID, arm.TokenID); err != nil {
		if !errors.Is(err, repositories.ErrSLTPArmNotFound) {
			log.Printf("SLTPMonitor: disarm for %d/%s: %v", arm.TelegramID, arm.TokenID, err)
		}
		return
	}

	// If TP already fired, only half the snapshot remains; otherwise the full amount.
	remaining := arm.SharesAtArm
	if !arm.TPArmed {
		remaining = arm.SharesAtArm * (1 - database.TPSellFraction)
	}
	sharesRaw := int64(remaining * 1e6)
	if sharesRaw <= 0 {
		return
	}

	log.Printf("SLTPMonitor: SL fire user=%d token=%s bid=%.4f sharesRaw=%d",
		arm.TelegramID, arm.TokenID, bid, sharesRaw)
	result := m.executor.ExecuteSell(m.ctx, arm, sharesRaw)
	m.notifier.NotifySLTPFired(arm.TelegramID, "SL", arm, bid, result)

	// If no other users are armed on this token, drop the feed subscription.
	rest, err := m.store.ListArmedByToken(m.ctx, arm.TokenID)
	if err == nil && len(rest) == 0 {
		m.feed.Unsubscribe(arm.TokenID)
	}
}
