package blockchain

import (
	"fmt"
	"math/big"
	"strings"

	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
)

// NegRiskAdapterAddress is the Polymarket NegRiskAdapter on Polygon
var NegRiskAdapterAddress = common.HexToAddress("0xd91E80cF2E7be2e162c6513ceD06f1dD0dA35296")

// USDCAddress is the bridged USDC on Polygon (collateral token)
var USDCAddress = common.HexToAddress("0x2791Bca1f2de4661ED88A30C99A7a9449Aa84174")

const ctfRedeemABI = `[{
	"name": "redeemPositions",
	"type": "function",
	"inputs": [
		{"name": "collateralToken", "type": "address"},
		{"name": "parentCollectionId", "type": "bytes32"},
		{"name": "conditionId", "type": "bytes32"},
		{"name": "indexSets", "type": "uint256[]"}
	],
	"outputs": []
}]`

const negRiskRedeemABI = `[{
	"name": "redeemPositions",
	"type": "function",
	"inputs": [
		{"name": "_conditionId", "type": "bytes32"},
		{"name": "_amounts", "type": "uint256[]"}
	],
	"outputs": []
}]`

const setApprovalForAllABI = `[{
	"name": "setApprovalForAll",
	"type": "function",
	"inputs": [
		{"name": "operator", "type": "address"},
		{"name": "approved", "type": "bool"}
	],
	"outputs": []
}]`

// EncodeStandardRedemption builds calldata for CTF.redeemPositions on standard (binary) markets.
// Burns the caller's entire position balance for the condition and pays out USDC for winning tokens.
func EncodeStandardRedemption(conditionID common.Hash) (common.Address, []byte, error) {
	parsed, err := abi.JSON(strings.NewReader(ctfRedeemABI))
	if err != nil {
		return common.Address{}, nil, fmt.Errorf("parse CTF redeem ABI: %w", err)
	}

	// indexSets [1, 2] covers both outcomes in a binary market:
	// 1 = 0b01 (outcome 0 / YES), 2 = 0b10 (outcome 1 / NO)
	indexSets := []*big.Int{big.NewInt(1), big.NewInt(2)}
	parentCollectionID := common.Hash{} // bytes32(0)

	data, err := parsed.Pack("redeemPositions", USDCAddress, parentCollectionID, conditionID, indexSets)
	if err != nil {
		return common.Address{}, nil, fmt.Errorf("pack redeemPositions: %w", err)
	}

	return ConditionalTokensAddress, data, nil
}

// EncodeNegRiskRedemption builds calldata for NegRiskAdapter.redeemPositions on negative-risk markets.
// Takes explicit amounts for each outcome position.
func EncodeNegRiskRedemption(conditionID common.Hash, amounts []*big.Int) (common.Address, []byte, error) {
	parsed, err := abi.JSON(strings.NewReader(negRiskRedeemABI))
	if err != nil {
		return common.Address{}, nil, fmt.Errorf("parse NegRisk redeem ABI: %w", err)
	}

	data, err := parsed.Pack("redeemPositions", conditionID, amounts)
	if err != nil {
		return common.Address{}, nil, fmt.Errorf("pack redeemPositions: %w", err)
	}

	return NegRiskAdapterAddress, data, nil
}

// EncodeSetApprovalForAll builds calldata for CTF.setApprovalForAll.
// Used to approve the NegRiskAdapter as an operator before neg-risk redemption.
func EncodeSetApprovalForAll(operator common.Address, approved bool) ([]byte, error) {
	parsed, err := abi.JSON(strings.NewReader(setApprovalForAllABI))
	if err != nil {
		return nil, fmt.Errorf("parse setApprovalForAll ABI: %w", err)
	}

	data, err := parsed.Pack("setApprovalForAll", operator, approved)
	if err != nil {
		return nil, fmt.Errorf("pack setApprovalForAll: %w", err)
	}

	return data, nil
}
