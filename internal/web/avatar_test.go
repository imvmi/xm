package web

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"xm/internal/archive"
)

func TestPrivateAvatarAndUserView(t *testing.T) {
	a := testApp(t)
	seedMonitor(t, a, "alice")
	if err := a.store.SaveTargetProfile("alice", archive.Profile{ID: "42", Username: "alice"}); err != nil {
		t.Fatal(err)
	}
	png, _ := base64.StdEncoding.DecodeString("iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mNk+A8AAQUBAScY42YAAAAASUVORK5CYII=")
	dir := filepath.Join(a.Config().DataDir, "avatars")
	if err := archive.PrivateDir(dir); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "42"), png, 0600); err != nil {
		t.Fatal(err)
	}
	anon := &browserClient{t: t, a: a}
	ok(t, anon.request("GET", "/avatars/42", nil, "", false), 401)
	b := login(t, a)
	w := b.request("GET", "/avatars/42", nil, "", false)
	ok(t, w, 200)
	if w.Header().Get("Content-Type") != "image/png" || len(w.Body.Bytes()) != len(png) {
		t.Fatal("avatar response incorrect", w.Header(), w.Body.Len())
	}
	w = b.request("GET", "/api/users", nil, "", false)
	ok(t, w, 200)
	var users []UserView
	if err := json.Unmarshal(w.Body.Bytes(), &users); err != nil || len(users) != 1 || users[0].AvatarURL != "/avatars/42" {
		t.Fatal("avatar URL missing from user view", err, w.Body.String())
	}
	ok(t, b.request("GET", "/avatars/not-an-id", nil, "", false), 404)
}

func TestAvatarCacheRefillAndMaintenanceEndpoints(t *testing.T) {
	a := testApp(t)
	const source = "https://pbs.twimg.com/profile_images/42/alice_normal.png"
	if err := a.store.SaveTargetProfile("alice", archive.Profile{ID: "42", Username: "alice", AvatarURL: source}); err != nil {
		t.Fatal(err)
	}
	seedMonitor(t, a, "alice")
	png, _ := base64.StdEncoding.DecodeString("iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mNk+A8AAQUBAScY42YAAAAASUVORK5CYII=")
	calls := 0
	a.scheduler.avatar = func(ctx context.Context, cfg archive.Config, p archive.Profile) error {
		calls++
		if p.ID != "42" || p.AvatarURL != source {
			t.Error("wrong avatar source", p)
		}
		if err := archive.PrivateDir(archive.AvatarCacheDir(cfg.DataDir)); err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(archive.AvatarCacheDir(cfg.DataDir), p.ID), png, 0600)
	}
	anon := &browserClient{t: t, a: a}
	ok(t, anon.request("GET", "/avatars/42", nil, "", false), 401)
	if calls != 0 {
		t.Fatal("anonymous request fetched image")
	}
	b := login(t, a)
	w := b.request("GET", "/api/users", nil, "", false)
	ok(t, w, 200)
	if !strings.Contains(w.Body.String(), "/avatars/42") {
		t.Fatal("missing cache hid usable avatar", w.Body.String())
	}
	for i := 0; i < 2; i++ {
		ok(t, b.request("GET", "/avatars/42", nil, "", false), 200)
	}
	if calls != 1 {
		t.Fatal("cache hit downloaded twice", calls)
	}
	ok(t, b.request("DELETE", "/api/settings/avatars/cache", nil, "http://localhost", false), 403)
	ok(t, b.request("DELETE", "/api/settings/avatars/cache", nil, "https://elsewhere.test", true), 403)
	a.maintenance.Store(true)
	ok(t, b.request("DELETE", "/api/settings/avatars/cache", nil, "http://localhost", true), 503)
	ok(t, b.request("POST", "/api/settings/avatars/migrate", nil, "http://localhost", true), 503)
	a.maintenance.Store(false)
	ok(t, b.request("DELETE", "/api/settings/avatars/cache", nil, "http://localhost", true), 200)
	if a.store.AvatarSource("42") != source || archive.AvatarExists(a.Config().DataDir, "42") {
		t.Fatal("clear lost URL or left image")
	}
	legacyDir := filepath.Join(a.Config().DataDir, "avatars")
	if err := archive.PrivateDir(legacyDir); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(legacyDir, "42"), png, 0600); err != nil {
		t.Fatal(err)
	}
	ok(t, b.request("GET", "/avatars/42", nil, "", false), 200)
	if calls != 2 {
		t.Fatal("cache miss was not refilled", calls)
	}
	// Invalid IDs and unknown sources never initiate network requests.
	ok(t, b.request("GET", "/avatars/invalid", nil, "", false), 404)
	ok(t, b.request("GET", "/avatars/999", nil, "", false), 404)
	if calls != 2 {
		t.Fatal("unknown source fetched")
	}
	// A corrupt disposable cache entry is replaced rather than hiding the avatar.
	if err := os.WriteFile(filepath.Join(archive.AvatarCacheDir(a.Config().DataDir), "42"), []byte("broken"), 0600); err != nil {
		t.Fatal(err)
	}
	ok(t, b.request("GET", "/avatars/42", nil, "", false), 200)
	if calls != 3 {
		t.Fatal("corrupt cache was not refilled", calls)
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
	if dst.AvatarSource("42") != source {
		t.Fatal("database migration lost avatar origin")
	}
}

func TestAvatarMigrationOnStartupAndManualRetry(t *testing.T) {
	a := testApp(t)
	const source = "https://pbs.twimg.com/profile_images/42/alice_normal.png"
	dir := filepath.Join(a.Config().DataDir, "avatars")
	if err := archive.PrivateDir(dir); err != nil {
		t.Fatal(err)
	}
	png, _ := base64.StdEncoding.DecodeString("iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mNk+A8AAQUBAScY42YAAAAASUVORK5CYII=")
	if err := os.WriteFile(filepath.Join(dir, "42"), png, 0600); err != nil {
		t.Fatal(err)
	}
	meta, _ := json.Marshal(map[string]string{"source_url": source})
	if err := os.WriteFile(filepath.Join(dir, "42.json"), meta, 0600); err != nil {
		t.Fatal(err)
	}
	restarted, err := New(a.configPath, a.Config(), a.store, false)
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.scheduler.closeSession()
	if restarted.store.AvatarSource("42") != source {
		t.Fatal("startup did not import legacy URL")
	}
	if _, err = os.Stat(filepath.Join(dir, "42")); !os.IsNotExist(err) {
		t.Fatal("legacy file not migrated", err)
	}
	b := login(t, restarted)
	ok(t, b.request("GET", "/avatars/42", nil, "", false), 200)
	ok(t, b.request("POST", "/api/settings/avatars/migrate", nil, "http://localhost", true), 200)
	w := b.request("GET", "/api/settings/avatars", nil, "", false)
	ok(t, w, 200)
	var status avatarCacheStatus
	if err = json.Unmarshal(w.Body.Bytes(), &status); err != nil || status.Sources != 1 || status.Cached != 1 || status.Legacy != 0 {
		t.Fatal(status, err)
	}
}
