package repositories

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/Catorpilor/poly/internal/database"
)

// UserRepository defines the interface for user data operations
type UserRepository interface {
	Create(ctx context.Context, user *database.User) error
	GetByTelegramID(ctx context.Context, telegramID int64) (*database.User, error)
	GetByEOAAddress(ctx context.Context, address string) (*database.User, error)
	GetByProxyAddress(ctx context.Context, address string) (*database.User, error)
	Update(ctx context.Context, user *database.User) error
	Delete(ctx context.Context, telegramID int64) error
	UpdateEncryptedKey(ctx context.Context, telegramID int64, encryptedKey string) error
	UpdateSettings(ctx context.Context, telegramID int64, settings database.JSONB) error
	SetActive(ctx context.Context, telegramID int64, isActive bool) error
	List(ctx context.Context, offset, limit int) ([]*database.User, error)
	Count(ctx context.Context) (int64, error)
}

// userRepo implements UserRepository interface
type userRepo struct {
	db *database.DB
}

// NewUserRepository creates a new user repository
func NewUserRepository(db *database.DB) UserRepository {
	return &userRepo{db: db}
}

// Create creates a new user
func (r *userRepo) Create(ctx context.Context, user *database.User) error {
	query := `
		INSERT INTO users (
			telegram_id, username, eoa_address, proxy_address,
			encrypted_key, settings, is_active
		) VALUES ($1, $2, $3, $4, $5, $6, $7)
		RETURNING created_at, updated_at
	`

	err := r.db.Pool.QueryRow(ctx, query,
		user.TelegramID,
		user.Username,
		user.EOAAddress,
		user.ProxyAddress,
		user.EncryptedKey,
		user.Settings,
		user.IsActive,
	).Scan(&user.CreatedAt, &user.UpdatedAt)

	if err != nil {
		return fmt.Errorf("failed to create user: %w", err)
	}

	return nil
}

// GetByTelegramID gets a user by telegram ID
func (r *userRepo) GetByTelegramID(ctx context.Context, telegramID int64) (*database.User, error) {
	query := `
		SELECT
			telegram_id, username, eoa_address, proxy_address,
			encrypted_key, settings, is_active, created_at, updated_at
		FROM users
		WHERE telegram_id = $1
	`

	user := &database.User{}
	err := r.db.Pool.QueryRow(ctx, query, telegramID).Scan(
		&user.TelegramID,
		&user.Username,
		&user.EOAAddress,
		&user.ProxyAddress,
		&user.EncryptedKey,
		&user.Settings,
		&user.IsActive,
		&user.CreatedAt,
		&user.UpdatedAt,
	)

	if err != nil {
		if err == pgx.ErrNoRows {
			return nil, nil // User not found
		}
		return nil, fmt.Errorf("failed to get user by telegram ID: %w", err)
	}

	return user, nil
}

// GetByEOAAddress gets a user by EOA address
func (r *userRepo) GetByEOAAddress(ctx context.Context, address string) (*database.User, error) {
	query := `
		SELECT
			telegram_id, username, eoa_address, proxy_address,
			encrypted_key, settings, is_active, created_at, updated_at
		FROM users
		WHERE LOWER(eoa_address) = LOWER($1)
	`

	user := &database.User{}
	err := r.db.Pool.QueryRow(ctx, query, address).Scan(
		&user.TelegramID,
		&user.Username,
		&user.EOAAddress,
		&user.ProxyAddress,
		&user.EncryptedKey,
		&user.Settings,
		&user.IsActive,
		&user.CreatedAt,
		&user.UpdatedAt,
	)

	if err != nil {
		if err == pgx.ErrNoRows {
			return nil, nil // User not found
		}
		return nil, fmt.Errorf("failed to get user by EOA address: %w", err)
	}

	return user, nil
}

// GetByProxyAddress gets a user by proxy address
func (r *userRepo) GetByProxyAddress(ctx context.Context, address string) (*database.User, error) {
	query := `
		SELECT
			telegram_id, username, eoa_address, proxy_address,
			encrypted_key, settings, is_active, created_at, updated_at
		FROM users
		WHERE LOWER(proxy_address) = LOWER($1)
	`

	user := &database.User{}
	err := r.db.Pool.QueryRow(ctx, query, address).Scan(
		&user.TelegramID,
		&user.Username,
		&user.EOAAddress,
		&user.ProxyAddress,
		&user.EncryptedKey,
		&user.Settings,
		&user.IsActive,
		&user.CreatedAt,
		&user.UpdatedAt,
	)

	if err != nil {
		if err == pgx.ErrNoRows {
			return nil, nil // User not found
		}
		return nil, fmt.Errorf("failed to get user by proxy address: %w", err)
	}

	return user, nil
}

// Update updates an existing user
func (r *userRepo) Update(ctx context.Context, user *database.User) error {
	query := `
		UPDATE users SET
			username = $2,
			eoa_address = $3,
			proxy_address = $4,
			encrypted_key = $5,
			settings = $6,
			is_active = $7,
			updated_at = NOW()
		WHERE telegram_id = $1
		RETURNING updated_at
	`

	err := r.db.Pool.QueryRow(ctx, query,
		user.TelegramID,
		user.Username,
		user.EOAAddress,
		user.ProxyAddress,
		user.EncryptedKey,
		user.Settings,
		user.IsActive,
	).Scan(&user.UpdatedAt)

	if err != nil {
		if err == pgx.ErrNoRows {
			return fmt.Errorf("user not found")
		}
		return fmt.Errorf("failed to update user: %w", err)
	}

	return nil
}

// Delete deletes a user
func (r *userRepo) Delete(ctx context.Context, telegramID int64) error {
	query := `DELETE FROM users WHERE telegram_id = $1`

	result, err := r.db.Pool.Exec(ctx, query, telegramID)
	if err != nil {
		return fmt.Errorf("failed to delete user: %w", err)
	}

	if result.RowsAffected() == 0 {
		return fmt.Errorf("user not found")
	}

	return nil
}

// UpdateEncryptedKey updates the encrypted key for a user
func (r *userRepo) UpdateEncryptedKey(ctx context.Context, telegramID int64, encryptedKey string) error {
	query := `
		UPDATE users SET
			encrypted_key = $2,
			updated_at = NOW()
		WHERE telegram_id = $1
	`

	result, err := r.db.Pool.Exec(ctx, query, telegramID, encryptedKey)
	if err != nil {
		return fmt.Errorf("failed to update encrypted key: %w", err)
	}

	if result.RowsAffected() == 0 {
		return fmt.Errorf("user not found")
	}

	return nil
}

// UpdateSettings updates user settings
func (r *userRepo) UpdateSettings(ctx context.Context, telegramID int64, settings database.JSONB) error {
	query := `
		UPDATE users SET
			settings = $2,
			updated_at = NOW()
		WHERE telegram_id = $1
	`

	result, err := r.db.Pool.Exec(ctx, query, telegramID, settings)
	if err != nil {
		return fmt.Errorf("failed to update settings: %w", err)
	}

	if result.RowsAffected() == 0 {
		return fmt.Errorf("user not found")
	}

	return nil
}

// SetActive sets the active status of a user
func (r *userRepo) SetActive(ctx context.Context, telegramID int64, isActive bool) error {
	query := `
		UPDATE users SET
			is_active = $2,
			updated_at = NOW()
		WHERE telegram_id = $1
	`

	result, err := r.db.Pool.Exec(ctx, query, telegramID, isActive)
	if err != nil {
		return fmt.Errorf("failed to set active status: %w", err)
	}

	if result.RowsAffected() == 0 {
		return fmt.Errorf("user not found")
	}

	return nil
}

// List lists users with pagination
func (r *userRepo) List(ctx context.Context, offset, limit int) ([]*database.User, error) {
	query := `
		SELECT
			telegram_id, username, eoa_address, proxy_address,
			encrypted_key, settings, is_active, created_at, updated_at
		FROM users
		ORDER BY created_at DESC
		LIMIT $1 OFFSET $2
	`

	rows, err := r.db.Pool.Query(ctx, query, limit, offset)
	if err != nil {
		return nil, fmt.Errorf("failed to list users: %w", err)
	}
	defer rows.Close()

	var users []*database.User
	for rows.Next() {
		user := &database.User{}
		err := rows.Scan(
			&user.TelegramID,
			&user.Username,
			&user.EOAAddress,
			&user.ProxyAddress,
			&user.EncryptedKey,
			&user.Settings,
			&user.IsActive,
			&user.CreatedAt,
			&user.UpdatedAt,
		)
		if err != nil {
			return nil, fmt.Errorf("failed to scan user: %w", err)
		}
		users = append(users, user)
	}

	return users, nil
}

// Count counts total number of users
func (r *userRepo) Count(ctx context.Context) (int64, error) {
	query := `SELECT COUNT(*) FROM users`

	var count int64
	err := r.db.Pool.QueryRow(ctx, query).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("failed to count users: %w", err)
	}

	return count, nil
}