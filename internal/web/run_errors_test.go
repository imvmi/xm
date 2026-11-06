package web

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"xm/internal/archive"
)

func seedErrorRun(t *testing.T, a *App) int64 {
	t.Helper()
	seedMediaRetryState(t, a)
	id, err := a.store.StartRun("alice")
	if err != nil {
		t.Fatal(err)
	}
	if err := a.store.EndRun(id, "media_failed", 40, "历史记录：媒体下载返回 HTTP 403"); err != nil {
		t.Fatal(err)
	}
	return id
}

func TestRunErrorsDetailsAndManualMediaActions(t *testing.T) {
	a := testApp(t)
	id := seedErrorRun(t, a)
	b := login(t, a)
	base := fmt.Sprintf("/api/runs/%d", id)
	unauth := &browserClient{t: t, a: a}
	ok(t, unauth.request("GET", base+"/errors", nil, "http://localhost", false), 401)
	r := b.request("GET", base+"/errors", nil, "http://localhost", false)
	ok(t, r, 200)
	var detail struct {
		Run   runErrorRecord
		Items []archive.MediaProblem
		Total int
	}
	if err := json.Unmarshal(r.Body.Bytes(), &detail); err != nil {
		t.Fatal(err)
	}
	if detail.Run.ID != id || detail.Total != 5 || len(detail.Items) != 5 || !strings.Contains(detail.Run.Message, "历史记录") {
		t.Fatal(r.Body.String())
	}
	for _, item := range detail.Items {
		if item.PostID == "300" || item.Status == "done" {
			t.Fatal("unrelated or saved media leaked", item)
		}
	}
	if strings.Contains(r.Body.String(), "source_url") || strings.Contains(r.Body.String(), "video.twimg.com") {
		t.Fatal("source URL leaked")
	}
	path := base + "/media/200/video/ignore"
	ok(t, b.request("POST", path, nil, "http://localhost", false), 403)
	ok(t, b.request("POST", path, nil, "https://elsewhere.invalid", true), 403)
	ok(t, b.request("POST", base+"/media/300/video/ignore", nil, "http://localhost", true), 404)
	ok(t, b.request("POST", base+"/media/500/video/ignore", nil, "http://localhost", true), 409)
	ok(t, b.request("POST", base+"/media/200/video/delete", nil, "http://localhost", true), 400)
	before := mediaRetrySnapshotRows(t, a)
	for i := 0; i < 2; i++ {
		ok(t, b.request("POST", path, nil, "http://localhost", true), 200)
	}
	for i, item := range mediaRetrySnapshotRows(t, a) {
		want := before[i]
		if want.ID == "200" {
			want.Status = "ignored"
			want.Next = 0
		}
		if item != want {
			t.Fatal("manual action lost media metadata", item, want)
		}
	}
	jobs, err := a.store.MediaJobs("alice", true)
	if err != nil {
		t.Fatal(err)
	}
	for _, job := range jobs {
		if job.PostID == "200" {
			t.Fatal("verify would retry ignored media")
		}
	}
	// A refreshed source URL must not revoke an explicit ignore decision.
	if err := a.store.SavePosts("", []archive.Post{{ID: "200", Username: "alice", Text: "refreshed", Assets: []archive.Asset{{ID: "video", Kind: "video", URL: "https://video.twimg.com/refreshed.mp4"}}}}); err != nil {
		t.Fatal(err)
	}
	var status, source, lastError string
	if err := a.store.DB.QueryRow("SELECT status,source_url,last_error FROM media WHERE post_id='200'").Scan(&status, &source, &lastError); err != nil || status != "ignored" || !strings.Contains(source, "refreshed") || lastError != "连接被重置" {
		t.Fatal(status, source, lastError, err)
	}
	ok(t, b.request("POST", base+"/media/200/video/retry", nil, "http://localhost", true), 200)
	jobs, err = a.store.MediaJobs("alice", false)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, job := range jobs {
		if job.PostID == "200" {
			found = job.Status == "pending" && job.NextAttempt == 0
		}
	}
	if !found {
		t.Fatal("restored media is not runnable", jobs)
	}
	var historical string
	if err := a.store.DB.QueryRow("SELECT message FROM runs WHERE id=?", id).Scan(&historical); err != nil || historical != "历史记录：媒体下载返回 HTTP 403" {
		t.Fatal("history rewritten", historical, err)
	}
	ok(t, b.request("GET", base+"/errors?offset=-1", nil, "http://localhost", false), 400)
	ok(t, b.request("GET", "/api/runs/nope/errors", nil, "http://localhost", false), 400)
	ok(t, b.request("GET", "/api/runs/999999/errors", nil, "http://localhost", false), 404)
}

func TestRunMediaActionsBlockDuringCollectionAndMaintenance(t *testing.T) {
	a := testApp(t)
	id := seedErrorRun(t, a)
	b := login(t, a)
	path := fmt.Sprintf("/api/runs/%d/media/200/video/ignore", id)
	a.scheduler.mu.Lock()
	a.scheduler.active = true
	a.scheduler.mu.Unlock()
	ok(t, b.request("POST", path, nil, "http://localhost", true), http.StatusConflict)
	a.scheduler.mu.Lock()
	a.scheduler.active = false
	a.scheduler.mu.Unlock()
	a.maintenance.Store(true)
	ok(t, b.request("POST", path, nil, "http://localhost", true), http.StatusServiceUnavailable)
	var status string
	if err := a.store.DB.QueryRow("SELECT status FROM media WHERE post_id='200'").Scan(&status); err != nil || status != "failed" {
		t.Fatal(status, err)
	}
}

func TestIgnoredMediaDoesNotFailFutureRunsOrCountAsSaved(t *testing.T) {
	a := testApp(t)
	seedMonitor(t, a, "alice")
	notificationFixture(t, a)
	p := archive.Post{ID: "100", Username: "alice", Assets: []archive.Asset{{ID: "video", Kind: "video", URL: "https://video.twimg.com/fixture.mp4"}}}
	if err := a.store.SavePosts("alice", []archive.Post{p}); err != nil {
		t.Fatal(err)
	}
	if _, err := a.store.DB.Exec("UPDATE media SET status='failed',next_attempt=?,last_error='HTTP 403'", time.Now().Add(time.Hour).Unix()); err != nil {
		t.Fatal(err)
	}
	var targets []string
	a.scheduler.collect = recordTargets(&targets)
	a.scheduler.download = archive.DownloadPending
	runCollection(t, a)
	if err := a.store.SetMediaIgnored("alice", "100", "video", true); err != nil {
		t.Fatal(err)
	}
	if n, err := a.store.OutstandingMedia("alice"); err != nil || n != 0 {
		t.Fatal(n, err)
	}
	if done, total, err := a.store.TargetMediaCounts("alice"); err != nil || done != 0 || total != 1 {
		t.Fatal("ignored media counted as saved", done, total, err)
	}
	runCollection(t, a)
	var status, message string
	if err := a.store.DB.QueryRow("SELECT status,message FROM runs ORDER BY id DESC LIMIT 1").Scan(&status, &message); err != nil || status != "bounded_complete" || !strings.Contains(message, "1 个媒体已手动忽略（未下载）") {
		t.Fatal(status, message, err)
	}
	all := events(t, a)
	if len(all) != 2 || all[1].Type != "recovered" || strings.Contains(all[1].Message, "媒体保存") {
		t.Fatal("recovery falsely claims media saved", all)
	}
}
