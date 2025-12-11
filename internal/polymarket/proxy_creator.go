package polymarket

import (
	"context"
	"crypto/ecdsa"
	"fmt"
	"math/big"
	"time"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethclient"
)

// ProxyCreator handles creation of new proxy wallets
type ProxyCreator struct {
	client          *ethclient.Client
	registryAddress common.Address
	chainID         *big.Int
}

// NewProxyCreator creates a new proxy creator instance
func NewProxyCreator(client *ethclient.Client) (*ProxyCreator, error) {
	chainID, err := client.ChainID(context.Background())
	if err != nil {
		return nil, fmt.Errorf("failed to get chain ID: %w", err)
	}

	return &ProxyCreator{
		client:          client,
		registryAddress: PolymarketProxyRegistry, // 0xaacfeea03eb1561c4e67d661e40682bd20e3541b
		chainID:         chainID,
	}, nil
}

// CreateProxyIfNeeded checks if a proxy exists and creates one if it doesn't
func (pc *ProxyCreator) CreateProxyIfNeeded(ctx context.Context, privateKey *ecdsa.PrivateKey) (common.Address, bool, error) {
	// Derive EOA address from private key
	publicKey := privateKey.Public()
	publicKeyECDSA, ok := publicKey.(*ecdsa.PublicKey)
	if !ok {
		return common.Address{}, false, fmt.Errorf("error casting public key to ECDSA")
	}
	eoaAddress := crypto.PubkeyToAddress(*publicKeyECDSA)

	// First, check if proxy already exists
	resolver := NewDeterministicProxyResolver(pc.client)
	existingProxy, err := resolver.GetProxyFromRegistry(ctx, eoaAddress)
	if err == nil && existingProxy != (common.Address{}) {
		// Proxy already exists
		return existingProxy, false, nil
	}

	// Proxy doesn't exist, create it
	fmt.Printf("No proxy found for EOA %s, creating new proxy...\n", eoaAddress.Hex())

	// Create the proxy
	proxyAddress, err := pc.CreateProxy(ctx, privateKey)
	if err != nil {
		return common.Address{}, false, fmt.Errorf("failed to create proxy: %w", err)
	}

	return proxyAddress, true, nil
}

// CreateProxy creates a new proxy wallet for the given EOA
func (pc *ProxyCreator) CreateProxy(ctx context.Context, privateKey *ecdsa.PrivateKey) (common.Address, error) {
	// Derive EOA address
	publicKey := privateKey.Public()
	publicKeyECDSA, ok := publicKey.(*ecdsa.PublicKey)
	if !ok {
		return common.Address{}, fmt.Errorf("error casting public key to ECDSA")
	}
	eoaAddress := crypto.PubkeyToAddress(*publicKeyECDSA)

	// Prepare the signature for proxy creation
	// The message to sign likely includes the EOA address and possibly a nonce
	signature, err := pc.createProxySignature(privateKey, eoaAddress)
	if err != nil {
		return common.Address{}, fmt.Errorf("failed to create signature: %w", err)
	}

	// Prepare the transaction data
	// Function selector for createProxy(address,uint256,address,(uint8,bytes32,bytes32))
	methodID := crypto.Keccak256([]byte("createProxy(address,uint256,address,(uint8,bytes32,bytes32))"))[:4]

	// Prepare parameters
	// For free proxy creation, we use zero values for payment
	paymentToken := common.Address{}     // 0x0 for no payment
	payment := big.NewInt(0)             // 0 payment
	paymentReceiver := common.Address{}  // 0x0 for no receiver

	// Encode the parameters
	data, err := pc.encodeCreateProxyData(methodID, paymentToken, payment, paymentReceiver, signature)
	if err != nil {
		return common.Address{}, fmt.Errorf("failed to encode data: %w", err)
	}

	// Get gas price
	gasPrice, err := pc.client.SuggestGasPrice(ctx)
	if err != nil {
		return common.Address{}, fmt.Errorf("failed to get gas price: %w", err)
	}

	// Estimate gas
	msg := ethereum.CallMsg{
		From:  eoaAddress,
		To:    &pc.registryAddress,
		Value: big.NewInt(0),
		Data:  data,
	}

	gasLimit, err := pc.client.EstimateGas(ctx, msg)
	if err != nil {
		// If estimation fails, use a default high gas limit
		gasLimit = uint64(500000)
		fmt.Printf("Gas estimation failed, using default: %d\n", gasLimit)
	}

	// Get nonce
	nonce, err := pc.client.PendingNonceAt(ctx, eoaAddress)
	if err != nil {
		return common.Address{}, fmt.Errorf("failed to get nonce: %w", err)
	}

	// Create the transaction
	tx := types.NewTransaction(
		nonce,
		pc.registryAddress,
		big.NewInt(0), // No ETH value
		gasLimit,
		gasPrice,
		data,
	)

	// Sign the transaction
	signedTx, err := types.SignTx(tx, types.NewEIP155Signer(pc.chainID), privateKey)
	if err != nil {
		return common.Address{}, fmt.Errorf("failed to sign transaction: %w", err)
	}

	// Send the transaction
	err = pc.client.SendTransaction(ctx, signedTx)
	if err != nil {
		return common.Address{}, fmt.Errorf("failed to send transaction: %w", err)
	}

	fmt.Printf("Proxy creation transaction sent: %s\n", signedTx.Hash().Hex())

	// Wait for transaction confirmation
	receipt, err := pc.waitForTransaction(ctx, signedTx.Hash())
	if err != nil {
		return common.Address{}, fmt.Errorf("transaction failed: %w", err)
	}

	if receipt.Status != 1 {
		return common.Address{}, fmt.Errorf("transaction reverted")
	}

	fmt.Printf("Proxy created successfully!\n")

	// Verify the proxy was created by querying it again
	resolver := NewDeterministicProxyResolver(pc.client)
	createdProxy, err := resolver.GetProxyFromRegistry(ctx, eoaAddress)
	if err != nil {
		return common.Address{}, fmt.Errorf("failed to verify proxy creation: %w", err)
	}

	return createdProxy, nil
}

// createProxySignature creates the signature required for proxy creation
func (pc *ProxyCreator) createProxySignature(privateKey *ecdsa.PrivateKey, eoaAddress common.Address) (Signature, error) {
	// The exact message format depends on Polymarket's implementation
	// Common patterns include:
	// 1. EIP-712 typed data
	// 2. Simple message hash
	// 3. Packed encoding of parameters

	// For now, we'll try a simple approach - signing the EOA address
	// This might need adjustment based on actual requirements
	message := crypto.Keccak256(
		[]byte("CREATE_PROXY"),
		eoaAddress.Bytes(),
	)

	// Sign the message
	sig, err := crypto.Sign(message, privateKey)
	if err != nil {
		return Signature{}, fmt.Errorf("failed to sign message: %w", err)
	}

	// Split signature into v, r, s components
	if len(sig) != 65 {
		return Signature{}, fmt.Errorf("invalid signature length")
	}

	return Signature{
		V: sig[64] + 27, // Ethereum signature standard adds 27 to v
		R: [32]byte(sig[0:32]),
		S: [32]byte(sig[32:64]),
	}, nil
}

// Signature represents the ECDSA signature components
type Signature struct {
	V uint8
	R [32]byte
	S [32]byte
}

// encodeCreateProxyData encodes the parameters for createProxy function
func (pc *ProxyCreator) encodeCreateProxyData(methodID []byte, paymentToken common.Address, payment *big.Int, paymentReceiver common.Address, sig Signature) ([]byte, error) {
	// Start with method ID
	data := methodID

	// Encode parameters according to ABI encoding rules
	// address paymentToken (32 bytes)
	data = append(data, common.LeftPadBytes(paymentToken.Bytes(), 32)...)

	// uint256 payment (32 bytes)
	data = append(data, common.LeftPadBytes(payment.Bytes(), 32)...)

	// address paymentReceiver (32 bytes)
	data = append(data, common.LeftPadBytes(paymentReceiver.Bytes(), 32)...)

	// Sig struct (v, r, s) - tuple encoding
	// For a struct, we encode each field in order
	// uint8 v (32 bytes, padded)
	data = append(data, common.LeftPadBytes([]byte{sig.V}, 32)...)

	// bytes32 r (32 bytes)
	data = append(data, sig.R[:]...)

	// bytes32 s (32 bytes)
	data = append(data, sig.S[:]...)

	return data, nil
}

// waitForTransaction waits for a transaction to be mined
func (pc *ProxyCreator) waitForTransaction(ctx context.Context, txHash common.Hash) (*types.Receipt, error) {
	// Wait up to 2 minutes for confirmation
	timeout := time.After(2 * time.Minute)
	ticker := time.NewTicker(3 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-timeout:
			return nil, fmt.Errorf("transaction timeout")
		case <-ticker.C:
			receipt, err := pc.client.TransactionReceipt(ctx, txHash)
			if err == nil {
				return receipt, nil
			}
			// Continue waiting if receipt not found
		}
	}
}

// GetOrCreateProxy ensures a proxy exists for the given EOA, creating one if needed
func GetOrCreateProxy(ctx context.Context, client *ethclient.Client, privateKey *ecdsa.PrivateKey) (common.Address, error) {
	creator, err := NewProxyCreator(client)
	if err != nil {
		return common.Address{}, err
	}

	proxy, created, err := creator.CreateProxyIfNeeded(ctx, privateKey)
	if err != nil {
		return common.Address{}, err
	}

	if created {
		fmt.Printf("New proxy created: %s\n", proxy.Hex())
	} else {
		fmt.Printf("Using existing proxy: %s\n", proxy.Hex())
	}

	return proxy, nil
}