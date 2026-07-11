package polymarket

import (
	"context"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/common"
)

// Position represents a user's position in a market
type Position struct {
	MarketID     string   `json:"market_id"`
	MarketTitle  string   `json:"market_title"`
	ConditionID  string   `json:"condition_id"`
	TokenID      string   `json:"token_id"`
	Outcome      string   `json:"outcome"` // YES or NO
	Shares       *big.Int `json:"shares"`
	AveragePrice float64  `json:"average_price,omitempty"`
	CurrentPrice float64  `json:"current_price,omitempty"`
	Value        float64  `json:"value,omitempty"`       // Current value in USDC
	PnL          float64  `json:"pnl,omitempty"`         // Profit/Loss
	PnLPercent   float64  `json:"pnl_percent,omitempty"` // P&L percentage
	NegativeRisk bool     `json:"negative_risk,omitempty"`
}

// DataAPIPosition represents the response from Polymarket Data API /positions endpoint
type DataAPIPosition struct {
	ProxyWallet        string  `json:"proxyWallet"`
	Asset              string  `json:"asset"`
	ConditionID        string  `json:"conditionId"`
	Size               float64 `json:"size"`
	AvgPrice           float64 `json:"avgPrice"`
	InitialValue       float64 `json:"initialValue"`
	CurrentValue       float64 `json:"currentValue"`
	CashPnl            float64 `json:"cashPnl"`
	PercentPnl         float64 `json:"percentPnl"`
	TotalBought        float64 `json:"totalBought"`
	RealizedPnl        float64 `json:"realizedPnl"`
	PercentRealizedPnl float64 `json:"percentRealizedPnl"`
	CurPrice           float64 `json:"curPrice"`
	Redeemable         bool    `json:"redeemable"`
	Mergeable          bool    `json:"mergeable"`
	Title              string  `json:"title"`
	Slug               string  `json:"slug"`
	Icon               string  `json:"icon"`
	EventSlug          string  `json:"eventSlug"`
	Outcome            string  `json:"outcome"`
	OutcomeIndex       int     `json:"outcomeIndex"`
	OppositeOutcome    string  `json:"oppositeOutcome"`
	OppositeAsset      string  `json:"oppositeAsset"`
	EndDate            string  `json:"endDate"`
	NegativeRisk       bool    `json:"negativeRisk"`
}

// RedeemablePositionInfo contains all data needed to display and execute a redemption.
type RedeemablePositionInfo struct {
	Title         string  `json:"title"`
	Outcome       string  `json:"outcome"`       // display label only — casing varies by API
	OutcomeIndex  int     `json:"outcome_index"` // positional identity; orders neg-risk amounts
	ConditionID   string  `json:"condition_id"`
	Asset         string  `json:"asset"`          // token ID (YES or NO)
	OppositeAsset string  `json:"opposite_asset"` // complementary token ID
	Size          float64 `json:"size"`           // shares (human-readable)
	NegativeRisk  bool    `json:"negative_risk"`
	CurPrice      float64 `json:"cur_price"`  // 1.0 for winners, 0.0 for losers
	EstPayout     float64 `json:"est_payout"` // estimated USDC payout
}

// PositionManager fetches positions from the Polymarket Data API.
type PositionManager struct {
	dataAPIURL string
	httpClient *http.Client
}

// NewPositionManager creates a new position manager
func NewPositionManager() *PositionManager {
	return &PositionManager{
		dataAPIURL: "https://data-api.polymarket.com",
		httpClient: &http.Client{
			Timeout: 10 * time.Second,
		},
	}
}

// NewPositionManagerWithDataAPI creates a position manager with custom Data API URL
func NewPositionManagerWithDataAPI(dataAPIURL string) *PositionManager {
	pm := NewPositionManager()
	if dataAPIURL != "" {
		pm.dataAPIURL = dataAPIURL
	}
	return pm
}

// GetUserPositionsFromAPI fetches positions using the Polymarket Data API
// This is the preferred method as it returns complete position data including P&L
func (pm *PositionManager) GetUserPositionsFromAPI(ctx context.Context, proxyAddress common.Address) ([]*Position, error) {
	// limit=500: the API defaults to 100 and we've seen users with 160+
	// historical positions where the newest active position falls past the
	// default page boundary, leading to /positions returning "no positions"
	// despite the trade succeeding.
	url := fmt.Sprintf("%s/positions?user=%s&limit=500", pm.dataAPIURL, strings.ToLower(proxyAddress.Hex()))

	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}
	req.Header.Set("Accept", "application/json")

	resp, err := pm.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch positions from Data API: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("Data API returned status %d", resp.StatusCode)
	}

	var apiPositions []DataAPIPosition
	if err := json.NewDecoder(resp.Body).Decode(&apiPositions); err != nil {
		return nil, fmt.Errorf("failed to decode positions: %w", err)
	}

	// Convert API positions to internal Position type
	// Filter out closed/resolved markets (curPrice == 0)
	positions := make([]*Position, 0, len(apiPositions))
	for _, ap := range apiPositions {
		// Skip positions in closed/resolved markets
		if ap.CurPrice <= 0 {
			continue
		}

		// Convert size (float64) to shares (*big.Int) - size is in tokens with 6 decimals
		shares := convertSizeToShares(ap.Size)

		positions = append(positions, &Position{
			MarketID:     ap.ConditionID,
			MarketTitle:  ap.Title,
			ConditionID:  ap.ConditionID,
			TokenID:      ap.Asset,
			Outcome:      ap.Outcome,
			Shares:       shares,
			AveragePrice: ap.AvgPrice,
			CurrentPrice: ap.CurPrice,
			Value:        ap.CurrentValue,
			PnL:          ap.CashPnl,
			PnLPercent:   ap.PercentPnl,
			NegativeRisk: ap.NegativeRisk,
		})
	}

	return positions, nil
}

// GetRedeemablePositions fetches positions with redeemable=true from the Data API.
// Only returns positions with a positive payout (winning side). Losing positions are
// excluded from display but still get burned automatically by the CTF contract when
// the winning side is redeemed.
func (pm *PositionManager) GetRedeemablePositions(ctx context.Context, proxyAddress common.Address) ([]*RedeemablePositionInfo, error) {
	url := fmt.Sprintf("%s/positions?user=%s&redeemable=true&limit=500",
		pm.dataAPIURL, strings.ToLower(proxyAddress.Hex()))

	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}
	req.Header.Set("Accept", "application/json")

	resp, err := pm.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch redeemable positions: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("Data API returned status %d", resp.StatusCode)
	}

	var apiPositions []DataAPIPosition
	if err := json.NewDecoder(resp.Body).Decode(&apiPositions); err != nil {
		return nil, fmt.Errorf("failed to decode positions: %w", err)
	}

	positions := make([]*RedeemablePositionInfo, 0, len(apiPositions))
	for _, ap := range apiPositions {
		if ap.Size <= 0 || ap.CurPrice <= 0 {
			continue
		}

		positions = append(positions, &RedeemablePositionInfo{
			Title:         ap.Title,
			Outcome:       ap.Outcome,
			OutcomeIndex:  ap.OutcomeIndex,
			ConditionID:   ap.ConditionID,
			Asset:         ap.Asset,
			OppositeAsset: ap.OppositeAsset,
			Size:          ap.Size,
			NegativeRisk:  ap.NegativeRisk,
			CurPrice:      ap.CurPrice,
			EstPayout:     ap.Size * ap.CurPrice,
		})
	}

	return positions, nil
}

// convertSizeToShares converts a float64 size to *big.Int shares (6 decimals)
func convertSizeToShares(size float64) *big.Int {
	// Size is in tokens, multiply by 1e6 to get raw shares
	sharesFloat := size * 1e6
	shares := new(big.Int)
	shares.SetInt64(int64(sharesFloat))
	return shares
}

// FormatShares formats share amount for display
func FormatShares(shares *big.Int) string {
	if shares == nil {
		return "0"
	}

	// Shares have 6 decimals (same as USDC)
	divisor := new(big.Float).SetInt(new(big.Int).Exp(big.NewInt(10), big.NewInt(6), nil))
	sharesFloat := new(big.Float).SetInt(shares)
	result := new(big.Float).Quo(sharesFloat, divisor)

	return fmt.Sprintf("%.2f", result)
}