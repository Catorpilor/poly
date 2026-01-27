package repositories

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/Catorpilor/poly/internal/database"
)

// LoginTokenRepository defines the interface for login token data operations
type LoginTokenRepository interface {
	Create(ctx context.Context, expiresIn time.Duration) (*database.LoginToken, error)
	GetByToken(ctx context.Context, token string) (*database.LoginToken, error)
	Authenticate(ctx context.Context, token string, telegramID int64, walletAddr, proxyAddr string) error
	MarkUsed(ctx context.Context, token string) (*database.LoginToken, error)
	CleanupExpired(ctx context.Context) (int64, error)
}

// loginTokenRepo implements LoginTokenRepository interface
type loginTokenRepo struct {
	db *database.DB
}

// NewLoginTokenRepository creates a new login token repository
func NewLoginTokenRepository(db *database.DB) LoginTokenRepository {
	return &loginTokenRepo{db: db}
}

// Create creates a new login token with the specified expiration duration
func (r *loginTokenRepo) Create(ctx context.Context, expiresIn time.Duration) (*database.LoginToken, error) {
	query := `
		INSERT INTO login_tokens (expires_at)
		VALUES ($1)
		RETURNING token, status, telegram_id, wallet_address, proxy_address, created_at, authenticated_at, expires_at, used_at
	`

	expiresAt := time.Now().Add(expiresIn)
	token := &database.LoginToken{}

	err := r.db.Pool.QueryRow(ctx, query, expiresAt).Scan(
		&token.Token,
		&token.Status,
		&token.TelegramID,
		&token.WalletAddress,
		&token.ProxyAddress,
		&token.CreatedAt,
		&token.AuthenticatedAt,
		&token.ExpiresAt,
		&token.UsedAt,
	)

	if err != nil {
		return nil, fmt.Errorf("failed to create login token: %w", err)
	}

	return token, nil
}

// GetByToken retrieves a login token by its UUID string
func (r *loginTokenRepo) GetByToken(ctx context.Context, tokenStr string) (*database.LoginToken, error) {
	// Validate UUID format
	parsedUUID, err := uuid.Parse(tokenStr)
	if err != nil {
		return nil, fmt.Errorf("invalid token format: %w", err)
	}

	query := `
		SELECT token, status, telegram_id, wallet_address, proxy_address, created_at, authenticated_at, expires_at, used_at
		FROM login_tokens
		WHERE token = $1
	`

	token := &database.LoginToken{}
	err = r.db.Pool.QueryRow(ctx, query, pgtype.UUID{Bytes: parsedUUID, Valid: true}).Scan(
		&token.Token,
		&token.Status,
		&token.TelegramID,
		&token.WalletAddress,
		&token.ProxyAddress,
		&token.CreatedAt,
		&token.AuthenticatedAt,
		&token.ExpiresAt,
		&token.UsedAt,
	)

	if err != nil {
		if err == pgx.ErrNoRows {
			return nil, nil // Token not found
		}
		return nil, fmt.Errorf("failed to get login token: %w", err)
	}

	return token, nil
}

// Authenticate marks a token as authenticated with the user's details
func (r *loginTokenRepo) Authenticate(ctx context.Context, tokenStr string, telegramID int64, walletAddr, proxyAddr string) error {
	// Validate UUID format
	parsedUUID, err := uuid.Parse(tokenStr)
	if err != nil {
		return fmt.Errorf("invalid token format: %w", err)
	}

	query := `
		UPDATE login_tokens SET
			status = $1,
			telegram_id = $2,
			wallet_address = $3,
			proxy_address = $4,
			authenticated_at = NOW()
		WHERE token = $5
			AND status = 'pending'
			AND expires_at > NOW()
	`

	result, err := r.db.Pool.Exec(ctx, query,
		database.LoginTokenStatusAuthenticated,
		telegramID,
		walletAddr,
		proxyAddr,
		pgtype.UUID{Bytes: parsedUUID, Valid: true},
	)

	if err != nil {
		return fmt.Errorf("failed to authenticate token: %w", err)
	}

	if result.RowsAffected() == 0 {
		return fmt.Errorf("token not found, already used, or expired")
	}

	return nil
}

// MarkUsed marks a token as used (consumed for session creation)
func (r *loginTokenRepo) MarkUsed(ctx context.Context, tokenStr string) (*database.LoginToken, error) {
	// Validate UUID format
	parsedUUID, err := uuid.Parse(tokenStr)
	if err != nil {
		return nil, fmt.Errorf("invalid token format: %w", err)
	}

	query := `
		UPDATE login_tokens SET
			status = $1,
			used_at = NOW()
		WHERE token = $2
			AND status = 'authenticated'
			AND expires_at > NOW()
		RETURNING token, status, telegram_id, wallet_address, proxy_address, created_at, authenticated_at, expires_at, used_at
	`

	token := &database.LoginToken{}
	err = r.db.Pool.QueryRow(ctx, query,
		database.LoginTokenStatusUsed,
		pgtype.UUID{Bytes: parsedUUID, Valid: true},
	).Scan(
		&token.Token,
		&token.Status,
		&token.TelegramID,
		&token.WalletAddress,
		&token.ProxyAddress,
		&token.CreatedAt,
		&token.AuthenticatedAt,
		&token.ExpiresAt,
		&token.UsedAt,
	)

	if err != nil {
		if err == pgx.ErrNoRows {
			return nil, fmt.Errorf("token not found, not authenticated, or expired")
		}
		return nil, fmt.Errorf("failed to mark token as used: %w", err)
	}

	return token, nil
}

// CleanupExpired removes expired tokens from the database
func (r *loginTokenRepo) CleanupExpired(ctx context.Context) (int64, error) {
	query := `
		DELETE FROM login_tokens
		WHERE expires_at < NOW()
			OR (status = 'used' AND used_at < NOW() - INTERVAL '1 hour')
	`

	result, err := r.db.Pool.Exec(ctx, query)
	if err != nil {
		return 0, fmt.Errorf("failed to cleanup expired tokens: %w", err)
	}

	return result.RowsAffected(), nil
}

// TokenToString converts a pgtype.UUID to string
func TokenToString(token pgtype.UUID) string {
	if !token.Valid {
		return ""
	}
	return uuid.UUID(token.Bytes).String()
}
