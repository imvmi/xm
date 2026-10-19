package web

import (
	"context"
	"encoding/json"
	"testing"
	"xm/internal/archive"
)

func TestActivityShowsActualPhaseAndTaskDeadline(t *testing.T) {
	a := testApp(t)
	b := login(t, a)
	seedMonitor(t, a, "alice")
	photo := func(id, author string) archive.Post {
		return archive.Post{ID: id, AuthorID: author, Username: author, Assets: []archive.Asset{{ID: "photo", Kind: "photo", URL: "https://pbs.twimg.com/fixture.jpg"}}}
	}
	post, quote := photo("100", "42"), photo("200", "43")
	post.Quote = &quote
	if err := a.store.SavePosts("alice", []archive.Post{post}); err != nil {
		t.Fatal(err)
	}
	if err := a.store.SavePosts("unrelated", []archive.Post{photo("300", "44")}); err != nil {
		t.Fatal(err)
	}
	if _, err := a.store.DB.Exec("UPDATE media SET status='done' WHERE post_id='100'"); err != nil {
		t.Fatal(err)
	}
	read := func(phase string, done int) {
		t.Helper()
		w := b.request("GET", "/api/activity", nil, "", false)
		ok(t, w, 200)
		var data struct {
			Current *CollectionJob `json:"current"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &data); err != nil {
			t.Fatal(err)
		}
		job := data.Current
		if job == nil || job.Phase != phase || job.Remaining == nil || *job.Remaining < 21500 || *job.Remaining > 21600 || job.Elapsed < 0 {
			t.Fatal("wrong phase or captured deadline", job)
		}
		if job.MediaTotal == nil || *job.MediaTotal != 2 || job.MediaDone == nil || *job.MediaDone != done {
			t.Fatal("media counts should include quotes, exclude unrelated assets", job)
		}
	}
	a.scheduler.collect = func(_ context.Context, _ archive.Config, _ archive.Cookies, _ string, _ archive.TargetIdentity, _ int, _ bool, save func(archive.Page) error) (archive.Collection, error) {
		read("timeline", 1)
		// Changing the next task's configuration must not change this task's countdown.
		ok(t, b.request("PUT", "/api/settings", map[string]any{"browser_max_age_seconds": 3600}, "http://localhost", true), 200)
		if err := save(archive.Page{Profile: &archive.Profile{ID: "42", Username: "alice", AvatarURL: "https://pbs.twimg.com/avatar.jpg"}}); err != nil {
			t.Fatal(err)
		}
		return archive.Collection{Stop: "known_boundary_reached", HistoryComplete: true}, nil
	}
	a.scheduler.avatar = func(context.Context, archive.Config, archive.Profile) error { read("avatar", 1); return nil }
	a.scheduler.download = func(context.Context, *archive.Store, archive.Config, string, bool) (archive.DownloadSummary, error) {
		read("media", 1)
		if _, err := a.store.DB.Exec("UPDATE media SET status='done' WHERE post_id='200'"); err != nil {
			t.Fatal(err)
		}
		read("media", 2)
		return archive.DownloadSummary{}, nil
	}
	schedulerStep(t, a)
	current, _, err := activityJobs(a.store.DB)
	if err != nil || current != nil {
		t.Fatal("finished task still running", current, err)
	}
	a.scheduler.mu.Lock()
	p := a.scheduler.progress
	a.scheduler.mu.Unlock()
	if p.jobID != 0 {
		t.Fatal("stale task progress retained", p)
	}
}
