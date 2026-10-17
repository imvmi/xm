package archive

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDatabaseConfigAndTLS(t *testing.T) {
	c := DatabaseConfig{Driver: "mysql", Host: "db.example.com", Name: "xm", User: "xm_app", PasswordEnv: "XM_TEST_PASSWORD"}
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
	if _, e = mysqlConfig(c); e == nil {
		t.Fatal("unencrypted remote connection accepted")
	}
	c.Host = "127.0.0.1"
	mc, e = mysqlConfig(c)
	if e != nil || mc.TLS != nil {
		t.Fatal(e)
	}
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
