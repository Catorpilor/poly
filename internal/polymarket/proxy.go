package polymarket

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/Catorpilor/poly/internal/config"
)

// ProxyResolver handles resolution of Polymarket proxy wallets
type ProxyResolver struct {
	apiURL     string
	apiKey     string
	httpClient *http.Client
}

// NewProxyResolver creates a new proxy resolver
func NewProxyResolver(cfg *config.PolymarketConfig) *ProxyResolver {
	return &ProxyResolver{
		apiURL: cfg.CLOBAPIUrl,
		apiKey: cfg.APIKey,
		httpClient: &http.Client{
			Timeout: 10 * time.Second,
		},
	}
}

// ProxyInfo contains proxy wallet information from Polymarket
type ProxyInfo struct {
	EOAAddress   string `json:"user"`
	ProxyAddress string `json:"proxy"`
	IsActive     bool   `json:"active"`
	CreatedAt    string `json:"created_at"`
}

// GetProxyWallet queries Polymarket API for a user's proxy wallet
func (r *ProxyResolver) GetProxyWallet(ctx context.Context, eoaAddress common.Address) (common.Address, error) {
	// Method 1: Try Polymarket's user endpoint
	proxy, err := r.getUserProxy(ctx, eoaAddress)
	if err == nil && proxy != (common.Address{}) {
		return proxy, nil
	}

	// Method 2: Try the address derivation API
	proxy, err = r.deriveProxyAddress(ctx, eoaAddress)
	if err == nil && proxy != (common.Address{}) {
		return proxy, nil
	}

	return common.Address{}, fmt.Errorf("no proxy wallet found for %s", eoaAddress.Hex())
}

// getUserProxy queries the user endpoint for proxy information
func (r *ProxyResolver) getUserProxy(ctx context.Context, eoaAddress common.Address) (common.Address, error) {
	// Format the URL - this endpoint might vary based on Polymarket's actual API
	url := fmt.Sprintf("%s/user/%s", r.apiURL, strings.ToLower(eoaAddress.Hex()))

	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return common.Address{}, err
	}

	// Add headers
	req.Header.Set("Accept", "application/json")
	if r.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+r.apiKey)
	}

	resp, err := r.httpClient.Do(req)
	if err != nil {
		return common.Address{}, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return common.Address{}, fmt.Errorf("API returned status %d", resp.StatusCode)
	}

	var info ProxyInfo
	if err := json.NewDecoder(resp.Body).Decode(&info); err != nil {
		return common.Address{}, err
	}

	if info.ProxyAddress == "" {
		return common.Address{}, fmt.Errorf("no proxy address in response")
	}

	return common.HexToAddress(info.ProxyAddress), nil
}

// deriveProxyAddress tries to derive the proxy address using Polymarket's derivation endpoint
func (r *ProxyResolver) deriveProxyAddress(ctx context.Context, eoaAddress common.Address) (common.Address, error) {
	// Polymarket might have an endpoint to derive proxy addresses
	url := fmt.Sprintf("%s/derive-proxy?address=%s", r.apiURL, strings.ToLower(eoaAddress.Hex()))

	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return common.Address{}, err
	}

	req.Header.Set("Accept", "application/json")
	if r.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+r.apiKey)
	}

	resp, err := r.httpClient.Do(req)
	if err != nil {
		return common.Address{}, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return common.Address{}, fmt.Errorf("API returned status %d", resp.StatusCode)
	}

	var result struct {
		ProxyAddress string `json:"proxy_address"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return common.Address{}, err
	}

	if result.ProxyAddress == "" {
		return common.Address{}, fmt.Errorf("no proxy address in response")
	}

	return common.HexToAddress(result.ProxyAddress), nil
}

// GetOrCreateProxy gets an existing proxy or requests creation of a new one
func (r *ProxyResolver) GetOrCreateProxy(ctx context.Context, eoaAddress common.Address) (common.Address, error) {
	// First try to get existing proxy
	proxy, err := r.GetProxyWallet(ctx, eoaAddress)
	if err == nil && proxy != (common.Address{}) {
		return proxy, nil
	}

	// If no proxy exists, Polymarket might have an endpoint to trigger creation
	// Note: This would typically require the user to sign a transaction
	return common.Address{}, fmt.Errorf("proxy creation not implemented - requires user signature")
}

// CheckProxyStatus checks if a proxy wallet is active and properly configured
func (r *ProxyResolver) CheckProxyStatus(ctx context.Context, proxyAddress common.Address) (bool, error) {
	url := fmt.Sprintf("%s/proxy/%s/status", r.apiURL, strings.ToLower(proxyAddress.Hex()))

	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return false, err
	}

	req.Header.Set("Accept", "application/json")
	if r.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+r.apiKey)
	}

	resp, err := r.httpClient.Do(req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return false, fmt.Errorf("API returned status %d", resp.StatusCode)
	}

	var status struct {
		Active bool `json:"active"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&status); err != nil {
		return false, err
	}

	return status.Active, nil
}