package polymarket

import (
	"encoding/json"
	"math/big"
	"strings"
	"testing"

	"github.com/Catorpilor/poly/internal/polymarket/orderv2"
)

// TestBuildOrderPayloadV2_SaltAboveInt64Range pins salt serialization as an
// unquoted JSON number for a value that would overflow Int64() to negative.
//
// Regression guard for two related bugs:
//  1. Original V2 buy failure where salts ≥ 2^63 overflowed Int64() to
//     negative, causing the CLOB to reject with `abi: negatively-signed
//     value cannot be packed into uint parameter`.
//  2. The follow-up where salt was sent as a JSON string, which the CLOB
//     schema rejected with the generic `Invalid order payload` (the SDK
//     emits salt as a JSON number, not a string).
//
// Using the bare *big.Int + big.Int.MarshalJSON lets values up to 2^256
// serialize as positive JSON numbers, matching the SDK shape.
func TestBuildOrderPayloadV2_SaltAboveInt64Range(t *testing.T) {
	t.Parallel()

	// 2^63 + 1 — first value that overflows int64 to negative when truncated.
	salt := new(big.Int).Add(new(big.Int).Lsh(big.NewInt(1), 63), big.NewInt(1))

	signed := &orderv2.SignedOrder{
		Order: orderv2.Order{
			Salt:          salt,
			TokenId:       big.NewInt(1),
			MakerAmount:   big.NewInt(1),
			TakerAmount:   big.NewInt(1),
			Side:          orderv2.BUY,
			SignatureType: orderv2.POLY_GNOSIS_SAFE,
			Timestamp:     big.NewInt(0),
			Expiration:    big.NewInt(0),
		},
		Signature: []byte{},
	}

	payload := buildOrderPayloadV2(signed, "owner-key", OrderTypeGTC)
	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	if strings.Contains(string(body), `"salt":-`) {
		t.Fatalf("payload contains negative salt — Int64() overflow:\n%s", body)
	}

	// Must be an unquoted JSON number, not a string. Both the SDK (parseInt
	// → Number) and the CLOB schema expect a numeric salt.
	want := `"salt":` + salt.String()
	if !strings.Contains(string(body), want) {
		t.Fatalf("payload missing %s\nbody: %s", want, body)
	}
	if strings.Contains(string(body), `"salt":"`) {
		t.Fatalf("salt is a JSON string — should be a number:\n%s", body)
	}
}
