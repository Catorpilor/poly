package polymarket

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// GammaEvent represents a parent event from the Gamma API
type GammaEvent struct {
	ID    string `json:"id"`
	Slug  string `json:"slug"`
	Title string `json:"title"`
}

// FeeSchedule represents Polymarket's dynamic fee configuration for a market.
// The fee formula is: fee = C × rate × p × (1-p)
// where C = shares, rate = category-specific fee rate, p = share price.
type FeeSchedule struct {
	Rate       float64 `json:"rate"`       // Fee rate as decimal (e.g., 0.03 = 30 bps for Sports)
	Exponent   int     `json:"exponent"`   // Fee curve exponent (currently 1)
	TakerOnly  bool    `json:"takerOnly"`  // Whether fees apply only to takers
	RebateRate float64 `json:"rebateRate"` // Maker rebate rate (e.g., 0.25 = 25%)
}

// GammaMarket represents a market from the Gamma API
type GammaMarket struct {
	ID               string        `json:"id"`
	Question         string        `json:"question"`
	ConditionID      string        `json:"conditionId"`
	Slug             string        `json:"slug"`
	EndDate          string        `json:"endDate"`
	OutcomesRaw      string        `json:"outcomes"`      // JSON string like "[\"Yes\", \"No\"]"
	OutcomePricesRaw string        `json:"outcomePrices"` // JSON string like "[\"0.55\", \"0.45\"]"
	Volume           float64       `json:"volumeNum"`
	Volume24hr       float64       `json:"volume24hr"`
	Liquidity        float64       `json:"liquidityNum"`
	Active           bool          `json:"active"`
	Closed           bool          `json:"closed"`
	BestBid          float64       `json:"bestBid"`
	BestAsk          float64       `json:"bestAsk"`
	LastTradePrice   float64       `json:"lastTradePrice"`
	OneHourChange    float64       `json:"oneHourPriceChange"`
	OneDayChange     float64       `json:"oneDayPriceChange,omitempty"`
	AcceptingOrders  bool          `json:"acceptingOrders"`
	Image            string        `json:"image"`
	Icon             string        `json:"icon"`
	Description      string        `json:"description"`
	GroupItemTitle   string        `json:"groupItemTitle"`
	NegRisk          bool          `json:"negRisk"`         // Whether this is a negative risk market
	NegRiskMarketID  string        `json:"negRiskMarketID"` // Neg risk market ID if applicable
	Events           []*GammaEvent `json:"events"`          // Parent events this market belongs to
	FeeSchedule      *FeeSchedule  `json:"feeSchedule"`     // Dynamic fee config (nil = no fees / legacy)
	FeeType          string        `json:"feeType"`         // Fee category (e.g., "sports_fees_v2", "crypto_fees_v2")
}

// GetFeeRateBps returns the taker fee rate in basis points from the market's feeSchedule.
// The Gamma API rate field is in per-mille units (0.03 = 30 bps, 0.072 = 72 bps),
// so we multiply by 1000 to convert to basis points.
// Returns 0 if no feeSchedule is set (fee-free markets like Geopolitics).
func (m *GammaMarket) GetFeeRateBps() int {
	if m.FeeSchedule == nil {
		return 0
	}
	return int(m.FeeSchedule.Rate * 1000)
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

// GetEventSlug returns the parent event slug, or market slug as fallback
func (m *GammaMarket) GetEventSlug() string {
	if len(m.Events) > 0 && m.Events[0].Slug != "" {
		return m.Events[0].Slug
	}
	// Fallback to market slug if no event
	return m.Slug
}

// MarketClient handles market queries from the Gamma API
type MarketClient struct {
	gammaAPIURL string
	httpClient  *http.Client
}

// defaultGammaAPIURL is the Gamma API base URL used by NewMarketClient.
// Override with SetGammaAPIURL during startup to honor POLYMARKET_GAMMA_API_URL.
var defaultGammaAPIURL = "https://gamma-api.polymarket.com"

// SetGammaAPIURL overrides the default Gamma API URL used by NewMarketClient.
// Empty input is ignored.
func SetGammaAPIURL(url string) {
	if url != "" {
		defaultGammaAPIURL = url
	}
}

// DefaultGammaAPIURL returns the Gamma API URL used by NewMarketClient.
// Exposed so other packages (e.g., trading.go) can reuse the same value.
func DefaultGammaAPIURL() string {
	return defaultGammaAPIURL
}

// NewMarketClient creates a new market client
func NewMarketClient() *MarketClient {
	return &MarketClient{
		gammaAPIURL: defaultGammaAPIURL,
		httpClient: &http.Client{
			Timeout: 15 * time.Second,
		},
	}
}

// NewMarketClientWithURL creates a market client with a custom base URL (for testing)
func NewMarketClientWithURL(baseURL string) *MarketClient {
	return &MarketClient{
		gammaAPIURL: baseURL,
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

// GammaEventDetail represents a full event with nested markets from the Gamma API
type GammaEventDetail struct {
	ID      string         `json:"id"`
	Slug    string         `json:"slug"`
	Title   string         `json:"title"`
	Markets []*GammaMarket `json:"markets"`
}

// GetEventBySlug fetches an event and its markets by event slug
func (mc *MarketClient) GetEventBySlug(ctx context.Context, slug string) (*GammaEventDetail, error) {
	url := fmt.Sprintf("%s/events?slug=%s", mc.gammaAPIURL, slug)

	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}
	req.Header.Set("Accept", "application/json")

	resp, err := mc.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch event: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return nil, fmt.Errorf("event not found: %s", slug)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("Gamma API returned status %d", resp.StatusCode)
	}

	var events []*GammaEventDetail
	if err := json.NewDecoder(resp.Body).Decode(&events); err != nil {
		return nil, fmt.Errorf("failed to decode events: %w", err)
	}

	if len(events) == 0 {
		return nil, fmt.Errorf("event not found: %s", slug)
	}

	return events[0], nil
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

// FormatPrice formats a price as a percentage. Long-shot markets price below
// 10% in 0.1¢ ticks, so we keep one decimal there to make the precision class
// visible (e.g. "5.1%" vs "5%"); 10%+ markets stay whole-percent for readability.
func FormatPrice(price float64) string {
	if price < 0.10 {
		return fmt.Sprintf("%.1f%%", price*100)
	}
	return fmt.Sprintf("%.0f%%", price*100)
}
