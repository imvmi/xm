package web

import (
	"encoding/json"
	"net/http"
	"reflect"
	"testing"
	"time"

	"xm/internal/archive"
)

func seedMediaRetryState(t *testing.T, a *App) {
	t.Helper()
	post := func(id string) archive.Post {
		return archive.Post{ID: id, Username: "alice", Assets: []archive.Asset{{ID: "video", Kind: "video", URL: "https://video.twimg.com/fixture.mp4"}}}
	}
	p, quote := post("100"), post("200")
	p.Quote = &quote
	if err := a.store.SavePosts("alice", []archive.Post{p, post("400"), post("500"), post("600"), post("700")}); err != nil {
		t.Fatal(err)
	}
	if err := a.store.SavePosts("bob", []archive.Post{post("300")}); err != nil {
		t.Fatal(err)
	}
	if _, err := a.store.DB.Exec("UPDATE media SET status='failed',attempts=4,next_attempt=?,last_error='连接被重置'", time.Now().Add(time.Hour).Unix()); err != nil {
		t.Fatal(err)
	}
	for id, status := range map[string]string{"400": "permanent", "500": "done", "600": "pending"} {
		if _, err := a.store.DB.Exec("UPDATE media SET status=? WHERE post_id=?", status, id); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := a.store.DB.Exec("UPDATE media SET next_attempt=1 WHERE post_id='700'"); err != nil {
		t.Fatal(err)
	}
}

type mediaRetrySnapshot struct {
	ID, Status, Error, URL, Updated string
	Attempts                        int
	Next                            int64
}

func mediaRetrySnapshotRows(t *testing.T, a *App) []mediaRetrySnapshot {
	t.Helper()
	rows, err := a.store.DB.Query("SELECT post_id,status,last_error,source_url,updated_at,attempts,next_attempt FROM media ORDER BY post_id")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var result []mediaRetrySnapshot
	for rows.Next() {
		var item mediaRetrySnapshot
		if err := rows.Scan(&item.ID, &item.Status, &item.Error, &item.URL, &item.Updated, &item.Attempts, &item.Next); err != nil {
			t.Fatal(err)
		}
		result = append(result, item)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return result
}

func TestResetMediaRetryOnlyReleasesDeferredFailures(t *testing.T) {
	a := testApp(t)
	seedMediaRetryState(t, a)
	before := mediaRetrySnapshotRows(t, a)
	cfg := a.Config()
	cfg.PollingPaused = true
	if err := a.saveConfig(cfg); err != nil {
		t.Fatal(err)
	}
	if err := setState(a.store.DB, "cooldown_until", "9999999999"); err != nil {
		t.Fatal(err)
	}
	b := login(t, a)
	unauth := &browserClient{t: t, a: a}
	const path = "/api/media/retry/reset"
	ok(t, unauth.request("POST", path, nil, "http://localhost", false), http.StatusUnauthorized)
	ok(t, b.request("POST", path, nil, "http://localhost", false), http.StatusForbidden)
	ok(t, b.request("POST", path, nil, "https://elsewhere.invalid", true), http.StatusForbidden)
	ok(t, b.request("GET", path, nil, "http://localhost", true), http.StatusMethodNotAllowed)
	if !reflect.DeepEqual(before, mediaRetrySnapshotRows(t, a)) {
		t.Fatal("unauthorized reset changed media")
	}
	for _, expectedCount := range []int{3, 0} {
		response := b.request("POST", path, nil, "http://localhost", true)
		ok(t, response, http.StatusOK)
		var body struct{ Cleared int }
		if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil || body.Cleared != expectedCount {
			t.Fatal(response.Body.String(), err)
		}
	}
	for i, got := range mediaRetrySnapshotRows(t, a) {
		want := before[i]
		if want.ID == "100" || want.ID == "200" || want.ID == "300" {
			want.Next = 0
		}
		if got != want {
			t.Fatal("reset changed unrelated media state", got, want)
		}
	}
	jobs, err := a.store.MediaJobs("alice", false)
	if err != nil || len(jobs) != 4 {
		t.Fatal("reset did not make parent and quoted media runnable", jobs, err)
	}
	if !reflect.DeepEqual(cfg, a.Config()) || state(a.store.DB, "cooldown_until") != "9999999999" {
		t.Fatal("reset changed pause, configuration, or rate limits")
	}
	for _, table := range []string{"runs", "collection_jobs"} {
		var count int
		if err := a.store.DB.QueryRow("SELECT COUNT(*) FROM " + table).Scan(&count); err != nil || count != 0 {
			t.Fatal("reset scheduled collection", table, count, err)
		}
	}
}

func TestResetMediaRetryRejectsActiveBatchAndMaintenance(t *testing.T) {
	a := testApp(t)
	seedMediaRetryState(t, a)
	b := login(t, a)
	before := mediaRetrySnapshotRows(t, a)
	a.scheduler.mu.Lock()
	a.scheduler.active = true
	a.scheduler.mu.Unlock()
	ok(t, b.request("POST", "/api/media/retry/reset", nil, "http://localhost", true), http.StatusConflict)
	a.scheduler.mu.Lock()
	a.scheduler.active = false
	a.scheduler.mu.Unlock()
	a.maintenance.Store(true)
	ok(t, b.request("POST", "/api/media/retry/reset", nil, "http://localhost", true), http.StatusServiceUnavailable)
	if !reflect.DeepEqual(before, mediaRetrySnapshotRows(t, a)) {
		t.Fatal("blocked reset changed media")
	}
}

func TestResetMediaRetryAtomicOnFailure(t *testing.T) {
	a := testApp(t)
	if a.store.DB.Dialect != "sqlite" {
		t.Skip("SQLite fault injection")
	}
	seedMediaRetryState(t, a)
	b := login(t, a)
	before := mediaRetrySnapshotRows(t, a)
	if _, err := a.store.DB.Exec(`CREATE TRIGGER fail_media_reset BEFORE UPDATE OF next_attempt ON media WHEN NEW.post_id='300' BEGIN SELECT RAISE(ABORT,'injected reset failure'); END`); err != nil {
		t.Fatal(err)
	}
	ok(t, b.request("POST", "/api/media/retry/reset", nil, "http://localhost", true), http.StatusInternalServerError)
	if !reflect.DeepEqual(before, mediaRetrySnapshotRows(t, a)) {
		t.Fatal("partially reset media after database failure")
	}
}
