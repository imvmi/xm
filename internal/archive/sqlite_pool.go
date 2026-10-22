package archive

import (
	"database/sql"
	"errors"
	"net/url"
	"path/filepath"
	"strings"
)

const DefaultSQLiteReadConnections = 4
const MaxSQLiteReadConnections = 16

// URI encoding protects spaces, Unicode, # and ? in paths. A Windows drive
// needs the leading slash in file:///C:/... as well as forward separators.
func sqliteDSN(path string, readOnly bool) (string, error) {
	full, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	full = filepath.ToSlash(full)
	if !strings.HasPrefix(full, "/") {
		full = "/" + full
	}
	u := url.URL{Scheme: "file", Path: full}
	q := url.Values{}
	// These pragmas are connection-local, so initialize every replacement
	// connection through the DSN rather than running them once on a pool.
	q.Add("_pragma", "busy_timeout(5000)")
	q.Add("_pragma", "foreign_keys(1)")
	if readOnly {
		q.Set("mode", "ro")
		q.Add("_pragma", "query_only(1)")
	} else {
		q.Set("mode", "rw")
		q.Set("_txlock", "immediate")
	}
	u.RawQuery = q.Encode()
	return u.String(), nil
}

func (s *Store) SetSQLiteReadConnections(n int) error {
	if n < 1 || n > MaxSQLiteReadConnections {
		return errors.New("SQLite 读取并发数必须为 1～16")
	}
	if s.DB.Reader != nil {
		s.DB.Reader.SetMaxOpenConns(n)
		s.DB.Reader.SetMaxIdleConns(n)
	}
	return nil
}

// Schema upgrades finish on the writer before any read-only pool is opened.
func (s *Store) openSQLiteReaders(path string) error {
	n := DefaultSQLiteReadConnections
	err := s.DB.QueryRow("SELECT sqlite_read_connections FROM system_settings WHERE id=1").Scan(&n)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if n < 1 || n > MaxSQLiteReadConnections {
		return errors.New("数据库中的 SQLite 读取并发数无效")
	}
	dsn, err := sqliteDSN(path, true)
	if err != nil {
		return err
	}
	reader, err := sql.Open("sqlite", dsn)
	if err != nil {
		return err
	}
	reader.SetMaxOpenConns(n)
	reader.SetMaxIdleConns(n)
	if err = reader.Ping(); err != nil {
		reader.Close()
		return err
	}
	s.DB.Reader = reader
	return nil
}
