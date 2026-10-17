package web

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"xm/internal/archive"
)

type fakeBatchSession struct {
	targets *[]string
	closed  *bool
}

func (s *fakeBatchSession) CollectBound(_ context.Context, target string, _ archive.TargetIdentity, _ int, _ bool, _ func(archive.Page) error) (archive.Collection, error) {
	*s.targets = append(*s.targets, target)
	return archive.Collection{Stop: "known_boundary_reached"}, nil
}
func (s *fakeBatchSession) Close() { *s.closed = true }

func seedMonitor(t *testing.T, a *App, name string) {
	t.Helper()
	if err := a.store.SaveCookies(archive.Cookies{AuthToken: "synthetic", CSRF: "synthetic"}); err != nil {
		t.Fatal(err)
	}
	_, err := a.store.DB.Exec("INSERT INTO monitors(username,enabled,next_run) VALUES(?,1,0)", name)
	if err != nil {
		t.Fatal(err)
	}
}
func TestSchedulerSuccessDedupAndPause(t *testing.T) {
	a := testApp(t)
	seedMonitor(t, a, "alice")
	calls := 0
	a.scheduler.collect = func(ctx context.Context, cfg archive.Config, c archive.Cookies, name string, identity archive.TargetIdentity, n int, replies bool, save func(archive.Page) error) (archive.Collection, error) {
		calls++
		err := save(archive.Page{Posts: []archive.Post{{ID: "100", AuthorID: "42", Username: name, Text: "saved", Raw: json.RawMessage(`{}`)}}})
		return archive.Collection{Posts: 1, Stop: "known_boundary_reached"}, err
	}
	worked, err := a.scheduler.step(context.Background())
	if !worked || err != nil {
		t.Fatal(worked, err)
	}
	m, _ := monitors(a.store.DB)
	if m[0].NewPosts != 1 || m[0].Running || m[0].NextRun < time.Now().Unix()+50 || m[0].LastSuccess == 0 {
		t.Fatal("incorrect completion", m)
	}
	worked, err = a.scheduler.step(context.Background())
	if worked || err != nil || calls != 1 {
		t.Fatal("not-due job executed")
	}
	cfg := a.Config()
	cfg.PollingPaused = true
	a.saveConfig(cfg)
	a.store.DB.Exec("UPDATE monitors SET next_run=0")
	worked, _ = a.scheduler.step(context.Background())
	if worked {
		t.Fatal("paused scheduler ran")
	}
	if err = a.scheduler.queue("alice"); err != nil {
		t.Fatal(err)
	}
	worked, err = a.scheduler.step(context.Background())
	if !worked || err != nil {
		t.Fatal("manual collection blocked by auto pause", err)
	}
	m, _ = monitors(a.store.DB)
	if m[0].NewPosts != 0 || m[0].Posts != 1 {
		t.Fatal("duplicate insertion", m)
	}
}

func TestSchedulerDownloadsCollectedProfileAvatar(t *testing.T) {
	a := testApp(t)
	seedMonitor(t, a, "alice")
	const source = "https://pbs.twimg.com/profile_images/42/alice_normal.jpg"
	a.scheduler.collect = func(_ context.Context, _ archive.Config, _ archive.Cookies, _ string, _ archive.TargetIdentity, _ int, _ bool, save func(archive.Page) error) (archive.Collection, error) {
		err := save(archive.Page{Profile: &archive.Profile{ID: "42", Username: "alice", AvatarURL: source}})
		return archive.Collection{}, err
	}
	called := false
	a.scheduler.avatar = func(_ context.Context, _ archive.Config, p archive.Profile) error {
		called = p.ID == "42" && p.AvatarURL == source
		return nil
	}
	worked, err := a.scheduler.step(context.Background())
	if !worked || err != nil || !called {
		t.Fatal("profile avatar was not downloaded", worked, err, called)
	}
}

func TestSchedulerLoadsIncrementalBoundary(t *testing.T) {
	a := testApp(t)
	seedMonitor(t, a, "alice")
	if err := a.store.SavePosts("alice", []archive.Post{{ID: "100", AuthorID: "42", Username: "alice"}}); err != nil {
		t.Fatal(err)
	}
	a.scheduler.collect = func(_ context.Context, _ archive.Config, _ archive.Cookies, _ string, identity archive.TargetIdentity, _ int, _ bool, _ func(archive.Page) error) (archive.Collection, error) {
		if !identity.KnownPostIDs["100"] {
			t.Fatal("scheduler did not load known post boundary")
		}
		return archive.Collection{Stop: "known_boundary_reached"}, nil
	}
	worked, err := a.scheduler.step(context.Background())
	if !worked || err != nil {
		t.Fatal(worked, err)
	}
}

func TestSchedulerUsesFixedSafetyLimitAndMarksIncomplete(t *testing.T) {
	a := testApp(t)
	seedMonitor(t, a, "alice")
	gotLimit := 0
	a.scheduler.collect = func(_ context.Context, _ archive.Config, _ archive.Cookies, _ string, _ archive.TargetIdentity, limit int, _ bool, _ func(archive.Page) error) (archive.Collection, error) {
		gotLimit = limit
		return archive.Collection{Posts: limit, Stop: "limit_reached"}, nil
	}
	worked, err := a.scheduler.step(context.Background())
	if !worked || err != nil {
		t.Fatal(worked, err)
	}
	list, err := monitors(a.store.DB)
	if err != nil || len(list) != 1 {
		t.Fatal(err, list)
	}
	m := list[0]
	if gotLimit != monitorCollectionLimit || m.LastStatus != "incomplete" || m.LastError != incompleteCollectionMessage || m.LastSuccess != 0 || m.Failures != 0 {
		t.Fatalf("safety-limit result was treated as complete: limit=%d monitor=%+v", gotLimit, m)
	}
	var status, message string
	if err = a.store.DB.QueryRow("SELECT status,message FROM runs ORDER BY id DESC LIMIT 1").Scan(&status, &message); err != nil {
		t.Fatal(err)
	}
	if status != "incomplete" || message != incompleteCollectionMessage {
		t.Fatal(status, message)
	}
}

func TestSchedulerPersistsAndClearsCatchupBoundary(t *testing.T) {
	a := testApp(t)
	seedMonitor(t, a, "alice")
	if err := a.store.SavePosts("alice", []archive.Post{{ID: "100", AuthorID: "42", Username: "alice", Raw: json.RawMessage(`{}`)}}); err != nil {
		t.Fatal(err)
	}
	calls := 0
	a.scheduler.collect = func(_ context.Context, _ archive.Config, _ archive.Cookies, _ string, identity archive.TargetIdentity, _ int, _ bool, _ func(archive.Page) error) (archive.Collection, error) {
		calls++
		if calls == 1 {
			if identity.Catchup {
				t.Fatal("first bounded run unexpectedly started in catch-up mode")
			}
			return archive.Collection{Stop: "max_scrolls_reached", Scrolls: 40}, nil
		}
		if !identity.Catchup || identity.CatchupPostAnchor != "100" || identity.CatchupScrollBudget == 0 {
			t.Fatalf("catch-up state not restored: %+v", identity)
		}
		return archive.Collection{Stop: "known_boundary_reached"}, nil
	}
	if worked, err := a.scheduler.step(context.Background()); !worked || err != nil {
		t.Fatal(worked, err)
	}
	list, _ := monitors(a.store.DB)
	if !list[0].Catchup || list[0].LastStatus != "incomplete" || list[0].CatchupPost != "100" {
		t.Fatalf("incomplete boundary not persisted: %+v", list[0])
	}
	if _, err := a.store.DB.Exec("UPDATE monitors SET next_run=0"); err != nil {
		t.Fatal(err)
	}
	if worked, err := a.scheduler.step(context.Background()); !worked || err != nil {
		t.Fatal(worked, err)
	}
	list, _ = monitors(a.store.DB)
	if list[0].Catchup || list[0].CatchupPost != "" || list[0].CatchupScrolls != 0 || list[0].LastStatus != "bounded_complete" {
		t.Fatalf("completed catch-up state not cleared: %+v", list[0])
	}
}

func TestSchedulerFailurePolicies(t *testing.T) {
	for _, kind := range []string{"auth", "access", "rate", "transient"} {
		t.Run(kind, func(t *testing.T) {
			a := testApp(t)
			seedMonitor(t, a, "alice")
			a.scheduler.collect = func(context.Context, archive.Config, archive.Cookies, string, archive.TargetIdentity, int, bool, func(archive.Page) error) (archive.Collection, error) {
				return archive.Collection{}, &archive.Failure{Kind: kind, Message: "test failure"}
			}
			worked, err := a.scheduler.step(context.Background())
			if !worked || err != nil {
				t.Fatal(worked, err)
			}
			m, _ := monitors(a.store.DB)
			if m[0].Running || m[0].Failures != 1 || m[0].LastError == "" {
				t.Fatal("failure state not persisted")
			}
			switch kind {
			case "auth":
				if state(a.store.DB, "cookie_blocked") == "" {
					t.Fatal("invalid cookie not blocked")
				}
			case "access":
				if m[0].Enabled {
					t.Fatal("access denied target still enabled")
				}
			case "rate":
				if state(a.store.DB, "cooldown_until") == "" {
					t.Fatal("missing account cooldown")
				}
			case "transient":
				if !m[0].Enabled {
					t.Fatal("transient failure permanently paused")
				}
			}
		})
	}
}
func TestSchedulerSerialCancelAndCookieReplacement(t *testing.T) {
	a := testApp(t)
	seedMonitor(t, a, "alice")
	seedMonitor(t, a, "bob")
	started := make(chan struct{})
	finish := make(chan struct{})
	done := make(chan error, 1)
	a.scheduler.collect = func(ctx context.Context, _ archive.Config, _ archive.Cookies, _ string, _ archive.TargetIdentity, _ int, _ bool, _ func(archive.Page) error) (archive.Collection, error) {
		close(started)
		select {
		case <-finish:
			return archive.Collection{}, &archive.Failure{Kind: "auth", Message: "old cookie expired"}
		case <-ctx.Done():
			return archive.Collection{}, ctx.Err()
		}
	}
	go func() { _, err := a.scheduler.step(context.Background()); done <- err }()
	<-started
	if worked, err := a.scheduler.step(context.Background()); worked || err != nil {
		t.Fatal("overlapping collection", err)
	}
	// Updating credentials during the old request must not invalidate the new ones.
	a.changeMu.Lock()
	setState(a.store.DB, "cookie_imported", "new-generation")
	a.changeMu.Unlock()
	close(finish)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if state(a.store.DB, "cookie_blocked") != "" {
		t.Fatal("stale failure blocked replacement cookie")
	}
	a.store.DB.Exec("UPDATE monitors SET enabled=0 WHERE username='bob'")
	started = make(chan struct{})
	finish = make(chan struct{})
	done = make(chan error, 1)
	go func() { _, err := a.scheduler.step(context.Background()); done <- err }()
	<-started
	current, _ := a.scheduler.Current()
	a.scheduler.Cancel(current)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	var status string
	a.store.DB.QueryRow("SELECT status FROM runs ORDER BY id DESC LIMIT 1").Scan(&status)
	if status != "cancelled" {
		t.Fatal("cancellation not recorded", status)
	}
}

func TestSchedulerAutomaticBatchReusesOneSession(t *testing.T) {
	a := testApp(t)
	seedMonitor(t, a, "alice")
	seedMonitor(t, a, "bob")
	targets := []string{}
	closed := false
	sessions := 0
	a.scheduler.newSession = func(context.Context, archive.Config, archive.Cookies) (collectionSession, error) {
		sessions++
		return &fakeBatchSession{targets: &targets, closed: &closed}, nil
	}
	worked, err := a.scheduler.step(context.Background())
	if !worked || err != nil {
		t.Fatal(worked, err)
	}
	if sessions != 1 || !closed || len(targets) != 2 || targets[0] != "alice" || targets[1] != "bob" {
		t.Fatalf("batch did not reuse one session: sessions=%d closed=%v targets=%v", sessions, closed, targets)
	}
	list, err := monitors(a.store.DB)
	if err != nil || len(list) != 2 || list[0].NextRun != list[1].NextRun || list[0].Interval != a.Config().DefaultInterval || list[1].Interval != a.Config().DefaultInterval {
		t.Fatalf("batch schedule was not synchronized: %+v err=%v", list, err)
	}
}

func TestSchedulerBatchStopsOnlyForGlobalFailures(t *testing.T) {
	for _, tc := range []struct {
		kind      string
		wantCalls int
	}{
		{kind: "transient", wantCalls: 2},
		{kind: "access", wantCalls: 2},
		{kind: "auth", wantCalls: 1},
		{kind: "rate", wantCalls: 1},
	} {
		t.Run(tc.kind, func(t *testing.T) {
			a := testApp(t)
			seedMonitor(t, a, "alice")
			seedMonitor(t, a, "bob")
			calls := 0
			a.scheduler.collect = func(_ context.Context, _ archive.Config, _ archive.Cookies, target string, _ archive.TargetIdentity, _ int, _ bool, _ func(archive.Page) error) (archive.Collection, error) {
				calls++
				if target == "alice" {
					return archive.Collection{}, &archive.Failure{Kind: tc.kind, Message: "fixture failure"}
				}
				return archive.Collection{}, nil
			}
			worked, err := a.scheduler.step(context.Background())
			if !worked || err != nil || calls != tc.wantCalls {
				t.Fatalf("unexpected batch result: worked=%v err=%v calls=%d", worked, err, calls)
			}
		})
	}
}

func TestSchedulerManualRunKeepsBatchSchedule(t *testing.T) {
	a := testApp(t)
	seedMonitor(t, a, "alice")
	seedMonitor(t, a, "bob")
	future := time.Now().Unix() + 3600
	a.store.DB.Exec("UPDATE monitors SET next_run=?", future)
	if err := a.scheduler.queue("bob"); err != nil {
		t.Fatal(err)
	}
	targets := []string{}
	a.scheduler.collect = func(_ context.Context, _ archive.Config, _ archive.Cookies, target string, _ archive.TargetIdentity, _ int, _ bool, _ func(archive.Page) error) (archive.Collection, error) {
		targets = append(targets, target)
		return archive.Collection{}, nil
	}
	worked, err := a.scheduler.step(context.Background())
	if !worked || err != nil || len(targets) != 1 || targets[0] != "bob" {
		t.Fatalf("manual run unexpectedly expanded to a batch: worked=%v err=%v targets=%v", worked, err, targets)
	}
	list, _ := monitors(a.store.DB)
	if list[0].NextRun != future || list[1].NextRun != future {
		t.Fatalf("manual run changed the automatic batch schedule: %+v", list)
	}
}

func TestNewSynchronizesMonitorSchedules(t *testing.T) {
	a := testApp(t)
	_, err := a.store.DB.Exec("INSERT INTO monitors(username,enabled,next_run) VALUES('alice',1,200),('bob',1,100),('paused',0,300)")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = New(a.configPath, a.Config(), a.store, false); err != nil {
		t.Fatal(err)
	}
	list, err := monitors(a.store.DB)
	if err != nil || len(list) != 3 {
		t.Fatal(err, list)
	}
	for _, monitor := range list {
		if monitor.Interval != a.Config().DefaultInterval {
			t.Fatalf("legacy interval was not normalized: %+v", monitor)
		}
		if monitor.Enabled && monitor.NextRun != 100 {
			t.Fatalf("enabled schedule was not synchronized to earliest run: %+v", monitor)
		}
	}
}
func TestRestartRecoversInterruptedJobs(t *testing.T) {
	a := testApp(t)
	seedMonitor(t, a, "alice")
	a.store.DB.Exec("UPDATE monitors SET running=1")
	a.store.StartRun("alice")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := a.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	m, _ := monitors(a.store.DB)
	if m[0].Running || m[0].LastError == "" {
		t.Fatal("stuck job not recovered")
	}
	var status string
	a.store.DB.QueryRow("SELECT status FROM runs LIMIT 1").Scan(&status)
	if status != "interrupted" {
		t.Fatal(status)
	}
}
