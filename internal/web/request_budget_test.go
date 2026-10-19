package web

import (
	"context"
	"testing"

	"xm/internal/archive"
)

func TestSchedulerDoesNotInjectRequestPacerAndCompletesBatch(t *testing.T) {
	a := testApp(t)
	for _, name := range []string{"alice", "bob", "carol"} {
		seedMonitor(t, a, name)
	}
	if err := setState(a.store.DB, "rate_limit_failures", "3"); err != nil {
		t.Fatal(err)
	}
	var targets []string
	a.scheduler.collect = func(_ context.Context, _ archive.Config, _ archive.Cookies, target string, identity archive.TargetIdentity, _ int, _ bool, _ func(archive.Page) error) (archive.Collection, error) {
		targets = append(targets, target)
		if identity.RequestBudget != nil {
			t.Fatal("request pacer was injected")
		}
		return archive.Collection{Stop: "known_boundary_reached"}, nil
	}
	if worked, err := a.scheduler.step(context.Background()); !worked || err != nil {
		t.Fatal(worked, err)
	}
	if len(targets) != 3 {
		t.Fatal("low remaining header paused the batch", targets)
	}
	if state(a.store.DB, "request_budget_until") != "" {
		t.Fatal("proactive request wait was created")
	}
	if state(a.store.DB, "rate_limit_failures") != "0" {
		t.Fatal("successful batch did not reset rate backoff")
	}
}

func TestSchedulerManualTaskCompletesWithoutRequestPacer(t *testing.T) {
	a := testApp(t)
	seedMonitor(t, a, "alice")
	cfg := a.Config()
	cfg.PollingPaused = true
	if err := a.saveConfig(cfg); err != nil {
		t.Fatal(err)
	}
	if err := a.scheduler.queue("alice"); err != nil {
		t.Fatal(err)
	}
	a.scheduler.collect = func(_ context.Context, _ archive.Config, _ archive.Cookies, _ string, identity archive.TargetIdentity, _ int, _ bool, _ func(archive.Page) error) (archive.Collection, error) {
		if identity.RequestBudget != nil {
			t.Fatal("request pacer was injected")
		}
		return archive.Collection{Stop: "known_boundary_reached"}, nil
	}
	if worked, err := a.scheduler.step(context.Background()); !worked || err != nil {
		t.Fatal(worked, err)
	}
	list, err := monitors(a.store.DB)
	if err != nil || list[0].Requested || list[0].Failures != 0 {
		t.Fatal(list, err)
	}
	var status string
	if err = a.store.DB.QueryRow("SELECT status FROM collection_jobs ORDER BY id DESC LIMIT 1").Scan(&status); err != nil || status != "completed" {
		t.Fatal(status, err)
	}
}
