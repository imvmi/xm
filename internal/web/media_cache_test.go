package web

import (
	"crypto/sha256"
	"encoding/base64"
	"fmt"
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

func seedCachedMedia(t *testing.T, a *App, id, kind, ext string, payload []byte) string {
	t.Helper()
	if err := a.store.SavePosts("alice", []archive.Post{{ID: id, Username: "alice", Assets: []archive.Asset{{ID: "asset", Kind: kind}}}}); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(a.Config().DataDir, "media")
	if err := archive.PrivateDir(dir); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, id+ext), payload, 0600); err != nil {
		t.Fatal(err)
	}
	hash := fmt.Sprintf("%x", sha256.Sum256(payload))
	if err := a.store.MediaResult(archive.MediaJob{PostID: id, ID: "asset", StorageID: "local"}, "media/"+id+ext, int64(len(payload)), hash, nil); err != nil {
		t.Fatal(err)
	}
	return hash
}

func cachedMediaRequest(a *App, b *browserClient, method, path string, headers map[string]string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, "http://localhost"+path, nil)
	if b != nil {
		r.AddCookie(b.cookie)
	}
	for name, value := range headers {
		r.Header.Set(name, value)
	}
	w := httptest.NewRecorder()
	a.Handler().ServeHTTP(w, r)
	return w
}

func TestMediaPrivateCacheValidatorsAndRanges(t *testing.T) {
	a := testApp(t)
	b := login(t, a)
	for _, fixture := range []struct {
		id, kind, ext string
		seconds       int
	}{{"100", "photo", ".jpg", imageCacheSeconds}, {"200", "video", ".mp4", videoCacheSeconds}} {
		hash := seedCachedMedia(t, a, fixture.id, fixture.kind, fixture.ext, []byte("0123456789"))
		path := "/media/" + fixture.id + "/asset?v=" + hash
		etag := `"sha256-` + hash + `"`
		for _, tc := range []struct {
			name, method, body string
			headers            map[string]string
			status             int
		}{
			{"get", "GET", "0123456789", nil, 200},
			{"head", "HEAD", "", nil, 200},
			{"not modified", "GET", "", map[string]string{"If-None-Match": etag}, 304},
			{"head not modified", "HEAD", "", map[string]string{"If-None-Match": etag}, 304},
			{"weak list", "GET", "", map[string]string{"If-None-Match": `"other", W/` + etag}, 304},
			{"wildcard", "GET", "", map[string]string{"If-None-Match": "*"}, 304},
			{"etag overrides date", "GET", "0123456789", map[string]string{"If-None-Match": `"other"`, "If-Modified-Since": time.Now().Add(time.Hour).UTC().Format(http.TimeFormat)}, 200},
			{"range", "GET", "2345", map[string]string{"Range": "bytes=2-5"}, 206},
			{"if range match", "GET", "2345", map[string]string{"Range": "bytes=2-5", "If-Range": etag}, 206},
			{"if range changed", "GET", "0123456789", map[string]string{"Range": "bytes=2-5", "If-Range": `"old"`}, 200},
			{"precondition order", "GET", "", map[string]string{"If-Match": `"old"`, "If-None-Match": etag}, 412},
			{"invalid range", "GET", "", map[string]string{"Range": "bytes=20-"}, 416},
		} {
			t.Run(fixture.kind+"/"+tc.name, func(t *testing.T) {
				w := cachedMediaRequest(a, b, tc.method, path, tc.headers)
				ok(t, w, tc.status)
				if tc.status < 400 {
					if w.Body.String() != tc.body || w.Header().Get("ETag") != etag || w.Header().Get("Cache-Control") != fmt.Sprintf("private, max-age=%d, must-revalidate", fixture.seconds) || !strings.Contains(w.Header().Get("Vary"), "Cookie") {
						t.Fatalf("cache response: %v %q", w.Header(), w.Body.String())
					}
				} else if w.Header().Get("Cache-Control") != "no-store" || w.Header().Get("ETag") != "" {
					t.Fatal("cached failure", w.Header())
				}
			})
		}
		unauthorized := cachedMediaRequest(a, nil, "GET", path, map[string]string{"If-None-Match": etag})
		ok(t, unauthorized, 401)
		if unauthorized.Header().Get("Cache-Control") != "no-store" {
			t.Fatal("unauthenticated cache response")
		}
		legacy := cachedMediaRequest(a, b, "GET", "/media/"+fixture.id+"/asset", nil)
		if legacy.Header().Get("Cache-Control") != "private, no-cache" {
			t.Fatal("unversioned URL received long caching", legacy.Header())
		}
	}
	page, err := listPosts(a.store.DB, "alice", "", "", "", 0, 24)
	if err != nil || len(page.Items) != 2 || page.Items[0].Media[0].Version == "" {
		t.Fatal("media version missing from timeline", page, err)
	}
	oldHash := page.Items[0].Media[0].Version
	newHash := seedCachedMedia(t, a, "200", "video", ".mp4", []byte("replacement"))
	w := cachedMediaRequest(a, b, "GET", "/media/200/asset?v="+oldHash, nil)
	ok(t, w, 404)
	if w.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("old version URL is cacheable")
	}
	w = cachedMediaRequest(a, b, "GET", "/media/200/asset?v="+newHash, nil)
	ok(t, w, 200)
	if w.Body.String() != "replacement" {
		t.Fatal("new content not served")
	}
	if _, err := a.store.DB.Exec("UPDATE media SET sha256='' WHERE post_id='200'"); err != nil {
		t.Fatal(err)
	}
	w = cachedMediaRequest(a, b, "GET", "/media/200/asset", nil)
	if w.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("legacy media without content digest must not get a long cache lifetime")
	}
}

func TestAvatarPrivateCache(t *testing.T) {
	a := testApp(t)
	b := login(t, a)
	payload, _ := base64.StdEncoding.DecodeString("iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mNk+A8AAQUBAScY42YAAAAASUVORK5CYII=")
	dir := filepath.Join(a.Config().DataDir, "avatars")
	if err := archive.PrivateDir(dir); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "42")
	if err := os.WriteFile(path, payload, 0600); err != nil {
		t.Fatal(err)
	}
	w := cachedMediaRequest(a, b, "GET", "/avatars/42", nil)
	ok(t, w, 200)
	etag := w.Header().Get("ETag")
	if etag == "" || w.Header().Get("Cache-Control") != "private, max-age=3600, must-revalidate" {
		t.Fatal(w.Header())
	}
	w = cachedMediaRequest(a, b, "GET", "/avatars/42", map[string]string{"If-None-Match": etag})
	ok(t, w, 304)
	// Change contents within the same second: the digest, not second-resolution
	// mtime, must decide freshness.
	if err := os.WriteFile(path, append(payload, 0), 0600); err != nil {
		t.Fatal(err)
	}
	w = cachedMediaRequest(a, b, "GET", "/avatars/42", map[string]string{"If-None-Match": etag})
	ok(t, w, 200)
	if w.Header().Get("ETag") == etag {
		t.Fatal("avatar validator did not change")
	}
	ok(t, cachedMediaRequest(a, nil, "GET", "/avatars/42", map[string]string{"If-None-Match": etag}), 401)
	for _, path := range []string{"/api/posts", "/api/session", "/api/settings"} {
		if w := cachedMediaRequest(a, b, "GET", path, nil); w.Header().Get("Cache-Control") != "no-store" {
			t.Fatal("API became cacheable", path, w.Header())
		}
	}
}

func TestRemoteMediaPrivateCacheAndColdVideoRanges(t *testing.T) {
	a := testApp(t)
	b := login(t, a)
	payload := "0123456789"
	hash := fmt.Sprintf("%x", sha256.Sum256([]byte(payload)))
	var calls atomic.Int64
	var failing atomic.Bool
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if failing.Load() {
			http.Error(w, "offline", 503)
			return
		}
		http.ServeContent(w, r, "test", time.Time{}, strings.NewReader(payload))
	}))
	defer remote.Close()
	cfg := a.Config()
	cfg.StorageProfiles = []archive.StorageProfile{{ID: "cache-test", Kind: "webdav", Name: "fixture", Endpoint: remote.URL, Prefix: "archive", Timeout: 5}}
	if err := a.store.SaveSettings(cfg); err != nil {
		t.Fatal(err)
	}
	a.config = cfg
	if err := setState(a.store.DB, remoteMediaCacheStateKey, `{"enabled":true,"max_bytes":67108864}`); err != nil {
		t.Fatal(err)
	}
	for _, f := range []struct{ id, kind, ext string }{{"100", "photo", ".jpg"}, {"200", "video", ".mp4"}} {
		if err := a.store.SavePosts("alice", []archive.Post{{ID: f.id, Assets: []archive.Asset{{ID: "asset", Kind: f.kind}}}}); err != nil {
			t.Fatal(err)
		}
		key := "archive/media/" + f.id + f.ext
		if err := a.store.MediaResult(archive.MediaJob{PostID: f.id, ID: "asset", StorageID: "cache-test", ObjectKey: key}, "", int64(len(payload)), hash, nil); err != nil {
			t.Fatal(err)
		}
		path := "/media/" + f.id + "/asset?v=" + hash
		w := cachedMediaRequest(a, b, "GET", path, map[string]string{"Range": "bytes=2-5"})
		ok(t, w, 206)
		if w.Body.String() != "2345" {
			t.Fatal("range data changed", w.Body.String())
		}
		etag := w.Header().Get("ETag")
		if etag == "" || w.Header().Get("Last-Modified") != "" {
			t.Fatal("remote must use stable content validators", w.Header())
		}
		cachePath := remoteMediaCachePath(cfg.DataDir, "cache-test", key, hash, f.ext, int64(len(payload)))
		if f.kind == "video" {
			if _, err := os.Stat(cachePath); !os.IsNotExist(err) {
				t.Fatal("cold video range fetched and cached the whole video", err)
			}
		}
		before := calls.Load()
		failing.Store(true)
		for _, method := range []string{"GET", "HEAD"} {
			w = cachedMediaRequest(a, b, method, path, map[string]string{"If-None-Match": `W/` + etag})
			ok(t, w, 304)
		}
		if calls.Load() != before {
			t.Fatal("conditional hit contacted remote storage")
		}
		if f.kind == "video" {
			w = cachedMediaRequest(a, b, "GET", path, map[string]string{"Range": "bytes=0-3"})
			ok(t, w, 502)
			if w.Header().Get("Cache-Control") != "no-store" || w.Header().Get("ETag") != "" {
				t.Fatal("upstream failure cached", w.Header())
			}
		} else {
			w = cachedMediaRequest(a, b, "GET", path, nil)
			ok(t, w, 200)
			if w.Header().Get("ETag") != etag || w.Header().Get("Last-Modified") != "" || calls.Load() != before {
				t.Fatal("cached file used unstable timestamp/contacted upstream", w.Header())
			}
		}
		failing.Store(false)
	}
}
