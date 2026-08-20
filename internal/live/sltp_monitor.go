package live

import (
	"context"
	"errors"
	"fmt"
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
	// UpdateHWM raises high_water_mark; a no-op when the stored value is
	// already >= hwm (monotonic ratchet, guarded in SQL).
	UpdateHWM(ctx context.Context, telegramID int64, tokenID string, hwm float64) error

	// UpdateSharesAtArm raises shares_at_arm monotonically up (SQL-guarded).
	// Used by the sweep to reconcile TP-only auto-arm coverage to the whole
	// position.
	UpdateSharesAtArm(ctx context.Context, telegramID int64, tokenID string, shares float64) error
}

// HoldingReader reports a recipient's CURRENT share balance for an arm's token,
// in 6-decimal raw units. It is the coverage source for TP-only auto-arms
// (SLArmed=false): those extend their sell coverage to the whole live position
// (manual tranches included) rather than the fill snapshot, because SharesAtArm
// there is a mechanical fill number, not a deliberate user freeze. ok=false on
// any failure ⇒ callers fall back to SharesAtArm (today's behavior). The
// production impl reuses the bot's existing positions read — no new source.
type HoldingReader interface {
	CurrentSharesRaw(ctx context.Context, arm *database.SLTPArm) (int64, bool)
}

// PriceFeedSubscriber is the subset of PriceFeedManager the monitor needs.
// Implementations must also invoke registered listeners for tokenID updates.
type PriceFeedSubscriber interface {
	Subscribe(tokenID string)
	Unsubscribe(tokenID string)
	BestBid(tokenID string) (float64, bool)
	// BestAsk returns the lowest live ask for tokenID. Used by the lottery
	// flow to gate a BUY of the opposite token.
	BestAsk(tokenID string) (float64, bool)
	// BidWithFallback returns the freshest available bid: WS if the per-token
	// last update is within maxAge, else an HTTP fetch. Used by the periodic
	// tick to backstop a silent WS subscription.
	BidWithFallback(tokenID string, maxAge time.Duration) (float64, string, bool)
	OnUpdate(PriceUpdateListener)
}

// TradeExecutor performs SELL / lottery-BUY on behalf of a user.
// Implementations resolve wallet, proxy address, API credentials, and fee
// parameters from the arm.
type TradeExecutor interface {
	// ExecuteSell sells sharesRaw of arm's token. limitPrice 0 = market-style
	// order (priced from the book); nonzero = exact limit. orderType is passed
	// through to the CLOB (GTC for market-style, FOK for floored SL exits).
	ExecuteSell(ctx context.Context, arm *database.SLTPArm, sharesRaw int64,
		limitPrice float64, orderType polymarket.OrderType) *polymarket.TradeResult

	// ExecuteLotteryBuy attempts a FOK BUY of otherTokenID at price <=
	// maxPrice for at most maxSpend USDC. Used by the ceiling-TP fire path
	// when the user has lottery_ticket_armed = true. The arm is passed for
	// context (telegramID, market resolution) but otherTokenID is the actual
	// token being purchased — NOT arm.TokenID.
	ExecuteLotteryBuy(ctx context.Context, arm *database.SLTPArm,
		otherTokenID string, otherOutcome string,
		maxSpend float64, maxPrice float64) *polymarket.TradeResult

	// ResolveOtherToken returns the second CTF token for the binary market
	// containing arm.TokenID, plus its outcome name. Returns ErrMultiOutcome
	// when the market has != 2 outcomes (the lottery feature only applies to
	// binary markets).
	ResolveOtherToken(ctx context.Context, arm *database.SLTPArm) (otherTokenID, otherOutcome string, err error)
}

// ErrMultiOutcome signals that lottery-ticket BUY isn't supported for this
// market because there's no single "other side".
var ErrMultiOutcome = errors.New("multi-outcome market: no single other side")

// BookReader reports the executable sell-VWAP for a token from a FRESH order
// book read at call time. The depth-aware fire confirm (issue #80) uses this
// INSTEAD of the price feed's local book: on this deployment the market WS
// sends last_trade_price only (no book/price_change frames), so
// PriceFeedManager's book is a one-shot subscribe-time HTTP seed that
// fossilizes — confirming against it would veto genuine fires indefinitely
// against stale liquidity. This reads the same live CLOB book the sell path
// walks, at fire time.
//
// It returns the VWAP of selling `shares` best-bid-first, the total bid depth,
// and ok. ok=false — empty book, fetch error, or timeout — is the confirm's
// fail-open signal; the implementation owns a short timeout so a firing
// decision never blocks on the network.
type BookReader interface {
	SellVWAP(ctx context.Context, tokenID string, shares float64) (vwap float64, depth float64, ok bool)
}

// ClosedMarketChecker resolves a condition ID to its market only when Gamma
// reports that market closed (resolved-arm sweeper, issue #39). A market that
// is open or finished-but-unresolved yields polymarket.ErrMarketNotFound.
// *polymarket.MarketClient satisfies this interface.
type ClosedMarketChecker interface {
	GetClosedMarketByConditionID(ctx context.Context, conditionID string) (*polymarket.GammaMarket, error)
}

// Notifier sends SL/TP fire and pause notifications to users.
type Notifier interface {
	NotifySLTPFired(telegramID int64, kind string, arm *database.SLTPArm, bid float64, result *polymarket.TradeResult)
	// NotifySLExitPending is sent at most once per breach episode when the
	// floored FOK exit can't fill; the monitor keeps retrying while the
	// breach persists.
	NotifySLExitPending(telegramID int64, arm *database.SLTPArm, bid, trigger, floor float64)
	// NotifySLTPStaleSize reports that the arm's share snapshot no longer
	// matches the wallet balance (shares sold outside the bot, issue #24).
	// availableRaw == 0: the position is gone and the arm was auto-disarmed.
	// > 0: later exits are clamped to the actual balance (6-decimal raw).
	NotifySLTPStaleSize(telegramID int64, arm *database.SLTPArm, availableRaw int64)
	// NotifySLTPPaused is sent at most once per user while the pause window is
	// active, so users understand why their arms aren't firing.
	NotifySLTPPaused(telegramID int64, arm *database.SLTPArm)
	// NotifyLottery describes the outcome of a lottery-ticket attempt:
	//   - reason="filled" with non-nil result: success
	//   - reason="ask-too-high" / "multi-outcome" / etc. with detail: skipped
	//   - reason="failed" with non-nil result: order rejected by exchange
	NotifyLottery(telegramID int64, arm *database.SLTPArm, otherOutcome string,
		reason string, detail string, result *polymarket.TradeResult)
	// NotifyArmsSwept reports one sweep's cleanup for a user: the outcome
	// labels of every arm auto-disarmed because its market closed. Sent at
	// most once per user per sweep — never per-arm spam (issue #39).
	NotifyArmsSwept(telegramID int64, outcomes []string)
}

// PauseWindow returns true when the monitor must skip evaluation (e.g., V2 cutover).
type PauseWindow func(now time.Time) bool

// sltpTickInterval is the cadence of the periodic re-evaluation tick.
// Backstops a silent WS subscription where evaluate() would otherwise never
// run for a token: PONGs keep the connection-level lastMsgAt fresh even when
// no book events flow for a specific asset, so per-token staleness is invisible
// to the WS health check.
const sltpTickInterval = 20 * time.Second

// sltpFreshnessMaxAge is how recent the per-token WS update must be for the
// tick to trust the cached bid; older = HTTP fallback.
const sltpFreshnessMaxAge = 30 * time.Second

// slConfirmWindowDefault is how long the bid must stay at/below the trailing
// trigger before the SL fires. The 20s tick guarantees at least one
// re-evaluation lands inside any 30s window, so a breach is always confirmed
// or reset by live data — never by a single gapped observation.
const slConfirmWindowDefault = 30 * time.Second

// slRetryIntervalDefault is the minimum spacing between FOK exit attempts
// while a confirmed breach persists.
const slRetryIntervalDefault = 30 * time.Second

// depthConfirmCooldownDefault is the per-arm spacing between depth-aware fire
// confirm attempts after a refusal (issue #80). Short because the trigger stays
// armed and a genuinely-crossing book must be allowed to fire promptly; long
// enough that a phantom oscillating around the threshold can't spam the confirm
// (and its refused log) — nor hammer the fresh-book HTTP fetch — on every WS
// tick. It bounds the confirm/fetch rate for TP and ceiling fires, which
// otherwise retry on every price update. For SL the pre-existing slGate retry
// interval (slRetryInterval, 30s) already spaces attempts and dominates this
// cooldown, so the effective SL re-confirm cadence is that 30s, not 5s.
const depthConfirmCooldownDefault = 5 * time.Second

// sltpSweepInitialDelay is how long after Start the first closed-market sweep
// runs: soon enough that a deploy purges zombie arms immediately, late enough
// to stay off the startup path.
const sltpSweepInitialDelay = 2 * time.Minute

// sltpSweepInterval is the cadence of closed-market sweeps after the first.
// Resolution lags the game by minutes-to-hours, so hourly is plenty.
const sltpSweepInterval = 1 * time.Hour

// sltpShareCoverageTolerance is the dust threshold (in shares) below which a
// TP-only coverage gain is ignored — the CLOB truncates sizes to 2 decimals, so
// sub-0.01-share deltas can't matter and would only churn reconciliation writes.
const sltpShareCoverageTolerance = 0.01

// minSellSizeRaw is the smallest order size the CLOB can express, in 6-decimal
// raw units: sizes are truncated to 2 decimals, so anything below 0.01 shares
// can never be part of a valid order.
const minSellSizeRaw = 10_000

// minOrderValueUSD is the CLOB's minimum order value.
const minOrderValueUSD = 1.0

// shortfallGone reports whether the balance reported by a shortfall rejection
// is unsellable at price: dust below the CLOB's 0.01-share size precision, or
// under its $1 minimum order value. Clamping a retry to such a balance would
// only collect more rejections, so the position is treated as gone (issue #24
// reopened: 2-decimal size truncation leaves dust behind fractional positions,
// so availableRaw == 0 almost never happens — production saw 16922 raw).
// price is the attempt's sell price: the FOK floor for SL, the current bid for
// TP/ceiling.
func shortfallGone(availableRaw int64, price float64) bool {
	if availableRaw < minSellSizeRaw {
		return true
	}
	return float64(availableRaw)/1e6*price < minOrderValueUSD
}

// slArmState is the in-memory (restart-resettable) SL breach state for one arm
// epoch, keyed by arm.ID. A disarm→re-arm normally produces a new ID; the
// upsert path can reuse an ID, but it also resets the HWM to entry, so the
// dormant branch wipes any stale state before it could fire.
type slArmState struct {
	breachStart     time.Time // first observation of bid <= trigger this episode
	lastAttempt     time.Time // last sell submission (rate limit)
	inFlight        bool      // a sell attempt is currently running
	sold            bool      // sell filled; only the disarm retry remains
	pendingNotified bool      // "exit pending" notice sent for this episode
	// clampedSharesRaw is the wallet's actual balance reported by a
	// balance-shortfall rejection (issue #24); later attempts in this episode
	// sell min(snapshot, clamped) instead of the stale arm-time snapshot.
	clampedSharesRaw int64
	staleNotified    bool // stale-size notice sent for this episode
	// escalated: this episode's floored FOK was killed, so every further
	// attempt sells at market. Counterfactual replay of realized crashes
	// (2026-08-04, ADR 0006) showed true collapses gap through any plausible
	// floor within a minute — after one refused floor, any fill beats zero.
	// A recovery above the trigger clears the episode and the flag with it.
	escalated bool
}

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

	// tickInterval is the periodic re-evaluation cadence. Tests override it
	// before Start() to make ticker-driven tests deterministic.
	tickInterval time.Duration
	// freshnessMaxAge is the per-token WS staleness threshold used by the tick.
	freshnessMaxAge time.Duration
	// slConfirmWindow / slRetryInterval are test-overridable copies of the
	// trailing-SL timing defaults.
	slConfirmWindow time.Duration
	slRetryInterval time.Duration
	// sweepInitialDelay / sweepInterval schedule the closed-market sweep.
	// Tests override them before Start() like tickInterval.
	sweepInitialDelay time.Duration
	sweepInterval     time.Duration
	// closedChecker resolves conditions to markets Gamma reports closed.
	// nil (the default) disables the sweeper. Set before Start — read
	// unguarded, matching the SetX wiring pattern.
	closedChecker ClosedMarketChecker
	// holdings reads a user's current share balance for TP-only auto-arm
	// coverage. nil (the default) disables coverage extension — fire/sweep use
	// SharesAtArm exactly as today. Set before Start, like closedChecker.
	holdings HoldingReader
	// book is the fresh-book price source for the depth-aware fire confirm
	// (issue #80). nil (the default) disables the confirm — every fire is
	// fail-open, i.e. the pre-issue-#80 one-bid behavior. Set before Start,
	// like holdings/closedChecker.
	book BookReader

	// depthConfirmWindow is the per-arm cooldown between depth-confirm attempts
	// after a refusal. Test-overridable copy of depthConfirmCooldownDefault.
	depthConfirmWindow time.Duration

	mu            sync.Mutex
	pauseNotified map[int64]bool      // telegramID -> notified at window start
	slState       map[int]*slArmState // arm.ID -> breach/attempt state
	// depthConfirm is the per-arm.ID bookkeeping for the depth-aware fire
	// confirm: single-flight + refusal cooldown in ONE struct so a claim is
	// atomic. A map SEPARATE from slState on purpose — evaluateArm wipes slState
	// before a TP/ceiling fire (SL-debounce reset), which would otherwise clear
	// the cooldown on every tick and defeat it. Entries are pruned on terminal
	// disarm/sweep.
	depthConfirm map[int]*depthConfirmState
}

// depthConfirmState is one arm's depth-confirm slot: an in-flight flag
// (single-flight across concurrent WS+tick evals) and the last refusal time
// (cooldown anchor). Both fields are only ever read/written under m.mu.
type depthConfirmState struct {
	inFlight  bool      // a confirm attempt (holdings + book fetch) is running
	refusedAt time.Time // last refusal — the cooldown starts here
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
		ctx:                ctx,
		cancel:             cancel,
		store:              store,
		feed:               feed,
		executor:           executor,
		notifier:           notifier,
		paused:             paused,
		now:                time.Now,
		tickInterval:       sltpTickInterval,
		freshnessMaxAge:    sltpFreshnessMaxAge,
		slConfirmWindow:    slConfirmWindowDefault,
		slRetryInterval:    slRetryIntervalDefault,
		depthConfirmWindow: depthConfirmCooldownDefault,
		sweepInitialDelay:  sltpSweepInitialDelay,
		sweepInterval:      sltpSweepInterval,
		pauseNotified:      make(map[int64]bool),
		slState:            make(map[int]*slArmState),
		depthConfirm:       make(map[int]*depthConfirmState),
	}
}

// SetClosedMarketChecker wires the Gamma closed-market lookup used by the
// resolved-arm sweeper (issue #39). nil keeps the sweeper disabled. Must be
// called before Start.
func (m *SLTPMonitor) SetClosedMarketChecker(c ClosedMarketChecker) {
	m.closedChecker = c
}

// SetHoldingReader wires the current-holding source for TP-only auto-arm
// coverage. nil keeps coverage extension disabled (fire/sweep use SharesAtArm).
// Must be called before Start.
func (m *SLTPMonitor) SetHoldingReader(h HoldingReader) {
	m.holdings = h
}

// SetBookReader wires the fresh-book price source used by the depth-aware fire
// confirm (issue #80). nil keeps the confirm disabled (every fire is fail-open,
// pre-#80 one-bid behavior). Must be called before Start.
func (m *SLTPMonitor) SetBookReader(r BookReader) {
	m.book = r
}

// tpOnlyCurrentShares returns the recipient's current holding (in shares) for a
// TP-only auto-arm, or (0,false) for a manual TP+SL arm, an unset reader, or any
// read failure. Callers take max(this, SharesAtArm-derived): coverage only ever
// extends up, and the reactive shortfall clamp still caps a Data-API-lag
// over-request. Manual (SLArmed=true) arms keep their deliberate frozen snapshot.
func (m *SLTPMonitor) tpOnlyCurrentShares(arm *database.SLTPArm) (float64, bool) {
	if arm.SLArmed || m.holdings == nil {
		return 0, false
	}
	raw, ok := m.holdings.CurrentSharesRaw(m.ctx, arm)
	if !ok {
		return 0, false
	}
	return float64(raw) / 1e6, true
}

// Start seeds WS subscriptions from the DB, registers the update handler, and
// launches the periodic re-evaluation tick.
func (m *SLTPMonitor) Start() error {
	tokenIDs, err := m.store.ListArmedTokenIDs(m.ctx)
	if err != nil {
		return err
	}
	for _, id := range tokenIDs {
		m.feed.Subscribe(id)
	}
	m.feed.OnUpdate(m.handleUpdate)
	go m.tickLoop()
	if m.closedChecker != nil {
		go m.sweepLoop()
	}
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

// evaluate is the WS-driven path: resolves the bid from the local WS book and
// hands off to evaluateToken.
func (m *SLTPMonitor) evaluate(tokenID string) {
	bid, ok := m.feed.BestBid(tokenID)
	m.evaluateToken(tokenID, bid, ok, "ws")
}

// evaluateToken loads armed rows for tokenID and checks each against the
// supplied bid. Skipped if the pause window is active. The source argument is
// recorded in logs only ("ws", "tick:ws", "tick:http") so we can tell which
// path made the decision.
func (m *SLTPMonitor) evaluateToken(tokenID string, bid float64, ok bool, source string) {
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

	log.Printf("SLTPMonitor eval: token=%s bid=%.4f ok=%v src=%s arms=%d",
		tokenID, bid, ok, source, len(arms))

	if !ok || bid <= 0 {
		return
	}

	for _, arm := range arms {
		m.evaluateArm(arm, bid)
	}
}

// tickLoop runs the periodic re-evaluation. Exits when the monitor's context
// is cancelled (Stop()).
func (m *SLTPMonitor) tickLoop() {
	t := time.NewTicker(m.tickInterval)
	defer t.Stop()
	for {
		select {
		case <-m.ctx.Done():
			return
		case <-t.C:
			m.tickEvaluateAll()
		}
	}
}

// tickEvaluateAll fans out one goroutine per armed token so a slow HTTP fetch
// for one token doesn't stall others.
func (m *SLTPMonitor) tickEvaluateAll() {
	tokenIDs, err := m.store.ListArmedTokenIDs(m.ctx)
	if err != nil {
		log.Printf("SLTPMonitor tick: list armed: %v", err)
		return
	}
	for _, id := range tokenIDs {
		id := id
		go func() {
			bid, src, ok := m.feed.BidWithFallback(id, m.freshnessMaxAge)
			m.evaluateToken(id, bid, ok, "tick:"+src)
		}()
	}
}

// sweepLoop schedules closed-market sweeps: one shortly after start (so a
// deploy purges zombie arms immediately), then every sweepInterval. Exits
// when the monitor's context is cancelled (Stop()).
func (m *SLTPMonitor) sweepLoop() {
	first := time.NewTimer(m.sweepInitialDelay)
	defer first.Stop()
	select {
	case <-m.ctx.Done():
		return
	case <-first.C:
	}
	m.sweepClosedArms()

	t := time.NewTicker(m.sweepInterval)
	defer t.Stop()
	for {
		select {
		case <-m.ctx.Done():
			return
		case <-t.C:
			m.sweepClosedArms()
		}
	}
}

// sweepClosedArms auto-disarms every arm whose market Gamma reports closed
// (issue #39): finished markets never fire and only spam book-fetch 404s.
// Fail-safe by construction — only positive, identity-matched closed:true
// evidence sweeps; empty responses (open/unresolved) and lookup errors keep
// the arm for the next sweep. Each affected user gets ONE notification per
// sweep, grouping all their swept outcomes.
func (m *SLTPMonitor) sweepClosedArms() {
	arms, errCount := m.snapshotArmedRows()

	// One Gamma lookup per condition, however many arms share it.
	closedByCondition := make(map[string]bool)
	for _, arm := range arms {
		id := arm.ConditionID
		if id == "" {
			continue // no lookup key: kept via the zero value below
		}
		if _, seen := closedByCondition[id]; seen {
			continue
		}
		market, err := m.closedChecker.GetClosedMarketByConditionID(m.ctx, id)
		switch {
		case err == nil && market != nil && market.Closed:
			closedByCondition[id] = true
		case err == nil || errors.Is(err, polymarket.ErrMarketNotFound):
			// Open or finished-but-unresolved (the common negative), or a
			// market that came back without closed=true: keep quietly.
			closedByCondition[id] = false
		default:
			log.Printf("SLTPMonitor sweep: closed lookup for %s: %v", id, err)
			errCount++
			closedByCondition[id] = false
		}
	}

	swept, kept := 0, 0
	sweptOutcomes := make(map[int64][]string)
	var notifyOrder []int64
	for _, arm := range arms {
		if !closedByCondition[arm.ConditionID] {
			kept++
			m.reconcileCoverage(arm)
			continue
		}
		// ErrSLTPArmNotFound is tolerated: the row vanished under us (manual
		// disarm, SL fire) — the goal state is already reached.
		if err := m.store.Disarm(m.ctx, arm.TelegramID, arm.TokenID); err != nil &&
			!errors.Is(err, repositories.ErrSLTPArmNotFound) {
			log.Printf("SLTPMonitor sweep: disarm %d/%s: %v (kept, next sweep retries)",
				arm.TelegramID, arm.TokenID, err)
			errCount++
			kept++
			continue
		}
		m.clearSLState(arm.ID)
		m.clearDepthConfirm(arm.ID)
		m.unsubscribeIfLast(arm.TokenID)
		if _, seen := sweptOutcomes[arm.TelegramID]; !seen {
			notifyOrder = append(notifyOrder, arm.TelegramID)
		}
		sweptOutcomes[arm.TelegramID] = append(sweptOutcomes[arm.TelegramID], string(arm.Outcome))
		swept++
	}

	for _, telegramID := range notifyOrder {
		m.notifier.NotifyArmsSwept(telegramID, sweptOutcomes[telegramID])
	}

	// A quiet no-op sweep (everything open, no errors) logs nothing.
	if swept > 0 || errCount > 0 {
		log.Printf("SLTPMonitor sweep: swept=%d kept=%d errors=%d", swept, kept, errCount)
	}
}

// reconcileCoverage keeps a TP-only auto-arm's SharesAtArm in step with the
// whole position: if the wallet now holds more than the fill snapshot (manual
// tranches added after the auto-arm), persist SharesAtArm upward so the list
// shows honest coverage and a fire before the next holdings read still sizes
// close. Upward only; AvgPrice/HWM/flags are never touched. Manual TP+SL arms
// keep their deliberate frozen snapshot (tpOnlyCurrentShares returns false for
// them). A dust-sized gain is ignored so the sweep doesn't churn writes.
func (m *SLTPMonitor) reconcileCoverage(arm *database.SLTPArm) {
	cur, ok := m.tpOnlyCurrentShares(arm)
	if !ok || cur <= arm.SharesAtArm+sltpShareCoverageTolerance {
		return
	}
	if err := m.store.UpdateSharesAtArm(m.ctx, arm.TelegramID, arm.TokenID, cur); err != nil {
		log.Printf("SLTPMonitor sweep: reconcile shares for %d/%s: %v", arm.TelegramID, arm.TokenID, err)
	}
}

// snapshotArmedRows loads every armed row through the same listing Start
// seeds subscriptions from: armed token IDs first, then the rows per token.
// Returns the rows plus how many listing calls failed (those tokens are
// simply absent — their arms are kept until a later sweep sees them).
func (m *SLTPMonitor) snapshotArmedRows() ([]*database.SLTPArm, int) {
	tokenIDs, err := m.store.ListArmedTokenIDs(m.ctx)
	if err != nil {
		log.Printf("SLTPMonitor sweep: list armed tokens: %v", err)
		return nil, 1
	}
	var arms []*database.SLTPArm
	errCount := 0
	for _, id := range tokenIDs {
		rows, err := m.store.ListArmedByToken(m.ctx, id)
		if err != nil {
			log.Printf("SLTPMonitor sweep: list armed for %s: %v", id, err)
			errCount++
			continue
		}
		arms = append(arms, rows...)
	}
	return arms, errCount
}

// evaluateArm ratchets the high-water mark, then checks ceiling-TP, 2× TP, and
// SL. At most one fires per call. Ceiling check goes first so it supersedes the
// 50%-sell standard TP for arms where both thresholds are close (e.g.,
// avg_price ≈ 0.475 makes 2× ≈ 0.95 = ceiling).
func (m *SLTPMonitor) evaluateArm(arm *database.SLTPArm, bid float64) {
	// An SL sell already filled but the disarm errored: the ONLY valid action
	// is retrying the disarm. Guarded first so a TP/ceiling branch can never
	// sell shares that are already gone.
	if m.isSLSold(arm.ID) {
		m.retrySLDisarm(arm)
		return
	}
	m.ratchetHWM(arm, bid)
	if bid >= database.CeilingTPPrice {
		m.clearSLState(arm.ID)
		m.fireCeilingTP(arm, bid)
		return
	}
	if arm.TPArmed && bid >= arm.TPTriggerPrice() {
		m.clearSLState(arm.ID)
		m.fireTP(arm, bid)
		return
	}
	if arm.SLArmed {
		m.evaluateSL(arm, bid)
	}
}

// ratchetHWM raises the arm's high-water mark to bid when bid is a new high.
// The DB write is monotonic (WHERE high_water_mark < $n); the in-memory field
// is updated regardless so this evaluation stays self-consistent even if the
// write raced or failed. A new high can never itself be an SL breach
// (trigger = max(avg, HWM*trail) < bid when bid > HWM), so ratchet-then-check
// in one pass is safe.
func (m *SLTPMonitor) ratchetHWM(arm *database.SLTPArm, bid float64) {
	if bid <= arm.HighWaterMark {
		return
	}
	if err := m.store.UpdateHWM(m.ctx, arm.TelegramID, arm.TokenID, bid); err != nil {
		log.Printf("SLTPMonitor: update hwm for %d/%s: %v", arm.TelegramID, arm.TokenID, err)
	}
	arm.HighWaterMark = bid
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

// confirmFire is the depth-aware fire decision (issue #80). Every SL/TP/ceiling
// fire triggers on a single best-bid print; the caller has ALREADY claimed the
// depth-confirm slot (claimDepthConfirm) and computed the EXACT `shares` that
// would sell. This reads a fresh executable VWAP from the live book (BookReader,
// an HTTP fetch — NOT the fossilized WS seed) and returns true to fire / false
// to refuse, releasing the slot (fire / fail-open) or arming the cooldown
// (refusal) before it returns:
//
//	TP / ceiling: fire iff VWAP >= threshold  (the print had real depth behind it)
//	SL:           fire iff VWAP <  stop        (the collapse is real, not a phantom low)
//
// Strict comparison, no tolerance. Fail-open by construction: no book reader
// wired, or a fresh read yielding ok=false (empty book / fetch error / timeout),
// fires exactly as before the check — the confirm is a guard, never a
// dependency. A refused fire re-arms (the caller must not have consumed the
// trigger yet) and is retried after the per-arm cooldown; the depth-refused log
// is emitted once per allowed attempt.
//
// Partial depth (book can't cover `shares`): the VWAP is then an upper bound on
// the true full-size VWAP (best bids fill first).
//   - TP/ceiling: an upper bound below the threshold still proves the target is
//     unreachable, so the refusal stands.
//   - SL: an upper bound at/above the stop does NOT prove the book clears the
//     stop, so a partial fill fails open and the stop fires — the safe direction.
func (m *SLTPMonitor) confirmFire(arm *database.SLTPArm, kind string, fireBid, threshold, shares float64) bool {
	if m.book == nil {
		m.clearDepthConfirm(arm.ID)
		return true // confirm disabled — fire as before
	}
	vwap, depth, ok := m.book.SellVWAP(m.ctx, arm.TokenID, shares)
	if !ok {
		m.clearDepthConfirm(arm.ID)
		return true // no usable fresh book — fail-open
	}

	var refuse bool
	if kind == "SL" {
		// Block the stop only with positive evidence the book is healthy: a
		// full-size fill whose VWAP still clears the stop.
		refuse = depth >= shares && vwap >= threshold
	} else {
		refuse = vwap < threshold
	}
	if !refuse {
		m.clearDepthConfirm(arm.ID)
		return true
	}

	m.refuseDepthConfirm(arm.ID, m.now())
	log.Printf("SLTPMonitor: depth-refused kind=%s user=%d token=%s fireBid=%.4f execVWAP=%.4f size=%.2f",
		kind, arm.TelegramID, arm.TokenID, fireBid, vwap, shares)
	return false
}

// claimDepthConfirm atomically acquires the single per-arm depth-confirm slot,
// so the caller may proceed to the (expensive) holdings read and fresh book
// fetch. It returns false — bail BEFORE any I/O — when a recent refusal's
// cooldown is still active OR another eval already holds the slot. This makes a
// standing refusal cheap (no Data-API / CLOB hammering while a phantom
// oscillates around the threshold) and stops concurrent WS+tick evals from
// double-fetching or double-logging: check-and-claim in one critical section.
// Per arm, not per token — one token can carry several users' arms.
func (m *SLTPMonitor) claimDepthConfirm(armID int, now time.Time) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	st := m.depthConfirm[armID]
	if st == nil {
		st = &depthConfirmState{}
		m.depthConfirm[armID] = st
	}
	if st.inFlight {
		return false
	}
	if !st.refusedAt.IsZero() && now.Sub(st.refusedAt) < m.depthConfirmWindow {
		return false
	}
	st.inFlight = true
	return true
}

// refuseDepthConfirm frees the slot AND arms the cooldown (a refusal): the next
// claim is suppressed for depthConfirmWindow.
func (m *SLTPMonitor) refuseDepthConfirm(armID int, now time.Time) {
	m.mu.Lock()
	st := m.depthConfirm[armID]
	if st == nil {
		st = &depthConfirmState{}
		m.depthConfirm[armID] = st
	}
	st.inFlight = false
	st.refusedAt = now
	m.mu.Unlock()
}

// clearDepthConfirm drops an arm's depth-confirm state: the fire proceeded /
// failed open / had nothing to sell, or the arm was disarmed/swept (F5). A no-op
// when absent.
func (m *SLTPMonitor) clearDepthConfirm(armID int) {
	m.mu.Lock()
	delete(m.depthConfirm, armID)
	m.mu.Unlock()
}

// fireTP clears the tp_armed flag (double-fire guard), sells TPSellFraction of
// the snapshot shares, and notifies the user. SL stays armed on the remainder.
func (m *SLTPMonitor) fireTP(arm *database.SLTPArm, bid float64) {
	// Depth-confirm slot claimed FIRST — cheap, in-memory — so a standing
	// refusal's cooldown skips the holdings read below entirely (F2) and
	// concurrent evals can't both reach the book fetch (F3).
	if !m.claimDepthConfirm(arm.ID, m.now()) {
		return
	}

	// Coverage basis: SharesAtArm for a manual TP+SL arm (deliberate freeze);
	// max(snapshot, current holding) for a TP-only auto-arm so the fraction is
	// taken off the WHOLE position (manual tranches included), never just the
	// fill snapshot. The reactive shortfall clamp still caps a shrunken wallet.
	// Computed BEFORE ClearTP so the depth confirm sizes off the exact shares
	// that would sell, and a refusal leaves tp_armed intact to retry.
	basis := arm.SharesAtArm
	if cur, ok := m.tpOnlyCurrentShares(arm); ok && cur > basis {
		basis = cur
	}
	shares := basis * database.TPSellFraction
	sharesRaw := int64(shares * 1e6)
	if sharesRaw <= 0 {
		m.clearDepthConfirm(arm.ID) // release the claimed slot; nothing to sell
		return
	}

	// Depth-aware confirm (issue #80): refuse the TP if the executable VWAP of
	// selling `shares` (fresh book) is below the trigger (the print was a phantom).
	if !m.confirmFire(arm, "TP", bid, arm.TPTriggerPrice(), shares) {
		return
	}

	if err := m.store.ClearTP(m.ctx, arm.TelegramID, arm.TokenID); err != nil {
		if !errors.Is(err, repositories.ErrSLTPArmNotFound) {
			log.Printf("SLTPMonitor: clear tp for %d/%s: %v", arm.TelegramID, arm.TokenID, err)
		}
		return
	}

	log.Printf("SLTPMonitor: TP fire user=%d token=%s bid=%.4f sharesRaw=%d",
		arm.TelegramID, arm.TokenID, bid, sharesRaw)
	result := m.executor.ExecuteSell(m.ctx, arm, sharesRaw, 0, polymarket.OrderTypeGTC)
	result, handled := m.retryTPShortfall("TP", arm, sharesRaw, bid, result)
	if handled {
		return
	}
	m.notifier.NotifySLTPFired(arm.TelegramID, "TP", arm, bid, result)
}

// retryTPShortfall handles a balance-shortfall rejection on a TP or
// ceiling-TP sell (issue #24). An unsellable balance at bid (dust or below
// the CLOB's $1 minimum — shortfallGone) means the position was closed
// outside the bot: fully disarm, tell the user, and report handled=true so
// the caller skips its fired notice. Sellable → one immediate retry clamped
// to the wallet's actual balance; the retry's result replaces the original.
func (m *SLTPMonitor) retryTPShortfall(kind string, arm *database.SLTPArm, intendedRaw int64,
	bid float64, result *polymarket.TradeResult) (*polymarket.TradeResult, bool) {
	if result == nil || result.Success || !result.InsufficientBalance {
		return result, false
	}
	if shortfallGone(result.AvailableSharesRaw, bid) {
		m.disarmGonePosition(arm)
		return result, true
	}
	sharesRaw := result.AvailableSharesRaw
	if intendedRaw < sharesRaw {
		sharesRaw = intendedRaw
	}
	log.Printf("SLTPMonitor: %s clamped retry user=%d token=%s sharesRaw=%d (was %d)",
		kind, arm.TelegramID, arm.TokenID, sharesRaw, intendedRaw)
	return m.executor.ExecuteSell(m.ctx, arm, sharesRaw, 0, polymarket.OrderTypeGTC), false
}

// disarmGonePosition removes an arm whose position no longer exists on-chain
// (zero-balance rejection): the user closed it outside the bot (issue #24).
// ErrSLTPArmNotFound is tolerated — the ceiling path disarms before selling.
func (m *SLTPMonitor) disarmGonePosition(arm *database.SLTPArm) {
	m.clearSLState(arm.ID)
	m.clearDepthConfirm(arm.ID)
	if err := m.store.Disarm(m.ctx, arm.TelegramID, arm.TokenID); err != nil && !errors.Is(err, repositories.ErrSLTPArmNotFound) {
		log.Printf("SLTPMonitor: disarm gone position for %d/%s: %v", arm.TelegramID, arm.TokenID, err)
	}
	m.notifier.NotifySLTPStaleSize(arm.TelegramID, arm, 0)
	m.unsubscribeIfLast(arm.TokenID)
}

// evaluateSL runs the breakeven-trailing stop for one arm. Dormant arms (HWM
// below activation) have no stop at all — the position rides and max loss is
// the stake. Once active, a breach must persist for slConfirmWindow before a
// floored FOK exit is attempted.
func (m *SLTPMonitor) evaluateSL(arm *database.SLTPArm, bid float64) {
	if !arm.SLActive() {
		// Dormant. Also covers a re-arm that reset the HWM to entry: any
		// stale breach state from the previous arm epoch is wiped here.
		m.clearSLState(arm.ID)
		return
	}
	trigger := arm.SLTriggerPrice()
	if bid > trigger {
		m.clearSLState(arm.ID) // recovery resets the confirmation debounce
		return
	}
	if !m.slGate(arm.ID, m.now()) {
		return
	}
	m.attemptSLExit(arm, bid, trigger)
}

// slGate implements the confirmation debounce plus single-flight/rate-limit
// gating for a breached, active SL. Returns true when this evaluation should
// attempt the sell (and has claimed the attempt slot).
func (m *SLTPMonitor) slGate(armID int, now time.Time) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	st := m.slState[armID]
	if st == nil {
		st = &slArmState{}
		m.slState[armID] = st
	}
	if st.breachStart.IsZero() {
		st.breachStart = now
		return false
	}
	if now.Sub(st.breachStart) < m.slConfirmWindow {
		return false
	}
	if st.inFlight {
		return false
	}
	if !st.lastAttempt.IsZero() && now.Sub(st.lastAttempt) < m.slRetryInterval {
		return false
	}
	st.inFlight = true
	st.lastAttempt = now
	return true
}

// attemptSLExit sells all remaining shares as a FOK limit at the floor —
// never a market order into a gapped book. On no-fill the arm stays armed and
// the gate retries after slRetryInterval; the arm row is deleted only after a
// successful sell.
func (m *SLTPMonitor) attemptSLExit(arm *database.SLTPArm, bid, trigger float64) {
	// If TP already fired, only half the snapshot remains; otherwise the full amount.
	remaining := arm.SharesAtArm
	if !arm.TPArmed {
		remaining = arm.SharesAtArm * (1 - database.TPSellFraction)
	}
	sharesRaw := int64(remaining * 1e6)
	if clamped := m.slClampedShares(arm.ID); clamped > 0 && clamped < sharesRaw {
		sharesRaw = clamped
	}
	if sharesRaw <= 0 {
		m.finishSLAttempt(arm.ID)
		return
	}

	// Depth-aware confirm (issue #80): the breach triggered on a single bid
	// print. Refuse the stop if the executable VWAP of the whole exit (fresh
	// book) is still at/above the stop (the low print was a phantom; the real
	// book is healthy). The atomic slot claim also serializes the fetch under
	// concurrent evals; slGate's 30s retry already single-flights SL, so this
	// arm's 5s cooldown is effectively inert here — the effective SL re-confirm
	// cadence is the slGate retry interval, not depthConfirmWindow.
	// finishSLAttempt releases the single-flight slot slGate claimed so the next
	// evaluation can retry; the trigger itself stays armed (nothing sold).
	if !m.claimDepthConfirm(arm.ID, m.now()) {
		m.finishSLAttempt(arm.ID)
		return
	}
	if !m.confirmFire(arm, "SL", bid, trigger, float64(sharesRaw)/1e6) {
		m.finishSLAttempt(arm.ID)
		return
	}

	floor := arm.SLFloorPrice()

	// Escalated episodes go straight to market: the floored FOK already had
	// its one shot this episode (ADR 0006).
	if !m.slEscalated(arm.ID) {
		log.Printf("SLTPMonitor: SL attempt user=%d token=%s bid=%.4f trigger=%.4f floor=%.4f sharesRaw=%d",
			arm.TelegramID, arm.TokenID, bid, trigger, floor, sharesRaw)
		result := m.executor.ExecuteSell(m.ctx, arm, sharesRaw, floor, polymarket.OrderTypeFOK)
		if result != nil && result.Success {
			m.completeSLExit(arm, "SL", bid, floor, sharesRaw, result)
			return
		}
		if result != nil && result.InsufficientBalance {
			m.finishSLAttempt(arm.ID)
			m.handleSLShortfall(arm, result.AvailableSharesRaw, floor)
			return
		}
		m.setSLEscalated(arm.ID)
	}

	log.Printf("SLTPMonitor: SL escalate user=%d token=%s bid=%.4f trigger=%.4f sharesRaw=%d (floor %.4f refused)",
		arm.TelegramID, arm.TokenID, bid, trigger, sharesRaw, floor)
	result := m.executor.ExecuteSell(m.ctx, arm, sharesRaw, 0, polymarket.OrderTypeGTC)
	if result == nil || !result.Success {
		m.finishSLAttempt(arm.ID)
		if result != nil && result.InsufficientBalance {
			m.handleSLShortfall(arm, result.AvailableSharesRaw, floor)
			return
		}
		m.notifySLPendingOnce(arm, bid, trigger, floor)
		return
	}
	m.completeSLExit(arm, "SL-market", bid, floor, sharesRaw, result)
}

// completeSLExit finishes a filled SL sell (floored FOK or escalated market):
// latch sold, disarm (tolerating an already-gone row), clear state, drop the
// feed sub, notify with the given kind.
func (m *SLTPMonitor) completeSLExit(arm *database.SLTPArm, kind string, bid, floor float64,
	sharesRaw int64, result *polymarket.TradeResult) {
	m.markSLSold(arm.ID)
	log.Printf("SLTPMonitor: SL fire user=%d token=%s kind=%s bid=%.4f floor=%.4f sharesRaw=%d",
		arm.TelegramID, arm.TokenID, kind, bid, floor, sharesRaw)
	if err := m.store.Disarm(m.ctx, arm.TelegramID, arm.TokenID); err != nil && !errors.Is(err, repositories.ErrSLTPArmNotFound) {
		// Keep the sold state; a later evaluation retries the disarm only.
		log.Printf("SLTPMonitor: disarm after SL sell for %d/%s: %v (will retry)",
			arm.TelegramID, arm.TokenID, err)
	} else {
		m.clearSLState(arm.ID)
		m.clearDepthConfirm(arm.ID)
		m.unsubscribeIfLast(arm.TokenID)
	}
	m.notifier.NotifySLTPFired(arm.TelegramID, kind, arm, bid, result)
}

// slEscalated reports whether this episode already burned its floored FOK.
func (m *SLTPMonitor) slEscalated(armID int) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	st := m.slState[armID]
	return st != nil && st.escalated
}

func (m *SLTPMonitor) setSLEscalated(armID int) {
	m.mu.Lock()
	if st := m.slState[armID]; st != nil {
		st.escalated = true
	}
	m.mu.Unlock()
}

// handleSLShortfall reacts to a balance-shortfall rejection of an SL exit:
// the arm-time share snapshot no longer matches the wallet (shares sold
// outside the bot, issue #24). A balance unsellable at the FOK floor (dust or
// below the CLOB's $1 minimum — shortfallGone) → the position is gone: latch
// the sold state (only the disarm remains, never another sell), disarm, and
// tell the user. Sellable → clamp every later attempt in this episode to the
// actual balance and tell the user once — INSTEAD of the misleading
// thin-book notice.
func (m *SLTPMonitor) handleSLShortfall(arm *database.SLTPArm, availableRaw int64, floor float64) {
	if shortfallGone(availableRaw, floor) {
		m.markSLSold(arm.ID)
		m.notifier.NotifySLTPStaleSize(arm.TelegramID, arm, 0)
		if err := m.store.Disarm(m.ctx, arm.TelegramID, arm.TokenID); err != nil && !errors.Is(err, repositories.ErrSLTPArmNotFound) {
			// Keep the sold state; a later evaluation retries the disarm only.
			log.Printf("SLTPMonitor: disarm gone position for %d/%s: %v (will retry)",
				arm.TelegramID, arm.TokenID, err)
			return
		}
		m.clearSLState(arm.ID)
		m.clearDepthConfirm(arm.ID)
		m.unsubscribeIfLast(arm.TokenID)
		return
	}

	m.mu.Lock()
	st := m.slState[arm.ID]
	notify := st != nil && !st.staleNotified
	if st != nil {
		st.clampedSharesRaw = availableRaw
		st.staleNotified = true
	}
	m.mu.Unlock()
	if notify {
		m.notifier.NotifySLTPStaleSize(arm.TelegramID, arm, availableRaw)
	}
}

// slClampedShares returns the episode's clamped sell size (0 = no clamp).
func (m *SLTPMonitor) slClampedShares(armID int) int64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	if st := m.slState[armID]; st != nil {
		return st.clampedSharesRaw
	}
	return 0
}

// retrySLDisarm finishes an SL exit whose sell filled but whose disarm errored.
func (m *SLTPMonitor) retrySLDisarm(arm *database.SLTPArm) {
	if err := m.store.Disarm(m.ctx, arm.TelegramID, arm.TokenID); err != nil {
		if !errors.Is(err, repositories.ErrSLTPArmNotFound) {
			log.Printf("SLTPMonitor: retry disarm for %d/%s: %v", arm.TelegramID, arm.TokenID, err)
			return // keep sold state; retry again on the next evaluation
		}
	}
	m.clearSLState(arm.ID)
	m.clearDepthConfirm(arm.ID)
	m.unsubscribeIfLast(arm.TokenID)
}

func (m *SLTPMonitor) isSLSold(armID int) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	st := m.slState[armID]
	return st != nil && st.sold
}

func (m *SLTPMonitor) clearSLState(armID int) {
	m.mu.Lock()
	delete(m.slState, armID)
	m.mu.Unlock()
}

// finishSLAttempt releases the single-flight slot after a failed attempt,
// preserving breachStart (episode continues) and pendingNotified (dedup).
func (m *SLTPMonitor) finishSLAttempt(armID int) {
	m.mu.Lock()
	if st := m.slState[armID]; st != nil {
		st.inFlight = false
	}
	m.mu.Unlock()
}

// markSLSold latches the sold flag: from here on the arm can only retry its
// disarm, never sell again.
func (m *SLTPMonitor) markSLSold(armID int) {
	m.mu.Lock()
	st := m.slState[armID]
	if st == nil {
		st = &slArmState{}
		m.slState[armID] = st
	}
	st.sold = true
	st.inFlight = false
	m.mu.Unlock()
}

// notifySLPendingOnce sends the "exit pending, book too thin" notice at most
// once per breach episode.
func (m *SLTPMonitor) notifySLPendingOnce(arm *database.SLTPArm, bid, trigger, floor float64) {
	m.mu.Lock()
	st := m.slState[arm.ID]
	send := st != nil && !st.pendingNotified
	if send {
		st.pendingNotified = true
	}
	m.mu.Unlock()
	if send {
		m.notifier.NotifySLExitPending(arm.TelegramID, arm, bid, trigger, floor)
	}
}

// unsubscribeIfLast drops the feed subscription when no armed rows remain on
// the token.
func (m *SLTPMonitor) unsubscribeIfLast(tokenID string) {
	rest, err := m.store.ListArmedByToken(m.ctx, tokenID)
	if err == nil && len(rest) == 0 {
		m.feed.Unsubscribe(tokenID)
	}
}

// fireCeilingTP sells all remaining shares when the bid reaches CeilingTPPrice.
// Behaviour mirrors fireSL (full disarm, sell remainder, drop feed sub) — the
// difference is intent: SL is a downside exit, ceiling-TP is an upside exit
// when there's no meaningful upside left to chase.
func (m *SLTPMonitor) fireCeilingTP(arm *database.SLTPArm, bid float64) {
	// Depth-confirm slot claimed FIRST (see fireTP): a standing refusal skips
	// the holdings read (F2); concurrent evals can't double-fetch (F3).
	if !m.claimDepthConfirm(arm.ID, m.now()) {
		return
	}

	// Frozen-snapshot remainder (manual TP+SL, unchanged): the whole snapshot if
	// the 2× TP hasn't fired, else the post-TP remainder.
	remaining := arm.SharesAtArm
	if !arm.TPArmed {
		remaining = arm.SharesAtArm * (1 - database.TPSellFraction)
	}
	// TP-only auto-arms sell the whole CURRENT holding. The live balance already
	// nets any earlier TP sale, so it replaces the snapshot remainder when
	// larger (manual tranches); the reactive clamp caps a Data-API-lag
	// over-request. Manual arms keep the frozen remainder above. Computed BEFORE
	// Disarm so the depth confirm sizes off the exact shares, and a refusal
	// leaves the arm armed to retry.
	if cur, ok := m.tpOnlyCurrentShares(arm); ok && cur > remaining {
		remaining = cur
	}
	sharesRaw := int64(remaining * 1e6)
	if sharesRaw <= 0 {
		m.clearDepthConfirm(arm.ID) // release the claimed slot; nothing to sell
		return
	}

	// Depth-aware confirm (issue #80): refuse the ceiling exit if the executable
	// VWAP for the whole remainder (fresh book) is below the ceiling (thin
	// top-level print).
	if !m.confirmFire(arm, "ceiling", bid, database.CeilingTPPrice, remaining) {
		return
	}

	if err := m.store.Disarm(m.ctx, arm.TelegramID, arm.TokenID); err != nil {
		if !errors.Is(err, repositories.ErrSLTPArmNotFound) {
			log.Printf("SLTPMonitor: ceiling disarm for %d/%s: %v", arm.TelegramID, arm.TokenID, err)
		}
		return
	}

	log.Printf("SLTPMonitor: TP-ceiling fire user=%d token=%s bid=%.4f sharesRaw=%d",
		arm.TelegramID, arm.TokenID, bid, sharesRaw)
	result := m.executor.ExecuteSell(m.ctx, arm, sharesRaw, 0, polymarket.OrderTypeGTC)
	result, handled := m.retryTPShortfall("TP-ceiling", arm, sharesRaw, bid, result)
	if handled {
		return
	}
	m.notifier.NotifySLTPFired(arm.TelegramID, "TP-ceiling", arm, bid, result)

	// Lottery ticket: cheap insurance on the losing side. Only attempt when
	// the SELL succeeded (otherwise the user might think we doubled down on a
	// failed exit).
	if arm.LotteryTicketArmed && result != nil && result.Success {
		m.tryLotteryBuy(arm)
	}

	m.unsubscribeIfLast(arm.TokenID)
}

// tryLotteryBuy is invoked after a successful ceiling-TP SELL. Resolves the
// opposite token, checks its ask is below LotteryMaxPrice, and attempts a FOK
// BUY for up to LotteryMaxSpend USDC. Every outcome (success, skip, failure)
// is reported via NotifyLottery — never silent.
func (m *SLTPMonitor) tryLotteryBuy(arm *database.SLTPArm) {
	otherTokenID, otherOutcome, err := m.executor.ResolveOtherToken(m.ctx, arm)
	if err != nil {
		if errors.Is(err, ErrMultiOutcome) {
			m.notifier.NotifyLottery(arm.TelegramID, arm, "",
				"multi-outcome", "market has more than 2 outcomes", nil)
			return
		}
		log.Printf("SLTPMonitor: lottery resolve other token for arm %d: %v", arm.ID, err)
		m.notifier.NotifyLottery(arm.TelegramID, arm, "",
			"resolve-failed", err.Error(), nil)
		return
	}

	ask, ok := m.feed.BestAsk(otherTokenID)
	if !ok {
		m.notifier.NotifyLottery(arm.TelegramID, arm, otherOutcome,
			"no-liquidity", "no ask available for other token", nil)
		return
	}
	if ask > database.LotteryMaxPrice {
		m.notifier.NotifyLottery(arm.TelegramID, arm, otherOutcome,
			"ask-too-high", fmt.Sprintf("best ask $%.4f > $%.2f cap", ask, database.LotteryMaxPrice), nil)
		return
	}

	log.Printf("SLTPMonitor: lottery BUY attempt user=%d arm=%d otherToken=%s ask=%.4f",
		arm.TelegramID, arm.ID, otherTokenID, ask)
	result := m.executor.ExecuteLotteryBuy(m.ctx, arm, otherTokenID, otherOutcome,
		database.LotteryMaxSpend, database.LotteryMaxPrice)
	if result != nil && result.Success {
		m.notifier.NotifyLottery(arm.TelegramID, arm, otherOutcome,
			"filled", "", result)
	} else {
		m.notifier.NotifyLottery(arm.TelegramID, arm, otherOutcome,
			"failed", lotteryErrMsg(result), result)
	}
}

func lotteryErrMsg(result *polymarket.TradeResult) string {
	if result == nil {
		return "executor returned no result"
	}
	if result.ErrorMsg != "" {
		return result.ErrorMsg
	}
	return "rejected by exchange"
}
