package web

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"xm/internal/archive"
	"xm/internal/database"
)

func TestSQLiteReadSettings(t *testing.T) {
	a := testApp(t)
	b := login(t, a)
	if a.Config().SQLiteReadConnections != 4 {
		t.Fatal("wrong default")
	}
	ok(t, b.request("PUT", "/api/settings", map[string]any{"sqlite_read_connections": 8}, "http://localhost", false), 403)
	for _, n := range []int{1, 16, 6} {
		ok(t, b.request("PUT", "/api/settings", map[string]any{"sqlite_read_connections": n}, "http://localhost", true), 200)
		if a.store.DB.Dialect == "mysql" && (a.store.DB.Reader != nil || a.store.DB.Stats().MaxOpenConnections != 8) {
			t.Fatal("SQLite preference changed MySQL connection pool")
		}
		if a.Config().SQLiteReadConnections != n {
			t.Fatal("live config unchanged")
		}
		if a.store.DB.Reader != nil && a.store.DB.Reader.Stats().MaxOpenConnections != n {
			t.Fatal("pool not resized immediately")
		}
	}
	for _, bad := range []any{0, -1, 17, 1.5, "8"} {
		ok(t, b.request("PUT", "/api/settings", map[string]any{"sqlite_read_connections": bad}, "http://localhost", true), 400)
	}
	ok(t, b.request("PUT", "/api/settings", map[string]any{"archive_theme": "dark"}, "http://localhost", true), 200)
	var response map[string]any
	w := b.request("GET", "/api/settings", nil, "", false)
	ok(t, w, 200)
	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response["sqlite_read_connections"] != float64(6) {
		t.Fatal("setting lost on partial update", response["sqlite_read_connections"])
	}
	// A failed settings transaction must not change the active read pool.
	cfg := a.Config()
	cfg.SQLiteReadConnections = 2
	if err := a.saveConfig(cfg, func(*database.Tx) error { return errors.New("test rollback") }); err == nil {
		t.Fatal("save unexpectedly succeeded")
	}
	if a.Config().SQLiteReadConnections != 6 || (a.store.DB.Reader != nil && a.store.DB.Reader.Stats().MaxOpenConnections != 6) {
		t.Fatal("failed save changed live settings")
	}
	loaded, err := a.store.LoadSettings(a.Config())
	if err != nil || loaded.SQLiteReadConnections != 6 {
		t.Fatal("setting not persisted", err)
	}
	reopened, err := archive.OpenConfiguredStore(a.Config())
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if reopened.DB.Reader != nil && reopened.DB.Reader.Stats().MaxOpenConnections != 6 {
		t.Fatal("restart did not restore pool size")
	}
	// Copying to SQLite must preserve the preference and restore it on restart.
	dir := t.TempDir()
	dst, err := archive.OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer dst.Close()
	if err = migrate(dst); err != nil {
		t.Fatal(err)
	}
	if err = a.store.CopyToSQLite(context.Background(), dst, nil); err != nil {
		t.Fatal(err)
	}
	loaded, err = dst.LoadSettings(archive.Config{DataDir: dir})
	if err != nil || loaded.SQLiteReadConnections != 6 {
		t.Fatal("migration lost preference", err)
	}
	dst.Close()
	dst, err = archive.OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer dst.Close()
	if dst.DB.Reader.Stats().MaxOpenConnections != 6 {
		t.Fatal("migrated pool size not restored")
	}
}

func TestSQLiteReadSettingsUpgrade(t *testing.T) {
	a := testApp(t)
	if a.store.DB.Dialect != "sqlite" {
		t.Skip("SQLite upgrade fixture")
	}
	cfg := a.Config()
	if _, err := a.store.DB.Exec("ALTER TABLE system_settings DROP COLUMN sqlite_read_connections"); err != nil {
		t.Fatal(err)
	}
	a.store.Close()
	s, err := archive.OpenConfiguredStore(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	loaded, err := s.LoadSettings(cfg)
	if err != nil || loaded.SQLiteReadConnections != 4 || s.DB.Reader.Stats().MaxOpenConnections != 4 {
		t.Fatal("old database did not gain default reader pool", err)
	}
}
