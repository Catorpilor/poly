package orderv2

import (
	"encoding/hex"
	"math/big"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
)

// Golden vector captured from py-clob-client-v2's
// tests/order_utils/test_exchange_order_builder_v2.py (EXPECTED_POLY_1271_SIGNATURE)
// and documented in docs/deposit-wallet-flow.md §7. The TS and Python SDKs
// produce this byte-for-byte; this test proves the Go implementation does too.
const (
	gvPrivKey       = "ac0974bec39a17e36ba4a6b4d238ff944bacb478cbed5efcae784d7bf4f2ff80" // anvil #0
	gvEOA           = "0xf39Fd6e51aad88F6F4ce6aB8827279cffFb92266"
	gvChainID       = int64(80002) // Amoy
	gvDepositWallet = "0x1111111111111111111111111111111111111111"
	gvSalt          = "479249096354"
	gvTimestamp     = "1710000000000"

	gvAppDomainSep = "0xa440cbd865bc0c6243d7a8df9a8bf48a8827b0a4abbb61c30e96d305423af148"
	gvContentsHash = "0xd23d42d3ad94e65d78258cecaf8dcbaddac0f73dc085040f2c12bb595dd83804"

	gvExpectedSig = "0xa3a093c83b6c20c83355c16ce94c92e6e9fcbdeb840618cc74f6c57a42ad145b" +
		"2b98db73d2c73cbf1f2b6af288566ae81960ddbc3a13921027358a8bff3be6ff1c" +
		"a440cbd865bc0c6243d7a8df9a8bf48a8827b0a4abbb61c30e96d305423af148" +
		"d23d42d3ad94e65d78258cecaf8dcbaddac0f73dc085040f2c12bb595dd83804" +
		"4f726465722875696e743235362073616c742c61646472657373206d616b65722c" +
		"61646472657373207369676e65722c75696e7432353620746f6b656e49642c75" +
		"696e74323536206d616b6572416d6f756e742c75696e743235362074616b6572" +
		"416d6f756e742c75696e743820736964652c75696e7438207369676e61747572" +
		"65547970652c75696e743235362074696d657374616d702c6279746573333220" +
		"6d657461646174612c62797465733332206275696c6465722900ba"
)

func goldenOrderData() *OrderData {
	return &OrderData{
		Maker:         gvDepositWallet,
		Signer:        gvDepositWallet,
		TokenId:       "1234",
		MakerAmount:   "100000000",
		TakerAmount:   "50000000",
		Side:          BUY,
		SignatureType: POLY_1271,
		Timestamp:     gvTimestamp,
		// Metadata and Builder default to zero (bytes32(0)).
	}
}

func goldenBuilder() *Builder {
	salt, _ := new(big.Int).SetString(gvSalt, 10)
	return NewBuilder(gvChainID).
		WithSaltGenerator(func() *big.Int { return new(big.Int).Set(salt) }).
		WithTimestampMillis(func() int64 { return 1710000000000 })
}

func TestOrderTypeStringIsCanonical(t *testing.T) {
	// The verifier slices contentsType by this length; it must be exactly 186 (0x00ba).
	if got := len(orderTypeString); got != 186 {
		t.Fatalf("len(orderTypeString) = %d, want 186 (0x00ba)", got)
	}
}

func TestDepositWalletIntermediateHashes(t *testing.T) {
	b := goldenBuilder()
	order, err := b.BuildOrder(goldenOrderData())
	if err != nil {
		t.Fatalf("BuildOrder: %v", err)
	}

	exchange, err := VerifyingContractAddress(CTFExchange)
	if err != nil {
		t.Fatalf("VerifyingContractAddress: %v", err)
	}

	if got := exchangeDomainSeparator(b.chainID, exchange).Hex(); !strings.EqualFold(got, gvAppDomainSep) {
		t.Errorf("appDomainSep = %s, want %s", got, gvAppDomainSep)
	}
	if got := orderStructHash(order).Hex(); !strings.EqualFold(got, gvContentsHash) {
		t.Errorf("contentsHash = %s, want %s", got, gvContentsHash)
	}
}

func TestDepositWalletGoldenVector(t *testing.T) {
	key, err := crypto.HexToECDSA(gvPrivKey)
	if err != nil {
		t.Fatalf("HexToECDSA: %v", err)
	}
	if got := crypto.PubkeyToAddress(key.PublicKey).Hex(); !strings.EqualFold(got, gvEOA) {
		t.Fatalf("EOA = %s, want %s", got, gvEOA)
	}

	// Exercise the public dispatch (BuildSignedOrder must route POLY_1271 here).
	signed, err := goldenBuilder().BuildSignedOrder(key, goldenOrderData(), CTFExchange)
	if err != nil {
		t.Fatalf("BuildSignedOrder: %v", err)
	}

	gotSig := "0x" + hex.EncodeToString(signed.Signature)
	if !strings.EqualFold(gotSig, gvExpectedSig) {
		t.Fatalf("packed signature mismatch\n got: %s\nwant: %s", gotSig, gvExpectedSig)
	}

	// Sanity: trailer encodes the contentsType length (186 = 0x00ba).
	n := len(signed.Signature)
	if trailer := signed.Signature[n-2:]; trailer[0] != 0x00 || trailer[1] != 0xba {
		t.Errorf("trailer = 0x%02x%02x, want 0x00ba", trailer[0], trailer[1])
	}
	// maker == signer == deposit wallet.
	if signed.Maker != signed.Signer || !strings.EqualFold(signed.Maker.Hex(), gvDepositWallet) {
		t.Errorf("maker/signer = %s/%s, want both %s", signed.Maker.Hex(), signed.Signer.Hex(), gvDepositWallet)
	}
}

func TestDepositWalletRejectsMismatchedMakerSigner(t *testing.T) {
	data := goldenOrderData()
	data.Signer = "0x2222222222222222222222222222222222222222" // != maker
	key, _ := crypto.HexToECDSA(gvPrivKey)

	if _, err := goldenBuilder().BuildSignedOrder(key, data, CTFExchange); err == nil {
		t.Fatal("expected error for POLY_1271 with maker != signer, got nil")
	}
}

func TestLegacyPathUnchangedForEOA(t *testing.T) {
	// A non-1271 order must still take the legacy ECDSA path: a plain 65-byte
	// signature that recovers to the signer (here EOA == maker).
	key, _ := crypto.HexToECDSA(gvPrivKey)
	data := &OrderData{
		Maker:         gvEOA,
		TokenId:       "1234",
		MakerAmount:   "100000000",
		TakerAmount:   "50000000",
		Side:          BUY,
		SignatureType: EOA,
		Timestamp:     gvTimestamp,
	}
	signed, err := goldenBuilder().BuildSignedOrder(key, data, CTFExchange)
	if err != nil {
		t.Fatalf("legacy BuildSignedOrder: %v", err)
	}
	if len(signed.Signature) != 65 {
		t.Errorf("legacy signature len = %d, want 65 (got the packed 1271 blob?)", len(signed.Signature))
	}
	if signed.Signer != common.HexToAddress(gvEOA) {
		t.Errorf("legacy signer = %s, want %s", signed.Signer.Hex(), gvEOA)
	}
}
