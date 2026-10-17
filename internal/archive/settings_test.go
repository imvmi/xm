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
	cfg.DefaultInterval = 1800
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
	if err != nil || loaded.Proxy != cfg.Proxy || loaded.DefaultInterval != 1800 || !loaded.PollingPaused || loaded.ActiveStorage() != "nas" || loaded.StorageProfiles[0].Password != "storage-secret" || loaded.NotificationChannels[0].BotToken != "12345:bot-secret" {
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
