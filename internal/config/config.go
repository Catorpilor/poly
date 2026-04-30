package config

import (
	"fmt"
	"log"
	"os"
	"strconv"
	"time"

	"github.com/joho/godotenv"
)

// Config holds all application configuration
type Config struct {
	// Telegram configuration
	Telegram TelegramConfig

	// Database configuration
	Database DatabaseConfig

	// Blockchain configuration
	Blockchain BlockchainConfig

	// Polymarket configuration
	Polymarket PolymarketConfig

	// Redis configuration
	Redis RedisConfig

	// Security configuration
	Security SecurityConfig

	// Trading configuration
	Trading TradingConfig

	// Application configuration
	App AppConfig

	// Gnosis Safe configuration
	GnosisSafe GnosisSafeConfig

	// Builder Relayer configuration (for on-chain Safe transactions)
	Builder BuilderConfig
}

// TelegramConfig holds Telegram bot configuration
type TelegramConfig struct {
	BotToken    string
	BotUsername string // Bot username for deep links (without @)
}

// DatabaseConfig holds database configuration
type DatabaseConfig struct {
	URL               string
	MaxConnections    int
	MaxIdleConns      int
	ConnMaxLifetime   time.Duration
	ConnMaxIdleTime   time.Duration
}

// BlockchainConfig holds blockchain configuration
type BlockchainConfig struct {
	PolygonRPCURL    string
	DefaultGasPrice  uint64 // in Gwei
	MaxGasPrice      uint64 // in Gwei
	ChainID          int64
}

// PolymarketConfig holds Polymarket API configuration
type PolymarketConfig struct {
	CLOBAPIUrl  string
	DataAPIUrl  string // Polymarket Data API for positions
	GammaAPIURL string // Gamma API for market metadata
	APIKey      string
	// ConditionalTokens contract address on Polygon
	ConditionalTokensAddress string
	// USDC / pUSD collateral contract address
	USDCAddress string
	// CTFExchange contract address
	CTFExchangeAddress string
	// NegRiskCTFExchange contract address
	NegRiskExchangeAddress string
	// CollateralOnramp wraps USDC/USDC.e → pUSD (V2 only; empty pre-V2)
	CollateralOnrampAddress string
}

// RedisConfig holds Redis configuration
type RedisConfig struct {
	URL string
}

// SecurityConfig holds security-related configuration
type SecurityConfig struct {
	EncryptionKey        string
	RateLimitPerUser     int
	RateLimitWindowMins  int
	SessionTimeoutMins   int
}

// TradingConfig holds trading-related configuration
type TradingConfig struct {
	DefaultSlippagePercent float64
	MaxOrderSizeUSDC       float64
	MinOrderSizeUSDC       float64
}

// AppConfig holds general application configuration
type AppConfig struct {
	Environment string
	LogLevel    string
	Port        int
	LiveWebURL  string // URL for live web interface callback
}

// BuilderConfig holds Polymarket Builder Relayer configuration
type BuilderConfig struct {
	RelayerURL string
	APIKey     string
	Secret     string
	Passphrase string
}

// GnosisSafeConfig holds Gnosis Safe configuration
type GnosisSafeConfig struct {
	FactoryAddress   string
	MasterCopyAddress string
}

// Load loads configuration from environment variables
func Load() (*Config, error) {
	// Load .env file if it exists (for local development)
	if err := godotenv.Load(); err != nil {
		// It's okay if .env doesn't exist in production
		log.Printf("No .env file found: %v", err)
	}

	cfg := &Config{}

	// Load Telegram configuration
	cfg.Telegram.BotToken = getEnv("TELEGRAM_BOT_TOKEN", "")
	if cfg.Telegram.BotToken == "" {
		return nil, fmt.Errorf("TELEGRAM_BOT_TOKEN is required")
	}
	cfg.Telegram.BotUsername = getEnv("TELEGRAM_BOT_USERNAME", "poly_trade_test_bot")

	// Load Database configuration
	cfg.Database.URL = getEnv("DATABASE_URL", "")
	if cfg.Database.URL == "" {
		return nil, fmt.Errorf("DATABASE_URL is required")
	}
	cfg.Database.MaxConnections = getEnvInt("DATABASE_MAX_CONNECTIONS", 25)
	cfg.Database.MaxIdleConns = getEnvInt("DATABASE_MAX_IDLE_CONNECTIONS", 5)
	cfg.Database.ConnMaxLifetime = time.Hour
	cfg.Database.ConnMaxIdleTime = time.Minute * 10

	// Load Blockchain configuration
	cfg.Blockchain.PolygonRPCURL = getEnv("POLYGON_RPC_URL", "")
	if cfg.Blockchain.PolygonRPCURL == "" {
		return nil, fmt.Errorf("POLYGON_RPC_URL is required")
	}
	cfg.Blockchain.DefaultGasPrice = uint64(getEnvInt("DEFAULT_GAS_PRICE", 30))
	cfg.Blockchain.MaxGasPrice = uint64(getEnvInt("MAX_GAS_PRICE", 200))

	// Determine chain ID based on RPC URL (Mumbai testnet or Polygon mainnet)
	if contains(cfg.Blockchain.PolygonRPCURL, "mumbai") {
		cfg.Blockchain.ChainID = 80001 // Mumbai testnet
	} else {
		cfg.Blockchain.ChainID = 137 // Polygon mainnet
	}

	// Load Polymarket configuration
	cfg.Polymarket.CLOBAPIUrl = getEnv("POLYMARKET_CLOB_API_URL", "https://clob.polymarket.com")
	cfg.Polymarket.DataAPIUrl = getEnv("POLYMARKET_DATA_API_URL", "https://data-api.polymarket.com")
	cfg.Polymarket.GammaAPIURL = getEnv("POLYMARKET_GAMMA_API_URL", "https://gamma-api.polymarket.com")
	cfg.Polymarket.APIKey = getEnv("POLYMARKET_API_KEY", "")
	// Contract addresses (defaults are V2, post-2026-04-28 cutover).
	// ConditionalTokens address did not change between V1 and V2.
	cfg.Polymarket.ConditionalTokensAddress = getEnv("POLYMARKET_CONDITIONAL_TOKENS_ADDRESS", "0x4D97DCd97eC945f40cF65F87097ACe5EA0476045")
	cfg.Polymarket.USDCAddress = getEnv("POLYMARKET_COLLATERAL_ADDRESS", "0xC011a7E12a19f7B1f670d46F03B03f3342E82DFB") // pUSD
	cfg.Polymarket.CTFExchangeAddress = getEnv("POLYMARKET_CTF_EXCHANGE_ADDRESS", "0xE111180000d2663C0091e4f400237545B87B996B")
	cfg.Polymarket.NegRiskExchangeAddress = getEnv("POLYMARKET_NEGRISK_EXCHANGE_ADDRESS", "0xe2222d279d744050d28e00520010520000310F59")
	cfg.Polymarket.CollateralOnrampAddress = getEnv("POLYMARKET_COLLATERAL_ONRAMP_ADDRESS", "0x93070a847efEf7F70739046A929D47a521F5B8ee")

	// Load Redis configuration
	cfg.Redis.URL = getEnv("REDIS_URL", "redis://localhost:6379/0")

	// Load Security configuration
	cfg.Security.EncryptionKey = getEnv("ENCRYPTION_KEY", "")
	if cfg.Security.EncryptionKey == "" {
		return nil, fmt.Errorf("ENCRYPTION_KEY is required")
	}
	cfg.Security.RateLimitPerUser = getEnvInt("RATE_LIMIT_PER_USER", 60)
	cfg.Security.RateLimitWindowMins = getEnvInt("RATE_LIMIT_WINDOW_MINUTES", 1)
	cfg.Security.SessionTimeoutMins = getEnvInt("SESSION_TIMEOUT_MINUTES", 30)

	// Load Trading configuration
	cfg.Trading.DefaultSlippagePercent = getEnvFloat("DEFAULT_SLIPPAGE_PERCENT", 2.0)
	cfg.Trading.MaxOrderSizeUSDC = getEnvFloat("MAX_ORDER_SIZE_USDC", 10000.0)
	cfg.Trading.MinOrderSizeUSDC = getEnvFloat("MIN_ORDER_SIZE_USDC", 1.0)

	// Load App configuration
	cfg.App.Environment = getEnv("ENVIRONMENT", "development")
	cfg.App.LogLevel = getEnv("LOG_LEVEL", "debug")
	cfg.App.Port = getEnvInt("PORT", 8080)
	cfg.App.LiveWebURL = getEnv("LIVE_WEB_URL", "http://localhost:8081")

	// Load Builder Relayer configuration (optional - redeem won't work without it)
	cfg.Builder.RelayerURL = getEnv("POLYMARKET_BUILDER_RELAYER_URL", "https://relayer-v2.polymarket.com")
	cfg.Builder.APIKey = getEnv("POLYMARKET_BUILDER_API_KEY", "")
	cfg.Builder.Secret = getEnv("POLYMARKET_BUILDER_SECRET", "")
	cfg.Builder.Passphrase = getEnv("POLYMARKET_BUILDER_PASSPHRASE", "")

	// Load Gnosis Safe configuration
	cfg.GnosisSafe.FactoryAddress = getEnv("GNOSIS_SAFE_FACTORY_ADDRESS", "0xa6B71E26C5e0845f74c812102Ca7114b6a896AB2")
	cfg.GnosisSafe.MasterCopyAddress = getEnv("GNOSIS_SAFE_MASTER_COPY", "0xd9Db270c1B5E3Bd161E8c8503c55cEABeE709552")

	return cfg, nil
}

// Helper functions

func getEnv(key, defaultValue string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return defaultValue
}

func getEnvInt(key string, defaultValue int) int {
	if value := os.Getenv(key); value != "" {
		if intVal, err := strconv.Atoi(value); err == nil {
			return intVal
		}
	}
	return defaultValue
}

func getEnvFloat(key string, defaultValue float64) float64 {
	if value := os.Getenv(key); value != "" {
		if floatVal, err := strconv.ParseFloat(value, 64); err == nil {
			return floatVal
		}
	}
	return defaultValue
}

func getEnvBool(key string, defaultValue bool) bool {
	if value := os.Getenv(key); value != "" {
		if boolVal, err := strconv.ParseBool(value); err == nil {
			return boolVal
		}
	}
	return defaultValue
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(substr) == 0 ||
		(len(s) > 0 && len(substr) > 0 && s[0:len(substr)] == substr) ||
		(len(s) > len(substr) && s[len(s)-len(substr):] == substr) ||
		(len(s) > len(substr) && findSubstring(s, substr)))
}

func findSubstring(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}

// Validate validates the configuration
func (c *Config) Validate() error {
	// Validate trading configuration
	if c.Trading.MinOrderSizeUSDC > c.Trading.MaxOrderSizeUSDC {
		return fmt.Errorf("MIN_ORDER_SIZE_USDC cannot be greater than MAX_ORDER_SIZE_USDC")
	}

	if c.Trading.DefaultSlippagePercent < 0 || c.Trading.DefaultSlippagePercent > 100 {
		return fmt.Errorf("DEFAULT_SLIPPAGE_PERCENT must be between 0 and 100")
	}

	// Validate gas prices
	if c.Blockchain.DefaultGasPrice > c.Blockchain.MaxGasPrice {
		return fmt.Errorf("DEFAULT_GAS_PRICE cannot be greater than MAX_GAS_PRICE")
	}

	// Validate encryption key length (should be 32 bytes hex encoded = 64 characters)
	if len(c.Security.EncryptionKey) != 64 {
		return fmt.Errorf("ENCRYPTION_KEY must be 64 characters (32 bytes hex encoded)")
	}

	return nil
}