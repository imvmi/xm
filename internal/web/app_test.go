package web

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"xm/internal/archive"
)

const testPassword = "test-password-for-local-tests"

func writeTestAdmin(t *testing.T, store *archive.Store, cfg archive.Config) {
	t.Helper()
	if err := archive.PrivateDir(cfg.DataDir); err != nil {
		t.Fatal(err)
	}
	record, err := makePasswordRecord("admin", testPassword)
	if err != nil {
		t.Fatal(err)
	}
	if err = store.Initialize(cfg, record); err != nil {
		t.Fatal(err)
	}
}

func testApp(t *testing.T) *App {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	cfg := archive.Config{DataDir: filepath.Join(dir, "data"), Headless: true, RequestTimeout: 5, CollectionTimeout: 5, ScrollDelay: 1, MaxScrolls: 2, MaxMediaBytes: 1024, DefaultInterval: 900}
	configureMySQLTest(t, &cfg)
	if err := archive.SaveBootstrap(path, cfg); err != nil {
		t.Fatal(err)
	}
	s, err := archive.OpenConfiguredStore(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	writeTestAdmin(t, s, cfg)
	a, err := New(path, cfg, s, false)
	if err != nil {
		t.Fatal(err)
	}
	a.scheduler.download = func(context.Context, *archive.Store, archive.Config, string, bool) (archive.DownloadSummary, error) {
		return archive.DownloadSummary{}, nil
	}
	return a
}

type browserClient struct {
	t      *testing.T
	a      *App
	cookie *http.Cookie
	csrf   string
}

func (b *browserClient) request(method, path string, input any, origin string, csrf bool) *httptest.ResponseRecorder {
	b.t.Helper()
	var body io.Reader
	if input != nil {
		data, _ := json.Marshal(input)
		body = bytes.NewReader(data)
	}
	r := httptest.NewRequest(method, "http://localhost"+path, body)
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Origin", origin)
	if b.cookie != nil {
		r.AddCookie(b.cookie)
	}
	if csrf {
		r.Header.Set("X-CSRF-Token", b.csrf)
	}
	w := httptest.NewRecorder()
	b.a.Handler().ServeHTTP(w, r)
	return w
}
func login(t *testing.T, a *App) *browserClient {
	t.Helper()
	b := &browserClient{t: t, a: a}
	w := b.request("POST", "/api/login", map[string]string{"username": "admin", "password": testPassword}, "http://localhost", false)
	if w.Code != 200 {
		t.Fatal("login failed", w.Code, w.Body.String())
	}
	var v map[string]string
	json.Unmarshal(w.Body.Bytes(), &v)
	b.csrf = v["csrf"]
	b.cookie = w.Result().Cookies()[0]
	if !b.cookie.HttpOnly || b.cookie.SameSite != http.SameSiteStrictMode {
		t.Fatal("unsafe session cookie")
	}
	return b
}
func ok(t *testing.T, w *httptest.ResponseRecorder, code int) {
	t.Helper()
	if w.Code != code {
		t.Fatalf("expected %d, got %d: %s", code, w.Code, w.Body.String())
	}
}

func TestAuthCSRFAndPrivateMedia(t *testing.T) {
	a := testApp(t)
	anon := &browserClient{t: t, a: a}
	for _, p := range []string{"/api/posts", "/api/settings", "/media/1/a", "/api/monitors"} {
		ok(t, anon.request("GET", p, nil, "", false), 401)
	}
	ok(t, anon.request("POST", "/api/login", map[string]string{"username": "admin", "password": testPassword}, "http://evil.example", false), 403)
	b := login(t, a)
	ok(t, b.request("PUT", "/api/settings", map[string]bool{"polling_paused": true}, "http://localhost", false), 403)
	ok(t, b.request("PUT", "/api/settings", map[string]bool{"polling_paused": true}, "http://evil.example", true), 403)
	p := archive.Post{ID: "1", AuthorID: "42", Username: "alice", Text: "private", Raw: json.RawMessage(`{}`), Assets: []archive.Asset{{ID: "a", Kind: "video", URL: "https://video.twimg.com/test.mp4"}}}
	if err := a.store.SavePosts("alice", []archive.Post{p}); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(a.Config().DataDir, "media")
	os.MkdirAll(dir, 0700)
	os.WriteFile(filepath.Join(dir, "test.mp4"), []byte("0123456789"), 0600)
	a.store.MediaResult(archive.MediaJob{PostID: "1", ID: "a"}, "media/test.mp4", 10, "hash", nil)
	r := httptest.NewRequest("GET", "http://localhost/media/1/a", nil)
	r.AddCookie(b.cookie)
	r.Header.Set("Range", "bytes=2-5")
	w := httptest.NewRecorder()
	a.Handler().ServeHTTP(w, r)
	ok(t, w, 206)
	if w.Body.String() != "2345" {
		t.Fatal("video range not honored")
	}
	outside := filepath.Join(t.TempDir(), "secret.mp4")
	os.WriteFile(outside, []byte("secret"), 0600)
	os.Symlink(outside, filepath.Join(dir, "escape.mp4"))
	a.store.DB.Exec("UPDATE media SET local_path='media/escape.mp4'")
	ok(t, b.request("GET", "/media/1/a", nil, "", false), 404)
	a.store.DB.Exec("UPDATE media SET local_path='media/../../admin.json'")
	ok(t, b.request("GET", "/media/1/a", nil, "", false), 404)
	ok(t, b.request("POST", "/api/logout", nil, "http://localhost", true), 200)
	ok(t, b.request("GET", "/api/posts", nil, "", false), 401)
}

func TestSettingsCookiesAndMonitorPersistence(t *testing.T) {
	a := testApp(t)
	b := login(t, a)
	ok(t, b.request("POST", "/api/cookies", map[string]string{"content": `{"auth_token":"local-test-secret","ct0":"test-csrf"}`}, "http://localhost", true), 200)
	var token, csrf string
	if err := a.store.DB.QueryRow("SELECT auth_token,ct0 FROM x_cookies WHERE id=1").Scan(&token, &csrf); err != nil || token != "local-test-secret" || csrf != "test-csrf" {
		t.Fatal("Cookie import did not write database plaintext", err)
	}
	if _, err := os.Stat(filepath.Join(a.Config().DataDir, "cookies.enc")); !os.IsNotExist(err) {
		t.Fatal("Cookie import wrote legacy file")
	}

	w := b.request("GET", "/api/settings", nil, "", false)
	ok(t, w, 200)
	if strings.Contains(w.Body.String(), "local-test-secret") {
		t.Fatal("cookie leaked")
	}
	ok(t, b.request("PUT", "/api/settings", map[string]any{"proxy": "socks5://user:proxy-secret@127.0.0.1:1080", "default_interval_seconds": 1200}, "http://localhost", true), 200)
	w = b.request("GET", "/api/settings", nil, "", false)
	if strings.Contains(w.Body.String(), "proxy-secret") {
		t.Fatal("proxy password leaked")
	}
	ok(t, b.request("PUT", "/api/settings", map[string]any{"polling_paused": true}, "http://localhost", true), 200)
	cfg, err := archive.LoadConfig(a.configPath)
	if err != nil || !cfg.PollingPaused || cfg.DefaultInterval != 1200 || !strings.Contains(cfg.Proxy, "proxy-secret") {
		t.Fatal("settings did not persist", err)
	}
	input := map[string]any{"username": "@Alice", "interval_seconds": 60, "limit": 20, "replies": true, "enabled": true}
	ok(t, b.request("POST", "/api/monitors", input, "http://localhost", true), 201)
	ok(t, b.request("POST", "/api/monitors", input, "http://localhost", true), 409)
	input["interval_seconds"] = 1
	ok(t, b.request("PUT", "/api/monitors/alice", input, "http://localhost", true), 400)
	input["interval_seconds"] = 300
	input["enabled"] = false
	ok(t, b.request("PUT", "/api/monitors/alice", input, "http://localhost", true), 200)
	// MySQL reports zero affected rows when an identical update changes nothing.
	ok(t, b.request("PUT", "/api/monitors/alice", input, "http://localhost", true), 200)
	list, err := monitors(a.store.DB)
	if err != nil || len(list) != 1 || list[0].Enabled || list[0].Interval != 300 {
		t.Fatal("monitor update failed", err)
	}
	ok(t, b.request("POST", "/api/monitors/alice/run", nil, "http://localhost", true), 202)
	ok(t, b.request("POST", "/api/monitors/alice/run", nil, "http://localhost", true), 409)
	ok(t, b.request("POST", "/api/monitors/alice/stop", nil, "http://localhost", true), 200)
	ok(t, b.request("POST", "/api/monitors/alice/stop", nil, "http://localhost", true), 200)
	ok(t, b.request("DELETE", "/api/monitors/alice", nil, "http://localhost", true), 200)
	ok(t, b.request("DELETE", "/api/monitors/alice", nil, "http://localhost", true), 404)
}

func TestArchivePaginationSearchAndMediaFilter(t *testing.T) {
	a := testApp(t)
	b := login(t, a)
	for i := 0; i < 30; i++ {
		id := time.Unix(int64(i), 0).Format("150405")
		p := archive.Post{ID: id, AuthorID: "42", Username: "alice", Text: "中文 archive <script>alert(1)</script>", Raw: json.RawMessage(`{}`)}
		if i%2 == 0 {
			p.Assets = []archive.Asset{{ID: "photo", Kind: "photo", URL: "https://pbs.twimg.com/x.jpg"}}
		}
		if err := a.store.SavePosts("alice", []archive.Post{p}); err != nil {
			t.Fatal(err)
		}
	}
	var list PostList
	w := b.request("GET", "/api/posts?kind=photo&q=archive", nil, "", false)
	ok(t, w, 200)
	json.Unmarshal(w.Body.Bytes(), &list)
	if list.Total != 15 || len(list.Items) != 15 || len(list.Items[0].Media) != 1 {
		t.Fatal("filter mismatch", list.Total, len(list.Items))
	}
	w = b.request("GET", "/api/posts?offset=24", nil, "", false)
	json.Unmarshal(w.Body.Bytes(), &list)
	if list.Total != 30 || len(list.Items) != 6 {
		t.Fatal("pagination mismatch")
	}
	if strings.Contains(w.Body.String(), "<script>") {
		t.Fatal("JSON should HTML-escape dangerous text")
	}
}
