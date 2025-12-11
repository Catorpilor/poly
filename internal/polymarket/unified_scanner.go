package polymarket

import (
	"context"
	"fmt"
	"log"
	"math/big"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/ethclient"
)

// UnifiedPositionScanner uses Polymarket Data API for position queries
type UnifiedPositionScanner struct {
	client          *ethclient.Client
	positionManager *PositionManager
}

// NewUnifiedPositionScanner creates a scanner that uses the Data API
func NewUnifiedPositionScanner(client *ethclient.Client) *UnifiedPositionScanner {
	return &UnifiedPositionScanner{
		client:          client,
		positionManager: NewPositionManager(client, "https://clob.polymarket.com"),
	}
}

// ScanAllStrategies fetches positions using Polymarket Data API
func (ups *UnifiedPositionScanner) ScanAllStrategies(ctx context.Context, proxyAddress common.Address) (string, error) {
	log.Printf("Starting position scan for proxy: %s", proxyAddress.Hex())

	// Use Polymarket Data API only
	log.Printf("Fetching positions from Polymarket Data API...")
	positions, err := ups.positionManager.GetUserPositionsFromAPI(ctx, proxyAddress)
	if err != nil {
		log.Printf("Data API error: %v", err)
		return "Failed to fetch positions. Please try again later.", err
	}

	log.Printf("Data API returned %d active positions", len(positions))

	// Return formatted positions (empty list if none)
	return ups.formatPositionsFromAPI(positions), nil
}

// GetPositions fetches raw positions for selling
func (ups *UnifiedPositionScanner) GetPositions(ctx context.Context, proxyAddress common.Address) ([]*Position, error) {
	return ups.positionManager.GetUserPositionsFromAPI(ctx, proxyAddress)
}

// formatPositionsFromAPI formats positions from the Data API for Telegram display
func (ups *UnifiedPositionScanner) formatPositionsFromAPI(positions []*Position) string {
	if len(positions) == 0 {
		return "📊 *No Active Positions Found*"
	}

	result := fmt.Sprintf("📊 *Your Positions (%d)*\n\n", len(positions))

	for i, pos := range positions {
		// Truncate long titles
		title := pos.MarketTitle
		if len(title) > 50 {
			title = title[:47] + "..."
		}

		result += fmt.Sprintf("*%d. %s*\n", i+1, title)
		result += fmt.Sprintf("   • Outcome: %s\n", pos.Outcome)
		result += fmt.Sprintf("   • Shares: %s\n", FormatShares(pos.Shares))
		result += fmt.Sprintf("   • Avg Price: $%.2f\n", pos.AveragePrice)
		result += fmt.Sprintf("   • Current: $%.2f\n", pos.CurrentPrice)
		result += fmt.Sprintf("   • Value: $%.2f\n", pos.Value)

		// Show P&L with color indicator
		pnlIndicator := "📈"
		if pos.PnL < 0 {
			pnlIndicator = "📉"
		}
		result += fmt.Sprintf("   • P&L: %s $%.2f (%.1f%%)\n", pnlIndicator, pos.PnL, pos.PnLPercent)

		if i < len(positions)-1 {
			result += "\n"
		}
	}

	result += "\n*Source:* Polymarket Data API"
	return result
}

// GetPositionValue attempts to calculate the value of positions
func (ups *UnifiedPositionScanner) GetPositionValue(ctx context.Context, proxyAddress common.Address) (*big.Int, error) {
	// This would require:
	// 1. Getting all position token IDs
	// 2. Getting balances for each
	// 3. Getting current prices from CLOB
	// 4. Calculating total value

	// For now, return a placeholder
	return big.NewInt(0), fmt.Errorf("position value calculation not implemented")
}