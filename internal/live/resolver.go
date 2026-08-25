package live

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Catorpilor/poly/internal/polymarket"
)

// ErrEventNotClosed is the common negative for the event-level closed check:
// Gamma returned no event under the closed=true filter, so the event is still
// active/open (or does not exist). The watch expiry sweep matches it with
// errors.Is to tell "keep quietly" from a real lookup error — the event-level
// analog of polymarket.ErrMarketNotFound in the resolved-arm sweeper.
var ErrEventNotClosed = errors.New("event not closed")

// subMarketKeywords is the single source of truth for keywords that indicate a sub-market
// (not a moneyline market). Used by GetPrimaryMarket and GetAllMLMarkets.
var subMarketKeywords = []string{
	"handicap", "kills", "first", "over", "under",
	"map ", "maps", "series:", "inhibitor", "dragon", "baron",
	"tower", "blood", "score", "spread", "points", "goals",
	"o/u", "rebounds", "assists", "1h ", "1q ", "(-", "(+",
	"1st ", "2nd ", "3rd ", "set ",
	"total games", "total goals", "total kills", "total sets",
	"total points", "total maps", "total rounds",
	": total", ": o/u",
	"- game ", "game handicap", "games total",
	// "game " (trailing space) marks per-game markets ("Game 1 Winner") as
	// sub-markets, mirroring "map ". Without it, LoL game-winner markets
	// classified as ML and a Bo3 series rendered as a fake 3-way market.
	// The trailing space keeps "games" (e.g. "total games") unaffected.
	"game ",
}

// isSubMarketQuestion checks if a market question contains sub-market keywords
// SeriesWatchMarket reports whether an event market belongs in the snipe
// watcher's series-continuation set (issue #94): the main moneyline plus the
// per-game/map WINNER markets — never the prop sub-markets. Game winners are
// sub-markets to the feed renderer (subMarketKeywords lists "game "/"map "),
// but for crash recipiency they are first-class: both recipients=0 misses
// (VISION G3, FUT Map 4) were game-winner markets of an event the holder was
// already exposed to. Pure — table-tested.
func SeriesWatchMarket(question string) bool {
	if !isSubMarketQuestion(question) {
		return true // the event's moneyline
	}
	q := strings.ToLower(question)
	if !strings.Contains(q, "winner") {
		return false
	}
	return strings.Contains(q, "game ") || strings.Contains(q, "map ")
}

// GameNumber extracts the game/map ordinal from a winner-market question
// ("… - Game 3 Winner", "… - Map 4 Winner") — 0 when the question is not a
// game-winner (series moneylines, props). Pure — table-tested. The future-game
// gate (issue #97) uses it to order an event's games.
func GameNumber(question string) int {
	m := gameWinnerRe.FindStringSubmatch(question)
	if m == nil {
		return 0
	}
	n, err := strconv.Atoi(m[1])
	if err != nil {
		return 0
	}
	return n
}

var gameWinnerRe = regexp.MustCompile(`(?i)(?:game|map)\s+(\d+)\s+winner`)

func isSubMarketQuestion(question string) bool {
	questionLower := strings.ToLower(question)
	for _, keyword := range subMarketKeywords {
		if strings.Contains(questionLower, keyword) {
			return true
		}
	}
	return false
}

// EventInfo contains information about a Polymarket event
type EventInfo struct {
	ID      string       `json:"id"`
	Slug    string       `json:"slug"`
	Title   string       `json:"title"`
	Markets []MarketInfo `json:"markets"`
}

// MarketInfo contains information about a market within an event
type MarketInfo struct {
	ID               string   `json:"id"`
	Question         string   `json:"question"`
	ConditionID      string   `json:"conditionId"`
	Slug             string   `json:"slug"`
	Outcomes         []string `json:"-"` // Parsed from OutcomesRaw
	OutcomesRaw      string   `json:"outcomes"`
	ClobTokenIds     []string `json:"-"` // Parsed from ClobTokenIdsRaw
	ClobTokenIdsRaw  string   `json:"clobTokenIds"`
	OutcomePrices    []string `json:"-"` // Parsed from OutcomePricesRaw
	OutcomePricesRaw string   `json:"outcomePrices"`
	Active           bool     `json:"active"`
	Closed           bool     `json:"closed"`
	// GameStartTimeRaw is Gamma's scheduled game start (sports markets only).
	// Parsed on demand by GetGameStartTime, like the other raw fields.
	GameStartTimeRaw string `json:"gameStartTime"`
}

// The accessors below parse their raw JSON on every call instead of
// memoizing into the struct: MarketInfo values live inside the resolver's
// shared event cache, so a lazy write races concurrent readers (two
// trades, or a trade and a picker fetch, on the same event). The parsed
// fields remain as read-only injection points for tests and pre-populated
// data.

// GetOutcomes parses the outcomes JSON string
func (m *MarketInfo) GetOutcomes() []string {
	if len(m.Outcomes) > 0 {
		return m.Outcomes
	}
	var outcomes []string
	if err := json.Unmarshal([]byte(m.OutcomesRaw), &outcomes); err != nil {
		return []string{"Yes", "No"}
	}
	return outcomes
}

// GetOutcomePrices parses the outcomePrices JSON string. These are
// indicative (Gamma's last-known prices, cached up to the resolver TTL) —
// a fill still prices off the live order book at execution.
func (m *MarketInfo) GetOutcomePrices() []string {
	if len(m.OutcomePrices) > 0 {
		return m.OutcomePrices
	}
	var prices []string
	if err := json.Unmarshal([]byte(m.OutcomePricesRaw), &prices); err != nil {
		return nil
	}
	return prices
}

// GetGameStartTime parses the market's scheduled game start. Zero when the
// field is absent or unparseable — the snipe watcher treats unknown starts as
// "never in-play".
func (m *MarketInfo) GetGameStartTime() time.Time {
	return polymarket.ParseGameStartTime(m.GameStartTimeRaw)
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

// defaultGammaAPIURL is the Gamma API base URL used by NewEventSlugResolver.
// Override with SetGammaAPIURL during startup to honor POLYMARKET_GAMMA_API_URL.
var defaultGammaAPIURL = "https://gamma-api.polymarket.com"

// SetGammaAPIURL overrides the default Gamma API URL used by NewEventSlugResolver.
// Empty input is ignored.
func SetGammaAPIURL(url string) {
	if url != "" {
		defaultGammaAPIURL = url
	}
}

// NewEventSlugResolver creates a new event slug resolver
func NewEventSlugResolver() *EventSlugResolver {
	return &EventSlugResolver{
		gammaAPIURL: defaultGammaAPIURL,
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

	event, err := r.fetchEventBySlug(ctx, slug)
	if err != nil {
		// Users paste market slugs where event slugs are expected — a
		// Polymarket market page URL ends in the market slug (e.g.
		// "…-2026-07-12-game1"). Follow the market to its parent event.
		parentSlug, perr := r.parentEventSlug(ctx, slug)
		if perr != nil {
			return nil, err // report the original event-not-found
		}
		if event, err = r.fetchEventBySlug(ctx, parentSlug); err != nil {
			return nil, err
		}
	}

	r.cacheEvent(slug, event)
	if event.Slug != "" && event.Slug != slug {
		r.cacheEvent(event.Slug, event)
	}

	return event, nil
}

// fetchEventBySlug does the raw Gamma /events lookup.
func (r *EventSlugResolver) fetchEventBySlug(ctx context.Context, slug string) (*EventInfo, error) {
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

	return &events[0], nil
}

// ClosedEventBySlug fetches the event for slug ONLY when Gamma affirmatively
// reports it closed. It is the event-level analog of
// polymarket.GetClosedMarketByConditionID (the #40 sweeper doctrine) and the
// only positive-evidence source the watch expiry sweep may act on (ADR 0008
// phase 4).
//
// The closed=true filter returns the event iff Gamma considers it closed; an
// active event yields an empty list, surfaced as ErrEventNotClosed (the common
// negative — "keep quietly"). The response slug is validated against the
// request: Gamma silently IGNORES unknown query params and returns a default
// list (#33), so an unmatched slug must NEVER pass as this event's result.
// Callers additionally require every nested market to be closed=true before
// expiring a watch, so even a regressed closed filter cannot sweep a live
// event. This lookup deliberately does NOT touch the resolver cache — a closed
// event must not start being served to the trade feed / refresh paths.
func (r *EventSlugResolver) ClosedEventBySlug(ctx context.Context, slug string) (*EventInfo, error) {
	url := fmt.Sprintf("%s/events?slug=%s&closed=true", r.gammaAPIURL, slug)

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
		return nil, fmt.Errorf("%w: %s", ErrEventNotClosed, slug)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("Gamma API returned status %d", resp.StatusCode)
	}

	var events []EventInfo
	if err := json.NewDecoder(resp.Body).Decode(&events); err != nil {
		return nil, fmt.Errorf("failed to decode events: %w", err)
	}

	if len(events) == 0 {
		return nil, fmt.Errorf("%w: %s", ErrEventNotClosed, slug)
	}

	// Identity check (#33): if Gamma ignored the slug filter and returned a
	// default list of closed events, the first must not pass as this event.
	if !strings.EqualFold(events[0].Slug, slug) {
		return nil, fmt.Errorf("gamma returned event %q for requested slug %s — filter not applied",
			events[0].Slug, slug)
	}

	return &events[0], nil
}

// parentEventSlug resolves a market slug to the slug of the event that
// contains it, via Gamma's /markets lookup.
func (r *EventSlugResolver) parentEventSlug(ctx context.Context, marketSlug string) (string, error) {
	url := fmt.Sprintf("%s/markets?slug=%s", r.gammaAPIURL, marketSlug)

	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return "", fmt.Errorf("failed to create request: %w", err)
	}
	req.Header.Set("Accept", "application/json")

	resp, err := r.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("failed to fetch market: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("Gamma API returned status %d", resp.StatusCode)
	}

	var markets []struct {
		Events []struct {
			Slug string `json:"slug"`
		} `json:"events"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&markets); err != nil {
		return "", fmt.Errorf("failed to decode markets: %w", err)
	}

	if len(markets) == 0 || len(markets[0].Events) == 0 || markets[0].Events[0].Slug == "" {
		return "", fmt.Errorf("market not found: %s", marketSlug)
	}

	return markets[0].Events[0].Slug, nil
}

func (r *EventSlugResolver) cacheEvent(slug string, event *EventInfo) {
	r.mu.Lock()
	r.cache[slug] = &cacheEntry{
		info:      event,
		expiresAt: time.Now().Add(r.cacheTTL),
	}
	r.mu.Unlock()
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
	var mlMarkets []*MarketInfo

	// Find all markets without sub-market keywords (these are ML markets)
	for i := range event.Markets {
		m := &event.Markets[i]
		if !m.Active || m.Closed {
			continue
		}

		if !isSubMarketQuestion(m.Question) {
			mlMarkets = append(mlMarkets, m)
		}
	}

	// If we found ML markets, return them
	if len(mlMarkets) > 0 {
		return mlMarkets
	}

	// Fallback: look for markets with "win" in question but still skip sub-markets
	// (e.g., "Who will win?" is OK, but "Set 1 Winner" is not)
	for i := range event.Markets {
		m := &event.Markets[i]
		if !m.Active || m.Closed {
			continue
		}
		questionLower := strings.ToLower(m.Question)
		if strings.Contains(questionLower, "win") && !isSubMarketQuestion(m.Question) {
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

// GetSubMarkets returns an event's tradeable non-Moneyline markets: active,
// not closed, and not in the ML set. The ML set is excluded by market ID
// (not question wording), so whatever GetAllMLMarkets selects — including
// its "win"/first-active fallbacks — is filtered out consistently.
func (r *EventSlugResolver) GetSubMarkets(event *EventInfo) []*MarketInfo {
	mlIDs := make(map[string]bool)
	for _, m := range r.GetAllMLMarkets(event) {
		mlIDs[m.ID] = true
	}

	var subs []*MarketInfo
	for i := range event.Markets {
		m := &event.Markets[i]
		if !m.Active || m.Closed || mlIDs[m.ID] {
			continue
		}
		subs = append(subs, m)
	}
	return subs
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
	// First pass: find ML market (no sub-market keywords, active, not closed)
	for i := range event.Markets {
		m := &event.Markets[i]
		if !m.Active || m.Closed {
			continue
		}

		if !isSubMarketQuestion(m.Question) {
			return m
		}
	}

	// Second pass: look for "win" in question but still skip sub-markets
	// (e.g., "Who will win?" is OK, but "Set 1 Winner" is not)
	for i := range event.Markets {
		m := &event.Markets[i]
		if !m.Active || m.Closed {
			continue
		}
		questionLower := strings.ToLower(m.Question)
		if strings.Contains(questionLower, "win") && !isSubMarketQuestion(m.Question) {
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
