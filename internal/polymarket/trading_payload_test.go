package polymarket

import (
	"encoding/json"
	"math/big"
	"strings"
	"testing"

	"github.com/Catorpilor/poly/internal/polymarket/orderv2"
)

// TestBuildOrderPayloadV2_SaltAboveInt64Range pins salt serialization to a
// decimal string. Regression guard for the V2 buy failure where salts in the
// upper half of uint64 (≥ 2^63) overflowed Int64() into a negative integer,
// causing the CLOB server to reject the order with
// `abi: negatively-signed value cannot be packed into uint parameter`.
//
// defaultSalt() draws uniformly from [0, 2^64), so ~50% of attempts land here.
func TestBuildOrderPayloadV2_SaltAboveInt64Range(t *testing.T) {
	t.Parallel()

	// 2^63 + 1 — first value that overflows int64 to negative when truncated.
	salt := new(big.Int).Add(new(big.Int).Lsh(big.NewInt(1), 63), big.NewInt(1))
	wantSalt := salt.String() // "9223372036854775809"

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

	want := `"salt":"` + wantSalt + `"`
	if !strings.Contains(string(body), want) {
		t.Fatalf("payload missing %s\nbody: %s", want, body)
	}
}

// TestBuildOrderPayloadV2_HasDeferExecAndPostOnly pins two top-level fields
// the CLOB schema validator requires (mirrors @polymarket/clob-client-v2's
// orderToJsonV2). Without them the server returns `Invalid order payload`.
func TestBuildOrderPayloadV2_HasDeferExecAndPostOnly(t *testing.T) {
	t.Parallel()

	signed := &orderv2.SignedOrder{
		Order: orderv2.Order{
			Salt:          big.NewInt(1),
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

	if got, ok := payload["deferExec"]; !ok || got != false {
		t.Errorf("deferExec = %v, ok=%v; want false present", got, ok)
	}
	if got, ok := payload["postOnly"]; !ok || got != false {
		t.Errorf("postOnly = %v, ok=%v; want false present", got, ok)
	}
}
