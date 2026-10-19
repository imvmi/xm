package web

import (
	"context"
	"reflect"
	"strconv"
	"testing"
	"time"

	"xm/internal/archive"
)

func TestCollectionJobsExecuteFIFOAndLinkRuns(t *testing.T) {
	a := testApp(t)
	seedMonitor(t, a, "alice")
	seedMonitor(t, a, "bob")
	cfg := a.Config()
	cfg.PollingPaused = true
	if err := a.saveConfig(cfg); err != nil {
		t.Fatal(err)
	}
	if err := a.scheduler.queue("bob"); err != nil {
		t.Fatal(err)
	}
	if err := a.scheduler.queue("alice"); err != nil {
		t.Fatal(err)
	}
	targets := []string{}
	a.scheduler.collect = func(_ context.Context, _ archive.Config, _ archive.Cookies, target string, _ archive.TargetIdentity, _ int, _ bool, _ func(archive.Page) error) (archive.Collection, error) {
		targets = append(targets, target)
		return archive.Collection{Stop: "known_boundary_reached"}, nil
	}
	worked, err := a.scheduler.step(context.Background())
	if !worked || err != nil {
		t.Fatal(worked, err)
	}
	if !reflect.DeepEqual(targets, []string{"bob", "alice"}) {
		t.Fatal("queue was not FIFO", targets)
	}
	var completed, linked int
	if err = a.store.DB.QueryRow("SELECT COUNT(*) FROM collection_jobs WHERE status='completed' AND finished_at>0").Scan(&completed); err != nil || completed != 2 {
		t.Fatal(completed, err)
	}
	if err = a.store.DB.QueryRow("SELECT COUNT(*) FROM runs WHERE job_id>0").Scan(&linked); err != nil || linked != 2 {
		t.Fatal(linked, err)
	}
	current, queue, err := activityJobs(a.store.DB)
	if err != nil || current != nil || len(queue) != 0 {
		t.Fatal(current, queue, err)
	}
}

func TestWaitingCollectionJobCanBeCancelledWithoutPausingMonitor(t *testing.T) {
	a := testApp(t)
	b := login(t, a)
	seedMonitor(t, a, "alice")
	seedMonitor(t, a, "bob")
	cfg := a.Config()
	cfg.PollingPaused = true
	if err := a.saveConfig(cfg); err != nil {
		t.Fatal(err)
	}
	if err := a.scheduler.queue("alice"); err != nil {
		t.Fatal(err)
	}
	if err := a.scheduler.queue("bob"); err != nil {
		t.Fatal(err)
	}
	var aliceJob int64
	if err := a.store.DB.QueryRow("SELECT id FROM collection_jobs WHERE target='alice' AND finished_at=0").Scan(&aliceJob); err != nil {
		t.Fatal(err)
	}
	ok(t, b.request("DELETE", "/api/activity/jobs/"+strconv.FormatInt(aliceJob, 10), nil, "http://localhost", true), 200)
	var requested, enabled bool
	var status string
	if err := a.store.DB.QueryRow("SELECT requested,enabled FROM monitors WHERE username='alice'").Scan(&requested, &enabled); err != nil {
		t.Fatal(err)
	}
	if err := a.store.DB.QueryRow("SELECT status FROM collection_jobs WHERE id=?", aliceJob).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if requested || !enabled || status != "cancelled" {
		t.Fatalf("cancel changed the wrong state: requested=%t enabled=%t status=%s", requested, enabled, status)
	}
	targets := []string{}
	a.scheduler.collect = func(_ context.Context, _ archive.Config, _ archive.Cookies, target string, _ archive.TargetIdentity, _ int, _ bool, _ func(archive.Page) error) (archive.Collection, error) {
		targets = append(targets, target)
		return archive.Collection{Stop: "known_boundary_reached"}, nil
	}
	if worked, err := a.scheduler.step(context.Background()); !worked || err != nil {
		t.Fatal(worked, err)
	}
	if !reflect.DeepEqual(targets, []string{"bob"}) {
		t.Fatal("cancelled job was executed", targets)
	}
	if response := b.request("DELETE", "/api/activity/jobs/"+strconv.FormatInt(aliceJob, 10), nil, "http://localhost", true); response.Code != 404 {
		t.Fatal("finished cancellation was accepted", response.Code, response.Body.String())
	}
}

func TestQueueIntervalWaitsAndRespondsToSettingsChange(t *testing.T) {
	a := testApp(t)
	seedMonitor(t, a, "alice")
	seedMonitor(t, a, "bob")
	cfg := a.Config()
	cfg.PollingPaused = true
	cfg.QueueInterval = 60
	if err := a.saveConfig(cfg); err != nil {
		t.Fatal(err)
	}
	if err := a.scheduler.queue("alice"); err != nil {
		t.Fatal(err)
	}
	if err := a.scheduler.queue("bob"); err != nil {
		t.Fatal(err)
	}
	started := make(chan string, 2)
	a.scheduler.collect = func(_ context.Context, _ archive.Config, _ archive.Cookies, target string, _ archive.TargetIdentity, _ int, _ bool, _ func(archive.Page) error) (archive.Collection, error) {
		started <- target
		return archive.Collection{Stop: "known_boundary_reached"}, nil
	}
	done := make(chan error, 1)
	go func() {
		_, err := a.scheduler.step(context.Background())
		done <- err
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("first task did not start")
	}
	deadline := time.Now().Add(time.Second)
	for a.scheduler.WaitingUntil() <= time.Now().Unix() && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if a.scheduler.WaitingUntil() <= time.Now().Unix() {
		t.Fatal("scheduler did not enter the configured queue interval")
	}
	select {
	case target := <-started:
		t.Fatal("next task bypassed queue interval", target)
	case <-time.After(50 * time.Millisecond):
	}
	cfg = a.Config()
	cfg.QueueInterval = 0
	if err := a.saveConfig(cfg); err != nil {
		t.Fatal(err)
	}
	a.scheduler.Wake()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("disabling queue interval did not release the next task")
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestCollectionJobWaitingStateAndManualUpgrade(t *testing.T) {
	a := testApp(t)
	seedMonitor(t, a, "alice")
	job, err := ensureCollectionJob(a.store.DB, "alice", "automatic")
	if err != nil {
		t.Fatal(err)
	}
	if err = finishCollectionJob(a.store.DB, job.ID, 0, "waiting_rate", "X 请求限流冷却中", true); err != nil {
		t.Fatal(err)
	}
	if err = a.scheduler.queue("alice"); err != nil {
		t.Fatal(err)
	}
	current, queue, err := activityJobs(a.store.DB)
	if err != nil || current != nil || len(queue) != 1 {
		t.Fatal(current, queue, err)
	}
	if queue[0].Source != "manual" || queue[0].Status != "waiting_rate" || queue[0].Position != 1 {
		t.Fatalf("manual request did not consume existing queue slot: %+v", queue[0])
	}
}

func TestTimelineRetryBackoffAndQueueFairness(t *testing.T) {
	for _, tc := range []struct {
		previous, count int
		delay           time.Duration
	}{{0, 1, 5 * time.Second}, {1, 2, 15 * time.Second}, {2, 3, 45 * time.Second}, {3, 3, 45 * time.Second}} {
		count, delay := timelineRetryBackoff(tc.previous)
		if count != tc.count || delay != tc.delay {
			t.Fatalf("previous=%d: got count=%d delay=%s", tc.previous, count, delay)
		}
	}

	a := testApp(t)
	seedMonitor(t, a, "alice")
	seedMonitor(t, a, "bob")
	cfg := a.Config()
	cfg.PollingPaused = true
	cfg.QueueInterval = 0
	if err := a.saveConfig(cfg); err != nil {
		t.Fatal(err)
	}
	if err := a.scheduler.queue("alice"); err != nil {
		t.Fatal(err)
	}
	if err := a.scheduler.queue("bob"); err != nil {
		t.Fatal(err)
	}
	var targets []string
	a.scheduler.collect = func(_ context.Context, _ archive.Config, _ archive.Cookies, target string, _ archive.TargetIdentity, _ int, _ bool, save func(archive.Page) error) (archive.Collection, error) {
		targets = append(targets, target)
		if target == "alice" {
			progress := archive.CollectionProgress{UserID: "42", Posts: archive.TimelineProgress{Cursor: "page-1"}}
			if err := save(archive.Page{Progress: &progress}); err != nil {
				return archive.Collection{}, err
			}
			return archive.Collection{}, &archive.Failure{Kind: "timeline", Message: "分页网络异常"}
		}
		return archive.Collection{Stop: "known_boundary_reached"}, nil
	}
	if worked, err := a.scheduler.step(context.Background()); !worked || err != nil {
		t.Fatal(worked, err)
	}
	if !reflect.DeepEqual(targets, []string{"alice"}) {
		t.Fatal("failure did not yield the queue", targets)
	}
	var retryAt int64
	var retryCount int
	var status string
	if err := a.store.DB.QueryRow("SELECT status,retry_at,retry_count FROM collection_jobs WHERE target='alice' AND finished_at=0").Scan(&status, &retryAt, &retryCount); err != nil {
		t.Fatal(err)
	}
	if status != "retrying" || retryCount != 1 || retryAt < time.Now().Unix()+4 || retryAt > time.Now().Unix()+6 {
		t.Fatalf("retry was not durably delayed: status=%s at=%d count=%d", status, retryAt, retryCount)
	}
	if worked, err := a.scheduler.step(context.Background()); !worked || err != nil {
		t.Fatal(worked, err)
	}
	if !reflect.DeepEqual(targets, []string{"alice", "bob"}) {
		t.Fatal("waiting retry blocked the next target", targets)
	}
	progress, err := a.store.LoadCollectionProgress("alice", "42")
	if err != nil || progress.Posts.Cursor != "page-1" {
		t.Fatal("retry lost its durable cursor", progress, err)
	}
}

func TestCollectionModesAndBaselineLifecycle(t *testing.T) {
	a := testApp(t)
	seedMonitor(t, a, "alice")
	if err := a.scheduler.queue("alice", collectionModeIncremental); err != nil {
		t.Fatal(err)
	}
	var mode string
	if err := a.store.DB.QueryRow("SELECT mode FROM collection_jobs WHERE target='alice' AND finished_at=0").Scan(&mode); err != nil || mode != collectionModeBackfill {
		t.Fatal("first collection was not promoted to backfill", mode, err)
	}
	a.scheduler.collect = func(_ context.Context, _ archive.Config, _ archive.Cookies, _ string, identity archive.TargetIdentity, _ int, _ bool, _ func(archive.Page) error) (archive.Collection, error) {
		if identity.CollectionMode != collectionModeBackfill {
			t.Fatal("collector did not receive backfill mode", identity.CollectionMode)
		}
		return archive.Collection{Stop: "known_boundary_reached", HistoryComplete: true, PostsHistoryComplete: true}, nil
	}
	if worked, err := a.scheduler.step(context.Background()); !worked || err != nil {
		t.Fatal(worked, err)
	}
	list, err := monitors(a.store.DB)
	if err != nil || list[0].BaselineStatus != "visible_history_complete" || list[0].PostsBaseline != "visible_history_complete" || list[0].BaselineFinished == 0 {
		t.Fatal("completed baseline was not persisted", list, err)
	}
	if _, err = a.store.DB.Exec("UPDATE monitors SET next_run=0"); err != nil {
		t.Fatal(err)
	}
	a.scheduler.collect = func(_ context.Context, _ archive.Config, _ archive.Cookies, _ string, identity archive.TargetIdentity, _ int, _ bool, _ func(archive.Page) error) (archive.Collection, error) {
		if identity.CollectionMode != collectionModeIncremental {
			t.Fatal("completed baseline did not switch to incremental", identity.CollectionMode)
		}
		return archive.Collection{Stop: "known_boundary_reached"}, nil
	}
	if worked, err := a.scheduler.step(context.Background()); !worked || err != nil {
		t.Fatal(worked, err)
	}
}

func TestRescanClearsCheckpointWithoutDeletingArchive(t *testing.T) {
	a := testApp(t)
	seedMonitor(t, a, "alice")
	progress := archive.CollectionProgress{UserID: "42", Posts: archive.TimelineProgress{Cursor: "old-page"}}
	if err := a.store.SaveCollectionPage("alice", 0, archive.Page{Posts: []archive.Post{{ID: "100", AuthorID: "42", Username: "alice"}}, Progress: &progress}); err != nil {
		t.Fatal(err)
	}
	if _, err := a.store.DB.Exec("UPDATE monitors SET baseline_status='visible_history_complete',posts_baseline_status='visible_history_complete',catchup=1,catchup_post_anchor='100' WHERE username='alice'"); err != nil {
		t.Fatal(err)
	}
	if err := a.scheduler.queue("alice", collectionModeRescan); err != nil {
		t.Fatal(err)
	}
	var mode, baseline, anchor string
	var posts int
	if err := a.store.DB.QueryRow("SELECT mode FROM collection_jobs WHERE target='alice' AND finished_at=0").Scan(&mode); err != nil {
		t.Fatal(err)
	}
	if err := a.store.DB.QueryRow("SELECT baseline_status,catchup_post_anchor,(SELECT COUNT(*) FROM target_posts WHERE target='alice') FROM monitors WHERE username='alice'").Scan(&baseline, &anchor, &posts); err != nil {
		t.Fatal(err)
	}
	var checkpoints int
	if err := a.store.DB.QueryRow("SELECT COUNT(*) FROM collection_progress WHERE target='alice'").Scan(&checkpoints); err != nil {
		t.Fatal(err)
	}
	if mode != collectionModeRescan || baseline != "not_started" || anchor != "" || checkpoints != 0 || posts != 1 {
		t.Fatalf("rescan reset the wrong state: mode=%s baseline=%s anchor=%q checkpoints=%d posts=%d", mode, baseline, anchor, checkpoints, posts)
	}
}
