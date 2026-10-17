package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"xm/internal/archive"
)

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
	p := archive.StorageProfile{Name: "Home NAS", Kind: "webdav", Endpoint: remote.URL, Prefix: "archive", Username: "nas-user", Password: "initial-password", Timeout: 5}
	w := b.request("POST", "/api/storage/profiles", p, "http://localhost", true)
	ok(t, w, 200)
	var result map[string]string
	json.Unmarshal(w.Body.Bytes(), &result)
	p.ID = result["id"]
	if p.ID == "" {
		t.Fatal("no storage ID")
	}
	p.Password = "rotated-password"
	ok(t, b.request("POST", "/api/storage/profiles", p, "http://localhost", true), 200)
	p.Password = ""
	p.Name = "Renamed NAS"
	ok(t, b.request("POST", "/api/storage/profiles", p, "http://localhost", true), 200)
	saved, e := a.Config().Storage(p.ID)
	if e != nil || saved.Password != "rotated-password" {
		t.Fatal("blank erased credentials")
	}
	clone := a.Config()
	clone.StorageProfiles[0].Name = "mutated snapshot"
	if a.Config().StorageProfiles[0].Name != p.Name {
		t.Fatal("shared mutable configuration")
	}
	w = b.request("GET", "/api/storage", nil, "", false)
	ok(t, w, 200)
	for _, secret := range []string{"initial-password", "rotated-password", "secret_key", "session_token", "access_key"} {
		if strings.Contains(w.Body.String(), secret) {
			t.Fatal("credential echoed", secret)
		}
	}
	ok(t, b.request("PUT", "/api/storage/active", map[string]string{"id": p.ID}, "http://localhost", true), 200)
	cfg, e := archive.LoadConfig(a.configPath)
	if e != nil || cfg.ActiveStorage() != p.ID {
		t.Fatal("storage not persisted", e)
	}
	p.Endpoint = "http://other.example"
	ok(t, b.request("POST", "/api/storage/profiles", p, "http://localhost", true), 400)
	ok(t, b.request("PUT", "/api/storage/active", map[string]string{"id": "unknown"}, "http://localhost", true), 400)
	ok(t, b.request("PUT", "/api/storage/active", map[string]string{"id": "local"}, "http://localhost", true), 200)
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
	ok(t, b.request("PUT", "/api/storage/active", map[string]string{"id": saved.ID}, "http://localhost", true), 200)
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
