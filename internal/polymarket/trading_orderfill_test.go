package polymarket

import (
	"context"
	"testing"

	"github.com/ethereum/go-ethereum/common"
)

// OrderFill (issue #92) probes a placed order's fill state so the snipe
// confirm-then-arm path can distinguish matched / resting / killed / reaped.
func TestOrderFill(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name         string
		pollStatus   string
		matchedSize  string
		matchedPrice string
		wantMatched  float64
		wantPrice    float64
		wantOpen     bool
		wantFound    bool
	}{
		{name: "matched", pollStatus: "matched", matchedSize: "71.43", matchedPrice: "0.07",
			wantMatched: 71.43, wantPrice: 0.07, wantOpen: false, wantFound: true},
		{name: "resting live", pollStatus: "live", matchedSize: "0", matchedPrice: "0.07",
			wantMatched: 0, wantPrice: 0.07, wantOpen: true, wantFound: true},
		{name: "delayed", pollStatus: "delayed", matchedSize: "0", matchedPrice: "0.07",
			wantMatched: 0, wantPrice: 0.07, wantOpen: true, wantFound: true},
		{name: "partial still live", pollStatus: "live", matchedSize: "30", matchedPrice: "0.07",
			wantMatched: 30, wantPrice: 0.07, wantOpen: true, wantFound: true},
		{name: "canceled", pollStatus: "canceled", matchedSize: "0", matchedPrice: "0.07",
			wantMatched: 0, wantPrice: 0.07, wantOpen: false, wantFound: true},
		{name: "reaped 404", pollStatus: "404", wantFound: false},
		{name: "reaped empty body", pollStatus: "gone", wantFound: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clob := &fokFakeCLOB{
				pollStatuses: []string{tt.pollStatus},
				matchedSize:  tt.matchedSize,
				matchedPrice: tt.matchedPrice,
			}
			tc, _, creds := newFOKFixture(t, clob)

			matched, price, open, found, err := tc.OrderFill(
				context.Background(), common.HexToAddress("0xabc"), creds, "ord-x")
			if err != nil {
				t.Fatalf("OrderFill: %v", err)
			}
			if found != tt.wantFound {
				t.Fatalf("found = %v, want %v", found, tt.wantFound)
			}
			if !tt.wantFound {
				return
			}
			if matched != tt.wantMatched || price != tt.wantPrice || open != tt.wantOpen {
				t.Errorf("OrderFill = (%.2f, %.3f, open=%v), want (%.2f, %.3f, open=%v)",
					matched, price, open, tt.wantMatched, tt.wantPrice, tt.wantOpen)
			}
		})
	}
}
