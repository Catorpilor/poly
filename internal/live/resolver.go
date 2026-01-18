package live

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
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
// The primary market is typically the first active market (index 0)
func (r *EventSlugResolver) GetPrimaryMarketAssetIDs(event *EventInfo) []string {
	// Find the first active, non-closed market
	for _, market := range event.Markets {
		if market.Active && !market.Closed {
			return market.GetClobTokenIds()
		}
	}
	// Fallback to first market if none are active
	if len(event.Markets) > 0 {
		return event.Markets[0].GetClobTokenIds()
	}
	return nil
}

// GetPrimaryMarket returns the primary (ML) market for an event
func (r *EventSlugResolver) GetPrimaryMarket(event *EventInfo) *MarketInfo {
	for i := range event.Markets {
		if event.Markets[i].Active && !event.Markets[i].Closed {
			return &event.Markets[i]
		}
	}
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
