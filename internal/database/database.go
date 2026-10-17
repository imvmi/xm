// Package database contains the small SQL dialect boundary shared by the
// collector, scheduler and notification queue. Ordinary queries use database/sql.
package database

import (
	"database/sql"
	"strings"
)

type DB struct {
	*sql.DB
	Dialect string
}
type Tx struct {
	*sql.Tx
	Dialect string
}

func (d *DB) Begin() (*Tx, error) {
	tx, e := d.DB.Begin()
	if e != nil {
		return nil, e
	}
	return &Tx{tx, d.Dialect}, nil
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
