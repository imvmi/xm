package archive

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCancelUploadRetainsCompleteDownloadAcrossRestart(t *testing.T) {
	dir := t.TempDir()
	s, err := OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { s.Close() }()
	post := Post{ID: "100", AuthorID: "42", Username: "alice", Assets: []Asset{{ID: "video", Kind: "video", URL: "https://video.twimg.com/fixture.mp4"}}}
	if err = s.SavePosts("alice", []Post{post}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "MKCOL" {
			w.WriteHeader(201)
			return
		}
		if r.Method == "PUT" {
			io.Copy(io.Discard, r.Body)
			cancel()
			w.WriteHeader(503)
			return
		}
		w.WriteHeader(404)
	}))
	defer srv.Close()
	p := StorageProfile{ID: "dav", Name: "test", Kind: "webdav", Endpoint: srv.URL, Timeout: 5}
	cfg := Config{DataDir: dir, MaxMediaBytes: 1024, MediaStorage: "dav", StorageProfiles: []StorageProfile{p}}
	calls := 0
	c := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		return mediaResponse(200, "", "", strings.NewReader("abcdef"), 6), nil
	})}
	if _, err = downloadPendingWithClient(ctx, s, cfg, "alice", false, c); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	jobs, err := s.MediaJobs("alice", true)
	if err != nil || len(jobs) != 1 || jobs[0].Status != "pending" || jobs[0].Attempts != 0 || jobs[0].NextAttempt != 0 || !validLocalMedia(dir, jobs[0]) {
		t.Fatal(jobs, err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	cfg.MediaStorage = "local"
	if summary, err := downloadPendingWithClient(context.Background(), s, cfg, "alice", false, c); err != nil || summary.Downloaded != 1 {
		t.Fatal(summary, err)
	}
	if calls != 1 {
		t.Fatal("completed file downloaded again", calls)
	}
}

func TestCompleteCheckpointSurvivesFailedStagingCommit(t *testing.T) {
	dir := t.TempDir()
	s, err := OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	post := Post{ID: "100", AuthorID: "42", Username: "alice", Assets: []Asset{{ID: "video", Kind: "video", URL: "https://video.twimg.com/fixture.mp4"}}}
	if err = s.SavePosts("alice", []Post{post}); err != nil {
		t.Fatal(err)
	}
	if _, err = s.DB.Exec(`CREATE TRIGGER reject_stage BEFORE UPDATE ON media WHEN NEW.local_path<>'' BEGIN SELECT RAISE(FAIL,'simulated disk/DB failure'); END`); err != nil {
		t.Fatal(err)
	}
	calls := 0
	c := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		return mediaResponse(200, "", "", strings.NewReader("abcdef"), 6), nil
	})}
	cfg := Config{DataDir: dir, MaxMediaBytes: 1024}
	if _, err = downloadPendingWithClient(context.Background(), s, cfg, "alice", false, c); err == nil {
		t.Fatal("expected DB failure")
	}
	if _, err = s.DB.Exec("DROP TRIGGER reject_stage"); err != nil {
		t.Fatal(err)
	}
	if summary, err := downloadPendingWithClient(context.Background(), s, cfg, "alice", false, c); err != nil || summary.Downloaded != 1 {
		t.Fatal(summary, err)
	}
	if calls != 1 {
		t.Fatal("complete checkpoint was not reused", calls)
	}
}

func TestStagedDownloadInvalidatedByRefreshedURL(t *testing.T) {
	dir := t.TempDir()
	s, err := OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	post := Post{ID: "100", AuthorID: "42", Username: "alice", Assets: []Asset{{ID: "video", Kind: "video", URL: "https://video.twimg.com/old.mp4"}}, Raw: json.RawMessage(`{"v":1}`)}
	if err = s.SavePosts("alice", []Post{post}); err != nil {
		t.Fatal(err)
	}
	jobs, _ := s.MediaJobs("alice", false)
	j := jobs[0]
	c := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return mediaResponse(200, "", "", strings.NewReader("old"), 3), nil
	})}
	_, _, _, err = downloadMedia(context.Background(), c, dir, j, 1024, func(path string, size int64, hash string) error {
		if err := s.StageMedia(&j, path, size, hash); err != nil {
			return err
		}
		post.Assets[0].URL = "https://video.twimg.com/new.mp4"
		post.Raw = json.RawMessage(`{"v":2}`)
		return s.SavePosts("alice", []Post{post})
	})
	if err != nil {
		t.Fatal(err)
	}
	jobs, _ = s.MediaJobs("alice", false)
	if len(jobs) != 1 || jobs[0].Path != "" {
		t.Fatal("old staged content remains reusable", jobs)
	}
	c.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if !strings.HasSuffix(r.URL.Path, "/new.mp4") {
			t.Error(r.URL)
		}
		return mediaResponse(200, "", "", strings.NewReader("new"), 3), nil
	})
	if _, err = downloadPendingWithClient(context.Background(), s, Config{DataDir: dir, MaxMediaBytes: 1024}, "alice", false, c); err != nil {
		t.Fatal(err)
	}
	jobs, _ = s.MediaJobs("alice", true)
	data, err := os.ReadFile(filepath.Join(dir, jobs[0].Path))
	if err != nil || string(data) != "new" {
		t.Fatal(string(data), err)
	}
}

func TestStagedCheckpointRecoversWhenPublicationFails(t *testing.T) {
	dir := t.TempDir()
	s, err := OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	post := Post{ID: "100", AuthorID: "42", Username: "alice", Assets: []Asset{{ID: "video", Kind: "video", URL: "https://video.twimg.com/fixture.mp4"}}}
	if err = s.SavePosts("alice", []Post{post}); err != nil {
		t.Fatal(err)
	}
	jobs, err := s.MediaJobs("alice", false)
	if err != nil {
		t.Fatal(err)
	}
	j := jobs[0]
	calls := 0
	c := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		return mediaResponse(200, "", "", strings.NewReader("abcdef"), 6), nil
	})}
	var destination string
	_, _, _, err = downloadMedia(context.Background(), c, dir, j, 1024, func(path string, size int64, hash string) error {
		if err := s.StageMedia(&j, path, size, hash); err != nil {
			return err
		}
		destination = filepath.Join(dir, path)
		return os.Mkdir(destination, 0700) // Simulate a failure after the DB staging commit.
	})
	if err == nil {
		t.Fatal("expected publication failure")
	}
	if err = os.Remove(destination); err != nil {
		t.Fatal(err)
	}
	if summary, err := downloadPendingWithClient(context.Background(), s, Config{DataDir: dir, MaxMediaBytes: 1024}, "alice", false, c); err != nil || summary.Downloaded != 1 {
		t.Fatal(summary, err)
	}
	if calls != 1 {
		t.Fatal("complete file was re-downloaded", calls)
	}
}
