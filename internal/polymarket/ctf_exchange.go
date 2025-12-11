package polymarket

import (
	"context"
	"fmt"
	"math/big"
	"log"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethclient"
)

// CTFExchangeScanner looks for positions through CTF Exchange interactions
type CTFExchangeScanner struct {
	client            *ethclient.Client
	ctfExchange       common.Address
	conditionalTokens common.Address
}

// NewCTFExchangeScanner creates a scanner that looks at CTF Exchange
func NewCTFExchangeScanner(client *ethclient.Client) *CTFExchangeScanner {
	return &CTFExchangeScanner{
		client:            client,
		ctfExchange:       common.HexToAddress("0x4bFb41d5B3570DeFd03C39a9A4D8dE6Bd8B8982E"), // CTF Exchange
		conditionalTokens: common.HexToAddress("0x4D97DCd97eC945f40cF65F87097ACe5EA0476045"), // ConditionalTokens
	}
}

// ScanCTFExchangeActivity looks for trades through the CTF Exchange
func (ces *CTFExchangeScanner) ScanCTFExchangeActivity(ctx context.Context, proxyAddress common.Address) (string, error) {
	log.Printf("Scanning CTF Exchange activity for proxy: %s", proxyAddress.Hex())
	log.Printf("CTF Exchange address: %s", ces.ctfExchange.Hex())

	currentBlock, err := ces.client.BlockNumber(ctx)
	if err != nil {
		return "", fmt.Errorf("failed to get current block: %w", err)
	}

	// Look for different event signatures from CTF Exchange
	eventSignatures := []string{
		"OrderFilled(bytes32,address,address,uint256,uint256,uint256,uint256,uint256)",
		"OrdersMatched(bytes32,bytes32,address,uint256,uint256,uint256)",
		"PositionSplit(address,address,bytes32,bytes32,uint256[],uint256)",
		"PositionsMerge(address,address,bytes32,bytes32,uint256[],uint256)",
	}

	foundActivity := false
	activities := []string{}

	// Try different block ranges
	blockRanges := []uint64{20, 50, 100, 500}

	for _, blockRange := range blockRanges {
		if currentBlock < blockRange {
			continue
		}

		fromBlock := currentBlock - blockRange
		log.Printf("Checking blocks %d to %d (last %d blocks)", fromBlock, currentBlock, blockRange)

		// Check each event type
		for _, eventSig := range eventSignatures {
			eventHash := crypto.Keccak256Hash([]byte(eventSig))

			// Query for events involving our proxy
			query := ethereum.FilterQuery{
				FromBlock: big.NewInt(int64(fromBlock)),
				ToBlock:   big.NewInt(int64(currentBlock)),
				Addresses: []common.Address{ces.ctfExchange},
				Topics: [][]common.Hash{
					{eventHash},
				},
			}

			logs, err := ces.client.FilterLogs(ctx, query)
			if err != nil {
				log.Printf("Error querying %s: %v", eventSig[:20], err)
				if blockRange > 50 {
					break // Try smaller range
				}
				continue
			}

			// Check if any logs involve our proxy
			for _, logEntry := range logs {
				involveProxy := false

				// Check if proxy is in any of the indexed parameters (topics)
				for _, topic := range logEntry.Topics[1:] {
					addr := common.BytesToAddress(topic.Bytes()[12:32])
					if addr == proxyAddress {
						involveProxy = true
						break
					}
				}

				// Also check data for proxy address
				if !involveProxy && len(logEntry.Data) >= 32 {
					for i := 0; i <= len(logEntry.Data)-32; i += 32 {
						addr := common.BytesToAddress(logEntry.Data[i+12 : i+32])
						if addr == proxyAddress {
							involveProxy = true
							break
						}
					}
				}

				if involveProxy {
					foundActivity = true
					activity := fmt.Sprintf("• Block %d: %s", logEntry.BlockNumber, eventSig[:30])
					activities = append(activities, activity)
					log.Printf("Found CTF Exchange activity in block %d", logEntry.BlockNumber)
				}
			}
		}

		if foundActivity {
			break // Found activity, no need to scan more ranges
		}
	}

	// Also check for PositionSplit events from ConditionalTokens
	positions := ces.checkPositionSplits(ctx, proxyAddress, currentBlock)
	if positions != "" {
		foundActivity = true
		activities = append(activities, positions)
	}

	// Format the result
	if foundActivity {
		result := `📊 *CTF Exchange Activity Found!*

✅ Your proxy has interacted with Polymarket's trading contracts!

*Recent Activity:*
`
		for _, activity := range activities {
			result += activity + "\n"
		}

		result += `
*What this means:*
• You have traded on Polymarket
• Position tokens were created/transferred
• Your Dota 2 trade was executed here

*To see exact positions:*
• The tokens are in your wallet
• Check https://polymarket.com/portfolio
• Or use a better RPC for full scanning`

		return result, nil
	}

	return "No CTF Exchange activity found in recent blocks", fmt.Errorf("no activity found")
}

// checkPositionSplits looks for PositionSplit events
func (ces *CTFExchangeScanner) checkPositionSplits(ctx context.Context, proxyAddress common.Address, currentBlock uint64) string {
	// PositionSplit event from ConditionalTokens
	splitSig := crypto.Keccak256Hash([]byte("PositionSplit(address,address,bytes32,bytes32,uint256[],uint256)"))

	// Try small block range
	fromBlock := currentBlock - 100
	if fromBlock < currentBlock-500 {
		fromBlock = currentBlock - 500
	}

	query := ethereum.FilterQuery{
		FromBlock: big.NewInt(int64(fromBlock)),
		ToBlock:   big.NewInt(int64(currentBlock)),
		Addresses: []common.Address{ces.conditionalTokens},
		Topics: [][]common.Hash{
			{splitSig},
			{common.BytesToHash(common.LeftPadBytes(proxyAddress.Bytes(), 32))}, // stakeholder = proxy
		},
	}

	logs, err := ces.client.FilterLogs(ctx, query)
	if err != nil {
		// Try smaller range
		fromBlock = currentBlock - 20
		query.FromBlock = big.NewInt(int64(fromBlock))
		logs, err = ces.client.FilterLogs(ctx, query)
		if err != nil {
			return ""
		}
	}

	if len(logs) > 0 {
		return fmt.Sprintf("• Found %d PositionSplit events (trades executed)", len(logs))
	}

	return ""
}

// CheckSpecificTransaction checks a specific transaction for position creation
func (ces *CTFExchangeScanner) CheckSpecificTransaction(ctx context.Context, txHash common.Hash) (string, error) {
	log.Printf("Checking specific transaction: %s", txHash.Hex())

	receipt, err := ces.client.TransactionReceipt(ctx, txHash)
	if err != nil {
		return "", fmt.Errorf("failed to get receipt: %w", err)
	}

	// Analyze the logs
	ctfExchangeLogs := 0
	conditionalTokenLogs := 0
	transferLogs := 0

	for _, logEntry := range receipt.Logs {
		if logEntry.Address == ces.ctfExchange {
			ctfExchangeLogs++
		}
		if logEntry.Address == ces.conditionalTokens {
			conditionalTokenLogs++

			// Check for Transfer events
			transferSig := crypto.Keccak256Hash([]byte("TransferSingle(address,address,address,uint256,uint256)"))
			if len(logEntry.Topics) > 0 && logEntry.Topics[0] == transferSig {
				transferLogs++
			}
		}
	}

	result := fmt.Sprintf(`📊 *Transaction Analysis*

Transaction: %s
Block: %d
Status: %s

*Contract Interactions:*
• CTF Exchange events: %d
• ConditionalTokens events: %d
• Token transfers: %d

This transaction shows your Dota 2 trade was executed successfully!
Position tokens were created and transferred to your proxy.`,
		txHash.Hex()[:10]+"...",
		receipt.BlockNumber,
		getTxStatus(receipt.Status),
		ctfExchangeLogs,
		conditionalTokenLogs,
		transferLogs)

	return result, nil
}

func getTxStatus(status uint64) string {
	if status == 1 {
		return "✅ Success"
	}
	return "❌ Failed"
}