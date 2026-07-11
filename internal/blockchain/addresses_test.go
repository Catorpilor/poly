package blockchain

import (
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/common"

	"github.com/Catorpilor/poly/internal/config"
)

// validPolymarketConfig mirrors the V2 defaults from config.LoadConfig.
func validPolymarketConfig() *config.PolymarketConfig {
	return &config.PolymarketConfig{
		ConditionalTokensAddress: "0x4D97DCd97eC945f40cF65F87097ACe5EA0476045",
		USDCAddress:              "0xC011a7E12a19f7B1f670d46F03B03f3342E82DFB", // pUSD
		CTFExchangeAddress:       "0xE111180000d2663C0091e4f400237545B87B996B",
		NegRiskExchangeAddress:   "0xe2222d279d744050d28e00520010520000310F59",
		CollateralOnrampAddress:  "0x93070a847efEf7F70739046A929D47a521F5B8ee",
	}
}

// saveAddresses snapshots the package-level address vars and restores them
// after the test, since InitAddresses mutates shared state.
func saveAddresses(t *testing.T) {
	t.Helper()
	ct, usdc := ConditionalTokensAddress, USDCAddress
	ctf, neg := CTFExchangeAddress, NegRiskExchangeAddress
	onramp := CollateralOnrampAddress
	t.Cleanup(func() {
		ConditionalTokensAddress, USDCAddress = ct, usdc
		CTFExchangeAddress, NegRiskExchangeAddress = ctf, neg
		CollateralOnrampAddress = onramp
	})
}

// TestInitAddresses_SetsAllFromConfig confirms every package-level address
// comes from config — the single source of truth. Regression guard for
// /wallet showing 0 (querying USDC.e instead of pUSD) and for stale V1
// exchange literals shadowing the V2 config defaults.
func TestInitAddresses_SetsAllFromConfig(t *testing.T) {
	saveAddresses(t)

	cfg := validPolymarketConfig()
	if err := InitAddresses(cfg); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	checks := []struct {
		name string
		got  common.Address
		want string
	}{
		{"ConditionalTokensAddress", ConditionalTokensAddress, cfg.ConditionalTokensAddress},
		{"USDCAddress", USDCAddress, cfg.USDCAddress},
		{"CTFExchangeAddress", CTFExchangeAddress, cfg.CTFExchangeAddress},
		{"NegRiskExchangeAddress", NegRiskExchangeAddress, cfg.NegRiskExchangeAddress},
		{"CollateralOnrampAddress", CollateralOnrampAddress, cfg.CollateralOnrampAddress},
	}
	for _, c := range checks {
		if c.got != common.HexToAddress(c.want) {
			t.Errorf("%s = %s, want %s", c.name, c.got.Hex(), c.want)
		}
	}
}

// A misconfigured deployment must fail at boot, not trade against the wrong
// contract mid-flight.
func TestInitAddresses_RejectsMissingAddress(t *testing.T) {
	saveAddresses(t)

	cfg := validPolymarketConfig()
	cfg.CTFExchangeAddress = ""

	err := InitAddresses(cfg)
	if err == nil || !strings.Contains(err.Error(), "POLYMARKET_CTF_EXCHANGE_ADDRESS") {
		t.Fatalf("err = %v, want error naming POLYMARKET_CTF_EXCHANGE_ADDRESS", err)
	}
}

func TestInitAddresses_RejectsMalformedAddress(t *testing.T) {
	saveAddresses(t)

	cfg := validPolymarketConfig()
	cfg.USDCAddress = "not-an-address"

	err := InitAddresses(cfg)
	if err == nil || !strings.Contains(err.Error(), "POLYMARKET_COLLATERAL_ADDRESS") {
		t.Fatalf("err = %v, want error naming POLYMARKET_COLLATERAL_ADDRESS", err)
	}
}
