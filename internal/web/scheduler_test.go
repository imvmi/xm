package web

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"xm/internal/archive"
)

func seedMonitor(t *testing.T, a *App, name string) {
	t.Helper()
	if err := a.store.SaveCookies(archive.Cookies{AuthToken: "synthetic", CSRF: "synthetic"}); err != nil {
		t.Fatal(err)
	}
	_, err := a.store.DB.Exec("INSERT INTO monitors(username,interval_seconds,post_limit,enabled,next_run) VALUES(?,60,20,1,0)", name)
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
		return archive.Collection{Posts: 1, Stop: "limit_reached"}, err
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
				if state(a.store.DB, "cooldown_until") == "" || m[0].NextRun < time.Now().Unix()+890 {
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
