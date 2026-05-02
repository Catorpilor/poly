package orderv2

import (
	"math/big"
	"testing"
)

// jsMaxSafeInteger == 2^53 - 1, the largest integer JavaScript Number can
// represent exactly. The Polymarket CLOB server is JS — JSON.parse / parseInt
// of a numeric salt above this loses precision (or, beyond 2^63, signed-overflows
// or wraps depending on the parser). Either way the recomputed signature won't
// match what we signed and the server replies with the generic
// "Invalid order payload" — the exact failure observed on prod 2026-05-01
// for salts >= 2^63.
//
// The reference SDK's generateOrderSalt is `Math.round(Math.random() * Date.now())`,
// max ~1.7e12 — comfortably below this bound. Our defaultSalt must do the same.
const jsMaxSafeInteger = (int64(1) << 53) - 1

func TestDefaultSalt_FitsInJSSafeInteger(t *testing.T) {
	t.Parallel()

	// Sample many draws — defaultSalt is randomized, so a single call won't
	// catch a generator that occasionally exceeds the bound.
	max := big.NewInt(jsMaxSafeInteger)
	for i := 0; i < 10_000; i++ {
		salt := defaultSalt()
		if salt == nil || salt.Sign() < 0 {
			t.Fatalf("defaultSalt() returned nil or negative: %v", salt)
		}
		if salt.Cmp(max) > 0 {
			t.Fatalf("defaultSalt() = %s exceeds JS safe integer 2^53-1 = %d (iter %d)",
				salt.String(), jsMaxSafeInteger, i)
		}
	}
}
