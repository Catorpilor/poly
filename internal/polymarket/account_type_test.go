package polymarket

import (
	"context"
	"errors"
	"math/big"
	"testing"

	"github.com/Catorpilor/poly/internal/polymarket/orderv2"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
)

// mockEthClient answers CodeAt with a fixed blob and CallContract by selector.
// A selector absent from responses simulates a reverting call.
type mockEthClient struct {
	code      []byte
	responses map[[4]byte][]byte
}

func (m *mockEthClient) CodeAt(_ context.Context, _ common.Address, _ *big.Int) ([]byte, error) {
	return m.code, nil
}

func (m *mockEthClient) CallContract(_ context.Context, msg ethereum.CallMsg, _ *big.Int) ([]byte, error) {
	if len(msg.Data) < 4 {
		return nil, errors.New("no selector")
	}
	var sel [4]byte
	copy(sel[:], msg.Data[:4])
	if r, ok := m.responses[sel]; ok {
		return r, nil
	}
	return nil, errors.New("execution reverted")
}

func sel(sig string) [4]byte {
	var s [4]byte
	copy(s[:], crypto.Keccak256([]byte(sig))[:4])
	return s
}

func addrWord(a common.Address) []byte {
	w := make([]byte, 32)
	copy(w[12:], a.Bytes())
	return w
}

func TestAccountClassifier(t *testing.T) {
	someContract := []byte{0x60, 0x80, 0x60, 0x40} // any non-empty code

	tests := []struct {
		name      string
		code      []byte
		responses map[[4]byte][]byte
		want      AccountType
	}{
		{
			name: "deposit wallet (factory == deposit factory)",
			code: someContract,
			responses: map[[4]byte][]byte{
				sel("factory()"): addrWord(DepositWalletFactory),
			},
			want: AccountDepositWallet,
		},
		{
			name: "factory returns a different address -> not deposit wallet",
			code: someContract,
			responses: map[[4]byte][]byte{
				sel("factory()"): addrWord(common.HexToAddress("0x00000000000000000000000000000000DeaDBeef")),
			},
			want: AccountLegacyProxy,
		},
		{
			name: "gnosis safe (getOwners answers)",
			code: someContract,
			responses: map[[4]byte][]byte{
				// abi-encoded address[] with one owner: offset, len, addr
				sel("getOwners()"): append(append(
					leftPad32(big.NewInt(0x20)), leftPad32(big.NewInt(1))...),
					addrWord(common.HexToAddress("0x1111111111111111111111111111111111111111"))...),
			},
			want: AccountSafe,
		},
		{
			name:      "legacy proxy (neither factory nor getOwners)",
			code:      someContract,
			responses: map[[4]byte][]byte{},
			want:      AccountLegacyProxy,
		},
		{
			name:      "no code -> unknown",
			code:      nil,
			responses: map[[4]byte][]byte{},
			want:      AccountUnknown,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := NewAccountClassifier(&mockEthClient{code: tt.code, responses: tt.responses})
			got, err := c.Classify(context.Background(), common.HexToAddress("0x4b2f0b7B91319419d52f01cAf7D73D453770318b"))
			if err != nil {
				t.Fatalf("Classify: %v", err)
			}
			if got != tt.want {
				t.Errorf("Classify = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestAccountTypeSignatureType(t *testing.T) {
	tests := []struct {
		at   AccountType
		want orderv2.SignatureType
	}{
		{AccountDepositWallet, orderv2.POLY_1271},
		{AccountSafe, orderv2.POLY_GNOSIS_SAFE},
		{AccountLegacyProxy, orderv2.POLY_PROXY},
		{AccountUnknown, orderv2.POLY_PROXY}, // safe default for proxy accounts
	}
	for _, tt := range tests {
		if got := tt.at.SignatureType(); got != tt.want {
			t.Errorf("%q.SignatureType() = %d, want %d", tt.at, got, tt.want)
		}
	}
}

func TestResolveOrderSigner(t *testing.T) {
	eoa := common.HexToAddress("0xf6C73cB22eEA470d479a3e40a1BaD1292f318B01")
	proxy := common.HexToAddress("0x4b2f0b7B91319419d52f01cAf7D73D453770318b")
	zero := common.Address{}

	tests := []struct {
		name        string
		proxy       common.Address
		accountType string
		wantMaker   common.Address
		wantSigner  common.Address
		wantSig     orderv2.SignatureType
	}{
		{"no proxy -> EOA order", zero, "", eoa, eoa, orderv2.EOA},
		{"proxy == eoa -> EOA order", eoa, "legacy_proxy", eoa, eoa, orderv2.EOA},
		{"deposit wallet -> 1271, maker==signer==proxy", proxy, "deposit_wallet", proxy, proxy, orderv2.POLY_1271},
		{"legacy proxy -> safe sig, signer=EOA", proxy, "legacy_proxy", proxy, eoa, orderv2.POLY_GNOSIS_SAFE},
		{"safe -> safe sig, signer=EOA", proxy, "safe", proxy, eoa, orderv2.POLY_GNOSIS_SAFE},
		{"unknown/empty -> legacy default (safe)", proxy, "", proxy, eoa, orderv2.POLY_GNOSIS_SAFE},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			maker, signer, sig := resolveOrderSigner(eoa, tt.proxy, tt.accountType)
			if maker != tt.wantMaker || signer != tt.wantSigner || sig != tt.wantSig {
				t.Errorf("resolveOrderSigner = (%s, %s, %d), want (%s, %s, %d)",
					maker.Hex(), signer.Hex(), sig, tt.wantMaker.Hex(), tt.wantSigner.Hex(), tt.wantSig)
			}
		})
	}
}

func leftPad32(n *big.Int) []byte {
	w := make([]byte, 32)
	b := n.Bytes()
	copy(w[32-len(b):], b)
	return w
}
