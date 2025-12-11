package wallet

import (
	"crypto/ecdsa"
	"encoding/hex"
	"fmt"
	"strings"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/Catorpilor/poly/pkg/encryption"
)

// Wallet represents a user's wallet
type Wallet struct {
	PrivateKey   *ecdsa.PrivateKey
	EOAAddress   common.Address
	ProxyAddress common.Address // Will be set after Gnosis Safe deployment
}

// Manager handles wallet operations
type Manager struct {
	encryptor *encryption.AESEncryptor
}

// NewManager creates a new wallet manager
func NewManager(encryptionKey string) (*Manager, error) {
	encryptor, err := encryption.NewAESEncryptor(encryptionKey)
	if err != nil {
		return nil, fmt.Errorf("failed to create encryptor: %w", err)
	}

	return &Manager{
		encryptor: encryptor,
	}, nil
}

// ImportPrivateKey imports a private key from hex string
func (m *Manager) ImportPrivateKey(privateKeyHex string) (*Wallet, error) {
	// Clean the private key string
	privateKeyHex = strings.TrimSpace(privateKeyHex)
	privateKeyHex = strings.TrimPrefix(privateKeyHex, "0x")
	privateKeyHex = strings.TrimPrefix(privateKeyHex, "0X")

	// Validate hex string length (should be 64 characters for 32 bytes)
	if len(privateKeyHex) != 64 {
		return nil, fmt.Errorf("private key must be 64 hex characters (32 bytes), got %d", len(privateKeyHex))
	}

	// Decode hex to bytes
	privateKeyBytes, err := hex.DecodeString(privateKeyHex)
	if err != nil {
		return nil, fmt.Errorf("invalid hex encoding: %w", err)
	}

	// Convert to ECDSA private key
	privateKey, err := crypto.ToECDSA(privateKeyBytes)
	if err != nil {
		return nil, fmt.Errorf("invalid private key: %w", err)
	}

	// Derive public key and address
	publicKey := privateKey.Public()
	publicKeyECDSA, ok := publicKey.(*ecdsa.PublicKey)
	if !ok {
		return nil, fmt.Errorf("failed to cast public key to ECDSA")
	}

	address := crypto.PubkeyToAddress(*publicKeyECDSA)

	return &Wallet{
		PrivateKey: privateKey,
		EOAAddress: address,
	}, nil
}

// GenerateNewWallet generates a new random wallet
func (m *Manager) GenerateNewWallet() (*Wallet, error) {
	// Generate new private key
	privateKey, err := crypto.GenerateKey()
	if err != nil {
		return nil, fmt.Errorf("failed to generate private key: %w", err)
	}

	// Derive address
	address := crypto.PubkeyToAddress(privateKey.PublicKey)

	return &Wallet{
		PrivateKey: privateKey,
		EOAAddress: address,
	}, nil
}

// EncryptPrivateKey encrypts a private key for storage
func (m *Manager) EncryptPrivateKey(wallet *Wallet) (string, error) {
	if wallet.PrivateKey == nil {
		return "", fmt.Errorf("wallet has no private key")
	}

	// Convert private key to hex
	privateKeyBytes := crypto.FromECDSA(wallet.PrivateKey)
	privateKeyHex := hex.EncodeToString(privateKeyBytes)

	// Encrypt the private key
	encrypted, err := m.encryptor.Encrypt(privateKeyHex)
	if err != nil {
		return "", fmt.Errorf("failed to encrypt private key: %w", err)
	}

	return encrypted, nil
}

// DecryptPrivateKey decrypts a stored private key
func (m *Manager) DecryptPrivateKey(encryptedKey string) (*Wallet, error) {
	// Decrypt the private key
	privateKeyHex, err := m.encryptor.Decrypt(encryptedKey)
	if err != nil {
		return nil, fmt.Errorf("failed to decrypt private key: %w", err)
	}

	// Import the decrypted private key
	return m.ImportPrivateKey(privateKeyHex)
}

// ValidatePrivateKey validates a private key hex string
func ValidatePrivateKey(privateKeyHex string) error {
	// Clean the private key string
	privateKeyHex = strings.TrimSpace(privateKeyHex)
	privateKeyHex = strings.TrimPrefix(privateKeyHex, "0x")
	privateKeyHex = strings.TrimPrefix(privateKeyHex, "0X")

	// Check length
	if len(privateKeyHex) != 64 {
		return fmt.Errorf("private key must be 64 hex characters (32 bytes), got %d", len(privateKeyHex))
	}

	// Try to decode hex
	_, err := hex.DecodeString(privateKeyHex)
	if err != nil {
		return fmt.Errorf("invalid hex encoding: %w", err)
	}

	// Try to convert to ECDSA key
	privateKeyBytes, _ := hex.DecodeString(privateKeyHex)
	_, err = crypto.ToECDSA(privateKeyBytes)
	if err != nil {
		return fmt.Errorf("invalid private key format: %w", err)
	}

	return nil
}

// GetAddressFromPrivateKey derives an Ethereum address from a private key
func GetAddressFromPrivateKey(privateKeyHex string) (string, error) {
	// Clean the private key string
	privateKeyHex = strings.TrimSpace(privateKeyHex)
	privateKeyHex = strings.TrimPrefix(privateKeyHex, "0x")
	privateKeyHex = strings.TrimPrefix(privateKeyHex, "0X")

	// Decode and convert to ECDSA
	privateKeyBytes, err := hex.DecodeString(privateKeyHex)
	if err != nil {
		return "", fmt.Errorf("invalid hex encoding: %w", err)
	}

	privateKey, err := crypto.ToECDSA(privateKeyBytes)
	if err != nil {
		return "", fmt.Errorf("invalid private key: %w", err)
	}

	// Derive address
	address := crypto.PubkeyToAddress(privateKey.PublicKey)
	return address.Hex(), nil
}