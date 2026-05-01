package blockchain

import (
	"encoding/hex"
	"math/big"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
)

// TestEncodeWrapCollateral_Selector pins the wrap selector to the on-chain
// signature wrap(address,address,uint256). Regression guard for the migrate
// failure where we were calling wrap(uint256), which doesn't exist on the
// CollateralOnramp contract and reverts.
func TestEncodeWrapCollateral_Selector(t *testing.T) {
	t.Parallel()

	asset := LegacyUSDCAddress
	to := common.HexToAddress("0x2C21F8E2d2F613c76Ad96e45A29E4DBB43c46CE6")
	amount := big.NewInt(104_000_000) // 104 USDC.e (6 decimals)

	data, err := EncodeWrapCollateral(asset, to, amount)
	if err != nil {
		t.Fatalf("EncodeWrapCollateral error: %v", err)
	}
	if len(data) < 4 {
		t.Fatalf("calldata too short: %d bytes", len(data))
	}

	wantSelector := crypto.Keccak256([]byte("wrap(address,address,uint256)"))[:4]
	gotSelector := data[:4]
	if !equalBytes(gotSelector, wantSelector) {
		t.Errorf("selector = %s, want %s", hex.EncodeToString(gotSelector), hex.EncodeToString(wantSelector))
	}

	parsed, err := abi.JSON(strings.NewReader(collateralOnrampABI))
	if err != nil {
		t.Fatalf("parse abi: %v", err)
	}
	args, err := parsed.Methods["wrap"].Inputs.Unpack(data[4:])
	if err != nil {
		t.Fatalf("unpack: %v", err)
	}
	if got := args[0].(common.Address); got != asset {
		t.Errorf("asset arg = %s, want %s", got.Hex(), asset.Hex())
	}
	if got := args[1].(common.Address); got != to {
		t.Errorf("to arg = %s, want %s", got.Hex(), to.Hex())
	}
	if got := args[2].(*big.Int); got.Cmp(amount) != 0 {
		t.Errorf("amount arg = %s, want %s", got, amount)
	}
}

func equalBytes(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
