package blockchain

import (
	"bytes"
	"math/big"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/common"
)

// BuildWrapAllTxs is the shared approve+wrap tail used by /migrate and by the
// post-redemption sweep (ADR 0003): redemptions pay raw USDC.e, which the V2
// exchanges cannot spend and the pUSD balance display does not show, so it
// must be wrapped to pUSD in the same flow.
func TestBuildWrapAllTxs(t *testing.T) {
	saveAddresses(t)
	CollateralOnrampAddress = common.HexToAddress("0x93070a847efEf7F70739046A929D47a521F5B8ee")

	proxy := common.HexToAddress("0x3eAc81d8Fc307b615606bdABE156476BBA6f67CB")
	amount := big.NewInt(595_238_093) // 595.238093 USDC.e

	txs, err := BuildWrapAllTxs(proxy, amount)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(txs) != 2 {
		t.Fatalf("got %d txs, want 2 (approve + wrap)", len(txs))
	}

	// [0] approve(onramp, MaxUint256) on the USDC.e contract
	if txs[0].To != LegacyUSDCAddress {
		t.Errorf("approve target = %s, want USDC.e %s", txs[0].To.Hex(), LegacyUSDCAddress.Hex())
	}
	wantApprove, err := EncodeApproveERC20(CollateralOnrampAddress, MaxUint256)
	if err != nil {
		t.Fatalf("encode expected approve: %v", err)
	}
	if !bytes.Equal(txs[0].Data, wantApprove) {
		t.Errorf("approve calldata mismatch")
	}

	// [1] wrap(USDC.e, proxy, amount) on the onramp
	if txs[1].To != CollateralOnrampAddress {
		t.Errorf("wrap target = %s, want onramp %s", txs[1].To.Hex(), CollateralOnrampAddress.Hex())
	}
	wantWrap, err := EncodeWrapCollateral(LegacyUSDCAddress, proxy, amount)
	if err != nil {
		t.Fatalf("encode expected wrap: %v", err)
	}
	if !bytes.Equal(txs[1].Data, wantWrap) {
		t.Errorf("wrap calldata mismatch")
	}
}

func TestBuildWrapAllTxs_ZeroAmountIsNoop(t *testing.T) {
	saveAddresses(t)
	CollateralOnrampAddress = common.HexToAddress("0x93070a847efEf7F70739046A929D47a521F5B8ee")

	txs, err := BuildWrapAllTxs(common.HexToAddress("0x1"), big.NewInt(0))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(txs) != 0 {
		t.Errorf("got %d txs, want 0 for zero amount", len(txs))
	}
}

func TestBuildWrapAllTxs_RequiresOnrampAddress(t *testing.T) {
	saveAddresses(t)
	CollateralOnrampAddress = common.Address{}

	_, err := BuildWrapAllTxs(common.HexToAddress("0x1"), big.NewInt(1))
	if err == nil || !strings.Contains(err.Error(), "onramp") {
		t.Fatalf("err = %v, want onramp-not-configured error", err)
	}
}
