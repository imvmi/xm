package archive

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestBootstrapRejectsInvalidYAML(t *testing.T) {
	for name, content := range map[string]string{
		"unknown runtime field":  "data_dir: data\nproxy: http://localhost:7890\n",
		"duplicate":              "data_dir: data\ndata_dir: other\n",
		"multiple documents":     "data_dir: data\n---\ndata_dir: other\n",
		"null":                   "null\n",
		"invalid database field": "data_dir: data\ndatabase:\n  unknown: true\n",
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.yaml")
			if err := os.WriteFile(path, []byte(content), 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := LoadBootstrap(path); err == nil {
				t.Fatal("invalid bootstrap accepted")
			}
		})
	}
}

func TestBootstrapAndDatabaseSettings(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "config.yaml")
	cfg := DefaultConfig()
	cfg.DataDir = filepath.Join(root, "data")
	cfg.Proxy = "http://user:proxy-secret@127.0.0.1:7890"
	cfg.DefaultInterval = 7200
	cfg.QueueInterval = 45
	cfg.RequestInterval = 12
	cfg.RequestJitter = 3
	cfg.RequestBudgetMode = "conservative"
	cfg.RateLimitMode = "fast"
	cfg.MaxPostsPerRun = 2400
	cfg.PageBudget = 30
	cfg.TimelineIdleSeconds = 45
	cfg.KnownBoundaryPosts = 12
	cfg.BrowserMaxAge = 12 * 3600
	cfg.BrowserMaxTasks = 250
	cfg.MediaRetryInitial = 10 * 60
	cfg.MediaRetryMax = 48 * 3600
	cfg.PollingPaused = true
	cfg.NotificationChannels = []NotificationChannel{{ID: "telegram", Name: "Telegram", Kind: "telegram", Enabled: true, BotToken: "12345:bot-secret", ChatID: "42", Alerts: true}}
	cfg.StorageProfiles = []StorageProfile{{ID: "nas", Name: "NAS", Kind: "webdav", Endpoint: "https://storage.example", Password: "storage-secret", Timeout: 120}}
	cfg.MediaStorage = "nas"
	if err := SaveBootstrap(path, cfg); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]any
	if err = yaml.Unmarshal(raw, &fields); err != nil || len(fields) != 2 {
		t.Fatal("bootstrap must contain only data_dir/database", err)
	}
	for _, secret := range []string{"proxy-secret", "bot-secret", "storage-secret", "polling_paused"} {
		if strings.Contains(string(raw), secret) {
			t.Fatal("runtime value leaked into bootstrap")
		}
	}
	store, err := OpenConfiguredStore(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err = store.SaveSettings(cfg); err != nil {
		t.Fatal(err)
	}
	store.Close()
	loaded, err := LoadConfig(path)
	if err != nil || loaded.Proxy != cfg.Proxy || loaded.DefaultInterval != 7200 || loaded.QueueInterval != 45 || loaded.RequestInterval != 12 || loaded.RequestJitter != 3 || loaded.RequestBudgetMode != "conservative" || loaded.RateLimitMode != "fast" || loaded.MaxPostsPerRun != 2400 || loaded.PageBudget != 30 || loaded.TimelineIdleSeconds != 45 || loaded.KnownBoundaryPosts != 12 || loaded.BrowserMaxAge != 12*3600 || loaded.BrowserMaxTasks != 250 || loaded.MediaRetryInitial != 10*60 || loaded.MediaRetryMax != 48*3600 || !loaded.PollingPaused || loaded.ActiveStorage() != "nas" || loaded.StorageProfiles[0].Password != "storage-secret" || loaded.NotificationChannels[0].BotToken != "12345:bot-secret" {
		t.Fatal("database settings did not survive restart", err)
	}
	// Empty lists clear previous values without inheriting stale in-memory config.
	store, err = OpenConfiguredStore(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	cleared := DefaultConfig()
	cleared.DataDir = cfg.DataDir
	if err = store.SaveSettings(cleared); err != nil {
		t.Fatal(err)
	}
	empty, err := store.LoadSettings(loaded)
	if err != nil || len(empty.NotificationChannels) != 0 || len(empty.StorageProfiles) != 0 || empty.Proxy != "" {
		t.Fatal("stale settings retained", err)
	}
	if _, err = store.DB.Exec("UPDATE system_settings SET active_storage_id='missing'"); err == nil {
		t.Fatal("broken active storage reference accepted")
	}
	if _, err = store.DB.Exec("UPDATE system_settings SET default_interval_seconds=1"); err == nil {
		t.Fatal("invalid polling interval accepted")
	}

}

func TestInitializationTransactionAndMissingSettings(t *testing.T) {
	cfg := DefaultConfig()
	cfg.DataDir = t.TempDir()
	store, err := OpenStore(cfg.DataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	admin := AdminCredentials{Username: "owner", Salt: make([]byte, 32), Hash: make([]byte, 32), ConfigurationLocked: true}
	_, err = store.DB.Exec(`CREATE TRIGGER reject_runtime BEFORE INSERT ON system_settings BEGIN SELECT RAISE(ABORT,'test failure'); END`)
	if err != nil {
		t.Fatal(err)
	}
	if err = store.Initialize(cfg, admin); err == nil {
		t.Fatal("injected write failure accepted")
	}
	var count int
	if err = store.DB.QueryRow("SELECT COUNT(*) FROM admin_accounts").Scan(&count); err != nil || count != 0 {
		t.Fatal("partial initialization committed", err)
	}
	if _, err = store.DB.Exec("DROP TRIGGER reject_runtime"); err != nil {
		t.Fatal(err)
	}
	if err = store.Initialize(cfg, admin); err != nil {
		t.Fatal(err)
	}
	changed := cfg
	changed.PollingPaused = true
	if err = store.Initialize(changed, admin); err == nil {
		t.Fatal("administrator overwritten")
	}
	loaded, err := store.LoadSettings(cfg)
	if err != nil || loaded.PollingPaused {
		t.Fatal("duplicate setup modified runtime", err)
	}
	if _, err = store.DB.Exec("DELETE FROM system_settings"); err != nil {
		t.Fatal(err)
	}
	if _, err = store.LoadSettings(cfg); err == nil {
		t.Fatal("initialized DB silently defaulted to local storage")
	}
}

func TestCollectionSettingsUpgrade(t *testing.T) {
	for _, interval := range []int{900, 5400} {
		s, err := OpenStore(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		if err = s.SaveSettings(DefaultConfig()); err != nil {
			t.Fatal(err)
		}
		// Recreate only the old settings table, then populate a legacy row.
		if _, err = s.DB.Exec("DROP TABLE system_settings"); err != nil {
			t.Fatal(err)
		}
		var legacy string
		for _, q := range settingsSchema {
			if strings.Contains(q, "CREATE TABLE IF NOT EXISTS system_settings(") {
				legacy = q
			}
		}
		legacy = strings.ReplaceAll(legacy, ",media_preload_mode VARCHAR(16) NOT NULL DEFAULT 'standard'", "")
		legacy = strings.ReplaceAll(legacy, ",request_interval_seconds INTEGER NOT NULL DEFAULT 5 CHECK(request_interval_seconds>=5)", "")
		legacy = strings.ReplaceAll(legacy, ",request_jitter_seconds INTEGER NOT NULL DEFAULT 0 CHECK(request_jitter_seconds>=0)", "")
		legacy = strings.ReplaceAll(legacy, ",scroll_jitter_seconds INTEGER NOT NULL DEFAULT 0 CHECK(scroll_jitter_seconds>=0)", "")
		legacy = strings.ReplaceAll(legacy, ",max_posts_per_run INTEGER NOT NULL DEFAULT 1000 CHECK(max_posts_per_run BETWEEN 1 AND 10000)", "")
		legacy = strings.ReplaceAll(legacy, ",page_budget INTEGER NOT NULL DEFAULT 15 CHECK(page_budget BETWEEN 1 AND 100)", "")
		legacy = strings.ReplaceAll(legacy, ",timeline_idle_seconds INTEGER NOT NULL DEFAULT 24 CHECK(timeline_idle_seconds BETWEEN 1 AND 300)", "")
		legacy = strings.ReplaceAll(legacy, ",known_boundary_posts INTEGER NOT NULL DEFAULT 8 CHECK(known_boundary_posts BETWEEN 1 AND 100)", "")
		legacy = strings.ReplaceAll(legacy, ",request_budget_mode VARCHAR(16) NOT NULL DEFAULT 'balanced' CHECK(request_budget_mode IN ('conservative','balanced','fast','turbo'))", "")
		legacy = strings.ReplaceAll(legacy, ",rate_limit_mode VARCHAR(16) NOT NULL DEFAULT 'balanced' CHECK(rate_limit_mode IN ('conservative','balanced','fast'))", "")
		legacy = strings.ReplaceAll(legacy, ",browser_max_age_seconds INTEGER NOT NULL DEFAULT 21600 CHECK(browser_max_age_seconds BETWEEN 300 AND 86400)", "")
		legacy = strings.ReplaceAll(legacy, ",browser_max_tasks INTEGER NOT NULL DEFAULT 100 CHECK(browser_max_tasks BETWEEN 1 AND 1000)", "")
		legacy = strings.ReplaceAll(legacy, ",media_retry_initial_seconds INTEGER NOT NULL DEFAULT 300 CHECK(media_retry_initial_seconds BETWEEN 60 AND 86400)", "")
		legacy = strings.ReplaceAll(legacy, ",media_retry_max_seconds INTEGER NOT NULL DEFAULT 86400 CHECK(media_retry_max_seconds BETWEEN 60 AND 604800)", "")
		legacy = strings.ReplaceAll(legacy, "BETWEEN 3600 AND 604800", "BETWEEN 60 AND 604800")
		if _, err = s.DB.Exec(legacy); err != nil {
			t.Fatal(err)
		}
		if _, err = s.DB.Exec(`INSERT INTO system_settings(id,proxy,chrome_path,headless,request_timeout_seconds,collection_timeout_seconds,scroll_delay_seconds,max_scrolls,max_media_bytes,default_interval_seconds,polling_paused,active_storage_id,updated_at) VALUES(1,'','',1,60,300,3,40,1024,?,1,'local','fixture')`, interval); err != nil {
			t.Fatal(err)
		}
		for i := 0; i < 2; i++ {
			if err = migrateSettings(s.DB); err != nil {
				t.Fatal(err)
			}
			loaded, err := s.LoadSettings(Config{DataDir: t.TempDir()})
			if err != nil || loaded.MediaPreloadMode != "standard" || loaded.DefaultInterval != max(interval, 3600) || loaded.RequestInterval != 5 || loaded.RequestJitter != 0 || loaded.ScrollJitter != 1 || loaded.RequestBudgetMode != "balanced" || loaded.RateLimitMode != "balanced" || loaded.MaxPostsPerRun != 1000 || loaded.PageBudget != 15 || loaded.TimelineIdleSeconds != 24 || loaded.KnownBoundaryPosts != 8 || loaded.BrowserMaxAge != 6*3600 || loaded.BrowserMaxTasks != 100 || loaded.MediaRetryInitial != 5*60 || loaded.MediaRetryMax != 24*3600 || !loaded.PollingPaused {
				t.Fatal(loaded, err)
			}
		}
		s.Close()
	}
}

func TestRequestBudgetModeConstraintUpgrade(t *testing.T) {
	dir := t.TempDir()
	s, err := OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	cfg := DefaultConfig()
	cfg.DataDir = dir
	if err = s.SaveSettings(cfg); err != nil {
		t.Fatal(err)
	}

	// Recreate the current settings row with the previous three-mode CHECK.
	tx, err := s.DB.Begin()
	if err != nil {
		t.Fatal(err)
	}
	oldDDL := strings.Replace(systemSettingsDDL, "CREATE TABLE IF NOT EXISTS system_settings", "CREATE TABLE system_settings_old_modes", 1)
	oldDDL = strings.Replace(oldDDL, "('conservative','balanced','fast','turbo')", "('conservative','balanced','fast')", 1)
	if _, err = tx.Exec(oldDDL); err == nil {
		_, err = tx.Exec("INSERT INTO system_settings_old_modes(" + systemSettingsColumns + ") SELECT " + systemSettingsColumns + " FROM system_settings")
	}
	if err == nil {
		_, err = tx.Exec("DROP TABLE system_settings")
	}
	if err == nil {
		_, err = tx.Exec("ALTER TABLE system_settings_old_modes RENAME TO system_settings")
	}
	if err == nil {
		err = tx.Commit()
	} else {
		tx.Rollback()
	}
	if err != nil {
		t.Fatal(err)
	}

	cfg.RequestBudgetMode = "turbo"
	if err = s.SaveSettings(cfg); err == nil {
		t.Fatal("legacy constraint accepted turbo before migration")
	}
	if err = migrateSettings(s.DB); err != nil {
		t.Fatal(err)
	}
	if err = s.SaveSettings(cfg); err != nil {
		t.Fatal(err)
	}
	loaded, err := s.LoadSettings(cfg)
	if err != nil || loaded.RequestBudgetMode != "turbo" {
		t.Fatal(loaded.RequestBudgetMode, err)
	}
}
