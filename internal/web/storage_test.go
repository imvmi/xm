package web

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"xm/internal/archive"
)

func TestRemoteMediaCacheAvoidsRepeatedStorageReads(t *testing.T) {
	a := testApp(t)
	b := login(t, a)
	payload := []byte("cached remote media")
	var heads, gets atomic.Int64
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodHead:
			heads.Add(1)
		case http.MethodGet:
			gets.Add(1)
		}
		http.ServeContent(w, r, "cached.jpg", time.Time{}, strings.NewReader(string(payload)))
	}))
	defer remote.Close()
	p := archive.StorageProfile{ID: "cache-fixture", Name: "Cache fixture", Kind: "webdav", Endpoint: remote.URL, Prefix: "archive", Timeout: 5}
	cfg := a.Config()
	cfg.StorageProfiles = []archive.StorageProfile{p}
	cfg.MediaStorage = p.ID
	if err := a.store.SaveSettings(cfg); err != nil {
		t.Fatal(err)
	}
	a.config = cfg
	if err := a.store.SavePosts("alice", []archive.Post{{ID: "20", Assets: []archive.Asset{{ID: "photo", Kind: "photo"}}}}); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(payload)
	if err := a.store.MediaResult(archive.MediaJob{PostID: "20", ID: "photo", StorageID: p.ID, ObjectKey: "archive/media/cached.jpg"}, "", int64(len(payload)), hex.EncodeToString(sum[:]), nil); err != nil {
		t.Fatal(err)
	}

	ok(t, b.request("PUT", "/api/storage/cache", map[string]any{"enabled": true, "max_bytes": int64(64 << 20)}, "http://localhost", true), 200)
	first := b.request("GET", "/media/20/photo", nil, "", false)
	ok(t, first, 200)
	if first.Body.String() != string(payload) || first.Header().Get("X-XM-Media-Cache") != "hit" || heads.Load() == 0 || gets.Load() == 0 {
		t.Fatal("first request did not populate cache", first.Body.String(), heads.Load(), gets.Load())
	}
	remoteCalls := heads.Load() + gets.Load()
	second := b.request("GET", "/media/20/photo", nil, "", false)
	ok(t, second, 200)
	if second.Body.String() != string(payload) || second.Header().Get("X-XM-Media-Cache") != "hit" || heads.Load()+gets.Load() != remoteCalls {
		t.Fatal("cache hit contacted remote storage", heads.Load(), gets.Load())
	}
	settings := b.request("GET", "/api/storage", nil, "", false)
	ok(t, settings, 200)
	if !strings.Contains(settings.Body.String(), `"enabled":true`) || !strings.Contains(settings.Body.String(), `"used_bytes":19`) {
		t.Fatal("cache status missing", settings.Body.String())
	}
	ok(t, b.request("PUT", "/api/storage/cache", map[string]any{"enabled": true, "max_bytes": 1}, "http://localhost", true), 400)
	ok(t, b.request("DELETE", "/api/storage/cache", nil, "http://localhost", true), 200)
	used, err := remoteMediaCacheUsage(a.Config().DataDir)
	if err != nil || used != 0 {
		t.Fatal("cache was not cleared", used, err)
	}
}

func TestRemoteMediaCachePrunesLeastRecentlyUsedFiles(t *testing.T) {
	a := testApp(t)
	root := remoteMediaCacheRoot(a.Config().DataDir)
	if err := archive.PrivateDir(root); err != nil {
		t.Fatal(err)
	}
	oldPath := filepath.Join(root, "old.jpg")
	newPath := filepath.Join(root, "new.jpg")
	if err := os.WriteFile(oldPath, []byte("12345678"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(newPath, []byte("abcdefgh"), 0600); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(oldPath, old, old); err != nil {
		t.Fatal(err)
	}
	if err := a.pruneRemoteMediaCache(8, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(oldPath); !os.IsNotExist(err) {
		t.Fatal("oldest entry was not evicted", err)
	}
	if data, err := os.ReadFile(newPath); err != nil || string(data) != "abcdefgh" {
		t.Fatal("newest entry was removed", err)
	}
}

func TestStorageSettingsAndRemoteMedia(t *testing.T) {
	a := testApp(t)
	b := login(t, a)
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u, p, ok := r.BasicAuth()
		if !ok || u != "nas-user" || p != "rotated-password" {
			w.WriteHeader(401)
			return
		}
		if r.Header.Get("Cookie") != "" {
			t.Error("XM cookie sent upstream")
		}
		http.ServeContent(w, r, "test.mp4", time.Time{}, strings.NewReader("0123456789"))
	}))
	defer remote.Close()
	p := archive.StorageProfile{ID: "fixture", Name: "Home NAS", Kind: "webdav", Endpoint: remote.URL, Prefix: "archive", Username: "nas-user", Password: "rotated-password", Timeout: 5}
	// Seed the already-initialized destination; production configuration is immutable.
	cfg := a.Config()
	cfg.StorageProfiles = []archive.StorageProfile{p}
	cfg.MediaStorage = p.ID
	if err := a.store.SaveSettings(cfg); err != nil {
		t.Fatal(err)
	}
	a.config = cfg
	var e error
	saved := p
	w := b.request("GET", "/api/storage", nil, "", false)
	ok(t, w, 200)
	for _, secret := range []string{"rotated-password", "secret_key", "session_token", "access_key"} {
		if strings.Contains(w.Body.String(), secret) {
			t.Fatal("credential echoed")
		}
	}
	if !strings.Contains(w.Body.String(), `"locked":true`) {
		t.Fatal("storage not locked")
	}
	ok(t, b.request("POST", "/api/storage/profiles", p, "http://localhost", true), 403)
	ok(t, b.request("PUT", "/api/storage/active", map[string]string{"id": "local"}, "http://localhost", true), 403)
	changed := a.Config()
	changed.MediaStorage = "local"
	if e = a.saveConfig(changed); e == nil {
		t.Fatal("internal storage mutation accepted")
	}
	changed = a.Config()
	changed.Database = archive.DatabaseConfig{Driver: "mysql"}
	if e = a.saveConfig(changed); e == nil {
		t.Fatal("internal database mutation accepted")
	}
	if e = a.store.SavePosts("alice", []archive.Post{{ID: "10", Assets: []archive.Asset{{ID: "v", Kind: "video"}}}}); e != nil {
		t.Fatal(e)
	}
	if e = a.store.MediaResult(archive.MediaJob{PostID: "10", ID: "v", StorageID: saved.ID, ObjectKey: "archive/media/test.mp4"}, "", 10, "hash", nil); e != nil {
		t.Fatal(e)
	}
	anon := &browserClient{t: t, a: a}
	ok(t, anon.request("GET", "/api/storage", nil, "", false), 401)
	ok(t, anon.request("GET", "/media/10/v", nil, "", false), 401)
	for _, test := range []struct {
		method, rangeValue, body string
		code                     int
	}{{"GET", "bytes=2-5", "2345", 206}, {"GET", "bytes=-2", "89", 206}, {"GET", "bytes=99-", "", 416}, {"HEAD", "", "", 200}, {"GET", "", "0123456789", 200}} {
		r := httptest.NewRequest(test.method, "http://localhost/media/10/v", nil)
		r.AddCookie(b.cookie)
		r.Header.Set("Range", test.rangeValue)
		w = httptest.NewRecorder()
		a.Handler().ServeHTTP(w, r)
		ok(t, w, test.code)
		if test.code != 416 && w.Body.String() != test.body {
			t.Fatal("wrong media", w.Body.String())
		}
		if w.Header().Get("Location") != "" || w.Header().Get("Authorization") != "" {
			t.Fatal("remote exposed")
		}
	}
	ok(t, b.request("POST", "/api/storage/profiles", p, "http://evil.example", true), 403)
	ok(t, b.request("POST", "/api/storage/profiles/"+saved.ID+"/test", nil, "http://localhost", false), 403)
	// Existing local media is still served when a remote destination is active.
	ok(t, b.request("PUT", "/api/storage/active", map[string]string{"id": saved.ID}, "http://localhost", true), 403)
	if e = a.store.SavePosts("alice", []archive.Post{{ID: "11", Assets: []archive.Asset{{ID: "p", Kind: "photo"}}}}); e != nil {
		t.Fatal(e)
	}
	archive.PrivateDir(filepath.Join(a.Config().DataDir, "media"))
	os.WriteFile(filepath.Join(a.Config().DataDir, "media/local.jpg"), []byte("local image"), 0600)
	a.store.MediaResult(archive.MediaJob{PostID: "11", ID: "p"}, "media/local.jpg", 11, "hash", nil)
	w = b.request("GET", "/media/11/p", nil, "", false)
	ok(t, w, 200)
	if w.Body.String() != "local image" {
		t.Fatal("local archive broken")
	}
}
