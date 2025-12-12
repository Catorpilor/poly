package polymarket

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// GammaMarket represents a market from the Gamma API
type GammaMarket struct {
	ID               string  `json:"id"`
	Question         string  `json:"question"`
	ConditionID      string  `json:"conditionId"`
	Slug             string  `json:"slug"`
	EndDate          string  `json:"endDate"`
	OutcomesRaw      string  `json:"outcomes"`      // JSON string like "[\"Yes\", \"No\"]"
	OutcomePricesRaw string  `json:"outcomePrices"` // JSON string like "[\"0.55\", \"0.45\"]"
	Volume           float64 `json:"volumeNum"`
	Volume24hr       float64 `json:"volume24hr"`
	Liquidity        float64 `json:"liquidityNum"`
	Active           bool    `json:"active"`
	Closed           bool    `json:"closed"`
	BestBid          float64 `json:"bestBid"`
	BestAsk          float64 `json:"bestAsk"`
	LastTradePrice   float64 `json:"lastTradePrice"`
	OneHourChange    float64 `json:"oneHourPriceChange"`
	OneDayChange     float64 `json:"oneDayPriceChange,omitempty"`
	AcceptingOrders  bool    `json:"acceptingOrders"`
	Image            string  `json:"image"`
	Icon             string  `json:"icon"`
	Description      string  `json:"description"`
	GroupItemTitle   string  `json:"groupItemTitle"`
	NegRisk          bool    `json:"negRisk"`          // Whether this is a negative risk market
	NegRiskMarketID  string  `json:"negRiskMarketID"`  // Neg risk market ID if applicable
}

// GetOutcomes parses the outcomes JSON string into a slice
func (m *GammaMarket) GetOutcomes() []string {
	var outcomes []string
	if err := json.Unmarshal([]byte(m.OutcomesRaw), &outcomes); err != nil {
		return []string{"Yes", "No"} // Default fallback
	}
	return outcomes
}

// GetOutcomePrices parses the outcome prices JSON string into a slice
func (m *GammaMarket) GetOutcomePrices() []string {
	var prices []string
	if err := json.Unmarshal([]byte(m.OutcomePricesRaw), &prices); err != nil {
		return []string{"0", "0"}
	}
	return prices
}

// MarketClient handles market queries from the Gamma API
type MarketClient struct {
	gammaAPIURL string
	httpClient  *http.Client
}

// NewMarketClient creates a new market client
func NewMarketClient() *MarketClient {
	return &MarketClient{
		gammaAPIURL: "https://gamma-api.polymarket.com",
		httpClient: &http.Client{
			Timeout: 15 * time.Second,
		},
	}
}

// GetTrendingMarkets fetches active markets sorted by 24h volume
func (mc *MarketClient) GetTrendingMarkets(ctx context.Context, limit int) ([]*GammaMarket, error) {
	url := fmt.Sprintf("%s/markets?closed=false&active=true&limit=%d&order=volume24hr&ascending=false",
		mc.gammaAPIURL, limit)

	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}
	req.Header.Set("Accept", "application/json")

	resp, err := mc.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch markets: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("Gamma API returned status %d", resp.StatusCode)
	}

	var markets []*GammaMarket
	if err := json.NewDecoder(resp.Body).Decode(&markets); err != nil {
		return nil, fmt.Errorf("failed to decode markets: %w", err)
	}

	// Filter out markets that aren't accepting orders
	activeMarkets := make([]*GammaMarket, 0, len(markets))
	for _, m := range markets {
		if m.AcceptingOrders && !m.Closed {
			activeMarkets = append(activeMarkets, m)
		}
	}

	return activeMarkets, nil
}

// GetMarketBySlug fetches a specific market by its slug
func (mc *MarketClient) GetMarketBySlug(ctx context.Context, slug string) (*GammaMarket, error) {
	url := fmt.Sprintf("%s/markets/slug/%s", mc.gammaAPIURL, slug)

	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}
	req.Header.Set("Accept", "application/json")

	resp, err := mc.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch market: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return nil, fmt.Errorf("market not found")
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("Gamma API returned status %d", resp.StatusCode)
	}

	var market GammaMarket
	if err := json.NewDecoder(resp.Body).Decode(&market); err != nil {
		return nil, fmt.Errorf("failed to decode market: %w", err)
	}

	return &market, nil
}

// GetMarketByID fetches a specific market by its ID
func (mc *MarketClient) GetMarketByID(ctx context.Context, id string) (*GammaMarket, error) {
	url := fmt.Sprintf("%s/markets/%s", mc.gammaAPIURL, id)

	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}
	req.Header.Set("Accept", "application/json")

	resp, err := mc.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch market: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return nil, fmt.Errorf("market not found")
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("Gamma API returned status %d", resp.StatusCode)
	}

	var market GammaMarket
	if err := json.NewDecoder(resp.Body).Decode(&market); err != nil {
		return nil, fmt.Errorf("failed to decode market: %w", err)
	}

	return &market, nil
}

// GetMarketByConditionID fetches a specific market by its condition ID
// This is useful for copy trading where signals provide conditionId
func (mc *MarketClient) GetMarketByConditionID(ctx context.Context, conditionID string) (*GammaMarket, error) {
	url := fmt.Sprintf("%s/markets?condition_id=%s", mc.gammaAPIURL, conditionID)

	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}
	req.Header.Set("Accept", "application/json")

	resp, err := mc.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch market: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("Gamma API returned status %d", resp.StatusCode)
	}

	var markets []*GammaMarket
	if err := json.NewDecoder(resp.Body).Decode(&markets); err != nil {
		return nil, fmt.Errorf("failed to decode markets: %w", err)
	}

	if len(markets) == 0 {
		return nil, fmt.Errorf("market not found for conditionId: %s", conditionID)
	}

	return markets[0], nil
}

// FormatVolume formats volume for display
func FormatVolume(volume float64) string {
	if volume >= 1000000 {
		return fmt.Sprintf("$%.1fM", volume/1000000)
	}
	if volume >= 1000 {
		return fmt.Sprintf("$%.1fK", volume/1000)
	}
	return fmt.Sprintf("$%.0f", volume)
}

// FormatPrice formats a price as percentage
func FormatPrice(price float64) string {
	return fmt.Sprintf("%.0f%%", price*100)
}
