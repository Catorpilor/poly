package blockchain

import (
	"context"
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
		{"name": "asset",  "type": "address"},
		{"name": "to",     "type": "address"},
		{"name": "amount", "type": "uint256"}
	],
	"outputs": []
},{
	"name": "unwrap",
	"type": "function",
	"inputs": [
		{"name": "asset",  "type": "address"},
		{"name": "to",     "type": "address"},
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

// EncodeWrapCollateral builds calldata for CollateralOnramp.wrap(asset, to, amount).
// `asset` is the input ERC-20 (USDC.e or USDC native); `to` is the recipient of
// the minted pUSD. Submit the resulting tx with To=CollateralOnrampAddress.
func EncodeWrapCollateral(asset, to common.Address, amount *big.Int) ([]byte, error) {
	parsed, err := abi.JSON(strings.NewReader(collateralOnrampABI))
	if err != nil {
		return nil, fmt.Errorf("parse onramp ABI: %w", err)
	}
	data, err := parsed.Pack("wrap", asset, to, amount)
	if err != nil {
		return nil, fmt.Errorf("pack wrap: %w", err)
	}
	return data, nil
}

// BuildWrapAllTxs returns the approve+wrap sub-transactions that convert
// `amount` of the proxy's USDC.e into pUSD via the Collateral Onramp.
// Shared by the /migrate bootstrap and the post-redemption sweep (ADR 0003):
// redemptions pay raw USDC.e, which the V2 exchanges cannot spend, so it is
// wrapped in the same flow. A zero amount returns no transactions.
func BuildWrapAllTxs(proxy common.Address, amount *big.Int) ([]MultiSendTx, error) {
	if CollateralOnrampAddress == (common.Address{}) {
		return nil, fmt.Errorf("V2 collateral onramp address not configured (POLYMARKET_COLLATERAL_ONRAMP_ADDRESS)")
	}
	if amount == nil || amount.Sign() <= 0 {
		return nil, nil
	}

	// Wrapping requires the onramp to be approved as a USDC.e spender.
	approveData, err := EncodeApproveERC20(CollateralOnrampAddress, MaxUint256)
	if err != nil {
		return nil, fmt.Errorf("encode USDC.e approve: %w", err)
	}
	wrapData, err := EncodeWrapCollateral(LegacyUSDCAddress, proxy, amount)
	if err != nil {
		return nil, fmt.Errorf("encode wrap: %w", err)
	}
	return []MultiSendTx{
		{To: LegacyUSDCAddress, Data: approveData},
		{To: CollateralOnrampAddress, Data: wrapData},
	}, nil
}

// PlanCollateralSweep reads the proxy's current USDC.e balance and returns
// the approve+wrap transactions that convert all of it to pUSD, plus the
// amount. Used after a confirmed redemption (the relayer waits for on-chain
// confirmation, so the payout is already in the balance) and by /migrate.
func PlanCollateralSweep(ctx context.Context, bc *BalanceChecker, proxyAddress common.Address) ([]MultiSendTx, *big.Int, error) {
	balance, err := bc.getERC20Balance(ctx, proxyAddress, LegacyUSDCAddress)
	if err != nil {
		return nil, nil, fmt.Errorf("read USDC.e balance: %w", err)
	}
	txs, err := BuildWrapAllTxs(proxyAddress, balance)
	if err != nil {
		return nil, nil, err
	}
	return txs, balance, nil
}

// EncodeUnwrapCollateral builds calldata for CollateralOnramp.unwrap(asset, to, amount).
// Submit the resulting tx with To=CollateralOnrampAddress.
func EncodeUnwrapCollateral(asset, to common.Address, amount *big.Int) ([]byte, error) {
	parsed, err := abi.JSON(strings.NewReader(collateralOnrampABI))
	if err != nil {
		return nil, fmt.Errorf("parse onramp ABI: %w", err)
	}
	data, err := parsed.Pack("unwrap", asset, to, amount)
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
