package archive

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"
)

func poolFixture(t *testing.T) *Store {
	t.Helper()
	name := "数据库 # space"
	if runtime.GOOS != "windows" {
		name += " ?"
	}
	s, err := OpenStore(filepath.Join(t.TempDir(), name))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	if _, err = s.DB.Exec("CREATE TABLE pool_probe(id INTEGER PRIMARY KEY,value INTEGER NOT NULL); INSERT INTO pool_probe VALUES(1,0)"); err != nil {
		t.Fatal(err)
	}
	return s
}
func poolValue(t *testing.T, query interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}) int {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	var value int
	if err := query.QueryRowContext(ctx, "SELECT value FROM pool_probe WHERE id=1").Scan(&value); err != nil {
		t.Fatal(err)
	}
	return value
}
func TestSQLiteConcurrentReadersAndSingleWriter(t *testing.T) {
	s := poolFixture(t)
	if s.DB.Stats().MaxOpenConnections != 1 || s.DB.Reader.Stats().MaxOpenConnections != 4 {
		t.Fatal("incorrect pool limits")
	}
	snapshot, err := s.DB.BeginTx(context.Background(), &sql.TxOptions{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer snapshot.Rollback()
	if poolValue(t, snapshot) != 0 {
		t.Fatal("unexpected initial value")
	}
	writer, err := s.DB.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Rollback()
	if _, err = writer.Exec("UPDATE pool_probe SET value=1"); err != nil {
		t.Fatal(err)
	}
	if poolValue(t, writer) != 1 || poolValue(t, s.DB) != 0 {
		t.Fatal("uncommitted data leaked or writer lost its own update")
	}
	// Both API styles must serialize writers, while a separate reader proceeds.
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if tx, err := s.DB.BeginTx(ctx, nil); !errors.Is(err, context.DeadlineExceeded) {
		if tx != nil {
			tx.Rollback()
		}
		t.Fatalf("second writer did not queue: %v", err)
	}
	if s.DB.Stats().WaitCount == 0 {
		t.Fatal("writer did not wait in the pool")
	}
	if err = writer.Commit(); err != nil {
		t.Fatal("reader blocked writer commit", err)
	}
	if poolValue(t, s.DB) != 1 || poolValue(t, snapshot) != 0 {
		t.Fatal("snapshot isolation broken")
	}
	if _, err = snapshot.Exec("UPDATE pool_probe SET value=99"); err == nil {
		t.Fatal("read-only transaction allowed a write")
	}
	snapshot.Rollback()
	// Concurrent read/modify/write transactions may not lose updates.
	var wg sync.WaitGroup
	errs := make(chan error, 12)
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			tx, err := s.DB.Begin()
			if err != nil {
				errs <- err
				return
			}
			defer tx.Rollback()
			var n int
			if err = tx.QueryRow("SELECT value FROM pool_probe WHERE id=1").Scan(&n); err == nil {
				_, err = tx.Exec("UPDATE pool_probe SET value=? WHERE id=1", n+1)
			}
			if err == nil {
				err = tx.Commit()
			}
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if poolValue(t, s.DB) != 13 {
		t.Fatal("concurrent writes lost updates")
	}
}

func TestSQLiteReadLimitResizeAndConnectionInitialization(t *testing.T) {
	s := poolFixture(t)
	if err := s.SetSQLiteReadConnections(2); err != nil {
		t.Fatal(err)
	}
	// Holding two result sets reserves both read connections.
	first, err := s.DB.Query("SELECT value FROM pool_probe")
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	second, err := s.DB.QueryContext(context.Background(), "SELECT value FROM pool_probe")
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	var n int
	if err = s.DB.QueryRowContext(ctx, "SELECT 1").Scan(&n); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("read limit ignored: %v", err)
	}
	if err = s.SetSQLiteReadConnections(1); err != nil {
		t.Fatal(err)
	}
	if !first.Next() || !second.Next() {
		t.Fatal("resizing interrupted active readers")
	}
	first.Close()
	second.Close()
	if poolValue(t, s.DB) != 0 {
		t.Fatal("reader failed after reducing limit")
	}
	for _, limit := range []int{0, -1, 17} {
		if s.SetSQLiteReadConnections(limit) == nil {
			t.Fatal("invalid limit accepted", limit)
		}
	}
	if s.DB.Reader.Stats().MaxOpenConnections != 1 {
		t.Fatal("invalid resize changed pool")
	}
	// Force all future operations onto newly opened connections.
	s.DB.Reader.SetMaxIdleConns(0)
	s.DB.SetMaxIdleConns(0)
	for i := 0; i < 3; i++ {
		for _, pool := range []*sql.DB{s.DB.DB, s.DB.Reader} {
			conn, err := pool.Conn(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			for name, want := range map[string]int{"foreign_keys": 1, "busy_timeout": 5000} {
				if err = conn.QueryRowContext(context.Background(), "PRAGMA "+name).Scan(&n); err != nil || n != want {
					conn.Close()
					t.Fatalf("%s=%d: %v", name, n, err)
				}
			}
			var mode string
			if err = conn.QueryRowContext(context.Background(), "PRAGMA journal_mode").Scan(&mode); err != nil || mode != "wal" {
				conn.Close()
				t.Fatal("WAL not enabled", mode, err)
			}
			if pool == s.DB.Reader {
				if _, err = conn.ExecContext(context.Background(), "UPDATE pool_probe SET value=99"); err == nil {
					conn.Close()
					t.Fatal("reader is writable")
				}
			}
			conn.Close()
		}
	}
	// Query/QueryRow cannot accidentally route RETURNING mutations to the writer.
	if err = s.DB.QueryRow("UPDATE pool_probe SET value=99 RETURNING value").Scan(&n); err == nil {
		t.Fatal("query mutation succeeded")
	}
	if _, err = s.DB.Exec("INSERT INTO target_posts(target,post_id) VALUES('alice','missing')"); err == nil {
		t.Fatal("replacement writer lost foreign key enforcement")
	}
	reader, writer := s.DB.Reader, s.DB.DB
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	if reader.Ping() == nil || writer.Ping() == nil {
		t.Fatal("Close leaked a pool")
	}
}
