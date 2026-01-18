package live

import (
	"fmt"
	"strings"

	"github.com/shopspring/decimal"
)

// TradeInfo represents a trade event for formatting
type TradeInfo struct {
	EventSlug   string
	ProxyWallet string
	Pseudonym   string
	Side        string // BUY or SELL
	Outcome     string // YES, NO, or custom outcome
	MarketName  string // Short market name (e.g., "WOL", "DRAW", "NEW" for 3-way)
	Size        decimal.Decimal
	Price       decimal.Decimal
	Timestamp   int64
}

// TradeFormatter formats trades for different outputs
type TradeFormatter struct{}

// NewTradeFormatter creates a new trade formatter
func NewTradeFormatter() *TradeFormatter {
	return &TradeFormatter{}
}

// FormatForTelegram formats a trade for Telegram display
// Format: "[LAL-POR] Whale123 BUY YES $500.00 @ $0.65"
// For 3-way: "[WOL-NEW] Whale123 BUY WOL YES $500.00 @ $0.65"
func (f *TradeFormatter) FormatForTelegram(trade *TradeInfo) string {
	trader := trade.Pseudonym
	if trader == "" {
		trader = truncateAddress(trade.ProxyWallet)
	}

	shortEvent := ShortenEventSlug(trade.EventSlug)

	// Combine market name with outcome for clearer display
	outcome := trade.Outcome
	if trade.MarketName != "" {
		outcome = trade.MarketName + " " + trade.Outcome
	}

	return fmt.Sprintf("[%s] %s %s %s $%s @ $%s",
		shortEvent,
		trader,
		strings.ToUpper(trade.Side),
		outcome,
		trade.Size.StringFixed(2),
		trade.Price.StringFixed(2),
	)
}

// FormatForWeb formats a trade for web display (returns JSON-friendly struct)
type WebTradeFormat struct {
	EventSlug   string `json:"eventSlug"`
	Trader      string `json:"trader"`
	ProxyWallet string `json:"proxyWallet"`
	Side        string `json:"side"`
	Outcome     string `json:"outcome"`
	Size        string `json:"size"`
	Price       string `json:"price"`
	Timestamp   int64  `json:"timestamp"`
}

// FormatForWeb converts TradeInfo to web-friendly format
func (f *TradeFormatter) FormatForWeb(trade *TradeInfo) *WebTradeFormat {
	trader := trade.Pseudonym
	if trader == "" {
		trader = truncateAddress(trade.ProxyWallet)
	}

	// Combine market name with outcome for 3-way markets
	outcome := trade.Outcome
	if trade.MarketName != "" {
		outcome = trade.MarketName + " " + trade.Outcome
	}

	return &WebTradeFormat{
		EventSlug:   trade.EventSlug,
		Trader:      trader,
		ProxyWallet: trade.ProxyWallet,
		Side:        strings.ToUpper(trade.Side),
		Outcome:     outcome,
		Size:        trade.Size.StringFixed(2),
		Price:       trade.Price.StringFixed(2),
		Timestamp:   trade.Timestamp,
	}
}

// truncateAddress shortens a wallet address for display
// e.g., "0x1234567890abcdef1234567890abcdef12345678" -> "0x1234...5678"
func truncateAddress(addr string) string {
	if len(addr) <= 10 {
		return addr
	}
	return addr[:6] + "..." + addr[len(addr)-4:]
}

// ShortenEventSlug extracts a short identifier from an event slug
// e.g., "nba-lal-por-2026-01-17" -> "LAL-POR"
func ShortenEventSlug(slug string) string {
	parts := strings.Split(slug, "-")
	if len(parts) < 3 {
		if len(slug) > 12 {
			return strings.ToUpper(slug[:12])
		}
		return strings.ToUpper(slug)
	}

	// Common patterns:
	// "nba-lal-por-2026-01-17" -> "LAL-POR"
	// "nfl-dal-phi-2026-01-17" -> "DAL-PHI"
	// Try to extract team codes (usually 3-letter codes after sport prefix)
	if len(parts) >= 3 {
		// Check if first part looks like a sport prefix (nba, nfl, mlb, nhl, etc.)
		sport := strings.ToLower(parts[0])
		if sport == "nba" || sport == "nfl" || sport == "mlb" || sport == "nhl" ||
			sport == "soccer" || sport == "football" || sport == "hockey" {
			// Extract team codes
			if len(parts) >= 3 {
				team1 := strings.ToUpper(parts[1])
				team2 := strings.ToUpper(parts[2])
				return team1 + "-" + team2
			}
		}
	}

	// Fallback: just use first 12 chars uppercased
	if len(slug) > 12 {
		return strings.ToUpper(slug[:12])
	}
	return strings.ToUpper(slug)
}
