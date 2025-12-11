package polymarket

import (
	"context"
	"fmt"
	"math/big"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethclient"
)

// SimplePositionManager uses a simpler approach to find positions
type SimplePositionManager struct {
	client            *ethclient.Client
	conditionalTokens common.Address
}

// NewSimplePositionManager creates a new simple position manager
func NewSimplePositionManager(client *ethclient.Client) *SimplePositionManager {
	return &SimplePositionManager{
		client:            client,
		conditionalTokens: common.HexToAddress("0x4D97DCd97eC945f40cF65F87097ACe5EA0476045"), // ConditionalTokens on Polygon
	}
}

// GetRecentPositions fetches recent position transfers for a user
func (spm *SimplePositionManager) GetRecentPositions(ctx context.Context, proxyAddress common.Address) (string, error) {
	// Get current block number
	currentBlock, err := spm.client.BlockNumber(ctx)
	if err != nil {
		return "", fmt.Errorf("failed to get current block: %w", err)
	}

	// Start with last 20 blocks (~40 seconds on Polygon)
	// Most RPCs are very restrictive
	fromBlock := currentBlock - 20

	// Event signature for TransferSingle(operator, from, to, id, value)
	transferSingleSig := crypto.Keccak256Hash([]byte("TransferSingle(address,address,address,uint256,uint256)"))

	// Query for recent transfers TO the proxy
	queryTo := ethereum.FilterQuery{
		FromBlock: big.NewInt(int64(fromBlock)),
		ToBlock:   big.NewInt(int64(currentBlock)),
		Addresses: []common.Address{spm.conditionalTokens},
		Topics: [][]common.Hash{
			{transferSingleSig},
			nil, // operator
			nil, // from
			{common.BytesToHash(common.LeftPadBytes(proxyAddress.Bytes(), 32))}, // to = proxy
		},
	}

	logsTo, err := spm.client.FilterLogs(ctx, queryTo)
	if err != nil {
		// If block range is still too large, try smaller
		fromBlock = currentBlock - 10 // Last ~20 seconds
		queryTo.FromBlock = big.NewInt(int64(fromBlock))

		logsTo, err = spm.client.FilterLogs(ctx, queryTo)
		if err != nil {
			// Last attempt with just 5 blocks
			fromBlock = currentBlock - 5 // Last ~10 seconds
			queryTo.FromBlock = big.NewInt(int64(fromBlock))

			logsTo, err = spm.client.FilterLogs(ctx, queryTo)
			if err != nil {
				// Can't scan events, return message about limitations
				return spm.getManualCheckMessage(ctx, proxyAddress)
			}
		}
	}

	// Also query for transfers FROM the proxy (sells)
	queryFrom := ethereum.FilterQuery{
		FromBlock: big.NewInt(int64(fromBlock)),
		ToBlock:   big.NewInt(int64(currentBlock)),
		Addresses: []common.Address{spm.conditionalTokens},
		Topics: [][]common.Hash{
			{transferSingleSig},
			nil, // operator
			{common.BytesToHash(common.LeftPadBytes(proxyAddress.Bytes(), 32))}, // from = proxy
			nil, // to
		},
	}

	logsFrom, err := spm.client.FilterLogs(ctx, queryFrom)
	if err != nil {
		// Ignore error for FROM query, we at least have TO events
		logsFrom = nil
	}

	// Process the events to find unique token IDs
	tokenActivity := make(map[string]*big.Int)

	// Process incoming transfers (buys)
	for _, log := range logsTo {
		if len(log.Data) >= 64 {
			tokenID := common.BytesToHash(log.Data[:32]).Hex()
			value := new(big.Int).SetBytes(log.Data[32:64])

			if current, exists := tokenActivity[tokenID]; exists {
				tokenActivity[tokenID] = new(big.Int).Add(current, value)
			} else {
				tokenActivity[tokenID] = value
			}
		}
	}

	// Process outgoing transfers (sells)
	if logsFrom != nil {
		for _, log := range logsFrom {
			if len(log.Data) >= 64 {
				tokenID := common.BytesToHash(log.Data[:32]).Hex()
				value := new(big.Int).SetBytes(log.Data[32:64])

				if current, exists := tokenActivity[tokenID]; exists {
					tokenActivity[tokenID] = new(big.Int).Sub(current, value)
				} else {
					tokenActivity[tokenID] = new(big.Int).Neg(value)
				}
			}
		}
	}

	// Now check current balances for active positions
	activePositions := []string{}

	for tokenID := range tokenActivity {
		tokenIDBig := common.HexToHash(tokenID).Big()
		balance, err := spm.getTokenBalance(ctx, proxyAddress, tokenIDBig)
		if err != nil {
			continue
		}

		if balance.Cmp(big.NewInt(0)) > 0 {
			// Format the position
			sharesDisplay := FormatShares(balance)
			position := fmt.Sprintf("Token %s: %s shares", tokenID[:10]+"...", sharesDisplay)
			activePositions = append(activePositions, position)
		}
	}

	// Format the summary
	if len(activePositions) == 0 {
		return "No active positions found in recent activity", nil
	}

	summary := fmt.Sprintf("📊 *Found %d Active Positions*\n\n", len(activePositions))
	for i, pos := range activePositions {
		summary += fmt.Sprintf("%d. %s\n", i+1, pos)
	}

	// Calculate time range scanned
	blocksScanned := currentBlock - uint64(fromBlock)
	var timeDesc string
	if blocksScanned < 100 {
		timeDesc = fmt.Sprintf("~%d minutes", blocksScanned/3)
	} else if blocksScanned < 3000 {
		timeDesc = fmt.Sprintf("~%.1f hours", float64(blocksScanned)/120)
	} else {
		timeDesc = fmt.Sprintf("~%.1f days", float64(blocksScanned)/43200)
	}

	summary += fmt.Sprintf("\n_Scanned last %d blocks (%s)_", blocksScanned, timeDesc)

	return summary, nil
}

// getTokenBalance queries the balance of a specific ERC-1155 token
func (spm *SimplePositionManager) getTokenBalance(ctx context.Context, owner common.Address, tokenID *big.Int) (*big.Int, error) {
	// ERC-1155 balanceOf(address,uint256) method
	methodID := common.FromHex("0x00fdd58e")

	// Encode parameters
	data := append(methodID, common.LeftPadBytes(owner.Bytes(), 32)...)
	data = append(data, common.LeftPadBytes(tokenID.Bytes(), 32)...)

	msg := ethereum.CallMsg{
		To:   &spm.conditionalTokens,
		Data: data,
	}

	result, err := spm.client.CallContract(ctx, msg, nil)
	if err != nil {
		return nil, err
	}

	if len(result) == 0 {
		return big.NewInt(0), nil
	}

	return new(big.Int).SetBytes(result), nil
}

// getManualCheckMessage returns a message when event scanning fails
func (spm *SimplePositionManager) getManualCheckMessage(ctx context.Context, proxyAddress common.Address) (string, error) {
	message := `📊 *Position Detection Limited*

⚠️ The RPC endpoint is restricting blockchain queries.

*What this means:*
• Can only scan last few seconds of activity
• Recent trades may not show immediately
• Older positions won't be detected

*Your positions are still safe!*
They exist on the blockchain even if not shown here.

*To see all positions:*
1. Visit https://polymarket.com/portfolio
2. Or try again in a few minutes
3. Or wait for trades to settle

_Note: This is a temporary RPC limitation, not a bot issue._`

	return message, nil
}

// ScanKnownMarkets checks balances for known popular market token IDs
func (spm *SimplePositionManager) ScanKnownMarkets(ctx context.Context, proxyAddress common.Address) (string, error) {
	// These would be actual token IDs from popular markets
	// For now, this is a placeholder - you'd need to maintain a list of active market token IDs
	knownMarkets := []struct {
		name     string
		question string
		yesToken string
		noToken  string
	}{
		// Add known market token IDs here
		// Example:
		// {
		//     name: "2024 Election",
		//     question: "Will Trump win?",
		//     yesToken: "0x123...",
		//     noToken: "0x456...",
		// },
	}

	if len(knownMarkets) == 0 {
		return "No known markets configured. Please check recent activity instead.", nil
	}

	positions := []string{}

	for _, market := range knownMarkets {
		// Check YES token balance
		if market.yesToken != "" {
			yesTokenID := common.HexToHash(market.yesToken).Big()
			yesBalance, err := spm.getTokenBalance(ctx, proxyAddress, yesTokenID)
			if err == nil && yesBalance.Cmp(big.NewInt(0)) > 0 {
				positions = append(positions, fmt.Sprintf("%s - YES: %s shares", market.name, FormatShares(yesBalance)))
			}
		}

		// Check NO token balance
		if market.noToken != "" {
			noTokenID := common.HexToHash(market.noToken).Big()
			noBalance, err := spm.getTokenBalance(ctx, proxyAddress, noTokenID)
			if err == nil && noBalance.Cmp(big.NewInt(0)) > 0 {
				positions = append(positions, fmt.Sprintf("%s - NO: %s shares", market.name, FormatShares(noBalance)))
			}
		}
	}

	if len(positions) == 0 {
		return "No positions found in known markets", nil
	}

	summary := fmt.Sprintf("📊 *Positions in Known Markets (%d)*\n\n", len(positions))
	for i, pos := range positions {
		summary += fmt.Sprintf("%d. %s\n", i+1, pos)
	}

	return summary, nil
}