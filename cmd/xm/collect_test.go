package main

import (
	"context"
	"errors"
	"testing"
	"time"

	"xm/internal/archive"
)

func cliTestServices() collectServices {
	return collectServices{
		collect: func(context.Context, archive.Config, archive.Cookies, string, archive.TargetIdentity, int, bool, func(archive.Page) error) (archive.Collection, error) {
			return archive.Collection{Stop: "end_reached"}, nil
		},
		download: func(context.Context, *archive.Store, archive.Config, string, bool) (archive.DownloadSummary, error) {
			return archive.DownloadSummary{}, nil
		},
		avatar: func(context.Context, archive.Config, archive.Profile) error { return nil },
	}
}

func assertCLIRun(t *testing.T, s *archive.Store, status string) {
	t.Helper()
	var got string
	if err := s.DB.QueryRow("SELECT status FROM runs ORDER BY id DESC LIMIT 1").Scan(&got); err != nil || got != status {
		t.Fatalf("run status=%s want=%s err=%v", got, status, err)
	}
}

func TestCLICollectionReportsDeferredAndPermanentMedia(t *testing.T) {
	for _, status := range []string{"failed", "permanent", "ignored", "none"} {
		t.Run(status, func(t *testing.T) {
			s, err := archive.OpenStore(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			if status != "none" {
				if err = s.SavePosts("alice", []archive.Post{{ID: "100", AuthorID: "42", Username: "alice", Assets: []archive.Asset{{ID: "v", Kind: "video", URL: "https://video.twimg.com/test.mp4"}}}}); err != nil {
					t.Fatal(err)
				}
				if _, err = s.DB.Exec("UPDATE media SET status=?,next_attempt=?", status, time.Now().Add(time.Hour).Unix()); err != nil {
					t.Fatal(err)
				}
			}
			out, err := collectTarget(context.Background(), s, archive.Config{BrowserMaxAge: 10}, archive.Cookies{}, "alice", 100, false, false, cliTestServices())
			want := "media_failed"
			if status == "ignored" || status == "none" {
				want = "bounded_complete"
			}
			if out.Status != want || (err != nil) != (want == "media_failed") {
				t.Fatal(out, err)
			}
			assertCLIRun(t, s, want)
		})
	}
}

func TestCLICollectionSharesDeadlineWithMediaAndAvatar(t *testing.T) {
	s, err := archive.OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	services := cliTestServices()
	mediaDeadline := make(chan time.Time, 1)
	var collectDeadline, avatarDeadline time.Time
	services.download = func(ctx context.Context, _ *archive.Store, _ archive.Config, _ string, _ bool) (archive.DownloadSummary, error) {
		d, ok := ctx.Deadline()
		if !ok {
			t.Error("media missing task deadline")
		}
		mediaDeadline <- d
		<-ctx.Done()
		return archive.DownloadSummary{}, ctx.Err()
	}
	services.collect = func(ctx context.Context, _ archive.Config, _ archive.Cookies, _ string, _ archive.TargetIdentity, _ int, _ bool, save func(archive.Page) error) (archive.Collection, error) {
		collectDeadline, _ = ctx.Deadline()
		return archive.Collection{}, save(archive.Page{Profile: &archive.Profile{ID: "42", Username: "alice", AvatarURL: "https://pbs.twimg.com/avatar.jpg"}})
	}
	services.avatar = func(ctx context.Context, _ archive.Config, _ archive.Profile) error {
		avatarDeadline, _ = ctx.Deadline()
		return nil
	}
	started := time.Now()
	out, err := collectTarget(context.Background(), s, archive.Config{BrowserMaxAge: 1}, archive.Cookies{}, "alice", 100, false, false, services)
	if !errors.Is(err, context.DeadlineExceeded) || out.Status != "incomplete" || time.Since(started) > 3*time.Second {
		t.Fatal(out, err, time.Since(started))
	}
	if d := <-mediaDeadline; !d.Equal(collectDeadline) || !d.Equal(avatarDeadline) {
		t.Fatal("different task deadlines", d, collectDeadline, avatarDeadline)
	}
	assertCLIRun(t, s, "incomplete")
}

func TestCLICollectionCancelledInMediaPhase(t *testing.T) {
	s, err := archive.OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	services := cliTestServices()
	services.download = func(ctx context.Context, _ *archive.Store, _ archive.Config, _ string, _ bool) (archive.DownloadSummary, error) {
		cancel()
		return archive.DownloadSummary{}, ctx.Err()
	}
	out, err := collectTarget(ctx, s, archive.Config{BrowserMaxAge: 10}, archive.Cookies{}, "alice", 100, false, false, services)
	if !errors.Is(err, context.Canceled) || out.Status != "cancelled" {
		t.Fatal(out, err)
	}
	assertCLIRun(t, s, "cancelled")
}

func TestCLICollectionRecordsMediaErrorAndRecoveredFailure(t *testing.T) {
	for _, recovered := range []bool{false, true} {
		s, err := archive.OpenStore(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		services := cliTestServices()
		services.download = func(context.Context, *archive.Store, archive.Config, string, bool) (archive.DownloadSummary, error) {
			if recovered {
				return archive.DownloadSummary{Failed: 1}, &archive.MediaDownloadError{Count: 1}
			}
			return archive.DownloadSummary{}, errors.New("queue read failed")
		}
		out, err := collectTarget(context.Background(), s, archive.Config{BrowserMaxAge: 10}, archive.Cookies{}, "alice", 100, false, false, services)
		if recovered {
			if err != nil || out.Status != "bounded_complete" {
				t.Fatal(out, err)
			}
		} else if err == nil || out.Status != "media_failed" {
			t.Fatal(out, err)
		}
		assertCLIRun(t, s, out.Status)
		s.Close()
	}
}
