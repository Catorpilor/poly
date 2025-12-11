package encryption

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
)

// AESEncryptor handles AES-256-GCM encryption/decryption
type AESEncryptor struct {
	key []byte
}

// NewAESEncryptor creates a new AES encryptor with the given key
func NewAESEncryptor(keyHex string) (*AESEncryptor, error) {
	// Decode hex key
	key, err := hex.DecodeString(keyHex)
	if err != nil {
		return nil, fmt.Errorf("failed to decode hex key: %w", err)
	}

	// Ensure key is 32 bytes for AES-256
	if len(key) != 32 {
		return nil, fmt.Errorf("key must be 32 bytes for AES-256, got %d bytes", len(key))
	}

	return &AESEncryptor{
		key: key,
	}, nil
}

// NewAESEncryptorFromPassword creates an encryptor from a password
func NewAESEncryptorFromPassword(password string, salt string) *AESEncryptor {
	// Create key from password using SHA-256
	h := sha256.New()
	h.Write([]byte(password))
	h.Write([]byte(salt))
	key := h.Sum(nil)

	return &AESEncryptor{
		key: key,
	}
}

// Encrypt encrypts plaintext using AES-256-GCM
func (e *AESEncryptor) Encrypt(plaintext string) (string, error) {
	// Create AES cipher
	block, err := aes.NewCipher(e.key)
	if err != nil {
		return "", fmt.Errorf("failed to create cipher: %w", err)
	}

	// Create GCM
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", fmt.Errorf("failed to create GCM: %w", err)
	}

	// Create nonce
	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", fmt.Errorf("failed to create nonce: %w", err)
	}

	// Encrypt data
	ciphertext := gcm.Seal(nonce, nonce, []byte(plaintext), nil)

	// Return hex-encoded ciphertext
	return hex.EncodeToString(ciphertext), nil
}

// Decrypt decrypts ciphertext using AES-256-GCM
func (e *AESEncryptor) Decrypt(ciphertextHex string) (string, error) {
	// Decode hex ciphertext
	ciphertext, err := hex.DecodeString(ciphertextHex)
	if err != nil {
		return "", fmt.Errorf("failed to decode hex ciphertext: %w", err)
	}

	// Create AES cipher
	block, err := aes.NewCipher(e.key)
	if err != nil {
		return "", fmt.Errorf("failed to create cipher: %w", err)
	}

	// Create GCM
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", fmt.Errorf("failed to create GCM: %w", err)
	}

	// Extract nonce
	nonceSize := gcm.NonceSize()
	if len(ciphertext) < nonceSize {
		return "", fmt.Errorf("ciphertext too short")
	}

	nonce, ciphertext := ciphertext[:nonceSize], ciphertext[nonceSize:]

	// Decrypt data
	plaintext, err := gcm.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return "", fmt.Errorf("failed to decrypt: %w", err)
	}

	return string(plaintext), nil
}

// GenerateKey generates a new 32-byte key for AES-256
func GenerateKey() (string, error) {
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return "", fmt.Errorf("failed to generate key: %w", err)
	}
	return hex.EncodeToString(key), nil
}

// ValidateHexKey validates that a hex string is a valid 32-byte key
func ValidateHexKey(hexKey string) error {
	key, err := hex.DecodeString(hexKey)
	if err != nil {
		return fmt.Errorf("invalid hex encoding: %w", err)
	}

	if len(key) != 32 {
		return fmt.Errorf("key must be 32 bytes (64 hex characters), got %d bytes", len(key))
	}

	return nil
}