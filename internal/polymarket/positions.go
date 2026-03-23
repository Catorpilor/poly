package polymarket

import (
	"context"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/ethclient"
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
	Outcome       string  `json:"outcome"`
	ConditionID   string  `json:"condition_id"`
	Asset         string  `json:"asset"`          // token ID (YES or NO)
	OppositeAsset string  `json:"opposite_asset"` // complementary token ID
	Size          float64 `json:"size"`           // shares (human-readable)
	NegativeRisk  bool    `json:"negative_risk"`
	CurPrice      float64 `json:"cur_price"`  // 1.0 for winners, 0.0 for losers
	EstPayout     float64 `json:"est_payout"` // estimated USDC payout
}

// Market represents a Polymarket market
type Market struct {
	ID          string  `json:"id"`
	Question    string  `json:"question"`
	ConditionID string  `json:"condition_id"`
	Slug        string  `json:"slug"`
	Active      bool    `json:"active"`
	Closed      bool    `json:"closed"`
	YesPrice    float64 `json:"yes_price"`
	NoPrice     float64 `json:"no_price"`
	Volume      float64 `json:"volume"`
}

// PositionManager handles position queries
type PositionManager struct {
	client            *ethclient.Client
	conditionalTokens common.Address
	clobAPIURL        string
	dataAPIURL        string // Polymarket Data API for positions
	httpClient        *http.Client
}

// NewPositionManager creates a new position manager
func NewPositionManager(client *ethclient.Client, clobAPIURL string) *PositionManager {
	return &PositionManager{
		client:            client,
		conditionalTokens: common.HexToAddress("0x4D97DCd97eC945f40cF65F87097ACe5EA0476045"), // ConditionalTokens on Polygon
		clobAPIURL:        clobAPIURL,
		dataAPIURL:        "https://data-api.polymarket.com", // Default Polymarket Data API
		httpClient: &http.Client{
			Timeout: 10 * time.Second,
		},
	}
}

// NewPositionManagerWithDataAPI creates a position manager with custom Data API URL
func NewPositionManagerWithDataAPI(client *ethclient.Client, clobAPIURL, dataAPIURL string) *PositionManager {
	pm := NewPositionManager(client, clobAPIURL)
	if dataAPIURL != "" {
		pm.dataAPIURL = dataAPIURL
	}
	return pm
}

// GetUserPositionsFromAPI fetches positions using the Polymarket Data API
// This is the preferred method as it returns complete position data including P&L
func (pm *PositionManager) GetUserPositionsFromAPI(ctx context.Context, proxyAddress common.Address) ([]*Position, error) {
	url := fmt.Sprintf("%s/positions?user=%s", pm.dataAPIURL, strings.ToLower(proxyAddress.Hex()))

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
	url := fmt.Sprintf("%s/positions?user=%s&redeemable=true",
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

// GetUserPositions fetches all positions for a user's proxy wallet
// Primary: Uses Data API. Fallback: Uses blockchain scanning
func (pm *PositionManager) GetUserPositions(ctx context.Context, proxyAddress common.Address) ([]*Position, error) {
	// Primary: Try the Data API first (most reliable and complete)
	positions, err := pm.GetUserPositionsFromAPI(ctx, proxyAddress)
	if err == nil {
		return positions, nil
	}

	// Fallback: Use blockchain scanning if Data API fails
	return pm.getUserPositionsFromBlockchain(ctx, proxyAddress)
}

// getUserPositionsFromBlockchain fetches positions by scanning blockchain (fallback method)
func (pm *PositionManager) getUserPositionsFromBlockchain(ctx context.Context, proxyAddress common.Address) ([]*Position, error) {
	// Step 1: Get markets where user might have positions
	// We'll use transaction history or Polymarket API to find traded markets
	markets, err := pm.getUserTradedMarkets(ctx, proxyAddress)
	if err != nil {
		return nil, fmt.Errorf("failed to get user markets: %w", err)
	}

	positions := []*Position{}

	// Step 2: For each market, check if user has position tokens
	for _, market := range markets {
		// Get YES and NO token balances
		yesBalance, noBalance, err := pm.getMarketBalances(ctx, proxyAddress, market)
		if err != nil {
			continue // Skip this market if we can't get balances
		}

		// Add YES position if exists
		if yesBalance != nil && yesBalance.Cmp(big.NewInt(0)) > 0 {
			positions = append(positions, &Position{
				MarketID:     market.ID,
				MarketTitle:  market.Question,
				ConditionID:  market.ConditionID,
				Outcome:      "YES",
				Shares:       yesBalance,
				CurrentPrice: market.YesPrice,
				Value:        pm.calculateValue(yesBalance, market.YesPrice),
			})
		}

		// Add NO position if exists
		if noBalance != nil && noBalance.Cmp(big.NewInt(0)) > 0 {
			positions = append(positions, &Position{
				MarketID:     market.ID,
				MarketTitle:  market.Question,
				ConditionID:  market.ConditionID,
				Outcome:      "NO",
				Shares:       noBalance,
				CurrentPrice: market.NoPrice,
				Value:        pm.calculateValue(noBalance, market.NoPrice),
			})
		}
	}

	return positions, nil
}

// getUserTradedMarkets fetches markets where user has traded
func (pm *PositionManager) getUserTradedMarkets(ctx context.Context, proxyAddress common.Address) ([]*Market, error) {
	// Method 1: Query Polymarket API for user's traded markets
	url := fmt.Sprintf("%s/markets?trader=%s", pm.clobAPIURL, strings.ToLower(proxyAddress.Hex()))

	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, err
	}

	req.Header.Set("Accept", "application/json")

	resp, err := pm.httpClient.Do(req)
	if err != nil {
		// Fallback: Try alternative method
		return pm.getMarketsFromEvents(ctx, proxyAddress)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		// Fallback to event-based method
		return pm.getMarketsFromEvents(ctx, proxyAddress)
	}

	var markets []*Market
	if err := json.NewDecoder(resp.Body).Decode(&markets); err != nil {
		return nil, err
	}

	return markets, nil
}

// getMarketsFromEvents fetches markets from blockchain events
func (pm *PositionManager) getMarketsFromEvents(ctx context.Context, proxyAddress common.Address) ([]*Market, error) {
	// Look for Transfer events from ConditionalTokens contract
	// This indicates the user has received position tokens

	// For now, return some known active markets as fallback
	// In production, this would query actual events or use a subgraph
	return pm.getActiveMarkets(ctx)
}

// getActiveMarkets fetches currently active markets
func (pm *PositionManager) getActiveMarkets(ctx context.Context) ([]*Market, error) {
	url := fmt.Sprintf("%s/markets?active=true&limit=50", pm.clobAPIURL)

	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, err
	}

	req.Header.Set("Accept", "application/json")

	resp, err := pm.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("API returned status %d", resp.StatusCode)
	}

	var response struct {
		Markets []*Market `json:"markets"`
	}

	// First try to decode as array
	var markets []*Market
	if err := json.NewDecoder(resp.Body).Decode(&markets); err != nil {
		// If that fails, try as object with markets field
		resp.Body.Close()
		resp, err = pm.httpClient.Do(req)
		if err != nil {
			return nil, fmt.Errorf("failed to retry request: %w", err)
		}
		defer resp.Body.Close()

		if err := json.NewDecoder(resp.Body).Decode(&response); err != nil {
			return nil, fmt.Errorf("failed to decode markets: %w", err)
		}
		return response.Markets, nil
	}

	return markets, nil
}

// getMarketBalances checks user's token balances for a specific market
func (pm *PositionManager) getMarketBalances(ctx context.Context, proxyAddress common.Address, market *Market) (*big.Int, *big.Int, error) {
	// Calculate token IDs for YES and NO outcomes
	yesTokenID, noTokenID := pm.calculateTokenIDs(market.ConditionID)

	// Query balances from ConditionalTokens contract
	yesBalance, err := pm.getTokenBalance(ctx, proxyAddress, yesTokenID)
	if err != nil {
		return nil, nil, err
	}

	noBalance, err := pm.getTokenBalance(ctx, proxyAddress, noTokenID)
	if err != nil {
		return nil, nil, err
	}

	return yesBalance, noBalance, nil
}

// calculateTokenIDs calculates the ERC-1155 token IDs for YES and NO outcomes
func (pm *PositionManager) calculateTokenIDs(_ string) (*big.Int, *big.Int) {
	// In Polymarket's ConditionalTokens:
	// Token ID = keccak256(USDC_address, collectionId)
	// CollectionId = keccak256(conditionId, indexSet)
	// IndexSet: YES = 1 (0b01), NO = 2 (0b10)

	// This is a simplified version - actual calculation needs proper encoding
	// For now, we'll need to get these from the API or events

	// Placeholder - in production, calculate actual token IDs
	yesTokenID := big.NewInt(0)
	noTokenID := big.NewInt(0)

	return yesTokenID, noTokenID
}

// getTokenBalance queries the balance of a specific ERC-1155 token
func (pm *PositionManager) getTokenBalance(ctx context.Context, owner common.Address, tokenID *big.Int) (*big.Int, error) {
	// ERC-1155 balanceOf(address,uint256) method
	// Method ID: 0x00fdd58e
	methodID := common.FromHex("0x00fdd58e")

	// Encode parameters
	paddedOwner := common.LeftPadBytes(owner.Bytes(), 32)
	paddedTokenID := common.LeftPadBytes(tokenID.Bytes(), 32)

	data := append(methodID, paddedOwner...)
	data = append(data, paddedTokenID...)

	msg := ethereum.CallMsg{
		To:   &pm.conditionalTokens,
		Data: data,
	}

	result, err := pm.client.CallContract(ctx, msg, nil)
	if err != nil {
		return nil, err
	}

	if len(result) == 0 {
		return big.NewInt(0), nil
	}

	balance := new(big.Int).SetBytes(result)
	return balance, nil
}

// calculateValue calculates the USDC value of a position
func (pm *PositionManager) calculateValue(shares *big.Int, price float64) float64 {
	// Convert shares to float (considering 6 decimals for USDC-based shares)
	sharesFloat := new(big.Float).SetInt(shares)
	divisor := new(big.Float).SetFloat64(1e6) // 6 decimals
	sharesNormalized := new(big.Float).Quo(sharesFloat, divisor)

	sharesValue, _ := sharesNormalized.Float64()
	return sharesValue * price
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