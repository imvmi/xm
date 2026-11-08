package web

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMediaStagingCleanupRequiresIdleConfirmedAdmin(t *testing.T) {
	a := testApp(t)
	b := login(t, a)
	dir := filepath.Join(a.Config().DataDir, "media", "users", "alice_42")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	part := filepath.Join(dir, ".download-"+strings.Repeat("a", 64)+".part")
	if err := os.WriteFile(part, []byte("prefix"), 0600); err != nil {
		t.Fatal(err)
	}
	const endpoint = "/api/media/staging/cleanup"
	unauth := &browserClient{t: t, a: a}
	ok(t, unauth.request("POST", endpoint, map[string]bool{"confirm": true}, "http://localhost", false), 401)
	ok(t, b.request("POST", endpoint, map[string]bool{"confirm": true}, "http://localhost", false), 403)
	ok(t, b.request("POST", endpoint, map[string]bool{"confirm": false}, "http://localhost", true), 400)
	a.scheduler.mu.Lock()
	a.scheduler.active = true
	a.scheduler.mu.Unlock()
	ok(t, b.request("POST", endpoint, map[string]bool{"confirm": true}, "http://localhost", true), 409)
	a.scheduler.mu.Lock()
	a.scheduler.active = false
	a.scheduler.mu.Unlock()
	a.maintenance.Store(true)
	ok(t, b.request("POST", endpoint, map[string]bool{"confirm": true}, "http://localhost", true), 503)
	a.maintenance.Store(false)
	r := b.request("GET", "/api/media/staging", nil, "http://localhost", true)
	ok(t, r, 200)
	if !strings.Contains(r.Body.String(), `"partial_bytes":6`) {
		t.Fatal(r.Body.String())
	}
	if _, err := os.Stat(part); err != nil {
		t.Fatal("blocked operations deleted file", err)
	}
	ok(t, b.request("POST", endpoint, map[string]bool{"confirm": true}, "http://localhost", true), 200)
	if _, err := os.Stat(part); !os.IsNotExist(err) {
		t.Fatal(err)
	}
}
