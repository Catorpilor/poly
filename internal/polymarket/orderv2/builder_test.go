package orderv2

import (
	"encoding/hex"
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/polymarket/go-order-utils/pkg/eip712"
)

// Golden vectors captured from @polymarket/clob-client-v2@1.0.2 via
// /tmp/v2-golden-vectors/gen.mjs (committed in this repo's history is
// docs/v2-migration-plan.md). These pin the Go implementation byte-for-byte
// against the canonical TypeScript SDK output.
//
// Inputs (fixed): privKey 0x0123…cdef, salt 1e12, timestamp 1714300000000 ms,
// CTF Exchange V2 verifying contract, chainId 137.
const (
	goldenPrivKey         = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	goldenChainID         = int64(137)
	goldenSalt            = int64(1000000000000)
	goldenTimestampMillis = int64(1714300000000)

	goldenEOAAddr         = "0xFCAd0B19bB29D4674531d6f115237E16AfCE377c"
	goldenTokenID         = "71321045679252212594626385532706912750332728571942532289631379312455583992563"
	goldenMakerAmount     = "100000000"
	goldenTakerAmount     = "200000000"
	goldenOrderTypehash   = "0xbb86318a2138f5fa8ae32fbe8e659f8fcf13cc6ae4014a707893055433818589"
	goldenDomainSeparator = "0x3264e159346253e26a64e00b69032db0e7d32f94628de3e6eecb50304d7af3d2"
	goldenEIP712Digest    = "0x3ab8128b03c7a143f1feaea0f4135834e53a5ab126f630c387587be80fbf257f"
	goldenSignature       = "0x58cdc09daff006bad5cb9838e28ddf6da0b0e3fa2979ee31733313fa0dd3e6ca466605210040064850fde4e8c40fa2342e63a33c410983915281c1614cf92fac1b"
)

func TestOrderTypehashMatchesSDK(t *testing.T) {
	t.Parallel()
	got := orderTypeHash.Hex()
	if got != goldenOrderTypehash {
		t.Fatalf("orderTypeHash mismatch\nwant: %s\n got: %s\n(type string: %q)", goldenOrderTypehash, got, orderTypeString)
	}
}

func TestDomainSeparatorMatchesSDK(t *testing.T) {
	t.Parallel()
	verifyingContract, err := VerifyingContractAddress(CTFExchange)
	if err != nil {
		t.Fatalf("VerifyingContractAddress: %v", err)
	}
	sep, err := eip712.BuildEIP712DomainSeparator(domainNameHash, domainVersionHash, big.NewInt(goldenChainID), verifyingContract)
	if err != nil {
		t.Fatalf("BuildEIP712DomainSeparator: %v", err)
	}
	if sep.Hex() != goldenDomainSeparator {
		t.Fatalf("domain separator mismatch\nwant: %s\n got: %s", goldenDomainSeparator, sep.Hex())
	}
}

func TestBuildSignedOrderMatchesSDK(t *testing.T) {
	t.Parallel()

	privKey, err := crypto.HexToECDSA(goldenPrivKey)
	if err != nil {
		t.Fatalf("HexToECDSA: %v", err)
	}

	b := NewBuilder(goldenChainID).
		WithSaltGenerator(func() *big.Int { return big.NewInt(goldenSalt) }).
		WithTimestampMillis(func() int64 { return goldenTimestampMillis })

	signed, err := b.BuildSignedOrder(privKey, &OrderData{
		Maker:         goldenEOAAddr,
		TokenId:       goldenTokenID,
		MakerAmount:   goldenMakerAmount,
		TakerAmount:   goldenTakerAmount,
		Side:          BUY,
		SignatureType: EOA,
	}, CTFExchange)
	if err != nil {
		t.Fatalf("BuildSignedOrder: %v", err)
	}

	// Signer defaulted to maker when OrderData.Signer was empty.
	if signed.Signer != common.HexToAddress(goldenEOAAddr) {
		t.Errorf("signer = %s, want %s", signed.Signer.Hex(), goldenEOAAddr)
	}

	// Verify the EIP-712 digest matches the TS SDK's hashTypedData output.
	hash, err := b.BuildOrderHash(&signed.Order, CTFExchange)
	if err != nil {
		t.Fatalf("BuildOrderHash: %v", err)
	}
	if hash.Hex() != goldenEIP712Digest {
		t.Fatalf("EIP-712 digest mismatch\nwant: %s\n got: %s", goldenEIP712Digest, hash.Hex())
	}

	gotSig := "0x" + hex.EncodeToString(signed.Signature)
	if gotSig != goldenSignature {
		t.Fatalf("signature mismatch\nwant: %s\n got: %s", goldenSignature, gotSig)
	}
}

func TestBuildOrderDefaultsSignerToMaker(t *testing.T) {
	t.Parallel()
	b := NewBuilder(137).
		WithSaltGenerator(func() *big.Int { return big.NewInt(1) }).
		WithTimestampMillis(func() int64 { return 1 })

	order, err := b.BuildOrder(&OrderData{
		Maker:       goldenEOAAddr,
		TokenId:     "1",
		MakerAmount: "1",
		TakerAmount: "1",
	})
	if err != nil {
		t.Fatalf("BuildOrder: %v", err)
	}
	if order.Signer != common.HexToAddress(goldenEOAAddr) {
		t.Errorf("signer = %s, want %s", order.Signer.Hex(), goldenEOAAddr)
	}
}

func TestBuildOrderRejectsInvalidDecimals(t *testing.T) {
	t.Parallel()
	b := NewBuilder(137)
	_, err := b.BuildOrder(&OrderData{
		Maker:       goldenEOAAddr,
		TokenId:     "not-a-number",
		MakerAmount: "1",
		TakerAmount: "1",
	})
	if err == nil {
		t.Fatal("expected error for invalid tokenId, got nil")
	}
}
