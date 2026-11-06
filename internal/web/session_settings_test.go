package web

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"testing"
	"time"

	"xm/internal/database"
)

func TestSessionSettings(t *testing.T) {
	a := testApp(t)
	b := login(t, a)
	if b.cookie.MaxAge != 43200 {
		t.Fatal("default cookie lifetime changed", b.cookie.MaxAge)
	}
	r := httptest.NewRequest("GET", "/api/session", nil)
	r.AddCookie(b.cookie)
	original, valid := a.auth.session(r)
	if !valid {
		t.Fatal("new login is invalid")
	}
	ok(t, b.request("PUT", "/api/settings", map[string]int{"session_ttl_seconds": 3600}, "http://localhost", false), 403)
	for _, seconds := range []int{3600, 43200, 86400, 604800, 2592000} {
		ok(t, b.request("PUT", "/api/settings", map[string]int{"session_ttl_seconds": seconds}, "http://localhost", true), 200)
		before := time.Now()
		fresh := login(t, a)
		after := time.Now()
		request := httptest.NewRequest("GET", "/api/session", nil)
		request.AddCookie(fresh.cookie)
		s, ok := a.auth.session(request)
		if !ok || fresh.cookie.MaxAge != seconds || s.Expires.Before(before.Add(time.Duration(seconds)*time.Second)) || s.Expires.After(after.Add(time.Duration(seconds)*time.Second)) {
			t.Fatal("cookie and server expiry do not match requested lifetime", seconds, fresh.cookie, s)
		}
		current, ok := a.auth.session(r)
		if !ok || current != original {
			t.Fatal("settings update or activity changed existing session")
		}
	}
	for _, invalid := range []any{0, -1, 3601, 2592001, 1.5, "86400"} {
		ok(t, b.request("PUT", "/api/settings", map[string]any{"session_ttl_seconds": invalid}, "http://localhost", true), 400)
	}
	ok(t, b.request("PUT", "/api/settings", map[string]string{"archive_theme": "dark"}, "http://localhost", true), 200)
	var settings struct {
		Seconds int `json:"session_ttl_seconds"`
	}
	w := b.request("GET", "/api/settings", nil, "", false)
	ok(t, w, 200)
	if err := json.Unmarshal(w.Body.Bytes(), &settings); err != nil || settings.Seconds != 2592000 {
		t.Fatal("partial or invalid update lost preference", settings, err)
	}
	cfg := a.Config()
	cfg.SessionTTLSeconds = 3600
	if err := a.saveConfig(cfg, func(*database.Tx) error { return errors.New("test rollback") }); err == nil {
		t.Fatal("save unexpectedly succeeded")
	}
	loaded, err := a.store.LoadSettings(a.Config())
	if err != nil || loaded.SessionTTLSeconds != 2592000 || a.Config().SessionTTLSeconds != 2592000 {
		t.Fatal("failed save changed persisted or active preference", err)
	}
	restarted, err := New(a.configPath, loaded, a.store, false)
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.scheduler.closeSession()
	if _, ok := restarted.auth.session(r); ok {
		t.Fatal("restart retained an in-memory session")
	}
	if fresh := login(t, restarted); fresh.cookie.MaxAge != 2592000 {
		t.Fatal("restart lost login lifetime")
	}
	// Force expiry without sleeping for the configured duration.
	key := sha256.Sum256([]byte(b.cookie.Value))
	a.auth.mu.Lock()
	expired := a.auth.sessions[key]
	expired.Expires = time.Now().Add(-time.Second)
	a.auth.sessions[key] = expired
	a.auth.mu.Unlock()
	ok(t, b.request("GET", "/api/settings", nil, "", false), 401)
	a.auth.mu.Lock()
	_, retained := a.auth.sessions[key]
	a.auth.mu.Unlock()
	if retained {
		t.Fatal("expired session was not removed")
	}
}
