package archive

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCookieFormatsAndEncryption(t *testing.T) {
	for _, input := range []string{
		`auth_token=secret-auth; ct0=secret-csrf; ignored=discard`,
		`Cookie: auth_token=secret-auth; ct0=secret-csrf`,
		`{"auth_token":"secret-auth","ct0":"secret-csrf","ignored":"discard"}`,
		`[{"name":"auth_token","value":"evil","domain":"evil.example"},{"name":"auth_token","value":"secret-auth","domain":".x.com"},{"name":"ct0","value":"secret-csrf","domain":".x.com"}]`,
		"# Netscape HTTP Cookie File\n#HttpOnly_.x.com\tTRUE\t/\tTRUE\t0\tauth_token\tsecret-auth\n.x.com\tTRUE\t/\tTRUE\t0\tct0\tsecret-csrf",
	} {
		c, err := ParseCookies([]byte(input))
		if err != nil {
			t.Fatal(err)
		}
		if c.AuthToken != "secret-auth" || c.CSRF != "secret-csrf" {
			t.Fatalf("bad parse: %+v", c)
		}
		dir := t.TempDir()
		if err = SaveCookies(dir, c); err != nil {
			t.Fatal(err)
		}
		b, _ := os.ReadFile(filepath.Join(dir, "cookies.enc"))
		if strings.Contains(string(b), "secret-") {
			t.Fatal("plaintext cookie stored")
		}
		loaded, err := LoadCookies(dir)
		if err != nil || loaded != c {
			t.Fatalf("vault roundtrip: %v", err)
		}
		b[len(b)-1] ^= 1
		os.WriteFile(filepath.Join(dir, "cookies.enc"), b, 0600)
		if _, err = LoadCookies(dir); err == nil {
			t.Fatal("tampering accepted")
		}
		st, _ := os.Stat(filepath.Join(dir, "cookie.key"))
		if st.Mode().Perm() != 0600 {
			t.Fatal("key permissions")
		}
	}
}

func TestCookiesRejectMissingForeignAndInvalid(t *testing.T) {
	for _, input := range []string{`auth_token=only`, `{"auth_token":"","ct0":"ok"}`, `{"auth_token":"a\nb","ct0":"ok"}`, `[{"name":"auth_token","value":"a","domain":"evil.x.com"},{"name":"ct0","value":"b","domain":"evil.x.com"}]`} {
		if _, err := ParseCookies([]byte(input)); err == nil {
			t.Fatalf("accepted invalid input: %s", input)
		}
	}
}
