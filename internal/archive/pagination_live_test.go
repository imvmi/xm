package archive

import (
	"context"
	"os"
	"testing"
)

// Explicit read-only verification: uses configured cookies, logs only counts,
// and does not save posts, download media or send notifications.
func TestLivePagination(t *testing.T) {
	path := os.Getenv("XM_LIVE_PAGINATION_CONFIG")
	if path == "" {
		t.Skip("set XM_LIVE_PAGINATION_CONFIG to inspect real timeline pagination")
	}
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatal("configuration unavailable")
	}
	store, err := OpenConfiguredStore(cfg)
	if err != nil {
		t.Fatal("database unavailable")
	}
	defer store.Close()
	var target string
	var replies bool
	if err = store.DB.QueryRow("SELECT username,replies FROM monitors ORDER BY last_started DESC LIMIT 1").Scan(&target, &replies); err != nil {
		t.Fatal("no monitor available")
	}
	if os.Getenv("XM_LIVE_PAGINATION_MODE") == "posts" {
		replies = false
	}
	identity, err := store.TargetIdentity(target)
	if err != nil {
		t.Fatal("identity unavailable")
	}
	cookies, err := store.LoadCookies()
	if err != nil {
		t.Fatal("cookies unavailable")
	}
	cfg.CollectionTimeout = 120
	result, err := CollectBound(context.Background(), cfg, cookies, target, identity, 100, replies, func(p Page) error {
		if p.Timeline {
			t.Logf("timeline batch: posts=%d unavailable=%d", len(p.Posts), p.Unavailable)
		}
		return nil
	})
	t.Logf("posts=%d pages=%d stop=%s", result.Posts, result.Pages, result.Stop)
	if err != nil {
		t.Fatal(err)
	}
}
