package archive

import (
	"context"
	"database/sql"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"xm/internal/database"
)

// Opt-in, read-only smoke test against X using an explicitly supplied local
// installation. No posts, cookies, usernames or IDs are printed or persisted.
func TestLiveUserIDResolution(t *testing.T) {
	path := os.Getenv("XM_LIVE_ID_CONFIG")
	if path == "" {
		t.Skip("set XM_LIVE_ID_CONFIG to opt into an authenticated X read")
	}
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	u := url.URL{Scheme: "file", Path: filepath.Join(cfg.DataDir, "archive.sqlite")}
	db, err := sql.Open("sqlite", u.String()+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	cookies, err := (&Store{DB: &database.DB{DB: db, Dialect: "sqlite"}}).LoadCookies()
	if err != nil {
		t.Fatal("Cookie unavailable")
	}
	var id, name, anchor string
	err = db.QueryRow(`SELECT p.author_id,p.username,p.id FROM posts p JOIN target_posts t ON t.post_id=p.id WHERE p.username=t.target AND length(p.author_id)>8 ORDER BY p.last_seen DESC LIMIT 1`).Scan(&id, &name, &anchor)
	if err != nil {
		t.Fatal("no captured identity available for live verification")
	}
	cfg.BrowserMaxAge = 60
	cfg.MaxScrolls = 3
	cfg.ScrollDelay = 2
	verified := false
	result, err := CollectBound(context.Background(), cfg, cookies, "xm_stale_handle", TargetIdentity{UserID: id, ReferencePostID: anchor}, 1, false, func(p Page) error {
		if p.Profile != nil {
			verified = p.Profile.ID == id && p.Profile.Username != "xm_stale_handle"
		}
		for _, post := range p.Posts {
			if post.AuthorID != id {
				t.Error("unexpected author identity")
			}
		}
		return nil
	})
	if err != nil {
		_, baselineErr := Collect(context.Background(), cfg, cookies, name, 1, false, func(Page) error { return nil })
		t.Logf("Current-handle baseline result: %v", baselineErr)
		t.Fatal(err)
	}
	if !verified {
		t.Fatal("Archived-post route did not resolve a verified current handle")
	}
	t.Logf("Archived-post route resolved a current handle; captured %d post(s), discarded after verification", result.Posts)
}
