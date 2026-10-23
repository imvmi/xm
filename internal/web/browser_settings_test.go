package web

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"xm/internal/archive"
)

func TestBrowserDiagnosticUsesUnsavedDraft(t *testing.T) {
	a := testApp(t)
	b := login(t, a)
	saved := a.Config()
	if err := a.store.SaveCookies(archive.Cookies{AuthToken: "fixture-auth", CSRF: "fixture-csrf"}); err != nil {
		t.Fatal(err)
	}
	for _, connectionOnly := range []bool{true, false} {
		calls := 0
		a.checkChrome = func(ctx context.Context, cfg archive.Config, cookies *archive.Cookies) (archive.ChromeInfo, error) {
			calls++
			if cfg.BrowserMode != "cdp" || cfg.CDPHost != "::1" || cfg.CDPPort != 5818 || cfg.Proxy != saved.Proxy || cfg.Headless != saved.Headless {
				t.Error("diagnostic did not use draft browser and saved environment")
			}
			if (cookies == nil) != connectionOnly {
				t.Error("connection-only check must not import cookies or visit X")
			}
			if _, ok := ctx.Deadline(); !ok {
				t.Error("diagnostic has no deadline")
			}
			return archive.ChromeInfo{Product: "Chrome/fixture"}, nil
		}
		input := map[string]any{"browser_mode": "cdp", "cdp_host": " ::1 ", "cdp_port": 5818, "connection_only": connectionOnly}
		w := b.request("POST", "/api/chrome-test", input, "http://localhost", true)
		ok(t, w, 200)
		var result struct {
			OK    bool              `json:"ok"`
			Steps []json.RawMessage `json:"steps"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
		wantSteps := 4
		if connectionOnly {
			wantSteps = 1
		}
		if calls != 1 || !result.OK || len(result.Steps) != wantSteps {
			t.Fatalf("diagnostic response: %s", w.Body)
		}
		persisted, err := a.store.LoadSettings(saved)
		if err != nil || !reflect.DeepEqual(a.Config(), saved) || persisted.BrowserMode != saved.BrowserMode || persisted.CDPHost != saved.CDPHost || persisted.CDPPort != saved.CDPPort {
			t.Fatal("testing a draft changed saved configuration", err)
		}
	}
	// Invalid and unrelated input must be rejected before starting any browser.
	a.checkChrome = func(context.Context, archive.Config, *archive.Cookies) (archive.ChromeInfo, error) {
		t.Error("invalid input started browser")
		return archive.ChromeInfo{}, nil
	}
	for _, input := range []map[string]any{
		{"browser_mode": ""}, {"browser_mode": "bad"}, {"cdp_host": "http://127.0.0.1"},
		{"cdp_host": ""}, {"cdp_port": 0}, {"cdp_port": 65536}, {"cdp_port": 1.5},
		{"proxy": "http://127.0.0.1:1"}, {"chrome_path": "/unexpected"},
	} {
		ok(t, b.request("POST", "/api/chrome-test", input, "http://localhost", true), 400)
	}
	// A local draft must override a saved remote connection as well.
	ok(t, b.request("PUT", "/api/settings", map[string]any{"browser_mode": "cdp"}, "http://localhost", true), 200)
	a.checkChrome = func(_ context.Context, cfg archive.Config, _ *archive.Cookies) (archive.ChromeInfo, error) {
		if cfg.BrowserMode != "local" {
			t.Error("saved CDP mode overrode local draft")
		}
		return archive.ChromeInfo{}, errors.New("fixture: connection refused")
	}
	w := b.request("POST", "/api/chrome-test", map[string]any{"browser_mode": "local", "connection_only": true}, "http://localhost", true)
	ok(t, w, 200)
	if !strings.Contains(w.Body.String(), "fixture: connection refused") || !strings.Contains(w.Body.String(), `"ok":false`) || a.Config().BrowserMode != "cdp" {
		t.Fatalf("failure cause or configuration lost: %s", w.Body)
	}
}

func TestBrowserConnectionDoesNotRequireCookies(t *testing.T) {
	a := testApp(t)
	b := login(t, a)
	a.checkChrome = func(_ context.Context, _ archive.Config, cookies *archive.Cookies) (archive.ChromeInfo, error) {
		if cookies != nil {
			t.Error("unexpected cookies")
		}
		return archive.ChromeInfo{Product: "Chrome/fixture"}, nil
	}
	w := b.request("POST", "/api/chrome-test", map[string]any{"connection_only": true}, "http://localhost", true)
	ok(t, w, 200)
	if !strings.Contains(w.Body.String(), `"ok":true`) {
		t.Fatal(w.Body)
	}
}

func TestBrowserSettingsPersistValidateAndDiagnose(t *testing.T) {
	a := testApp(t)
	b := login(t, a)
	if a.Config().BrowserMode != "local" {
		t.Fatal("local must remain default")
	}
	input := map[string]any{"browser_mode": "cdp", "cdp_host": "192.0.2.10", "cdp_port": 5818}
	ok(t, b.request("PUT", "/api/settings", input, "http://localhost", true), 200)
	ok(t, b.request("PUT", "/api/settings", map[string]any{"proxy": ""}, "http://localhost", true), 200)
	cfg, err := a.store.LoadSettings(a.Config())
	if err != nil || cfg.BrowserMode != "cdp" || cfg.CDPHost != "192.0.2.10" || cfg.CDPPort != 5818 {
		t.Fatal("CDP config did not persist", err)
	}
	w := b.request("GET", "/api/settings", nil, "", false)
	var got archive.Config
	if err = json.Unmarshal(w.Body.Bytes(), &got); err != nil || got.BrowserMode != cfg.BrowserMode || got.CDPHost != cfg.CDPHost || got.CDPPort != cfg.CDPPort {
		t.Fatal("GET omitted browser settings", err)
	}
	called := false
	a.checkChrome = func(_ context.Context, c archive.Config, _ *archive.Cookies) (archive.ChromeInfo, error) {
		called = true
		if c.BrowserMode != "cdp" || c.CDPPort != 5818 {
			t.Error("diagnostic used old config")
		}
		return archive.ChromeInfo{Product: "Chrome/fixture"}, nil
	}
	ok(t, b.request("POST", "/api/chrome-test", nil, "http://localhost", true), 200)
	if !called {
		t.Fatal("diagnostic skipped")
	}
	for _, bad := range []map[string]any{{"browser_mode": ""}, {"browser_mode": "remote"}, {"cdp_host": "http://127.0.0.1"}, {"cdp_host": "127.0.0.1:9222"}, {"cdp_host": ""}, {"cdp_port": 0}, {"cdp_port": 65536}, {"cdp_port": -1}} {
		ok(t, b.request("PUT", "/api/settings", bad, "http://localhost", true), 400)
		if a.Config().CDPHost != cfg.CDPHost || a.Config().CDPPort != cfg.CDPPort || a.Config().BrowserMode != "cdp" {
			t.Fatal("invalid update altered configuration")
		}
	}

	dst, err := archive.OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer dst.Close()
	if err = migrate(dst); err != nil {
		t.Fatal(err)
	}
	if err = a.store.CopyDatabase(context.Background(), dst, nil); err != nil {
		t.Fatal(err)
	}
	copied, err := dst.LoadSettings(cfg)
	if err != nil || copied.BrowserMode != "cdp" || copied.CDPHost != cfg.CDPHost || copied.CDPPort != 5818 {
		t.Fatal("database migration lost browser settings", err)
	}
	ok(t, b.request("PUT", "/api/settings", map[string]any{"browser_mode": "local"}, "http://localhost", true), 200)
	if a.Config().BrowserMode != "local" || a.Config().CDPPort != 5818 {
		t.Fatal("local switch should preserve remote address")
	}
}

func TestBrowserConnectionChangesInvalidateReuse(t *testing.T) {
	c := archive.DefaultConfig()
	key := sessionKey(c, archive.Cookies{})
	for _, change := range []func(*archive.Config){func(c *archive.Config) { c.BrowserMode = "cdp" }, func(c *archive.Config) { c.CDPHost = "192.0.2.1" }, func(c *archive.Config) { c.CDPPort = 5818 }} {
		updated := c
		change(&updated)
		if sessionKey(updated, archive.Cookies{}) == key {
			t.Fatal("connection change reused old browser")
		}
	}
}
