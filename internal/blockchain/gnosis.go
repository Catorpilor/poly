package blockchain

import (
	"context"
	"fmt"
	"math/big"
	"strings"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/ethclient"
)

// GnosisSafeDetector handles detection of Gnosis Safe proxy wallets
type GnosisSafeDetector struct {
	client *ethclient.Client
	// Known Gnosis Safe factory addresses on Polygon
	factoryAddresses []common.Address
	// Polymarket's specific proxy implementation
	polymarketRegistry common.Address
}

// NewGnosisSafeDetector creates a new Gnosis Safe detector
func NewGnosisSafeDetector(client *ethclient.Client) *GnosisSafeDetector {
	return &GnosisSafeDetector{
		client: client,
		// These are known Gnosis Safe factory addresses on Polygon
		factoryAddresses: []common.Address{
			common.HexToAddress("0xa6B71E26C5e0845f74c812102Ca7114b6a896AB2"), // GnosisSafeProxyFactory 1.3.0
			common.HexToAddress("0xC22834581EbC8527d974F8a1c97E1bEA4EF910BC"), // GnosisSafeProxyFactory 1.1.1
		},
		// Polymarket's registry contract (if they have one)
		polymarketRegistry: common.HexToAddress("0x0000000000000000000000000000000000000000"), // TODO: Find actual address
	}
}

// FindProxyWallet attempts to find a Gnosis Safe proxy wallet for an EOA
func (d *GnosisSafeDetector) FindProxyWallet(ctx context.Context, eoaAddress common.Address) (common.Address, error) {
	// Method 1: Check if the EOA is an owner of any Gnosis Safe
	// This is done by checking ProxyCreation events from the factory
	proxy, err := d.checkFactoryEvents(ctx, eoaAddress)
	if err == nil && proxy != (common.Address{}) {
		return proxy, nil
	}

	// Method 2: Use Polymarket's specific registry if available
	proxy, err = d.checkPolymarketRegistry(ctx, eoaAddress)
	if err == nil && proxy != (common.Address{}) {
		return proxy, nil
	}

	// Method 3: Check recent transactions from the EOA to find proxy interactions
	proxy, err = d.checkRecentTransactions(ctx, eoaAddress)
	if err == nil && proxy != (common.Address{}) {
		return proxy, nil
	}

	// Method 4: Brute force check known proxy patterns
	proxy, err = d.checkKnownPatterns(ctx, eoaAddress)
	if err == nil && proxy != (common.Address{}) {
		return proxy, nil
	}

	return common.Address{}, fmt.Errorf("no proxy wallet found for EOA %s", eoaAddress.Hex())
}

// checkFactoryEvents checks ProxyCreation events from Gnosis Safe factories
func (d *GnosisSafeDetector) checkFactoryEvents(ctx context.Context, eoaAddress common.Address) (common.Address, error) {
	// ProxyCreation event signature
	// event ProxyCreation(address proxy, address singleton);
	proxyCreationTopic := common.HexToHash("0x4f51faf6c4561ff95f067657e43439f0f856d97c04d9ec9070a6199ad418e235")

	for _, factory := range d.factoryAddresses {
		// Query for ProxyCreation events
		query := ethereum.FilterQuery{
			FromBlock: big.NewInt(30000000), // Start from a recent block on Polygon
			ToBlock:   nil,                   // Latest block
			Addresses: []common.Address{factory},
			Topics: [][]common.Hash{
				{proxyCreationTopic},
			},
		}

		logs, err := d.client.FilterLogs(ctx, query)
		if err != nil {
			continue // Try next factory
		}

		// Check each proxy to see if the EOA is an owner
		for _, log := range logs {
			if len(log.Data) >= 32 {
				proxyAddr := common.BytesToAddress(log.Data[:32])

				// Check if EOA is an owner of this proxy
				isOwner, err := d.isOwnerOfSafe(ctx, proxyAddr, eoaAddress)
				if err == nil && isOwner {
					return proxyAddr, nil
				}
			}
		}
	}

	return common.Address{}, fmt.Errorf("no proxy found in factory events")
}

// isOwnerOfSafe checks if an address is an owner of a Gnosis Safe
func (d *GnosisSafeDetector) isOwnerOfSafe(ctx context.Context, safeAddress, potentialOwner common.Address) (bool, error) {
	// ABI for isOwner function
	const isOwnerABI = `[{"inputs":[{"name":"owner","type":"address"}],"name":"isOwner","outputs":[{"name":"","type":"bool"}],"stateMutability":"view","type":"function"}]`

	parsedABI, err := abi.JSON(strings.NewReader(isOwnerABI))
	if err != nil {
		return false, err
	}

	// Pack the function call
	data, err := parsedABI.Pack("isOwner", potentialOwner)
	if err != nil {
		return false, err
	}

	// Call the contract
	msg := ethereum.CallMsg{
		To:   &safeAddress,
		Data: data,
	}

	result, err := d.client.CallContract(ctx, msg, nil)
	if err != nil {
		return false, err
	}

	// Unpack the result
	var isOwner bool
	err = parsedABI.UnpackIntoInterface(&isOwner, "isOwner", result)
	if err != nil {
		return false, err
	}

	return isOwner, nil
}

// checkPolymarketRegistry checks Polymarket's specific registry for proxy wallets
func (d *GnosisSafeDetector) checkPolymarketRegistry(ctx context.Context, eoaAddress common.Address) (common.Address, error) {
	// TODO: Implement when we know Polymarket's registry contract
	// For now, return not found
	return common.Address{}, fmt.Errorf("polymarket registry not implemented")
}

// checkRecentTransactions checks recent transactions to find proxy interactions
func (d *GnosisSafeDetector) checkRecentTransactions(ctx context.Context, eoaAddress common.Address) (common.Address, error) {
	// Get the latest block
	latestBlock, err := d.client.BlockByNumber(ctx, nil)
	if err != nil {
		return common.Address{}, err
	}

	// Check last 1000 blocks (adjust as needed)
	fromBlock := new(big.Int).Sub(latestBlock.Number(), big.NewInt(1000))
	if fromBlock.Sign() < 0 {
		fromBlock = big.NewInt(0)
	}

	// Look for transactions from the EOA
	// This is a simplified approach - in production you'd want to use an indexer
	// or a service like Etherscan API

	return common.Address{}, fmt.Errorf("transaction scan not fully implemented")
}

// checkKnownPatterns checks for known proxy wallet patterns
func (d *GnosisSafeDetector) checkKnownPatterns(ctx context.Context, eoaAddress common.Address) (common.Address, error) {
	// Common pattern: Proxy address is deterministic based on EOA and salt
	// This would require knowing Polymarket's specific salt pattern

	return common.Address{}, fmt.Errorf("pattern matching not implemented")
}

// GetSafeOwners returns all owners of a Gnosis Safe
func (d *GnosisSafeDetector) GetSafeOwners(ctx context.Context, safeAddress common.Address) ([]common.Address, error) {
	// ABI for getOwners function
	const getOwnersABI = `[{"inputs":[],"name":"getOwners","outputs":[{"name":"","type":"address[]"}],"stateMutability":"view","type":"function"}]`

	parsedABI, err := abi.JSON(strings.NewReader(getOwnersABI))
	if err != nil {
		return nil, err
	}

	// Pack the function call
	data, err := parsedABI.Pack("getOwners")
	if err != nil {
		return nil, err
	}

	// Call the contract
	msg := ethereum.CallMsg{
		To:   &safeAddress,
		Data: data,
	}

	result, err := d.client.CallContract(ctx, msg, nil)
	if err != nil {
		return nil, err
	}

	// Unpack the result
	var owners []common.Address
	err = parsedABI.UnpackIntoInterface(&owners, "getOwners", result)
	if err != nil {
		return nil, err
	}

	return owners, nil
}