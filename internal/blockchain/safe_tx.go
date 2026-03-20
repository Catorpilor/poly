package blockchain

import (
	"context"
	"crypto/ecdsa"
	"fmt"
	"log"
	"math/big"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethclient"
)

// Gnosis Safe 1.3.0 ABI fragments

const safeExecTransactionABI = `[{
	"name": "execTransaction",
	"type": "function",
	"inputs": [
		{"name": "to", "type": "address"},
		{"name": "value", "type": "uint256"},
		{"name": "data", "type": "bytes"},
		{"name": "operation", "type": "uint8"},
		{"name": "safeTxGas", "type": "uint256"},
		{"name": "baseGas", "type": "uint256"},
		{"name": "gasPrice", "type": "uint256"},
		{"name": "gasToken", "type": "address"},
		{"name": "refundReceiver", "type": "address"},
		{"name": "signatures", "type": "bytes"}
	],
	"outputs": [
		{"name": "success", "type": "bool"}
	]
}]`

const safeGetTransactionHashABI = `[{
	"name": "getTransactionHash",
	"type": "function",
	"inputs": [
		{"name": "to", "type": "address"},
		{"name": "value", "type": "uint256"},
		{"name": "data", "type": "bytes"},
		{"name": "operation", "type": "uint8"},
		{"name": "safeTxGas", "type": "uint256"},
		{"name": "baseGas", "type": "uint256"},
		{"name": "gasPrice", "type": "uint256"},
		{"name": "gasToken", "type": "address"},
		{"name": "refundReceiver", "type": "address"},
		{"name": "_nonce", "type": "uint256"}
	],
	"outputs": [
		{"name": "", "type": "bytes32"}
	]
}]`

const safeNonceABI = `[{
	"name": "nonce",
	"type": "function",
	"inputs": [],
	"outputs": [
		{"name": "", "type": "uint256"}
	]
}]`

// SafeTransactionExecutor handles sending transactions through a Gnosis Safe (1-of-1 multisig).
type SafeTransactionExecutor struct {
	client  *ethclient.Client
	chainID *big.Int
}

// NewSafeTransactionExecutor creates a new executor for the given client and chain.
func NewSafeTransactionExecutor(client *ethclient.Client, chainID *big.Int) *SafeTransactionExecutor {
	return &SafeTransactionExecutor{
		client:  client,
		chainID: chainID,
	}
}

// ExecTransaction sends an arbitrary call through a Gnosis Safe.
// The EOA (privateKey owner) must be the sole owner of the Safe.
// Returns the transaction hash of the outer (EOA → Safe) transaction.
func (s *SafeTransactionExecutor) ExecTransaction(
	ctx context.Context,
	safeAddress, to common.Address,
	value *big.Int,
	data []byte,
	privateKey *ecdsa.PrivateKey,
) (common.Hash, error) {
	eoaAddress := crypto.PubkeyToAddress(*privateKey.Public().(*ecdsa.PublicKey))

	// 1. Get Safe nonce
	log.Printf("SafeTx: getting nonce for safe %s...", safeAddress.Hex())
	safeNonce, err := s.getSafeNonce(ctx, safeAddress)
	if err != nil {
		return common.Hash{}, fmt.Errorf("get safe nonce: %w", err)
	}
	log.Printf("SafeTx: safe nonce = %s", safeNonce.String())

	// 2. Get Safe transaction hash (what the owner must sign)
	log.Printf("SafeTx: computing transaction hash...")
	txHash, err := s.getTransactionHash(ctx, safeAddress, to, value, data, safeNonce)
	if err != nil {
		return common.Hash{}, fmt.Errorf("get transaction hash: %w", err)
	}
	log.Printf("SafeTx: tx hash = %s", txHash.Hex())

	// 3. EOA signs the Safe transaction hash
	signature, err := signSafeHash(txHash, privateKey)
	if err != nil {
		return common.Hash{}, fmt.Errorf("sign safe hash: %w", err)
	}
	log.Printf("SafeTx: signature ready (v=%d)", signature[64])

	// 4. Encode the execTransaction call
	execData, err := packExecTransaction(to, value, data, signature)
	if err != nil {
		return common.Hash{}, fmt.Errorf("pack execTransaction: %w", err)
	}

	// 5. Get gas parameters (EIP-1559)
	log.Printf("SafeTx: fetching gas parameters...")
	head, err := s.client.HeaderByNumber(ctx, nil)
	if err != nil {
		return common.Hash{}, fmt.Errorf("get latest header: %w", err)
	}
	baseFee := head.BaseFee
	log.Printf("SafeTx: base fee = %s", baseFee.String())

	// Max priority fee (tip) — use suggested or fallback to 30 Gwei
	gasTipCap, err := s.client.SuggestGasTipCap(ctx)
	if err != nil {
		gasTipCap = new(big.Int).Mul(big.NewInt(30), big.NewInt(1e9)) // 30 Gwei
		log.Printf("SafeTx: tip cap suggestion failed, using 30 Gwei")
	}
	log.Printf("SafeTx: tip cap = %s", gasTipCap.String())

	// Max fee = 2 * baseFee + tip (ensures inclusion even if base fee rises)
	gasFeeCap := new(big.Int).Add(
		new(big.Int).Mul(baseFee, big.NewInt(2)),
		gasTipCap,
	)
	log.Printf("SafeTx: fee cap = %s", gasFeeCap.String())

	msg := ethereum.CallMsg{
		From:  eoaAddress,
		To:    &safeAddress,
		Value: big.NewInt(0),
		Data:  execData,
	}

	log.Printf("SafeTx: estimating gas...")
	gasLimit, err := s.client.EstimateGas(ctx, msg)
	if err != nil {
		log.Printf("SafeTx: gas estimation failed (%v), using default 500000", err)
		gasLimit = uint64(500000)
	}
	// Add 20% buffer for safety
	gasLimit = gasLimit * 120 / 100
	log.Printf("SafeTx: gas limit = %d", gasLimit)

	// 6. Get EOA nonce and build EIP-1559 transaction
	log.Printf("SafeTx: getting EOA nonce for %s...", eoaAddress.Hex())
	nonce, err := s.client.PendingNonceAt(ctx, eoaAddress)
	if err != nil {
		return common.Hash{}, fmt.Errorf("get EOA nonce: %w", err)
	}
	log.Printf("SafeTx: EOA nonce = %d", nonce)

	tx := types.NewTx(&types.DynamicFeeTx{
		ChainID:   s.chainID,
		Nonce:     nonce,
		GasTipCap: gasTipCap,
		GasFeeCap: gasFeeCap,
		Gas:       gasLimit,
		To:        &safeAddress,
		Value:     big.NewInt(0),
		Data:      execData,
	})
	signedTx, err := types.SignTx(tx, types.LatestSignerForChainID(s.chainID), privateKey)
	if err != nil {
		return common.Hash{}, fmt.Errorf("sign transaction: %w", err)
	}

	// 7. Send and wait for receipt
	log.Printf("SafeTx: sending transaction %s...", signedTx.Hash().Hex())
	err = s.client.SendTransaction(ctx, signedTx)
	if err != nil {
		return common.Hash{}, fmt.Errorf("send transaction: %w", err)
	}
	log.Printf("SafeTx: tx sent, waiting for receipt...")

	receipt, err := WaitForReceipt(ctx, s.client, signedTx.Hash(), 2*time.Minute)
	if err != nil {
		return signedTx.Hash(), fmt.Errorf("wait for receipt: %w", err)
	}

	if receipt.Status != 1 {
		return signedTx.Hash(), fmt.Errorf("transaction reverted (tx: %s)", signedTx.Hash().Hex())
	}

	log.Printf("SafeTx: confirmed! status=%d gasUsed=%d", receipt.Status, receipt.GasUsed)
	return signedTx.Hash(), nil
}

// getSafeNonce calls nonce() on the Safe to get the current Safe transaction nonce.
func (s *SafeTransactionExecutor) getSafeNonce(ctx context.Context, safeAddress common.Address) (*big.Int, error) {
	parsed, err := abi.JSON(strings.NewReader(safeNonceABI))
	if err != nil {
		return nil, err
	}

	data, err := parsed.Pack("nonce")
	if err != nil {
		return nil, err
	}

	result, err := s.client.CallContract(ctx, ethereum.CallMsg{
		To:   &safeAddress,
		Data: data,
	}, nil)
	if err != nil {
		return nil, fmt.Errorf("call nonce(): %w", err)
	}

	var nonce *big.Int
	err = parsed.UnpackIntoInterface(&nonce, "nonce", result)
	if err != nil {
		return nil, fmt.Errorf("unpack nonce: %w", err)
	}

	return nonce, nil
}

// getTransactionHash calls getTransactionHash() on the Safe to compute the EIP-712 hash
// that the owner must sign.
func (s *SafeTransactionExecutor) getTransactionHash(
	ctx context.Context,
	safeAddress, to common.Address,
	value *big.Int,
	data []byte,
	nonce *big.Int,
) (common.Hash, error) {
	parsed, err := abi.JSON(strings.NewReader(safeGetTransactionHashABI))
	if err != nil {
		return common.Hash{}, err
	}

	calldata, err := parsed.Pack("getTransactionHash",
		to,
		value,
		data,
		uint8(0),        // operation: CALL
		big.NewInt(0),   // safeTxGas
		big.NewInt(0),   // baseGas
		big.NewInt(0),   // gasPrice
		common.Address{}, // gasToken
		common.Address{}, // refundReceiver
		nonce,
	)
	if err != nil {
		return common.Hash{}, err
	}

	result, err := s.client.CallContract(ctx, ethereum.CallMsg{
		To:   &safeAddress,
		Data: calldata,
	}, nil)
	if err != nil {
		return common.Hash{}, fmt.Errorf("call getTransactionHash(): %w", err)
	}

	var hash [32]byte
	err = parsed.UnpackIntoInterface(&hash, "getTransactionHash", result)
	if err != nil {
		return common.Hash{}, fmt.Errorf("unpack hash: %w", err)
	}

	return hash, nil
}

// signSafeHash signs a Safe transaction hash with the EOA's private key.
// Returns a 65-byte signature (r[32] + s[32] + v[1]) with v adjusted to 27/28.
func signSafeHash(hash common.Hash, privateKey *ecdsa.PrivateKey) ([]byte, error) {
	sig, err := crypto.Sign(hash.Bytes(), privateKey)
	if err != nil {
		return nil, err
	}

	if len(sig) != 65 {
		return nil, fmt.Errorf("unexpected signature length: %d", len(sig))
	}

	// go-ethereum produces v=0/1, but Gnosis Safe expects v=27/28
	sig[64] += 27

	return sig, nil
}

// packExecTransaction encodes the execTransaction call with the provided signature.
func packExecTransaction(to common.Address, value *big.Int, data []byte, signature []byte) ([]byte, error) {
	parsed, err := abi.JSON(strings.NewReader(safeExecTransactionABI))
	if err != nil {
		return nil, err
	}

	return parsed.Pack("execTransaction",
		to,
		value,
		data,
		uint8(0),         // operation: CALL
		big.NewInt(0),    // safeTxGas
		big.NewInt(0),    // baseGas
		big.NewInt(0),    // gasPrice
		common.Address{}, // gasToken
		common.Address{}, // refundReceiver
		signature,
	)
}

// WaitForReceipt polls for a transaction receipt until it's mined or the timeout expires.
func WaitForReceipt(ctx context.Context, client *ethclient.Client, txHash common.Hash, timeout time.Duration) (*types.Receipt, error) {
	deadline := time.After(timeout)
	ticker := time.NewTicker(3 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-deadline:
			return nil, fmt.Errorf("transaction timeout after %v", timeout)
		case <-ticker.C:
			receipt, err := client.TransactionReceipt(ctx, txHash)
			if err == nil {
				return receipt, nil
			}
		}
	}
}
