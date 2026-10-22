package orderv2

import (
	"bytes"
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
)

// Exchange v3 (Combos) per docs.polymarket.com/trading/combos/requesters:
// domain {name "Polymarket CTF Exchange", version "3", chainId 137,
// verifyingContract 0xe3333700cA9d93003F00f0F71f8515005F6c00Aa}, same Order
// struct as V2.
const v3Address = "0xe3333700cA9d93003F00f0F71f8515005F6c00Aa"

// manualDomainSeparator builds an EIP-712 domain separator from literal
// strings, independently of the package's cached hashes.
func manualDomainSeparator(name, version string, chainID int64, contract common.Address) common.Hash {
	return crypto.Keccak256Hash(concatBytes(
		crypto.Keccak256([]byte("EIP712Domain(string name,string version,uint256 chainId,address verifyingContract)")),
		crypto.Keccak256([]byte(name)),
		crypto.Keccak256([]byte(version)),
		uint256Word(big.NewInt(chainID)),
		addressWord(contract),
	))
}

func v3TestOrder(t *testing.T, sigType SignatureType, maker, signer string) *Order {
	t.Helper()
	b := NewBuilder(137).WithSaltGenerator(func() *big.Int { return big.NewInt(424242) })
	o, err := b.BuildOrder(&OrderData{
		Maker: maker, Signer: signer,
		TokenId:     "1587172404441704079474252447958188878554308508048282572074770511662550089728",
		MakerAmount: "966191", TakerAmount: "1932381",
		Side: BUY, SignatureType: sigType,
		Timestamp: "1792245804", // v3 timestamps are unix SECONDS
	})
	if err != nil {
		t.Fatal(err)
	}
	return o
}

func TestExchangeV3Address(t *testing.T) {
	t.Parallel()
	got, err := VerifyingContractAddress(ExchangeV3)
	if err != nil || got != common.HexToAddress(v3Address) {
		t.Fatalf("ExchangeV3 = %s, %v; want %s", got.Hex(), err, v3Address)
	}
}

func TestExchangeV3OrderHashUsesVersion3Domain(t *testing.T) {
	t.Parallel()

	o := v3TestOrder(t, POLY_GNOSIS_SAFE, "0x2222222222222222222222222222222222222222", "0x1111111111111111111111111111111111111111")
	got, err := NewBuilder(137).BuildOrderHash(o, ExchangeV3)
	if err != nil {
		t.Fatal(err)
	}
	dom := manualDomainSeparator("Polymarket CTF Exchange", "3", 137, common.HexToAddress(v3Address))
	want := crypto.Keccak256Hash(concatBytes([]byte{0x19, 0x01}, dom.Bytes(), orderStructHash(o).Bytes()))
	if got != want {
		t.Errorf("v3 order hash = %s, want %s (domain v\"3\" @ %s)", got.Hex(), want.Hex(), v3Address)
	}

	// The same order on V2 must hash differently (version "2", other address).
	v2, _ := NewBuilder(137).BuildOrderHash(o, CTFExchange)
	if v2 == got {
		t.Error("v3 and v2 hashes must differ")
	}
}

func TestExchangeV3V2DomainUnchanged(t *testing.T) {
	t.Parallel()
	// Guard: adding v3 must not move the V2 domain (golden tests cover the
	// full V2 vectors; this pins the version string directly).
	o := v3TestOrder(t, EOA, "0x1111111111111111111111111111111111111111", "")
	got, _ := NewBuilder(137).BuildOrderHash(o, CTFExchange)
	dom := manualDomainSeparator("Polymarket CTF Exchange", "2", 137, common.HexToAddress("0xE111180000d2663C0091e4f400237545B87B996B"))
	want := crypto.Keccak256Hash(concatBytes([]byte{0x19, 0x01}, dom.Bytes(), orderStructHash(o).Bytes()))
	if got != want {
		t.Errorf("v2 hash moved: %s, want %s", got.Hex(), want.Hex())
	}
}

func TestExchangeV3DepositWalletWrapUsesVersion3AppDomain(t *testing.T) {
	t.Parallel()

	key, err := crypto.HexToECDSA(gvPrivKey)
	if err != nil {
		t.Fatal(err)
	}
	wallet := "0x4b2f0b7B91319419d52f01cAf7D73D453770318b"
	b := NewBuilder(137).WithSaltGenerator(func() *big.Int { return big.NewInt(424242) })
	so, err := b.BuildSignedOrder(key, &OrderData{
		Maker: wallet, Signer: wallet,
		TokenId:     "1587172404441704079474252447958188878554308508048282572074770511662550089728",
		MakerAmount: "966191", TakerAmount: "1932381",
		Side: BUY, SignatureType: POLY_1271, Timestamp: "1792245804",
	}, ExchangeV3)
	if err != nil {
		t.Fatal(err)
	}
	// Packed blob: innerSig(65) || appDomainSep(32) || contentsHash(32) || type || len.
	if len(so.Signature) < 65+32+32 {
		t.Fatalf("signature too short: %d", len(so.Signature))
	}
	appDom := so.Signature[65:97]
	want := manualDomainSeparator("Polymarket CTF Exchange", "3", 137, common.HexToAddress(v3Address))
	if !bytes.Equal(appDom, want.Bytes()) {
		t.Errorf("ERC-7739 app domain = %x, want v3 domain %s", appDom, want.Hex())
	}
}
