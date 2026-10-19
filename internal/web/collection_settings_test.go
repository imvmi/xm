package web

import (
	"testing"
	"time"
)

func TestCollectionSettingsValidationAndSchedule(t *testing.T) {
	a := testApp(t)
	b := login(t, a)
	seedMonitor(t, a, "alice")
	seedMonitor(t, a, "paused")
	if _, err := a.store.DB.Exec("UPDATE monitors SET enabled=0,next_run=123 WHERE username='paused'"); err != nil {
		t.Fatal(err)
	}
	for _, payload := range []map[string]any{
		{"request_interval_seconds": 4}, {"request_jitter_seconds": -1},
		{"default_interval_seconds": 3599}, {"default_interval_seconds": 604801},
		{"queue_interval_seconds": -1}, {"queue_interval_seconds": 3601},
		{"max_posts_per_run": 19}, {"max_posts_per_run": 10001}, {"page_budget": 0}, {"page_budget": 101},
		{"max_scrolls": 4}, {"max_scrolls": 501}, {"scroll_delay_seconds": 0}, {"scroll_delay_seconds": 31}, {"scroll_jitter_seconds": -1}, {"scroll_jitter_seconds": 5}, {"scroll_delay_seconds": 4, "scroll_jitter_seconds": 4},
		{"timeline_idle_seconds": 9}, {"timeline_idle_seconds": 121}, {"known_boundary_posts": 2}, {"known_boundary_posts": 51},
		{"collection_timeout_seconds": 59}, {"collection_timeout_seconds": 1801},
		{"request_budget_mode": "unsafe"}, {"rate_limit_mode": "none"}, {"rate_limit_mode": "turbo"}, {"request_timeout_seconds": 9}, {"request_timeout_seconds": 301},
		{"max_media_bytes": (1 << 20) - 1}, {"max_media_bytes": int64(10<<30) + 1},
		{"browser_max_age_seconds": 3599}, {"browser_max_age_seconds": 86401}, {"browser_max_tasks": 9}, {"browser_max_tasks": 1001},
		{"media_retry_initial_seconds": 59}, {"media_retry_initial_seconds": 86401}, {"media_retry_initial_seconds": 600, "media_retry_max_seconds": 599}, {"media_retry_max_seconds": 604801},
	} {
		ok(t, b.request("PUT", "/api/settings", payload, "http://localhost", true), 400)
	}
	if a.Config().RequestInterval != 5 || a.Config().RequestJitter != 0 || a.Config().DefaultInterval != 3600 {
		t.Fatal("invalid update changed settings")
	}
	before := time.Now().Unix()
	ok(t, b.request("PUT", "/api/settings", map[string]any{"default_interval_seconds": 7200, "queue_interval_seconds": 30}, "http://localhost", true), 200)
	ok(t, b.request("PUT", "/api/settings", map[string]any{"scroll_delay_seconds": 4, "scroll_jitter_seconds": 1, "timeline_idle_seconds": 40, "known_boundary_posts": 12, "collection_timeout_seconds": 600}, "http://localhost", true), 200)
	ok(t, b.request("PUT", "/api/settings", map[string]any{"rate_limit_mode": "fast", "request_timeout_seconds": 90, "max_media_bytes": 1024 << 20, "browser_max_age_seconds": 12 * 3600, "browser_max_tasks": 250, "media_retry_initial_seconds": 10 * 60, "media_retry_max_seconds": 48 * 3600}, "http://localhost", true), 200)
	var next int64
	if err := a.store.DB.QueryRow("SELECT next_run FROM monitors WHERE username='alice'").Scan(&next); err != nil || next < before+7200 || next > time.Now().Unix()+7200 {
		t.Fatal(next, err)
	}
	if err := a.store.DB.QueryRow("SELECT next_run FROM monitors WHERE username='paused'").Scan(&next); err != nil || next != 123 {
		t.Fatal(next, err)
	}
	loaded, err := a.store.LoadSettings(a.Config())
	if err != nil || loaded.RequestInterval != 5 || loaded.RequestJitter != 0 || loaded.DefaultInterval != 7200 || loaded.QueueInterval != 30 || loaded.MaxPostsPerRun != 1000 || loaded.PageBudget != 15 || loaded.MaxScrolls != 40 || loaded.ScrollDelay != 4 || loaded.ScrollJitter != 1 || loaded.TimelineIdleSeconds != 40 || loaded.KnownBoundaryPosts != 12 || loaded.CollectionTimeout != 600 || loaded.RequestBudgetMode != "balanced" || loaded.RateLimitMode != "fast" || loaded.RequestTimeout != 90 || loaded.MaxMediaBytes != 1024<<20 || loaded.BrowserMaxAge != 12*3600 || loaded.BrowserMaxTasks != 250 || loaded.MediaRetryInitial != 10*60 || loaded.MediaRetryMax != 48*3600 {
		t.Fatal(loaded, err)
	}
}
