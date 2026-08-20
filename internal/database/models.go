package database

import (
	"database/sql/driver"
	"encoding/json"
	"fmt"
	"math"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
)

// JSONB is a custom type for handling JSONB columns
type JSONB map[string]interface{}

// Value implements the driver.Valuer interface
func (j JSONB) Value() (driver.Value, error) {
	if j == nil {
		return nil, nil
	}
	return json.Marshal(j)
}

// Scan implements the sql.Scanner interface
func (j *JSONB) Scan(src interface{}) error {
	if src == nil {
		*j = make(JSONB)
		return nil
	}

	switch v := src.(type) {
	case []byte:
		return json.Unmarshal(v, j)
	case string:
		return json.Unmarshal([]byte(v), j)
	default:
		return fmt.Errorf("unsupported type for JSONB: %T", src)
	}
}

// User represents a Telegram bot user
type User struct {
	TelegramID   int64  `json:"telegram_id" db:"telegram_id"`
	Username     string `json:"username" db:"username"`
	EOAAddress   string `json:"eoa_address" db:"eoa_address"`
	ProxyAddress string `json:"proxy_address" db:"proxy_address"`
	// AccountType classifies the trading account architecture
	// (legacy_proxy | safe | deposit_wallet) so the order signer can pick the
	// right signature type. Empty is treated as legacy_proxy by the repository.
	AccountType  string    `json:"account_type" db:"account_type"`
	EncryptedKey string    `json:"-" db:"encrypted_key"` // Never expose in JSON
	Settings     JSONB     `json:"settings" db:"settings"`
	IsActive     bool      `json:"is_active" db:"is_active"`
	CreatedAt    time.Time `json:"created_at" db:"created_at"`
	UpdatedAt    time.Time `json:"updated_at" db:"updated_at"`
}

// LiveSubscription is the durable record of one Live Watch (ADR 0008): a
// per-user subscription to an event, owned by the session's Telegram identity.
// One row per (ChatID, EventSlug). A Live Watch is user intent, not a soft
// rail — it is persisted so a restart re-registers and re-resolves it, whereas
// snipe caps and episode latches stay in-memory (bounds on harm, self-healing).
// The registry in internal/live is the runtime view; this is the record.
type LiveSubscription struct {
	ChatID    int64  `json:"chat_id" db:"chat_id"`
	EventSlug string `json:"event_slug" db:"event_slug"`
	// Tape opts the watch into the batched Telegram trade feed. A quiet watch
	// (false, the default) still arms the snipe watch and the web tape.
	Tape      bool      `json:"tape" db:"tape"`
	CreatedAt time.Time `json:"created_at" db:"created_at"`
}

// SLTPArm represents an armed take-profit / stop-loss for a user's position on a token.
// v1 uses fixed presets: TP fires at avg_price*2.0 selling 50% of remaining;
// SL fires at avg_price*0.70 selling 100% of remaining.
// avg_price and shares_at_arm are snapshotted at arm time so threshold evaluation is
// deterministic and independent of later Data API drift.
type SLTPArm struct {
	ID          int     `json:"id" db:"id"`
	TelegramID  int64   `json:"telegram_id" db:"telegram_id"`
	TokenID     string  `json:"token_id" db:"token_id"`
	ConditionID string  `json:"condition_id" db:"condition_id"`
	MarketID    *string `json:"market_id" db:"market_id"`
	Outcome     Outcome `json:"outcome" db:"outcome"`
	AvgPrice    float64 `json:"avg_price" db:"avg_price"`
	SharesAtArm float64 `json:"shares_at_arm" db:"shares_at_arm"`
	// HighWaterMark is the highest best-bid observed since arm/re-arm, seeded
	// to AvgPrice. The trailing SL is dormant until it reaches
	// AvgPrice*SLActivationMult; the monitor ratchets it monotonically.
	HighWaterMark float64 `json:"high_water_mark" db:"high_water_mark"`
	// TickSize is the market's minimum price increment, captured at arm time
	// from the CLOB. The TP trigger is floored to this grid so it lands on a
	// price the best bid can actually print (issue #25). Zero (legacy rows,
	// fetch failures) falls back to 0.01.
	TickSize float64 `json:"tick_size" db:"tick_size"`
	TPArmed  bool    `json:"tp_armed" db:"tp_armed"`
	SLArmed  bool    `json:"sl_armed" db:"sl_armed"`
	NegRisk  bool    `json:"neg_risk" db:"neg_risk"`
	// LotteryTicketArmed: when the ceiling-TP fires, optionally also attempt a
	// FOK BUY of the OPPOSITE token at <= LotteryMaxPrice with up to
	// LotteryMaxSpend USDC. Cheap "what if it flips" insurance.
	LotteryTicketArmed bool `json:"lottery_ticket_armed" db:"lottery_ticket_armed"`
	// LadderRungsFired is how many deep-entry exit-ladder rungs have already
	// sold (issue #81), 0..len(DeepEntryLadder). Meaningful only for a deep
	// entry (IsDeepEntry); always 0 for every other arm. Monotonic — rungs fire
	// as a contiguous prefix (a gap that crosses rung k fires 0..k in one sell),
	// so a count, not a bitmask, fully describes the state. Persisted so a
	// restart resumes mid-ladder.
	LadderRungsFired int `json:"ladder_rungs_fired" db:"ladder_rungs_fired"`
	// LadderBaseShares is the COMMON BASE the rung fractions are taken off:
	// the whole-position share count frozen at the FIRST rung fire (issue #81).
	// Zero until the first rung fires. Frozen thereafter so later blended
	// tranches never re-inflate earlier fractions (matches the study's model).
	LadderBaseShares float64   `json:"ladder_base_shares" db:"ladder_base_shares"`
	CreatedAt        time.Time `json:"created_at" db:"created_at"`
	UpdatedAt        time.Time `json:"updated_at" db:"updated_at"`
}

// TPMultiplier is the fixed v1 take-profit multiplier: trigger when bid >= avg_price * TPMultiplier.
const TPMultiplier = 2.0

// SLActivationMult gates the breakeven-trailing stop: it stays dormant until
// the high-water mark reaches avg_price * SLActivationMult. Below that the
// position has no stop — max loss is the stake. Chosen over the old fixed
// -30% stop, which realized ~-48% of basis and sold 29% of eventual winners.
const SLActivationMult = 1.20

// SLTrailMult sets the trailing distance once active:
// trigger = max(avg_price, high_water_mark * SLTrailMult).
const SLTrailMult = 0.80

// SLMaxSlip bounds SL execution: the sell is a FOK limit at
// trigger * (1 - SLMaxSlip), never a market order into a gapped book.
const SLMaxSlip = 0.10

// TPSellFraction is the fraction of current shares sold on a TP fire.
// Lowered from 0.50 on 2026-08-02: a 184-episode counterfactual over July's
// realized price paths ranked lighter banking strictly better (no-TP +$92,
// 25% +$46 vs 50%, ladders ≈ noise, heavier/earlier −$108) — the trailing
// stop already insures positions past activation, so early banking mostly
// sells winners cheap. 25% keeps a guaranteed bank against the stop's
// observed no-fill risk in gapped in-play books.
const TPSellFraction = 0.25

// CeilingTPPrice is the bid threshold at which the monitor sells ALL remaining
// shares regardless of avg_price. At this level the upside (resolve = $1.00)
// is capped at ~5%, which isn't worth the resolution-day risk.
const CeilingTPPrice = 0.95

// LadderMaxEntry is the inclusive entry ceiling for the deep-entry exit ladder
// (issue #81). An arm whose entry ≤ this replaces the single 25% @ 2× partial
// with the DeepEntryLadder rung schedule below; every other arm keeps today's
// 25% @ 2× + $0.95 ceiling. Strict ≤: entry EXACTLY 0.05 qualifies — it is the
// class-A edge of the 2026-08-20 counterfactual study (n=30 deep taps, 0
// winners, laddering monotonically better), so the boundary sits on the studied
// class, not a tick inside it.
const LadderMaxEntry = 0.05

// ladderRung is one exit-ladder rung: sell Fraction of the COMMON BASE when the
// bid reaches Multiple × entry.
type ladderRung struct {
	Multiple float64
	Fraction float64
}

// DeepEntryLadder is the ratified deep-entry rung schedule (issue #81, study
// 2026-08-20): 25% @ 2×, 20% @ 3×, 15% @ 4×, 15% @ 5×; the remaining ~25% rides
// to the $0.95 ceiling. Fractions are of a COMMON BASE frozen at the first rung
// fire — cumulative, never compounding on shrinking remainders. In class A
// (entry ≤ 0.05) this P4 policy beat current −$84.5 vs −$133.0 over the 45
// replayed August episodes: every ≥3× bounce died mid-bounce, none reached the
// ceiling, so harvesting them beats riding. Multiples are off arm entry.
var DeepEntryLadder = []ladderRung{
	{Multiple: 2.0, Fraction: 0.25},
	{Multiple: 3.0, Fraction: 0.20},
	{Multiple: 4.0, Fraction: 0.15},
	{Multiple: 5.0, Fraction: 0.15},
}

// IsDeepEntry reports whether this arm uses the deep-entry exit ladder: entry ≤
// LadderMaxEntry (issue #81). Membership is derived purely from the arm's entry
// at evaluation time — there is no stored flag — so an existing qualifying arm
// adopts the ladder with no backfill, and a non-qualifying arm is byte-identical
// to pre-#81 behavior. AvgPrice > 0 guards a zero-value arm.
func (a *SLTPArm) IsDeepEntry() bool {
	return a.AvgPrice > 0 && a.AvgPrice <= LadderMaxEntry
}

// DeepEntryRungPrice returns the tick-floored bid trigger for rung idx: entry ×
// the rung multiple, floored to the market grid exactly like TPTriggerPrice so
// it lands on a price the best bid can actually print (issue #25).
func (a *SLTPArm) DeepEntryRungPrice(idx int) float64 {
	return a.triggerAtMultiple(DeepEntryLadder[idx].Multiple)
}

// DeepEntryRungsCrossed returns how many ladder rungs the bid has reached — the
// count of rungs whose tick-floored trigger price ≤ bid. Rung prices strictly
// increase with the multiple, so this is always a contiguous prefix count
// (0..len), which is why the fired state is a count and a gap that jumps two
// rungs fires both in one combined sell.
func (a *SLTPArm) DeepEntryRungsCrossed(bid float64) int {
	n := 0
	for i := range DeepEntryLadder {
		if bid >= a.DeepEntryRungPrice(i) {
			n = i + 1
		}
	}
	return n
}

// DeepEntryLadderSoldFraction sums the fractions of the first k rungs — the
// cumulative fraction of the common base sold once k rungs have fired.
func DeepEntryLadderSoldFraction(k int) float64 {
	return DeepEntryLadderFractionBetween(0, k)
}

// DeepEntryLadderFractionBetween sums the fractions of rungs [from, to) — the
// slice of the common base a single combined fire sells when the bid crosses
// from `from` already-fired rungs up to `to` crossed rungs.
func DeepEntryLadderFractionBetween(from, to int) float64 {
	sum := 0.0
	for i := from; i < to && i < len(DeepEntryLadder); i++ {
		sum += DeepEntryLadder[i].Fraction
	}
	return sum
}

// RemainderShares returns the frozen-basis share count still covered by the
// arm's downside/ceiling exits — what an SL or ceiling fire should sell, before
// any TP-only whole-position coverage extension. It is the single place the
// "how much is left" question is answered:
//   - deep entry mid/post-ladder (rungs fired): the common base minus the
//     cumulative rung fractions already sold;
//   - post-2×-TP standard arm (tp_armed cleared): the snapshot minus the 25%
//     partial;
//   - otherwise: the full snapshot.
//
// Non-deep arms are byte-identical to the pre-#81 inline calculation.
func (a *SLTPArm) RemainderShares() float64 {
	if a.IsDeepEntry() && a.LadderRungsFired > 0 {
		return a.LadderBaseShares * (1 - DeepEntryLadderSoldFraction(a.LadderRungsFired))
	}
	if !a.TPArmed {
		return a.SharesAtArm * (1 - TPSellFraction)
	}
	return a.SharesAtArm
}

// LotteryMaxPrice is the highest ask we'll pay for a lottery-ticket BUY on the
// opposite token after a ceiling-TP fire.
const LotteryMaxPrice = 0.05

// LotteryMaxSpend is the absolute USDC cap for a single lottery-ticket BUY.
// At LotteryMaxPrice this implies a 100-share max take.
const LotteryMaxSpend = 5.0

// TPTriggerPrice returns the bid threshold for TP on this arm: avg_price ×
// TPMultiplier, capped at 0.99, then floored to the market's tick grid so the
// trigger is a price the best bid can actually reach (issue #25: entry 0.2355
// doubled to 0.471 on a 0.01-tick book, where only 0.47 or 0.48 can print —
// the effective threshold was silently 0.48). The 1e-6 epsilon absorbs float
// artifacts: an exactly-on-grid price like 0.47 divided by 0.01 can evaluate
// just below 47.0 and would otherwise floor a full tick down.
func (a *SLTPArm) TPTriggerPrice() float64 {
	return a.triggerAtMultiple(TPMultiplier)
}

// triggerAtMultiple returns entry × mult, capped at 0.99 and floored to the
// market's tick grid so it lands on a price the best bid can actually reach
// (issue #25). Shared by TPTriggerPrice (2×) and the deep-entry ladder rungs
// (2×/3×/4×/5×) so every trigger is snapped the same way.
func (a *SLTPArm) triggerAtMultiple(mult float64) float64 {
	p := a.AvgPrice * mult
	if p > 0.99 {
		p = 0.99
	}
	tick := a.TickSize
	if tick <= 0 {
		tick = 0.01
	}
	p = math.Floor(p/tick+1e-6) * tick
	// n×tick can land a hair above the float the book actually prints
	// (47×0.01 = 0.47000000000000003 > float64(0.47)), which would make the
	// monitor's bid >= trigger comparison miss by ~5e-17. Prices are
	// 6-decimal fixed point on the CLOB; snap to that precision.
	p = math.Round(p*1e6) / 1e6
	if p < tick {
		p = tick
	}
	return p
}

// SLActive reports whether the trailing stop has been activated by the
// high-water mark reaching avg_price * SLActivationMult.
func (a *SLTPArm) SLActive() bool {
	return a.HighWaterMark >= a.AvgPrice*SLActivationMult
}

// SLTriggerPrice returns the bid threshold for the trailing SL: the stop
// follows the high-water mark down by SLTrailMult but never sits below entry,
// so a once-activated stop exits at worst around breakeven.
func (a *SLTPArm) SLTriggerPrice() float64 {
	trigger := a.HighWaterMark * SLTrailMult
	if trigger < a.AvgPrice {
		return a.AvgPrice
	}
	return trigger
}

// SLFloorPrice is the lowest acceptable fill for an SL exit (the FOK limit
// price). Clamped to 0.001 because the explicit-price order path has no floor
// of its own.
func (a *SLTPArm) SLFloorPrice() float64 {
	floor := a.SLTriggerPrice() * (1 - SLMaxSlip)
	if floor < 0.001 {
		return 0.001
	}
	return floor
}

// Validate validates the SLTPArm.
func (a *SLTPArm) Validate() error {
	if a.TelegramID == 0 {
		return fmt.Errorf("telegram_id is required")
	}
	if a.TokenID == "" {
		return fmt.Errorf("token_id is required")
	}
	if a.ConditionID == "" {
		return fmt.Errorf("condition_id is required")
	}
	if a.AvgPrice <= 0 || a.AvgPrice > 1 {
		return fmt.Errorf("avg_price must be in (0, 1]")
	}
	if a.SharesAtArm <= 0 {
		return fmt.Errorf("shares_at_arm must be positive")
	}
	// Outcome is display metadata only — token_id is the canonical key for
	// SL/TP. Polymarket markets are binary at the contract level but their
	// display outcome is the user-facing name (YES/NO for prediction
	// questions, but team/candidate names for sports/esports/elections
	// like "WEIBO GAMING" or "KNICKS"). We only require it be non-empty.
	if a.Outcome == "" {
		return fmt.Errorf("outcome is required")
	}
	return nil
}

// LoginToken represents a web authentication token
type LoginToken struct {
	Token           pgtype.UUID `json:"token" db:"token"`
	Status          string      `json:"status" db:"status"` // pending, authenticated, used, expired
	TelegramID      *int64      `json:"telegram_id" db:"telegram_id"`
	WalletAddress   *string     `json:"wallet_address" db:"wallet_address"`
	ProxyAddress    *string     `json:"proxy_address" db:"proxy_address"`
	CreatedAt       time.Time   `json:"created_at" db:"created_at"`
	AuthenticatedAt *time.Time  `json:"authenticated_at" db:"authenticated_at"`
	ExpiresAt       time.Time   `json:"expires_at" db:"expires_at"`
	UsedAt          *time.Time  `json:"used_at" db:"used_at"`
}

// LoginToken status constants
const (
	LoginTokenStatusPending       = "pending"
	LoginTokenStatusAuthenticated = "authenticated"
	LoginTokenStatusUsed          = "used"
	LoginTokenStatusExpired       = "expired"
)

// Outcome is a position's outcome label. Display metadata only — the token
// ID is the canonical key. YES/NO for binary prediction questions, team or
// candidate names for sports/esports/election markets.
type Outcome string

const (
	OutcomeYes Outcome = "YES"
	OutcomeNo  Outcome = "NO"
)
