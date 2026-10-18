package web

import (
	"context"
	"github.com/chromedp/cdproto/network"
	"testing"
	"time"
	"xm/internal/archive"
)

func TestSchedulerBudgetWaitPreservesFailuresAndManualQueue(t *testing.T) {
	a := testApp(t)
	seedMonitor(t, a, "alice")
	seedMonitor(t, a, "bob")
	if _, err := a.store.DB.Exec("UPDATE monitors SET failures=2"); err != nil {
		t.Fatal(err)
	}
	var shared *archive.RequestBudget
	called := 0
	a.scheduler.collect = func(ctx context.Context, _ archive.Config, _ archive.Cookies, _ string, i archive.TargetIdentity, _ int, _ bool, save func(archive.Page) error) (archive.Collection, error) {
		called++
		shared = i.RequestBudget
		if shared == nil {
			t.Fatal("missing shared budget")
		}
		if err := save(archive.Page{Progress: &archive.CollectionProgress{UserID: "42", Posts: archive.TimelineProgress{Cursor: "saved"}}}); err != nil {
			t.Fatal(err)
		}
		if err := shared.Observe("UserTweets", 200, network.Headers{"x-rate-limit-remaining": "0", "x-rate-limit-reset": time.Now().Add(time.Hour).Unix()}); err != nil {
			t.Fatal(err)
		}
		return archive.Collection{}, shared.Acquire(ctx, "UserTweets")
	}
	if worked, err := a.scheduler.step(context.Background()); !worked || err != nil {
		t.Fatal(worked, err)
	}
	if called != 1 {
		t.Fatal(called)
	}
	runs, err := recentRuns(a.store.DB)
	if err != nil || runs[0]["status"] != "budget_wait" {
		t.Fatal(runs, err)
	}
	list, err := monitors(a.store.DB)
	if err != nil || list[0].Failures != 2 || !list[0].Catchup {
		t.Fatal(list, err)
	}
	if state(a.store.DB, "rate_limit_failures") != "" || state(a.store.DB, "cooldown_until") != "" {
		t.Fatal("proactive wait treated as X rejection")
	}
	if err = a.scheduler.queue("bob"); err != nil {
		t.Fatal(err)
	}
	b, err := New(a.configPath, a.Config(), a.store, false)
	if err != nil {
		t.Fatal(err)
	}
	b.scheduler.collect = a.scheduler.collect
	if worked, err := b.scheduler.step(context.Background()); worked || err != nil {
		t.Fatal("restart/manual bypass", worked, err)
	}
	if called != 1 {
		t.Fatal(called)
	}
	var requested bool
	a.store.DB.QueryRow("SELECT requested FROM monitors WHERE username='bob'").Scan(&requested)
	if !requested {
		t.Fatal("manual queue lost during wait")
	}
}

func TestSchedulerBudgetResumesRemainingBatchWithoutRevisitingCompletedTargets(t *testing.T) {
	a := testApp(t)
	for _, name := range []string{"alice", "bob", "carol"} {
		seedMonitor(t, a, name)
	}
	setState(a.store.DB, "rate_limit_failures", "3")
	targets := []string{}
	a.scheduler.collect = func(_ context.Context, _ archive.Config, _ archive.Cookies, target string, i archive.TargetIdentity, _ int, _ bool, _ func(archive.Page) error) (archive.Collection, error) {
		targets = append(targets, target)
		if target == "alice" {
			if err := i.RequestBudget.Observe("UserTweets", 200, network.Headers{"x-rate-limit-remaining": "0"}); err != nil {
				t.Fatal(err)
			}
		}
		return archive.Collection{Stop: "known_boundary_reached"}, nil
	}
	if worked, err := a.scheduler.step(context.Background()); !worked || err != nil {
		t.Fatal(worked, err)
	}
	if len(targets) != 1 || state(a.store.DB, "budget_batch_pending") != `["bob","carol"]` {
		t.Fatal(targets, state(a.store.DB, "budget_batch_pending"))
	}
	if state(a.store.DB, "rate_limit_failures") != "3" {
		t.Fatal("partial batch reset backoff")
	}
	// Expire the synthetic budget without sending any live requests.
	if _, err := a.store.DB.Exec("UPDATE request_budgets SET state='{}'"); err != nil {
		t.Fatal(err)
	}
	b, err := New(a.configPath, a.Config(), a.store, false)
	if err != nil {
		t.Fatal(err)
	}
	b.scheduler.collect = a.scheduler.collect
	b.scheduler.download = a.scheduler.download
	b.scheduler.avatar = a.scheduler.avatar
	if worked, err := b.scheduler.step(context.Background()); !worked || err != nil {
		t.Fatal(worked, err)
	}
	if len(targets) != 3 || targets[1] != "bob" || targets[2] != "carol" {
		t.Fatal("revisited completed target", targets)
	}
	if state(a.store.DB, "rate_limit_failures") != "0" {
		t.Fatal("completed split batch did not reset backoff")
	}
}
func TestSchedulerManualBudgetWaitKeepsRequest(t *testing.T) {
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
	a.scheduler.collect = func(ctx context.Context, _ archive.Config, _ archive.Cookies, _ string, i archive.TargetIdentity, _ int, _ bool, _ func(archive.Page) error) (archive.Collection, error) {
		if err := i.RequestBudget.Observe("UserTweets", 200, network.Headers{"x-rate-limit-remaining": "0"}); err != nil {
			t.Fatal(err)
		}
		return archive.Collection{}, i.RequestBudget.Acquire(ctx, "UserTweets")
	}
	if worked, err := a.scheduler.step(context.Background()); !worked || err != nil {
		t.Fatal(worked, err)
	}
	list, err := monitors(a.store.DB)
	if err != nil || !list[0].Requested {
		t.Fatal("manual task lost after proactive wait", list, err)
	}
}
