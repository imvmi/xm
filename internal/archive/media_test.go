package archive

import (
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestMediaDownloadAtomicitySizeAndCredentials(t *testing.T) {
	dir := t.TempDir()
	j := MediaJob{AuthorID: "123", Username: "Alice", PostID: "100", ID: "photo1", Kind: "photo", URL: "https://pbs.twimg.com/a.jpg"}
	c := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.Header.Get("Cookie") != "" || r.Header.Get("Authorization") != "" {
			t.Fatal("account credentials leaked to media host")
		}
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"image/jpeg"}}, Body: io.NopCloser(strings.NewReader("image-bytes")), ContentLength: -1}, nil
	})}
	path, size, hash, err := Download(context.Background(), c, dir, j, 100)
	if err != nil || size != 11 || len(hash) != 64 {
		t.Fatal(path, size, hash, err)
	}
	if !strings.HasPrefix(path, "media/users/alice_123/") {
		t.Fatal("media not grouped under author", path)
	}
	j.Path = path
	j.Bytes = size
	j.SHA256 = hash
	if !validLocalMedia(dir, j) {
		t.Fatal("downloaded media not valid")
	}
	if _, _, _, err = Download(context.Background(), c, dir, j, 3); err == nil {
		t.Fatal("oversized body accepted")
	}
	b, _ := os.ReadFile(filepath.Join(dir, path))
	if string(b) != "image-bytes" {
		t.Fatal("failed download damaged existing file")
	}
	entries, _ := os.ReadDir(filepath.Dir(filepath.Join(dir, path)))
	if len(entries) != 1 {
		t.Fatal("partial file leaked")
	}
	os.WriteFile(filepath.Join(dir, path), []byte("corruption"), 0600)
	if validLocalMedia(dir, j) {
		t.Fatal("corruption not detected")
	}
	c.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"text/html"}}, Body: io.NopCloser(strings.NewReader("login"))}, nil
	})
	if _, _, _, err = Download(context.Background(), c, dir, j, 100); err == nil {
		t.Fatal("HTML stored as photo")
	}
	for _, bad := range []string{"http://pbs.twimg.com/a.jpg", "https://evil.example/a.jpg", "https://pbs.twimg.com.evil.example/a.jpg", "https://u:p@pbs.twimg.com/a.jpg", "https://127.0.0.1/a.jpg"} {
		j.URL = bad
		if _, _, _, err = Download(context.Background(), c, dir, j, 100); err == nil {
			t.Fatal("unsafe URL accepted", bad)
		}
	}
}

func TestMediaRedirectCannotEscape(t *testing.T) {
	c := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Host != "pbs.twimg.com" {
			t.Fatal("followed foreign redirect")
		}
		return &http.Response{StatusCode: 302, Header: http.Header{"Location": []string{"https://example.com/private"}}, Body: io.NopCloser(strings.NewReader(""))}, nil
	})}
	_, _, _, err := Download(context.Background(), c, t.TempDir(), MediaJob{URL: "https://pbs.twimg.com/a.jpg", Kind: "photo"}, 100)
	if err == nil {
		t.Fatal("unsafe redirect accepted")
	}
}

func TestStoreIdempotencyAndRetryState(t *testing.T) {
	s, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	p, err := ParsePage(fixture(t, "timeline.json"), "UserTweets")
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err = s.SavePosts("alice", p.Posts); err != nil {
			t.Fatal(err)
		}
	}
	var count int
	s.DB.QueryRow("SELECT count(*) FROM posts").Scan(&count)
	if count != 3 {
		t.Fatalf("duplicates: %d", count)
	}
	jobs, err := s.MediaJobs("alice", false)
	if err != nil || len(jobs) != 3 {
		t.Fatal(len(jobs), err)
	}
	if err = s.MediaResult(jobs[0], "", 0, "", errors.New("temporary failure")); err != nil {
		t.Fatal(err)
	}
	jobs, err = s.MediaJobs("alice", false)
	if err != nil || len(jobs) != 3 {
		t.Fatal("retry job lost", err)
	}
	if err = s.MediaResult(jobs[0], "media/test.jpg", 3, "sha", nil); err != nil {
		t.Fatal(err)
	}
	if err = s.SavePosts("alice", p.Posts); err != nil {
		t.Fatal(err)
	}
	jobs, err = s.MediaJobs("alice", false)
	if err != nil || len(jobs) != 2 {
		t.Fatal("completed job reset", err)
	}
	var attempts int
	s.DB.QueryRow("SELECT attempts FROM media WHERE status='done'").Scan(&attempts)
	if attempts != 2 {
		t.Fatal("attempt count incorrect")
	}
	// Simulated batch failure must roll back earlier rows from that batch.
	_, err = s.DB.Exec(`CREATE TRIGGER reject_test BEFORE INSERT ON posts WHEN NEW.id='fail' BEGIN SELECT RAISE(ABORT,'fixture failure'); END`)
	if err != nil {
		t.Fatal(err)
	}
	p1, p2 := p.Posts[0], p.Posts[0]
	p1.ID = "new"
	p2.ID = "fail"
	if err = s.SavePosts("alice", []Post{p1, p2}); err == nil {
		t.Fatal("expected failing batch")
	}
	s.DB.QueryRow("SELECT count(*) FROM posts WHERE id='new'").Scan(&count)
	if count != 0 {
		t.Fatal("batch not atomic")
	}
}
