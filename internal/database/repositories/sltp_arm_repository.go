package repositories

import (
	"context"
	"errors"
	"fmt"

	"github.com/Catorpilor/poly/internal/database"
	"github.com/jackc/pgx/v5"
)

// ErrSLTPArmNotFound is returned when no arm exists for the given (telegramID, tokenID).
var ErrSLTPArmNotFound = errors.New("sltp arm not found")

// SLTPArmRepository defines data operations for take-profit / stop-loss arms.
type SLTPArmRepository interface {
	// Arm creates or replaces the arm row for (telegramID, tokenID).
	// Both tp_armed and sl_armed are set to true on arm/re-arm.
	Arm(ctx context.Context, arm *database.SLTPArm) (*database.SLTPArm, error)

	// Disarm removes the arm row entirely. Returns ErrSLTPArmNotFound if missing.
	Disarm(ctx context.Context, telegramID int64, tokenID string) error

	// ClearTP marks tp_armed = false. Called by the monitor after a TP fires
	// so SL can continue watching the remaining shares.
	ClearTP(ctx context.Context, telegramID int64, tokenID string) error

	// GetByUserAndToken returns the arm row or nil if not found.
	GetByUserAndToken(ctx context.Context, telegramID int64, tokenID string) (*database.SLTPArm, error)

	// ListArmed returns all arms where tp_armed OR sl_armed is true.
	ListArmed(ctx context.Context) ([]*database.SLTPArm, error)

	// ListArmedByToken returns all arms for tokenID with tp_armed OR sl_armed true.
	ListArmedByToken(ctx context.Context, tokenID string) ([]*database.SLTPArm, error)

	// ListArmedTokenIDs returns the distinct token IDs with any armed row.
	// Used by the monitor to seed WebSocket subscriptions on startup.
	ListArmedTokenIDs(ctx context.Context) ([]string, error)
}

type sltpArmRepo struct {
	db *database.DB
}

// NewSLTPArmRepository creates a new SL/TP arm repository.
func NewSLTPArmRepository(db *database.DB) SLTPArmRepository {
	return &sltpArmRepo{db: db}
}

const sltpArmColumns = `id, telegram_id, token_id, condition_id, market_id, outcome,
		avg_price, shares_at_arm, tp_armed, sl_armed, neg_risk, created_at, updated_at`

func scanArm(row pgx.Row) (*database.SLTPArm, error) {
	a := &database.SLTPArm{}
	if err := row.Scan(
		&a.ID, &a.TelegramID, &a.TokenID, &a.ConditionID, &a.MarketID, &a.Outcome,
		&a.AvgPrice, &a.SharesAtArm, &a.TPArmed, &a.SLArmed, &a.NegRisk, &a.CreatedAt, &a.UpdatedAt,
	); err != nil {
		return nil, err
	}
	return a, nil
}

func (r *sltpArmRepo) Arm(ctx context.Context, arm *database.SLTPArm) (*database.SLTPArm, error) {
	if err := arm.Validate(); err != nil {
		return nil, fmt.Errorf("invalid arm: %w", err)
	}

	query := `
		INSERT INTO sltp_arms (
			telegram_id, token_id, condition_id, market_id, outcome,
			avg_price, shares_at_arm, tp_armed, sl_armed, neg_risk
		) VALUES ($1, $2, $3, $4, $5, $6, $7, TRUE, TRUE, $8)
		ON CONFLICT (telegram_id, token_id) DO UPDATE SET
			condition_id = EXCLUDED.condition_id,
			market_id = EXCLUDED.market_id,
			outcome = EXCLUDED.outcome,
			avg_price = EXCLUDED.avg_price,
			shares_at_arm = EXCLUDED.shares_at_arm,
			tp_armed = TRUE,
			sl_armed = TRUE,
			neg_risk = EXCLUDED.neg_risk
		RETURNING ` + sltpArmColumns

	row := r.db.Pool.QueryRow(ctx, query,
		arm.TelegramID, arm.TokenID, arm.ConditionID, arm.MarketID, arm.Outcome,
		arm.AvgPrice, arm.SharesAtArm, arm.NegRisk,
	)
	result, err := scanArm(row)
	if err != nil {
		return nil, fmt.Errorf("failed to arm sltp: %w", err)
	}
	return result, nil
}

func (r *sltpArmRepo) Disarm(ctx context.Context, telegramID int64, tokenID string) error {
	query := `DELETE FROM sltp_arms WHERE telegram_id = $1 AND token_id = $2`
	tag, err := r.db.Pool.Exec(ctx, query, telegramID, tokenID)
	if err != nil {
		return fmt.Errorf("failed to disarm sltp: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrSLTPArmNotFound
	}
	return nil
}

// ClearTP atomically flips tp_armed from TRUE to FALSE. Returns ErrSLTPArmNotFound
// if the row is missing OR tp_armed was already false (prevents double-fire under
// concurrent evaluate calls).
func (r *sltpArmRepo) ClearTP(ctx context.Context, telegramID int64, tokenID string) error {
	query := `UPDATE sltp_arms SET tp_armed = FALSE
		WHERE telegram_id = $1 AND token_id = $2 AND tp_armed = TRUE`
	tag, err := r.db.Pool.Exec(ctx, query, telegramID, tokenID)
	if err != nil {
		return fmt.Errorf("failed to clear tp: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrSLTPArmNotFound
	}
	return nil
}

func (r *sltpArmRepo) GetByUserAndToken(ctx context.Context, telegramID int64, tokenID string) (*database.SLTPArm, error) {
	query := `SELECT ` + sltpArmColumns + ` FROM sltp_arms WHERE telegram_id = $1 AND token_id = $2`
	row := r.db.Pool.QueryRow(ctx, query, telegramID, tokenID)
	arm, err := scanArm(row)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("failed to get sltp arm: %w", err)
	}
	return arm, nil
}

func (r *sltpArmRepo) ListArmed(ctx context.Context) ([]*database.SLTPArm, error) {
	query := `SELECT ` + sltpArmColumns + ` FROM sltp_arms WHERE tp_armed OR sl_armed ORDER BY id`
	return r.queryList(ctx, query)
}

func (r *sltpArmRepo) ListArmedByToken(ctx context.Context, tokenID string) ([]*database.SLTPArm, error) {
	query := `SELECT ` + sltpArmColumns + ` FROM sltp_arms WHERE token_id = $1 AND (tp_armed OR sl_armed) ORDER BY id`
	return r.queryList(ctx, query, tokenID)
}

func (r *sltpArmRepo) ListArmedTokenIDs(ctx context.Context) ([]string, error) {
	query := `SELECT DISTINCT token_id FROM sltp_arms WHERE tp_armed OR sl_armed`
	rows, err := r.db.Pool.Query(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("failed to list armed token ids: %w", err)
	}
	defer rows.Close()

	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("failed to scan token id: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("rows error: %w", err)
	}
	return ids, nil
}

func (r *sltpArmRepo) queryList(ctx context.Context, query string, args ...any) ([]*database.SLTPArm, error) {
	rows, err := r.db.Pool.Query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("failed to query arms: %w", err)
	}
	defer rows.Close()

	var out []*database.SLTPArm
	for rows.Next() {
		arm, err := scanArm(rows)
		if err != nil {
			return nil, fmt.Errorf("failed to scan arm: %w", err)
		}
		out = append(out, arm)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("rows error: %w", err)
	}
	return out, nil
}
