package web

import (
	"context"
	"reflect"
	"strconv"
	"testing"
	"time"

	"xm/internal/archive"
)

func recordTargets(targets *[]string) collector {
	return func(_ context.Context, _ archive.Config, _ archive.Cookies, target string, _ archive.TargetIdentity, _ int, _ bool, _ func(archive.Page) error) (archive.Collection, error) {
		*targets = append(*targets, target)
		return archive.Collection{Stop: "known_boundary_reached", HistoryComplete: true}, nil
	}
}

func TestPausedQueueAllowsLaterManualWorkWithoutSpinning(t *testing.T) {
	a := testApp(t)
	for _, name := range []string{"alice", "bob", "carol"} {
		seedMonitor(t, a, name)
	}
	var targets []string
	a.scheduler.collect = func(ctx context.Context, cfg archive.Config, c archive.Cookies, target string, id archive.TargetIdentity, n int, replies bool, save func(archive.Page) error) (archive.Collection, error) {
		targets = append(targets, target)
		if target == "alice" {
			cfg.PollingPaused = true
			if err := a.saveConfig(cfg); err != nil {
				t.Fatal(err)
			}
			if err := a.scheduler.queue("carol"); err != nil {
				t.Fatal(err)
			}
		}
		return archive.Collection{Stop: "known_boundary_reached"}, nil
	}
	schedulerStep(t, a)
	if !reflect.DeepEqual(targets, []string{"alice", "carol"}) {
		t.Fatal("manual task was blocked by paused automatic work", targets)
	}
	if pending := state(a.store.DB, "budget_batch_pending"); pending != `["bob"]` {
		t.Fatal("paused work was lost", pending)
	}
	for range 3 {
		if worked, err := a.scheduler.step(context.Background()); worked || err != nil {
			t.Fatal("idle paused queue would spin", worked, err)
		}
	}
	// A fresh manual task must also pass the automatic job left at the front.
	if err := a.scheduler.queue("alice"); err != nil {
		t.Fatal(err)
	}
	a.scheduler.collect = recordTargets(&targets)
	schedulerStep(t, a)
	if !reflect.DeepEqual(targets, []string{"alice", "carol", "alice"}) {
		t.Fatal(targets)
	}
	cfg := a.Config()
	cfg.PollingPaused = false
	if err := a.saveConfig(cfg); err != nil {
		t.Fatal(err)
	}
	schedulerStep(t, a)
	if !reflect.DeepEqual(targets, []string{"alice", "carol", "alice", "bob"}) {
		t.Fatal("resume replayed completed work", targets)
	}
}

func TestQueuedRescanUsesLatestMode(t *testing.T) {
	a := testApp(t)
	seedMonitor(t, a, "alice")
	seedMonitor(t, a, "bob")
	if _, err := a.store.DB.Exec("UPDATE monitors SET baseline_status='visible_history_complete'"); err != nil {
		t.Fatal(err)
	}
	var got string
	a.scheduler.collect = func(_ context.Context, _ archive.Config, _ archive.Cookies, target string, id archive.TargetIdentity, _ int, _ bool, _ func(archive.Page) error) (archive.Collection, error) {
		if target == "alice" {
			if err := a.scheduler.queue("bob", collectionModeRescan); err != nil {
				t.Fatal(err)
			}
		} else {
			got = id.CollectionMode
		}
		return archive.Collection{Stop: "known_boundary_reached", HistoryComplete: true}, nil
	}
	schedulerStep(t, a)
	if got != collectionModeRescan {
		t.Fatal("queued mode update ignored", got)
	}
}

func TestCancelledQueuedJobStaysCancelledAfterRetry(t *testing.T) {
	for _, kind := range []string{"rate", "timeline"} {
		t.Run(kind, func(t *testing.T) {
			a := testApp(t)
			b := login(t, a)
			seedMonitor(t, a, "alice")
			seedMonitor(t, a, "bob")
			var targets []string
			first := true
			a.scheduler.collect = func(_ context.Context, _ archive.Config, _ archive.Cookies, target string, _ archive.TargetIdentity, _ int, _ bool, _ func(archive.Page) error) (archive.Collection, error) {
				targets = append(targets, target)
				if first {
					first = false
					var id int64
					if err := a.store.DB.QueryRow("SELECT id FROM collection_jobs WHERE target='bob' AND finished_at=0").Scan(&id); err != nil {
						t.Fatal(err)
					}
					ok(t, b.request("DELETE", "/api/activity/jobs/"+strconv.FormatInt(id, 10), nil, "http://localhost", true), 200)
					return archive.Collection{}, &archive.Failure{Kind: kind, Message: "fixture failure"}
				}
				return archive.Collection{Stop: "known_boundary_reached"}, nil
			}
			schedulerStep(t, a)
			if pending := state(a.store.DB, "budget_batch_pending"); pending != `["alice"]` {
				t.Fatal("cancelled target reintroduced", pending)
			}
			if err := setState(a.store.DB, "cooldown_until", "0"); err != nil {
				t.Fatal(err)
			}
			if _, err := a.store.DB.Exec("UPDATE collection_jobs SET retry_at=0"); err != nil {
				t.Fatal(err)
			}
			schedulerStep(t, a)
			if !reflect.DeepEqual(targets, []string{"alice", "alice"}) {
				t.Fatal("cancelled target collected", targets)
			}
			var count int
			if err := a.store.DB.QueryRow("SELECT COUNT(*) FROM collection_jobs WHERE target='bob'").Scan(&count); err != nil || count != 1 {
				t.Fatal("cancelled task recreated", count, err)
			}
		})
	}
}

func TestDisabledMonitorManualTaskRetriesWhilePollingPaused(t *testing.T) {
	for _, kind := range []string{"rate", "timeline"} {
		t.Run(kind, func(t *testing.T) {
			a := testApp(t)
			seedMonitor(t, a, "alice")
			if _, err := a.store.DB.Exec("UPDATE monitors SET enabled=0"); err != nil {
				t.Fatal(err)
			}
			cfg := a.Config()
			cfg.PollingPaused = true
			if err := a.saveConfig(cfg); err != nil {
				t.Fatal(err)
			}
			if err := a.scheduler.queue("alice"); err != nil {
				t.Fatal(err)
			}
			calls := 0
			a.scheduler.collect = func(context.Context, archive.Config, archive.Cookies, string, archive.TargetIdentity, int, bool, func(archive.Page) error) (archive.Collection, error) {
				calls++
				if calls == 1 {
					return archive.Collection{}, &archive.Failure{Kind: kind, Message: "fixture failure"}
				}
				return archive.Collection{Stop: "known_boundary_reached"}, nil
			}
			schedulerStep(t, a)
			if worked, err := a.scheduler.step(context.Background()); worked || err != nil {
				t.Fatal("retry ignored waiting period", worked, err)
			}
			if err := setState(a.store.DB, "cooldown_until", "0"); err != nil {
				t.Fatal(err)
			}
			if _, err := a.store.DB.Exec("UPDATE collection_jobs SET retry_at=0"); err != nil {
				t.Fatal(err)
			}
			schedulerStep(t, a)
			if calls != 2 {
				t.Fatal("manual retry lost", calls)
			}
			var enabled bool
			if err := a.store.DB.QueryRow("SELECT enabled FROM monitors WHERE username='alice'").Scan(&enabled); err != nil || enabled {
				t.Fatal("retry changed polling preference", enabled, err)
			}
			if worked, err := a.scheduler.step(context.Background()); worked || err != nil {
				t.Fatal("completed task left an active queue", worked, err)
			}
		})
	}
}

func TestTaskDeadlineCoversMediaAndReleasesNextTarget(t *testing.T) {
	a := testApp(t)
	seedMonitor(t, a, "alice")
	seedMonitor(t, a, "bob")
	cfg := a.Config()
	cfg.CollectionTimeout = 1
	if err := a.saveConfig(cfg); err != nil {
		t.Fatal(err)
	}
	if err := a.store.SavePosts("alice", []archive.Post{{ID: "100", AuthorID: "42", Username: "alice", Assets: []archive.Asset{{ID: "photo", Kind: "photo", URL: "https://pbs.twimg.com/fixture.jpg"}}}}); err != nil {
		t.Fatal(err)
	}
	var targets []string
	var deadline time.Time
	a.scheduler.collect = func(ctx context.Context, _ archive.Config, _ archive.Cookies, target string, _ archive.TargetIdentity, _ int, _ bool, _ func(archive.Page) error) (archive.Collection, error) {
		targets = append(targets, target)
		if target == "alice" {
			deadline, _ = ctx.Deadline()
		}
		return archive.Collection{Stop: "known_boundary_reached", HistoryComplete: true}, nil
	}
	a.scheduler.download = func(ctx context.Context, _ *archive.Store, _ archive.Config, target string, _ bool) (archive.DownloadSummary, error) {
		if target == "alice" {
			mediaDeadline, ok := ctx.Deadline()
			if !ok || !mediaDeadline.Equal(deadline) {
				t.Fatal("media did not share task deadline", mediaDeadline, deadline)
			}
			<-ctx.Done()
			return archive.DownloadSummary{}, ctx.Err()
		}
		return archive.DownloadSummary{}, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if worked, err := a.scheduler.step(ctx); !worked || err != nil {
		t.Fatal(worked, err)
	}
	if !reflect.DeepEqual(targets, []string{"alice", "bob"}) {
		t.Fatal("media blocked next task", targets)
	}
	var status string
	var lastSuccess int64
	if err := a.store.DB.QueryRow("SELECT status FROM runs WHERE target='alice'").Scan(&status); err != nil || status != "incomplete" {
		t.Fatal("budget treated as cancellation or failure", status, err)
	}
	if err := a.store.DB.QueryRow("SELECT last_success FROM monitors WHERE username='alice'").Scan(&lastSuccess); err != nil || lastSuccess != 0 {
		t.Fatal("incomplete media marked successful", lastSuccess, err)
	}
	if remaining, err := a.store.OutstandingMedia("alice"); err != nil || remaining != 1 {
		t.Fatal("media job lost", remaining, err)
	}
}

func TestTaskDeadlineDuringCollectionPreservesCheckpoint(t *testing.T) {
	a := testApp(t)
	seedMonitor(t, a, "alice")
	cfg := a.Config()
	cfg.CollectionTimeout = 1
	if err := a.saveConfig(cfg); err != nil {
		t.Fatal(err)
	}
	a.scheduler.collect = func(ctx context.Context, _ archive.Config, _ archive.Cookies, _ string, _ archive.TargetIdentity, _ int, _ bool, save func(archive.Page) error) (archive.Collection, error) {
		if err := save(archive.Page{Profile: &archive.Profile{ID: "42", Username: "alice"}, Progress: &archive.CollectionProgress{UserID: "42", Posts: archive.TimelineProgress{Cursor: "fixture-next"}}}); err != nil {
			return archive.Collection{}, err
		}
		<-ctx.Done()
		return archive.Collection{}, ctx.Err()
	}
	schedulerStep(t, a)
	list, err := monitors(a.store.DB)
	if err != nil || list[0].LastStatus != "incomplete" || !list[0].Catchup || list[0].Failures != 0 {
		t.Fatal(list, err)
	}
	progress, err := a.store.LoadCollectionProgress("alice", "42")
	if err != nil || progress.Posts.Cursor != "fixture-next" {
		t.Fatal("deadline lost checkpoint", progress, err)
	}
}

func TestExplicitStopWinsOverLateManualRetry(t *testing.T) {
	for _, action := range []string{"stop", "disable"} {
		t.Run(action, func(t *testing.T) {
			a := testApp(t)
			b := login(t, a)
			seedMonitor(t, a, "alice")
			if err := a.scheduler.queue("alice"); err != nil {
				t.Fatal(err)
			}
			var id int64
			if err := a.store.DB.QueryRow("SELECT id FROM collection_jobs WHERE target='alice' AND finished_at=0").Scan(&id); err != nil {
				t.Fatal(err)
			}
			// The run has finished, but its queue slot has not yet recorded the retry.
			if _, err := a.store.DB.Exec("UPDATE collection_jobs SET status='running' WHERE id=?", id); err != nil {
				t.Fatal(err)
			}
			if _, err := a.store.DB.Exec("UPDATE monitors SET running=0,requested=0 WHERE username='alice'"); err != nil {
				t.Fatal(err)
			}
			if action == "stop" {
				ok(t, b.request("POST", "/api/monitors/alice/stop", nil, "http://localhost", true), 200)
			} else {
				ok(t, b.request("PUT", "/api/monitors/alice", map[string]any{"enabled": false, "replies": false}, "http://localhost", true), 200)
			}
			if err := finishCollectionJob(a.store.DB, id, 0, "retrying", "late failure", true); err != nil {
				t.Fatal(err)
			}
			job, err := scanCollectionJob(a.store.DB.QueryRow("SELECT "+collectionJobColumns+" FROM collection_jobs WHERE id=?", id))
			if err != nil || job.Status != "cancelled" || job.FinishedAt == 0 {
				t.Fatal("explicit stop revived by retry", job, err)
			}
			if worked, err := a.scheduler.step(context.Background()); worked || err != nil {
				t.Fatal("stopped job ran", worked, err)
			}
		})
	}
}
