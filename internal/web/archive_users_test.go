package web

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"xm/internal/archive"
)

func TestArchiveDirectoryAndStatistics(t *testing.T) {
	a := testApp(t)
	seedMonitor(t, a, "empty")
	quote := archive.Post{ID: "50", Username: "quoted", Assets: []archive.Asset{{ID: "shared", Kind: "photo"}}}
	posts := []archive.Post{
		{ID: "100", AuthorID: "42", Username: "alice", Quote: &quote, Assets: []archive.Asset{{ID: "root", Kind: "photo"}}},
		{ID: "101", AuthorID: "42", Username: "alice", Quote: &quote, Raw: json.RawMessage(`{"core":{"user_results":{"result":{"core":{"name":"Alice"},"legacy":{"description":"bio"}}}}}`)},
		{ID: "102", AuthorID: "42", Username: "alice", QuoteID: "missing", Assets: []archive.Asset{{ID: "pending", Kind: "photo"}}},
	}
	if err := a.store.SavePosts("alice", posts); err != nil {
		t.Fatal(err)
	}
	if err := a.store.SavePosts("bob", []archive.Post{{ID: "200", Username: "bob", Quote: &quote}}); err != nil {
		t.Fatal(err)
	}
	if err := a.store.SaveTargetProfile("alice", archive.Profile{ID: "42", Username: "renamed", Protected: true}); err != nil {
		t.Fatal(err)
	}
	// A shared quote cycles back to a root. It must still be counted once.
	for _, q := range []string{"UPDATE posts SET quote_id='100' WHERE id='50'", "UPDATE media SET status='done' WHERE post_id IN ('50','100')"} {
		if _, err := a.store.DB.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	users, err := archiveUserDirectory(a.store.DB, "")
	if err != nil || len(users) != 3 {
		t.Fatalf("directory: %+v %v", users, err)
	}
	if u := users[0]; u.Username != "alice" || u.CurrentUsername != "renamed" || u.UserID != "42" || !u.Protected || u.Monitored || !u.StatsPending || u.Posts != 0 || u.Media != 0 {
		t.Fatalf("light directory: %+v", u)
	}
	stats, err := archiveUserStats(a.store.DB, "alice")
	if err != nil || len(stats) != 1 || stats[0].Posts != 3 || stats[0].Media != 2 || stats[0].LastSaved == "" {
		t.Fatalf("deduplicated statistics: %+v %v", stats, err)
	}
	full, err := archiveUsersFor(a.store.DB, "alice")
	if err != nil || len(full) != 1 || full[0].StatsPending || full[0].Posts != 3 || full[0].Media != 2 {
		t.Fatalf("legacy full directory: %+v %v", full, err)
	}
	for _, target := range []string{"empty", "absent"} {
		directory, err := archiveUserDirectory(a.store.DB, target)
		if err != nil {
			t.Fatal(err)
		}
		want := 0
		if target == "empty" {
			want = 1
		}
		if len(directory) != want {
			t.Fatalf("target %s: %+v", target, directory)
		}
	}
}

func TestArchiveMediaCountUsesCoveringIndex(t *testing.T) {
	a := testApp(t)
	if a.store.DB.Dialect != "sqlite" {
		t.Skip("SQLite query plan")
	}
	rows, err := a.store.DB.Query("EXPLAIN QUERY PLAN "+archiveMediaCountSQL, "alice")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var plan strings.Builder
	for rows.Next() {
		var id, parent, unused int
		var detail string
		if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
			t.Fatal(err)
		}
		plan.WriteString(detail + "\n")
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(plan.String(), "USING COVERING INDEX media_post_status (post_id=? AND status=?)") || strings.Contains(plan.String(), "media_status_bytes") {
		t.Fatal("media lookup must be bounded by related post ID:", plan.String())
	}
}

func TestArchiveStatsAPIRefreshAndInvalidation(t *testing.T) {
	a := testApp(t)
	seedMonitor(t, a, "alice")
	anon := &browserClient{t: t, a: a}
	ok(t, anon.request("GET", "/api/users/stats", nil, "", false), 401)
	b := login(t, a)
	read := func(path string) []ArchiveUserStats {
		t.Helper()
		w := b.request("GET", path, nil, "", false)
		ok(t, w, 200)
		var stats []ArchiveUserStats
		if err := json.Unmarshal(w.Body.Bytes(), &stats); err != nil {
			t.Fatal(err)
		}
		return stats
	}
	if got := read("/api/users/stats"); len(got) != 1 || got[0].Posts != 0 {
		t.Fatal(got)
	}
	if err := a.store.SavePosts("alice", []archive.Post{{ID: "1", Username: "alice"}}); err != nil {
		t.Fatal(err)
	}
	if got := read("/api/users/stats?target=alice"); len(got) != 1 || got[0].Posts != 0 {
		t.Fatal("expected cached counts", got)
	}
	if got := read("/api/users/stats?target=alice&refresh=1"); len(got) != 1 || got[0].Posts != 1 {
		t.Fatal("forced refresh", got)
	}
	if got := read("/api/users/stats?target=absent"); len(got) != 0 {
		t.Fatal("scope leak", got)
	}
	w := b.request("GET", "/api/users?stats=0&target=alice", nil, "", false)
	ok(t, w, 200)
	var directory []UserView
	if err := json.Unmarshal(w.Body.Bytes(), &directory); err != nil || len(directory) != 1 || !directory[0].StatsPending {
		t.Fatalf("light response: %s %v", w.Body.String(), err)
	}
	ok(t, b.request("POST", "/api/monitors", monitorInput{Username: "bob", Enabled: true}, "http://localhost", true), 201)
	if got := read("/api/users/stats"); len(got) != 2 {
		t.Fatal("monitor changes did not invalidate statistics", got)
	}
}

func TestStatsCacheLifecycle(t *testing.T) {
	var c statsCache[int]
	calls := 0
	load := func() (int, error) { calls++; return calls, nil }
	for i := 0; i < 3; i++ {
		if v, err := c.get(false, load); err != nil || v != 1 {
			t.Fatal(v, err)
		}
	}
	if v, _ := c.get(true, load); v != 2 {
		t.Fatal("force refresh", v)
	}
	c.expires = time.Now().Add(-time.Second)
	if v, _ := c.get(false, load); v != 3 {
		t.Fatal("expiration", v)
	}
	c.invalidate()
	if v, _ := c.get(false, load); v != 4 {
		t.Fatal("invalidation", v)
	}
}

func TestStatsCacheCoalescesMissesAndRetriesErrors(t *testing.T) {
	var c statsCache[int]
	var calls atomic.Int32
	var wg sync.WaitGroup
	for range 32 {
		wg.Go(func() {
			v, err := c.get(false, func() (int, error) { calls.Add(1); return 42, nil })
			if err != nil || v != 42 {
				t.Errorf("cache: %d %v", v, err)
			}
		})
	}
	wg.Wait()
	if calls.Load() != 1 {
		t.Fatal("duplicate loads", calls.Load())
	}
	c.invalidate()
	if _, err := c.get(false, func() (int, error) { return 0, errors.New("offline") }); err == nil {
		t.Fatal("missing load error")
	}
	if v, _ := c.get(false, func() (int, error) { return 43, nil }); v != 43 {
		t.Fatal("failed refresh was cached", v)
	}
}

func TestCollectionInvalidatesArchiveAndOverviewStats(t *testing.T) {
	a := testApp(t)
	seedMonitor(t, a, "alice")
	_, err := a.userStatsCache.get(false, func() ([]ArchiveUserStats, error) { return archiveUserStats(a.store.DB, "") })
	if err != nil {
		t.Fatal(err)
	}
	_, err = a.overviewStatsCache.get(false, func() (map[string]int64, error) { return counts(a.store.DB) })
	if err != nil {
		t.Fatal(err)
	}
	a.scheduler.collect = func(ctx context.Context, cfg archive.Config, cookies archive.Cookies, name string, identity archive.TargetIdentity, limit int, replies bool, save func(archive.Page) error) (archive.Collection, error) {
		err := save(archive.Page{Posts: []archive.Post{{ID: "100", Username: name}}})
		return archive.Collection{Posts: 1, Stop: "known_boundary_reached"}, err
	}
	if worked, err := a.scheduler.step(context.Background()); !worked || err != nil {
		t.Fatal(worked, err)
	}
	stats, err := a.userStatsCache.get(false, func() ([]ArchiveUserStats, error) { return archiveUserStats(a.store.DB, "") })
	if err != nil || len(stats) != 1 || stats[0].Posts != 1 {
		t.Fatal("collection left stale archive statistics", stats, err)
	}
	overview, err := a.overviewStatsCache.get(false, func() (map[string]int64, error) { return counts(a.store.DB) })
	if err != nil || overview["posts"] != 1 {
		t.Fatal("collection left stale overview statistics", overview, err)
	}
}
