package database

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/Catorpilor/poly/internal/config"
)

// DB represents the database connection pool
type DB struct {
	Pool *pgxpool.Pool
	cfg  *config.DatabaseConfig
}

// NewConnection creates a new database connection pool
func NewConnection(cfg *config.DatabaseConfig) (*DB, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Parse the connection config
	poolConfig, err := pgxpool.ParseConfig(cfg.URL)
	if err != nil {
		return nil, fmt.Errorf("failed to parse database URL: %w", err)
	}

	// Configure the pool
	poolConfig.MaxConns = int32(cfg.MaxConnections)
	poolConfig.MinConns = 1
	poolConfig.MaxConnIdleTime = cfg.ConnMaxIdleTime
	poolConfig.MaxConnLifetime = cfg.ConnMaxLifetime

	// Set up logging for database operations in development
	poolConfig.BeforeAcquire = func(ctx context.Context, conn *pgx.Conn) bool {
		return true
	}

	poolConfig.AfterRelease = func(conn *pgx.Conn) bool {
		return true
	}

	// Create the connection pool
	pool, err := pgxpool.NewWithConfig(ctx, poolConfig)
	if err != nil {
		return nil, fmt.Errorf("failed to create database pool: %w", err)
	}

	// Test the connection
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("failed to ping database: %w", err)
	}

	log.Println("Database connection established successfully")

	return &DB{
		Pool: pool,
		cfg:  cfg,
	}, nil
}

// Close closes the database connection pool
func (db *DB) Close() {
	if db.Pool != nil {
		db.Pool.Close()
		log.Println("Database connection closed")
	}
}

// Health checks the database health
func (db *DB) Health(ctx context.Context) error {
	return db.Pool.Ping(ctx)
}

// BeginTx starts a new database transaction
func (db *DB) BeginTx(ctx context.Context) (pgx.Tx, error) {
	return db.Pool.Begin(ctx)
}

// ExecTx executes a function within a database transaction
func (db *DB) ExecTx(ctx context.Context, fn func(pgx.Tx) error) error {
	tx, err := db.BeginTx(ctx)
	if err != nil {
		return fmt.Errorf("failed to begin transaction: %w", err)
	}

	defer func() {
		if err := recover(); err != nil {
			_ = tx.Rollback(ctx)
			panic(err)
		}
	}()

	if err := fn(tx); err != nil {
		if rbErr := tx.Rollback(ctx); rbErr != nil {
			return fmt.Errorf("tx err: %v, rollback err: %v", err, rbErr)
		}
		return err
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("failed to commit transaction: %w", err)
	}

	return nil
}