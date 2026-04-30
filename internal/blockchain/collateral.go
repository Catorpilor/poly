package blockchain

import (
	"fmt"
	"math/big"
	"strings"

	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
)

// LegacyUSDCAddress is the bridged USDC (USDC.e) on Polygon — the V1 collateral
// token. After the V2 cutover this address is no longer the default collateral,
// but balances persist and must be wrapped into pUSD via the Collateral Onramp.
var LegacyUSDCAddress = common.HexToAddress("0x2791Bca1f2de4661ED88A30C99A7a9449Aa84174")

// MaxUint256 is the standard ERC-20 unlimited-allowance value.
var MaxUint256 = new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), 256), big.NewInt(1))

const collateralOnrampABI = `[{
	"name": "wrap",
	"type": "function",
	"inputs": [
		{"name": "amount", "type": "uint256"}
	],
	"outputs": []
},{
	"name": "unwrap",
	"type": "function",
	"inputs": [
		{"name": "amount", "type": "uint256"}
	],
	"outputs": []
}]`

const erc20ApproveABI = `[{
	"name": "approve",
	"type": "function",
	"inputs": [
		{"name": "spender", "type": "address"},
		{"name": "amount",  "type": "uint256"}
	],
	"outputs": [{"name": "", "type": "bool"}]
},{
	"name": "allowance",
	"type": "function",
	"inputs": [
		{"name": "owner",   "type": "address"},
		{"name": "spender", "type": "address"}
	],
	"outputs": [{"name": "", "type": "uint256"}]
}]`

// EncodeWrapCollateral builds calldata for CollateralOnramp.wrap(amount).
// Submit the resulting tx with To=CollateralOnrampAddress.
func EncodeWrapCollateral(amount *big.Int) ([]byte, error) {
	parsed, err := abi.JSON(strings.NewReader(collateralOnrampABI))
	if err != nil {
		return nil, fmt.Errorf("parse onramp ABI: %w", err)
	}
	data, err := parsed.Pack("wrap", amount)
	if err != nil {
		return nil, fmt.Errorf("pack wrap: %w", err)
	}
	return data, nil
}

// EncodeUnwrapCollateral builds calldata for CollateralOnramp.unwrap(amount).
// Submit the resulting tx with To=CollateralOnrampAddress.
func EncodeUnwrapCollateral(amount *big.Int) ([]byte, error) {
	parsed, err := abi.JSON(strings.NewReader(collateralOnrampABI))
	if err != nil {
		return nil, fmt.Errorf("parse onramp ABI: %w", err)
	}
	data, err := parsed.Pack("unwrap", amount)
	if err != nil {
		return nil, fmt.Errorf("pack unwrap: %w", err)
	}
	return data, nil
}

// EncodeApproveERC20 builds calldata for an ERC-20 approve(spender, amount) call.
// Submit the resulting tx with To=<token contract address>.
func EncodeApproveERC20(spender common.Address, amount *big.Int) ([]byte, error) {
	parsed, err := abi.JSON(strings.NewReader(erc20ApproveABI))
	if err != nil {
		return nil, fmt.Errorf("parse erc20 ABI: %w", err)
	}
	data, err := parsed.Pack("approve", spender, amount)
	if err != nil {
		return nil, fmt.Errorf("pack approve: %w", err)
	}
	return data, nil
}

// EncodeAllowanceCall builds calldata for ERC-20 allowance(owner, spender).
// Used to read existing allowances so we can skip already-approved spenders.
func EncodeAllowanceCall(owner, spender common.Address) ([]byte, error) {
	parsed, err := abi.JSON(strings.NewReader(erc20ApproveABI))
	if err != nil {
		return nil, fmt.Errorf("parse erc20 ABI: %w", err)
	}
	data, err := parsed.Pack("allowance", owner, spender)
	if err != nil {
		return nil, fmt.Errorf("pack allowance: %w", err)
	}
	return data, nil
}
