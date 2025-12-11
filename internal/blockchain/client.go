package blockchain

import (
	"context"
	"fmt"
	"math/big"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/Catorpilor/poly/internal/config"
)

// Client represents a blockchain client
type Client struct {
	eth    *ethclient.Client
	cfg    *config.BlockchainConfig
	chainID *big.Int
}

// NewClient creates a new blockchain client
func NewClient(cfg *config.BlockchainConfig) (*Client, error) {
	eth, err := ethclient.Dial(cfg.PolygonRPCURL)
	if err != nil {
		return nil, fmt.Errorf("failed to connect to Polygon RPC: %w", err)
	}

	// Get chain ID
	chainID, err := eth.ChainID(context.Background())
	if err != nil {
		return nil, fmt.Errorf("failed to get chain ID: %w", err)
	}

	return &Client{
		eth:     eth,
		cfg:     cfg,
		chainID: chainID,
	}, nil
}

// Close closes the blockchain client connection
func (c *Client) Close() {
	if c.eth != nil {
		c.eth.Close()
	}
}

// GetBalance gets the native token (MATIC) balance for an address
func (c *Client) GetBalance(ctx context.Context, address common.Address) (*big.Int, error) {
	balance, err := c.eth.BalanceAt(ctx, address, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to get balance: %w", err)
	}
	return balance, nil
}

// GetERC20Balance gets the balance of an ERC20 token for an address
func (c *Client) GetERC20Balance(ctx context.Context, tokenAddress, holderAddress common.Address) (*big.Int, error) {
	// ERC20 balanceOf method signature
	// First 4 bytes of Keccak256("balanceOf(address)")
	methodID := common.Hex2Bytes("70a08231")

	// Pad the address to 32 bytes
	paddedAddress := common.LeftPadBytes(holderAddress.Bytes(), 32)

	// Construct the call data
	data := append(methodID, paddedAddress...)

	// Make the call using ethereum.CallMsg
	msg := ethereum.CallMsg{
		To:   &tokenAddress,
		Data: data,
	}

	result, err := c.eth.CallContract(ctx, msg, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to call contract: %w", err)
	}

	// Convert result to big.Int
	balance := new(big.Int).SetBytes(result)
	return balance, nil
}

// GetClient returns the underlying ethereum client
func (c *Client) GetClient() *ethclient.Client {
	return c.eth
}

// GetChainID returns the chain ID
func (c *Client) GetChainID() *big.Int {
	return c.chainID
}