package archive

import (
	"context"
	"os"
	"strconv"
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
	target := os.Getenv("XM_LIVE_PAGINATION_TARGET")
	var replies bool
	query := "SELECT username,replies FROM monitors ORDER BY last_started DESC LIMIT 1"
	args := []any{}
	if target != "" {
		query = "SELECT username,replies FROM monitors WHERE username=?"
		args = append(args, target)
	}
	if err = store.DB.QueryRow(query, args...).Scan(&target, &replies); err != nil {
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
	if os.Getenv("XM_LIVE_PAGINATION_BUDGET") == "1" {
		identity.RequestBudget, err = store.LoadRequestBudget(cookies)
		if err != nil {
			t.Fatal("request budget unavailable")
		}
	}
	limit := 100
	if raw := os.Getenv("XM_LIVE_PAGINATION_LIMIT"); raw != "" {
		limit, err = strconv.Atoi(raw)
		if err != nil || limit < 1 || limit > 10000 {
			t.Fatal("invalid live pagination limit")
		}
	}
	cfg.BrowserMaxAge = 120
	result, err := CollectBound(context.Background(), cfg, cookies, target, identity, limit, replies, func(p Page) error {
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
