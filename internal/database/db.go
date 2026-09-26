// Package database opens the SQLite database and applies migrations.
package database

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"time"

	"github.com/pressly/goose/v3"
	_ "modernc.org/sqlite"

	"github.com/tiim/photo-collect/internal/database/sqlc"
	"github.com/tiim/photo-collect/migrations"
)

// TimeFormat is the format used for all timestamps stored as TEXT. It matches
// the SQLite defaults in the schema so lexical comparison equals time order.
const TimeFormat = "2006-01-02T15:04:05.000Z"

// Time formats t for storage.
func Time(t time.Time) string { return t.UTC().Format(TimeFormat) }

// ParseTime parses a stored timestamp.
func ParseTime(s string) (time.Time, error) { return time.Parse(TimeFormat, s) }

type DB struct {
	*sql.DB
	Q *sqlc.Queries
}

// Open opens (creating if needed) the database at path and applies migrations.
func Open(ctx context.Context, path string) (*DB, error) {
	q := url.Values{}
	q.Add("_pragma", "journal_mode(WAL)")
	q.Add("_pragma", "busy_timeout(5000)")
	q.Add("_pragma", "foreign_keys(1)")
	q.Add("_pragma", "synchronous(NORMAL)")
	q.Set("_txlock", "immediate")
	dsn := "file:" + path + "?" + q.Encode()

	sqldb, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}
	// SQLite allows a single writer; a small pool keeps readers concurrent
	// while writers queue on busy_timeout.
	sqldb.SetMaxOpenConns(8)
	if err := sqldb.PingContext(ctx); err != nil {
		sqldb.Close()
		return nil, fmt.Errorf("ping database: %w", err)
	}
	if err := migrate(ctx, sqldb); err != nil {
		sqldb.Close()
		return nil, err
	}
	return &DB{DB: sqldb, Q: sqlc.New(sqldb)}, nil
}

func migrate(ctx context.Context, db *sql.DB) error {
	goose.SetBaseFS(migrations.FS)
	goose.SetLogger(goose.NopLogger())
	if err := goose.SetDialect("sqlite3"); err != nil {
		return err
	}
	if err := goose.UpContext(ctx, db, "."); err != nil {
		return fmt.Errorf("migrate database: %w", err)
	}
	return nil
}

// InTx runs fn inside a write transaction, committing on nil error.
func (d *DB) InTx(ctx context.Context, fn func(q *sqlc.Queries) error) error {
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	if err := fn(d.Q.WithTx(tx)); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}

// Backup writes a consistent snapshot of the database to dest using VACUUM INTO.
func (d *DB) Backup(ctx context.Context, dest string) error {
	_, err := d.ExecContext(ctx, "VACUUM INTO ?", dest)
	return err
}
