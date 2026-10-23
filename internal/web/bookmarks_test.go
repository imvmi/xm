package web

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"xm/internal/archive"
)

func putTestBookmark(t *testing.T, a *App, id string, at int64) {
	t.Helper()
	if _, err := a.store.DB.Exec("INSERT INTO post_bookmarks(post_id,created_at) VALUES(?,?)", id, at); err != nil {
		t.Fatal(err)
	}
}

func TestBookmarkAPIAndPersistence(t *testing.T) {
	a := testApp(t)
	b := login(t, a)
	p := archive.Post{ID: "100", Username: "alice", Text: "saved"}
	if err := a.store.SavePosts("alice", []archive.Post{p}); err != nil {
		t.Fatal(err)
	}
	path := "/api/posts/100/bookmark"
	input := map[string]bool{"bookmarked": true}
	w := httptest.NewRecorder()
	r := httptest.NewRequest("PUT", "http://localhost"+path, strings.NewReader(`{"bookmarked":true}`))
	r.Header.Set("Origin", "http://localhost")
	a.Handler().ServeHTTP(w, r)
	ok(t, w, 401)
	ok(t, b.request("PUT", path, input, "http://evil.example", true), 403)
	ok(t, b.request("PUT", path, input, "http://localhost", false), 403)
	ok(t, b.request("PUT", path, map[string]bool{}, "http://localhost", true), 400)
	ok(t, b.request("PUT", "/api/posts/bad/bookmark", input, "http://localhost", true), 400)
	ok(t, b.request("PUT", "/api/posts/999/bookmark", input, "http://localhost", true), 404)
	a.maintenance.Store(true)
	ok(t, b.request("PUT", path, input, "http://localhost", true), 503)
	a.maintenance.Store(false)
	response := b.request("PUT", path, input, "http://localhost", true)
	ok(t, response, 200)
	var first struct {
		Bookmarked bool  `json:"bookmarked"`
		At         int64 `json:"bookmarked_at"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &first); err != nil || !first.Bookmarked || first.At <= 0 {
		t.Fatal("invalid bookmark response", err, first)
	}
	ok(t, b.request("PUT", path, input, "http://localhost", true), 200)
	p.Text = "collected again"
	if err := a.store.SavePosts("alice", []archive.Post{p}); err != nil {
		t.Fatal(err)
	}
	if err := a.store.SavePosts("other", []archive.Post{p}); err != nil {
		t.Fatal(err)
	}
	var at int64
	if err := a.store.DB.QueryRow("SELECT created_at FROM post_bookmarks WHERE post_id='100'").Scan(&at); err != nil || at != first.At {
		t.Fatal("bookmark overwritten on repeat save", err)
	}
	page, err := listPosts(context.Background(), a.store.DB, "alice", "", "", "posts", 0, 10)
	if err != nil || len(page.Items) != 1 || !page.Items[0].Bookmarked {
		t.Fatal("normal timeline lacks bookmark state", err)
	}
	// A new App/connection sees the same durable bookmark.
	store, err := archive.OpenConfiguredStore(a.Config())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	page, err = listPosts(context.Background(), store.DB, "", "", "", "bookmarks", 0, 10)
	if err != nil || page.Total != 1 || page.Items[0].BookmarkedAt != first.At {
		t.Fatal("bookmark not persistent or duplicated", err, page)
	}
	for i := 0; i < 2; i++ {
		ok(t, b.request("PUT", path, map[string]bool{"bookmarked": false}, "http://localhost", true), 200)
	}
	page, err = listPosts(context.Background(), a.store.DB, "", "", "", "bookmarks", 0, 10)
	if err != nil || page.Total != 0 {
		t.Fatal("unbookmark failed", err)
	}
	var posts int
	if err = a.store.DB.QueryRow("SELECT COUNT(*) FROM posts").Scan(&posts); err != nil || posts != 1 {
		t.Fatal("unbookmark deleted archived post", err)
	}
}

func TestBookmarkScopesQuotesAndFilters(t *testing.T) {
	a := testApp(t)
	quote := archive.Post{ID: "50", Username: "bob", Text: "quote needle", Assets: []archive.Asset{{ID: "photo", Kind: "photo"}}}
	if err := a.store.SavePosts("alice", []archive.Post{{ID: "100", Username: "alice", Quote: &quote}, {ID: "101", Username: "alice", ReplyTo: "1", Text: "reply"}}); err != nil {
		t.Fatal(err)
	}
	if err := a.store.SavePosts("carol", []archive.Post{{ID: "200", Username: "carol", Text: "other"}}); err != nil {
		t.Fatal(err)
	}
	seedMonitor(t, a, "alice")
	if _, err := a.store.DB.Exec("UPDATE monitors SET show_in_timeline=0 WHERE username='alice'"); err != nil {
		t.Fatal(err)
	}
	for i, id := range []string{"100", "50", "200", "101"} {
		putTestBookmark(t, a, id, int64(i+1))
	}
	for _, tc := range []struct{ target, query, kind, section, want string }{
		{"", "", "", "", "200"},
		{"", "", "", "bookmarks", "101,200,50,100"},
		{"alice", "", "", "bookmarks", "101,50,100"},
		{"carol", "", "", "bookmarks", "200"},
		{"unknown", "", "", "bookmarks", ""},
		{"alice", "needle", "", "bookmarks", "50,100"},
		{"", "needle", "photo", "bookmarks", "50,100"},
		{"alice", "", "text", "bookmarks", "101"},
		{"alice", "", "video", "bookmarks", ""},
	} {
		page, err := listPosts(context.Background(), a.store.DB, tc.target, tc.query, tc.kind, tc.section, 0, 20)
		if err != nil {
			t.Fatal(err)
		}
		ids := []string{}
		for _, p := range page.Items {
			ids = append(ids, p.ID)
			if tc.section == "bookmarks" && !p.Bookmarked {
				t.Fatal("missing bookmark flag")
			}
			if p.Quote != nil && !p.Quote.Bookmarked {
				t.Fatal("quote missing bookmark flag")
			}
		}
		if strings.Join(ids, ",") != tc.want || page.Total != len(ids) {
			t.Fatalf("%+v: %+v", tc, page)
		}
	}
	if _, err := a.store.DB.Exec("DELETE FROM monitors WHERE username='alice'"); err != nil {
		t.Fatal(err)
	}
	page, err := listPosts(context.Background(), a.store.DB, "alice", "", "", "bookmarks", 0, 20)
	if err != nil || page.Total != 3 {
		t.Fatal("removing monitor lost bookmarks", err)
	}
}

func TestBookmarkCursorSurvivesChanges(t *testing.T) {
	a := testApp(t)
	b := login(t, a)
	for _, id := range []string{"9", "10", "11", "99", "100"} {
		if err := a.store.SavePosts("alice", []archive.Post{{ID: id}}); err != nil {
			t.Fatal(err)
		}
	}
	for _, id := range []string{"9", "10", "11", "99"} {
		putTestBookmark(t, a, id, 100)
	}
	first, err := listPostsPage(context.Background(), a.store.DB, "alice", "", "", "bookmarks", 0, 2, "", true)
	if err != nil || first.Total != 4 || len(first.Items) != 2 || first.Items[0].ID != "99" || first.Items[1].ID != "9" || !first.HasMore {
		t.Fatal("bad first page", err, first)
	}
	ok(t, b.request("PUT", "/api/posts/9/bookmark", map[string]bool{"bookmarked": false}, "http://localhost", true), 200)
	putTestBookmark(t, a, "100", 200)
	page, err := listPostsPage(context.Background(), a.store.DB, "alice", "", "", "bookmarks", 0, 2, first.NextCursor, false)
	if err != nil || len(page.Items) != 2 || page.Items[0].ID != "11" || page.Items[1].ID != "10" || page.HasMore || page.Total != -1 {
		t.Fatal("cursor skipped or repeated", err, page)
	}
	ok(t, b.request("GET", "/api/posts?section=bookmarks&before="+url.QueryEscape(first.NextCursor), nil, "", false), 200)
	for _, cursor := range []string{"9", "b:0:9", "b:-1:9", "b:1:bad", "b:01:9", "b:9223372036854775808:9", "b:1:9:extra"} {
		ok(t, b.request("GET", "/api/posts?section=bookmarks&before="+url.QueryEscape(cursor), nil, "", false), 400)
	}
	ok(t, b.request("GET", "/api/posts?before="+url.QueryEscape(first.NextCursor), nil, "", false), 400)
}

func TestBookmarkOrderedIndex(t *testing.T) {
	a := testApp(t)
	if a.store.DB.Dialect != "sqlite" {
		t.Skip("SQLite plan regression")
	}
	rows, err := a.store.DB.Query("EXPLAIN QUERY PLAN SELECT p.id FROM post_bookmarks b JOIN posts p ON p.id=b.post_id WHERE b.created_at<100 ORDER BY b.created_at DESC,b.post_id DESC LIMIT 25")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	plan := ""
	for rows.Next() {
		var id, parent, unused int
		var detail string
		if err = rows.Scan(&id, &parent, &unused, &detail); err != nil {
			t.Fatal(err)
		}
		plan += fmt.Sprintln(detail)
	}
	if err = rows.Err(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(plan, "post_bookmarks_recent") || strings.Contains(plan, "TEMP B-TREE") {
		t.Fatal("unindexed bookmark order", plan)
	}
}
