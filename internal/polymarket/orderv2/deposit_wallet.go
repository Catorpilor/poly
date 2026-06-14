package orderv2

import (
	"crypto/ecdsa"
	"errors"
	"fmt"
	"math/big"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
)

// Deposit-wallet (POLY_1271 / ERC-7739) order signing.
//
// Polymarket "deposit wallet" accounts are smart-contract wallets that validate
// order signatures via EIP-1271 (isValidSignature). Before the controlling EOA
// signs, the order is wrapped with Solady's ERC-7739 nested TypedDataSign
// scheme, and the on-wire order.signature is a packed blob the wallet
// reconstructs and validates.
//
// For POLY_1271: order.Maker == order.Signer == the deposit-wallet contract
// address. The EOA that holds the private key does NOT appear in the order — it
// only produces the inner ECDSA signature (and rides in the POLY_ADDRESS auth
// header at submission time, handled by the trading client, not here).
//
// Algorithm and the verified golden vector are documented in
// docs/deposit-wallet-flow.md §3 and §7. This implementation is checked against
// that vector in deposit_wallet_test.go.

const (
	depositWalletDomainName    = "DepositWallet"
	depositWalletDomainVersion = "1"

	// soladyTypeString is the ERC-7739 TypedDataSign wrapper. Per Solady, the
	// nested struct's own type string (orderTypeString) is appended when
	// computing the type hash.
	soladyTypeString = "TypedDataSign(Order contents,string name,string version,uint256 chainId,address verifyingContract,bytes32 salt)"

	eip712DomainTypeString = "EIP712Domain(string name,string version,uint256 chainId,address verifyingContract)"
)

var (
	depositWalletNameHash    = crypto.Keccak256Hash([]byte(depositWalletDomainName))
	depositWalletVersionHash = crypto.Keccak256Hash([]byte(depositWalletDomainVersion))
	soladyTypeHash           = crypto.Keccak256Hash([]byte(soladyTypeString + orderTypeString))
	eip712DomainTypeHash     = crypto.Keccak256Hash([]byte(eip712DomainTypeString))
)

// word left-pads b into a single 32-byte EIP-712 ABI word.
func word(b []byte) []byte {
	w := make([]byte, 32)
	copy(w[32-len(b):], b)
	return w
}

func uint256Word(n *big.Int) []byte      { return word(n.Bytes()) }
func addressWord(a common.Address) []byte { return word(a.Bytes()) }

// concatBytes joins byte slices left-to-right.
func concatBytes(parts ...[]byte) []byte {
	var out []byte
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}

// exchangeDomainSeparator is the CTF Exchange's EIP-712 domain separator
// (the "app domain" that ERC-7739 folds in at digest time).
func exchangeDomainSeparator(chainID *big.Int, exchange common.Address) common.Hash {
	return crypto.Keccak256Hash(concatBytes(
		eip712DomainTypeHash.Bytes(),
		domainNameHash.Bytes(),    // keccak("Polymarket CTF Exchange")
		domainVersionHash.Bytes(), // keccak("2")
		uint256Word(chainID),
		addressWord(exchange),
	))
}

// orderStructHash is the EIP-712 hashStruct of the Order ("contents"); field
// order matches orderTypeString verbatim.
func orderStructHash(o *Order) common.Hash {
	return crypto.Keccak256Hash(concatBytes(
		orderTypeHash.Bytes(),
		uint256Word(o.Salt),
		addressWord(o.Maker),
		addressWord(o.Signer),
		uint256Word(o.TokenId),
		uint256Word(o.MakerAmount),
		uint256Word(o.TakerAmount),
		uint256Word(big.NewInt(int64(o.Side))),
		uint256Word(big.NewInt(int64(o.SignatureType))),
		uint256Word(o.Timestamp),
		o.Metadata.Bytes(),
		o.Builder.Bytes(),
	))
}

// typedDataSignStructHash folds the deposit wallet's own EIP-712 domain in as
// plain TypedDataSign fields (verifyingContract = the deposit-wallet contract,
// salt = bytes32(0)).
func typedDataSignStructHash(chainID *big.Int, contentsHash common.Hash, depositWallet common.Address) common.Hash {
	return crypto.Keccak256Hash(concatBytes(
		soladyTypeHash.Bytes(),
		contentsHash.Bytes(),
		depositWalletNameHash.Bytes(),
		depositWalletVersionHash.Bytes(),
		uint256Word(chainID),
		addressWord(depositWallet),
		make([]byte, 32), // DEPOSIT_WALLET_DOMAIN_SALT = bytes32(0)
	))
}

// BuildSignedDepositWalletOrder builds a POLY_1271 order and produces the packed
// ERC-7739 signature blob. privateKey is the controlling EOA's key; the order's
// Maker and Signer must both be the deposit-wallet contract address.
func (b *Builder) BuildSignedDepositWalletOrder(privateKey *ecdsa.PrivateKey, data *OrderData, contract VerifyingContract) (*SignedOrder, error) {
	if data == nil {
		return nil, errors.New("order data is nil")
	}
	if data.SignatureType != POLY_1271 {
		return nil, fmt.Errorf("deposit-wallet signing requires POLY_1271 (3), got %d", data.SignatureType)
	}

	order, err := b.BuildOrder(data)
	if err != nil {
		return nil, err
	}
	// For POLY_1271 the maker and signer are both the deposit-wallet contract;
	// the EOA-equality check that the legacy path runs does not apply.
	if order.Maker != order.Signer {
		return nil, fmt.Errorf("POLY_1271 requires maker == signer (deposit wallet), got maker=%s signer=%s",
			order.Maker.Hex(), order.Signer.Hex())
	}

	sig, err := b.depositWalletSignature(privateKey, order, contract)
	if err != nil {
		return nil, err
	}
	return &SignedOrder{Order: *order, Signature: sig}, nil
}

// depositWalletSignature implements the 5-step ERC-7739 algorithm and returns
// the packed on-wire order.signature:
//
//	innerSig(65) || appDomainSep(32) || contentsHash(32) || contentsType || uint16_BE(len)
func (b *Builder) depositWalletSignature(privateKey *ecdsa.PrivateKey, order *Order, contract VerifyingContract) ([]byte, error) {
	exchange, err := VerifyingContractAddress(contract)
	if err != nil {
		return nil, err
	}

	appDomainSep := exchangeDomainSeparator(b.chainID, exchange)
	contentsHash := orderStructHash(order)
	tdsHash := typedDataSignStructHash(b.chainID, contentsHash, order.Signer)

	digest := crypto.Keccak256(concatBytes([]byte{0x19, 0x01}, appDomainSep.Bytes(), tdsHash.Bytes()))

	innerSig, err := crypto.Sign(digest, privateKey)
	if err != nil {
		return nil, err
	}
	// go-ethereum yields V in {0,1}; ERC-1271 verifiers (and eth_account) expect {27,28}.
	innerSig[64] += 27

	contentsType := []byte(orderTypeString)
	l := len(contentsType)
	return concatBytes(
		innerSig,
		appDomainSep.Bytes(),
		contentsHash.Bytes(),
		contentsType,
		[]byte{byte(l >> 8), byte(l)}, // uint16 big-endian
	), nil
}
