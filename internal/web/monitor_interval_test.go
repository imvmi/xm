package web

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"xm/internal/archive"
)

func TestMonitorIntervalSettings(t *testing.T) {
	a := testApp(t)
	b := login(t, a)
	seedMonitor(t, a, "global")
	ok(t, b.request("POST", "/api/monitors", map[string]any{"username": "custom", "enabled": true, "interval_override_seconds": 300}, "http://localhost", true), 201)
	read := func() Monitor {
		t.Helper()
		w := b.request("GET", "/api/monitors", nil, "", false)
		ok(t, w, 200)
		var list []Monitor
		if err := json.Unmarshal(w.Body.Bytes(), &list); err != nil {
			t.Fatal(err)
		}
		for _, m := range list {
			if m.Username == "custom" {
				return m
			}
		}
		t.Fatal("custom account missing")
		return Monitor{}
	}
	if m := read(); m.Interval != 300 || m.IntervalOverride != 300 || m.NextRun > time.Now().Unix() {
		t.Fatal("custom interval or first run", m)
	}
	// Invalid values must not create or change an account.
	for _, value := range []any{-1, 1, 59, 604801, 1.5, "300"} {
		for _, method := range []string{"POST", "PUT"} {
			path := "/api/monitors"
			if method == "PUT" {
				path += "/custom"
			}
			ok(t, b.request(method, path, map[string]any{"username": "invalid", "enabled": true, "interval_override_seconds": value}, "http://localhost", true), 400)
		}
	}
	if m := read(); m.IntervalOverride != 300 {
		t.Fatal("invalid update changed interval", m)
	}
	// Existing clients and quick switches omit the override and must preserve it.
	ok(t, b.request("PUT", "/api/monitors/custom", monitorInput{Enabled: true}, "http://localhost", true), 200)
	if m := read(); m.IntervalOverride != 300 {
		t.Fatal("omitted interval was reset", m)
	}
	before := time.Now().Unix()
	if _, err := a.store.DB.Exec("UPDATE monitors SET last_finished=?,next_run=? WHERE username='custom'", before-100, before+300); err != nil {
		t.Fatal(err)
	}
	next := monitorNextRun(t, a, "custom")
	ok(t, b.request("PUT", "/api/settings", map[string]any{"default_interval_seconds": 7200}, "http://localhost", true), 200)
	if m := read(); m.Interval != 300 || m.NextRun != next {
		t.Fatal("global update changed custom schedule", m)
	}
	if monitorNextRun(t, a, "global") < before+7200 {
		t.Fatal("global account was not rescheduled")
	}
	for _, value := range []int{60, 604800, 0} {
		before = time.Now().Unix()
		ok(t, b.request("PUT", "/api/monitors/custom", map[string]any{"enabled": true, "interval_override_seconds": value}, "http://localhost", true), 200)
		want := value
		if want == 0 {
			want = 7200
		}
		m := read()
		if m.IntervalOverride != value || m.Interval != want || m.NextRun < before+int64(want) || m.NextRun > time.Now().Unix()+int64(want) {
			t.Fatal("edit did not reschedule", m)
		}
		users, err := archiveUserDirectory(a.store.DB, "custom")
		if err != nil || len(users) != 1 || users[0].Interval != want {
			t.Fatal("profile interval", users, err)
		}
	}
	// Resetting to global restores inheritance on subsequent global updates.
	ok(t, b.request("PUT", "/api/settings", map[string]any{"default_interval_seconds": 10800}, "http://localhost", true), 200)
	if m := read(); m.Interval != 10800 || m.NextRun < time.Now().Unix()+10799 {
		t.Fatal("inheritance not restored", m)
	}
}

func TestMonitorIntervalSchedulingAndRestart(t *testing.T) {
	a := testApp(t)
	b := login(t, a)
	seedMonitor(t, a, "alice")
	seedMonitor(t, a, "bob")
	ok(t, b.request("PUT", "/api/monitors/alice", map[string]any{"enabled": true, "interval_override_seconds": 120}, "http://localhost", true), 200)
	if monitorNextRun(t, a, "alice") != 0 {
		t.Fatal("first run was postponed")
	}
	// Change the override during collection; scheduling must read the saved value.
	a.scheduler.collect = func(_ context.Context, _ archive.Config, _ archive.Cookies, target string, _ archive.TargetIdentity, _ int, _ bool, _ func(archive.Page) error) (archive.Collection, error) {
		if target == "alice" {
			ok(t, b.request("PUT", "/api/monitors/alice", map[string]any{"enabled": true, "interval_override_seconds": 60}, "http://localhost", true), 200)
		}
		return archive.Collection{Stop: "known_boundary_reached", HistoryComplete: true}, nil
	}
	before := time.Now().Unix()
	schedulerStep(t, a)
	for name, interval := range map[string]int64{"alice": 60, "bob": 3600} {
		next := monitorNextRun(t, a, name)
		if next < before+interval || next > time.Now().Unix()+interval {
			t.Fatal("wrong interval after completion", name, next)
		}
	}
	next := monitorNextRun(t, a, "alice")
	if _, err := New(a.configPath, a.Config(), a.store, false); err != nil {
		t.Fatal(err)
	}
	list, err := monitors(a.store.DB)
	if err != nil || list[0].IntervalOverride != 60 || list[0].Interval != 60 || list[0].NextRun != next {
		t.Fatal("restart lost override", list, err)
	}
	// Paused accounts keep their schedule when editing; resuming runs immediately.
	ok(t, b.request("PUT", "/api/monitors/alice", map[string]any{"enabled": false, "interval_override_seconds": 180}, "http://localhost", true), 200)
	if monitorNextRun(t, a, "alice") != next {
		t.Fatal("edit changed paused schedule")
	}
	ok(t, b.request("PUT", "/api/monitors/alice", monitorInput{Enabled: true}, "http://localhost", true), 200)
	if monitorNextRun(t, a, "alice") > time.Now().Unix() {
		t.Fatal("resumed account is not due")
	}
}

func TestMonitorIntervalSchemaUpgrade(t *testing.T) {
	a := testApp(t)
	seedMonitor(t, a, "legacy")
	// Simulate a database from before account-specific intervals existed.
	if _, err := a.store.DB.Exec("ALTER TABLE monitors DROP COLUMN interval_override_seconds"); err != nil {
		t.Fatal(err)
	}
	if err := migrate(a.store); err != nil {
		t.Fatal(err)
	}
	list, err := monitors(a.store.DB)
	if err != nil || len(list) != 1 || list[0].IntervalOverride != 0 || list[0].Interval != a.Config().DefaultInterval || list[0].NextRun != 0 {
		t.Fatal("upgrade changed legacy account", list, err)
	}
	if _, err := a.store.DB.Exec("UPDATE monitors SET interval_override_seconds=300"); err != nil {
		t.Fatal(err)
	}
	if err := migrate(a.store); err != nil {
		t.Fatal(err)
	}
	list, err = monitors(a.store.DB)
	if err != nil || list[0].IntervalOverride != 300 {
		t.Fatal("migration reset override", list, err)
	}
}

func TestMonitorIntervalCancelQueuedRun(t *testing.T) {
	a := testApp(t)
	b := login(t, a)
	seedMonitor(t, a, "alice")
	seedMonitor(t, a, "bob")
	ok(t, b.request("PUT", "/api/monitors/bob", map[string]any{"enabled": true, "interval_override_seconds": 120}, "http://localhost", true), 200)
	a.scheduler.collect = func(_ context.Context, _ archive.Config, _ archive.Cookies, target string, _ archive.TargetIdentity, _ int, _ bool, _ func(archive.Page) error) (archive.Collection, error) {
		if target != "alice" {
			t.Fatal("cancelled account ran", target)
		}
		var id int64
		if err := a.store.DB.QueryRow("SELECT id FROM collection_jobs WHERE target='bob' AND finished_at=0").Scan(&id); err != nil {
			t.Fatal(err)
		}
		if _, err := cancelWaitingCollectionJob(a.store.DB, id); err != nil {
			t.Fatal(err)
		}
		return archive.Collection{Stop: "known_boundary_reached", HistoryComplete: true}, nil
	}
	before := time.Now().Unix()
	schedulerStep(t, a)
	if next := monitorNextRun(t, a, "bob"); next < before+120 || next > time.Now().Unix()+120 {
		t.Fatal("cancelled account used global interval", next)
	}
}

func TestMonitorIntervalEditRollsBack(t *testing.T) {
	a := testApp(t)
	b := login(t, a)
	seedMonitor(t, a, "alice")
	if _, err := a.store.DB.Exec("UPDATE monitors SET last_finished=1,next_run=123,interval_override_seconds=300"); err != nil {
		t.Fatal(err)
	}
	trigger := "CREATE TRIGGER reject_interval BEFORE UPDATE OF interval_override_seconds ON monitors BEGIN SELECT RAISE(ABORT,'injected interval failure'); END"
	if a.store.DB.Dialect == "mysql" {
		trigger = "CREATE TRIGGER reject_interval BEFORE UPDATE ON monitors FOR EACH ROW BEGIN IF NEW.interval_override_seconds<>OLD.interval_override_seconds THEN SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT='injected interval failure'; END IF; END"
	}
	if _, err := a.store.DB.Exec(trigger); err != nil {
		t.Fatal(err)
	}
	ok(t, b.request("PUT", "/api/monitors/alice", map[string]any{"enabled": true, "replies": true, "show_in_timeline": false, "interval_override_seconds": 60}, "http://localhost", true), 500)
	list, err := monitors(a.store.DB)
	if err != nil || len(list) != 1 || list[0].IntervalOverride != 300 || list[0].NextRun != 123 || list[0].Replies || !list[0].ShowInTimeline {
		t.Fatal("failed interval update was partially saved", list, err)
	}
}
