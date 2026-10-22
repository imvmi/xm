package archive

import (
	"context"
	"encoding/base64"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDownloadAvatarCachesImageAndRejectsUnsafeContent(t *testing.T) {
	dir := t.TempDir()
	png, err := base64.StdEncoding.DecodeString("iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mNk+A8AAQUBAScY42YAAAAASUVORK5CYII=")
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Host != "pbs.twimg.com" || !strings.Contains(r.URL.Path, "_400x400.png") {
			t.Fatal("unexpected avatar request", r.URL)
		}
		if r.Header.Get("Cookie") != "" || r.Header.Get("Authorization") != "" {
			t.Fatal("account credentials leaked to avatar host")
		}
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"image/png"}}, Body: io.NopCloser(strings.NewReader(string(png))), ContentLength: int64(len(png))}, nil
	})}
	p := Profile{ID: "42", Username: "alice", AvatarURL: "https://pbs.twimg.com/profile_images/42/alice_normal.png"}
	if err = downloadAvatar(context.Background(), client, dir, p); err != nil {
		t.Fatal(err)
	}
	if !AvatarExists(dir, "42") {
		t.Fatal("cached avatar not found")
	}
	got, err := os.ReadFile(filepath.Join(AvatarCacheDir(dir), "42"))
	if err != nil || string(got) != string(png) {
		t.Fatal("cached avatar differs", err)
	}
	client.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) {
		t.Fatal("fresh unchanged avatar should not be requested")
		return nil, nil
	})
	if err = downloadAvatar(context.Background(), client, dir, p); err != nil {
		t.Fatal("fresh avatar cache not reused", err)
	}

	client.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"text/html"}}, Body: io.NopCloser(strings.NewReader("not an image"))}, nil
	})
	p.AvatarURL = "https://pbs.twimg.com/profile_images/42/alice_changed_normal.png"
	if err = downloadAvatar(context.Background(), client, dir, p); err == nil {
		t.Fatal("non-image avatar accepted")
	}
	got, _ = os.ReadFile(filepath.Join(AvatarCacheDir(dir), "42"))
	if string(got) != string(png) {
		t.Fatal("failed refresh damaged existing avatar")
	}
	for _, raw := range []string{"http://pbs.twimg.com/a.png", "https://evil.example/a.png", "https://pbs.twimg.com.evil.example/a.png", "https://u:p@pbs.twimg.com/a.png"} {
		p.AvatarURL = raw
		if err = downloadAvatar(context.Background(), client, dir, p); err == nil {
			t.Fatal("unsafe avatar URL accepted", raw)
		}
	}
}

func TestDownloadAvatarUsesConditionalRefresh(t *testing.T) {
	dir := t.TempDir()
	avatarDir := AvatarCacheDir(dir)
	if err := PrivateDir(avatarDir); err != nil {
		t.Fatal(err)
	}
	png, _ := base64.StdEncoding.DecodeString("iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mNk+A8AAQUBAScY42YAAAAASUVORK5CYII=")
	if err := os.WriteFile(filepath.Join(avatarDir, "42"), png, 0600); err != nil {
		t.Fatal(err)
	}
	source := "https://pbs.twimg.com/profile_images/42/alice_normal.png"
	metaPath := filepath.Join(avatarDir, "42.json")
	if err := writeAvatarMetadata(metaPath, avatarMetadata{SourceURL: source, ETag: `"avatar-v1"`, CheckedAt: time.Now().Add(-25 * time.Hour)}); err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.Header.Get("If-None-Match") != `"avatar-v1"` {
			t.Fatal("missing conditional avatar header")
		}
		return &http.Response{StatusCode: http.StatusNotModified, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(""))}, nil
	})}
	if err := downloadAvatar(context.Background(), client, dir, Profile{ID: "42", AvatarURL: source}); err != nil {
		t.Fatal(err)
	}
	meta := readAvatarMetadata(metaPath)
	if time.Since(meta.CheckedAt) > time.Minute {
		t.Fatal("avatar refresh timestamp not updated")
	}
}
