package web

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"xm/internal/archive"
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
		{"collection_timeout_seconds": 300}, // Removed setting must not silently accept an ineffective timeout.
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
	ok(t, b.request("PUT", "/api/settings", map[string]any{"scroll_delay_seconds": 4, "scroll_jitter_seconds": 1, "timeline_idle_seconds": 40, "known_boundary_posts": 12}, "http://localhost", true), 200)
	ok(t, b.request("PUT", "/api/settings", map[string]any{"rate_limit_mode": "fast", "request_timeout_seconds": 90, "max_media_bytes": 1024 << 20, "browser_max_age_seconds": 12 * 3600, "browser_max_tasks": 250, "media_retry_initial_seconds": 10 * 60, "media_retry_max_seconds": 48 * 3600}, "http://localhost", true), 200)
	var next int64
	if err := a.store.DB.QueryRow("SELECT next_run FROM monitors WHERE username='alice'").Scan(&next); err != nil || next < before+7200 || next > time.Now().Unix()+7200 {
		t.Fatal(next, err)
	}
	if err := a.store.DB.QueryRow("SELECT next_run FROM monitors WHERE username='paused'").Scan(&next); err != nil || next != 123 {
		t.Fatal(next, err)
	}
	loaded, err := a.store.LoadSettings(a.Config())
	if err != nil || loaded.RequestInterval != 5 || loaded.RequestJitter != 0 || loaded.DefaultInterval != 7200 || loaded.QueueInterval != 30 || loaded.MaxPostsPerRun != 1000 || loaded.PageBudget != 15 || loaded.MaxScrolls != 40 || loaded.ScrollDelay != 4 || loaded.ScrollJitter != 1 || loaded.TimelineIdleSeconds != 40 || loaded.KnownBoundaryPosts != 12 || loaded.RequestBudgetMode != "balanced" || loaded.RateLimitMode != "fast" || loaded.RequestTimeout != 90 || loaded.MaxMediaBytes != 1024<<20 || loaded.BrowserMaxAge != 12*3600 || loaded.BrowserMaxTasks != 250 || loaded.MediaRetryInitial != 10*60 || loaded.MediaRetryMax != 48*3600 {
		t.Fatal(loaded, err)
	}
}

func TestTaskLifetimeUsesBrowserSettingAfterReload(t *testing.T) {
	a := testApp(t)
	b := login(t, a)
	for _, hours := range []int{1, 24, 12} {
		ok(t, b.request("PUT", "/api/settings", map[string]any{"browser_max_age_seconds": hours * 3600}, "http://localhost", true), 200)
	}
	// Existing databases may still contain a short independent task timeout.
	if _, err := a.store.DB.Exec("UPDATE system_settings SET collection_timeout_seconds=60 WHERE id=1"); err != nil {
		t.Fatal(err)
	}
	loaded, err := a.store.LoadSettings(a.Config())
	if err != nil || loaded.BrowserMaxAge != 12*3600 {
		t.Fatal("browser lifetime did not survive reload", loaded.BrowserMaxAge, err)
	}
	a.configMu.Lock()
	a.config = loaded
	a.configMu.Unlock()
	w := b.request("GET", "/api/settings", nil, "", false)
	ok(t, w, 200)
	var settings map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &settings); err != nil {
		t.Fatal(err)
	}
	if _, exists := settings["collection_timeout_seconds"]; exists {
		t.Fatal("removed task timeout still exposed")
	}
	seedMonitor(t, a, "alice")
	called := false
	a.scheduler.collect = func(ctx context.Context, _ archive.Config, _ archive.Cookies, _ string, _ archive.TargetIdentity, _ int, _ bool, _ func(archive.Page) error) (archive.Collection, error) {
		called = true
		deadline, ok := ctx.Deadline()
		if remaining := time.Until(deadline); !ok || remaining < 12*time.Hour-time.Minute || remaining > 12*time.Hour {
			t.Fatal("task deadline did not use browser lifetime", remaining, ok)
		}
		return archive.Collection{Stop: "known_boundary_reached", HistoryComplete: true}, nil
	}
	schedulerStep(t, a)
	if !called {
		t.Fatal("collector was not called")
	}
}

func TestCollectionSettingsScheduleFailureRollsBack(t *testing.T) {
	a := testApp(t)
	b := login(t, a)
	seedMonitor(t, a, "alice")
	seedMonitor(t, a, "paused")
	if _, err := a.store.DB.Exec("UPDATE monitors SET enabled=0,next_run=123 WHERE username='paused'"); err != nil {
		t.Fatal(err)
	}
	trigger := "CREATE TRIGGER reject_schedule BEFORE UPDATE OF next_run ON monitors BEGIN SELECT RAISE(ABORT,'injected schedule failure'); END"
	if a.store.DB.Dialect == "mysql" {
		trigger = "CREATE TRIGGER reject_schedule BEFORE UPDATE ON monitors FOR EACH ROW SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT='injected schedule failure'"
	}
	if _, err := a.store.DB.Exec(trigger); err != nil {
		t.Fatal(err)
	}
	payload := map[string]any{"default_interval_seconds": 7200, "queue_interval_seconds": 30}
	ok(t, b.request("PUT", "/api/settings", payload, "http://localhost", true), 500)
	loaded, err := a.store.LoadSettings(a.Config())
	if err != nil {
		t.Fatal(err)
	}
	if loaded.DefaultInterval != 3600 || loaded.QueueInterval != 0 || a.Config().DefaultInterval != 3600 || a.Config().QueueInterval != 0 {
		t.Fatal("failed schedule update changed persisted or runtime settings")
	}
	list, err := monitors(a.store.DB)
	if err != nil || len(list) != 2 {
		t.Fatal(list, err)
	}
	for _, m := range list {
		want := int64(0)
		if m.Username == "paused" {
			want = 123
		}
		if m.NextRun != want {
			t.Fatal("failed save changed schedule", m.Username, m.NextRun)
		}
	}
	select {
	case <-a.scheduler.wake:
		t.Fatal("failed save woke scheduler")
	default:
	}
	if _, err := a.store.DB.Exec("DROP TRIGGER reject_schedule"); err != nil {
		t.Fatal(err)
	}
	retriedAt := time.Now().Unix()
	ok(t, b.request("PUT", "/api/settings", payload, "http://localhost", true), 200)
	loaded, err = a.store.LoadSettings(a.Config())
	if err != nil || loaded.DefaultInterval != 7200 || loaded.QueueInterval != 30 || a.Config().DefaultInterval != 7200 || a.Config().QueueInterval != 30 {
		t.Fatal("retry did not update settings", err)
	}
	list, err = monitors(a.store.DB)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range list {
		if m.Username == "paused" {
			if m.NextRun != 123 {
				t.Fatal("retry changed disabled monitor schedule", m.NextRun)
			}
		} else if m.NextRun < retriedAt+7200 || m.NextRun > time.Now().Unix()+7200 {
			t.Fatal("retry did not update enabled monitor schedule", m.NextRun)
		}
	}
	select {
	case <-a.scheduler.wake:
	default:
		t.Fatal("successful retry did not wake scheduler")
	}
}

func TestCollectionIntervalChangesDuringBatch(t *testing.T) {
	a := testApp(t)
	b := login(t, a)
	seedMonitor(t, a, "alice")
	seedMonitor(t, a, "bob")
	calls := 0
	a.scheduler.collect = func(ctx context.Context, cfg archive.Config, _ archive.Cookies, target string, _ archive.TargetIdentity, _ int, _ bool, _ func(archive.Page) error) (archive.Collection, error) {
		calls++
		if target == "alice" {
			ok(t, b.request("PUT", "/api/settings", map[string]any{"default_interval_seconds": 7200}, "http://localhost", true), 200)
			if a.Config().DefaultInterval != 7200 || cfg.DefaultInterval != 3600 || ctx.Err() != nil {
				t.Fatal("save did not update runtime or interrupted current task")
			}
		} else if cfg.DefaultInterval != 7200 {
			t.Fatal("next task retained old interval")
		}
		return archive.Collection{Stop: "known_boundary_reached", HistoryComplete: true}, nil
	}
	before := time.Now().Unix()
	schedulerStep(t, a)
	list, err := monitors(a.store.DB)
	if err != nil || calls != 2 || len(list) != 2 {
		t.Fatal(calls, list, err)
	}
	for _, m := range list {
		if m.NextRun < before+7200 || m.NextRun > time.Now().Unix()+7200 || m.Interval != 7200 {
			t.Fatal("batch finished with old schedule", m)
		}
	}
	if worked, err := a.scheduler.step(context.Background()); worked || err != nil {
		t.Fatal("new schedule ignored", worked, err)
	}
}
