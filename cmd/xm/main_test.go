package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"xm/internal/archive"
)

func TestImportCookieUsesConfiguredDatabase(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := run(context.Background(), []string{"init", "-config", path}); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(dir, "input.txt")
	if err := os.WriteFile(file, []byte("auth_token=cli-synthetic; ct0=cli-csrf"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := run(context.Background(), []string{"import-cookie", "-config", path, "-file", file}); err != nil {
		t.Fatal(err)
	}
	cfg, err := archive.LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	store, err := archive.OpenConfiguredStore(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	cookie, err := store.LoadCookies()
	if err != nil || cookie.AuthToken != "cli-synthetic" || cookie.CSRF != "cli-csrf" {
		t.Fatal("CLI import did not persist Cookie in database", err)
	}
	for _, name := range []string{"cookies.enc", "cookie.key"} {
		if _, err := os.Stat(filepath.Join(cfg.DataDir, name)); !os.IsNotExist(err) {
			t.Fatal("CLI import wrote a legacy Cookie file")
		}
	}
	var imported string
	if err = store.DB.QueryRow("SELECT value FROM web_state WHERE key='cookie_imported'").Scan(&imported); err != nil || imported == "" {
		t.Fatal("CLI import did not update scheduler state", err)
	}
}
