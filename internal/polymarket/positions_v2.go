package polymarket

import (
	"context"
	"encoding/hex"
	"fmt"
	"math/big"
	"strings"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethclient"

	"github.com/Catorpilor/poly/internal/blockchain"
)

// PositionV2 represents a user's position with market details
type PositionV2 struct {
	MarketQuestion string   `json:"market_question"`
	ConditionID    string   `json:"condition_id"`
	TokenID        string   `json:"token_id"`
	Outcome        string   `json:"outcome"` // YES or NO
	Shares         *big.Int `json:"shares"`
	SharesDisplay  string   `json:"shares_display"`
	Value          float64  `json:"value,omitempty"` // Current value in USDC
}

// PositionManagerV2 handles position queries using multiple data sources
type PositionManagerV2 struct {
	client            *ethclient.Client
	conditionalTokens common.Address
	subgraph          *SubgraphClient
}

// NewPositionManagerV2 creates a new position manager.
// Reads addresses from blockchain package vars (configured via blockchain.InitAddresses at startup).
func NewPositionManagerV2(client *ethclient.Client) *PositionManagerV2 {
	return &PositionManagerV2{
		client:            client,
		conditionalTokens: blockchain.ConditionalTokensAddress,
		subgraph:          NewSubgraphClient(),
	}
}

// GetPositions fetches all positions for a user's proxy wallet
func (pm *PositionManagerV2) GetPositions(ctx context.Context, proxyAddress common.Address) ([]*PositionV2, error) {
	positions := []*PositionV2{}

	// Method 1: Try to get positions from subgraph
	subgraphPositions, err := pm.subgraph.GetUserPositions(ctx, proxyAddress)
	if err == nil && len(subgraphPositions) > 0 {
		// Convert subgraph positions to our format
		for _, sp := range subgraphPositions {
			if sp.Market == nil {
				continue
			}

			// Parse YES shares
			if sp.YesShares != "" && sp.YesShares != "0" {
				yesShares, _ := new(big.Int).SetString(sp.YesShares, 10)
				if yesShares != nil && yesShares.Cmp(big.NewInt(0)) > 0 {
					positions = append(positions, &PositionV2{
						MarketQuestion: sp.Market.Question,
						ConditionID:    sp.Market.ConditionID,
						Outcome:        "YES",
						Shares:         yesShares,
						SharesDisplay:  FormatShares(yesShares),
					})
				}
			}

			// Parse NO shares
			if sp.NoShares != "" && sp.NoShares != "0" {
				noShares, _ := new(big.Int).SetString(sp.NoShares, 10)
				if noShares != nil && noShares.Cmp(big.NewInt(0)) > 0 {
					positions = append(positions, &PositionV2{
						MarketQuestion: sp.Market.Question,
						ConditionID:    sp.Market.ConditionID,
						Outcome:        "NO",
						Shares:         noShares,
						SharesDisplay:  FormatShares(noShares),
					})
				}
			}
		}

		return positions, nil
	}

	// Method 2: Scan for positions using event logs
	return pm.scanForPositions(ctx, proxyAddress)
}

// scanForPositions scans blockchain events to find user positions
func (pm *PositionManagerV2) scanForPositions(ctx context.Context, proxyAddress common.Address) ([]*PositionV2, error) {
	// Look for TransferSingle and TransferBatch events from ConditionalTokens
	// These indicate position token transfers

	// Event signature for TransferSingle(operator, from, to, id, value)
	transferSingleSig := crypto.Keccak256Hash([]byte("TransferSingle(address,address,address,uint256,uint256)"))

	// Query for events where proxy received tokens (to == proxyAddress)
	query := ethereum.FilterQuery{
		FromBlock: big.NewInt(40000000), // Start from a reasonable block
		Addresses: []common.Address{pm.conditionalTokens},
		Topics: [][]common.Hash{
			{transferSingleSig},
			nil, // operator (any)
			nil, // from (any)
			{common.BytesToHash(common.LeftPadBytes(proxyAddress.Bytes(), 32))}, // to = proxy
		},
	}

	logs, err := pm.client.FilterLogs(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("failed to query events: %w", err)
	}

	// Track unique token IDs
	tokenIDs := make(map[string]bool)
	for _, log := range logs {
		if len(log.Data) >= 64 {
			// Extract token ID from event data
			tokenID := common.BytesToHash(log.Data[:32])
			tokenIDs[tokenID.Hex()] = true
		}
	}

	// For each unique token ID, check current balance
	positions := []*PositionV2{}
	for tokenIDHex := range tokenIDs {
		tokenID := common.HexToHash(tokenIDHex).Big()

		// Get current balance
		balance, err := pm.getTokenBalance(ctx, proxyAddress, tokenID)
		if err != nil {
			continue
		}

		if balance.Cmp(big.NewInt(0)) > 0 {
			// Try to get market info for this token
			conditionID, outcome := pm.parseTokenID(tokenID)

			// Get market details from subgraph
			market, _ := pm.subgraph.GetMarketByConditionID(ctx, conditionID)

			marketQuestion := "Unknown Market"
			if market != nil {
				marketQuestion = market.Question
			}

			positions = append(positions, &PositionV2{
				MarketQuestion: marketQuestion,
				ConditionID:    conditionID,
				TokenID:        tokenIDHex,
				Outcome:        outcome,
				Shares:         balance,
				SharesDisplay:  FormatShares(balance),
			})
		}
	}

	return positions, nil
}

// getTokenBalance queries the balance of a specific ERC-1155 token
func (pm *PositionManagerV2) getTokenBalance(ctx context.Context, owner common.Address, tokenID *big.Int) (*big.Int, error) {
	// ERC-1155 balanceOf(address,uint256) method
	// Method ID: 0x00fdd58e (keccak256("balanceOf(address,uint256)"))
	methodID := common.FromHex("0x00fdd58e")

	// Encode parameters
	data := append(methodID, common.LeftPadBytes(owner.Bytes(), 32)...)
	data = append(data, common.LeftPadBytes(tokenID.Bytes(), 32)...)

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

	return new(big.Int).SetBytes(result), nil
}

// parseTokenID attempts to extract condition ID and outcome from token ID
func (pm *PositionManagerV2) parseTokenID(tokenID *big.Int) (conditionID string, outcome string) {
	// Token ID structure in ConditionalTokens:
	// tokenId = keccak256(abi.encodePacked(collateralToken, collectionId))
	// collectionId = keccak256(abi.encodePacked(conditionId, indexSet))

	// This is complex to reverse, so we'll return placeholders
	// In production, you'd maintain a mapping or use the subgraph

	// Check if it's a YES or NO token based on patterns
	// This is a simplified heuristic
	tokenIDHex := hex.EncodeToString(tokenID.Bytes())
	if strings.HasSuffix(tokenIDHex, "01") {
		outcome = "YES"
	} else if strings.HasSuffix(tokenIDHex, "02") {
		outcome = "NO"
	} else {
		outcome = "UNKNOWN"
	}

	// For condition ID, we'd need to maintain a mapping or query events
	conditionID = "0x" + tokenIDHex[:16] + "..." // Truncated for display

	return conditionID, outcome
}

// GetPositionsSummary returns a summary of all positions
func (pm *PositionManagerV2) GetPositionsSummary(ctx context.Context, proxyAddress common.Address) (string, int, error) {
	positions, err := pm.GetPositions(ctx, proxyAddress)
	if err != nil {
		return "", 0, err
	}

	if len(positions) == 0 {
		return "No active positions", 0, nil
	}

	summary := fmt.Sprintf("📊 *Active Positions (%d)*\n\n", len(positions))

	for i, pos := range positions {
		// Truncate long questions
		question := pos.MarketQuestion
		if len(question) > 60 {
			question = question[:57] + "..."
		}

		summary += fmt.Sprintf("%d. *%s*\n", i+1, question)
		summary += fmt.Sprintf("   • Outcome: %s\n", pos.Outcome)
		summary += fmt.Sprintf("   • Shares: %s\n", pos.SharesDisplay)

		if i < len(positions)-1 {
			summary += "\n"
		}
	}

	return summary, len(positions), nil
}