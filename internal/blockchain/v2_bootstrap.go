package blockchain

import (
	"context"
	"fmt"
	"math/big"

	"github.com/ethereum/go-ethereum/common"
)

// V2BootstrapPlan describes what needs to happen to make a proxy wallet ready
// for V2 trading: wrap any leftover USDC.e, approve pUSD for the V2 exchanges,
// and grant the V2 exchanges permission to move CTF outcome tokens.
type V2BootstrapPlan struct {
	// WrapAmount is the USDC.e balance to wrap into pUSD. Zero if none.
	WrapAmount *big.Int

	// ApprovePUSDFor lists the V2 exchange addresses that need pUSD allowance
	// (only those currently below the half-MaxUint256 threshold).
	ApprovePUSDFor []common.Address

	// SetCTFApprovalFor lists the V2 exchange addresses that need
	// setApprovalForAll on the ConditionalTokens contract (only those not
	// already approved).
	SetCTFApprovalFor []common.Address

	// Txs is the ordered MultiSend payload that performs the work above.
	Txs []MultiSendTx
}

// IsEmpty reports whether the plan has any work to do.
func (p *V2BootstrapPlan) IsEmpty() bool {
	return len(p.Txs) == 0
}

// Summary returns a one-line human-readable description of the plan.
func (p *V2BootstrapPlan) Summary() string {
	if p.IsEmpty() {
		return "no migration needed — wallet already V2-ready"
	}
	parts := []string{}
	if p.WrapAmount != nil && p.WrapAmount.Sign() > 0 {
		parts = append(parts, fmt.Sprintf("wrap %s USDC.e → pUSD", FormatUSDC(p.WrapAmount)))
	}
	if n := len(p.ApprovePUSDFor); n > 0 {
		parts = append(parts, fmt.Sprintf("approve pUSD for %d V2 exchange(s)", n))
	}
	if n := len(p.SetCTFApprovalFor); n > 0 {
		parts = append(parts, fmt.Sprintf("grant CTF approval for %d V2 exchange(s)", n))
	}
	if len(parts) == 0 {
		return "no migration needed — wallet already V2-ready"
	}
	out := parts[0]
	for _, p := range parts[1:] {
		out += ", " + p
	}
	return out
}

// halfMaxUint256 is the threshold below which we re-approve. We don't require
// exactly MaxUint256 because some allowances decay over time; anything above
// half-max is "effectively unlimited" for our purposes.
var halfMaxUint256 = new(big.Int).Lsh(big.NewInt(1), 255)

// PlanV2Bootstrap inspects the proxy wallet's on-chain state and returns the
// minimum set of transactions needed to make it ready for V2 trading.
//
// Reads performed: USDC.e balance, pUSD allowances against both V2 exchanges,
// CTF setApprovalForAll status against both V2 exchanges. Cheap (5 calls).
func PlanV2Bootstrap(ctx context.Context, bc *BalanceChecker, proxyAddress common.Address) (*V2BootstrapPlan, error) {
	if CollateralOnrampAddress == (common.Address{}) {
		return nil, fmt.Errorf("V2 collateral onramp address not configured (POLYMARKET_COLLATERAL_ONRAMP_ADDRESS)")
	}

	plan := &V2BootstrapPlan{WrapAmount: big.NewInt(0)}
	v2Exchanges := []common.Address{CTFExchangeAddress, NegRiskExchangeAddress}

	// 1. Wrap leftover USDC.e if any.
	usdcEBalance, err := bc.getERC20Balance(ctx, proxyAddress, LegacyUSDCAddress)
	if err != nil {
		return nil, fmt.Errorf("read USDC.e balance: %w", err)
	}
	if usdcEBalance.Sign() > 0 {
		// Wrapping requires the onramp to be approved as a USDC.e spender.
		// Add an approve(MaxUint256) for the onramp before the wrap call.
		approveData, err := EncodeApproveERC20(CollateralOnrampAddress, MaxUint256)
		if err != nil {
			return nil, fmt.Errorf("encode USDC.e approve: %w", err)
		}
		plan.Txs = append(plan.Txs, MultiSendTx{To: LegacyUSDCAddress, Data: approveData})

		wrapData, err := EncodeWrapCollateral(LegacyUSDCAddress, proxyAddress, usdcEBalance)
		if err != nil {
			return nil, fmt.Errorf("encode wrap: %w", err)
		}
		plan.Txs = append(plan.Txs, MultiSendTx{To: CollateralOnrampAddress, Data: wrapData})
		plan.WrapAmount = usdcEBalance
	}

	// 2. Approve pUSD for each V2 exchange where allowance is insufficient.
	for _, exchange := range v2Exchanges {
		allow, err := bc.GetERC20Allowance(ctx, USDCAddress, proxyAddress, exchange)
		if err != nil {
			return nil, fmt.Errorf("read pUSD allowance for %s: %w", exchange.Hex(), err)
		}
		if allow.Cmp(halfMaxUint256) < 0 {
			data, err := EncodeApproveERC20(exchange, MaxUint256)
			if err != nil {
				return nil, fmt.Errorf("encode pUSD approve: %w", err)
			}
			plan.Txs = append(plan.Txs, MultiSendTx{To: USDCAddress, Data: data})
			plan.ApprovePUSDFor = append(plan.ApprovePUSDFor, exchange)
		}
	}

	// 3. setApprovalForAll(CTF) for each V2 exchange that isn't already approved.
	for _, exchange := range v2Exchanges {
		approved, err := bc.IsApprovedForAll(ctx, proxyAddress, exchange)
		if err != nil {
			return nil, fmt.Errorf("read CTF approval for %s: %w", exchange.Hex(), err)
		}
		if !approved {
			data, err := EncodeSetApprovalForAll(exchange, true)
			if err != nil {
				return nil, fmt.Errorf("encode setApprovalForAll: %w", err)
			}
			plan.Txs = append(plan.Txs, MultiSendTx{To: ConditionalTokensAddress, Data: data})
			plan.SetCTFApprovalFor = append(plan.SetCTFApprovalFor, exchange)
		}
	}

	return plan, nil
}
