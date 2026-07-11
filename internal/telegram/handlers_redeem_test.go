package telegram

import (
	"math/big"
	"strings"
	"testing"

	"github.com/Catorpilor/poly/internal/polymarket"
)

// negRiskTokenIDs must order tokens by the Data API's outcomeIndex — the
// neg-risk adapter's amounts array is positional. The display label is
// unreliable: casing varies across Polymarket APIs ("Yes" vs "YES") and
// multi-outcome markets use team/candidate names.
func TestNegRiskTokenIDs_OrdersByOutcomeIndex(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		positions  []*polymarket.RedeemablePositionInfo
		wantToken0 string // "" means nil
		wantToken1 string
		wantErr    string
	}{
		{
			name: "title-case Yes at index 0 (historical prod shape)",
			positions: []*polymarket.RedeemablePositionInfo{
				{Outcome: "Yes", OutcomeIndex: 0, Asset: "111", OppositeAsset: "222"},
			},
			wantToken0: "111",
			wantToken1: "222",
		},
		{
			name: "uppercase YES at index 0 must not flip sides",
			positions: []*polymarket.RedeemablePositionInfo{
				{Outcome: "YES", OutcomeIndex: 0, Asset: "111", OppositeAsset: "222"},
			},
			wantToken0: "111",
			wantToken1: "222",
		},
		{
			name: "team-name outcome at index 0 must not flip sides",
			positions: []*polymarket.RedeemablePositionInfo{
				{Outcome: "KNICKS", OutcomeIndex: 0, Asset: "111", OppositeAsset: "222"},
			},
			wantToken0: "111",
			wantToken1: "222",
		},
		{
			name: "No side at index 1",
			positions: []*polymarket.RedeemablePositionInfo{
				{Outcome: "No", OutcomeIndex: 1, Asset: "333", OppositeAsset: "444"},
			},
			wantToken0: "444",
			wantToken1: "333",
		},
		{
			name: "both sides held, consistent assignment",
			positions: []*polymarket.RedeemablePositionInfo{
				{Outcome: "Yes", OutcomeIndex: 0, Asset: "111", OppositeAsset: "222"},
				{Outcome: "No", OutcomeIndex: 1, Asset: "222", OppositeAsset: "111"},
			},
			wantToken0: "111",
			wantToken1: "222",
		},
		{
			name: "missing opposite asset leaves other side nil",
			positions: []*polymarket.RedeemablePositionInfo{
				{Outcome: "Yes", OutcomeIndex: 0, Asset: "111", OppositeAsset: ""},
			},
			wantToken0: "111",
			wantToken1: "",
		},
		{
			name: "out-of-range outcome index rejected",
			positions: []*polymarket.RedeemablePositionInfo{
				{Outcome: "X", OutcomeIndex: 2, Asset: "111", OppositeAsset: "222"},
			},
			wantErr: "outcome index",
		},
		{
			name: "invalid token id rejected",
			positions: []*polymarket.RedeemablePositionInfo{
				{Outcome: "Yes", OutcomeIndex: 0, Asset: "not-a-number", OppositeAsset: "222"},
			},
			wantErr: "invalid token ID",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			token0, token1, err := negRiskTokenIDs(tt.positions)

			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want containing %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			assertTokenID(t, "token0", token0, tt.wantToken0)
			assertTokenID(t, "token1", token1, tt.wantToken1)
		})
	}
}

// /redeem is unavailable for Deposit Wallet accounts (ADR 0003): the relayer
// path signs Gnosis SafeTx hashes, which a deposit-wallet contract cannot
// validate, and Polymarket auto-redeems winners anyway.
func TestRedeemUnavailable(t *testing.T) {
	t.Parallel()

	tests := []struct {
		accountType string
		blocked     bool
	}{
		{string(polymarket.AccountDepositWallet), true},
		{string(polymarket.AccountLegacyProxy), false},
		{string(polymarket.AccountSafe), false},
		{"", false}, // legacy rows default to proxy behavior
	}
	for _, tt := range tests {
		msg, blocked := redeemUnavailable(tt.accountType)
		if blocked != tt.blocked {
			t.Errorf("redeemUnavailable(%q) blocked = %v, want %v", tt.accountType, blocked, tt.blocked)
		}
		if blocked && !strings.Contains(msg, "automatically") {
			t.Errorf("blocked message should explain winnings arrive automatically, got %q", msg)
		}
	}
}

func assertTokenID(t *testing.T, label string, got *big.Int, want string) {
	t.Helper()
	if want == "" {
		if got != nil {
			t.Errorf("%s = %s, want nil", label, got)
		}
		return
	}
	if got == nil || got.String() != want {
		t.Errorf("%s = %v, want %s", label, got, want)
	}
}
