package polymarket

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math/big"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/Catorpilor/poly/internal/blockchain"
	"github.com/Catorpilor/poly/internal/config"
)

// RelayerClient handles submitting Gnosis Safe transactions through Polymarket's Builder Relayer.
// The relayer executes transactions on-chain and pays gas — the user only signs a message.
type RelayerClient struct {
	relayerURL string
	apiKey     string
	secret     string
	passphrase string
	httpClient *http.Client
	chainID    *big.Int
}

// SafeTransactionRequest is the body for POST /submit
type SafeTransactionRequest struct {
	Type        string                 `json:"type"`
	From        string                 `json:"from"`
	To          string                 `json:"to"`
	ProxyWallet string                 `json:"proxyWallet"`
	Data        string                 `json:"data"`
	Nonce       string                 `json:"nonce"`
	Signature   string                 `json:"signature"`
	SignParams  SafeTransactionParams  `json:"signatureParams"`
	Metadata    string                 `json:"metadata,omitempty"`
}

// SafeTransactionParams holds the gas-related fields (all zeros — relayer handles gas)
type SafeTransactionParams struct {
	GasPrice       string `json:"gasPrice"`
	Operation      string `json:"operation"`
	SafeTxnGas     string `json:"safeTxnGas"`
	BaseGas        string `json:"baseGas"`
	GasToken       string `json:"gasToken"`
	RefundReceiver string `json:"refundReceiver"`
}

// SubmitResponse is the response from POST /submit
type SubmitResponse struct {
	TransactionID   string `json:"transactionID"`
	State           string `json:"state"`
	TransactionHash string `json:"transactionHash,omitempty"`
}

// TransactionStatus is the response from GET /transaction
type TransactionStatus struct {
	TransactionID   string `json:"transactionID"`
	TransactionHash string `json:"transactionHash"`
	State           string `json:"state"`
	From            string `json:"from"`
	To              string `json:"to"`
}

var zeroAddress = "0x0000000000000000000000000000000000000000"

// NewRelayerClient creates a new relayer client from Builder config.
func NewRelayerClient(cfg *config.BuilderConfig, chainID *big.Int) *RelayerClient {
	return &RelayerClient{
		relayerURL: strings.TrimRight(cfg.RelayerURL, "/"),
		apiKey:     cfg.APIKey,
		secret:     cfg.Secret,
		passphrase: cfg.Passphrase,
		httpClient: &http.Client{Timeout: 30 * time.Second},
		chainID:    chainID,
	}
}

// ExecSafeTransaction orchestrates the full relayer flow:
// get nonce → compute SafeTx hash → personal_sign → submit → wait for confirmation.
func (rc *RelayerClient) ExecSafeTransaction(
	ctx context.Context,
	eoaAddress, safeAddress, to common.Address,
	data []byte,
	privateKey *ecdsa.PrivateKey,
) (string, error) {
	// 1. Get Safe nonce from relayer
	log.Printf("Relayer: getting safe nonce for %s...", eoaAddress.Hex())
	nonce, err := rc.GetSafeNonce(ctx, eoaAddress)
	if err != nil {
		return "", fmt.Errorf("get safe nonce: %w", err)
	}
	log.Printf("Relayer: safe nonce = %d", nonce)

	// 2. Compute EIP-712 SafeTx hash (operation=0 for direct Call)
	safeTxHash := computeSafeTxHash(rc.chainID, safeAddress, to, data, 0, nonce)
	log.Printf("Relayer: safeTx hash = %s", safeTxHash.Hex())

	// 3. Sign with personal_sign + adjust v for Gnosis Safe
	signature, err := signSafeTransaction(safeTxHash, privateKey)
	if err != nil {
		return "", fmt.Errorf("sign safe tx: %w", err)
	}
	log.Printf("Relayer: signature ready (v=%d)", signature[64])

	// 4. Submit to relayer
	req := &SafeTransactionRequest{
		Type:        "SAFE",
		From:        eoaAddress.Hex(),
		To:          to.Hex(),
		ProxyWallet: safeAddress.Hex(),
		Data:        "0x" + hex.EncodeToString(data),
		Nonce:       strconv.FormatInt(nonce, 10),
		Signature:   "0x" + hex.EncodeToString(signature),
		SignParams: SafeTransactionParams{
			GasPrice:       "0",
			Operation:      "0",
			SafeTxnGas:     "0",
			BaseGas:        "0",
			GasToken:       zeroAddress,
			RefundReceiver: zeroAddress,
		},
		Metadata: "redeem positions",
	}

	log.Printf("Relayer: submitting transaction...")
	resp, err := rc.SubmitSafeTransaction(ctx, req)
	if err != nil {
		return "", fmt.Errorf("submit to relayer: %w", err)
	}
	log.Printf("Relayer: submitted, transactionID=%s, state=%s", resp.TransactionID, resp.State)

	// 5. Wait for confirmation
	log.Printf("Relayer: waiting for confirmation...")
	status, err := rc.WaitForConfirmation(ctx, resp.TransactionID, 3*time.Minute)
	if err != nil {
		return "", fmt.Errorf("wait for confirmation: %w", err)
	}

	log.Printf("Relayer: confirmed! state=%s, txHash=%s", status.State, status.TransactionHash)
	return status.TransactionHash, nil
}

// ExecMultiSendTransaction batches multiple calls into a single Safe transaction
// via the MultiSend contract (DelegateCall). All sub-transactions execute atomically.
func (rc *RelayerClient) ExecMultiSendTransaction(
	ctx context.Context,
	eoaAddress, safeAddress common.Address,
	txs []blockchain.MultiSendTx,
	privateKey *ecdsa.PrivateKey,
) (string, error) {
	// 1. Encode all sub-txs into multiSend calldata
	log.Printf("Relayer: encoding MultiSend with %d sub-transactions...", len(txs))
	multiSendData, err := blockchain.EncodeMultiSend(txs)
	if err != nil {
		return "", fmt.Errorf("encode multisend: %w", err)
	}
	log.Printf("Relayer: multiSend calldata length = %d", len(multiSendData))

	// 2. Get Safe nonce
	log.Printf("Relayer: getting safe nonce for %s...", eoaAddress.Hex())
	nonce, err := rc.GetSafeNonce(ctx, eoaAddress)
	if err != nil {
		return "", fmt.Errorf("get safe nonce: %w", err)
	}
	log.Printf("Relayer: safe nonce = %d", nonce)

	// 3. Compute EIP-712 SafeTx hash with operation=1 (DelegateCall) and to=MultiSend
	safeTxHash := computeSafeTxHash(rc.chainID, safeAddress, blockchain.MultiSendAddress, multiSendData, 1, nonce)
	log.Printf("Relayer: safeTx hash = %s (MultiSend, DelegateCall)", safeTxHash.Hex())

	// 4. Sign
	signature, err := signSafeTransaction(safeTxHash, privateKey)
	if err != nil {
		return "", fmt.Errorf("sign safe tx: %w", err)
	}
	log.Printf("Relayer: signature ready (v=%d)", signature[64])

	// 5. Submit to relayer with operation=1
	req := &SafeTransactionRequest{
		Type:        "SAFE",
		From:        eoaAddress.Hex(),
		To:          blockchain.MultiSendAddress.Hex(),
		ProxyWallet: safeAddress.Hex(),
		Data:        "0x" + hex.EncodeToString(multiSendData),
		Nonce:       strconv.FormatInt(nonce, 10),
		Signature:   "0x" + hex.EncodeToString(signature),
		SignParams: SafeTransactionParams{
			GasPrice:       "0",
			Operation:      "1", // DelegateCall for MultiSend
			SafeTxnGas:     "0",
			BaseGas:        "0",
			GasToken:       zeroAddress,
			RefundReceiver: zeroAddress,
		},
		Metadata: "redeem positions (multisend)",
	}

	log.Printf("Relayer: submitting MultiSend transaction...")
	resp, err := rc.SubmitSafeTransaction(ctx, req)
	if err != nil {
		return "", fmt.Errorf("submit to relayer: %w", err)
	}
	log.Printf("Relayer: submitted, transactionID=%s, state=%s", resp.TransactionID, resp.State)

	// 6. Wait for confirmation
	log.Printf("Relayer: waiting for confirmation...")
	status, err := rc.WaitForConfirmation(ctx, resp.TransactionID, 3*time.Minute)
	if err != nil {
		return "", fmt.Errorf("wait for confirmation: %w", err)
	}

	log.Printf("Relayer: confirmed! state=%s, txHash=%s", status.State, status.TransactionHash)
	return status.TransactionHash, nil
}

// GetSafeNonce fetches the current Safe nonce from the relayer.
func (rc *RelayerClient) GetSafeNonce(ctx context.Context, eoaAddress common.Address) (int64, error) {
	url := fmt.Sprintf("%s/nonce?address=%s&type=SAFE", rc.relayerURL, eoaAddress.Hex())

	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return 0, err
	}

	resp, err := rc.httpClient.Do(req)
	if err != nil {
		return 0, fmt.Errorf("relayer nonce request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return 0, fmt.Errorf("relayer nonce: status %d: %s", resp.StatusCode, string(body))
	}

	var result struct {
		Nonce json.Number `json:"nonce"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return 0, fmt.Errorf("decode nonce: %w", err)
	}

	nonce, err := result.Nonce.Int64()
	if err != nil {
		return 0, fmt.Errorf("parse nonce %q: %w", result.Nonce, err)
	}

	return nonce, nil
}

// SubmitSafeTransaction submits a signed Safe transaction to the relayer.
func (rc *RelayerClient) SubmitSafeTransaction(ctx context.Context, txReq *SafeTransactionRequest) (*SubmitResponse, error) {
	bodyBytes, err := json.Marshal(txReq)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}
	bodyStr := string(bodyBytes)

	timestamp := strconv.FormatInt(time.Now().Unix(), 10)
	signature := rc.signBuilderRequest(timestamp, "POST", "/submit", bodyStr)

	url := fmt.Sprintf("%s/submit", rc.relayerURL)
	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(bodyBytes))
	if err != nil {
		return nil, err
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("POLY_BUILDER_API_KEY", rc.apiKey)
	req.Header.Set("POLY_BUILDER_PASSPHRASE", rc.passphrase)
	req.Header.Set("POLY_BUILDER_TIMESTAMP", timestamp)
	req.Header.Set("POLY_BUILDER_SIGNATURE", signature)

	resp, err := rc.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("relayer submit request: %w", err)
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		return nil, fmt.Errorf("relayer submit: status %d: %s", resp.StatusCode, string(respBody))
	}

	var result SubmitResponse
	if err := json.Unmarshal(respBody, &result); err != nil {
		return nil, fmt.Errorf("decode submit response: %w", err)
	}

	return &result, nil
}

// GetTransactionStatus fetches the status of a submitted transaction.
func (rc *RelayerClient) GetTransactionStatus(ctx context.Context, transactionID string) (*TransactionStatus, error) {
	url := fmt.Sprintf("%s/transaction?id=%s", rc.relayerURL, transactionID)

	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, err
	}

	resp, err := rc.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("relayer transaction request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("relayer transaction: status %d: %s", resp.StatusCode, string(body))
	}

	// Response can be a single object or an array
	body, _ := io.ReadAll(resp.Body)

	// Try array first
	var statuses []TransactionStatus
	if err := json.Unmarshal(body, &statuses); err == nil && len(statuses) > 0 {
		return &statuses[0], nil
	}

	// Try single object
	var status TransactionStatus
	if err := json.Unmarshal(body, &status); err != nil {
		return nil, fmt.Errorf("decode transaction status: %w", err)
	}

	return &status, nil
}

// WaitForConfirmation polls the relayer until the transaction reaches a terminal state.
func (rc *RelayerClient) WaitForConfirmation(ctx context.Context, transactionID string, timeout time.Duration) (*TransactionStatus, error) {
	deadline := time.After(timeout)
	ticker := time.NewTicker(3 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-deadline:
			return nil, fmt.Errorf("relayer confirmation timeout after %v", timeout)
		case <-ticker.C:
			status, err := rc.GetTransactionStatus(ctx, transactionID)
			if err != nil {
				log.Printf("Relayer: poll error (will retry): %v", err)
				continue
			}

			switch status.State {
			case "STATE_CONFIRMED":
				return status, nil
			case "STATE_FAILED", "STATE_INVALID":
				return status, fmt.Errorf("transaction %s: %s", status.State, transactionID)
			default:
				// STATE_NEW, STATE_EXECUTED, STATE_MINED — keep polling
				log.Printf("Relayer: poll state=%s, txHash=%s", status.State, status.TransactionHash)
			}
		}
	}
}

// signBuilderRequest creates HMAC-SHA256 signature for Builder Relayer requests.
// Same algorithm as signL2Request in trading.go but with Builder headers.
func (rc *RelayerClient) signBuilderRequest(timestamp, method, path, body string) string {
	message := timestamp + method + path + body

	// Decode base64 secret (same strategy as trading.go:signL2Request)
	secretBytes, err := base64.StdEncoding.DecodeString(rc.secret)
	if err != nil {
		// Try with padding
		padded := rc.secret
		switch len(rc.secret) % 4 {
		case 2:
			padded += "=="
		case 3:
			padded += "="
		}
		secretBytes, err = base64.StdEncoding.DecodeString(padded)
		if err != nil {
			// Try URL-safe base64
			std := strings.ReplaceAll(rc.secret, "-", "+")
			std = strings.ReplaceAll(std, "_", "/")
			switch len(std) % 4 {
			case 2:
				std += "=="
			case 3:
				std += "="
			}
			secretBytes, err = base64.StdEncoding.DecodeString(std)
			if err != nil {
				secretBytes = []byte(rc.secret)
			}
		}
	}

	h := hmac.New(sha256.New, secretBytes)
	h.Write([]byte(message))

	sig := base64.URLEncoding.EncodeToString(h.Sum(nil))
	return sig
}

// computeSafeTxHash computes the EIP-712 typed data hash for a Gnosis Safe transaction.
// Domain: {chainId, verifyingContract: safeAddress}
// All gas fields are zero — the relayer handles gas.
func computeSafeTxHash(chainID *big.Int, safeAddress, to common.Address, data []byte, operation uint8, nonce int64) common.Hash {
	// EIP-712 domain separator
	// keccak256("EIP712Domain(uint256 chainId,address verifyingContract)")
	domainTypeHash := crypto.Keccak256Hash([]byte("EIP712Domain(uint256 chainId,address verifyingContract)"))
	domainSeparator := crypto.Keccak256Hash(
		domainTypeHash.Bytes(),
		common.LeftPadBytes(chainID.Bytes(), 32),
		common.LeftPadBytes(safeAddress.Bytes(), 32),
	)

	// SafeTx type hash
	// keccak256("SafeTx(address to,uint256 value,bytes data,uint8 operation,uint256 safeTxGas,uint256 baseGas,uint256 gasPrice,address gasToken,address refundReceiver,uint256 nonce)")
	safeTxTypeHash := crypto.Keccak256Hash([]byte("SafeTx(address to,uint256 value,bytes data,uint8 operation,uint256 safeTxGas,uint256 baseGas,uint256 gasPrice,address gasToken,address refundReceiver,uint256 nonce)"))

	// Hash the data field
	dataHash := crypto.Keccak256Hash(data)

	// Encode the SafeTx struct
	zero32 := make([]byte, 32)
	structHash := crypto.Keccak256Hash(
		safeTxTypeHash.Bytes(),
		common.LeftPadBytes(to.Bytes(), 32),                         // to
		zero32,                                                      // value = 0
		dataHash.Bytes(),                                            // keccak256(data)
		common.LeftPadBytes([]byte{operation}, 32),                  // operation (0=Call, 1=DelegateCall)
		zero32,                                     // safeTxGas = 0
		zero32,                                     // baseGas = 0
		zero32,                                     // gasPrice = 0
		common.LeftPadBytes(nil, 32),               // gasToken = address(0)
		common.LeftPadBytes(nil, 32),               // refundReceiver = address(0)
		common.LeftPadBytes(big.NewInt(nonce).Bytes(), 32), // nonce
	)

	// EIP-712 hash: keccak256("\x19\x01" + domainSeparator + structHash)
	return crypto.Keccak256Hash(
		[]byte{0x19, 0x01},
		domainSeparator.Bytes(),
		structHash.Bytes(),
	)
}

// personalSignHash computes the EIP-191 personal_sign hash:
// keccak256("\x19Ethereum Signed Message:\n32" + hash)
func personalSignHash(hash common.Hash) common.Hash {
	prefix := []byte("\x19Ethereum Signed Message:\n32")
	return crypto.Keccak256Hash(append(prefix, hash.Bytes()...))
}

// signSafeTransaction signs a Safe transaction hash using personal_sign with v adjusted for Gnosis Safe.
// Returns a 65-byte signature with v = 31 or 32 (Gnosis Safe eth_sign convention).
func signSafeTransaction(safeTxHash common.Hash, privateKey *ecdsa.PrivateKey) ([]byte, error) {
	// personal_sign: sign keccak256("\x19Ethereum Signed Message:\n32" + safeTxHash)
	msgHash := personalSignHash(safeTxHash)

	sig, err := crypto.Sign(msgHash.Bytes(), privateKey)
	if err != nil {
		return nil, err
	}

	if len(sig) != 65 {
		return nil, fmt.Errorf("unexpected signature length: %d", len(sig))
	}

	// Adjust v for Gnosis Safe eth_sign convention:
	// go-ethereum produces v=0/1, add 31 to get 31/32
	// (standard personal_sign would be 27/28, Gnosis Safe adds +4 → 31/32)
	sig[64] += 31

	return sig, nil
}
