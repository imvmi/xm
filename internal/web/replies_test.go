package web

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"xm/internal/archive"
)

func replyIDs(items []PostView) []string {
	ids := []string{}
	for _, p := range items {
		ids = append(ids, p.ID)
	}
	return ids
}

func TestSelfRepliesTimelineFiltersAndPreviews(t *testing.T) {
	a := testApp(t)
	posts := []archive.Post{
		{ID: "100", AuthorID: "42", Username: "oldname", Text: strings.Repeat("原", 200), Assets: []archive.Asset{{ID: "p", Kind: "photo", URL: "https://pbs.twimg.com/original.jpg"}}},
		{ID: "200", AuthorID: "42", Username: "newname", Text: "续篇", ReplyTo: "100"}, // Parent fallback, despite changed username.
		{ID: "201", AuthorID: "42", Username: "newname", ReplyTo: "999", ReplyToAuthorID: "42", Text: "无原帖", Assets: []archive.Asset{{ID: "v", Kind: "video", URL: "https://video.twimg.com/a.mp4"}}},
		{ID: "202", AuthorID: "42", Username: "newname", ReplyTo: "998", ReplyToAuthorID: "88", Text: "回复别人"},
		{ID: "203", AuthorID: "42", Username: "newname", ReplyTo: "997", Text: "关系未知"},
		{ID: "204", AuthorID: "88", Username: "oldname", ReplyTo: "100", Text: "相同用户名但不同用户 ID"},
	}
	if err := a.store.SavePosts("alice", posts); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		target, section, query, kind string
		want                         []string
	}{
		{"", "", "", "", []string{"201", "200", "100"}},
		{"alice", "posts", "", "", []string{"201", "200", "100"}},
		{"alice", "replies", "", "", []string{"204", "203", "202", "201", "200"}},
		{"", "", "续篇", "", []string{"200"}},
		{"", "", "", "video", []string{"201"}},
		{"", "", "", "photo", []string{"100"}}, // Context media must not change root filtering.
	} {
		got, err := listPosts(a.store.DB, tc.target, tc.query, tc.kind, tc.section, 0, 20)
		if err != nil || !reflect.DeepEqual(replyIDs(got.Items), tc.want) || got.Total != len(tc.want) {
			t.Fatalf("%+v got %v total %d: %v", tc, replyIDs(got.Items), got.Total, err)
		}
		for _, p := range got.Items {
			if p.ID == "200" {
				if !p.SelfReply || p.ReplyPreview == nil || p.ReplyPreview.ID != "100" || len([]rune(p.ReplyPreview.Text)) != 161 {
					t.Fatal("missing or unbounded preview", p)
				}
			}
			if p.ID == "201" && p.ReplyPreview != nil {
				t.Fatal("missing parent fabricated")
			}
		}
	}
	first, err := listPostsPage(a.store.DB, "", "", "", "", 0, 1, "", true)
	if err != nil || first.NextCursor != "201" || !first.HasMore {
		t.Fatal(first, err)
	}
	second, err := listPostsPage(a.store.DB, "", "", "", "", 0, 2, first.NextCursor, false)
	if err != nil || !reflect.DeepEqual(replyIDs(second.Items), []string{"200", "100"}) || second.HasMore {
		t.Fatal(second, err)
	}
	// Unknown parent arriving later immediately enables classification, without a rescan.
	if err = a.store.SavePosts("alice", []archive.Post{{ID: "997", AuthorID: "42", Username: "oldname"}}); err != nil {
		t.Fatal(err)
	}
	got, err := listPosts(a.store.DB, "", "关系未知", "", "", 0, 20)
	if err != nil || len(got.Items) != 1 || !got.Items[0].SelfReply {
		t.Fatal(got, err)
	}
	// The account's home visibility still wins over the new reply rule.
	if _, err = a.store.DB.Exec("INSERT INTO account_preferences(target,show_in_timeline) VALUES('alice',0)"); err != nil {
		t.Fatal(err)
	}
	got, err = listPosts(a.store.DB, "", "", "", "", 0, 20)
	if err != nil || got.Total != 0 {
		t.Fatal(got, err)
	}
}

func TestReplyContextBoundariesAndAuthorization(t *testing.T) {
	a := testApp(t)
	b := login(t, a)
	posts := []archive.Post{{ID: "100", AuthorID: "42", Username: "alice", Text: "root"}}
	for i := 101; i <= 125; i++ {
		posts = append(posts, archive.Post{ID: fmt.Sprint(i), AuthorID: "42", Username: "alice", ReplyTo: fmt.Sprint(i - 1), ReplyToAuthorID: "42", Text: fmt.Sprint(i)})
	}
	posts = append(posts, archive.Post{ID: "200", AuthorID: "42", Username: "alice", ReplyTo: "999", ReplyToAuthorID: "42"}, archive.Post{ID: "300", AuthorID: "42", Username: "alice", ReplyTo: "301", ReplyToAuthorID: "42"}, archive.Post{ID: "301", AuthorID: "42", Username: "alice", ReplyTo: "300", ReplyToAuthorID: "42"}, archive.Post{ID: "400", AuthorID: "42", Username: "alice", ReplyTo: "500", ReplyToAuthorID: "88"}, archive.Post{ID: "500", AuthorID: "88", Username: "bob", Text: "other author"})
	if err := a.store.SavePosts("alice", posts); err != nil {
		t.Fatal(err)
	}
	anon := &browserClient{t: t, a: a}
	ok(t, anon.request("GET", "/api/posts/125/context", nil, "", false), 401)
	read := func(id string) ReplyContext {
		t.Helper()
		w := b.request("GET", "/api/posts/"+id+"/context", nil, "", false)
		ok(t, w, 200)
		var v ReplyContext
		if err := json.Unmarshal(w.Body.Bytes(), &v); err != nil {
			t.Fatal(err)
		}
		return v
	}
	first := read("125")
	if len(first.Items) != 20 || first.Items[0].ID != "106" || first.Items[19].ID != "125" || first.NextCursor != "105" {
		t.Fatal(first)
	}
	next := read(first.NextCursor)
	if len(next.Items) != 6 || next.Items[0].ID != "100" || next.End != "beginning" || next.NextCursor != "" {
		t.Fatal(next)
	}
	if got := read("200"); len(got.Items) != 1 || got.End != "missing" || got.MissingID != "999" {
		t.Fatal(got)
	}
	if got := read("300"); len(got.Items) != 2 || got.End != "cycle" {
		t.Fatal(got)
	}
	if got := read("400"); len(got.Items) != 1 || got.End != "other_reply" {
		t.Fatal(got)
	}
	ok(t, b.request("GET", "/api/posts/999/context", nil, "", false), 404)
	p := PostView{ReplyPreview: &ReplySummary{Text: "must not cross API account scope"}, Quote: &PostView{ReplyPreview: &ReplySummary{Text: "also private"}}}
	sanitizeExternalPost(&p, "alice")
	if p.ReplyPreview != nil || p.Quote.ReplyPreview != nil {
		t.Fatal("external API leaked reply preview")
	}
}

func TestSelfReplyDatabaseCopy(t *testing.T) {
	a := testApp(t)
	if err := a.store.SavePosts("alice", []archive.Post{{ID: "200", AuthorID: "42", Username: "alice", ReplyTo: "100", ReplyToAuthorID: "42", Text: "parent not yet archived"}}); err != nil {
		t.Fatal(err)
	}
	dst, err := archive.OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer dst.Close()
	if err = migrate(dst); err != nil {
		t.Fatal(err)
	}
	if err = a.store.CopyDatabase(context.Background(), dst, nil); err != nil {
		t.Fatal(err)
	}
	result, err := listPosts(dst.DB, "", "", "", "", 0, 20)
	if err != nil || len(result.Items) != 1 || !result.Items[0].SelfReply || result.Items[0].ReplyToAuthorID != "42" || result.Items[0].ReplyTo != "100" {
		t.Fatal("migration lost self-reply identity", result, err)
	}
}
