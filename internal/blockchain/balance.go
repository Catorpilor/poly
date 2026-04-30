package blockchain

import (
	"context"
	"fmt"
	"math/big"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/ethclient"
)

// Balance represents token and native currency balances
type Balance struct {
	MATIC *big.Int // Native MATIC balance (for gas)
	USDC  *big.Int // USDC balance (for trading)
}

// BalanceChecker handles balance queries
type BalanceChecker struct {
	client *ethclient.Client
}

// NewBalanceChecker creates a new balance checker
func NewBalanceChecker(client *ethclient.Client) *BalanceChecker {
	return &BalanceChecker{client: client}
}

// GetBalances fetches both MATIC and USDC balances for an address
func (bc *BalanceChecker) GetBalances(ctx context.Context, address common.Address) (*Balance, error) {
	balance := &Balance{}

	// Get MATIC balance
	maticBalance, err := bc.client.BalanceAt(ctx, address, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to get MATIC balance: %w", err)
	}
	balance.MATIC = maticBalance

	// Get collateral balance (pUSD on V2, USDC.e on V1 — selected via InitAddresses).
	usdcBalance, err := bc.getERC20Balance(ctx, address, USDCAddress)
	if err != nil {
		return nil, fmt.Errorf("failed to get collateral balance: %w", err)
	}
	balance.USDC = usdcBalance

	return balance, nil
}

// getERC20Balance queries the balance of an ERC20 token
func (bc *BalanceChecker) getERC20Balance(ctx context.Context, owner common.Address, tokenAddress common.Address) (*big.Int, error) {
	// ERC20 balanceOf method signature
	// balanceOf(address) returns (uint256)
	// Method ID: 0x70a08231 (first 4 bytes of keccak256("balanceOf(address)"))
	methodID := common.FromHex("0x70a08231")
	paddedAddress := common.LeftPadBytes(owner.Bytes(), 32)

	// Prepare the call data
	data := append(methodID, paddedAddress...)

	// Call the contract
	msg := ethereum.CallMsg{
		To:   &tokenAddress,
		Data: data,
	}

	result, err := bc.client.CallContract(ctx, msg, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to call balanceOf: %w", err)
	}

	// Parse the result as big.Int
	if len(result) == 0 {
		return big.NewInt(0), nil
	}

	balance := new(big.Int).SetBytes(result)
	return balance, nil
}

// GetAllBalances fetches balances for both EOA and Proxy addresses
func (bc *BalanceChecker) GetAllBalances(ctx context.Context, eoaAddress, proxyAddress common.Address) (eoaBalance, proxyBalance *Balance, err error) {
	// Get EOA balances
	eoaBalance, err = bc.GetBalances(ctx, eoaAddress)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to get EOA balances: %w", err)
	}

	// Get Proxy balances
	proxyBalance, err = bc.GetBalances(ctx, proxyAddress)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to get proxy balances: %w", err)
	}

	return eoaBalance, proxyBalance, nil
}

// FormatBalance converts balance from smallest unit to human readable format
func FormatBalance(balance *big.Int, decimals int) string {
	if balance == nil {
		return "0"
	}

	// Create divisor (10^decimals)
	divisor := new(big.Float).SetInt(new(big.Int).Exp(
		big.NewInt(10),
		big.NewInt(int64(decimals)),
		nil,
	))

	// Convert to float for display
	balanceFloat := new(big.Float).SetInt(balance)
	result := new(big.Float).Quo(balanceFloat, divisor)

	// Format with appropriate precision
	return fmt.Sprintf("%.6f", result)
}

// FormatUSDC formats USDC balance (6 decimals)
func FormatUSDC(balance *big.Int) string {
	return FormatBalance(balance, 6)
}

// FormatMATIC formats MATIC balance (18 decimals)
func FormatMATIC(balance *big.Int) string {
	return FormatBalance(balance, 18)
}

// GetERC1155Balance queries the balance of an ERC1155 token on the ConditionalTokens contract.
// Used for neg-risk redemptions where explicit token amounts are required.
func (bc *BalanceChecker) GetERC1155Balance(ctx context.Context, owner common.Address, tokenID *big.Int) (*big.Int, error) {
	// ERC-1155 balanceOf(address,uint256) returns (uint256)
	// Method ID: 0x00fdd58e
	methodID := common.FromHex("0x00fdd58e")
	paddedOwner := common.LeftPadBytes(owner.Bytes(), 32)
	paddedTokenID := common.LeftPadBytes(tokenID.Bytes(), 32)

	data := append(methodID, paddedOwner...)
	data = append(data, paddedTokenID...)

	msg := ethereum.CallMsg{
		To:   &ConditionalTokensAddress,
		Data: data,
	}

	result, err := bc.client.CallContract(ctx, msg, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to call ERC1155 balanceOf: %w", err)
	}

	if len(result) == 0 {
		return big.NewInt(0), nil
	}

	return new(big.Int).SetBytes(result), nil
}

// Polymarket contract addresses on Polygon
var (
	ConditionalTokensAddress = common.HexToAddress("0x4D97DCd97eC945f40cF65F87097ACe5EA0476045")
	CTFExchangeAddress       = common.HexToAddress("0x4bFb41d5B3570DeFd03C39a9A4D8dE6Bd8B8982E")
	NegRiskExchangeAddress   = common.HexToAddress("0xC5d563A36AE78145C45a50134d48A1215220f80a")
)

// IsApprovedForAll checks if an operator is approved to transfer ERC-1155 tokens
func (bc *BalanceChecker) IsApprovedForAll(ctx context.Context, owner, operator common.Address) (bool, error) {
	// ERC-1155 isApprovedForAll(address,address) returns (bool)
	// Method ID: 0xe985e9c5
	methodID := common.FromHex("0xe985e9c5")
	paddedOwner := common.LeftPadBytes(owner.Bytes(), 32)
	paddedOperator := common.LeftPadBytes(operator.Bytes(), 32)

	data := append(methodID, paddedOwner...)
	data = append(data, paddedOperator...)

	msg := ethereum.CallMsg{
		To:   &ConditionalTokensAddress,
		Data: data,
	}

	result, err := bc.client.CallContract(ctx, msg, nil)
	if err != nil {
		return false, fmt.Errorf("failed to call isApprovedForAll: %w", err)
	}

	if len(result) == 0 {
		return false, nil
	}

	// Result is a 32-byte bool (1 = true, 0 = false)
	return new(big.Int).SetBytes(result).Cmp(big.NewInt(0)) != 0, nil
}

// GetERC20Allowance returns the current allowance of `spender` over `owner`'s
// balance for the ERC-20 token at `tokenAddress`. Used for reading pUSD
// allowances against the V2 exchanges before deciding whether to re-approve.
func (bc *BalanceChecker) GetERC20Allowance(ctx context.Context, tokenAddress, owner, spender common.Address) (*big.Int, error) {
	data, err := EncodeAllowanceCall(owner, spender)
	if err != nil {
		return nil, fmt.Errorf("encode allowance call: %w", err)
	}

	result, err := bc.client.CallContract(ctx, ethereum.CallMsg{To: &tokenAddress, Data: data}, nil)
	if err != nil {
		return nil, fmt.Errorf("call allowance: %w", err)
	}
	if len(result) == 0 {
		return big.NewInt(0), nil
	}
	return new(big.Int).SetBytes(result), nil
}

// CheckExchangeApproval checks if the proxy has approved the appropriate exchange
func (bc *BalanceChecker) CheckExchangeApproval(ctx context.Context, proxyAddress common.Address, isNegRisk bool) (bool, common.Address, error) {
	var exchangeAddress common.Address
	if isNegRisk {
		exchangeAddress = NegRiskExchangeAddress
	} else {
		exchangeAddress = CTFExchangeAddress
	}

	approved, err := bc.IsApprovedForAll(ctx, proxyAddress, exchangeAddress)
	return approved, exchangeAddress, err
}