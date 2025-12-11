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
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/polymarket/go-order-utils/pkg/builder"
	"github.com/polymarket/go-order-utils/pkg/model"
)

// TradingClient handles order creation and submission to Polymarket CLOB
type TradingClient struct {
	clobURL    string
	chainID    *big.Int
	httpClient *http.Client
	builder    builder.ExchangeOrderBuilder
}

// APICredentials holds L2 API credentials for authenticated requests
type APICredentials struct {
	APIKey     string
	Secret     string
	Passphrase string
}

// OrderType represents the type of order (GTC, GTD, FOK)
type OrderType string

const (
	OrderTypeGTC OrderType = "GTC" // Good-til-cancelled
	OrderTypeGTD OrderType = "GTD" // Good-til-date
	OrderTypeFOK OrderType = "FOK" // Fill-or-kill
)

// TradeRequest represents a trade request from the user
type TradeRequest struct {
	MarketID     string  // Gamma market ID
	TokenID      string  // ERC1155 token ID for the outcome
	Side         string  // "BUY" or "SELL"
	Outcome      string  // "YES" or "NO"
	Amount       float64 // Amount in USDC (for BUY) or value estimate (for SELL)
	SharesRaw    int64   // Raw shares with 6 decimals (for SELL orders, takes precedence over Amount)
	Price        float64 // Price per share (0-1)
	OrderType    OrderType
	Expiration   int64 // Unix timestamp for GTD orders
	NegativeRisk bool  // Whether this is a negative risk market
}

// TradeResult represents the result of a trade
type TradeResult struct {
	Success     bool
	OrderID     string
	OrderHash   string
	ErrorMsg    string
	FilledSize  float64
	AveragePrice float64
}

// OrderBookEntry represents an entry in the order book
type OrderBookEntry struct {
	Price float64 `json:"price,string"`
	Size  float64 `json:"size,string"`
}

// OrderBook represents the order book for a market
type OrderBook struct {
	Bids []OrderBookEntry `json:"bids"`
	Asks []OrderBookEntry `json:"asks"`
}

// NewTradingClient creates a new trading client
func NewTradingClient(clobURL string, chainID int64) *TradingClient {
	return &TradingClient{
		clobURL:    clobURL,
		chainID:    big.NewInt(chainID),
		httpClient: &http.Client{Timeout: 30 * time.Second},
		builder:    builder.NewExchangeOrderBuilderImpl(big.NewInt(chainID), nil),
	}
}

// GetOrCreateAPICredentials gets existing or creates new API credentials
func (tc *TradingClient) GetOrCreateAPICredentials(ctx context.Context, privateKey *ecdsa.PrivateKey) (*APICredentials, error) {
	address := crypto.PubkeyToAddress(privateKey.PublicKey)
	log.Printf("Getting API credentials for address: %s", address.Hex())

	// First, try to derive existing credentials
	creds, err := tc.deriveAPICredentials(ctx, privateKey, 0)
	if err == nil && creds != nil {
		// Check if secret contains URL-safe characters
		hasURLSafe := strings.ContainsAny(creds.Secret, "-_")
		hasStandard := strings.ContainsAny(creds.Secret, "+/")
		log.Printf("Derived existing API credentials: %s", creds.APIKey)
		log.Printf("Secret: len=%d, hasURLSafe=%v, hasStandard=%v, prefix: %s...",
			len(creds.Secret), hasURLSafe, hasStandard, creds.Secret[:min(12, len(creds.Secret))])
		return creds, nil
	}
	log.Printf("Failed to derive credentials: %v, creating new ones", err)

	// If that fails, create new credentials
	return tc.createAPICredentials(ctx, privateKey)
}

// createAPICredentials creates new L2 API credentials using L1 authentication
func (tc *TradingClient) createAPICredentials(ctx context.Context, privateKey *ecdsa.PrivateKey) (*APICredentials, error) {
	timestamp := time.Now().Unix()
	nonce := int64(0)

	// Sign the L1 auth message
	signature, err := tc.signL1Auth(privateKey, timestamp, nonce)
	if err != nil {
		return nil, fmt.Errorf("failed to sign L1 auth: %w", err)
	}

	address := crypto.PubkeyToAddress(privateKey.PublicKey)

	// Create API key request
	url := fmt.Sprintf("%s/auth/api-key", tc.clobURL)
	req, err := http.NewRequestWithContext(ctx, "POST", url, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	// Set L1 headers
	req.Header.Set("POLY_ADDRESS", address.Hex())
	req.Header.Set("POLY_SIGNATURE", signature)
	req.Header.Set("POLY_TIMESTAMP", strconv.FormatInt(timestamp, 10))
	req.Header.Set("POLY_NONCE", strconv.FormatInt(nonce, 10))
	req.Header.Set("Content-Type", "application/json")

	log.Printf("Creating API credentials for address: %s", address.Hex())

	resp, err := tc.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to create API key: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("API key creation failed: %s - %s", resp.Status, string(body))
	}

	var result struct {
		APIKey     string `json:"apiKey"`
		Secret     string `json:"secret"`
		Passphrase string `json:"passphrase"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("failed to decode API credentials: %w", err)
	}

	log.Printf("API credentials created - APIKey: %s, Secret len: %d", result.APIKey, len(result.Secret))

	return &APICredentials{
		APIKey:     result.APIKey,
		Secret:     result.Secret,
		Passphrase: result.Passphrase,
	}, nil
}

// deriveAPICredentials derives existing API credentials
func (tc *TradingClient) deriveAPICredentials(ctx context.Context, privateKey *ecdsa.PrivateKey, nonce int64) (*APICredentials, error) {
	timestamp := time.Now().Unix()

	signature, err := tc.signL1Auth(privateKey, timestamp, nonce)
	if err != nil {
		return nil, fmt.Errorf("failed to sign L1 auth: %w", err)
	}

	address := crypto.PubkeyToAddress(privateKey.PublicKey)

	url := fmt.Sprintf("%s/auth/derive-api-key", tc.clobURL)
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	req.Header.Set("POLY_ADDRESS", address.Hex())
	req.Header.Set("POLY_SIGNATURE", signature)
	req.Header.Set("POLY_TIMESTAMP", strconv.FormatInt(timestamp, 10))
	req.Header.Set("POLY_NONCE", strconv.FormatInt(nonce, 10))

	resp, err := tc.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to derive API key: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("derive API key failed: %s", resp.Status)
	}

	var result struct {
		APIKey     string `json:"apiKey"`
		Secret     string `json:"secret"`
		Passphrase string `json:"passphrase"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("failed to decode API credentials: %w", err)
	}

	return &APICredentials{
		APIKey:     result.APIKey,
		Secret:     result.Secret,
		Passphrase: result.Passphrase,
	}, nil
}

// signL1Auth creates an EIP-712 signature for L1 authentication
func (tc *TradingClient) signL1Auth(privateKey *ecdsa.PrivateKey, timestamp, nonce int64) (string, error) {
	address := crypto.PubkeyToAddress(privateKey.PublicKey)

	// Build EIP-712 typed data for ClobAuth
	// Domain: { name: "ClobAuthDomain", version: "1", chainId: 137 }
	domainSeparator := tc.buildAuthDomainSeparator()

	// Message structure hash
	message := "This message attests that I control the given wallet"
	messageHash := tc.buildAuthMessageHash(address, timestamp, nonce, message)

	// Final hash to sign
	finalHash := crypto.Keccak256Hash(
		[]byte("\x19\x01"),
		domainSeparator.Bytes(),
		messageHash.Bytes(),
	)

	// Sign the hash
	sig, err := crypto.Sign(finalHash.Bytes(), privateKey)
	if err != nil {
		return "", fmt.Errorf("failed to sign: %w", err)
	}

	// Adjust v value for Ethereum signature standard
	if sig[64] < 27 {
		sig[64] += 27
	}

	return "0x" + hex.EncodeToString(sig), nil
}

// buildAuthDomainSeparator builds the EIP-712 domain separator for auth
func (tc *TradingClient) buildAuthDomainSeparator() common.Hash {
	// keccak256("EIP712Domain(string name,string version,uint256 chainId)")
	typeHash := crypto.Keccak256Hash([]byte("EIP712Domain(string name,string version,uint256 chainId)"))
	nameHash := crypto.Keccak256Hash([]byte("ClobAuthDomain"))
	versionHash := crypto.Keccak256Hash([]byte("1"))

	chainIDBytes := common.LeftPadBytes(tc.chainID.Bytes(), 32)

	return crypto.Keccak256Hash(
		typeHash.Bytes(),
		nameHash.Bytes(),
		versionHash.Bytes(),
		chainIDBytes,
	)
}

// buildAuthMessageHash builds the message hash for auth
func (tc *TradingClient) buildAuthMessageHash(address common.Address, timestamp, nonce int64, message string) common.Hash {
	// ClobAuth type: { address: address, timestamp: string, nonce: uint256, message: string }
	// Note: timestamp is a STRING type, not uint256!
	typeHash := crypto.Keccak256Hash([]byte("ClobAuth(address address,string timestamp,uint256 nonce,string message)"))

	addressBytes := common.LeftPadBytes(address.Bytes(), 32)
	// Timestamp is encoded as a string (hash the string representation)
	timestampHash := crypto.Keccak256Hash([]byte(strconv.FormatInt(timestamp, 10)))
	nonceBytes := common.LeftPadBytes(big.NewInt(nonce).Bytes(), 32)
	messageHash := crypto.Keccak256Hash([]byte(message))

	return crypto.Keccak256Hash(
		typeHash.Bytes(),
		addressBytes,
		timestampHash.Bytes(),
		nonceBytes,
		messageHash.Bytes(),
	)
}

// GetOrderBook fetches the order book for a token
func (tc *TradingClient) GetOrderBook(ctx context.Context, tokenID string) (*OrderBook, error) {
	url := fmt.Sprintf("%s/book?token_id=%s", tc.clobURL, tokenID)

	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	resp, err := tc.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch order book: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("order book request failed: %s", resp.Status)
	}

	var orderBook OrderBook
	if err := json.NewDecoder(resp.Body).Decode(&orderBook); err != nil {
		return nil, fmt.Errorf("failed to decode order book: %w", err)
	}

	return &orderBook, nil
}

// GetBestPrice gets the best available price for a trade
// For BUY: amount = USDC to spend, returns the VWAP price per share
// For SELL: amount = shares to sell, returns the VWAP price per share
func (tc *TradingClient) GetBestPrice(ctx context.Context, tokenID string, side string, amount float64) (float64, error) {
	orderBook, err := tc.GetOrderBook(ctx, tokenID)
	if err != nil {
		return 0, err
	}

	isBuy := strings.ToUpper(side) == "BUY"

	// For BUY orders, look at asks (selling side) - we're buying from sellers
	// For SELL orders, look at bids (buying side) - we're selling to buyers
	var entries []OrderBookEntry
	if isBuy {
		entries = orderBook.Asks
		// Sort asks by price ascending - we want to buy from cheapest sellers first
		sort.Slice(entries, func(i, j int) bool {
			return entries[i].Price < entries[j].Price
		})
	} else {
		entries = orderBook.Bids
		// Sort bids by price descending - we want to sell to highest bidders first
		sort.Slice(entries, func(i, j int) bool {
			return entries[i].Price > entries[j].Price
		})
	}

	if len(entries) == 0 {
		return 0, fmt.Errorf("no liquidity available")
	}

	if isBuy {
		log.Printf("GetBestPrice BUY: amount=%.2f USDC, %d asks in book (sorted ascending)", amount, len(entries))
	} else {
		log.Printf("GetBestPrice SELL: amount=%.2f shares, %d bids in book (sorted descending)", amount, len(entries))
	}
	for i, e := range entries {
		if i < 5 { // Log first 5 entries
			log.Printf("  Entry %d: price=%.4f, size=%.4f", i, e.Price, e.Size)
		}
	}

	totalShares := 0.0
	totalUSDC := 0.0

	if isBuy {
		// BUY: We want to spend 'amount' USDC to buy shares
		// Iterate through asks (cheapest first), accumulating shares
		remainingUSDC := amount

		for _, entry := range entries {
			if remainingUSDC <= 0 {
				break
			}

			// How much USDC to buy all shares at this price level?
			costForLevel := entry.Size * entry.Price

			if costForLevel >= remainingUSDC {
				// We can fill our order at this level
				sharesToBuy := remainingUSDC / entry.Price
				totalShares += sharesToBuy
				totalUSDC += remainingUSDC
				remainingUSDC = 0
			} else {
				// Take all shares at this level
				totalShares += entry.Size
				totalUSDC += costForLevel
				remainingUSDC -= costForLevel
			}
		}

		if remainingUSDC > 0 {
			return 0, fmt.Errorf("insufficient liquidity for BUY order (needed %.2f more USDC)", remainingUSDC)
		}
	} else {
		// SELL: We want to sell 'amount' shares to get USDC
		// Iterate through bids (highest first), accumulating USDC
		remainingShares := amount

		for _, entry := range entries {
			if remainingShares <= 0 {
				break
			}

			if entry.Size >= remainingShares {
				// We can sell all remaining shares at this level
				totalUSDC += remainingShares * entry.Price
				totalShares += remainingShares
				remainingShares = 0
			} else {
				// Sell all shares at this level, continue to next
				totalUSDC += entry.Size * entry.Price
				totalShares += entry.Size
				remainingShares -= entry.Size
			}
		}

		if remainingShares > 0 {
			return 0, fmt.Errorf("insufficient liquidity for SELL order (%.2f shares remaining)", remainingShares)
		}
	}

	// VWAP = total USDC / total shares
	vwap := totalUSDC / totalShares
	log.Printf("GetBestPrice: VWAP=%.6f (totalUSDC=%.2f, totalShares=%.2f)", vwap, totalUSDC, totalShares)

	return vwap, nil
}

// ExecuteTrade executes a trade
func (tc *TradingClient) ExecuteTrade(
	ctx context.Context,
	privateKey *ecdsa.PrivateKey,
	proxyAddress common.Address,
	creds *APICredentials,
	trade *TradeRequest,
) (*TradeResult, error) {
	log.Printf("Executing trade: %+v", trade)

	// Get the best price if not specified (market order)
	price := trade.Price
	if price == 0 {
		var err error
		var amountForPricing float64

		if strings.ToUpper(trade.Side) == "BUY" {
			// BUY: pass USDC amount to get best price
			amountForPricing = trade.Amount
		} else {
			// SELL: pass shares to get best price
			// SharesRaw is in 6-decimal format (e.g., 1000000 = 1 share)
			if trade.SharesRaw > 0 {
				amountForPricing = float64(trade.SharesRaw) / 1e6
			} else {
				// Fallback: estimate shares from Amount (assumes Amount is USDC value)
				// Use a rough mid-market estimate (0.5) to convert
				amountForPricing = trade.Amount / 0.5
				log.Printf("ExecuteTrade SELL: No SharesRaw set, estimating shares=%.2f from Amount=%.2f", amountForPricing, trade.Amount)
			}
		}

		price, err = tc.GetBestPrice(ctx, trade.TokenID, trade.Side, amountForPricing)
		if err != nil {
			return &TradeResult{Success: false, ErrorMsg: fmt.Sprintf("Failed to get price: %v", err)}, nil
		}
		log.Printf("ExecuteTrade: Got market price=%.6f", price)

		// Add slippage buffer (2%) but cap within valid range
		if strings.ToUpper(trade.Side) == "BUY" {
			price *= 1.02
			// Cap at 0.99 for buy orders (max price)
			if price > 0.99 {
				price = 0.99
			}
		} else {
			price *= 0.98
			// Floor at 0.01 for sell orders (min price)
			if price < 0.01 {
				price = 0.01
			}
		}
		log.Printf("ExecuteTrade: Price with slippage (capped)=%.6f", price)
	}

	// Validate price is in valid range (0, 1)
	if price <= 0 || price >= 1 {
		return &TradeResult{Success: false, ErrorMsg: fmt.Sprintf("Invalid price: %.6f (must be between 0 and 1)", price)}, nil
	}

	// Round price to 2 decimal places (Polymarket tick size requirement)
	// This must be done BEFORE calculating amounts to ensure consistency
	price = float64(int64(price*100+0.5)) / 100
	log.Printf("ExecuteTrade: Price rounded to tick size=%.2f", price)

	// Calculate order amounts
	// For BUY: makerAmount = USDC spent, takerAmount = shares received
	// For SELL: makerAmount = shares sold, takerAmount = USDC received
	//
	// Polymarket decimal precision requirements:
	// - BUY orders: makerAmount max 5 decimals, takerAmount max 2 decimals
	// - SELL orders: makerAmount max 2 decimals, takerAmount max 5 decimals
	// With 6 decimal representation:
	// - 5 decimals = must be divisible by 10
	// - 2 decimals = must be divisible by 10000
	var makerAmount, takerAmount string
	var side model.Side

	if strings.ToUpper(trade.Side) == "BUY" {
		side = model.BUY
		// BUY: spending USDC to get shares
		// Step 1: Calculate shares from USDC amount
		// takerAmount (shares): max 2 decimals -> round down to nearest 10000
		shares := int64((trade.Amount / price) * 1e6)
		sharesRounded := (shares / 10000) * 10000
		takerAmount = strconv.FormatInt(sharesRounded, 10)
		// Step 2: Calculate makerAmount FROM shares * price to ensure consistency
		// This is critical: Polymarket validates makerAmount == takerAmount * price
		// makerAmount (USDC): max 5 decimals -> round to nearest 10
		makerAmountRaw := int64(float64(sharesRounded) * price)
		makerAmountRaw = (makerAmountRaw / 10) * 10
		makerAmount = strconv.FormatInt(makerAmountRaw, 10)
		log.Printf("ExecuteTrade BUY: makerAmount=%s USDC (calculated from shares*price), takerAmount=%s shares (raw=%d), price=%.6f, originalUSDC=%.2f",
			makerAmount, takerAmount, shares, price, trade.Amount)
	} else {
		side = model.SELL
		// SELL: selling shares to get USDC
		// makerAmount (shares): max 2 decimals -> round to nearest 10000
		var shares int64
		if trade.SharesRaw > 0 {
			// Use exact shares from position (preferred for selling existing positions)
			shares = trade.SharesRaw
			log.Printf("ExecuteTrade SELL: Using exact SharesRaw=%d from position", shares)
		} else {
			// Calculate from USD amount (fallback)
			shares = int64((trade.Amount / price) * 1e6)
			log.Printf("ExecuteTrade SELL: Calculated shares=%d from Amount=%.2f / price=%.6f", shares, trade.Amount, price)
		}
		sharesRounded := (shares / 10000) * 10000
		makerAmount = strconv.FormatInt(sharesRounded, 10)
		// takerAmount (USDC): calculated from shares * price, max 5 decimals -> round down to nearest 10
		// We calculate from shares to ensure amounts are consistent
		takerAmountRaw := int64(float64(sharesRounded) * price)
		takerAmountRaw = (takerAmountRaw / 10) * 10
		takerAmount = strconv.FormatInt(takerAmountRaw, 10)
		log.Printf("ExecuteTrade SELL: makerAmount=%s shares (raw=%d, rounded=%d), takerAmount=%s USDC (calculated from shares*price), impliedPrice=%.6f",
			makerAmount, shares, sharesRounded, takerAmount, float64(takerAmountRaw)/float64(sharesRounded))
	}

	// Determine signature type based on whether we're using a proxy
	sigType := model.EOA
	eoaAddress := crypto.PubkeyToAddress(privateKey.PublicKey)
	maker := eoaAddress.Hex()

	if proxyAddress != (common.Address{}) && proxyAddress != eoaAddress {
		// POLY_GNOSIS_SAFE (2) is for browser wallet proxies (most common)
		// POLY_PROXY (1) is for email/Magic wallet proxies
		// Most users connecting via browser wallets use Gnosis Safe
		sigType = model.POLY_GNOSIS_SAFE
		maker = proxyAddress.Hex()
		log.Printf("ExecuteTrade: Using proxy wallet, sigType=POLY_GNOSIS_SAFE(2)")
	}

	// Build order data
	orderData := &model.OrderData{
		Maker:         maker,
		Taker:         "0x0000000000000000000000000000000000000000", // Public order
		TokenId:       trade.TokenID,
		MakerAmount:   makerAmount,
		TakerAmount:   takerAmount,
		Side:          side,
		FeeRateBps:    "0", // Default fee
		Nonce:         "0",
		Signer:        eoaAddress.Hex(),
		SignatureType: sigType,
	}

	// Set expiration for GTD orders
	if trade.OrderType == OrderTypeGTD && trade.Expiration > 0 {
		orderData.Expiration = strconv.FormatInt(trade.Expiration, 10)
	}

	// Determine which exchange to use
	contract := model.CTFExchange
	contractName := "CTFExchange"
	if trade.NegativeRisk {
		contract = model.NegRiskCTFExchange
		contractName = "NegRiskCTFExchange"
	}
	log.Printf("ExecuteTrade: Using contract=%s (negRisk=%v)", contractName, trade.NegativeRisk)

	// Build and sign the order
	signedOrder, err := tc.builder.BuildSignedOrder(privateKey, orderData, contract)
	if err != nil {
		return &TradeResult{Success: false, ErrorMsg: fmt.Sprintf("Failed to build order: %v", err)}, nil
	}

	// Submit the order
	return tc.submitOrder(ctx, creds, eoaAddress, signedOrder, trade.OrderType)
}

// submitOrder submits a signed order to the CLOB
func (tc *TradingClient) submitOrder(
	ctx context.Context,
	creds *APICredentials,
	address common.Address,
	signedOrder *model.SignedOrder,
	orderType OrderType,
) (*TradeResult, error) {
	// Convert side from int to string
	sideStr := "BUY"
	if signedOrder.Side.Int64() == 1 {
		sideStr = "SELL"
	}

	// Build request body
	// Note: salt is an integer, most other fields are strings
	orderPayload := map[string]any{
		"order": map[string]any{
			"salt":          signedOrder.Salt.Int64(), // Integer, not string
			"maker":         signedOrder.Maker.Hex(),
			"signer":        signedOrder.Signer.Hex(),
			"taker":         signedOrder.Taker.Hex(),
			"tokenId":       signedOrder.TokenId.String(),
			"makerAmount":   signedOrder.MakerAmount.String(),
			"takerAmount":   signedOrder.TakerAmount.String(),
			"expiration":    signedOrder.Expiration.String(),
			"nonce":         signedOrder.Nonce.String(),
			"feeRateBps":    signedOrder.FeeRateBps.String(),
			"side":          sideStr,
			"signatureType": signedOrder.SignatureType.Int64(),
			"signature":     "0x" + hex.EncodeToString(signedOrder.Signature),
		},
		"owner":     creds.APIKey,
		"orderType": string(orderType),
	}

	body, err := json.Marshal(orderPayload)
	if err != nil {
		return &TradeResult{Success: false, ErrorMsg: "Failed to marshal order"}, nil
	}

	log.Printf("Order payload: %s", string(body))

	url := fmt.Sprintf("%s/order", tc.clobURL)
	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(body))
	if err != nil {
		return &TradeResult{Success: false, ErrorMsg: "Failed to create request"}, nil
	}

	// Set L2 headers
	timestamp := strconv.FormatInt(time.Now().Unix(), 10)
	signature := tc.signL2Request(creds.Secret, timestamp, "POST", "/order", string(body))

	log.Printf("L2 headers - POLY_ADDRESS: %s, POLY_API_KEY: %s", address.Hex(), creds.APIKey)

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("POLY_ADDRESS", address.Hex())
	req.Header.Set("POLY_SIGNATURE", signature)
	req.Header.Set("POLY_TIMESTAMP", timestamp)
	req.Header.Set("POLY_API_KEY", creds.APIKey)
	req.Header.Set("POLY_PASSPHRASE", creds.Passphrase)

	resp, err := tc.httpClient.Do(req)
	if err != nil {
		return &TradeResult{Success: false, ErrorMsg: fmt.Sprintf("Request failed: %v", err)}, nil
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)
	log.Printf("Order response: %s", string(respBody))

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		return &TradeResult{Success: false, ErrorMsg: fmt.Sprintf("Order failed: %s", string(respBody))}, nil
	}

	var result struct {
		Success    bool     `json:"success"`
		ErrorMsg   string   `json:"errorMsg"`
		OrderID    string   `json:"orderId"`
		OrderHashes []string `json:"orderHashes"`
	}
	if err := json.Unmarshal(respBody, &result); err != nil {
		// Try to parse as success even if structure is different
		return &TradeResult{
			Success:   true,
			OrderHash: string(respBody),
		}, nil
	}

	return &TradeResult{
		Success:   result.Success,
		OrderID:   result.OrderID,
		OrderHash: strings.Join(result.OrderHashes, ","),
		ErrorMsg:  result.ErrorMsg,
	}, nil
}

// signL2Request creates HMAC-SHA256 signature for L2 requests
// Implementation follows Python's py-clob-client which uses:
// - base64.urlsafe_b64decode for the secret
// - base64.urlsafe_b64encode for the output signature
func (tc *TradingClient) signL2Request(secret, timestamp, method, path, body string) string {
	message := timestamp + method + path + body

	log.Printf("L2 signing - timestamp: %s, method: %s, path: %s", timestamp, method, path)
	log.Printf("L2 signing - body first 100 chars: %s", truncateString(body, 100))
	log.Printf("L2 signing - message length: %d, secret length: %d", len(message), len(secret))

	// Python uses base64.urlsafe_b64decode which:
	// 1. Replaces - with + and _ with / (normalizes URL-safe to standard)
	// 2. Adds padding if missing
	// So it accepts both standard and URL-safe base64

	var secretBytes []byte
	var decodeMethod string
	var err error

	// First try standard base64 (most common for API responses)
	secretBytes, err = base64.StdEncoding.DecodeString(secret)
	if err == nil {
		decodeMethod = "StdEncoding"
	} else {
		// Try adding padding if missing
		paddedSecret := secret
		switch len(secret) % 4 {
		case 2:
			paddedSecret += "=="
		case 3:
			paddedSecret += "="
		}
		secretBytes, err = base64.StdEncoding.DecodeString(paddedSecret)
		if err == nil {
			decodeMethod = "StdEncoding+padding"
		} else {
			// Try URL-safe base64 (convert to standard first)
			stdSecret := strings.ReplaceAll(secret, "-", "+")
			stdSecret = strings.ReplaceAll(stdSecret, "_", "/")
			// Add padding if needed
			switch len(stdSecret) % 4 {
			case 2:
				stdSecret += "=="
			case 3:
				stdSecret += "="
			}
			secretBytes, err = base64.StdEncoding.DecodeString(stdSecret)
			if err == nil {
				decodeMethod = "URLSafe->StdEncoding"
			} else {
				log.Printf("Failed to decode secret as base64: %v, using raw bytes", err)
				secretBytes = []byte(secret)
				decodeMethod = "rawBytes"
			}
		}
	}
	log.Printf("L2 signing - decoded using: %s", decodeMethod)

	log.Printf("L2 signing - decoded secret length: %d bytes", len(secretBytes))
	log.Printf("L2 signing - secret hex: %x", secretBytes[:min(8, len(secretBytes))])

	h := hmac.New(sha256.New, secretBytes)
	h.Write([]byte(message))

	// Output as URL-safe base64 (matching Python's base64.urlsafe_b64encode)
	sig := base64.URLEncoding.EncodeToString(h.Sum(nil))

	log.Printf("L2 signature: %s", sig)
	return sig
}

func truncateString(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "..."
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// TestL2Auth tests L2 authentication by calling a simple authenticated endpoint
func (tc *TradingClient) TestL2Auth(ctx context.Context, address common.Address, creds *APICredentials) error {
	url := fmt.Sprintf("%s/auth/api-keys", tc.clobURL)

	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return fmt.Errorf("failed to create request: %w", err)
	}

	// Set L2 headers
	timestamp := strconv.FormatInt(time.Now().Unix(), 10)
	signature := tc.signL2Request(creds.Secret, timestamp, "GET", "/auth/api-keys", "")

	log.Printf("TestL2Auth - timestamp: %s, signature: %s", timestamp, signature)

	req.Header.Set("POLY_ADDRESS", address.Hex())
	req.Header.Set("POLY_SIGNATURE", signature)
	req.Header.Set("POLY_TIMESTAMP", timestamp)
	req.Header.Set("POLY_API_KEY", creds.APIKey)
	req.Header.Set("POLY_PASSPHRASE", creds.Passphrase)

	resp, err := tc.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	log.Printf("TestL2Auth response: status=%d, body=%s", resp.StatusCode, string(body))

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("L2 auth test failed: %s - %s", resp.Status, string(body))
	}

	return nil
}

// OpenOrder represents an open order from the CLOB API
type OpenOrder struct {
	ID              string   `json:"id"`
	Status          string   `json:"status"`
	Owner           string   `json:"owner"`
	MakerAddress    string   `json:"maker_address"`
	Market          string   `json:"market"`
	AssetID         string   `json:"asset_id"`
	Side            string   `json:"side"`
	OriginalSize    string   `json:"original_size"`
	SizeMatched     string   `json:"size_matched"`
	Price           string   `json:"price"`
	AssociateTrades []string `json:"associate_trades"`
	Outcome         string   `json:"outcome"`
	CreatedAt       int64    `json:"created_at"`
	Expiration      string   `json:"expiration"`
	OrderType       string   `json:"order_type"`
}

// OpenOrdersResponse represents the paginated response for open orders
type OpenOrdersResponse struct {
	NextCursor string       `json:"next_cursor"`
	Data       []*OpenOrder `json:"data"`
}

// GetOpenOrders fetches all open orders for an address using L2 authentication
func (tc *TradingClient) GetOpenOrders(ctx context.Context, address common.Address, creds *APICredentials) ([]*OpenOrder, error) {
	var allOrders []*OpenOrder
	cursor := ""
	endCursor := "LTE="

	for {
		// Build URL with cursor for pagination
		url := fmt.Sprintf("%s/data/orders", tc.clobURL)
		if cursor != "" {
			url = fmt.Sprintf("%s?next_cursor=%s", url, cursor)
		}

		req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
		if err != nil {
			return nil, fmt.Errorf("failed to create request: %w", err)
		}

		// Determine the path for signing (with or without query params)
		path := "/data/orders"
		if cursor != "" {
			path = fmt.Sprintf("/data/orders?next_cursor=%s", cursor)
		}

		// Set L2 headers
		timestamp := strconv.FormatInt(time.Now().Unix(), 10)
		signature := tc.signL2Request(creds.Secret, timestamp, "GET", path, "")

		req.Header.Set("POLY_ADDRESS", address.Hex())
		req.Header.Set("POLY_SIGNATURE", signature)
		req.Header.Set("POLY_TIMESTAMP", timestamp)
		req.Header.Set("POLY_API_KEY", creds.APIKey)
		req.Header.Set("POLY_PASSPHRASE", creds.Passphrase)

		resp, err := tc.httpClient.Do(req)
		if err != nil {
			return nil, fmt.Errorf("failed to fetch orders: %w", err)
		}
		defer resp.Body.Close()

		body, _ := io.ReadAll(resp.Body)

		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("get orders failed: %s - %s", resp.Status, string(body))
		}

		var response OpenOrdersResponse
		if err := json.Unmarshal(body, &response); err != nil {
			// Try parsing as direct array (fallback)
			var orders []*OpenOrder
			if err2 := json.Unmarshal(body, &orders); err2 != nil {
				return nil, fmt.Errorf("failed to decode orders: %w (response: %s)", err, string(body))
			}
			allOrders = append(allOrders, orders...)
			break
		}

		allOrders = append(allOrders, response.Data...)

		// Check if we've reached the end
		if response.NextCursor == "" || response.NextCursor == endCursor {
			break
		}
		cursor = response.NextCursor
	}

	return allOrders, nil
}

// CancelOrder cancels a single order by ID using L2 authentication
func (tc *TradingClient) CancelOrder(ctx context.Context, address common.Address, creds *APICredentials, orderID string) error {
	url := fmt.Sprintf("%s/order/%s", tc.clobURL, orderID)

	req, err := http.NewRequestWithContext(ctx, "DELETE", url, nil)
	if err != nil {
		return fmt.Errorf("failed to create request: %w", err)
	}

	// Set L2 headers
	path := fmt.Sprintf("/order/%s", orderID)
	timestamp := strconv.FormatInt(time.Now().Unix(), 10)
	signature := tc.signL2Request(creds.Secret, timestamp, "DELETE", path, "")

	req.Header.Set("POLY_ADDRESS", address.Hex())
	req.Header.Set("POLY_SIGNATURE", signature)
	req.Header.Set("POLY_TIMESTAMP", timestamp)
	req.Header.Set("POLY_API_KEY", creds.APIKey)
	req.Header.Set("POLY_PASSPHRASE", creds.Passphrase)

	resp, err := tc.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("failed to cancel order: %w", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNoContent {
		return fmt.Errorf("cancel order failed: %s - %s", resp.Status, string(body))
	}

	log.Printf("Order %s cancelled successfully", orderID)
	return nil
}

// CancelAllOrders cancels all open orders using L2 authentication
func (tc *TradingClient) CancelAllOrders(ctx context.Context, address common.Address, creds *APICredentials) (int, error) {
	// First get all open orders
	orders, err := tc.GetOpenOrders(ctx, address, creds)
	if err != nil {
		return 0, fmt.Errorf("failed to get orders: %w", err)
	}

	if len(orders) == 0 {
		return 0, nil
	}

	// Cancel each order
	cancelled := 0
	for _, order := range orders {
		if err := tc.CancelOrder(ctx, address, creds, order.ID); err != nil {
			log.Printf("Failed to cancel order %s: %v", order.ID, err)
			continue
		}
		cancelled++
	}

	return cancelled, nil
}

// GetTokenIDByIndex gets the token ID for a specific outcome index (0 or 1)
func (tc *TradingClient) GetTokenIDByIndex(ctx context.Context, marketID string, outcomeIndex int) (string, error) {
	url := fmt.Sprintf("https://gamma-api.polymarket.com/markets/%s", marketID)
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return "", err
	}

	resp, err := tc.httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	var marketDetail struct {
		ClobTokenIds string `json:"clobTokenIds"`
		Outcomes     string `json:"outcomes"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&marketDetail); err != nil {
		return "", err
	}

	// Parse the token IDs array
	var tokenIDs []string
	if err := json.Unmarshal([]byte(marketDetail.ClobTokenIds), &tokenIDs); err != nil {
		return "", fmt.Errorf("failed to parse clobTokenIds: %w", err)
	}

	// Parse outcomes for logging
	var outcomes []string
	json.Unmarshal([]byte(marketDetail.Outcomes), &outcomes)

	log.Printf("GetTokenIDByIndex: marketID=%s, outcomeIndex=%d, outcomes=%v, tokenIDs count=%d",
		marketID, outcomeIndex, outcomes, len(tokenIDs))

	if outcomeIndex < 0 || outcomeIndex >= len(tokenIDs) {
		return "", fmt.Errorf("invalid outcome index %d (have %d tokens)", outcomeIndex, len(tokenIDs))
	}

	log.Printf("GetTokenIDByIndex: returning tokenID=%s for index %d", tokenIDs[outcomeIndex], outcomeIndex)
	return tokenIDs[outcomeIndex], nil
}

// GetTokenIDForOutcome gets the token ID for a specific outcome in a market
func (tc *TradingClient) GetTokenIDForOutcome(ctx context.Context, market *GammaMarket, outcome string) (string, error) {
	// The market should have clobTokenIds field
	// For now, we'll need to fetch it from the Gamma API

	url := fmt.Sprintf("https://gamma-api.polymarket.com/markets/%s", market.ID)
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return "", err
	}

	resp, err := tc.httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	var marketDetail struct {
		ClobTokenIds string `json:"clobTokenIds"` // JSON array string like "[\"123\",\"456\"]"
		Outcomes     string `json:"outcomes"`     // JSON array string like "[\"Yes\",\"No\"]" or "[\"Spurs\",\"Lakers\"]"
	}
	if err := json.NewDecoder(resp.Body).Decode(&marketDetail); err != nil {
		return "", err
	}

	// Parse the token IDs array
	var tokenIDs []string
	if err := json.Unmarshal([]byte(marketDetail.ClobTokenIds), &tokenIDs); err != nil {
		return "", fmt.Errorf("failed to parse clobTokenIds: %w", err)
	}

	// Parse the outcomes array to find the correct index
	var outcomes []string
	if err := json.Unmarshal([]byte(marketDetail.Outcomes), &outcomes); err != nil {
		log.Printf("Warning: failed to parse outcomes, falling back to index-based lookup: %v", err)
		// Fallback to old behavior if outcomes parsing fails
		outcome = strings.ToUpper(outcome)
		if outcome == "YES" && len(tokenIDs) > 0 {
			return tokenIDs[0], nil
		} else if outcome == "NO" && len(tokenIDs) > 1 {
			return tokenIDs[1], nil
		}
		return "", fmt.Errorf("token ID not found for outcome: %s", outcome)
	}

	log.Printf("GetTokenIDForOutcome: market outcomes=%v, looking for outcome=%s", outcomes, outcome)

	// Find the matching outcome index (case-insensitive)
	outcomeUpper := strings.ToUpper(outcome)
	for i, o := range outcomes {
		if strings.ToUpper(o) == outcomeUpper {
			if i < len(tokenIDs) {
				log.Printf("GetTokenIDForOutcome: matched outcome '%s' at index %d, tokenID=%s", o, i, tokenIDs[i])
				return tokenIDs[i], nil
			}
		}
	}

	// If exact match not found, try Yes/No mapping for binary markets
	// Some markets use "Yes"/"No" internally but display different names
	if len(outcomes) == 2 && len(tokenIDs) == 2 {
		switch outcomeUpper {
		case "YES":
			log.Printf("GetTokenIDForOutcome: using index 0 for YES, tokenID=%s", tokenIDs[0])
			return tokenIDs[0], nil
		case "NO":
			log.Printf("GetTokenIDForOutcome: using index 1 for NO, tokenID=%s", tokenIDs[1])
			return tokenIDs[1], nil
		}
	}

	return "", fmt.Errorf("token ID not found for outcome: %s (available outcomes: %v)", outcome, outcomes)
}
