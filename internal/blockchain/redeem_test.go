package blockchain

import (
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/common"
)

func TestEncodeStandardRedemption(t *testing.T) {
	t.Parallel()

	conditionID := common.HexToHash("0xabcdef1234567890abcdef1234567890abcdef1234567890abcdef1234567890")

	target, calldata, err := EncodeStandardRedemption(conditionID)
	if err != nil {
		t.Fatalf("EncodeStandardRedemption() error = %v", err)
	}

	// Should target the ConditionalTokens contract
	if target != ConditionalTokensAddress {
		t.Errorf("target = %s, want %s", target.Hex(), ConditionalTokensAddress.Hex())
	}

	// Calldata should not be empty
	if len(calldata) == 0 {
		t.Fatal("calldata is empty")
	}

	// First 4 bytes are the method selector for redeemPositions(address,bytes32,bytes32,uint256[])
	if len(calldata) < 4 {
		t.Fatal("calldata too short for method selector")
	}
}

func TestEncodeStandardRedemption_DifferentConditions(t *testing.T) {
	t.Parallel()

	cond1 := common.HexToHash("0x1111111111111111111111111111111111111111111111111111111111111111")
	cond2 := common.HexToHash("0x2222222222222222222222222222222222222222222222222222222222222222")

	_, data1, err := EncodeStandardRedemption(cond1)
	if err != nil {
		t.Fatalf("EncodeStandardRedemption(cond1) error = %v", err)
	}

	_, data2, err := EncodeStandardRedemption(cond2)
	if err != nil {
		t.Fatalf("EncodeStandardRedemption(cond2) error = %v", err)
	}

	// Same method selector
	if string(data1[:4]) != string(data2[:4]) {
		t.Error("method selectors differ for same function")
	}

	// Different calldata overall (different conditionID)
	if string(data1) == string(data2) {
		t.Error("calldata should differ for different conditionIDs")
	}
}

func TestEncodeNegRiskRedemption(t *testing.T) {
	t.Parallel()

	conditionID := common.HexToHash("0xabcdef1234567890abcdef1234567890abcdef1234567890abcdef1234567890")
	amounts := []*big.Int{
		big.NewInt(1000000), // 1 USDC worth of YES tokens
		big.NewInt(500000),  // 0.5 USDC worth of NO tokens
	}

	target, calldata, err := EncodeNegRiskRedemption(conditionID, amounts)
	if err != nil {
		t.Fatalf("EncodeNegRiskRedemption() error = %v", err)
	}

	// Should target the NegRiskAdapter contract
	if target != NegRiskAdapterAddress {
		t.Errorf("target = %s, want %s", target.Hex(), NegRiskAdapterAddress.Hex())
	}

	if len(calldata) == 0 {
		t.Fatal("calldata is empty")
	}

	if len(calldata) < 4 {
		t.Fatal("calldata too short for method selector")
	}
}

func TestEncodeNegRiskRedemption_DifferentAmounts(t *testing.T) {
	t.Parallel()

	conditionID := common.HexToHash("0xabcdef1234567890abcdef1234567890abcdef1234567890abcdef1234567890")

	amounts1 := []*big.Int{big.NewInt(100), big.NewInt(200)}
	amounts2 := []*big.Int{big.NewInt(300), big.NewInt(400)}

	_, data1, err := EncodeNegRiskRedemption(conditionID, amounts1)
	if err != nil {
		t.Fatalf("error = %v", err)
	}

	_, data2, err := EncodeNegRiskRedemption(conditionID, amounts2)
	if err != nil {
		t.Fatalf("error = %v", err)
	}

	// Same method selector
	if string(data1[:4]) != string(data2[:4]) {
		t.Error("method selectors differ for same function")
	}

	// Different calldata (different amounts)
	if string(data1) == string(data2) {
		t.Error("calldata should differ for different amounts")
	}
}

func TestEncodeSetApprovalForAll(t *testing.T) {
	t.Parallel()

	operator := NegRiskAdapterAddress

	calldata, err := EncodeSetApprovalForAll(operator, true)
	if err != nil {
		t.Fatalf("EncodeSetApprovalForAll() error = %v", err)
	}

	if len(calldata) == 0 {
		t.Fatal("calldata is empty")
	}

	if len(calldata) < 4 {
		t.Fatal("calldata too short for method selector")
	}

	// Encoding for true vs false should differ
	calldataFalse, err := EncodeSetApprovalForAll(operator, false)
	if err != nil {
		t.Fatalf("EncodeSetApprovalForAll(false) error = %v", err)
	}

	if string(calldata) == string(calldataFalse) {
		t.Error("calldata for true and false should differ")
	}

	// Same method selector
	if string(calldata[:4]) != string(calldataFalse[:4]) {
		t.Error("method selectors should match")
	}
}
