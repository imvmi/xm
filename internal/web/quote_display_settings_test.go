package web

import (
	"context"
	"encoding/json"
	"testing"

	"xm/internal/archive"
)

func TestQuoteDisplaySettingsPersistenceAndValidation(t *testing.T) {
	a := testApp(t)
	b := login(t, a)
	if a.Config().QuoteDepthLimit() != 4 {
		t.Fatal("default quote depth must remain four")
	}
	for _, depth := range []int{0, 1, 2, 3, 4, 0} {
		ok(t, b.request("PUT", "/api/settings", map[string]int{"quote_display_depth": depth}, "http://localhost", true), 200)
		w := b.request("GET", "/api/settings", nil, "", false)
		var got struct {
			Depth int `json:"quote_display_depth"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil || got.Depth != depth {
			t.Fatal("settings response lost depth", got, err)
		}
		loaded, err := a.store.LoadSettings(a.Config())
		if err != nil || loaded.QuoteDepthLimit() != depth {
			t.Fatal("saved quote depth not restored", depth, err)
		}
	}
	for _, invalid := range []any{-1, 5, 1.5, "2", true} {
		ok(t, b.request("PUT", "/api/settings", map[string]any{"quote_display_depth": invalid}, "http://localhost", true), 400)
	}
	ok(t, b.request("PUT", "/api/settings", map[string]string{"archive_theme": "dark"}, "http://localhost", true), 200)
	if a.Config().QuoteDepthLimit() != 0 {
		t.Fatal("invalid or unrelated edits changed zero depth")
	}
	// A display preference must not truncate archive data or API responses.
	post := archive.Post{ID: "5", Username: "alice", Text: "deepest"}
	for _, id := range []string{"4", "3", "2", "1"} {
		child := post
		post = archive.Post{ID: id, Username: "alice", QuoteID: child.ID, Quote: &child}
	}
	if err := a.store.SavePosts("alice", []archive.Post{post}); err != nil {
		t.Fatal(err)
	}
	w := b.request("GET", "/api/posts/1", nil, "", false)
	ok(t, w, 200)
	var result PostList
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if len(result.Items) != 1 {
		t.Fatal("missing post", w.Body.String())
	}
	got := result.Items[0]
	for depth := 0; depth < 4; depth++ {
		if got.Quote == nil {
			t.Fatal("display preference truncated archived quote", depth)
		}
		got = *got.Quote
	}
	dst, err := archive.OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer dst.Close()
	if err = migrate(dst); err != nil {
		t.Fatal(err)
	}
	if err = a.store.CopyToSQLite(context.Background(), dst, nil); err != nil {
		t.Fatal(err)
	}
	loaded, err := dst.LoadSettings(archive.DefaultConfig())
	if err != nil || loaded.QuoteDepthLimit() != 0 {
		t.Fatal("database copy lost zero depth", err)
	}
}
