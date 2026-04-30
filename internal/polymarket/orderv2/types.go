// Package orderv2 implements Polymarket CTF Exchange V2 order construction
// and EIP-712 signing. Mirrors @polymarket/clob-client-v2 (TS) — Polymarket
// has not shipped an upstream Go SDK for V2 as of the 2026-04-28 cutover.
//
// V2 changes vs V1: the order struct drops nonce/feeRateBps/taker/expiration
// (fees are now computed at protocol match-time, not signed) and adds
// timestamp/metadata/builder. The EIP-712 domain version is bumped 1 → 2.
//
// `expiration` still exists on the API JSON payload, but is NOT included in
// the signed EIP-712 message. Including it would produce a wrong hash and
// silent rejection by the exchange.
package orderv2

import (
	"math/big"

	"github.com/ethereum/go-ethereum/common"
)

// Side encodes order direction. Matches the SDK's enum (BUY=0, SELL=1).
type Side uint8

const (
	BUY  Side = 0
	SELL Side = 1
)

// SignatureType encodes how the signer authenticates the order.
// POLY_1271 is new in V2 (EIP-1271 smart-contract wallet signatures).
type SignatureType uint8

const (
	EOA              SignatureType = 0
	POLY_PROXY       SignatureType = 1
	POLY_GNOSIS_SAFE SignatureType = 2
	POLY_1271        SignatureType = 3
)

// VerifyingContract identifies which V2 exchange will verify the order.
type VerifyingContract uint8

const (
	CTFExchange        VerifyingContract = 0
	NegRiskCTFExchange VerifyingContract = 1
)

// OrderData is the caller-friendly input shape (strings for big numbers,
// matching the vendored V1 SDK's idiom and trading.go's call site).
type OrderData struct {
	Maker         string // address (hex)
	Signer        string // address (hex); defaults to Maker if empty
	TokenId       string // decimal uint256
	MakerAmount   string // decimal uint256
	TakerAmount   string // decimal uint256
	Side          Side
	SignatureType SignatureType
	// Timestamp is the order creation time in milliseconds.
	// Replaces V1's nonce as the per-order uniqueness field.
	// If empty, BuildSignedOrder fills it with time.Now().UnixMilli().
	Timestamp string
	// Metadata is an optional bytes32. Defaults to zero.
	Metadata common.Hash
	// Builder is an optional bytes32 builder code. Defaults to zero.
	Builder common.Hash
	// Expiration is in the API payload but NOT signed. "0" = no expiry.
	Expiration string
}

// Order is the parsed, normalized form ready to hash and sign.
// Field order matches the V2 EIP-712 type string verbatim — do not reorder.
type Order struct {
	Salt          *big.Int
	Maker         common.Address
	Signer        common.Address
	TokenId       *big.Int
	MakerAmount   *big.Int
	TakerAmount   *big.Int
	Side          Side
	SignatureType SignatureType
	Timestamp     *big.Int
	Metadata      common.Hash // bytes32
	Builder       common.Hash // bytes32

	// Expiration is carried through to the API payload but excluded from
	// the EIP-712 hash. See package docs.
	Expiration *big.Int
}

// SignedOrder is an Order plus its 65-byte signature.
type SignedOrder struct {
	Order
	Signature []byte
}

// OrderHash is the final EIP-712 digest that gets signed
// (i.e. keccak256(0x1901 || domainSeparator || hashStruct(order))).
type OrderHash = common.Hash
