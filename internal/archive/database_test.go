package archive

import (
	"net"
	"os"
	"path/filepath"
	"testing"
)

func TestDatabaseConfigAndTLS(t *testing.T) {
	for _, driver := range []string{"mysql", "mariadb"} {
		t.Run(driver, func(t *testing.T) { testDatabaseConfigAndTLS(t, driver) })
	}
}

func testDatabaseConfigAndTLS(t *testing.T, driver string) {
	c := DatabaseConfig{Driver: driver, Host: "db.example.com", Name: "xm", User: "xm_app", PasswordEnv: "XM_TEST_PASSWORD"}
	t.Setenv("XM_TEST_PASSWORD", "secret:p@ss/word")
	mc, e := mysqlConfig(c)
	if e != nil {
		t.Fatal(e)
	}
	if mc.Passwd != "secret:p@ss/word" || mc.TLS == nil || mc.TLS.InsecureSkipVerify || mc.TLS.ServerName != c.Host || mc.MultiStatements || mc.ClientFoundRows {
		t.Fatal("unsafe MySQL defaults")
	}
	c.ServerName = "verified.example.com"
	mc, e = mysqlConfig(c)
	if e != nil || mc.TLS.ServerName != c.ServerName {
		t.Fatal(e)
	}
	c.TLSMode = "disabled"
	for _, host := range []string{"db.example.com", "192.0.2.1", "2001:db8::1", "localhost", "127.0.0.1", "::1"} {
		c.Host = host
		mc, e = mysqlConfig(c)
		if e != nil || mc.TLS != nil || mc.Addr != net.JoinHostPort(host, "3306") {
			t.Fatalf("explicitly disabled TLS rejected for %s: %v", host, e)
		}
	}
	for _, mode := range []string{"preferred", "skip_verify", "invalid"} {
		c.TLSMode = mode
		if _, e = mysqlConfig(c); e == nil {
			t.Fatalf("invalid TLS mode %q accepted", mode)
		}
	}
	c.TLSMode = "disabled"
	c.PasswordEnv = "XM_TEST_MISSING_PASSWORD"
	os.Unsetenv(c.PasswordEnv)
	if _, e = mysqlConfig(c); e == nil {
		t.Fatal("missing secret ignored")
	}
	c.PasswordEnv = ""
	c.TLSMode = "verify_identity"
	c.CAFile = filepath.Join(t.TempDir(), "invalid.pem")
	os.WriteFile(c.CAFile, []byte("not a certificate"), 0600)
	if _, e = mysqlConfig(c); e == nil {
		t.Fatal("invalid CA accepted")
	}
}

func TestDatabaseCAPathRoundTrip(t *testing.T) {
	c := DatabaseConfig{CAFile: "certs/ca.pem"}
	if err := c.resolvePaths("config"); err != nil {
		t.Fatal(err)
	}
	want, err := filepath.Abs("config/certs/ca.pem")
	if err != nil {
		t.Fatal(err)
	}
	if c.CAFile != want {
		t.Fatal("CA path not relative to config directory")
	}
	if err := c.resolvePaths("config"); err != nil || c.CAFile != want {
		t.Fatal("saved CA path changed on reload", err)
	}
}

func TestSQLiteDefaultAndExplicitFilenames(t *testing.T) {
	for _, explicit := range []string{"", "custom.db", "archive-migrated-fixture.sqlite"} {
		t.Run("explicit_"+explicit, func(t *testing.T) {
			cfg := DefaultConfig()
			cfg.DataDir = t.TempDir()
			cfg.Database = DatabaseConfig{Driver: "sqlite", SQLiteFile: explicit}
			s, err := OpenConfiguredStore(cfg)
			if err != nil {
				t.Fatal(err)
			}
			want := explicit
			if want == "" {
				want = DefaultSQLiteFile
			}
			if _, err = os.Stat(filepath.Join(cfg.DataDir, want)); err != nil {
				t.Fatal(err)
			}
			if err = s.SavePosts("alice", []Post{{ID: "123", Text: "filename preserved"}}); err != nil {
				t.Fatal(err)
			}
			s.Close()
			s, err = OpenConfiguredStore(cfg)
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			var count int
			if err = s.DB.QueryRow("SELECT COUNT(*) FROM posts").Scan(&count); err != nil || count != 1 {
				t.Fatal("database was not reopened", err)
			}
		})
	}
	for _, configured := range []bool{false, true} {
		name := "default_store"
		if configured {
			name = "configured_store"
		}
		t.Run(name+"_ignores_legacy_file", func(t *testing.T) {
			dir := t.TempDir()
			legacy := filepath.Join(dir, "archive.sqlite")
			const contents = "unrelated file must not be opened or modified"
			if err := os.WriteFile(legacy, []byte(contents), 0600); err != nil {
				t.Fatal(err)
			}
			var s *Store
			var err error
			if configured {
				cfg := DefaultConfig()
				cfg.DataDir = dir
				s, err = OpenConfiguredStore(cfg)
			} else {
				s, err = OpenStore(dir)
			}
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			if _, err := os.Stat(filepath.Join(dir, DefaultSQLiteFile)); err != nil {
				t.Fatal("default database not created", err)
			}
			data, err := os.ReadFile(legacy)
			if err != nil || string(data) != contents {
				t.Fatal("unrelated file changed", err)
			}
		})
	}
	for _, name := range []string{"../xm.db", "/tmp/xm.db", "nested/xm.db", `nested\xm.db`, "xm.db-wal", "bad\x00.db", "xm.txt"} {
		if err := (DatabaseConfig{SQLiteFile: name}).Validate(); err == nil {
			t.Fatalf("unsafe filename accepted: %q", name)
		}
	}
}
