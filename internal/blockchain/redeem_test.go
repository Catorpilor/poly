package blockchain

import (
	"math/big"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/accounts/abi"
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

// TestEncodeStandardRedemption_UsesUSDCe pins the collateral token argument
// to USDC.e (LegacyUSDCAddress). All currently-resolvable Polymarket
// conditions were minted pre-V2, so their CTF positions are backed by USDC.e.
// Passing pUSD instead would compute a different position ID and silently
// no-op — see the redeem incident on tx 0xbac169e7…92a6f5.
//
// The test simulates production startup by overriding USDCAddress to pUSD
// (as InitAddresses does), then asserts the encoder still uses USDC.e.
func TestEncodeStandardRedemption_UsesUSDCe(t *testing.T) {
	originalUSDC := USDCAddress
	t.Cleanup(func() { USDCAddress = originalUSDC })
	USDCAddress = common.HexToAddress("0xC011a7E12a19f7B1f670d46F03B03f3342E82DFB") // pUSD

	conditionID := common.HexToHash("0xabcdef1234567890abcdef1234567890abcdef1234567890abcdef1234567890")
	_, calldata, err := EncodeStandardRedemption(conditionID)
	if err != nil {
		t.Fatalf("EncodeStandardRedemption() error = %v", err)
	}

	parsed, err := abi.JSON(strings.NewReader(ctfRedeemABI))
	if err != nil {
		t.Fatalf("parse abi: %v", err)
	}
	args, err := parsed.Methods["redeemPositions"].Inputs.Unpack(calldata[4:])
	if err != nil {
		t.Fatalf("unpack: %v", err)
	}

	gotCollateral, ok := args[0].(common.Address)
	if !ok {
		t.Fatalf("collateralToken arg type = %T, want common.Address", args[0])
	}
	if gotCollateral != LegacyUSDCAddress {
		t.Errorf("collateralToken = %s, want %s (USDC.e)", gotCollateral.Hex(), LegacyUSDCAddress.Hex())
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

func TestEncodeMultiSend(t *testing.T) {
	t.Parallel()

	txs := []MultiSendTx{
		{
			To:   common.HexToAddress("0x4D97DCd97eC945f40cF65F87097ACe5EA0476045"),
			Data: []byte{0x01, 0x02, 0x03, 0x04},
		},
		{
			To:   common.HexToAddress("0xd91E80cF2E7be2e162c6513ceD06f1dD0dA35296"),
			Data: []byte{0xAA, 0xBB},
		},
	}

	calldata, err := EncodeMultiSend(txs)
	if err != nil {
		t.Fatalf("EncodeMultiSend() error = %v", err)
	}

	if len(calldata) < 4 {
		t.Fatal("calldata too short")
	}

	// Should be longer than a single tx encoding
	singleTx := []MultiSendTx{txs[0]}
	singleCalldata, err := EncodeMultiSend(singleTx)
	if err != nil {
		t.Fatalf("EncodeMultiSend(single) error = %v", err)
	}

	if len(calldata) <= len(singleCalldata) {
		t.Error("two-tx encoding should be longer than single-tx")
	}

	// Same method selector for both
	if string(calldata[:4]) != string(singleCalldata[:4]) {
		t.Error("method selectors should match")
	}
}

func TestEncodeMultiSend_Empty(t *testing.T) {
	t.Parallel()

	_, err := EncodeMultiSend(nil)
	if err == nil {
		t.Error("expected error for empty tx list")
	}
}
