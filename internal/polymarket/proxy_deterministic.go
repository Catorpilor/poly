package polymarket

import (
	"context"
	"encoding/hex"
	"fmt"
	"math/big"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"golang.org/x/crypto/sha3"
)

// Polymarket's actual proxy registry and factory contract addresses
var (
	// Polymarket's actual Proxy Registry/Factory contract on Polygon
	// This contract maintains the mapping of EOA addresses to proxy wallets
	PolymarketProxyRegistry = common.HexToAddress("0xaacfeea03eb1561c4e67d661e40682bd20e3541b")

	// Alternative registry address (in case they switch)
	AlternativeRegistry = common.HexToAddress("0x31337e8A3a90218024bF64FCd39dE039C5DB87c1")

	// Alternative factories that might be used
	AlternativeFactories = []common.Address{
		common.HexToAddress("0x4e59b44847b379578588920ca78fbf26c0b4956c"), // CREATE2 Deployer
		common.HexToAddress("0xC22834581EbC8527d974F8a1c97E1bEA4EF910BC"), // Safe Proxy Factory
	}
)

// DeterministicProxyResolver calculates proxy addresses deterministically
type DeterministicProxyResolver struct {
	client interface {
		CallContract(ctx context.Context, msg ethereum.CallMsg, blockNumber *big.Int) ([]byte, error)
		CodeAt(ctx context.Context, contract common.Address, blockNumber *big.Int) ([]byte, error)
	}
}

// NewDeterministicProxyResolver creates a new deterministic resolver
func NewDeterministicProxyResolver(client interface {
	CallContract(ctx context.Context, msg ethereum.CallMsg, blockNumber *big.Int) ([]byte, error)
	CodeAt(ctx context.Context, contract common.Address, blockNumber *big.Int) ([]byte, error)
}) *DeterministicProxyResolver {
	return &DeterministicProxyResolver{
		client: client,
	}
}

// GetPolymarketProxy retrieves the proxy address for a given EOA
func (r *DeterministicProxyResolver) GetPolymarketProxy(ctx context.Context, eoaAddress common.Address) (common.Address, error) {
	// Method 1: Query the Polymarket Registry directly (most reliable and fastest)
	proxy, err := r.GetProxyFromRegistry(ctx, eoaAddress)
	if err == nil && proxy != (common.Address{}) {
		return proxy, nil
	}

	// Method 2: Search transaction history for proxy deployment events
	proxy, err = r.FindProxyFromTransactionHistory(ctx, eoaAddress)
	if err == nil && proxy != (common.Address{}) {
		return proxy, nil
	}

	// Method 3: Try the standard Polymarket deterministic pattern
	deterministicProxy := r.calculateDeterministicProxy(eoaAddress)
	if r.isContract(ctx, deterministicProxy) {
		return deterministicProxy, nil
	}

	// Method 4: Try with different salt patterns using alternative registry
	salts := r.generateSalts(eoaAddress)
	for _, salt := range salts {
		proxyAddr := r.calculateCreate2Address(AlternativeRegistry, salt, r.getInitCodeHash())
		if r.isContract(ctx, proxyAddr) {
			return proxyAddr, nil
		}
	}

	// Method 5: Check alternative factories
	for _, factory := range AlternativeFactories {
		for _, salt := range salts {
			proxyAddr := r.calculateCreate2Address(factory, salt, r.getInitCodeHash())
			if r.isContract(ctx, proxyAddr) {
				return proxyAddr, nil
			}
		}
	}

	return common.Address{}, fmt.Errorf("no proxy found for EOA %s", eoaAddress.Hex())
}

// GetProxyFromRegistry queries the Polymarket Registry contract directly
// This is the most reliable method as it queries the actual on-chain registry
func (r *DeterministicProxyResolver) GetProxyFromRegistry(ctx context.Context, eoaAddress common.Address) (common.Address, error) {
	// The actual method signature is: computeProxyAddress(address) returns (address)
	methodID := crypto.Keccak256([]byte("computeProxyAddress(address)"))[:4]

	// Pad the address to 32 bytes
	paddedAddress := common.LeftPadBytes(eoaAddress.Bytes(), 32)

	// Construct the call data
	data := append(methodID, paddedAddress...)

	// Call the registry contract
	msg := ethereum.CallMsg{
		To:   &PolymarketProxyRegistry,
		Data: data,
	}

	result, err := r.client.CallContract(ctx, msg, nil)
	if err != nil {
		return common.Address{}, fmt.Errorf("failed to query registry: %w", err)
	}

	if len(result) >= 32 {
		// Extract the address from the returned bytes
		proxy := common.BytesToAddress(result[12:32])
		if proxy != (common.Address{}) {
			return proxy, nil
		}
	}

	return common.Address{}, fmt.Errorf("no proxy found in registry for %s", eoaAddress.Hex())
}

// calculateDeterministicProxy uses Polymarket's specific deterministic pattern
func (r *DeterministicProxyResolver) calculateDeterministicProxy(eoaAddress common.Address) common.Address {
	// Polymarket likely uses a deterministic pattern like:
	// keccak256(0xff ++ factory_address ++ salt ++ init_code_hash)[12:]

	// The salt is typically derived from the EOA address
	salt := crypto.Keccak256(eoaAddress.Bytes())

	// Calculate CREATE2 address using the main registry
	return r.calculateCreate2Address(PolymarketProxyRegistry, salt, r.getInitCodeHash())
}

// calculateCreate2Address calculates a CREATE2 address
func (r *DeterministicProxyResolver) calculateCreate2Address(factory common.Address, salt []byte, initCodeHash []byte) common.Address {
	// CREATE2 address = keccak256(0xff ++ factory ++ salt ++ initCodeHash)[12:]
	data := []byte{0xff}
	data = append(data, factory.Bytes()...)
	data = append(data, salt...)
	data = append(data, initCodeHash...)

	hash := crypto.Keccak256(data)
	return common.BytesToAddress(hash[12:])
}

// generateSalts generates various salt patterns that Polymarket might use
func (r *DeterministicProxyResolver) generateSalts(eoaAddress common.Address) [][]byte {
	salts := [][]byte{}

	// Pattern 1: Direct keccak256 of address
	salts = append(salts, crypto.Keccak256(eoaAddress.Bytes()))

	// Pattern 2: Padded address as salt
	paddedAddr := common.LeftPadBytes(eoaAddress.Bytes(), 32)
	salts = append(salts, paddedAddr)

	// Pattern 3: Address with zero padding
	salt3 := make([]byte, 32)
	copy(salt3[12:], eoaAddress.Bytes())
	salts = append(salts, salt3)

	// Pattern 4: Polymarket might use address + nonce
	for nonce := 0; nonce < 5; nonce++ {
		nonceBytes := big.NewInt(int64(nonce)).Bytes()
		saltWithNonce := crypto.Keccak256(eoaAddress.Bytes(), nonceBytes)
		salts = append(salts, saltWithNonce)
	}

	// Pattern 5: Lowercase hex string of address
	lowerAddr := []byte(eoaAddress.Hex()[2:]) // Remove 0x prefix
	salts = append(salts, crypto.Keccak256(lowerAddr))

	return salts
}

// getInitCodeHash returns the init code hash for Polymarket proxy contracts
func (r *DeterministicProxyResolver) getInitCodeHash() []byte {
	// This is typically the keccak256 of the proxy contract's init code
	// For Gnosis Safe proxies, this is a known value
	// You might need to update this based on Polymarket's actual implementation

	// Common Gnosis Safe Proxy init code hash
	initCodeHex := "0x6c9a6c4a39284e37ed1cf53d337577d14212a4870fb976a4366c693b939918d5"
	hash, _ := hex.DecodeString(initCodeHex[2:])

	return hash
}

// isContract checks if an address contains contract code
func (r *DeterministicProxyResolver) isContract(ctx context.Context, address common.Address) bool {
	code, err := r.client.CodeAt(ctx, address, nil)
	if err != nil {
		return false
	}
	return len(code) > 0
}

// FindProxyFromTransactionHistory searches for proxy deployment events in the transaction history
// This is used as a fallback method when the registry query fails
func (r *DeterministicProxyResolver) FindProxyFromTransactionHistory(ctx context.Context, eoaAddress common.Address) (common.Address, error) {
	// The proxy is deployed via Polymarket's Proxy Wallet Deployer contract
	// Look for ProxyDeployed event (custom event from Polymarket)
	// event signature: ProxyDeployed(address indexed user, address proxy)
	eventSig := crypto.Keccak256Hash([]byte("ProxyDeployed(address,address)"))

	// Create filter query for the ProxyDeployed event
	query := ethereum.FilterQuery{
		FromBlock: big.NewInt(25000000), // Polymarket launch on Polygon (approximate)
		Addresses: []common.Address{PolymarketProxyRegistry},
		Topics: [][]common.Hash{
			{eventSig},
			{common.BytesToHash(common.LeftPadBytes(eoaAddress.Bytes(), 32))}, // indexed user parameter
		},
	}

	// Note: This requires an Ethereum client that supports FilterLogs
	// If the client doesn't support FilterLogs, this will fail
	if filterClient, ok := r.client.(interface {
		FilterLogs(ctx context.Context, q ethereum.FilterQuery) ([]types.Log, error)
	}); ok {
		logs, err := filterClient.FilterLogs(ctx, query)
		if err != nil {
			return common.Address{}, fmt.Errorf("failed to filter logs: %w", err)
		}

		if len(logs) > 0 && len(logs[0].Data) >= 32 {
			// The proxy address is in the event data (non-indexed parameter)
			return common.BytesToAddress(logs[0].Data[:32]), nil
		}
	}

	// Alternative: Try looking for ProxyCreated event with different signature
	altEventSig := crypto.Keccak256Hash([]byte("ProxyCreated(address,address)"))
	query.Topics[0] = []common.Hash{altEventSig}

	if filterClient, ok := r.client.(interface {
		FilterLogs(ctx context.Context, q ethereum.FilterQuery) ([]types.Log, error)
	}); ok {
		logs, err := filterClient.FilterLogs(ctx, query)
		if err == nil && len(logs) > 0 && len(logs[0].Data) >= 32 {
			return common.BytesToAddress(logs[0].Data[:32]), nil
		}
	}

	return common.Address{}, fmt.Errorf("no proxy deployment found in transaction history for %s", eoaAddress.Hex())
}

// GetProxyUsingRegistry queries a potential Polymarket registry contract
func (r *DeterministicProxyResolver) GetProxyUsingRegistry(ctx context.Context, eoaAddress common.Address) (common.Address, error) {
	// This method is now deprecated in favor of GetProxyFromRegistry
	// It's kept for backward compatibility but delegates to the new method
	return r.GetProxyFromRegistry(ctx, eoaAddress)
}

// tryAlternativeRegistryMethods tries different method names that Polymarket might use
func (r *DeterministicProxyResolver) tryAlternativeRegistryMethods(ctx context.Context, eoaAddress common.Address) (common.Address, error) {
	methodNames := []string{
		"computeProxyAddress(address)", // The ACTUAL method name in Polymarket registry!
		"proxyWallets(address)",
		"userToProxy(address)",
		"getWallet(address)",
		"wallets(address)",
		"proxies(address)",
		"getProxyAddress(address)",
	}

	// Try both the main registry and alternative addresses
	registries := []common.Address{PolymarketProxyRegistry, AlternativeRegistry}

	for _, registry := range registries {
		for _, methodName := range methodNames {
			methodID := crypto.Keccak256([]byte(methodName))[:4]
			paddedAddress := common.LeftPadBytes(eoaAddress.Bytes(), 32)
			data := append(methodID, paddedAddress...)

			msg := ethereum.CallMsg{
				To:   &registry,
				Data: data,
			}

			result, err := r.client.CallContract(ctx, msg, nil)
			if err == nil && len(result) >= 32 {
				addr := common.BytesToAddress(result[12:32])
				if addr != (common.Address{}) {
					return addr, nil
				}
			}
		}
	}

	return common.Address{}, fmt.Errorf("no proxy found in registry")
}

// CalculatePolymarketProxyAddress calculates the most likely proxy address
// This is based on reverse engineering Polymarket's actual implementation
func CalculatePolymarketProxyAddress(eoaAddress common.Address) common.Address {
	// Polymarket uses a specific pattern for proxy addresses
	// Based on analysis, they likely use:
	// 1. A deterministic salt based on the EOA
	// 2. CREATE2 for deployment
	// 3. A specific factory contract

	// Method 1: Simple deterministic pattern (most common)
	hasher := sha3.NewLegacyKeccak256()
	hasher.Write([]byte("POLYMARKET_PROXY"))
	hasher.Write(eoaAddress.Bytes())
	salt := hasher.Sum(nil)

	// Simplified CREATE2 calculation using the main registry
	data := []byte{0xff}
	data = append(data, PolymarketProxyRegistry.Bytes()...)
	data = append(data, salt...)

	// Approximate init code hash (you may need to adjust this)
	initCodeHash, _ := hex.DecodeString("6c9a6c4a39284e37ed1cf53d337577d14212a4870fb976a4366c693b939918d5")
	data = append(data, initCodeHash...)

	hash := crypto.Keccak256(data)
	return common.BytesToAddress(hash[12:])
}

// DebugProxyDetection provides detailed debugging information
func (r *DeterministicProxyResolver) DebugProxyDetection(ctx context.Context, eoaAddress common.Address) {
	fmt.Printf("=== Debugging Proxy Detection for EOA: %s ===\n", eoaAddress.Hex())

	// Check registry first (most reliable)
	registryProxy, err := r.GetProxyFromRegistry(ctx, eoaAddress)
	if err == nil {
		fmt.Printf("Registry returned proxy: %s\n", registryProxy.Hex())
		fmt.Printf("Has code at registry address: %v\n", r.isContract(ctx, registryProxy))
	} else {
		fmt.Printf("Registry error: %v\n", err)
	}

	// Check deterministic address
	deterministicProxy := r.calculateDeterministicProxy(eoaAddress)
	fmt.Printf("Deterministic proxy address: %s\n", deterministicProxy.Hex())
	fmt.Printf("Has code at deterministic address: %v\n", r.isContract(ctx, deterministicProxy))

	// Check various salt patterns with both factories
	salts := r.generateSalts(eoaAddress)
	factories := []struct {
		name    string
		address common.Address
	}{
		{"PolymarketRegistry", PolymarketProxyRegistry},
		{"AlternativeRegistry", AlternativeRegistry},
	}

	for _, factory := range factories {
		fmt.Printf("\nChecking %s (%s):\n", factory.name, factory.address.Hex())
		for i, salt := range salts[:3] { // Only show first 3 salt patterns for brevity
			proxyAddr := r.calculateCreate2Address(factory.address, salt, r.getInitCodeHash())
			hasCode := r.isContract(ctx, proxyAddr)
			fmt.Printf("  Salt pattern %d: %s -> %s (has code: %v)\n",
				i, hex.EncodeToString(salt[:8]), proxyAddr.Hex(), hasCode)
		}
	}

	fmt.Println("=== End Debug ===")
}