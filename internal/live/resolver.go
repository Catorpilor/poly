package live

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"
)

// EventInfo contains information about a Polymarket event
type EventInfo struct {
	ID      string       `json:"id"`
	Slug    string       `json:"slug"`
	Title   string       `json:"title"`
	Markets []MarketInfo `json:"markets"`
}

// MarketInfo contains information about a market within an event
type MarketInfo struct {
	ID             string   `json:"id"`
	Question       string   `json:"question"`
	ConditionID    string   `json:"conditionId"`
	Slug           string   `json:"slug"`
	Outcomes       []string `json:"-"` // Parsed from OutcomesRaw
	OutcomesRaw    string   `json:"outcomes"`
	ClobTokenIds   []string `json:"-"` // Parsed from ClobTokenIdsRaw
	ClobTokenIdsRaw string  `json:"clobTokenIds"`
	Active         bool     `json:"active"`
	Closed         bool     `json:"closed"`
}

// GetOutcomes parses the outcomes JSON string
func (m *MarketInfo) GetOutcomes() []string {
	if len(m.Outcomes) > 0 {
		return m.Outcomes
	}
	var outcomes []string
	if err := json.Unmarshal([]byte(m.OutcomesRaw), &outcomes); err != nil {
		return []string{"Yes", "No"}
	}
	m.Outcomes = outcomes
	return outcomes
}

// GetClobTokenIds parses the clobTokenIds JSON string
func (m *MarketInfo) GetClobTokenIds() []string {
	if len(m.ClobTokenIds) > 0 {
		return m.ClobTokenIds
	}
	var tokenIds []string
	if err := json.Unmarshal([]byte(m.ClobTokenIdsRaw), &tokenIds); err != nil {
		return nil
	}
	m.ClobTokenIds = tokenIds
	return tokenIds
}

// cacheEntry holds cached event info with expiration
type cacheEntry struct {
	info      *EventInfo
	expiresAt time.Time
}

// EventSlugResolver resolves event slugs to event information
type EventSlugResolver struct {
	gammaAPIURL string
	httpClient  *http.Client
	cache       map[string]*cacheEntry
	cacheTTL    time.Duration
	mu          sync.RWMutex
}

// NewEventSlugResolver creates a new event slug resolver
func NewEventSlugResolver() *EventSlugResolver {
	return &EventSlugResolver{
		gammaAPIURL: "https://gamma-api.polymarket.com",
		httpClient: &http.Client{
			Timeout: 15 * time.Second,
		},
		cache:    make(map[string]*cacheEntry),
		cacheTTL: 5 * time.Minute,
	}
}

// GetEventInfo fetches event information by slug
func (r *EventSlugResolver) GetEventInfo(ctx context.Context, slug string) (*EventInfo, error) {
	// Check cache first
	r.mu.RLock()
	if entry, ok := r.cache[slug]; ok && time.Now().Before(entry.expiresAt) {
		r.mu.RUnlock()
		return entry.info, nil
	}
	r.mu.RUnlock()

	// Fetch from API
	url := fmt.Sprintf("%s/events?slug=%s", r.gammaAPIURL, slug)

	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}
	req.Header.Set("Accept", "application/json")

	resp, err := r.httpClient.Do(req)
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

	var events []EventInfo
	if err := json.NewDecoder(resp.Body).Decode(&events); err != nil {
		return nil, fmt.Errorf("failed to decode events: %w", err)
	}

	if len(events) == 0 {
		return nil, fmt.Errorf("event not found: %s", slug)
	}

	event := &events[0]

	// Cache the result
	r.mu.Lock()
	r.cache[slug] = &cacheEntry{
		info:      event,
		expiresAt: time.Now().Add(r.cacheTTL),
	}
	r.mu.Unlock()

	return event, nil
}

// GetAllAssetIDs returns all asset/token IDs for an event
func (r *EventSlugResolver) GetAllAssetIDs(event *EventInfo) []string {
	var assetIDs []string
	for _, market := range event.Markets {
		tokenIds := market.GetClobTokenIds()
		assetIDs = append(assetIDs, tokenIds...)
	}
	return assetIDs
}

// GetPrimaryMarketAssetIDs returns asset IDs for only the primary (ML) market
// The ML market is identified by NOT having sub-market keywords in the question
func (r *EventSlugResolver) GetPrimaryMarketAssetIDs(event *EventInfo) []string {
	market := r.GetPrimaryMarket(event)
	if market != nil {
		return market.GetClobTokenIds()
	}
	return nil
}

// GetAllMLMarkets returns all moneyline markets for an event
// This handles both 2-way (e.g., NBA: Team A vs Team B) and 3-way (e.g., Football: Team A/Draw/Team B)
func (r *EventSlugResolver) GetAllMLMarkets(event *EventInfo) []*MarketInfo {
	subMarketKeywords := []string{
		"handicap", "kills", "first", "total", "over", "under",
		"map ", "maps", "series:", "inhibitor", "dragon", "baron",
		"tower", "blood", "score", "spread", "points", "goals",
		"o/u", "rebounds", "assists", "1h ", "1q ", "(-", "(+",
		"1st ", "2nd ", "3rd ", "set ",
	}

	var mlMarkets []*MarketInfo

	// Find all markets without sub-market keywords (these are ML markets)
	for i := range event.Markets {
		m := &event.Markets[i]
		if !m.Active || m.Closed {
			continue
		}

		questionLower := strings.ToLower(m.Question)
		isSubMarket := false
		for _, keyword := range subMarketKeywords {
			if strings.Contains(questionLower, keyword) {
				isSubMarket = true
				break
			}
		}

		if !isSubMarket {
			mlMarkets = append(mlMarkets, m)
		}
	}

	// If we found ML markets, return them
	if len(mlMarkets) > 0 {
		return mlMarkets
	}

	// Fallback: look for markets with "win" in question
	for i := range event.Markets {
		m := &event.Markets[i]
		if !m.Active || m.Closed {
			continue
		}
		if strings.Contains(strings.ToLower(m.Question), "win") {
			mlMarkets = append(mlMarkets, m)
		}
	}

	if len(mlMarkets) > 0 {
		return mlMarkets
	}

	// Last resort: return first active market
	for i := range event.Markets {
		if event.Markets[i].Active && !event.Markets[i].Closed {
			return []*MarketInfo{&event.Markets[i]}
		}
	}

	return nil
}

// GetAllMLMarketsAssetIDs returns asset IDs from all moneyline markets
// For 2-way moneyline (NBA), this returns 2 asset IDs (Yes/No for the single ML market)
// For 3-way moneyline (Football), this returns 6 asset IDs (Yes/No for each of 3 markets: Team A/Draw/Team B)
func (r *EventSlugResolver) GetAllMLMarketsAssetIDs(event *EventInfo) []string {
	markets := r.GetAllMLMarkets(event)
	if len(markets) == 0 {
		return nil
	}

	var assetIDs []string
	for _, market := range markets {
		tokenIds := market.GetClobTokenIds()
		assetIDs = append(assetIDs, tokenIds...)
	}
	return assetIDs
}

// GetPrimaryMarket returns the primary (ML) market for an event
// ML markets typically ask "Who will win?" or just have team names as the question
// Sub-markets have keywords like: handicap, kills, first, total, over, under, map, series
func (r *EventSlugResolver) GetPrimaryMarket(event *EventInfo) *MarketInfo {
	subMarketKeywords := []string{
		"handicap", "kills", "first", "total", "over", "under",
		"map ", "maps", "series:", "inhibitor", "dragon", "baron",
		"tower", "blood", "score", "spread", "points", "goals",
		"o/u", "rebounds", "assists", "1h ", "1q ", "(-", "(+",
		"1st ", "2nd ", "3rd ", "set ",
	}

	// First pass: find ML market (no sub-market keywords, active, not closed)
	for i := range event.Markets {
		m := &event.Markets[i]
		if !m.Active || m.Closed {
			continue
		}

		questionLower := strings.ToLower(m.Question)
		isSubMarket := false
		for _, keyword := range subMarketKeywords {
			if strings.Contains(questionLower, keyword) {
				isSubMarket = true
				break
			}
		}

		if !isSubMarket {
			return m
		}
	}

	// Second pass: look for "win" in question
	for i := range event.Markets {
		m := &event.Markets[i]
		if !m.Active || m.Closed {
			continue
		}
		if strings.Contains(strings.ToLower(m.Question), "win") {
			return m
		}
	}

	// Fallback to first active market
	for i := range event.Markets {
		if event.Markets[i].Active && !event.Markets[i].Closed {
			return &event.Markets[i]
		}
	}

	// Last resort: first market
	if len(event.Markets) > 0 {
		return &event.Markets[0]
	}
	return nil
}

// GetAllConditionIDs returns all condition IDs for an event
func (r *EventSlugResolver) GetAllConditionIDs(event *EventInfo) []string {
	var conditionIDs []string
	for _, market := range event.Markets {
		if market.ConditionID != "" {
			conditionIDs = append(conditionIDs, market.ConditionID)
		}
	}
	return conditionIDs
}

// CleanupCache removes expired entries from the cache
func (r *EventSlugResolver) CleanupCache() {
	r.mu.Lock()
	defer r.mu.Unlock()

	now := time.Now()
	for slug, entry := range r.cache {
		if now.After(entry.expiresAt) {
			delete(r.cache, slug)
		}
	}
}

// ExtractMarketShortName extracts a short display name from market question
// e.g., "Will Wolverhampton Wanderers FC win?" -> "WOL"
// e.g., "Draw?" -> "DRAW"
// e.g., "Will Newcastle United FC win?" -> "NEW"
func ExtractMarketShortName(question string) string {
	questionLower := strings.ToLower(question)

	// Check for draw first
	if strings.Contains(questionLower, "draw") {
		return "DRAW"
	}

	// Remove common prefixes
	q := question
	q = strings.TrimPrefix(q, "Will ")
	q = strings.TrimPrefix(q, "will ")

	// Remove common suffixes
	q = strings.TrimSuffix(q, " win?")
	q = strings.TrimSuffix(q, " Win?")
	q = strings.TrimSuffix(q, "?")

	// Try to extract short code from team name
	// Common patterns: "Team Name FC", "Team Name United", etc.
	parts := strings.Fields(q)
	if len(parts) == 0 {
		return strings.ToUpper(q)
	}

	// Use first word, max 3-4 chars for short code
	shortName := strings.ToUpper(parts[0])
	if len(shortName) > 4 {
		shortName = shortName[:3]
	}

	return shortName
}

// GetAssetToMarketNameMap returns a mapping from asset ID to market short name
// This is used to display which market a trade belongs to (e.g., "WOL", "DRAW", "NEW")
func (r *EventSlugResolver) GetAssetToMarketNameMap(event *EventInfo) map[string]string {
	result := make(map[string]string)

	markets := r.GetAllMLMarkets(event)
	if len(markets) <= 1 {
		// For 2-way markets, no need for market name prefix
		return result
	}

	// For 3-way (or more) markets, map each asset to its market short name
	for _, market := range markets {
		shortName := ExtractMarketShortName(market.Question)
		tokenIds := market.GetClobTokenIds()
		for _, tokenId := range tokenIds {
			result[tokenId] = shortName
		}
	}

	return result
}
