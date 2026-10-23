package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"
	"xm/internal/archive"
)

func TestTimelineSectionsAndPagination(t *testing.T) {
	a := testApp(t)
	b := login(t, a)
	quote := archive.Post{ID: "50", Username: "bob", ReplyTo: "49", Text: "引用搜索词", Raw: json.RawMessage(`{}`), Assets: []archive.Asset{{ID: "v", Kind: "video", URL: "https://video.twimg.com/test.mp4"}}}
	posts := []archive.Post{{ID: "100", Username: "alice", Text: "普通推文", Raw: json.RawMessage(`{}`), Quote: &quote}}
	for i := 0; i < 30; i++ {
		posts = append(posts, archive.Post{ID: fmt.Sprint(200 + i), Username: "alice", ReplyTo: "40", Text: "回复内容", Raw: json.RawMessage(`{}`)})
	}
	posts[1].Quote = &quote
	if err := a.store.SavePosts("alice", posts); err != nil {
		t.Fatal(err)
	}
	if err := a.store.SavePosts("other", []archive.Post{{ID: "999", Username: "other", ReplyTo: "40", Raw: json.RawMessage(`{}`)}}); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		query        string
		total, items int
		first        string
	}{
		{"", 31, 24, "229"},
		{"&section=posts", 1, 1, "100"},
		{"&section=replies", 30, 24, "229"},
		{"&section=replies&offset=24", 30, 6, "205"},
		{"&section=replies&offset=30", 30, 0, ""},
		{"&section=replies&q=引用搜索词", 1, 1, "200"},
		{"&section=posts&kind=video", 1, 1, "100"},
		{"&section=media", 2, 2, "200"},
		{"&section=media&kind=text", 0, 0, ""},
	} {
		t.Run(tc.query, func(t *testing.T) {
			w := b.request("GET", "/api/posts?target=alice"+tc.query, nil, "", false)
			ok(t, w, 200)
			var got PostList
			if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
				t.Fatal(err)
			}
			if got.Total != tc.total || len(got.Items) != tc.items {
				t.Fatalf("got total=%d items=%d; want %d/%d", got.Total, len(got.Items), tc.total, tc.items)
			}
			if tc.first != "" && got.Items[0].ID != tc.first {
				t.Fatalf("first ID=%s, want %s", got.Items[0].ID, tc.first)
			}
			uncounted := b.request("GET", "/api/posts?target=alice"+tc.query+"&count=0", nil, "", false)
			ok(t, uncounted, 200)
			var fast PostList
			if err := json.Unmarshal(uncounted.Body.Bytes(), &fast); err != nil {
				t.Fatal(err)
			}
			if fast.Total != -1 || !reflect.DeepEqual(fast.Items, got.Items) || fast.HasMore != got.HasMore || fast.NextCursor != got.NextCursor {
				t.Fatalf("count=0 changed page contents or pagination: %+v", fast)
			}
		})
	}
	for _, query := range []string{"", "&q=引用搜索词", "&kind=video", "&section=media"} {
		w := b.request("GET", "/api/posts?"+query, nil, "", false)
		ok(t, w, 200)
		var got PostList
		if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
			t.Fatal(err)
		}
		if got.Total != 1 || len(got.Items) != 1 || got.Items[0].ID != "100" || got.Items[0].Quote == nil || got.Items[0].Quote.ReplyTo != "49" {
			t.Fatalf("home feed must exclude reply roots while preserving quotes (%s): %+v", query, got)
		}
	}
	ok(t, b.request("GET", "/api/posts?section=unknown", nil, "", false), 400)
}

func TestTimelineCursorAcrossAccountsAndConcurrentInserts(t *testing.T) {
	a := testApp(t)
	quote := archive.Post{ID: "8", Username: "quoted", Text: "quote needle", Raw: json.RawMessage(`{}`)}
	for _, target := range []string{"alice", "bob"} {
		if err := a.store.SavePosts(target, []archive.Post{
			{ID: "101", Username: "alice", Raw: json.RawMessage(`{}`), Quote: &quote},
			{ID: "99", Username: "bob", Raw: json.RawMessage(`{}`)},
			{ID: "9", Username: "alice", Raw: json.RawMessage(`{}`)},
		}); err != nil {
			t.Fatal(err)
		}
	}
	first, err := listPostsPage(context.Background(), a.store.DB, "", "", "", "", 0, 2, "", true)
	if err != nil || first.Total != 3 || len(first.Items) != 2 || first.Items[0].ID != "101" || first.NextCursor != "99" || !first.HasMore {
		t.Fatalf("first page: %+v %v", first, err)
	}
	if first.Items[0].Quote == nil || first.Items[0].ArchiveTarget != "alice" {
		t.Fatal("quote or archive link missing")
	}
	if err := a.store.SavePosts("carol", []archive.Post{{ID: "102", Username: "carol", Raw: json.RawMessage(`{}`)}}); err != nil {
		t.Fatal(err)
	}
	for _, target := range []string{"", "alice"} {
		next, err := listPostsPage(context.Background(), a.store.DB, target, "", "", "", 0, 2, first.NextCursor, false)
		if err != nil || next.Total != -1 || len(next.Items) != 1 || next.Items[0].ID != "9" || next.HasMore {
			t.Fatalf("cursor page: %+v %v", next, err)
		}
	}
	filtered, err := listPostsPage(context.Background(), a.store.DB, "alice", "needle", "", "", 0, 2, "", true)
	if err != nil || filtered.Total != 1 || filtered.Items[0].ID != "101" {
		t.Fatalf("quote search: %+v %v", filtered, err)
	}
	users, err := archiveUsersFor(a.store.DB, "bob")
	if err != nil || len(users) != 1 || users[0].Username != "bob" {
		t.Fatalf("single profile: %+v %v", users, err)
	}
	b := login(t, a)
	ok(t, b.request("GET", "/api/posts?before=bad-cursor", nil, "", false), 400)
}

func TestLargeTimelineUsesOrderedIndexes(t *testing.T) {
	a := testApp(t)
	if a.store.DB.Dialect != "sqlite" {
		t.Skip("SQLite query-plan regression")
	}
	// The opt-in larger fixture also includes 2 KiB of JSON per post to model
	// archives whose rows no longer fit in SQLite's page cache.
	rowCount, raw := 30000, "{}"
	if value := os.Getenv("XM_TIMELINE_PERF_ROWS"); value != "" {
		var err error
		rowCount, err = strconv.Atoi(value)
		if err != nil || rowCount < 30000 || rowCount > 1000000 {
			t.Fatal("XM_TIMELINE_PERF_ROWS must be between 30000 and 1000000")
		}
		raw = `{"fixture":"` + strings.Repeat("x", 2048) + `"}`
	}
	// Populate an isolated archive without touching real data.
	tx, err := a.store.DB.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	_, err = tx.Exec(`WITH RECURSIVE n(x) AS (SELECT 1 UNION ALL SELECT x+1 FROM n WHERE x<?)
 INSERT INTO posts(id,author_id,username,text,created_at,url,reply_to,quote_id,repost_id,raw_json,first_seen,last_seen) SELECT CAST(100000+x AS TEXT),'42','alice','fixture','2026-10-19T00:00:00Z','','','','',?,'2026-10-19T00:00:00Z','2026-10-19T00:00:00Z' FROM n`, rowCount, raw)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec("INSERT INTO target_posts SELECT 'alice',id FROM posts"); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ query, index string }{
		{"SELECT p.id FROM posts p WHERE EXISTS (SELECT 1 FROM target_posts t LEFT JOIN monitors m ON m.username=t.target WHERE t.post_id=p.id AND COALESCE(m.show_in_timeline,1)=1) AND " + rootPostSQL + " ORDER BY length(p.id) DESC,p.id DESC LIMIT 25", "posts_timeline"},
		{"SELECT p.id FROM target_posts t JOIN posts p ON p.id=t.post_id WHERE t.target='alice' AND " + rootPostSQL + " ORDER BY length(t.post_id) DESC,t.post_id DESC LIMIT 25", "target_posts_timeline"},
	} {
		rows, err := a.store.DB.Query("EXPLAIN QUERY PLAN " + tc.query)
		if err != nil {
			t.Fatal(err)
		}
		plan := ""
		for rows.Next() {
			var id, parent, unused int
			var detail string
			if err = rows.Scan(&id, &parent, &unused, &detail); err != nil {
				t.Fatal(err)
			}
			plan += detail + "\n"
		}
		rows.Close()
		if !strings.Contains(plan, tc.index) || strings.Contains(plan, "TEMP B-TREE FOR ORDER BY") {
			t.Fatalf("unindexed sort: %s", plan)
		}
	}
	for _, target := range []string{"", "alice"} {
		start := time.Now()
		page, err := listPostsPage(context.Background(), a.store.DB, target, "", "", "", 0, 24, "", true)
		if err != nil || len(page.Items) != 24 || page.Total != rowCount {
			t.Fatalf("large page: %v", err)
		}
		t.Logf("%d rows target=%q first page + count: %s", rowCount, target, time.Since(start))
		counted := page
		start = time.Now()
		page, err = listPostsPage(context.Background(), a.store.DB, target, "", "", "", 0, 24, "", false)
		if err != nil || page.Total != -1 || !reflect.DeepEqual(page.Items, counted.Items) || page.HasMore != counted.HasMore || page.NextCursor != counted.NextCursor {
			t.Fatalf("uncounted first page differs: %+v %v", page, err)
		}
		t.Logf("%d rows target=%q first page without count: %s", rowCount, target, time.Since(start))
		start = time.Now()
		page, err = listPostsPage(context.Background(), a.store.DB, target, "", "", "", 0, 24, "100100", false)
		if err != nil || len(page.Items) != 24 || page.Items[0].ID != "100099" {
			t.Fatalf("deep cursor: %+v %v", page, err)
		}
		t.Logf("%d rows target=%q deep cursor: %s", rowCount, target, time.Since(start))
	}
	// Search and media filters must retain ordered seeks. Every root has media,
	// so first/deep pages should only inspect enough candidates to fill the page.
	if _, err = a.store.DB.Exec("INSERT INTO media(post_id,media_id,kind,source_url,updated_at) SELECT id,'photo','photo','','' FROM posts"); err != nil {
		t.Fatal(err)
	}
	if _, err = a.store.DB.Exec("INSERT INTO post_bookmarks(post_id,created_at) SELECT id,100 FROM posts"); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ query, kind, section string }{
		{"fixture", "", ""}, {"", "photo", ""}, {"fixture", "photo", "media"}, {"fixture", "photo", "bookmarks"},
	} {
		for _, target := range []string{"", "alice"} {
			for _, cursor := range []string{"", "100100"} {
				before := cursor
				if tc.section == "bookmarks" && cursor != "" {
					before = "b:100:" + cursor
				}
				start := time.Now()
				page, err := listPostsPage(context.Background(), a.store.DB, target, tc.query, tc.kind, tc.section, 0, 24, before, false)
				if err != nil || len(page.Items) != 24 || (cursor != "" && page.Items[0].ID != "100099") {
					t.Fatalf("filtered page %+v target=%s before=%s: %+v %v", tc, target, before, page, err)
				}
				t.Logf("%d rows filtered=%+v target=%q cursor=%q: %s", rowCount, tc, target, before, time.Since(start))
			}
		}
	}
	filter, args := postFilter("fixture", "photo", false)
	for _, tc := range []struct{ query, index string }{
		{"SELECT p.id FROM posts p WHERE " + rootPostSQL + filter + " ORDER BY length(p.id) DESC,p.id DESC LIMIT 25", "posts_timeline"},
		{"SELECT p.id FROM target_posts t JOIN posts p ON p.id=t.post_id WHERE t.target='alice'" + filter + " ORDER BY length(t.post_id) DESC,t.post_id DESC LIMIT 25", "target_posts_timeline"},
		{"SELECT p.id FROM post_bookmarks b JOIN posts p ON p.id=b.post_id WHERE b.created_at=100 AND b.post_id<'100100'" + filter + " ORDER BY b.post_id DESC LIMIT 25", "bookmarks_recent"},
		{"SELECT p.id FROM posts p WHERE " + rootPostSQL + filter + " AND length(p.id)=6 AND p.id<'100100' ORDER BY p.id DESC LIMIT 25", "posts_timeline"},
		{"SELECT p.id FROM target_posts t JOIN posts p ON p.id=t.post_id WHERE t.target='alice'" + filter + " AND length(t.post_id)=6 AND t.post_id<'100100' ORDER BY t.post_id DESC LIMIT 25", "target_posts_timeline"},
		{"SELECT p.id FROM post_bookmarks b JOIN posts p ON p.id=b.post_id WHERE 1=1" + filter + " ORDER BY b.created_at DESC,b.post_id DESC LIMIT 25", "bookmarks_recent"},
	} {
		rows, err := a.store.DB.Query("EXPLAIN QUERY PLAN "+tc.query, args...)
		if err != nil {
			t.Fatal(err)
		}
		plan := ""
		for rows.Next() {
			var id, parent, unused int
			var detail string
			if err = rows.Scan(&id, &parent, &unused, &detail); err != nil {
				t.Fatal(err)
			}
			plan += detail + "\n"
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(plan, tc.index) || strings.Contains(plan, "TEMP B-TREE FOR ORDER BY") || strings.Contains(plan, "AUTOMATIC") || !strings.Contains(plan, "CORRELATED") {
			t.Fatalf("filtered query lost candidate/index evaluation: %s", plan)
		}
	}

	// Interrupt an executing full-range search as well as pool waits (covered
	// in read_timeout_test). The connection must be reusable afterwards.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := listPostsPage(ctx, a.store.DB, "", "absent search term", "photo", "", 0, 24, "", true)
		done <- err
	}()
	pool := a.store.DB.Reader
	deadline := time.Now().Add(2 * time.Second)
	for pool.Stats().InUse == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if pool.Stats().InUse == 0 {
		t.Fatal("search did not acquire connection")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("running search cancellation: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("running search ignored cancellation")
	}
	if err := pool.PingContext(context.Background()); err != nil {
		t.Fatal("canceled query left connection unusable", err)
	}

}
