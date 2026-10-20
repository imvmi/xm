package web

import (
	"encoding/json"
	"fmt"
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
		})
	}
	ok(t, b.request("GET", "/api/posts?section=unknown", nil, "", false), 400)
}

func TestTimelineCursorAcrossAccountsAndConcurrentInserts(t *testing.T) {
	a := testApp(t)
	quote := archive.Post{ID: "8", Username: "quoted", Text: "quote needle", Raw: json.RawMessage(`{}`)}
	for _, target := range []string{"alice", "bob"} {
		if err := a.store.SavePosts(target, []archive.Post{
			{ID: "101", Username: "alice", Raw: json.RawMessage(`{}`), Quote: &quote},
			{ID: "99", Username: "bob", ReplyTo: "5", Raw: json.RawMessage(`{}`)},
			{ID: "9", Username: "alice", Raw: json.RawMessage(`{}`)},
		}); err != nil {
			t.Fatal(err)
		}
	}
	first, err := listPostsPage(a.store.DB, "", "", "", "", 0, 2, "", true)
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
		next, err := listPostsPage(a.store.DB, target, "", "", "", 0, 2, first.NextCursor, false)
		if err != nil || next.Total != -1 || len(next.Items) != 1 || next.Items[0].ID != "9" || next.HasMore {
			t.Fatalf("cursor page: %+v %v", next, err)
		}
	}
	filtered, err := listPostsPage(a.store.DB, "alice", "needle", "", "", 0, 2, "", true)
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
	// Populate an isolated 30,000-row archive without touching real data.
	tx, err := a.store.DB.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	_, err = tx.Exec(`WITH RECURSIVE n(x) AS (SELECT 1 UNION ALL SELECT x+1 FROM n WHERE x<30000)
 INSERT INTO posts SELECT CAST(100000+x AS TEXT),'42','alice','fixture','2026-10-19T00:00:00Z','','','','','{}','2026-10-19T00:00:00Z','2026-10-19T00:00:00Z' FROM n`)
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
		{"SELECT p.id FROM posts p WHERE EXISTS (SELECT 1 FROM target_posts t LEFT JOIN monitors m ON m.username=t.target WHERE t.post_id=p.id AND COALESCE(m.show_in_timeline,1)=1) ORDER BY length(p.id) DESC,p.id DESC LIMIT 25", "posts_timeline"},
		{"SELECT p.id FROM target_posts t JOIN posts p ON p.id=t.post_id WHERE t.target='alice' ORDER BY length(t.post_id) DESC,t.post_id DESC LIMIT 25", "target_posts_timeline"},
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
		page, err := listPostsPage(a.store.DB, target, "", "", "", 0, 24, "", true)
		if err != nil || len(page.Items) != 24 || page.Total != 30000 {
			t.Fatalf("large page: %v", err)
		}
		t.Logf("30k rows target=%q first page + count: %s", target, time.Since(start))
		start = time.Now()
		page, err = listPostsPage(a.store.DB, target, "", "", "", 0, 24, "100100", false)
		if err != nil || len(page.Items) != 24 || page.Items[0].ID != "100099" {
			t.Fatalf("deep cursor: %+v %v", page, err)
		}
		t.Logf("30k rows target=%q deep cursor: %s", target, time.Since(start))
	}
}
