package web

import (
	"encoding/json"
	"testing"
	"xm/internal/archive"
)

func TestQuoteTimelineFiltersUsersAndCycles(t *testing.T) {
	a := testApp(t)
	b := login(t, a)
	quote := archive.Post{ID: "100", AuthorID: "bob-id", Username: "bob", Text: "引用关键词", Raw: json.RawMessage(`{"core":{"user_results":{"result":{"core":{"name":"Bob"}}}}}`), Assets: []archive.Asset{{ID: "v", Kind: "video", URL: "https://video.twimg.com/q.mp4"}}}
	root := archive.Post{ID: "200", AuthorID: "alice-id", Username: "alice", Text: "父推文", Raw: json.RawMessage(`{}`), Quote: &quote}
	if err := a.store.SavePosts("alice", []archive.Post{root}); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/api/posts?target=alice", "/api/posts?kind=video", "/api/posts?q=引用关键词", "/api/posts?kind=media"} {
		w := b.request("GET", path, nil, "", false)
		ok(t, w, 200)
		var list PostList
		if err := json.Unmarshal(w.Body.Bytes(), &list); err != nil {
			t.Fatal(err)
		}
		if list.Total != 1 || len(list.Items) != 1 || list.Items[0].ID != "200" || list.Items[0].Quote == nil || list.Items[0].Quote.Name != "Bob" || len(list.Items[0].Quote.Media) != 1 {
			t.Fatalf("quote relation/filter failed: %s %s", path, w.Body.String())
		}
	}
	list, err := listPosts(a.store.DB, "", "", "text", "", 0, 24)
	if err != nil || list.Total != 0 {
		t.Fatal("quoted video classified as plain text")
	}
	list, err = listPosts(a.store.DB, "bob", "", "", "", 0, 24)
	if err != nil || list.Total != 0 {
		t.Fatal("quote author became monitored timeline")
	}
	users, err := archiveUsers(a.store.DB)
	if err != nil || len(users) != 1 || users[0].Username != "alice" || users[0].Posts != 1 {
		t.Fatal("directory should list targets, not quote authors", err)
	}
	counts, err := counts(a.store.DB)
	if err != nil || counts["posts"] != 1 {
		t.Fatal("global post count includes standalone quote")
	}
	anon := &browserClient{t: t, a: a}
	ok(t, anon.request("GET", "/api/users", nil, "", false), 401)
	if _, err = a.store.DB.Exec("UPDATE posts SET quote_id='200' WHERE id='100'"); err != nil {
		t.Fatal(err)
	}
	list, err = listPosts(a.store.DB, "alice", "", "video", "", 0, 24)
	if err != nil || list.Items[0].Quote == nil || !list.Items[0].Quote.QuoteUnavailable {
		t.Fatal("cycle did not stop at unavailable marker", err)
	}
	if _, err = a.store.DB.Exec("UPDATE posts SET quote_id='missing' WHERE id='200'"); err != nil {
		t.Fatal(err)
	}
	list, err = listPosts(a.store.DB, "alice", "", "", "", 0, 24)
	if err != nil || !list.Items[0].QuoteUnavailable || list.Items[0].Quote != nil {
		t.Fatal("missing quote should keep parent visible", err)
	}
}
