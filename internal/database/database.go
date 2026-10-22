// Package database contains the small SQL dialect boundary shared by the
// collector, scheduler and notification queue. Ordinary queries use database/sql.
package database

import (
	"context"
	"database/sql"
	"errors"
	"strings"
)

type DB struct {
	// DB is the write pool. Reader, when non-nil, is a separate read-only
	// pool installed before this handle is shared. MySQL uses DB for both.
	*sql.DB
	Reader  *sql.DB
	Dialect string
}
type Tx struct {
	*sql.Tx
	Dialect string
}

func (d *DB) Begin() (*Tx, error) {
	tx, e := d.BeginTx(context.Background(), nil)
	if e != nil {
		return nil, e
	}
	return &Tx{tx, d.Dialect}, nil
}

func (d *DB) readPool() *sql.DB {
	if d.Reader != nil {
		return d.Reader
	}
	return d.DB
}

// Ordinary queries use readers; all statements inside a transaction stay on
// its connection, including reads preceding a write. Never infer SQL intent
// by parsing query strings. Use Exec or a write transaction for mutations.
func (d *DB) Query(query string, args ...any) (*sql.Rows, error) {
	return d.QueryContext(context.Background(), query, args...)
}
func (d *DB) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	return d.readPool().QueryContext(ctx, query, args...)
}
func (d *DB) QueryRow(query string, args ...any) *sql.Row {
	return d.QueryRowContext(context.Background(), query, args...)
}
func (d *DB) QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row {
	return d.readPool().QueryRowContext(ctx, query, args...)
}
func (d *DB) BeginTx(ctx context.Context, opts *sql.TxOptions) (*sql.Tx, error) {
	if opts != nil && opts.ReadOnly {
		return d.readPool().BeginTx(ctx, opts)
	}
	return d.DB.BeginTx(ctx, opts)
}
func (d *DB) Close() error {
	var err error
	if d.Reader != nil {
		err = d.Reader.Close()
	}
	return errors.Join(err, d.DB.Close())
}
func Upsert(dialect, insert string, keys, columns []string) string {
	assignments := make([]string, len(columns))
	for i, c := range columns {
		if dialect == "mysql" {
			assignments[i] = "`" + c + "`=VALUES(`" + c + "`)"
		} else {
			assignments[i] = "`" + c + "`=excluded.`" + c + "`"
		}
	}
	if dialect == "mysql" {
		return insert + " ON DUPLICATE KEY UPDATE " + strings.Join(assignments, ",")
	}
	return insert + " ON CONFLICT(`" + strings.Join(keys, "`,`") + "`) DO UPDATE SET " + strings.Join(assignments, ",")
}

// A no-op duplicate update preserves strict-mode errors (unlike INSERT IGNORE).
// MySQL connections intentionally use affected-row, not found-row semantics.
func Ignore(dialect, insert, key string) string {
	if dialect == "mysql" {
		return insert + " ON DUPLICATE KEY UPDATE `" + key + "`=VALUES(`" + key + "`)"
	}
	return insert + " ON CONFLICT DO NOTHING"
}
func (d *DB) InsertIgnore(query, key string, args ...any) (sql.Result, error) {
	return d.Exec(Ignore(d.Dialect, query, key), args...)
}
func (t *Tx) InsertIgnore(query, key string, args ...any) (sql.Result, error) {
	return t.Exec(Ignore(t.Dialect, query, key), args...)
}
