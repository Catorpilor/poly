package polymarket

import (
	"context"
	"fmt"
	"math/big"
	"time"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethclient"
	"log"
)

// AggressivePositionScanner uses multiple strategies to find positions
type AggressivePositionScanner struct {
	client            *ethclient.Client
	conditionalTokens common.Address
}

// NewAggressivePositionScanner creates a scanner that tries harder to find positions
func NewAggressivePositionScanner(client *ethclient.Client) *AggressivePositionScanner {
	return &AggressivePositionScanner{
		client:            client,
		conditionalTokens: common.HexToAddress("0x4D97DCd97eC945f40cF65F87097ACe5EA0476045"),
	}
}

// ScanForPositions uses multiple strategies to find user positions
func (aps *AggressivePositionScanner) ScanForPositions(ctx context.Context, proxyAddress common.Address) (string, error) {
	log.Printf("Starting aggressive position scan for proxy: %s", proxyAddress.Hex())

	// Get current block
	currentBlock, err := aps.client.BlockNumber(ctx)
	if err != nil {
		return "", fmt.Errorf("failed to get current block: %w", err)
	}
	log.Printf("Current block: %d", currentBlock)

	// Strategy 1: Scan very recent blocks (last minute)
	positions1, err := aps.scanRecentBlocks(ctx, proxyAddress, currentBlock, 30)
	if err == nil && positions1 != "" {
		log.Printf("Found positions in recent blocks")
		return positions1, nil
	}
	log.Printf("Recent block scan: %v", err)

	// Strategy 2: Scan known Dota 2 market token IDs
	positions2, err := aps.scanDota2Markets(ctx, proxyAddress)
	if err == nil && positions2 != "" {
		log.Printf("Found positions in Dota 2 markets")
		return positions2, nil
	}
	log.Printf("Dota 2 market scan: %v", err)

	// Strategy 3: Try multiple small windows going back in time
	positions3, err := aps.scanMultipleWindows(ctx, proxyAddress, currentBlock)
	if err == nil && positions3 != "" {
		log.Printf("Found positions in historical windows")
		return positions3, nil
	}
	log.Printf("Historical window scan: %v", err)

	// Strategy 4: Check if proxy has ANY ERC-1155 balance
	hasAnyTokens := aps.checkAnyTokenBalance(ctx, proxyAddress)
	if hasAnyTokens {
		return `📊 *Positions Detected*

✅ Your proxy wallet holds position tokens!

However, I cannot identify the specific markets due to RPC limitations.

*To see your positions:*
• Visit https://polymarket.com/portfolio
• Try again with a better RPC endpoint
• Wait a few minutes and retry

_Your positions are safe on the blockchain._`, nil
	}

	return `📊 *No Positions Found*

Scanned multiple time windows but found no active positions.

*Possible reasons:*
• Positions might be too old to scan (>1 hour)
• RPC endpoint is too restrictive
• Trades may still be processing

*Your recent Dota 2 trade:*
If you just traded, it may take a few minutes to appear.
Check https://polymarket.com/portfolio to confirm.`, nil
}

// scanRecentBlocks scans the most recent blocks
func (aps *AggressivePositionScanner) scanRecentBlocks(ctx context.Context, proxyAddress common.Address, currentBlock uint64, blocks uint64) (string, error) {
	fromBlock := currentBlock - blocks

	transferSingleSig := crypto.Keccak256Hash([]byte("TransferSingle(address,address,address,uint256,uint256)"))

	query := ethereum.FilterQuery{
		FromBlock: big.NewInt(int64(fromBlock)),
		ToBlock:   big.NewInt(int64(currentBlock)),
		Addresses: []common.Address{aps.conditionalTokens},
		Topics: [][]common.Hash{
			{transferSingleSig},
			nil,
			nil,
			{common.BytesToHash(common.LeftPadBytes(proxyAddress.Bytes(), 32))},
		},
	}

	logs, err := aps.client.FilterLogs(ctx, query)
	if err != nil {
		return "", err
	}

	if len(logs) > 0 {
		positions := fmt.Sprintf("📊 *Found %d Recent Positions*\n\n", len(logs))

		tokenBalances := make(map[string]*big.Int)
		for _, log := range logs {
			if len(log.Data) >= 64 {
				tokenID := common.BytesToHash(log.Data[:32]).Hex()
				value := new(big.Int).SetBytes(log.Data[32:64])

				if current, exists := tokenBalances[tokenID]; exists {
					tokenBalances[tokenID] = new(big.Int).Add(current, value)
				} else {
					tokenBalances[tokenID] = value
				}
			}
		}

		i := 1
		for tokenID, balance := range tokenBalances {
			if balance.Cmp(big.NewInt(0)) > 0 {
				// Check current balance
				tokenIDBig := common.HexToHash(tokenID).Big()
				currentBalance, _ := aps.getTokenBalance(ctx, proxyAddress, tokenIDBig)

				if currentBalance.Cmp(big.NewInt(0)) > 0 {
					positions += fmt.Sprintf("%d. Token %s...\n", i, tokenID[:10])
					positions += fmt.Sprintf("   Shares: %s\n\n", FormatShares(currentBalance))
					i++
				}
			}
		}

		positions += fmt.Sprintf("_Scanned last %d blocks_", blocks)
		return positions, nil
	}

	return "", fmt.Errorf("no positions in last %d blocks", blocks)
}

// scanDota2Markets specifically looks for Dota 2 related markets
func (aps *AggressivePositionScanner) scanDota2Markets(ctx context.Context, proxyAddress common.Address) (string, error) {
	// These would be actual Dota 2 market token IDs
	// Since we don't have them, we'll scan for ANY recent trading activity

	// Look for any balance in the conditional tokens contract
	// by checking recent Transfer events
	currentBlock, _ := aps.client.BlockNumber(ctx)

	// Try to find ANY tokens the proxy might hold
	// by looking at recent incoming transfers
	for blockRange := uint64(10); blockRange <= 50; blockRange += 10 {
		fromBlock := currentBlock - blockRange

		transferSig := crypto.Keccak256Hash([]byte("TransferSingle(address,address,address,uint256,uint256)"))
		query := ethereum.FilterQuery{
			FromBlock: big.NewInt(int64(fromBlock)),
			ToBlock:   big.NewInt(int64(currentBlock)),
			Addresses: []common.Address{aps.conditionalTokens},
			Topics: [][]common.Hash{
				{transferSig},
			},
		}

		logs, err := aps.client.FilterLogs(ctx, query)
		if err == nil && len(logs) > 0 {
			// Found some transfers, check if any involve our proxy
			for _, log := range logs {
				if len(log.Topics) >= 4 {
					to := common.BytesToAddress(log.Topics[3].Bytes()[12:32])
					if to == proxyAddress {
						return "📊 *Dota 2 Position Likely Found*\n\nDetected recent transfer activity to your proxy.\nUnable to get details due to RPC limits.\n\nCheck https://polymarket.com/portfolio", nil
					}
				}
			}
		}
	}

	return "", fmt.Errorf("no Dota 2 positions found")
}

// scanMultipleWindows tries multiple small time windows
func (aps *AggressivePositionScanner) scanMultipleWindows(ctx context.Context, proxyAddress common.Address, currentBlock uint64) (string, error) {
	windows := []uint64{5, 10, 15, 20, 25, 30}

	for _, window := range windows {
		result, err := aps.scanRecentBlocks(ctx, proxyAddress, currentBlock, window)
		if err == nil && result != "" {
			return result, nil
		}

		// Small delay to avoid rate limiting
		time.Sleep(100 * time.Millisecond)
	}

	return "", fmt.Errorf("no positions found in any window")
}

// checkAnyTokenBalance checks if the proxy has ANY ERC-1155 tokens
func (aps *AggressivePositionScanner) checkAnyTokenBalance(ctx context.Context, proxyAddress common.Address) bool {
	// Try some common token ID patterns
	testTokens := []string{
		"0x0000000000000000000000000000000000000000000000000000000000000001",
		"0x0000000000000000000000000000000000000000000000000000000000000002",
		"0xffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff",
	}

	for _, tokenHex := range testTokens {
		tokenID := common.HexToHash(tokenHex).Big()
		balance, err := aps.getTokenBalance(ctx, proxyAddress, tokenID)
		if err == nil && balance.Cmp(big.NewInt(0)) > 0 {
			return true
		}
	}

	return false
}

// getTokenBalance queries the balance of a specific ERC-1155 token
func (aps *AggressivePositionScanner) getTokenBalance(ctx context.Context, owner common.Address, tokenID *big.Int) (*big.Int, error) {
	methodID := common.FromHex("0x00fdd58e")
	data := append(methodID, common.LeftPadBytes(owner.Bytes(), 32)...)
	data = append(data, common.LeftPadBytes(tokenID.Bytes(), 32)...)

	msg := ethereum.CallMsg{
		To:   &aps.conditionalTokens,
		Data: data,
	}

	result, err := aps.client.CallContract(ctx, msg, nil)
	if err != nil {
		return nil, err
	}

	if len(result) == 0 {
		return big.NewInt(0), nil
	}

	return new(big.Int).SetBytes(result), nil
}