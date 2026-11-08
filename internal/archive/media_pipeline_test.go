package archive

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDownloadDiscardsSupersededResults(t *testing.T) {
	for _, change := range []string{"url", "ignore", "ignore_then_restore"} {
		for _, fail := range []bool{false, true} {
			t.Run(change+map[bool]string{true: "_failure", false: "_success"}[fail], func(t *testing.T) {
				dir := t.TempDir()
				s, err := OpenStore(dir)
				if err != nil {
					t.Fatal(err)
				}
				defer s.Close()
				post := Post{ID: "100", AuthorID: "42", Username: "alice", Assets: []Asset{{ID: "photo", Kind: "photo", URL: "https://pbs.twimg.com/old.jpg"}}}
				if err := s.SavePosts("alice", []Post{post}); err != nil {
					t.Fatal(err)
				}
				client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
					var err error
					if change == "url" {
						post.Assets[0].URL = "https://pbs.twimg.com/new.jpg"
						post.Raw = json.RawMessage(`{"url":"https://pbs.twimg.com/new.jpg"}`)
						err = s.SavePosts("alice", []Post{post})
					} else {
						err = s.SetMediaIgnored("alice", "100", "photo", true)
						if err == nil && change == "ignore_then_restore" {
							err = s.SetMediaIgnored("alice", "100", "photo", false)
						}
					}
					if err != nil {
						t.Error(err)
						return nil, err
					}
					if fail {
						return nil, errors.New("old request failed")
					}
					return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"image/jpeg"}}, Body: io.NopCloser(strings.NewReader("old image"))}, nil
				})}
				cfg := Config{DataDir: dir, MaxMediaBytes: 1024}
				summary, err := downloadPendingWithClient(context.Background(), s, cfg, "alice", false, client)
				if err != nil || summary.Downloaded != 0 || summary.Failed != 0 || len(summary.ChangedPostIDs) != 0 {
					t.Fatal(summary, err)
				}
				var status, source, path, message string
				var attempts, next int64
				if err := s.DB.QueryRow("SELECT status,source_url,local_path,last_error,attempts,next_attempt FROM media").Scan(&status, &source, &path, &message, &attempts, &next); err != nil {
					t.Fatal(err)
				}
				want := "pending"
				if change == "ignore" {
					want = "ignored"
				}
				if status != want || path != "" || message != "" || attempts != 0 || next != 0 || source != post.Assets[0].URL {
					t.Fatalf("old result overwrote new state: %s %s %s %s %d %d", status, source, path, message, attempts, next)
				}
				if change == "url" {
					client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
						if r.URL.String() != post.Assets[0].URL {
							t.Error("downloaded stale URL", r.URL)
						}
						return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"image/jpeg"}}, Body: io.NopCloser(strings.NewReader("new image"))}, nil
					})
					summary, err = downloadPendingWithClient(context.Background(), s, cfg, "alice", false, client)
					if err != nil || summary.Downloaded != 1 {
						t.Fatal("refreshed URL not downloaded", summary, err)
					}
				}
			})
		}
	}
}

func TestMediaPipelineCancellationPreservesQueueAndRetry(t *testing.T) {
	dir := t.TempDir()
	s, err := OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { s.Close() }()
	posts := []Post{}
	for _, id := range []string{"100", "200", "300", "400"} {
		posts = append(posts, Post{ID: id, AuthorID: "42", Username: "alice", Assets: []Asset{{ID: "photo", Kind: "photo", URL: "https://pbs.twimg.com/" + id + ".jpg"}}})
	}
	if err = s.SavePosts("alice", posts); err != nil {
		t.Fatal(err)
	}
	future := time.Now().Add(time.Hour).Unix()
	if _, err = s.DB.Exec("UPDATE media SET status='failed',attempts=2,next_attempt=? WHERE post_id='200'", future); err != nil {
		t.Fatal(err)
	}
	if _, err = s.DB.Exec("UPDATE media SET status='permanent' WHERE post_id='300'"); err != nil {
		t.Fatal(err)
	}
	if err = s.SetMediaIgnored("alice", "400", "photo", true); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	started := make(chan struct{})
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		// Simulate a large response that stalls after some bytes, then gets cancelled.
		reader, writer := io.Pipe()
		go func() {
			_, _ = writer.Write([]byte("partial video bytes"))
			close(started)
			<-r.Context().Done()
			_ = writer.CloseWithError(r.Context().Err())
		}()
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"image/jpeg"}}, Body: reader}, nil
	})}
	cfg := Config{DataDir: dir, MaxMediaBytes: 1024}
	p := StartMediaPipeline(ctx, func(ctx context.Context) (DownloadSummary, error) {
		return downloadPendingWithClient(ctx, s, cfg, "alice", false, client)
	})
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		cancel()
		t.Fatal("download did not start")
	}
	cancel()
	summary, err := p.Finish()
	if !errors.Is(err, context.Canceled) || summary.Failed != 0 {
		t.Fatal(summary, err)
	}
	partials, err := filepath.Glob(filepath.Join(dir, "media", "users", "*", ".download-*"))
	if err != nil || len(partials) != 0 {
		t.Fatal("cancelled response left temporary files", partials, err)
	}
	// Restart loses no pending entries and does not bypass deferred/permanent/ignored media.
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	jobs, err := s.MediaJobs("alice", false)
	if err != nil || len(jobs) != 1 || jobs[0].PostID != "100" || jobs[0].Attempts != 0 || jobs[0].Status != "pending" || jobs[0].NextAttempt != 0 {
		t.Fatal(jobs, err)
	}
	requests := 0
	client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		requests++
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"image/jpeg"}}, Body: io.NopCloser(strings.NewReader("image"))}, nil
	})
	p = StartMediaPipeline(context.Background(), func(ctx context.Context) (DownloadSummary, error) {
		return downloadPendingWithClient(ctx, s, cfg, "alice", false, client)
	})
	p.Notify()
	p.Notify()
	summary, err = p.Finish()
	if err != nil || summary.Downloaded != 1 || requests != 1 {
		t.Fatal(summary, requests, err)
	}
	var next int64
	if err = s.DB.QueryRow("SELECT next_attempt FROM media WHERE post_id='200'").Scan(&next); err != nil || next != future {
		t.Fatal(next, err)
	}
}

func TestMediaSnapshotSurvivesUnrelatedPostRefresh(t *testing.T) {
	s, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	post := Post{ID: "100", AuthorID: "42", Username: "alice", Text: "original", Assets: []Asset{{ID: "photo", Kind: "photo", URL: "https://pbs.twimg.com/fixture.jpg"}}}
	if err = s.SavePosts("alice", []Post{post}); err != nil {
		t.Fatal(err)
	}
	jobs, err := s.MediaJobs("alice", false)
	if err != nil || len(jobs) != 1 {
		t.Fatal(jobs, err)
	}
	// Engagement/profile/text updates on repeated pages must not discard an
	// ongoing large download when its media source and state are unchanged.
	post.Text = "edited text"
	if err = s.SavePosts("alice", []Post{post}); err != nil {
		t.Fatal(err)
	}
	if current, err := s.MediaJobCurrent(jobs[0]); err != nil || !current {
		t.Fatal(current, err)
	}
	if err = s.MediaResult(jobs[0], "media/fixture.jpg", 7, "fixture", nil); err != nil {
		t.Fatal(err)
	}
}
