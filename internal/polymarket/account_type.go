package polymarket

import (
	"context"
	"fmt"
	"math/big"

	"github.com/Catorpilor/poly/internal/polymarket/orderv2"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
)

// AccountType classifies a user's Polymarket trading account architecture,
// which determines the EIP-712 signature type the order signer must use.
//
// The string values are the canonical persisted form (users.account_type) and
// must stay in sync with migrations/006_account_type.sql.
type AccountType string

const (
	// AccountUnknown means the account could not be classified (e.g. the proxy
	// is not deployed yet). Callers should fall back to a default.
	AccountUnknown AccountType = ""
	// AccountLegacyProxy is the classic Polymarket proxy (POLY_PROXY, sig type 1).
	AccountLegacyProxy AccountType = "legacy_proxy"
	// AccountSafe is a Gnosis Safe proxy (POLY_GNOSIS_SAFE, sig type 2).
	AccountSafe AccountType = "safe"
	// AccountDepositWallet is a new-architecture deposit wallet that validates
	// signatures via ERC-1271/ERC-7739 (POLY_1271, sig type 3).
	AccountDepositWallet AccountType = "deposit_wallet"
)

// DepositWalletFactory is Polymarket's deposit-wallet factory on Polygon.
// A trading proxy whose factory() returns this address is a deposit wallet.
// Confirmed from clob-client-v2 source + on-chain. See docs/deposit-wallet-flow.md.
var DepositWalletFactory = common.HexToAddress("0x00000000000Fb5C9ADea0298D729A0CB3823Cc07")

// SignatureType maps an account type to the order signature type. Used when a
// proxy/contract account is the maker; a bare EOA maker uses orderv2.EOA and
// does not consult this.
func (t AccountType) SignatureType() orderv2.SignatureType {
	switch t {
	case AccountDepositWallet:
		return orderv2.POLY_1271
	case AccountSafe:
		return orderv2.POLY_GNOSIS_SAFE
	default: // AccountLegacyProxy and AccountUnknown
		return orderv2.POLY_PROXY
	}
}

// accountCodeCaller is the read-only on-chain surface the classifier needs.
// *ethclient.Client satisfies it.
type accountCodeCaller interface {
	CallContract(ctx context.Context, msg ethereum.CallMsg, blockNumber *big.Int) ([]byte, error)
	CodeAt(ctx context.Context, contract common.Address, blockNumber *big.Int) ([]byte, error)
}

// AccountClassifier determines the account type of a deployed trading account.
type AccountClassifier struct {
	client               accountCodeCaller
	depositWalletFactory common.Address
}

// NewAccountClassifier returns a classifier bound to the given chain client.
func NewAccountClassifier(client accountCodeCaller) *AccountClassifier {
	return &AccountClassifier{client: client, depositWalletFactory: DepositWalletFactory}
}

// Classify inspects an on-chain account and returns its type. It returns
// AccountUnknown (no error) when the address has no contract code (an EOA or a
// proxy that hasn't been deployed yet) — the caller decides the default.
func (c *AccountClassifier) Classify(ctx context.Context, account common.Address) (AccountType, error) {
	code, err := c.client.CodeAt(ctx, account, nil)
	if err != nil {
		return AccountUnknown, fmt.Errorf("failed to read code at %s: %w", account.Hex(), err)
	}
	if len(code) == 0 {
		return AccountUnknown, nil
	}

	// Deposit wallet: factory() == the deposit-wallet factory.
	if f, err := c.callAddress(ctx, account, "factory()"); err == nil && f == c.depositWalletFactory {
		return AccountDepositWallet, nil
	}

	// Gnosis Safe: getOwners() returns a well-formed array (reverts otherwise).
	if c.isSafe(ctx, account) {
		return AccountSafe, nil
	}

	// A deployed contract that's neither → treat as a legacy Polymarket proxy.
	return AccountLegacyProxy, nil
}

// callAddress invokes a no-arg view method that returns a single address.
func (c *AccountClassifier) callAddress(ctx context.Context, to common.Address, sig string) (common.Address, error) {
	res, err := c.client.CallContract(ctx, ethereum.CallMsg{To: &to, Data: methodSelector(sig)}, nil)
	if err != nil {
		return common.Address{}, err
	}
	if len(res) < 32 {
		return common.Address{}, fmt.Errorf("short result for %s", sig)
	}
	return common.BytesToAddress(res[12:32]), nil
}

// isSafe reports whether the account answers getOwners() (a Gnosis Safe method).
func (c *AccountClassifier) isSafe(ctx context.Context, addr common.Address) bool {
	res, err := c.client.CallContract(ctx, ethereum.CallMsg{To: &addr, Data: methodSelector("getOwners()")}, nil)
	return err == nil && len(res) >= 32
}

// methodSelector returns the 4-byte function selector for a Solidity signature.
func methodSelector(sig string) []byte {
	return crypto.Keccak256([]byte(sig))[:4]
}
