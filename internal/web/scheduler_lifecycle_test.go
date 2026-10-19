package web

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"
	"time"

	"xm/internal/archive"
)

type managedTestSession struct {
	closed  int
	dead    bool
	collect func(context.Context, string, archive.TargetIdentity, func(archive.Page) error) (archive.Collection, error)
}

func (s *managedTestSession) Healthy(context.Context) error {
	if s.dead {
		return errors.New("closed fixture browser")
	}
	return nil
}
func (s *managedTestSession) Close() { s.closed++ }
func (s *managedTestSession) CollectBound(ctx context.Context, target string, id archive.TargetIdentity, _ int, _ bool, save func(archive.Page) error) (archive.Collection, error) {
	if s.collect != nil {
		return s.collect(ctx, target, id, save)
	}
	return archive.Collection{Stop: "known_boundary_reached"}, nil
}
func schedulerStep(t *testing.T, a *App) {
	t.Helper()
	if worked, err := a.scheduler.step(context.Background()); !worked || err != nil {
		t.Fatal(worked, err)
	}
}
func makeDue(t *testing.T, a *App) {
	t.Helper()
	if _, err := a.store.DB.Exec("UPDATE monitors SET next_run=0"); err != nil {
		t.Fatal(err)
	}
}

func TestSchedulerSessionSurvivesBatchesAndRecycles(t *testing.T) {
	a := testApp(t)
	cfg := a.Config()
	cfg.BrowserMaxAge = 3600
	cfg.BrowserMaxTasks = 10
	if err := a.saveConfig(cfg); err != nil {
		t.Fatal(err)
	}
	seedMonitor(t, a, "alice")
	var sessions []*managedTestSession
	a.scheduler.newSession = func(context.Context, archive.Config, archive.Cookies) (collectionSession, error) {
		session := &managedTestSession{}
		sessions = append(sessions, session)
		return session, nil
	}
	schedulerStep(t, a)
	makeDue(t, a)
	schedulerStep(t, a)
	if len(sessions) != 1 || sessions[0].closed != 0 {
		t.Fatal("browser restarted between batches")
	}
	// Local health failure is detected before the next collection.
	sessions[0].dead = true
	makeDue(t, a)
	schedulerStep(t, a)
	if len(sessions) != 2 || sessions[0].closed != 1 {
		t.Fatal("dead idle browser was reused")
	}
	a.scheduler.browser.created = time.Now().Add(-time.Duration(cfg.BrowserMaxAge) * time.Second)
	makeDue(t, a)
	schedulerStep(t, a)
	if len(sessions) != 3 || sessions[1].closed != 1 {
		t.Fatal("old session was not recycled")
	}
	a.scheduler.browser.tasks = cfg.BrowserMaxTasks
	makeDue(t, a)
	schedulerStep(t, a)
	if len(sessions) != 4 || sessions[2].closed != 1 {
		t.Fatal("task-count recycling failed")
	}
	stopped, cancel := context.WithCancel(context.Background())
	cancel()
	a.scheduler.Run(stopped)
	if sessions[3].closed != 1 {
		t.Fatal("shutdown did not close session")
	}
}

func TestSchedulerBrowserCrashRetainsCheckpointAndBacksOff(t *testing.T) {
	a := testApp(t)
	seedMonitor(t, a, "alice")
	creates := 0
	a.scheduler.newSession = func(context.Context, archive.Config, archive.Cookies) (collectionSession, error) {
		creates++
		session := &managedTestSession{}
		if creates == 1 {
			session.collect = func(_ context.Context, _ string, _ archive.TargetIdentity, save func(archive.Page) error) (archive.Collection, error) {
				err := save(archive.Page{Profile: &archive.Profile{ID: "42", Username: "alice"}, Posts: []archive.Post{{ID: "100", AuthorID: "42", Username: "alice"}}, Progress: &archive.CollectionProgress{UserID: "42", Posts: archive.TimelineProgress{Cursor: "saved-cursor"}}})
				if err != nil {
					t.Fatal(err)
				}
				session.dead = true
				return archive.Collection{Posts: 1}, errors.New("fixture browser crash")
			}
		} else {
			session.collect = func(_ context.Context, _ string, id archive.TargetIdentity, _ func(archive.Page) error) (archive.Collection, error) {
				if id.Progress.Posts.Cursor != "saved-cursor" {
					t.Fatal("lost committed cursor", id.Progress)
				}
				return archive.Collection{Stop: "known_boundary_reached"}, nil
			}
		}
		return session, nil
	}
	schedulerStep(t, a)
	if state(a.store.DB, "budget_batch_pending") != `["alice"]` || a.scheduler.browser.session != nil {
		t.Fatal("failed target/browser retained incorrectly")
	}
	if worked, err := a.scheduler.step(context.Background()); worked || err != nil || creates != 1 {
		t.Fatal("browser restart storm", worked, err, creates)
	}
	a.scheduler.browser.retryAt = time.Time{}
	schedulerStep(t, a)
	if creates != 2 || state(a.store.DB, "budget_batch_pending") != "" {
		t.Fatal("failed batch did not resume")
	}
}

func TestSchedulerBrowserStartupFailureBackoff(t *testing.T) {
	a := testApp(t)
	seedMonitor(t, a, "alice")
	seedMonitor(t, a, "bob")
	attempts := 0
	a.scheduler.newSession = func(context.Context, archive.Config, archive.Cookies) (collectionSession, error) {
		attempts++
		return nil, errors.New("fixture launch failure")
	}
	for _, delay := range []time.Duration{30 * time.Second, time.Minute, 2 * time.Minute} {
		before := time.Now()
		schedulerStep(t, a)
		if a.scheduler.browser.retryAt.Before(before.Add(delay)) {
			t.Fatal("missing exponential backoff")
		}
		previous := attempts
		if worked, err := a.scheduler.step(context.Background()); worked || err != nil || attempts != previous {
			t.Fatal("launch retry bypassed backoff")
		}
		a.scheduler.browser.retryAt = time.Time{}
	}
	list, err := monitors(a.store.DB)
	if err != nil || list[0].Running || list[1].Running || state(a.store.DB, "budget_batch_pending") == "" {
		t.Fatal(list, err)
	}
}

func TestSchedulerReadsChangedSettingsAtTaskBoundary(t *testing.T) {
	a := testApp(t)
	seedMonitor(t, a, "alice")
	seedMonitor(t, a, "bob")
	sessions := []*managedTestSession{}
	var cookiesSeen []archive.Cookies
	var proxies []string
	a.scheduler.newSession = func(_ context.Context, cfg archive.Config, cookies archive.Cookies) (collectionSession, error) {
		proxies = append(proxies, cfg.Proxy)
		cookiesSeen = append(cookiesSeen, cookies)
		session := &managedTestSession{}
		sessions = append(sessions, session)
		session.collect = func(_ context.Context, target string, _ archive.TargetIdentity, _ func(archive.Page) error) (archive.Collection, error) {
			if target == "alice" {
				cfg := a.Config()
				cfg.Proxy = "http://127.0.0.1:12345"
				if err := a.saveConfig(cfg); err != nil {
					t.Fatal(err)
				}
				if err := a.store.SaveCookies(archive.Cookies{AuthToken: "new-fixture", CSRF: "new-csrf"}); err != nil {
					t.Fatal(err)
				}
			}
			return archive.Collection{Stop: "known_boundary_reached"}, nil
		}
		return session, nil
	}
	schedulerStep(t, a)
	if len(sessions) != 2 || sessions[0].closed != 1 || proxies[1] != "http://127.0.0.1:12345" || cookiesSeen[1].AuthToken != "new-fixture" {
		t.Fatal("stale session settings", len(sessions))
	}
}

func TestSchedulerPauseResumeDurableQueueAndManualDedup(t *testing.T) {
	a := testApp(t)
	for _, name := range []string{"alice", "bob", "carol"} {
		seedMonitor(t, a, name)
	}
	targets := []string{}
	collect := func(_ context.Context, _ archive.Config, _ archive.Cookies, target string, _ archive.TargetIdentity, _ int, _ bool, _ func(archive.Page) error) (archive.Collection, error) {
		targets = append(targets, target)
		if target == "alice" {
			cfg := a.Config()
			cfg.PollingPaused = true
			if err := a.saveConfig(cfg); err != nil {
				t.Fatal(err)
			}
		}
		return archive.Collection{Stop: "known_boundary_reached"}, nil
	}
	a.scheduler.collect = collect
	schedulerStep(t, a)
	if !reflect.DeepEqual(targets, []string{"alice"}) || state(a.store.DB, "budget_batch_pending") != `["bob","carol"]` {
		t.Fatal("pause did not preserve queue", targets)
	}
	// Manual work during pause consumes its matching queued automatic task.
	if err := a.scheduler.queue("bob"); err != nil {
		t.Fatal(err)
	}
	if err := a.scheduler.queue("bob"); err == nil {
		t.Fatal("duplicate request accepted")
	}
	schedulerStep(t, a)
	if state(a.store.DB, "budget_batch_pending") != `["carol"]` {
		t.Fatal("manual request left duplicate automatic work")
	}
	restarted, err := New(a.configPath, a.Config(), a.store, false)
	if err != nil {
		t.Fatal(err)
	}
	restarted.scheduler.collect = collect
	restarted.scheduler.download = a.scheduler.download
	restarted.scheduler.avatar = a.scheduler.avatar
	cfg := restarted.Config()
	cfg.PollingPaused = false
	if err := restarted.saveConfig(cfg); err != nil {
		t.Fatal(err)
	}
	// Simulate persisted future schedules: unfinished work must still resume now.
	if _, err := a.store.DB.Exec("UPDATE monitors SET next_run=?", time.Now().Add(time.Hour).Unix()); err != nil {
		t.Fatal(err)
	}
	schedulerStep(t, restarted)
	if !reflect.DeepEqual(targets, []string{"alice", "bob", "carol"}) {
		t.Fatal("queue restarted completed tasks", targets)
	}
}

func TestSchedulerFairnessAndServiceCancellation(t *testing.T) {
	a := testApp(t)
	seedMonitor(t, a, "alice")
	seedMonitor(t, a, "bob")
	if _, err := a.store.DB.Exec("UPDATE monitors SET last_started=100,requested=1 WHERE username='alice'"); err != nil {
		t.Fatal(err)
	}
	targets := []string{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	a.scheduler.collect = func(_ context.Context, _ archive.Config, _ archive.Cookies, target string, _ archive.TargetIdentity, _ int, _ bool, _ func(archive.Page) error) (archive.Collection, error) {
		targets = append(targets, target)
		cancel()
		return archive.Collection{}, ctx.Err()
	}
	if worked, err := a.scheduler.step(ctx); !worked || err != nil {
		t.Fatal(worked, err)
	}
	if !reflect.DeepEqual(targets, []string{"bob"}) {
		t.Fatal("manual work starved older task", targets)
	}
	var pending []string
	if err := json.Unmarshal([]byte(state(a.store.DB, "budget_batch_pending")), &pending); err != nil {
		t.Fatal(err)
	}
	if len(pending) != 2 {
		t.Fatal("shutdown lost interrupted task", pending)
	}
}
