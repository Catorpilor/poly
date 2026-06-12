package orderv2

import (
	"crypto/ecdsa"
	"crypto/rand"
	"errors"
	"fmt"
	"math"
	"math/big"
	"time"

	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/polymarket/go-order-utils/pkg/eip712"
	"github.com/polymarket/go-order-utils/pkg/signer"
)

// Canonical V2 EIP-712 type string. Field order is load-bearing — it drives
// the typehash computed below and must match the SDK's
// CTF_EXCHANGE_V2_ORDER_STRUCT verbatim.
const orderTypeString = "Order(uint256 salt,address maker,address signer,uint256 tokenId,uint256 makerAmount,uint256 takerAmount,uint8 side,uint8 signatureType,uint256 timestamp,bytes32 metadata,bytes32 builder)"

const (
	domainName    = "Polymarket CTF Exchange"
	domainVersion = "2"
)

var (
	domainNameHash    = crypto.Keccak256Hash([]byte(domainName))
	domainVersionHash = crypto.Keccak256Hash([]byte(domainVersion))

	orderTypeHash = crypto.Keccak256Hash([]byte(orderTypeString))

	// orderStructure is the abi.Type list passed to eip712.HashTypedDataV4.
	// First entry is the typehash; remaining entries match orderTypeString
	// fields in the same order.
	orderStructure = []abi.Type{
		eip712.Bytes32, // typehash
		eip712.Uint256, // salt
		eip712.Address, // maker
		eip712.Address, // signer
		eip712.Uint256, // tokenId
		eip712.Uint256, // makerAmount
		eip712.Uint256, // takerAmount
		eip712.Uint8,   // side
		eip712.Uint8,   // signatureType
		eip712.Uint256, // timestamp
		eip712.Bytes32, // metadata
		eip712.Bytes32, // builder
	}
)

// V2 verifying contracts. Same address on Polygon mainnet (chainId 137)
// and Amoy testnet (chainId 80002) per Polymarket docs as of 2026-04-30.
var v2Contracts = map[VerifyingContract]common.Address{
	CTFExchange:        common.HexToAddress("0xE111180000d2663C0091e4f400237545B87B996B"),
	NegRiskCTFExchange: common.HexToAddress("0xe2222d279d744050d28e00520010520000310F59"),
}

// Builder constructs and signs V2 orders.
//
// SaltGenerator and TimestampMillis are injectable for deterministic tests;
// production callers should leave them nil to get crypto/rand salts and
// time.Now().UnixMilli() timestamps.
type Builder struct {
	chainID         *big.Int
	saltGenerator   func() *big.Int
	timestampMillis func() int64
}

// NewBuilder returns a Builder bound to the given chain.
func NewBuilder(chainID int64) *Builder {
	return &Builder{
		chainID:         big.NewInt(chainID),
		saltGenerator:   defaultSalt,
		timestampMillis: func() int64 { return time.Now().UnixMilli() },
	}
}

// WithSaltGenerator overrides the salt source. Returns the receiver for chaining.
// Pass nil to restore the default crypto/rand source.
func (b *Builder) WithSaltGenerator(fn func() *big.Int) *Builder {
	if fn == nil {
		b.saltGenerator = defaultSalt
	} else {
		b.saltGenerator = fn
	}
	return b
}

// WithTimestampMillis overrides the timestamp source. Returns the receiver
// for chaining. Pass nil to restore time.Now().UnixMilli().
func (b *Builder) WithTimestampMillis(fn func() int64) *Builder {
	if fn == nil {
		b.timestampMillis = func() int64 { return time.Now().UnixMilli() }
	} else {
		b.timestampMillis = fn
	}
	return b
}

// VerifyingContractAddress returns the on-chain address for a given V2 exchange.
func VerifyingContractAddress(c VerifyingContract) (common.Address, error) {
	addr, ok := v2Contracts[c]
	if !ok {
		return common.Address{}, fmt.Errorf("unknown V2 verifying contract: %d", c)
	}
	return addr, nil
}

// BuildSignedOrder builds a V2 order, hashes it, signs it, and verifies the
// signature recovers to the signer address.
func (b *Builder) BuildSignedOrder(privateKey *ecdsa.PrivateKey, data *OrderData, contract VerifyingContract) (*SignedOrder, error) {
	order, err := b.BuildOrder(data)
	if err != nil {
		return nil, err
	}

	hash, err := b.BuildOrderHash(order, contract)
	if err != nil {
		return nil, err
	}

	sig, err := signer.Sign(privateKey, hash)
	if err != nil {
		return nil, err
	}

	ok, err := signer.ValidateSignature(order.Signer, hash, sig)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, errors.New("signature recovery did not match signer address")
	}

	return &SignedOrder{Order: *order, Signature: sig}, nil
}

// BuildOrder normalizes OrderData into an Order, parsing decimal strings and
// filling defaults (signer=maker, timestamp=now-ms, expiration=0).
func (b *Builder) BuildOrder(data *OrderData) (*Order, error) {
	if data == nil {
		return nil, errors.New("order data is nil")
	}
	if data.Maker == "" {
		return nil, errors.New("maker is required")
	}

	maker := common.HexToAddress(data.Maker)
	signerAddr := maker
	if data.Signer != "" {
		signerAddr = common.HexToAddress(data.Signer)
	}

	tokenID, err := parseBigInt("tokenId", data.TokenId)
	if err != nil {
		return nil, err
	}
	makerAmount, err := parseBigInt("makerAmount", data.MakerAmount)
	if err != nil {
		return nil, err
	}
	takerAmount, err := parseBigInt("takerAmount", data.TakerAmount)
	if err != nil {
		return nil, err
	}

	timestamp := big.NewInt(b.timestampMillis())
	if data.Timestamp != "" {
		timestamp, err = parseBigInt("timestamp", data.Timestamp)
		if err != nil {
			return nil, err
		}
	}

	expiration := big.NewInt(0)
	if data.Expiration != "" {
		expiration, err = parseBigInt("expiration", data.Expiration)
		if err != nil {
			return nil, err
		}
	}

	return &Order{
		Salt:          b.saltGenerator(),
		Maker:         maker,
		Signer:        signerAddr,
		TokenId:       tokenID,
		MakerAmount:   makerAmount,
		TakerAmount:   takerAmount,
		Side:          data.Side,
		SignatureType: data.SignatureType,
		Timestamp:     timestamp,
		Metadata:      data.Metadata,
		Builder:       data.Builder,
		Expiration:    expiration,
	}, nil
}

// BuildOrderHash computes the EIP-712 digest that gets signed for the given
// order against the named V2 verifying contract.
func (b *Builder) BuildOrderHash(order *Order, contract VerifyingContract) (OrderHash, error) {
	verifyingContract, err := VerifyingContractAddress(contract)
	if err != nil {
		return OrderHash{}, err
	}

	domainSeparator, err := eip712.BuildEIP712DomainSeparator(domainNameHash, domainVersionHash, b.chainID, verifyingContract)
	if err != nil {
		return OrderHash{}, err
	}

	values := []interface{}{
		orderTypeHash,
		order.Salt,
		order.Maker,
		order.Signer,
		order.TokenId,
		order.MakerAmount,
		order.TakerAmount,
		uint8(order.Side),
		uint8(order.SignatureType),
		order.Timestamp,
		[32]byte(order.Metadata),
		[32]byte(order.Builder),
	}

	return eip712.HashTypedDataV4(domainSeparator, orderStructure, values)
}

// parseBigInt parses a decimal string into a *big.Int, returning a wrapped
// error if the string is not a valid decimal integer.
func parseBigInt(name, s string) (*big.Int, error) {
	if s == "" {
		return nil, fmt.Errorf("%s is required", name)
	}
	v, ok := new(big.Int).SetString(s, 10)
	if !ok {
		return nil, fmt.Errorf("can't parse %s: %q as decimal *big.Int", name, s)
	}
	return v, nil
}

// defaultSalt mirrors the JS SDK's `Math.round(Math.random() * Date.now())`.
//
// The CLOB server is JS — JSON.parse / parseInt of a salt above
// Number.MAX_SAFE_INTEGER (2^53-1) loses precision, and above 2^63 most JSON
// libraries either signed-overflow or wrap. Either way the recomputed signature
// won't match what we signed, and the server replies with the generic
// "Invalid order payload". The SDK avoids this entirely by capping salt at
// `random * Date.now()` ≈ 1.7e12. We do the same — exactly — to stay
// byte-for-byte compatible. Salt only needs to be unique per (maker, timestamp)
// for replay protection; ~10^12 of randomness is ample.
func defaultSalt() *big.Int {
	// Draw a 53-bit unsigned int and convert to a [0,1) float64 — same
	// distribution as JS's Math.random().
	const mantissa53 = int64(1) << 53
	r, err := rand.Int(rand.Reader, big.NewInt(mantissa53))
	if err != nil {
		// crypto/rand.Reader is not expected to fail on Linux; fall back
		// to a time-derived salt rather than panicking.
		return big.NewInt(time.Now().UnixNano())
	}
	random01 := float64(r.Int64()) / float64(mantissa53)
	salt := math.Round(random01 * float64(time.Now().UnixMilli()))
	return new(big.Int).SetInt64(int64(salt))
}
