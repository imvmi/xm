package archive

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCookieFormatsAndDatabaseStorage(t *testing.T) {
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
		store, err := OpenStore(dir)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = store.LoadCookies(); err == nil {
			t.Fatal("empty database accepted")
		}
		if err = store.SaveCookies(c); err != nil {
			t.Fatal(err)
		}
		var token, csrf string
		if err = store.DB.QueryRow("SELECT auth_token,ct0 FROM x_cookies WHERE id=1").Scan(&token, &csrf); err != nil || token != c.AuthToken || csrf != c.CSRF {
			t.Fatal("cookies were not stored as plaintext", err)
		}
		if err = store.Close(); err != nil {
			t.Fatal(err)
		}
		store, err = OpenStore(dir)
		if err != nil {
			t.Fatal(err)
		}
		loaded, err := store.LoadCookies()
		if err != nil || loaded != c {
			t.Fatal("database roundtrip failed", err)
		}
		replacement := Cookies{AuthToken: "new-auth", CSRF: "new-csrf"}
		if err = store.SaveCookies(replacement); err != nil {
			t.Fatal(err)
		}
		if err = store.SaveCookies(Cookies{AuthToken: "invalid"}); err == nil {
			t.Fatal("invalid Cookie saved")
		}
		loaded, err = store.LoadCookies()
		if err != nil || loaded != replacement {
			t.Fatal("replacement was not retained", err)
		}
		var n int
		if err = store.DB.QueryRow("SELECT COUNT(*) FROM x_cookies").Scan(&n); err != nil || n != 1 {
			t.Fatal("cookie replacement created duplicate rows", err)
		}
		store.Close()
		for _, name := range []string{"cookies.enc", "cookie.key"} {
			if _, err = os.Stat(filepath.Join(dir, name)); !os.IsNotExist(err) {
				t.Fatal("legacy Cookie file created")
			}
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
