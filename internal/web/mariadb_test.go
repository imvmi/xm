package web

import (
	"os"
	"strings"
	"testing"

	"xm/internal/archive"
)

func TestMariaDBConstraintUpgrade(t *testing.T) {
	if os.Getenv("XM_TEST_MYSQL_ADDR") == "" {
		t.Skip("requires isolated MySQL or MariaDB")
	}
	cfg := archive.DefaultConfig()
	cfg.DataDir = t.TempDir()
	configureMySQLTest(t, &cfg)
	cfg.Database.Driver = "mariadb"
	store, err := archive.OpenConfiguredStore(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	for _, tc := range []struct{ table, column, oldCheck string }{
		{"storage_profiles", "kind", "kind IN ('local','webdav','s3')"},
		{"system_settings", "request_budget_mode", "request_budget_mode IN ('conservative','balanced','fast')"},
		{"system_settings", "timeline_page_size", "timeline_page_size BETWEEN 20 AND 200"},
	} {
		rows, err := store.DB.CheckConstraints(tc.table)
		if err != nil {
			t.Fatal(err)
		}
		var drop string
		var inline bool
		for rows.Next() {
			var name, clause string
			if err := rows.Scan(&name, &clause); err != nil {
				t.Fatal(err)
			}
			if strings.Contains(clause, tc.column) {
				drop = store.DB.DropCheckConstraint(name)
				inline = store.DB.MariaDB && name == tc.column
			}
		}
		err = rows.Err()
		rows.Close()
		if err != nil || drop == "" {
			t.Fatalf("missing %s constraint: %v", tc.column, err)
		}
		alter := drop + ", ADD CONSTRAINT legacy_" + tc.column + " CHECK(" + tc.oldCheck + ")"
		if inline {
			definition := "VARCHAR(16) NOT NULL"
			if tc.column == "request_budget_mode" {
				definition += " DEFAULT 'balanced'"
			} else if tc.column == "timeline_page_size" {
				definition = "INTEGER NOT NULL DEFAULT 24"
			}
			alter = "MODIFY COLUMN " + tc.column + " " + definition + " CHECK(" + tc.oldCheck + ")"
		}
		if _, err = store.DB.Exec("ALTER TABLE " + tc.table + " " + alter); err != nil {
			t.Fatal(err)
		}
	}
	store.Close()
	upgraded, err := archive.OpenConfiguredStore(cfg)
	if err != nil {
		t.Fatal("cannot reopen legacy schema", err)
	}
	defer upgraded.Close()
	if err = upgraded.SaveSettings(cfg); err != nil {
		t.Fatal(err)
	}
	if _, err = upgraded.DB.Exec("UPDATE system_settings SET request_budget_mode='turbo',timeline_page_size=1000"); err != nil {
		t.Fatal("settings constraint not upgraded", err)
	}
	if _, err = upgraded.DB.Exec("UPDATE storage_profiles SET kind='smb' WHERE id='local'"); err != nil {
		t.Fatal("storage constraint not upgraded", err)
	}
	if _, err = upgraded.DB.Exec("UPDATE system_settings SET timeline_page_size=1001"); err == nil {
		t.Fatal("upgrade removed validation")
	}
}
