package archive

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const testAvatarURL = "https://pbs.twimg.com/profile_images/42/alice_normal.png"

func avatarFixture(t *testing.T, dir, id, source string) []byte {
	t.Helper()
	data, _ := base64.StdEncoding.DecodeString("iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mNk+A8AAQUBAScY42YAAAAASUVORK5CYII=")
	if err := PrivateDir(dir); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, id), data, 0600); err != nil {
		t.Fatal(err)
	}
	if source != "" {
		if err := writeAvatarMetadata(filepath.Join(dir, id+".json"), avatarMetadata{SourceURL: source, CheckedAt: time.Now()}); err != nil {
			t.Fatal(err)
		}
	}
	return data
}

func TestAvatarSourceSavedWithProfile(t *testing.T) {
	s, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	p := Profile{ID: "42", Username: "alice", AvatarURL: testAvatarURL}
	if err = s.SaveTargetProfile("alice", p); err != nil {
		t.Fatal(err)
	}
	if s.AvatarSource("42") != testAvatarURL {
		t.Fatal("original URL not saved")
	}
	p.AvatarURL = "https://pbs.twimg.com/profile_images/42/changed_normal.png"
	if err = s.SaveProfile(p); err != nil {
		t.Fatal(err)
	}
	if s.AvatarSource("42") != p.AvatarURL {
		t.Fatal("changed source not saved")
	}
	for _, invalid := range []string{"", "http://pbs.twimg.com/a.png", "https://evil.test/a.png"} {
		if err = s.SaveTargetProfile("alice", Profile{ID: "42", Username: "alice", AvatarURL: invalid}); err != nil {
			t.Fatal(err)
		}
		if s.AvatarSource("42") != p.AvatarURL {
			t.Fatal("missing/invalid source erased known source")
		}
	}
	if _, err = s.DB.Exec(`CREATE TRIGGER fail_avatar BEFORE UPDATE ON avatar_sources BEGIN SELECT RAISE(ABORT,'fixture'); END`); err != nil {
		t.Fatal(err)
	}
	if err = s.SaveTargetProfile("alice", Profile{ID: "42", Username: "renamed", AvatarURL: testAvatarURL}); err == nil {
		t.Fatal("expected transaction failure")
	}
	identity, err := s.TargetIdentity("alice")
	if err != nil || identity.CurrentUsername != "alice" {
		t.Fatal("profile and source were not atomic", identity, err)
	}
}

func TestLegacyAvatarMigrationPreservesOriginsAndUnknownFiles(t *testing.T) {
	dir := t.TempDir()
	s, err := OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	oldDir := filepath.Join(dir, "avatars")
	data := avatarFixture(t, oldDir, "42", testAvatarURL)
	avatarFixture(t, oldDir, "43", "")
	avatarFixture(t, oldDir, "44", "https://unsafe.example/avatar.png")
	avatarFixture(t, oldDir, "45", testAvatarURL)
	// Metadata without an image must still recover its URL.
	if err = os.Remove(filepath.Join(oldDir, "45")); err != nil {
		t.Fatal(err)
	}
	result, err := s.MigrateLegacyAvatars(context.Background(), dir)
	if err != nil || result.Imported != 2 || result.Migrated != 1 || result.Retained != 2 || result.Failed != 0 {
		t.Fatal(result, err)
	}
	if s.AvatarSource("42") != testAvatarURL || s.AvatarSource("45") != testAvatarURL {
		t.Fatal("source lost")
	}
	got, err := os.ReadFile(filepath.Join(AvatarCacheDir(dir), "42"))
	if err != nil || string(got) != string(data) {
		t.Fatal("cache differs", err)
	}
	if _, err = os.Stat(filepath.Join(oldDir, "42")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("legacy copy not removed", err)
	}
	for _, id := range []string{"43", "44"} {
		if !AvatarExists(dir, id) {
			t.Fatal("unknown origin was deleted", id)
		}
	}
	// A newer DB source must never be replaced with the older sidecar URL.
	if err = s.SaveProfile(Profile{ID: "46", Username: "newer", AvatarURL: strings.Replace(testAvatarURL, "alice", "newer", 1)}); err != nil {
		t.Fatal(err)
	}
	avatarFixture(t, oldDir, "46", testAvatarURL)
	result, err = s.MigrateLegacyAvatars(context.Background(), dir)
	if err != nil || result.Imported != 0 || result.Migrated != 0 || result.Retained != 3 {
		t.Fatal("retry was not idempotent", result, err)
	}
	if s.AvatarSource("46") == testAvatarURL {
		t.Fatal("overwrote newer source")
	}
	// Clearing deletes only cache files with a durable, valid origin.
	avatarFixture(t, AvatarCacheDir(dir), "47", "")
	n, err := s.ClearAvatarCache(context.Background(), dir)
	if err != nil || n != 1 {
		t.Fatal(n, err)
	}
	if cachedAvatarExists(dir, "42") || s.AvatarSource("42") != testAvatarURL || !cachedAvatarExists(dir, "47") || !AvatarExists(dir, "43") {
		t.Fatal("unsafe cache clearing")
	}
}

func TestLegacyAvatarMigrationRetryAfterFailure(t *testing.T) {
	dir := t.TempDir()
	s, err := OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	oldDir := filepath.Join(dir, "avatars")
	avatarFixture(t, oldDir, "42", testAvatarURL)
	if _, err = s.DB.Exec(`CREATE TRIGGER fail_avatar BEFORE INSERT ON avatar_sources BEGIN SELECT RAISE(ABORT,'fixture'); END`); err != nil {
		t.Fatal(err)
	}
	result, err := s.MigrateLegacyAvatars(context.Background(), dir)
	if err != nil || result.Failed != 1 || !AvatarExists(dir, "42") || cachedAvatarExists(dir, "42") {
		t.Fatal("DB failure damaged old avatar", result, err)
	}
	if _, err = s.DB.Exec("DROP TRIGGER fail_avatar"); err != nil {
		t.Fatal(err)
	}
	// Simulate cache destination failure without relying on OS permission semantics.
	if err = os.WriteFile(filepath.Join(dir, "cache"), []byte("blocked"), 0600); err != nil {
		t.Fatal(err)
	}
	result, err = s.MigrateLegacyAvatars(context.Background(), dir)
	if err != nil || result.Failed != 1 || !AvatarExists(dir, "42") || s.AvatarSource("42") != testAvatarURL {
		t.Fatal("cache failure lost origin or file", result, err)
	}
	if err = os.Remove(filepath.Join(dir, "cache")); err != nil {
		t.Fatal(err)
	}
	result, err = s.MigrateLegacyAvatars(context.Background(), dir)
	if err != nil || result.Migrated != 1 || result.Failed != 0 {
		t.Fatal("retry failed", result, err)
	}
}

func TestAvatarRefreshReplacementAndFailureCooldown(t *testing.T) {
	dir := t.TempDir()
	original := avatarFixture(t, AvatarCacheDir(dir), "42", testAvatarURL)
	p := Profile{ID: "42", AvatarURL: strings.Replace(testAvatarURL, "alice", "changed", 1)}
	refreshed := append(append([]byte{}, original...), 1)
	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(string(refreshed)))}, nil
	})}
	if err := downloadAvatar(context.Background(), client, dir, p); err != nil {
		t.Fatal("replacement failed", err)
	}
	got, err := os.ReadFile(filepath.Join(AvatarCacheDir(dir), "42"))
	if err != nil || string(got) != string(refreshed) {
		t.Fatal("replacement differs", err)
	}
	// An offline origin is attempted only once during the retry interval.
	attempts := 0
	client.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) { attempts++; return nil, errors.New("offline") })
	p.ID = "99"
	for i := 0; i < 3; i++ {
		if downloadAvatar(context.Background(), client, dir, p) == nil {
			t.Fatal("expected failure")
		}
	}
	if attempts != 1 {
		t.Fatal("missing retry backoff", attempts)
	}
	b, err := os.ReadFile(filepath.Join(AvatarCacheDir(dir), "99.json"))
	if err != nil {
		t.Fatal(err)
	}
	var meta avatarMetadata
	if json.Unmarshal(b, &meta) != nil || meta.SourceURL != p.AvatarURL || meta.RetryAfter.IsZero() {
		t.Fatal("retry metadata missing")
	}
}

func TestAvatarConcurrentMissDownloadsOnce(t *testing.T) {
	dir := t.TempDir()
	png, _ := base64.StdEncoding.DecodeString("iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mNk+A8AAQUBAScY42YAAAAASUVORK5CYII=")
	var requests atomic.Int32
	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		requests.Add(1)
		return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(string(png)))}, nil
	})}
	results := make(chan error, 8)
	for i := 0; i < 8; i++ {
		go func() {
			results <- downloadAvatar(context.Background(), client, dir, Profile{ID: "42", AvatarURL: testAvatarURL})
		}()
	}
	for i := 0; i < 8; i++ {
		if err := <-results; err != nil {
			t.Fatal(err)
		}
	}
	if requests.Load() != 1 {
		t.Fatal("concurrent miss downloaded more than once", requests.Load())
	}
}

func TestAvatarCorruptCacheRefilled(t *testing.T) {
	dir := t.TempDir()
	image := avatarFixture(t, AvatarCacheDir(dir), "42", testAvatarURL)
	if err := os.WriteFile(filepath.Join(AvatarCacheDir(dir), "42"), []byte("broken"), 0600); err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.Header.Get("If-None-Match") != "" || r.Header.Get("If-Modified-Since") != "" {
			t.Error("corrupt cache used conditional request")
		}
		return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(string(image)))}, nil
	})}
	if err := downloadAvatar(context.Background(), client, dir, Profile{ID: "42", AvatarURL: testAvatarURL}); err != nil {
		t.Fatal(err)
	}
	if !cachedAvatarExists(dir, "42") {
		t.Fatal("corrupt cache did not recover")
	}
}
