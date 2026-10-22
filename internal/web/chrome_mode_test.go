package web

import (
	"context"
	"encoding/json"
	"testing"

	"xm/internal/archive"
)

func TestChromeModePersistsAndReachesDiagnostic(t *testing.T) {
	a := testApp(t)
	b := login(t, a)
	if !archive.DefaultConfig().Headless || !a.Config().Headless {
		t.Fatal("headless must be the default")
	}
	seedMonitor(t, a, "alice")
	var next int64
	if err := a.store.DB.QueryRow("SELECT next_run FROM monitors WHERE username='alice'").Scan(&next); err != nil {
		t.Fatal(err)
	}
	for _, headless := range []bool{false, true} {
		ok(t, b.request("PUT", "/api/settings", map[string]any{"chrome_headless": headless}, "http://localhost", true), 200)
		// Saving an unrelated form must preserve the selected mode.
		ok(t, b.request("PUT", "/api/settings", map[string]any{"proxy": ""}, "http://localhost", true), 200)
		loaded, err := a.store.LoadSettings(archive.DefaultConfig())
		if err != nil || loaded.Headless != headless || a.Config().Headless != headless {
			t.Fatal("mode did not persist", err)
		}
		var settings struct {
			Headless bool `json:"chrome_headless"`
		}
		w := b.request("GET", "/api/settings", nil, "", false)
		ok(t, w, 200)
		if err := json.Unmarshal(w.Body.Bytes(), &settings); err != nil || settings.Headless != headless {
			t.Fatal("mode not returned", err)
		}
		called := false
		a.checkChrome = func(_ context.Context, cfg archive.Config, _ *archive.Cookies) (archive.ChromeInfo, error) {
			called = true
			if cfg.Headless != headless {
				t.Fatal("diagnostic used old mode")
			}
			return archive.ChromeInfo{Product: "Chrome/fixture", Headless: cfg.Headless}, nil
		}
		ok(t, b.request("POST", "/api/chrome-test", nil, "http://localhost", true), 200)
		if !called {
			t.Fatal("diagnostic did not run")
		}
		var after int64
		if err := a.store.DB.QueryRow("SELECT next_run FROM monitors WHERE username='alice'").Scan(&after); err != nil || after != next {
			t.Fatal("mode change rescheduled collection", err)
		}
	}
}
