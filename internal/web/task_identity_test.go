package web

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"testing"

	"xm/internal/archive"
)

func TestTaskIdentityRefreshPreservesKeys(t *testing.T) {
	a := identityRefreshFixture(t)
	b := login(t, a)
	if err := a.store.SavePosts("alice", []archive.Post{{ID: "124", AuthorID: "42", Username: "alice", Raw: json.RawMessage(`{"core":{"user_results":{"result":{"core":{"name":"测试昵称"}}}}}`)}}); err != nil {
		t.Fatal(err)
	}
	job, err := ensureCollectionJob(a.store.DB, "alice", "manual")
	if err != nil {
		t.Fatal(err)
	}
	runID, err := a.store.StartRun("alice")
	if err != nil {
		t.Fatal(err)
	}
	if err = a.store.EndRun(runID, "bounded_complete", 1, ""); err != nil {
		t.Fatal(err)
	}
	// Entries created before the rename must resolve the new handle on read.
	a.refreshIdentity = func(context.Context, archive.Config, archive.Cookies, archive.TargetIdentity, []string, string) (archive.Profile, error) {
		return archive.Profile{ID: "42", Username: "newalice"}, nil
	}
	ok(t, b.request("POST", "/api/monitors/alice/identity", nil, "http://localhost", true), 200)
	// Run timestamps are strings, unlike queue timestamps; inspect only labels.
	read := func() (map[string]any, CollectionJob) {
		t.Helper()
		w := b.request("GET", "/api/activity", nil, "", false)
		ok(t, w, 200)
		var payload struct {
			Current *CollectionJob   `json:"current"`
			Queue   []CollectionJob  `json:"queue"`
			Runs    []map[string]any `json:"runs"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &payload); err != nil {
			t.Fatal(err)
		}
		if len(payload.Runs) != 1 {
			t.Fatal("history disappeared", payload.Runs)
		}
		if payload.Current != nil {
			return payload.Runs[0], *payload.Current
		}
		if len(payload.Queue) != 1 {
			t.Fatal("queue disappeared", payload.Queue)
		}
		return payload.Runs[0], payload.Queue[0]
	}
	for _, status := range []string{"queued", "running"} {
		if _, err = a.store.DB.Exec("UPDATE collection_jobs SET status=? WHERE id=?", status, job.ID); err != nil {
			t.Fatal(err)
		}
		history, active := read()
		if history["target"] != "alice" || history["current_username"] != "newalice" || history["name"] != "测试昵称" || active.ID != job.ID || active.Target != "alice" || active.CurrentUsername != "newalice" || active.Name != "测试昵称" {
			t.Fatal("task labels or archive keys are wrong", history, active)
		}
	}
	if _, err = a.store.DB.Exec("UPDATE collection_jobs SET status='queued' WHERE id=?", job.ID); err != nil {
		t.Fatal(err)
	}
	ok(t, b.request("DELETE", "/api/activity/jobs/"+strconv.FormatInt(job.ID, 10), nil, "http://localhost", true), 200)
	// Removing a monitor must not lose labels for retained execution history.
	if _, err = a.store.DB.Exec("DELETE FROM monitors WHERE username='alice'"); err != nil {
		t.Fatal(err)
	}
	history, err := recentRuns(a.store.DB)
	if err != nil || len(history) != 1 || history[0]["current_username"] != "newalice" || history[0]["name"] != "测试昵称" {
		t.Fatal("removed monitor lost archive identity", history, err)
	}
	profiles, err := taskProfiles(a.store.DB, []string{"unknown", "unknown"})
	if err != nil || len(profiles) != 1 || profiles["unknown"].CurrentUsername != "unknown" || profiles["unknown"].Name != "unknown" {
		t.Fatal("missing profile fallback failed", profiles, err)
	}
}

func TestRenamedAccountIncidentAndRecoveryLabels(t *testing.T) {
	for _, kind := range []string{"identity", "access", "media", "transient"} {
		t.Run(kind, func(t *testing.T) {
			a := identityRefreshFixture(t)
			notificationFixture(t, a)
			m := Monitor{Username: "alice"}
			if err := a.prepareNotifications(m); err != nil {
				t.Fatal(err)
			}
			if err := a.store.SaveTargetProfile("alice", archive.Profile{ID: "42", Username: "newalice"}); err != nil {
				t.Fatal(err)
			}
			if err := a.recordNotifications(a.Config(), m, false, false, kind, 3); err != nil {
				t.Fatal(err)
			}
			all := events(t, a)
			incident := all[len(all)-1]
			if incident.Target != "newalice" || !strings.Contains(incident.Message, "@newalice") || strings.Contains(incident.Message, "@alice") {
				t.Fatal("incident uses stale name", incident)
			}
			// Rename again before recovery: dedup/recovery keys must stay stable.
			if err := a.store.SaveTargetProfile("alice", archive.Profile{ID: "42", Username: "latestalice"}); err != nil {
				t.Fatal(err)
			}
			if err := a.recordNotifications(a.Config(), m, true, false, "", 0); err != nil {
				t.Fatal(err)
			}
			all = events(t, a)
			recovery := all[len(all)-1]
			if recovery.Type != "recovered" || recovery.Target != "latestalice" || !strings.Contains(recovery.Message, "@latestalice") {
				t.Fatal("recovery did not use latest identity", recovery)
			}
			var pending int
			if err := a.store.DB.QueryRow("SELECT COUNT(*) FROM notification_incidents").Scan(&pending); err != nil || pending != 0 {
				t.Fatal("rename orphaned incident", pending, err)
			}
		})
	}
}
