package polymarket

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"sort"
	"strings"

	"github.com/ethereum/go-ethereum/common"
)

// ComboPosition is a user's holding of one Combo (Data API v2
// /v2/positions/combos). Read-only: SL/TP, Comeback Snipe and Resting Order
// Intent never apply to it (issue #118).
type ComboPosition struct {
	ComboConditionID string
	Status           string // OPEN, PARTIAL, REDEEMABLE, RESOLVED_WIN, RESOLVED_LOSS, RESOLVED_PARTIAL
	Redeemable       bool
	Shares           float64 // combo shares; each pays $1 if every leg wins
	SharesE6         int64   // Shares in 6-decimal units (what a SELL quotes)
	Paid             float64 // gross entry cost, fees included
	LegsTotal        int
	LegsResolved     int
	Legs             []ComboPositionLeg // in leg order
}

// ComboPositionLeg is one leg of a held Combo.
type ComboPositionLeg struct {
	PositionID   string // leg position id (RFQ leg_position_ids)
	Question     string
	OutcomeLabel string
	Status       string // OPEN, RESOLVED_WIN, RESOLVED_LOSS
	Price        float64
}

type comboPositionsPage struct {
	Data []struct {
		ComboConditionID string  `json:"combo_condition_id"`
		Status           string  `json:"status"`
		Redeemable       bool    `json:"redeemable"`
		CurrentSize      float64 `json:"current_size"`
		GrossCost        float64 `json:"gross_entry_cost_usdc"`
		LegsTotal        int     `json:"legs_total"`
		LegsResolved     int     `json:"legs_resolved"`
		Legs             []struct {
			LegIndex     int     `json:"leg_index"`
			PositionID   string  `json:"leg_position_id"`
			LegStatus    string  `json:"leg_status"`
			OutcomeLabel string  `json:"leg_outcome_label"`
			CurrentPrice float64 `json:"leg_current_price"`
			Market       struct {
				Question string `json:"question"`
			} `json:"market"`
		} `json:"legs"`
	} `json:"data"`
	Pagination struct {
		NextCursor *string `json:"next_cursor"`
	} `json:"pagination"`
}

// comboHeld reports whether a combo position is still worth listing: live,
// partly resolved, or won and not yet claimed.
func comboHeld(status string, redeemable bool) bool {
	switch status {
	case "OPEN", "PARTIAL", "REDEEMABLE":
		return true
	}
	return redeemable
}

// GetComboPositions lists the wallet's held Combos (see comboHeld), newest
// first, following cursor pagination.
func (pm *PositionManager) GetComboPositions(ctx context.Context, proxyAddress common.Address) ([]*ComboPosition, error) {
	return pm.fetchComboPositions(ctx, proxyAddress, "")
}

// GetComboPosition returns one held Combo by its Combo Condition, or nil
// when the wallet no longer holds it.
func (pm *PositionManager) GetComboPosition(ctx context.Context, proxyAddress common.Address, comboConditionID string) (*ComboPosition, error) {
	combos, err := pm.fetchComboPositions(ctx, proxyAddress, comboConditionID)
	if err != nil {
		return nil, err
	}
	for _, c := range combos {
		if strings.EqualFold(c.ComboConditionID, comboConditionID) {
			return c, nil
		}
	}
	return nil, nil
}

// LegPositionIDs returns the leg position ids in leg order, for a SELL RFQ.
func (c *ComboPosition) LegPositionIDs() []string {
	ids := make([]string, len(c.Legs))
	for i, l := range c.Legs {
		ids[i] = l.PositionID
	}
	return ids
}

// ComboShareStepE6 is the RFQ gateway's sell size step: 0.01 share. It
// rounds requested SELL sizes down to it (13.798319 -> 13.79, observed live
// 2026-10-22), so anything smaller cannot be sold back.
const ComboShareStepE6 = 10_000

// Sellable reports whether a held combo can be sold back: live (not
// redeemable), at least one 0.01-share step, and no leg lost — a lost leg
// makes it worthless, and a won combo should be redeemed, not sold (#125).
func (c *ComboPosition) Sellable() bool {
	if c.Redeemable || c.SharesE6 < ComboShareStepE6 || (c.Status != "OPEN" && c.Status != "PARTIAL") {
		return false
	}
	for _, l := range c.Legs {
		if l.Status == "RESOLVED_LOSS" {
			return false
		}
	}
	return true
}

func (pm *PositionManager) fetchComboPositions(ctx context.Context, proxyAddress common.Address, condition string) ([]*ComboPosition, error) {
	base := pm.dataAPIURL + "/v2/positions/combos"
	user := strings.ToLower(proxyAddress.Hex())

	var out []*ComboPosition
	cursor := ""
	for page := 0; page < maxPositionPages; page++ {
		q := url.Values{}
		q.Set("user", user)
		if condition != "" {
			q.Set("condition", condition)
		}
		if cursor == "" {
			q.Set("limit", "100")
		} else {
			q.Set("cursor", cursor)
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"?"+q.Encode(), nil)
		if err != nil {
			return nil, fmt.Errorf("create request: %w", err)
		}
		req.Header.Set("Accept", "application/json")
		resp, err := pm.httpClient.Do(req)
		if err != nil {
			return nil, fmt.Errorf("fetch combo positions: %w", err)
		}
		var body comboPositionsPage
		var decodeErr error
		if resp.StatusCode == http.StatusOK {
			decodeErr = json.NewDecoder(resp.Body).Decode(&body)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("Data API returned status %d", resp.StatusCode)
		}
		if decodeErr != nil {
			return nil, fmt.Errorf("decode combo positions: %w", decodeErr)
		}

		for _, d := range body.Data {
			if !comboHeld(d.Status, d.Redeemable) {
				continue
			}
			sort.SliceStable(d.Legs, func(i, j int) bool { return d.Legs[i].LegIndex < d.Legs[j].LegIndex })
			c := &ComboPosition{
				ComboConditionID: d.ComboConditionID,
				Status:           d.Status, Redeemable: d.Redeemable, Shares: d.CurrentSize, Paid: d.GrossCost,
				SharesE6:  int64(math.Round(d.CurrentSize * 1e6)),
				LegsTotal: d.LegsTotal, LegsResolved: d.LegsResolved,
			}
			for _, l := range d.Legs {
				c.Legs = append(c.Legs, ComboPositionLeg{
					PositionID: l.PositionID, Question: l.Market.Question, OutcomeLabel: l.OutcomeLabel,
					Status: l.LegStatus, Price: l.CurrentPrice,
				})
			}
			out = append(out, c)
		}
		if body.Pagination.NextCursor == nil || *body.Pagination.NextCursor == "" {
			return out, nil
		}
		cursor = *body.Pagination.NextCursor
	}
	return nil, fmt.Errorf("combo positions pagination exceeded %d pages", maxPositionPages)
}

// FormatComboPositions renders the /positions combo section ("" for none).
func FormatComboPositions(combos []*ComboPosition) string {
	if len(combos) == 0 {
		return ""
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "🧩 *Combos (%d)*\n", len(combos))
	for i, c := range combos {
		fmt.Fprintf(&sb, "\n*%d. Combo · %d legs* — %s\n", i+1, len(c.Legs), comboPositionState(c))
		for _, l := range c.Legs {
			fmt.Fprintf(&sb, "   • %s — %s\n", escapeMD(truncateRunes(l.Question, 50)), comboPositionLeg(l))
		}
		fmt.Fprintf(&sb, "   • Paid $%.2f (fees incl.) · %.2f shares\n", c.Paid, c.Shares)
		if c.Redeemable {
			fmt.Fprintf(&sb, "   • 🎁 Won — $%.2f to redeem on polymarket.com\n", c.Shares)
		} else {
			fmt.Fprintf(&sb, "   • Pays $%.2f if every leg wins\n", c.Shares)
		}
	}
	return sb.String()
}

func comboPositionState(c *ComboPosition) string {
	if c.Redeemable || c.Status == "REDEEMABLE" {
		return "🏆 won"
	}
	return fmt.Sprintf("⏳ %d/%d legs resolved", c.LegsResolved, c.LegsTotal)
}

func comboPositionLeg(l ComboPositionLeg) string {
	label := "*" + escapeMD(l.OutcomeLabel) + "*"
	switch l.Status {
	case "RESOLVED_WIN":
		return label + " ✅"
	case "RESOLVED_LOSS":
		return label + " ❌"
	}
	return fmt.Sprintf("%s @ %.2f ⏳", label, l.Price)
}

// escapeMD escapes Telegram legacy-Markdown control characters.
func escapeMD(s string) string {
	return strings.NewReplacer("_", "\\_", "*", "\\*", "`", "\\`", "[", "\\[").Replace(s)
}
