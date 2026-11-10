package web

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"xm/internal/archive"
)

func TestAccountFollowPersistenceAndIsolation(t *testing.T) {
	a := testApp(t)
	b := login(t, a)
	seedMonitor(t, a, "alice")
	if err := a.store.SavePosts("alice", []archive.Post{{ID: "100", Username: "alice", AuthorID: "42"}}); err != nil {
		t.Fatal(err)
	}
	path := "/api/users/alice/follow"
	input := map[string]bool{"followed": true}
	w := httptest.NewRecorder()
	request := httptest.NewRequest("PUT", "http://localhost"+path, strings.NewReader(`{"followed":true}`))
	request.Header.Set("Origin", "http://localhost")
	a.Handler().ServeHTTP(w, request)
	ok(t, w, 401)
	ok(t, b.request("PUT", path, input, "http://localhost", false), 403)
	ok(t, b.request("PUT", path, input, "http://evil.example", true), 403)
	ok(t, b.request("PUT", path, map[string]bool{}, "http://localhost", true), 400)
	ok(t, b.request("PUT", "/api/users/missing/follow", input, "http://localhost", true), 404)
	a.maintenance.Store(true)
	ok(t, b.request("PUT", path, input, "http://localhost", true), 503)
	a.maintenance.Store(false)
	if _, err := a.store.DB.Exec("UPDATE monitors SET enabled=0,show_in_timeline=0,next_run=123,requested=1 WHERE username='alice'"); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		ok(t, b.request("PUT", path, input, "http://localhost", true), 200)
	}
	var enabled, shown, requested bool
	var next int
	if err := a.store.DB.QueryRow("SELECT enabled,show_in_timeline,next_run,requested FROM monitors WHERE username='alice'").Scan(&enabled, &shown, &next, &requested); err != nil || enabled || shown || next != 123 || !requested {
		t.Fatal("follow changed collection or visibility", err)
	}
	if err := a.store.SaveTargetProfile("alice", archive.Profile{ID: "42", Username: "alice_new"}); err != nil {
		t.Fatal(err)
	}
	ok(t, b.request("DELETE", "/api/monitors/alice", nil, "http://localhost", true), 200)
	store, err := archive.OpenConfiguredStore(a.Config())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	users, err := archiveUserDirectory(store.DB, "alice")
	if err != nil || len(users) != 1 || !users[0].Followed || users[0].CurrentUsername != "alice_new" || users[0].Monitored || users[0].ShowInTimeline {
		t.Fatal("follow lost on rename, unmonitor or reopen", users, err)
	}
	w = b.request("GET", "/api/users?stats=0&target=alice", nil, "", false)
	ok(t, w, 200)
	var response []UserView
	if err = json.Unmarshal(w.Body.Bytes(), &response); err != nil || len(response) != 1 || !response[0].Followed {
		t.Fatal("directory missing follow flag", err)
	}
	dest, err := archive.OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer dest.Close()
	if err = migrate(dest); err != nil {
		t.Fatal(err)
	}
	if err = a.store.CopyDatabase(context.Background(), dest, nil); err != nil {
		t.Fatal(err)
	}
	users, err = archiveUserDirectory(dest.DB, "alice")
	if err != nil || len(users) != 1 || !users[0].Followed {
		t.Fatal("database migration lost follow", users, err)
	}
	for i := 0; i < 2; i++ {
		ok(t, b.request("PUT", path, map[string]bool{"followed": false}, "http://localhost", true), 200)
	}
	var posts, follows int
	a.store.DB.QueryRow("SELECT COUNT(*) FROM posts").Scan(&posts)
	a.store.DB.QueryRow("SELECT COUNT(*) FROM account_follows").Scan(&follows)
	if posts != 1 || follows != 0 {
		t.Fatal("unfollow damaged archive or retained follow", posts, follows)
	}
}

func TestFollowingUpgradePreservesExistingAccounts(t *testing.T) {
	a := testApp(t)
	seedMonitor(t, a, "alice")
	if _, err := a.store.DB.Exec("UPDATE monitors SET show_in_timeline=0,enabled=0 WHERE username='alice'"); err != nil {
		t.Fatal(err)
	}
	// Simulate a database created before local follows were introduced.
	if _, err := a.store.DB.Exec("DROP TABLE account_follows"); err != nil {
		t.Fatal(err)
	}
	store, err := archive.OpenConfiguredStore(a.Config())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	users, err := archiveUserDirectory(store.DB, "alice")
	if err != nil || len(users) != 1 || users[0].Followed || users[0].ShowInTimeline || users[0].Enabled {
		t.Fatal("upgrade changed existing preferences", users, err)
	}
}

func TestFollowingTimelineScopesAndPagination(t *testing.T) {
	a := testApp(t)
	b := login(t, a)
	quote := archive.Post{ID: "50", Username: "other", Text: "needle", Assets: []archive.Asset{{ID: "photo", Kind: "photo"}}}
	for target, posts := range map[string][]archive.Post{
		"alice":   {{ID: "100", Username: "alice", Quote: &quote}, {ID: "99", Username: "alice", Text: "older"}, {ID: "101", Username: "alice", ReplyTo: "100", Text: "reply"}},
		"bob":     {{ID: "200", Username: "bob"}, {ID: "100", Username: "alice", Quote: &quote}},
		"hidden":  {{ID: "300", Username: "hidden"}},
		"visible": {{ID: "300", Username: "hidden"}, {ID: "400", Username: "visible"}},
	} {
		if err := a.store.SavePosts(target, posts); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := a.store.DB.Exec("INSERT INTO account_preferences(target,show_in_timeline) VALUES('hidden',0)"); err != nil {
		t.Fatal(err)
	}
	page, err := listPosts(context.Background(), a.store.DB, "", "", "", "following", 0, 20)
	if err != nil || page.Total != 0 {
		t.Fatal("empty following feed", page, err)
	}
	for _, target := range []string{"alice", "bob", "hidden"} {
		ok(t, b.request("PUT", "/api/users/"+target+"/follow", map[string]bool{"followed": true}, "http://localhost", true), 200)
	}
	for _, tc := range []struct{ query, kind, want string }{{"", "", "200,100,99"}, {"needle", "photo", "100"}, {"", "video", ""}} {
		page, err = listPosts(context.Background(), a.store.DB, "", tc.query, tc.kind, "following", 0, 20)
		if err != nil {
			t.Fatal(err)
		}
		ids := []string{}
		for _, p := range page.Items {
			ids = append(ids, p.ID)
		}
		if strings.Join(ids, ",") != tc.want || page.Total != len(ids) {
			t.Fatal("following scope, deduplication or filter", tc, page)
		}
	}
	page, err = listPostsPage(context.Background(), a.store.DB, "", "", "", "following", 0, 2, "", false)
	if err != nil || !page.HasMore || page.NextCursor != "100" {
		t.Fatal("following first page", page, err)
	}
	page, err = listPostsPage(context.Background(), a.store.DB, "", "", "", "following", 0, 2, page.NextCursor, false)
	if err != nil || page.HasMore || len(page.Items) != 1 || page.Items[0].ID != "99" {
		t.Fatal("following cursor", page, err)
	}
	ok(t, b.request("GET", "/api/posts?section=following", nil, "", false), 200)
	ok(t, b.request("GET", "/api/posts?section=following&target=alice", nil, "", false), 400)
	// Following never changes the full home feed.
	page, err = listPosts(context.Background(), a.store.DB, "", "", "", "", 0, 20)
	if err != nil || page.Total != 5 {
		t.Fatal("full feed changed", page, err)
	}
}
