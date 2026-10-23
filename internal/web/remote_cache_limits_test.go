package web

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
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

func cacheRequest(method, body string) *http.Request {
	r := httptest.NewRequest(method, "http://localhost/api/storage/cache", strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	return r
}

func TestRemoteCacheReservesSpaceAndSerializesFills(t *testing.T) {
	a := testApp(t)
	started, release := make(chan struct{}), make(chan struct{})
	var gets atomic.Int64
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "8")
		if r.Method == "HEAD" {
			return
		}
		gets.Add(1)
		_, _ = w.Write([]byte("1234"))
		w.(http.Flusher).Flush()
		close(started)
		select {
		case <-release:
			_, _ = w.Write([]byte("5678"))
		case <-r.Context().Done():
		}
	}))
	defer remote.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// Always unblock the fixture even when an assertion fails.
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()
	p := archive.StorageProfile{ID: "cache", Name: "Cache", Kind: "webdav", Endpoint: remote.URL, Timeout: 5}
	cfg := a.Config()
	cfg.StorageProfiles = []archive.StorageProfile{p}
	a.config = cfg
	if err := setState(a.store.DB, remoteMediaCacheStateKey, `{"enabled":true,"max_bytes":67108864}`); err != nil {
		t.Fatal(err)
	}
	root := remoteMediaCacheRoot(cfg.DataDir)
	if err := archive.PrivateDir(root); err != nil {
		t.Fatal(err)
	}
	old := filepath.Join(root, "old.jpg")
	if err := os.WriteFile(old, []byte("12345678"), 0600); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte("12345678"))
	type outcome struct {
		path string
		err  error
	}
	done := make(chan outcome, 1)
	go func() {
		path, err := a.ensureRemoteMediaCached(ctx, p.ID, "one.jpg", 8, hex.EncodeToString(sum[:]), ".jpg", 10)
		done <- outcome{path, err}
	}()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("fill did not start")
	}
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Fatal("old cache was not evicted before download", err)
	}
	// A different image must not consume a second disk reservation.
	if _, err := a.ensureRemoteMediaCached(ctx, p.ID, "two.jpg", 8, "", ".jpg", 10); err == nil {
		t.Fatal("second fill admitted")
	}
	if gets.Load() != 1 {
		t.Fatal("concurrent fill contacted remote")
	}
	w := httptest.NewRecorder()
	a.clearRemoteMediaCache(w, cacheRequest("DELETE", ""))
	ok(t, w, 409)
	w = httptest.NewRecorder()
	a.updateRemoteMediaCache(w, cacheRequest("PUT", `{"enabled":false,"max_bytes":67108864}`))
	ok(t, w, 409)
	if !a.remoteMediaCacheSettings().Enabled {
		t.Fatal("busy update persisted")
	}
	close(release)
	result := <-done
	if result.err != nil {
		t.Fatal(result.err)
	}
	if b, err := os.ReadFile(result.path); err != nil || string(b) != "12345678" {
		t.Fatal(string(b), err)
	}
	if used, err := remoteMediaCacheUsage(cfg.DataDir); err != nil || used > 10 {
		t.Fatal("quota exceeded", used, err)
	}
	// Low free disk and a stale larger quota both reject before opening remote.
	a.cacheDiskAvailable = func(string) (uint64, error) { return remoteCacheDiskReserve + 7, nil }
	if _, err := a.ensureRemoteMediaCached(ctx, p.ID, "low-space.jpg", 8, "", ".jpg", 10); err == nil {
		t.Fatal("disk reserve ignored")
	}
	a.cacheDiskAvailable = func(string) (uint64, error) { return 0, errors.New("disk unavailable") }
	if _, err := a.ensureRemoteMediaCached(ctx, p.ID, "disk-error.jpg", 8, "", ".jpg", 10); err == nil {
		t.Fatal("disk error ignored")
	}
	if _, err := a.ensureRemoteMediaCached(ctx, p.ID, "stale.jpg", 80<<20, "", ".jpg", 128<<20); err == nil {
		t.Fatal("stale capacity ignored")
	}
	if gets.Load() != 1 {
		t.Fatal("rejected reservation contacted remote")
	}
	w = httptest.NewRecorder()
	a.clearRemoteMediaCache(w, cacheRequest("DELETE", ""))
	ok(t, w, 200)
	w = httptest.NewRecorder()
	a.updateRemoteMediaCache(w, cacheRequest("PUT", `{"enabled":false,"max_bytes":67108864}`))
	ok(t, w, 200)
	if _, err := a.ensureRemoteMediaCached(ctx, p.ID, "disabled.jpg", 8, "", ".jpg", 10); err == nil {
		t.Fatal("stale request filled disabled cache")
	}
}

func TestRemoteCacheFailedFillsReleaseSpace(t *testing.T) {
	a := testApp(t)
	var mode atomic.Int32
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "HEAD" {
			w.Header().Set("Content-Length", "4")
			return
		}
		switch mode.Load() {
		case 0:
			_, _ = w.Write([]byte("12345"))
		case 1:
			_, _ = w.Write([]byte("12"))
		case 2:
			<-r.Context().Done()
		default:
			_, _ = w.Write([]byte("1234"))
		}
	}))
	defer remote.Close()
	p := archive.StorageProfile{ID: "cache", Name: "Cache", Kind: "webdav", Endpoint: remote.URL, Timeout: 5}
	cfg := a.Config()
	cfg.StorageProfiles = []archive.StorageProfile{p}
	a.config = cfg
	if err := setState(a.store.DB, remoteMediaCacheStateKey, `{"enabled":true,"max_bytes":67108864}`); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name string
		mode int32
		hash string
	}{{"oversized", 0, ""}, {"short", 1, ""}, {"cancelled", 2, ""}, {"corrupt", 3, strings.Repeat("0", 64)}} {
		t.Run(test.name, func(t *testing.T) {
			mode.Store(test.mode)
			ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
			defer cancel()
			if _, err := a.ensureRemoteMediaCached(ctx, p.ID, "test.jpg", 4, test.hash, ".jpg", 4); err == nil {
				t.Fatal("bad fill accepted")
			}
			entries, err := os.ReadDir(remoteMediaCacheRoot(cfg.DataDir))
			if err != nil || len(entries) != 0 {
				t.Fatal("failed fill leaked files", entries, err)
			}
		})
	}
	mode.Store(3)
	if _, err := a.ensureRemoteMediaCached(context.Background(), p.ID, "test.jpg", 4, "", ".jpg", 4); err != nil {
		t.Fatal("reservation not released", err)
	}
}

func TestRemoteCacheStartupRemovesCrashFiles(t *testing.T) {
	a := testApp(t)
	cfg := a.Config()
	root := remoteMediaCacheRoot(cfg.DataDir)
	if err := archive.PrivateDir(root); err != nil {
		t.Fatal(err)
	}
	for name, value := range map[string]string{".cache-crashed": "incomplete", "valid.jpg": "complete"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte(value), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if used, err := remoteMediaCacheUsage(cfg.DataDir); err != nil || used != 18 {
		t.Fatal("temporary bytes missing from usage", used, err)
	}
	restarted, err := New(a.configPath, cfg, a.store, false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(restarted.migrations.stop)
	if _, err := os.Stat(filepath.Join(root, ".cache-crashed")); !os.IsNotExist(err) {
		t.Fatal("crash file retained", err)
	}
	if data, err := os.ReadFile(filepath.Join(root, "valid.jpg")); err != nil || string(data) != "complete" {
		t.Fatal("valid cache removed", err)
	}
	if err := restarted.recoverRemoteMediaCache(); err != nil {
		t.Fatal("recovery not idempotent", err)
	}
}
