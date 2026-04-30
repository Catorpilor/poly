package blockchain

import (
	"github.com/ethereum/go-ethereum/common"

	"github.com/Catorpilor/poly/internal/config"
)

// CollateralOnrampAddress wraps USDC/USDC.e → pUSD on V2. Empty pre-V2.
var CollateralOnrampAddress common.Address

// InitAddresses overrides the package-level contract addresses from config.
// Call once at startup, before any code reads the address vars.
func InitAddresses(cfg *config.PolymarketConfig) {
	if cfg.ConditionalTokensAddress != "" {
		ConditionalTokensAddress = common.HexToAddress(cfg.ConditionalTokensAddress)
	}
	if cfg.USDCAddress != "" {
		USDCAddress = common.HexToAddress(cfg.USDCAddress)
	}
	if cfg.CTFExchangeAddress != "" {
		CTFExchangeAddress = common.HexToAddress(cfg.CTFExchangeAddress)
	}
	if cfg.NegRiskExchangeAddress != "" {
		NegRiskExchangeAddress = common.HexToAddress(cfg.NegRiskExchangeAddress)
	}
	if cfg.CollateralOnrampAddress != "" {
		CollateralOnrampAddress = common.HexToAddress(cfg.CollateralOnrampAddress)
	}
}
