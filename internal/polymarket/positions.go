package polymarket

import (
	"context"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"net/url"
	"strconv"
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

// DataAPIPosition is one row of the Polymarket Data API v2 /v2/positions
// response (snake_case). v1 /positions was retired on 2026-10-24 (#117).
type DataAPIPosition struct {
	ProxyWallet        string  `json:"proxy_wallet"`
	Asset              string  `json:"token_id"`
	ConditionID        string  `json:"condition_id"`
	Size               float64 `json:"current_size"`
	AvgPrice           float64 `json:"avg_price"`
	InitialValue       float64 `json:"entry_cost_usdc"`
	CurrentValue       float64 `json:"current_value"`
	CashPnl            float64 `json:"unrealized_pnl"`
	PercentPnl         float64 `json:"percent_pnl"`
	TotalBought        float64 `json:"total_size"`
	RealizedPnl        float64 `json:"realized_pnl"`
	PercentRealizedPnl float64 `json:"percent_realized_pnl"`
	CurPrice           float64 `json:"current_price"`
	Redeemable         bool    `json:"redeemable"`
	Mergeable          bool    `json:"mergeable"`
	Title              string  `json:"title"`
	Slug               string  `json:"slug"`
	Icon               string  `json:"icon"`
	EventSlug          string  `json:"event_slug"`
	Outcome            string  `json:"outcome"`
	OutcomeIndex       int     `json:"outcome_index"`
	OppositeOutcome    string  `json:"opposite_outcome"`
	OppositeAsset      string  `json:"opposite_token_id"`
	EndDate            string  `json:"end_date"`
	NegativeRisk       bool    `json:"negative_risk"`
}

// dataAPIPositionsPage is the v2 response envelope for /v2/positions.
type dataAPIPositionsPage struct {
	Data       []DataAPIPosition `json:"data"`
	Pagination struct {
		NextCursor *string `json:"next_cursor"`
	} `json:"pagination"`
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
	// status OPEN (v2's default) is the superset matching v1 /positions: it
	// includes settled-but-unredeemed winners. limit=500 avoids the 100-row
	// default page that once hid a fresh position (see fetchPositionsV2).
	apiPositions, err := pm.fetchPositionsV2(ctx, proxyAddress, "OPEN")
	if err != nil {
		return nil, fmt.Errorf("failed to fetch positions from Data API: %w", err)
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
	// REDEEMABLE includes losing sides (price 0), as v1 redeemable=true did;
	// the price filter below drops them.
	apiPositions, err := pm.fetchPositionsV2(ctx, proxyAddress, "REDEEMABLE")
	if err != nil {
		return nil, fmt.Errorf("failed to fetch redeemable positions: %w", err)
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

// maxPositionPages bounds cursor pagination on /v2/positions.
const maxPositionPages = 20

// positionsPageSize is the first-page size; v2 allows up to 1000.
const positionsPageSize = 500

// fetchPositionsV2 reads every page of /v2/positions for one wallet and
// status, following pagination.next_cursor until it is null. v2 rejects the
// v1 offset/redeemable params with HTTP 400, so neither is ever sent.
func (pm *PositionManager) fetchPositionsV2(ctx context.Context, proxyAddress common.Address, status string) ([]DataAPIPosition, error) {
	base := pm.dataAPIURL + "/v2/positions"
	user := strings.ToLower(proxyAddress.Hex())

	var all []DataAPIPosition
	cursor := ""
	for page := 0; page < maxPositionPages; page++ {
		q := url.Values{}
		q.Set("user", user)
		q.Set("status", status)
		if cursor == "" {
			q.Set("limit", strconv.Itoa(positionsPageSize))
		} else {
			q.Set("cursor", cursor) // the cursor carries the page size
		}

		req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"?"+q.Encode(), nil)
		if err != nil {
			return nil, fmt.Errorf("create request: %w", err)
		}
		req.Header.Set("Accept", "application/json")

		resp, err := pm.httpClient.Do(req)
		if err != nil {
			return nil, err
		}
		var body dataAPIPositionsPage
		var decodeErr error
		if resp.StatusCode == http.StatusOK {
			decodeErr = json.NewDecoder(resp.Body).Decode(&body)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("Data API returned status %d", resp.StatusCode)
		}
		if decodeErr != nil {
			return nil, fmt.Errorf("decode positions: %w", decodeErr)
		}

		all = append(all, body.Data...)
		if body.Pagination.NextCursor == nil || *body.Pagination.NextCursor == "" {
			return all, nil
		}
		cursor = *body.Pagination.NextCursor
	}
	return nil, fmt.Errorf("positions pagination exceeded %d pages", maxPositionPages)
}
