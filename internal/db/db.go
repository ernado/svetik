// Package db implements database operations.
package db

import (
	"context"

	"github.com/ernado/svetik"

	sq "github.com/Masterminds/squirrel"
	"github.com/jackc/pgx/v5/pgxpool"
)

var _ lilith.DB = (*DB)(nil)

var psql = sq.StatementBuilder.PlaceholderFormat(sq.Dollar)

// DB is the database implementation.
type DB struct {
	pgx *pgxpool.Pool
}

// Ready checks if database is ready.
func (db *DB) Ready(ctx context.Context) error {
	return db.pgx.Ping(ctx)
}

func New(pgx *pgxpool.Pool) *DB {
	return &DB{pgx: pgx}
}
