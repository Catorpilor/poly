package blockchain

import (
	"fmt"

	"github.com/ethereum/go-ethereum/common"

	"github.com/Catorpilor/poly/internal/config"
)

// Polymarket contract addresses on Polygon. Zero until InitAddresses runs —
// config is the single source of truth (V2 defaults live in config.LoadConfig).
// Deliberately not initialized with literals: a stale literal silently
// shadowing config is how pre-InitAddresses reads once pointed at V1
// exchanges.
var (
	// ConditionalTokensAddress is the CTF (ERC-1155) contract; unchanged V1→V2.
	ConditionalTokensAddress common.Address
	// USDCAddress is the collateral users hold and exchanges spend (pUSD on V2).
	USDCAddress common.Address
	// CTFExchangeAddress is the CTF Exchange (V2 post-cutover).
	CTFExchangeAddress common.Address
	// NegRiskExchangeAddress is the NegRisk CTF Exchange (V2 post-cutover).
	NegRiskExchangeAddress common.Address
	// CollateralOnrampAddress wraps USDC/USDC.e → pUSD on V2.
	CollateralOnrampAddress common.Address
)

// InitAddresses sets the package-level contract addresses from config.
// Call once at startup, before any code reads the address vars. A missing
// or malformed address errors here so a misconfigured deployment fails at
// boot, not against the wrong contract mid-trade.
func InitAddresses(cfg *config.PolymarketConfig) error {
	required := []struct {
		env string
		dst *common.Address
		val string
	}{
		{"POLYMARKET_CONDITIONAL_TOKENS_ADDRESS", &ConditionalTokensAddress, cfg.ConditionalTokensAddress},
		{"POLYMARKET_COLLATERAL_ADDRESS", &USDCAddress, cfg.USDCAddress},
		{"POLYMARKET_CTF_EXCHANGE_ADDRESS", &CTFExchangeAddress, cfg.CTFExchangeAddress},
		{"POLYMARKET_NEGRISK_EXCHANGE_ADDRESS", &NegRiskExchangeAddress, cfg.NegRiskExchangeAddress},
		{"POLYMARKET_COLLATERAL_ONRAMP_ADDRESS", &CollateralOnrampAddress, cfg.CollateralOnrampAddress},
	}
	for _, r := range required {
		if !common.IsHexAddress(r.val) {
			return fmt.Errorf("invalid or missing contract address %s: %q", r.env, r.val)
		}
		*r.dst = common.HexToAddress(r.val)
	}
	return nil
}
