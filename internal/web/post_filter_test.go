package web

import (
	"context"
	"reflect"
	"testing"

	"xm/internal/archive"
)

func TestFilteredPaginationSearchMediaAndCycles(t *testing.T) {
	a := testApp(t)
	media := archive.Post{ID: "2", Username: "quoted", Assets: []archive.Asset{{ID: "p", Kind: "photo", URL: "https://pbs.twimg.com/p.jpg"}, {ID: "g", Kind: "animated_gif", URL: "https://video.twimg.com/g.mp4"}}}
	quote := archive.Post{ID: "1", Username: "QuotedAuthor", Text: "Needle 中文%_ literal", Quote: &media}
	ids := []string{"1000", "999", "100", "99", "9"}
	roots := []archive.Post{}
	for _, id := range ids {
		roots = append(roots, archive.Post{ID: id, Username: "alice", Quote: &quote})
	}
	if err := a.store.SavePosts("alice", roots); err != nil {
		t.Fatal(err)
	}
	// A shared root must appear once, and a quote cycle must terminate.
	if err := a.store.SavePosts("bob", roots[:1]); err != nil {
		t.Fatal(err)
	}
	if _, err := a.store.DB.Exec("UPDATE posts SET quote_id='1' WHERE id='2'"); err != nil {
		t.Fatal(err)
	}
	for _, id := range append(append([]string{}, ids...), "1") {
		at := 100
		if id == "9" {
			at = 200
		}
		if _, err := a.store.DB.Exec("INSERT INTO post_bookmarks(post_id,created_at) VALUES(?,?)", id, at); err != nil {
			t.Fatal(err)
		}
	}
	for _, target := range []string{"", "alice"} {
		for _, tc := range []struct {
			query, kind, section string
			want                 []string
		}{
			{"中文%_", "photo", "", ids},
			{"quotedauthor", "video", "media", ids},
			{"NEEDLE", "animated_gif", "", ids},
			{"中文%_", "text", "", []string{}},
			{"", "text", "media", []string{}},
			{"中文%_", "photo", "bookmarks", []string{"9", "999", "99", "1000", "100", "1"}},
		} {
			all := []string{}
			cursor := ""
			for pages := 0; pages < 10; pages++ {
				page, err := listPostsPage(context.Background(), a.store.DB, target, tc.query, tc.kind, tc.section, 0, 2, cursor, true)
				if err != nil {
					t.Fatal(err)
				}
				if page.Total != len(tc.want) {
					t.Fatalf("%+v total=%d", tc, page.Total)
				}
				all = append(all, replyIDs(page.Items)...)
				if !page.HasMore {
					break
				}
				if page.NextCursor == cursor {
					t.Fatal("cursor did not advance")
				}
				cursor = page.NextCursor
			}
			if !reflect.DeepEqual(all, tc.want) {
				t.Fatalf("target=%s %+v got %v", target, tc, all)
			}
			page, err := listPostsPage(context.Background(), a.store.DB, target, tc.query, tc.kind, tc.section, 2, 20, "", false)
			if err != nil || !reflect.DeepEqual(replyIDs(page.Items), tc.want[min(2, len(tc.want)):]) {
				t.Fatalf("offset mismatch: %+v %v", page, err)
			}
		}
	}
}
