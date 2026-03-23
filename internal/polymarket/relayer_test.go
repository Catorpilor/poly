package polymarket

import (
	"context"
	"crypto/ecdsa"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
)

func TestComputeSafeTxHash(t *testing.T) {
	t.Parallel()

	safeAddress := common.HexToAddress("0x2C21F8E2d2F613c76Ad96e45A29E4DBB43c46CE6")
	to := common.HexToAddress("0x4D97DCd97eC945f40cF65F87097ACe5EA0476045")
	data := []byte{0x01, 0x02, 0x03}
	nonce := int64(42)
	chainID := big.NewInt(137)

	hash := computeSafeTxHash(chainID, safeAddress, to, data, 0, nonce)

	// Should produce a non-zero 32-byte hash
	if hash == (common.Hash{}) {
		t.Fatal("hash is zero")
	}

	// Same inputs should produce same hash
	hash2 := computeSafeTxHash(chainID, safeAddress, to, data, 0, nonce)
	if hash != hash2 {
		t.Error("deterministic: same inputs should produce same hash")
	}

	// Different nonce should produce different hash
	hash3 := computeSafeTxHash(chainID, safeAddress, to, data, 0, 43)
	if hash == hash3 {
		t.Error("different nonce should produce different hash")
	}

	// Different operation should produce different hash
	hashDelegateCall := computeSafeTxHash(chainID, safeAddress, to, data, 1, nonce)
	if hash == hashDelegateCall {
		t.Error("different operation (Call vs DelegateCall) should produce different hash")
	}
}

func TestSignSafeTransaction(t *testing.T) {
	t.Parallel()

	privateKey, err := crypto.GenerateKey()
	if err != nil {
		t.Fatalf("failed to generate key: %v", err)
	}

	hash := common.HexToHash("0x1234567890abcdef1234567890abcdef1234567890abcdef1234567890abcdef")

	sig, err := signSafeTransaction(hash, privateKey)
	if err != nil {
		t.Fatalf("signSafeTransaction() error = %v", err)
	}

	// Should be 65 bytes
	if len(sig) != 65 {
		t.Errorf("signature length = %d, want 65", len(sig))
	}

	// v should be 31 or 32 (Gnosis Safe eth_sign convention: 27+4=31, 28+4=32)
	v := sig[64]
	if v != 31 && v != 32 {
		t.Errorf("signature v = %d, want 31 or 32", v)
	}
}

func TestSignSafeTransaction_RecoversSigner(t *testing.T) {
	t.Parallel()

	privateKey, err := crypto.GenerateKey()
	if err != nil {
		t.Fatalf("failed to generate key: %v", err)
	}
	expectedAddr := crypto.PubkeyToAddress(*privateKey.Public().(*ecdsa.PublicKey))

	safeTxHash := common.HexToHash("0xdeadbeef12345678deadbeef12345678deadbeef12345678deadbeef12345678")

	sig, err := signSafeTransaction(safeTxHash, privateKey)
	if err != nil {
		t.Fatalf("error = %v", err)
	}

	// Reconstruct the personal_sign hash that was actually signed
	personalHash := personalSignHash(safeTxHash)

	// Convert v back for ecrecover: 31→0, 32→1
	sigForRecover := make([]byte, 65)
	copy(sigForRecover, sig)
	sigForRecover[64] -= 31

	pubKey, err := crypto.SigToPub(personalHash.Bytes(), sigForRecover)
	if err != nil {
		t.Fatalf("SigToPub error = %v", err)
	}

	recoveredAddr := crypto.PubkeyToAddress(*pubKey)
	if recoveredAddr != expectedAddr {
		t.Errorf("recovered address = %s, want %s", recoveredAddr.Hex(), expectedAddr.Hex())
	}
}

func TestSignBuilderRequest(t *testing.T) {
	t.Parallel()

	rc := &RelayerClient{
		secret: "dGVzdC1zZWNyZXQ=", // base64("test-secret")
	}

	sig := rc.signBuilderRequest("1710000000", "POST", "/submit", `{"type":"SAFE"}`)

	// Should produce a non-empty base64 string
	if sig == "" {
		t.Fatal("signature is empty")
	}

	// Same inputs should produce same signature
	sig2 := rc.signBuilderRequest("1710000000", "POST", "/submit", `{"type":"SAFE"}`)
	if sig != sig2 {
		t.Error("deterministic: same inputs should produce same signature")
	}

	// Different timestamp should produce different signature
	sig3 := rc.signBuilderRequest("1710000001", "POST", "/submit", `{"type":"SAFE"}`)
	if sig == sig3 {
		t.Error("different timestamp should produce different signature")
	}
}

func TestGetSafeNonce(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/nonce" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		if r.URL.Query().Get("type") != "SAFE" {
			t.Errorf("expected type=SAFE, got %s", r.URL.Query().Get("type"))
		}
		if r.URL.Query().Get("address") == "" {
			t.Error("expected address param")
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"nonce": "42"})
	}))
	defer server.Close()

	rc := &RelayerClient{
		relayerURL: server.URL,
		httpClient: http.DefaultClient,
	}

	nonce, err := rc.GetSafeNonce(context.Background(), common.HexToAddress("0x1234"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if nonce != 42 {
		t.Errorf("nonce = %d, want 42", nonce)
	}
}

func TestSubmitSafeTransaction(t *testing.T) {
	t.Parallel()

	var receivedBody map[string]interface{}
	var receivedHeaders http.Header

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedHeaders = r.Header
		json.NewDecoder(r.Body).Decode(&receivedBody)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(SubmitResponse{
			TransactionID: "test-tx-id-123",
			State:         "STATE_NEW",
		})
	}))
	defer server.Close()

	rc := &RelayerClient{
		relayerURL: server.URL,
		apiKey:     "test-key",
		secret:     "dGVzdC1zZWNyZXQ=",
		passphrase: "test-pass",
		httpClient: http.DefaultClient,
	}

	resp, err := rc.SubmitSafeTransaction(context.Background(), &SafeTransactionRequest{
		Type:        "SAFE",
		From:        "0xEOA",
		To:          "0xTARGET",
		ProxyWallet: "0xSAFE",
		Data:        "0xCALLDATA",
		Nonce:       "42",
		Signature:   "0xSIG",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if resp.TransactionID != "test-tx-id-123" {
		t.Errorf("transactionID = %s, want test-tx-id-123", resp.TransactionID)
	}

	// Verify Builder auth headers were set
	if receivedHeaders.Get("POLY_BUILDER_API_KEY") != "test-key" {
		t.Errorf("missing or wrong POLY_BUILDER_API_KEY header")
	}
	if receivedHeaders.Get("POLY_BUILDER_PASSPHRASE") != "test-pass" {
		t.Errorf("missing or wrong POLY_BUILDER_PASSPHRASE header")
	}
	if receivedHeaders.Get("POLY_BUILDER_TIMESTAMP") == "" {
		t.Error("missing POLY_BUILDER_TIMESTAMP header")
	}
	if receivedHeaders.Get("POLY_BUILDER_SIGNATURE") == "" {
		t.Error("missing POLY_BUILDER_SIGNATURE header")
	}

	// Verify body fields
	if receivedBody["type"] != "SAFE" {
		t.Errorf("body type = %v, want SAFE", receivedBody["type"])
	}
	if receivedBody["nonce"] != "42" {
		t.Errorf("body nonce = %v, want 42", receivedBody["nonce"])
	}
}

func TestGetTransactionStatus(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("id") != "test-id" {
			t.Errorf("expected id=test-id, got %s", r.URL.Query().Get("id"))
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode([]TransactionStatus{{
			TransactionID:   "test-id",
			TransactionHash: "0xabc123",
			State:           "STATE_CONFIRMED",
		}})
	}))
	defer server.Close()

	rc := &RelayerClient{
		relayerURL: server.URL,
		httpClient: http.DefaultClient,
	}

	status, err := rc.GetTransactionStatus(context.Background(), "test-id")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if status.State != "STATE_CONFIRMED" {
		t.Errorf("state = %s, want STATE_CONFIRMED", status.State)
	}
	if status.TransactionHash != "0xabc123" {
		t.Errorf("txHash = %s, want 0xabc123", status.TransactionHash)
	}
}
