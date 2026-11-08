package web

import (
	"context"
	"fmt"
	"testing"
	"time"

	"xm/internal/archive"
)

func TestSchedulerDownloadsWhilePagesContinue(t *testing.T) {
	for _, failed := range []bool{false, true} {
		t.Run(fmt.Sprintf("collection_failed_%t", failed), func(t *testing.T) { testStreamingMedia(t, failed) })
	}
}

func testStreamingMedia(t *testing.T, collectionFailed bool) {
	a := testApp(t)
	seedMonitor(t, a, "alice")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	started := make(chan struct{})
	release := make(chan struct{})
	a.scheduler.download = func(ctx context.Context, s *archive.Store, _ archive.Config, target string, _ bool) (archive.DownloadSummary, error) {
		jobs, err := s.MediaJobs(target, false)
		if err != nil {
			return archive.DownloadSummary{}, err
		}
		var summary archive.DownloadSummary
		for _, j := range jobs {
			if j.PostID == "100" {
				close(started)
				select {
				case <-release:
				case <-ctx.Done():
					return summary, ctx.Err()
				}
			}
			if err := s.MediaResult(j, "media/fixture.jpg", 1, "fixture", nil); err != nil {
				return summary, err
			}
			summary.Downloaded++
			summary.ChangedPostIDs = append(summary.ChangedPostIDs, j.PostID)
		}
		return summary, nil
	}
	a.scheduler.collect = func(ctx context.Context, _ archive.Config, _ archive.Cookies, _ string, _ archive.TargetIdentity, _ int, _ bool, save func(archive.Page) error) (archive.Collection, error) {
		page := func(id string) archive.Page {
			return archive.Page{Posts: []archive.Post{{ID: id, AuthorID: "42", Username: "alice", Assets: []archive.Asset{{ID: "photo", Kind: "photo", URL: "https://pbs.twimg.com/fixture.jpg"}}}}}
		}
		if err := save(page("100")); err != nil {
			return archive.Collection{}, err
		}
		select {
		case <-started:
		case <-ctx.Done():
			return archive.Collection{}, fmt.Errorf("first-page download did not start during collection: %w", ctx.Err())
		}
		// These commits must finish while the first large download is still blocked.
		for _, id := range []string{"200", "300"} {
			if err := save(page(id)); err != nil {
				return archive.Collection{}, err
			}
		}
		close(release)
		if collectionFailed {
			return archive.Collection{Posts: 3}, &archive.Failure{Kind: "access", Message: "fixture access failure after saved pages"}
		}
		return archive.Collection{Posts: 3, Stop: "known_boundary_reached", HistoryComplete: true}, nil
	}
	if worked, err := a.scheduler.step(ctx); !worked || err != nil {
		t.Fatal(worked, err)
	}
	done, total, err := a.store.TargetMediaCounts("alice")
	if err != nil || done != 3 || total != 3 {
		t.Fatal("final pages not drained", done, total, err)
	}
	var status string
	want := "bounded_complete"
	if collectionFailed {
		want = "failed"
	}
	if err := a.store.DB.QueryRow("SELECT status FROM runs ORDER BY id DESC LIMIT 1").Scan(&status); err != nil || status != want {
		t.Fatal(status, err)
	}
}
