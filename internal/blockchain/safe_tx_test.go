package blockchain

import (
	"crypto/ecdsa"
	"math/big"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
)

func TestSafeExecTransactionABI(t *testing.T) {
	t.Parallel()

	// Verify the execTransaction ABI parses correctly
	parsed, err := abi.JSON(strings.NewReader(safeExecTransactionABI))
	if err != nil {
		t.Fatalf("failed to parse execTransaction ABI: %v", err)
	}

	method, ok := parsed.Methods["execTransaction"]
	if !ok {
		t.Fatal("execTransaction method not found in ABI")
	}

	// Should have 10 inputs
	if len(method.Inputs) != 10 {
		t.Errorf("execTransaction inputs = %d, want 10", len(method.Inputs))
	}
}

func TestSafeGetTransactionHashABI(t *testing.T) {
	t.Parallel()

	parsed, err := abi.JSON(strings.NewReader(safeGetTransactionHashABI))
	if err != nil {
		t.Fatalf("failed to parse getTransactionHash ABI: %v", err)
	}

	method, ok := parsed.Methods["getTransactionHash"]
	if !ok {
		t.Fatal("getTransactionHash method not found in ABI")
	}

	// Should have 10 inputs
	if len(method.Inputs) != 10 {
		t.Errorf("getTransactionHash inputs = %d, want 10", len(method.Inputs))
	}
}

func TestSafeNonceABI(t *testing.T) {
	t.Parallel()

	parsed, err := abi.JSON(strings.NewReader(safeNonceABI))
	if err != nil {
		t.Fatalf("failed to parse nonce ABI: %v", err)
	}

	_, ok := parsed.Methods["nonce"]
	if !ok {
		t.Fatal("nonce method not found in ABI")
	}
}

func TestPackExecTransaction(t *testing.T) {
	t.Parallel()

	parsed, err := abi.JSON(strings.NewReader(safeExecTransactionABI))
	if err != nil {
		t.Fatalf("failed to parse ABI: %v", err)
	}

	to := common.HexToAddress("0x4D97DCd97eC945f40cF65F87097ACe5EA0476045")
	value := big.NewInt(0)
	innerData := []byte{0x01, 0x02, 0x03, 0x04}
	operation := uint8(0)
	safeTxGas := big.NewInt(0)
	baseGas := big.NewInt(0)
	gasPrice := big.NewInt(0)
	gasToken := common.Address{}
	refundReceiver := common.Address{}
	signatures := []byte{0xAA, 0xBB}

	data, err := parsed.Pack("execTransaction",
		to, value, innerData, operation,
		safeTxGas, baseGas, gasPrice,
		gasToken, refundReceiver, signatures,
	)
	if err != nil {
		t.Fatalf("Pack execTransaction: %v", err)
	}

	// Should have at least 4 bytes (method selector) + params
	if len(data) < 4 {
		t.Fatal("packed data too short")
	}
}

func TestPackGetTransactionHash(t *testing.T) {
	t.Parallel()

	parsed, err := abi.JSON(strings.NewReader(safeGetTransactionHashABI))
	if err != nil {
		t.Fatalf("failed to parse ABI: %v", err)
	}

	to := common.HexToAddress("0x4D97DCd97eC945f40cF65F87097ACe5EA0476045")
	value := big.NewInt(0)
	innerData := []byte{0x01, 0x02}
	operation := uint8(0)
	safeTxGas := big.NewInt(0)
	baseGas := big.NewInt(0)
	gasPrice := big.NewInt(0)
	gasToken := common.Address{}
	refundReceiver := common.Address{}
	nonce := big.NewInt(5)

	data, err := parsed.Pack("getTransactionHash",
		to, value, innerData, operation,
		safeTxGas, baseGas, gasPrice,
		gasToken, refundReceiver, nonce,
	)
	if err != nil {
		t.Fatalf("Pack getTransactionHash: %v", err)
	}

	if len(data) < 4 {
		t.Fatal("packed data too short")
	}
}

func TestSignSafeTransactionHash(t *testing.T) {
	t.Parallel()

	// Generate a test key
	privateKey, err := crypto.GenerateKey()
	if err != nil {
		t.Fatalf("failed to generate key: %v", err)
	}

	// Sign a fake transaction hash
	txHash := common.HexToHash("0x1234567890abcdef1234567890abcdef1234567890abcdef1234567890abcdef")

	sig, err := signSafeHash(txHash, privateKey)
	if err != nil {
		t.Fatalf("signSafeHash() error = %v", err)
	}

	// Gnosis Safe expects 65-byte signatures (r=32, s=32, v=1)
	if len(sig) != 65 {
		t.Errorf("signature length = %d, want 65", len(sig))
	}

	// v should be 27 or 28 (Ethereum standard)
	v := sig[64]
	if v != 27 && v != 28 {
		t.Errorf("signature v = %d, want 27 or 28", v)
	}
}

func TestSignSafeHash_RecoversSigner(t *testing.T) {
	t.Parallel()

	privateKey, err := crypto.GenerateKey()
	if err != nil {
		t.Fatalf("failed to generate key: %v", err)
	}

	expectedAddr := crypto.PubkeyToAddress(*privateKey.Public().(*ecdsa.PublicKey))
	txHash := common.HexToHash("0xdeadbeef12345678deadbeef12345678deadbeef12345678deadbeef12345678")

	sig, err := signSafeHash(txHash, privateKey)
	if err != nil {
		t.Fatalf("signSafeHash() error = %v", err)
	}

	// Convert v back for ecrecover (expects 0 or 1)
	sigForRecover := make([]byte, 65)
	copy(sigForRecover, sig)
	sigForRecover[64] -= 27

	pubKey, err := crypto.SigToPub(txHash.Bytes(), sigForRecover)
	if err != nil {
		t.Fatalf("SigToPub error = %v", err)
	}

	recoveredAddr := crypto.PubkeyToAddress(*pubKey)
	if recoveredAddr != expectedAddr {
		t.Errorf("recovered address = %s, want %s", recoveredAddr.Hex(), expectedAddr.Hex())
	}
}
