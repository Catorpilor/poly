package repositories

import (
	"context"
	"fmt"

	"github.com/Catorpilor/poly/internal/database"
)

// LiveWatchRepository is the durable store of Live Watches (ADR 0008): one row
// per (chat_id, event_slug). The registry in internal/live is the runtime view;
// this table is the record a restart re-reads. The narrower consumer-side
// interface (live.LiveWatchStore) is satisfied structurally by this type, so
// internal/live never imports this package.
type LiveWatchRepository interface {
	// Save upserts the watch, updating tape on an existing (chat_id, event_slug).
	Save(ctx context.Context, chatID int64, eventSlug string, tape bool) error
	// Delete removes one watch. Missing row is not an error (idempotent).
	Delete(ctx context.Context, chatID int64, eventSlug string) error
	// DeleteAll removes every watch for chatID and returns the removed slugs.
	DeleteAll(ctx context.Context, chatID int64) ([]string, error)
	// ListAll returns every stored watch, ordered by creation, for boot
	// re-registration.
	ListAll(ctx context.Context) ([]*database.LiveSubscription, error)
}

type liveWatchRepo struct {
	db *database.DB
}

// NewLiveWatchRepository creates a new Live Watch repository.
func NewLiveWatchRepository(db *database.DB) LiveWatchRepository {
	return &liveWatchRepo{db: db}
}

func (r *liveWatchRepo) Save(ctx context.Context, chatID int64, eventSlug string, tape bool) error {
	query := `
		INSERT INTO live_subscriptions (chat_id, event_slug, tape)
		VALUES ($1, $2, $3)
		ON CONFLICT (chat_id, event_slug) DO UPDATE SET tape = EXCLUDED.tape`
	if _, err := r.db.Pool.Exec(ctx, query, chatID, eventSlug, tape); err != nil {
		return fmt.Errorf("failed to save live watch: %w", err)
	}
	return nil
}

func (r *liveWatchRepo) Delete(ctx context.Context, chatID int64, eventSlug string) error {
	query := `DELETE FROM live_subscriptions WHERE chat_id = $1 AND event_slug = $2`
	if _, err := r.db.Pool.Exec(ctx, query, chatID, eventSlug); err != nil {
		return fmt.Errorf("failed to delete live watch: %w", err)
	}
	return nil
}

func (r *liveWatchRepo) DeleteAll(ctx context.Context, chatID int64) ([]string, error) {
	query := `DELETE FROM live_subscriptions WHERE chat_id = $1 RETURNING event_slug`
	rows, err := r.db.Pool.Query(ctx, query, chatID)
	if err != nil {
		return nil, fmt.Errorf("failed to delete all live watches: %w", err)
	}
	defer rows.Close()

	var slugs []string
	for rows.Next() {
		var slug string
		if err := rows.Scan(&slug); err != nil {
			return nil, fmt.Errorf("failed to scan deleted slug: %w", err)
		}
		slugs = append(slugs, slug)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("rows error: %w", err)
	}
	return slugs, nil
}

func (r *liveWatchRepo) ListAll(ctx context.Context) ([]*database.LiveSubscription, error) {
	query := `SELECT chat_id, event_slug, tape, created_at
		FROM live_subscriptions
		ORDER BY created_at, chat_id, event_slug`
	rows, err := r.db.Pool.Query(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("failed to list live watches: %w", err)
	}
	defer rows.Close()

	var out []*database.LiveSubscription
	for rows.Next() {
		s := &database.LiveSubscription{}
		if err := rows.Scan(&s.ChatID, &s.EventSlug, &s.Tape, &s.CreatedAt); err != nil {
			return nil, fmt.Errorf("failed to scan live watch: %w", err)
		}
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("rows error: %w", err)
	}
	return out, nil
}
