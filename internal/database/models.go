package database

import (
	"database/sql/driver"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
)

// JSONB is a custom type for handling JSONB columns
type JSONB map[string]interface{}

// Value implements the driver.Valuer interface
func (j JSONB) Value() (driver.Value, error) {
	if j == nil {
		return nil, nil
	}
	return json.Marshal(j)
}

// Scan implements the sql.Scanner interface
func (j *JSONB) Scan(src interface{}) error {
	if src == nil {
		*j = make(JSONB)
		return nil
	}

	switch v := src.(type) {
	case []byte:
		return json.Unmarshal(v, j)
	case string:
		return json.Unmarshal([]byte(v), j)
	default:
		return fmt.Errorf("unsupported type for JSONB: %T", src)
	}
}

// User represents a Telegram bot user
type User struct {
	TelegramID   int64     `json:"telegram_id" db:"telegram_id"`
	Username     string    `json:"username" db:"username"`
	EOAAddress   string    `json:"eoa_address" db:"eoa_address"`
	ProxyAddress string    `json:"proxy_address" db:"proxy_address"`
	EncryptedKey string    `json:"-" db:"encrypted_key"` // Never expose in JSON
	Settings     JSONB     `json:"settings" db:"settings"`
	IsActive     bool      `json:"is_active" db:"is_active"`
	CreatedAt    time.Time `json:"created_at" db:"created_at"`
	UpdatedAt    time.Time `json:"updated_at" db:"updated_at"`
}

// Market represents a Polymarket market
type Market struct {
	MarketID        string         `json:"market_id" db:"market_id"`
	QuickAccessUUID pgtype.UUID    `json:"quick_access_uuid" db:"quick_access_uuid"`
	Title           string         `json:"title" db:"title"`
	ConditionID     string         `json:"condition_id" db:"condition_id"`
	TokenIDs        JSONB          `json:"token_ids" db:"token_ids"`
	CachedData      JSONB          `json:"cached_data" db:"cached_data"`
	IsActive        bool           `json:"is_active" db:"is_active"`
	EndsAt          *time.Time     `json:"ends_at" db:"ends_at"`
	CreatedAt       time.Time      `json:"created_at" db:"created_at"`
	UpdatedAt       time.Time      `json:"updated_at" db:"updated_at"`
}

// Order represents a trading order
type Order struct {
	OrderID      string     `json:"order_id" db:"order_id"`
	TelegramID   int64      `json:"telegram_id" db:"telegram_id"`
	MarketID     string     `json:"market_id" db:"market_id"`
	Side         OrderSide  `json:"side" db:"side"`
	Outcome      Outcome    `json:"outcome" db:"outcome"`
	Size         float64    `json:"size" db:"size"`
	Price        float64    `json:"price" db:"price"`
	Status       string     `json:"status" db:"status"`
	Filled       float64    `json:"filled" db:"filled"`
	TxHash       *string    `json:"tx_hash" db:"tx_hash"`
	ErrorMessage *string    `json:"error_message" db:"error_message"`
	CreatedAt    time.Time  `json:"created_at" db:"created_at"`
	UpdatedAt    time.Time  `json:"updated_at" db:"updated_at"`
	ExecutedAt   *time.Time `json:"executed_at" db:"executed_at"`
}

// Position represents a user's position in a market
type Position struct {
	TelegramID    int64     `json:"telegram_id" db:"telegram_id"`
	MarketID      string    `json:"market_id" db:"market_id"`
	PositionID    string    `json:"position_id" db:"position_id"`
	Outcome       Outcome   `json:"outcome" db:"outcome"`
	Shares        float64   `json:"shares" db:"shares"`
	AvgPrice      *float64  `json:"avg_price" db:"avg_price"`
	LastPrice     *float64  `json:"last_price" db:"last_price"`
	UnrealizedPNL *float64  `json:"unrealized_pnl" db:"unrealized_pnl"`
	CreatedAt     time.Time `json:"created_at" db:"created_at"`
	UpdatedAt     time.Time `json:"updated_at" db:"updated_at"`
}

// Session represents a user session
type Session struct {
	SessionID    pgtype.UUID `json:"session_id" db:"session_id"`
	TelegramID   int64       `json:"telegram_id" db:"telegram_id"`
	IsActive     bool        `json:"is_active" db:"is_active"`
	LastActivity time.Time   `json:"last_activity" db:"last_activity"`
	ExpiresAt    time.Time   `json:"expires_at" db:"expires_at"`
	CreatedAt    time.Time   `json:"created_at" db:"created_at"`
}

// AuditLog represents an audit log entry
type AuditLog struct {
	ID         int       `json:"id" db:"id"`
	TelegramID *int64    `json:"telegram_id" db:"telegram_id"`
	Action     string    `json:"action" db:"action"`
	Details    JSONB     `json:"details" db:"details"`
	IPAddress  *string   `json:"ip_address" db:"ip_address"`
	UserAgent  *string   `json:"user_agent" db:"user_agent"`
	CreatedAt  time.Time `json:"created_at" db:"created_at"`
}

// PriceAlert represents a price alert
type PriceAlert struct {
	ID             int        `json:"id" db:"id"`
	TelegramID     int64      `json:"telegram_id" db:"telegram_id"`
	MarketID       string     `json:"market_id" db:"market_id"`
	Outcome        Outcome    `json:"outcome" db:"outcome"`
	AlertType      AlertType  `json:"alert_type" db:"alert_type"`
	PriceThreshold float64    `json:"price_threshold" db:"price_threshold"`
	IsActive       bool       `json:"is_active" db:"is_active"`
	TriggeredAt    *time.Time `json:"triggered_at" db:"triggered_at"`
	CreatedAt      time.Time  `json:"created_at" db:"created_at"`
}

// OrderSide represents the side of an order (BUY or SELL)
type OrderSide string

const (
	OrderSideBuy  OrderSide = "BUY"
	OrderSideSell OrderSide = "SELL"
)

// Outcome represents the outcome of a position (YES or NO)
type Outcome string

const (
	OutcomeYes Outcome = "YES"
	OutcomeNo  Outcome = "NO"
)

// AlertType represents the type of price alert
type AlertType string

const (
	AlertTypeAbove AlertType = "ABOVE"
	AlertTypeBelow AlertType = "BELOW"
)

// OrderStatus represents the status of an order
type OrderStatus string

const (
	OrderStatusPending   OrderStatus = "PENDING"
	OrderStatusOpen      OrderStatus = "OPEN"
	OrderStatusPartial   OrderStatus = "PARTIAL"
	OrderStatusFilled    OrderStatus = "FILLED"
	OrderStatusCancelled OrderStatus = "CANCELLED"
	OrderStatusFailed    OrderStatus = "FAILED"
)

// Validate validates the Order
func (o *Order) Validate() error {
	if o.Size <= 0 {
		return fmt.Errorf("order size must be positive")
	}
	if o.Price < 0 || o.Price > 1 {
		return fmt.Errorf("order price must be between 0 and 1")
	}
	if o.Side != OrderSideBuy && o.Side != OrderSideSell {
		return fmt.Errorf("invalid order side: %s", o.Side)
	}
	if o.Outcome != OutcomeYes && o.Outcome != OutcomeNo {
		return fmt.Errorf("invalid outcome: %s", o.Outcome)
	}
	return nil
}

// Validate validates the PriceAlert
func (p *PriceAlert) Validate() error {
	if p.PriceThreshold < 0 || p.PriceThreshold > 1 {
		return fmt.Errorf("price threshold must be between 0 and 1")
	}
	if p.AlertType != AlertTypeAbove && p.AlertType != AlertTypeBelow {
		return fmt.Errorf("invalid alert type: %s", p.AlertType)
	}
	if p.Outcome != OutcomeYes && p.Outcome != OutcomeNo {
		return fmt.Errorf("invalid outcome: %s", p.Outcome)
	}
	return nil
}