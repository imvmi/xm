package web

import (
	"context"
	"reflect"
	"strconv"
	"testing"
	"time"

	"xm/internal/archive"
)

func monitorNextRun(t *testing.T, a *App, target string) int64 {
	t.Helper()
	var next int64
	if err := a.store.DB.QueryRow("SELECT next_run FROM monitors WHERE username=?", target).Scan(&next); err != nil {
		t.Fatal(err)
	}
	return next
}

func TestNewAccountDoesNotTriggerFutureAccounts(t *testing.T) {
	for _, tc := range []struct {
		name            string
		enabled, paused bool
		want            []string
	}{
		{"enabled", true, false, []string{"carol"}},
		{"disabled", false, false, nil},
		{"globally_paused", true, true, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := testApp(t)
			b := login(t, a)
			seedMonitor(t, a, "alice")
			seedMonitor(t, a, "bob")
			future := time.Now().Add(2 * time.Hour).Unix()
			if _, err := a.store.DB.Exec("UPDATE monitors SET next_run=?", future); err != nil {
				t.Fatal(err)
			}
			cfg := a.Config()
			cfg.PollingPaused = tc.paused
			if err := a.saveConfig(cfg); err != nil {
				t.Fatal(err)
			}
			var targets []string
			a.scheduler.collect = recordTargets(&targets)
			ok(t, b.request("POST", "/api/monitors", monitorInput{Username: "carol", Enabled: tc.enabled}, "http://localhost", true), 201)
			for range 2 {
				if _, err := a.scheduler.step(context.Background()); err != nil {
					t.Fatal(err)
				}
			}
			if !reflect.DeepEqual(targets, tc.want) {
				t.Fatalf("got %v want %v", targets, tc.want)
			}
			for _, target := range []string{"alice", "bob"} {
				if next := monitorNextRun(t, a, target); next != future {
					t.Fatalf("%s schedule changed: %d != %d", target, next, future)
				}
			}
			if tc.enabled && !tc.paused && monitorNextRun(t, a, "carol") <= time.Now().Unix() {
				t.Fatal("new account was not rescheduled")
			}
		})
	}
}

func TestAccountAddedDuringBatchKeepsFirstRunDue(t *testing.T) {
	for _, restart := range []bool{false, true} {
		t.Run(strconv.FormatBool(restart), func(t *testing.T) {
			a := testApp(t)
			b := login(t, a)
			seedMonitor(t, a, "alice")
			seedMonitor(t, a, "future")
			future := time.Now().Add(2 * time.Hour).Unix()
			if _, err := a.store.DB.Exec("UPDATE monitors SET next_run=? WHERE username='future'", future); err != nil {
				t.Fatal(err)
			}
			var targets []string
			a.scheduler.collect = func(_ context.Context, _ archive.Config, _ archive.Cookies, target string, _ archive.TargetIdentity, _ int, _ bool, _ func(archive.Page) error) (archive.Collection, error) {
				targets = append(targets, target)
				ok(t, b.request("POST", "/api/monitors", monitorInput{Username: "carol", Enabled: true}, "http://localhost", true), 201)
				return archive.Collection{Stop: "known_boundary_reached", HistoryComplete: true}, nil
			}
			schedulerStep(t, a)
			if !reflect.DeepEqual(targets, []string{"alice"}) || monitorNextRun(t, a, "carol") > time.Now().Unix() {
				t.Fatal("new account's first run postponed", targets)
			}
			aliceNext := monitorNextRun(t, a, "alice")
			if restart {
				next, err := New(a.configPath, a.Config(), a.store, false)
				if err != nil {
					t.Fatal(err)
				}
				next.scheduler.download = a.scheduler.download
				next.scheduler.avatar = a.scheduler.avatar
				a = next
				t.Cleanup(a.scheduler.closeSession)
			}
			a.scheduler.collect = recordTargets(&targets)
			schedulerStep(t, a)
			if worked, err := a.scheduler.step(context.Background()); worked || err != nil {
				t.Fatal("finished accounts ran again", worked, err)
			}
			if !reflect.DeepEqual(targets, []string{"alice", "carol"}) {
				t.Fatal("wrong accounts executed", targets)
			}
			if monitorNextRun(t, a, "alice") != aliceNext || monitorNextRun(t, a, "future") != future {
				t.Fatal("unrelated account plans changed")
			}
		})
	}
}

func TestResumingAccountDoesNotTriggerFutureAccounts(t *testing.T) {
	a := testApp(t)
	b := login(t, a)
	seedMonitor(t, a, "alice")
	seedMonitor(t, a, "bob")
	future := time.Now().Add(2 * time.Hour).Unix()
	if _, err := a.store.DB.Exec("UPDATE monitors SET next_run=?,enabled=CASE WHEN username='bob' THEN 0 ELSE 1 END", future); err != nil {
		t.Fatal(err)
	}
	ok(t, b.request("PUT", "/api/monitors/bob", monitorInput{Enabled: true}, "http://localhost", true), 200)
	var targets []string
	a.scheduler.collect = recordTargets(&targets)
	schedulerStep(t, a)
	if !reflect.DeepEqual(targets, []string{"bob"}) || monitorNextRun(t, a, "alice") != future {
		t.Fatal("resume affected other accounts", targets)
	}
}

func TestDueBatchPreservesFutureManualAccountSchedule(t *testing.T) {
	a := testApp(t)
	seedMonitor(t, a, "alice")
	seedMonitor(t, a, "bob")
	future := time.Now().Add(2 * time.Hour).Unix()
	if _, err := a.store.DB.Exec("UPDATE monitors SET next_run=? WHERE username='bob'", future); err != nil {
		t.Fatal(err)
	}
	if err := a.scheduler.queue("bob"); err != nil {
		t.Fatal(err)
	}
	var targets []string
	a.scheduler.collect = recordTargets(&targets)
	schedulerStep(t, a)
	if !reflect.DeepEqual(targets, []string{"bob", "alice"}) {
		t.Fatal("queued task did not retain FIFO order", targets)
	}
	if monitorNextRun(t, a, "bob") != future || monitorNextRun(t, a, "alice") <= time.Now().Unix() {
		t.Fatal("completion changed wrong schedule")
	}
}

func TestCancelledAutomaticAccountWaitsUntilNextCycle(t *testing.T) {
	a := testApp(t)
	b := login(t, a)
	seedMonitor(t, a, "alice")
	seedMonitor(t, a, "bob")
	var targets []string
	a.scheduler.collect = func(_ context.Context, _ archive.Config, _ archive.Cookies, target string, _ archive.TargetIdentity, _ int, _ bool, _ func(archive.Page) error) (archive.Collection, error) {
		targets = append(targets, target)
		var id int64
		if err := a.store.DB.QueryRow("SELECT id FROM collection_jobs WHERE target='bob' AND finished_at=0").Scan(&id); err != nil {
			t.Fatal(err)
		}
		ok(t, b.request("DELETE", "/api/activity/jobs/"+strconv.FormatInt(id, 10), nil, "http://localhost", true), 200)
		return archive.Collection{Stop: "known_boundary_reached", HistoryComplete: true}, nil
	}
	schedulerStep(t, a)
	a.scheduler.collect = recordTargets(&targets)
	if worked, err := a.scheduler.step(context.Background()); worked || err != nil {
		t.Fatal("cancelled slot was immediately recreated", worked, err)
	}
	if !reflect.DeepEqual(targets, []string{"alice"}) || monitorNextRun(t, a, "bob") <= time.Now().Unix() {
		t.Fatal("cancelled account did not skip this cycle", targets)
	}
	// A newly added account must not pull the cancelled account back into its batch.
	ok(t, b.request("POST", "/api/monitors", monitorInput{Username: "carol", Enabled: true}, "http://localhost", true), 201)
	schedulerStep(t, a)
	if !reflect.DeepEqual(targets, []string{"alice", "carol"}) {
		t.Fatal(targets)
	}
}
