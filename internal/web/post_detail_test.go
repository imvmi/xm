package web

import (
	"encoding/json"
	"testing"

	"xm/internal/archive"
)

func TestArchivedPostDetailIncludesQuoteOnlyPosts(t *testing.T) {
	a := testApp(t)
	quote := archive.Post{ID: "50", Username: "bob", Text: "quoted content", Quote: &archive.Post{ID: "40", Username: "carol", Text: "nested quote"}}
	if err := a.store.SavePosts("alice", []archive.Post{{ID: "100", Username: "alice", Quote: &quote}}); err != nil {
		t.Fatal(err)
	}
	var targets int
	if err := a.store.DB.QueryRow("SELECT COUNT(*) FROM target_posts WHERE post_id='50'").Scan(&targets); err != nil || targets != 0 {
		t.Fatal("fixture must be quote-only", targets, err)
	}
	anon := &browserClient{t: t, a: a}
	ok(t, anon.request("GET", "/api/posts/50", nil, "", false), 401)
	b := login(t, a)
	ok(t, b.request("GET", "/api/posts/invalid", nil, "", false), 400)
	ok(t, b.request("GET", "/api/posts/999", nil, "", false), 404)
	w := b.request("GET", "/api/posts/50", nil, "", false)
	ok(t, w, 200)
	var result PostList
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if len(result.Items) != 1 || result.Items[0].ID != "50" || result.Items[0].Text != "quoted content" || result.Items[0].Quote == nil || result.Items[0].Quote.ID != "40" || result.Total != 1 || result.HasMore {
		t.Fatal(result)
	}
}
