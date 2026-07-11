package polymarket

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/common"
)

// ComboPosition is a multi-leg Polymarket combo position held by a user.
// A combo YES pays out only if every leg resolves to its specified outcome
// (a conjunction/parlay), so a single losing leg resolves the whole combo to a loss.
type ComboPosition struct {
	ConditionID    string     // combo_condition_id
	PositionID     string     // combo_position_id (ERC-1155 token id)
	UserAddress    string     // proxy wallet that holds the combo
	Shares         float64    // shares_balance
	EntryAvgPrice  float64    // entry_avg_price_usdc
	EntryCost      float64    // entry_cost_usdc
	TotalCost      float64    // total_cost_usdc
	RealizedPayout float64    // realized_payout_usdc
	Status         string     // OPEN | RESOLVED_WIN | RESOLVED_LOSS
	FirstEntryAt   string     // ISO8601
	ResolvedAt     string     // ISO8601, empty until resolved
	LegsTotal      int        // legs_total
	LegsResolved   int        // legs_resolved
	LegsPending    int        // legs_pending
	Legs           []ComboLeg // ordered by leg_index
}

// ComboLeg is one underlying market outcome within a combo.
type ComboLeg struct {
	Index        int
	PositionID   string  // leg_position_id
	ConditionID  string  // leg_condition_id
	OutcomeIndex int     // 0 = Yes, 1 = No (per leg)
	OutcomeLabel string  // "Yes" | "No"
	Status       string  // OPEN | RESOLVED_WIN | RESOLVED_LOSS
	CurrentPrice float64 // leg_current_price
	MarketTitle  string  // market.title
	MarketSlug   string  // market.slug
	EventTitle   string  // market.event.event_title
	EndDate      string  // market.end_date
}

// comboAPIResponse mirrors the raw GET /v1/positions/combos payload. Numeric
// fields arrive as JSON strings, so they are decoded as strings and parsed.
type comboAPIResponse struct {
	Combos []struct {
		ComboConditionID  string `json:"combo_condition_id"`
		ComboPositionID   string `json:"combo_position_id"`
		UserAddress       string `json:"user_address"`
		SharesBalance     string `json:"shares_balance"`
		EntryAvgPriceUSDC string `json:"entry_avg_price_usdc"`
		EntryCostUSDC     string `json:"entry_cost_usdc"`
		TotalCostUSDC     string `json:"total_cost_usdc"`
		RealizedPayout    string `json:"realized_payout_usdc"`
		Status            string `json:"status"`
		FirstEntryAt      string `json:"first_entry_at"`
		ResolvedAt        string `json:"resolved_at"`
		LegsTotal         int    `json:"legs_total"`
		LegsResolved      int    `json:"legs_resolved"`
		LegsPending       int    `json:"legs_pending"`
		Legs              []struct {
			LegIndex        int    `json:"leg_index"`
			LegPositionID   string `json:"leg_position_id"`
			LegConditionID  string `json:"leg_condition_id"`
			LegOutcomeIndex int    `json:"leg_outcome_index"`
			LegOutcomeLabel string `json:"leg_outcome_label"`
			LegStatus       string `json:"leg_status"`
			LegCurrentPrice string `json:"leg_current_price"`
			Market          struct {
				Slug  string `json:"slug"`
				Title string `json:"title"`
				Event struct {
					EventTitle string `json:"event_title"`
				} `json:"event"`
				EndDate string `json:"end_date"`
			} `json:"market"`
		} `json:"legs"`
	} `json:"combos"`
}

// parseComboPositions decodes a raw /v1/positions/combos response body into
// domain ComboPosition values, parsing string-encoded numerics.
func parseComboPositions(body []byte) ([]*ComboPosition, error) {
	var resp comboAPIResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("failed to decode combo positions: %w", err)
	}

	combos := make([]*ComboPosition, 0, len(resp.Combos))
	for _, c := range resp.Combos {
		cp := &ComboPosition{
			ConditionID:    c.ComboConditionID,
			PositionID:     c.ComboPositionID,
			UserAddress:    c.UserAddress,
			Shares:         parseFloat(c.SharesBalance),
			EntryAvgPrice:  parseFloat(c.EntryAvgPriceUSDC),
			EntryCost:      parseFloat(c.EntryCostUSDC),
			TotalCost:      parseFloat(c.TotalCostUSDC),
			RealizedPayout: parseFloat(c.RealizedPayout),
			Status:         c.Status,
			FirstEntryAt:   c.FirstEntryAt,
			ResolvedAt:     c.ResolvedAt,
			LegsTotal:      c.LegsTotal,
			LegsResolved:   c.LegsResolved,
			LegsPending:    c.LegsPending,
		}
		for _, l := range c.Legs {
			cp.Legs = append(cp.Legs, ComboLeg{
				Index:        l.LegIndex,
				PositionID:   l.LegPositionID,
				ConditionID:  l.LegConditionID,
				OutcomeIndex: l.LegOutcomeIndex,
				OutcomeLabel: l.LegOutcomeLabel,
				Status:       l.LegStatus,
				CurrentPrice: parseFloat(l.LegCurrentPrice),
				MarketTitle:  l.Market.Title,
				MarketSlug:   l.Market.Slug,
				EventTitle:   l.Market.Event.EventTitle,
				EndDate:      l.Market.EndDate,
			})
		}
		combos = append(combos, cp)
	}
	return combos, nil
}

// parseFloat tolerantly parses a string-encoded numeric, returning 0 on error.
func parseFloat(s string) float64 {
	if s == "" {
		return 0
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0
	}
	return f
}

// CombosClient reads a user's combo positions from the Polymarket Data API.
type CombosClient struct {
	dataAPIURL string
	httpClient *http.Client
}

// NewCombosClient creates a combos reader. dataAPIURL defaults to the public
// Data API host when empty.
func NewCombosClient(dataAPIURL string) *CombosClient {
	if dataAPIURL == "" {
		dataAPIURL = "https://data-api.polymarket.com"
	}
	return &CombosClient{
		dataAPIURL: strings.TrimRight(dataAPIURL, "/"),
		httpClient: &http.Client{Timeout: 10 * time.Second},
	}
}

// GetComboPositions fetches all combo positions for a proxy wallet.
func (cc *CombosClient) GetComboPositions(ctx context.Context, proxyAddress common.Address) ([]*ComboPosition, error) {
	// limit=500 mirrors /positions: avoid the default 100-item page hiding
	// recent positions behind historical ones.
	url := fmt.Sprintf("%s/v1/positions/combos?user=%s&limit=500", cc.dataAPIURL, strings.ToLower(proxyAddress.Hex()))

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}
	req.Header.Set("Accept", "application/json")

	resp, err := cc.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch combo positions: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("combos Data API returned status %d", resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read combo positions body: %w", err)
	}
	return parseComboPositions(body)
}

// statusEmoji maps a combo/leg status to a display glyph.
func statusEmoji(status string) string {
	switch status {
	case "RESOLVED_WIN":
		return "✅"
	case "RESOLVED_LOSS":
		return "❌"
	default: // OPEN and anything pending
		return "⏳"
	}
}

// FormatComboPositions renders combo positions as a Telegram markdown summary.
func FormatComboPositions(combos []*ComboPosition) string {
	if len(combos) == 0 {
		return "🧩 *Combo Positions*\n\nNo combo positions found."
	}

	var b strings.Builder
	b.WriteString("🧩 *Combo Position")
	if len(combos) > 1 {
		b.WriteString("s")
	}
	fmt.Fprintf(&b, "* (%d)\n", len(combos))

	for i, c := range combos {
		fmt.Fprintf(&b, "\n%s *Combo #%d* — %d legs · %s\n",
			statusEmoji(c.Status), i+1, c.LegsTotal, c.Status)
		fmt.Fprintf(&b, "Shares: %.2f · Entry: $%.4f · Cost: $%.2f\n",
			c.Shares, c.EntryAvgPrice, c.TotalCost)
		if c.RealizedPayout > 0 {
			fmt.Fprintf(&b, "Payout: $%.2f\n", c.RealizedPayout)
		}
		fmt.Fprintf(&b, "Legs (%d/%d resolved):\n", c.LegsResolved, c.LegsTotal)
		for _, l := range c.Legs {
			title := truncateComboTitle(l.MarketTitle, 40)
			fmt.Fprintf(&b, "  %s %s — %s · $%.3f\n",
				statusEmoji(l.Status), title, l.OutcomeLabel, l.CurrentPrice)
		}
	}
	return b.String()
}

// truncateComboTitle shortens a market title to maxRunes, UTF-8 safe.
func truncateComboTitle(s string, maxRunes int) string {
	r := []rune(s)
	if len(r) <= maxRunes {
		return s
	}
	return string(r[:maxRunes-1]) + "…"
}
