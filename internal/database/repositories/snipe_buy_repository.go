package repositories

import (
	"context"
	"fmt"
	"time"

	"github.com/Catorpilor/poly/internal/database"
)

// SnipeBuyRepository is the durable snipe-buy log (issue #84): one row per
// ACCEPTED comeback-snipe buy. The in-memory bought record, the watcher's
// bought latch, and the two spend ledgers are the runtime view; this table is
// what a restart re-reads to rebuild them (internal/telegram.RestoreSnipeBuys).
// The narrower consumer-side interface (telegram.SnipeBuyStore) is satisfied
// structurally by this type, so internal/telegram never imports this package —
// the same seam as LiveWatchRepository/live.LiveWatchStore (ADR 0008).
type SnipeBuyRepository interface {
	// Save records one accepted buy. pool is 'main' or 'deep'; bought_at
	// defaults to NOW() in the DB.
	Save(ctx context.Context, chatID int64, tokenID string, amountUSD float64, pool string) error
	// ListSince returns every buy at or after since, oldest first — the boot
	// restore scan (since = now-24h). The bought_at index serves it.
	ListSince(ctx context.Context, since time.Time) ([]*database.SnipeBuy, error)
}

type snipeBuyRepo struct {
	db *database.DB
}

// NewSnipeBuyRepository creates a new snipe-buy repository.
func NewSnipeBuyRepository(db *database.DB) SnipeBuyRepository {
	return &snipeBuyRepo{db: db}
}

func (r *snipeBuyRepo) Save(ctx context.Context, chatID int64, tokenID string, amountUSD float64, pool string) error {
	query := `
		INSERT INTO snipe_buys (chat_id, token_id, amount_usd, pool)
		VALUES ($1, $2, $3, $4)`
	if _, err := r.db.Pool.Exec(ctx, query, chatID, tokenID, amountUSD, pool); err != nil {
		return fmt.Errorf("failed to save snipe buy: %w", err)
	}
	return nil
}

func (r *snipeBuyRepo) ListSince(ctx context.Context, since time.Time) ([]*database.SnipeBuy, error) {
	query := `
		SELECT id, chat_id, token_id, amount_usd, pool, bought_at
		FROM snipe_buys
		WHERE bought_at >= $1
		ORDER BY bought_at, id`
	rows, err := r.db.Pool.Query(ctx, query, since)
	if err != nil {
		return nil, fmt.Errorf("failed to list snipe buys: %w", err)
	}
	defer rows.Close()

	var out []*database.SnipeBuy
	for rows.Next() {
		b := &database.SnipeBuy{}
		if err := rows.Scan(&b.ID, &b.ChatID, &b.TokenID, &b.AmountUSD, &b.Pool, &b.BoughtAt); err != nil {
			return nil, fmt.Errorf("failed to scan snipe buy: %w", err)
		}
		out = append(out, b)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("rows error: %w", err)
	}
	return out, nil
}
