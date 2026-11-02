
package database

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Open creates a PostgreSQL pool and verifies that the database is reachable.
func Open(ctx context.Context, databaseURL string, configuredMaxConnections ...int32) (*pgxpool.Pool, error) {
	if strings.TrimSpace(databaseURL) == "" {
		return nil, errors.New("DATABASE_URL is required")
	}
	maxConnections := int32(10)
	if len(configuredMaxConnections) > 0 {
		maxConnections = configuredMaxConnections[0]
	}
	if maxConnections <= 0 {
		return nil, errors.New("DB_MAX_CONNECTIONS must be positive")
	}

	poolConfig, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		return nil, fmt.Errorf("parse PostgreSQL configuration: %w", err)
	}
	poolConfig.MaxConns = maxConnections
	pool, err := pgxpool.NewWithConfig(ctx, poolConfig)
	if err != nil {
		return nil, fmt.Errorf("create PostgreSQL pool: %w", err)
	}

	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping PostgreSQL: %w", err)
	}

	return pool, nil
}
