package blockchain

import (
	"testing"

	"github.com/ethereum/go-ethereum/common"

	"github.com/Catorpilor/poly/internal/config"
)

// TestInitAddresses_OverridesUSDCToPUSD confirms that the package-level
// USDCAddress (which BalanceChecker reads on every /wallet call) gets
// overridden to the V2 collateral (pUSD) when InitAddresses is invoked
// at startup. Regression guard for /wallet showing 0 because it was
// querying USDC.e instead of pUSD.
func TestInitAddresses_OverridesUSDCToPUSD(t *testing.T) {
	originalUSDC := USDCAddress
	t.Cleanup(func() { USDCAddress = originalUSDC })

	pUSD := "0xC011a7E12a19f7B1f670d46F03B03f3342E82DFB"
	cfg := &config.PolymarketConfig{USDCAddress: pUSD}

	InitAddresses(cfg)

	if USDCAddress != common.HexToAddress(pUSD) {
		t.Errorf("USDCAddress = %s, want %s", USDCAddress.Hex(), pUSD)
	}
}
