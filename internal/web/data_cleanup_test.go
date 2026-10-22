package web

import (
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"testing"

	"xm/internal/archive"
)

func seedDataCleanup(t *testing.T, a *App) {
	t.Helper()
	seedMonitor(t, a, "alice")
	if err := a.store.SavePosts("alice", []archive.Post{{ID: "1", Text: "keep archive"}}); err != nil {
		t.Fatal(err)
	}
	queries := []string{
		"INSERT INTO notification_seen(target,post_id) VALUES('alice','1')",
		"INSERT INTO notification_targets(target,ready) VALUES('alice',1)",
		"INSERT INTO notification_incidents(`key`,kind,last_notice) VALUES('cookie','auth',123)",
		"INSERT INTO collection_jobs(id,target,source,status,queued_at,queue_order,finished_at,run_id) VALUES(1,'alice','manual','success',1,1,2,25),(2,'alice','manual','running',1,2,0,26),(3,'alice','manual','retrying',1,3,0,27),(4,'alice','manual','queued',1,4,0,28),(5,'alice','manual','cancelled',1,5,2,0)",
	}
	for _, query := range queries {
		if _, err := a.store.DB.Exec(query); err != nil {
			t.Fatal(err)
		}
	}
	for i := 1; i <= 28; i++ {
		status, jobID := "success", 1
		if i > 25 {
			status = []string{"running", "failed", "incomplete"}[i-26]
			jobID = i - 24
		}
		if _, err := a.store.DB.Exec("INSERT INTO runs(id,target,started_at,status,job_id) VALUES(?,'alice','2026-01-01T00:00:00Z',?,?)", i, status, jobID); err != nil {
			t.Fatal(err)
		}
		if _, err := a.store.DB.Exec("INSERT INTO run_metrics(run_id,new_posts,posts_read) VALUES(?,1,2)", i); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := a.store.DB.Exec("INSERT INTO run_metrics(run_id,new_posts,posts_read) VALUES(99,1,2)"); err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= 39; i++ {
		status := "sent"
		if i > 36 {
			status = []string{"failed", "cancelled", "pending"}[i-37]
		}
		// Pending retries also have finished_at set by the notification worker.
		if _, err := a.store.DB.Exec(`INSERT INTO notification_deliveries(event_id,channel_id,destination,payload,status,attempts,next_attempt,created_at,finished_at) VALUES(?,'channel','recipient','{"title":"test"}',?,2,456,123,234)`, fmt.Sprint(i), status); err != nil {
			t.Fatal(err)
		}
	}
}

func assertCleanupCount(t *testing.T, a *App, table string, want int) {
	t.Helper()
	var got int
	if err := a.store.DB.QueryRow("SELECT COUNT(*) FROM " + table).Scan(&got); err != nil || got != want {
		t.Fatalf("%s: count=%d, want=%d, err=%v", table, got, want, err)
	}
}

func TestDataCleanupSelectionAndPreservation(t *testing.T) {
	for _, selection := range []dataCleanupInput{
		{Notifications: true, Confirm: true},
		{Tasks: true, Confirm: true},
		{Notifications: true, Tasks: true, Confirm: true},
	} {
		t.Run(fmt.Sprintf("notifications=%t/tasks=%t", selection.Notifications, selection.Tasks), func(t *testing.T) {
			a := testApp(t)
			seedDataCleanup(t, a)
			b := login(t, a)
			w := b.request("POST", "/api/settings/cleanup", selection, "http://localhost", true)
			ok(t, w, 200)
			var got, want dataCleanupResult
			if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
				t.Fatal(err)
			}
			notifications, runs, jobs, metrics := 39, 28, 5, 29
			if selection.Notifications {
				want.Notifications = 38
				notifications = 1
			}
			if selection.Tasks {
				want.Runs, want.Jobs = 25, 2
				runs, jobs, metrics = 3, 3, 3
			}
			if got != want {
				t.Fatalf("cleanup result: %+v, want %+v", got, want)
			}
			for table, count := range map[string]int{"notification_deliveries": notifications, "runs": runs, "collection_jobs": jobs, "run_metrics": metrics, "posts": 1, "monitors": 1, "notification_seen": 1, "notification_targets": 1, "notification_incidents": 1} {
				assertCleanupCount(t, a, table, count)
			}
			assertCleanupCount(t, a, "notification_deliveries WHERE status='pending' AND attempts=2 AND next_attempt=456", 1)
			assertCleanupCount(t, a, "runs WHERE id IN (26,27,28)", 3)
			assertCleanupCount(t, a, "collection_jobs WHERE finished_at=0 AND run_id IN (26,27,28)", 3)
			w = b.request("POST", "/api/settings/cleanup", selection, "http://localhost", true)
			ok(t, w, 200)
			if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil || got != (dataCleanupResult{}) {
				t.Fatal("repeated cleanup should be empty", got, err)
			}
		})
	}
}

func TestDataCleanupGuards(t *testing.T) {
	a := testApp(t)
	seedDataCleanup(t, a)
	b := login(t, a)
	input := dataCleanupInput{Notifications: true, Tasks: true, Confirm: true}
	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "http://localhost/api/settings/cleanup", nil)
	r.Header.Set("Origin", "http://localhost")
	a.Handler().ServeHTTP(w, r)
	ok(t, w, 401)
	ok(t, b.request("POST", "/api/settings/cleanup", input, "http://evil.example", true), 403)
	ok(t, b.request("POST", "/api/settings/cleanup", input, "http://localhost", false), 403)
	ok(t, b.request("POST", "/api/settings/cleanup", dataCleanupInput{Tasks: true}, "http://localhost", true), 400)
	ok(t, b.request("POST", "/api/settings/cleanup", dataCleanupInput{Confirm: true}, "http://localhost", true), 400)
	a.maintenance.Store(true)
	ok(t, b.request("POST", "/api/settings/cleanup", input, "http://localhost", true), 503)
	a.maintenance.Store(false)
	assertCleanupCount(t, a, "notification_deliveries", 39)
	assertCleanupCount(t, a, "runs", 28)
}

func TestDataCleanupRollsBackOnFailure(t *testing.T) {
	a := testApp(t)
	seedDataCleanup(t, a)
	b := login(t, a)
	if _, err := a.store.DB.Exec("DROP TABLE run_metrics"); err != nil {
		t.Fatal(err)
	}
	ok(t, b.request("POST", "/api/settings/cleanup", dataCleanupInput{Notifications: true, Tasks: true, Confirm: true}, "http://localhost", true), 500)
	assertCleanupCount(t, a, "notification_deliveries", 39)
	assertCleanupCount(t, a, "runs", 28)
	assertCleanupCount(t, a, "collection_jobs", 5)
}
