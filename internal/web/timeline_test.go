package web

import (
	"encoding/json"
	"fmt"
	"testing"
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
