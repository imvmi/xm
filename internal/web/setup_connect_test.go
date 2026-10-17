package web

import (
	"context"
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"xm/internal/archive"
)

func existingSetupFixture(t *testing.T) (*setupWizard, *App, setupInput) {
	t.Helper()
	a := testApp(t)
	s := setupFixture(t)
	db := a.Config().Database
	if db.DriverName() == "sqlite" {
		// Simulate a second host's destination without requiring MySQL in unit
		// tests. With XM_TEST_MYSQL_ADDR the real connector and worker lock run.
		db = archive.DatabaseConfig{Driver: "mysql", Host: "database.example.test", Port: 3306, Name: "xm", User: "xm", Password: "database-secret", TLSMode: "disabled"}
		s.openStore = func(c archive.Config) (*archive.Store, error) {
			if !reflect.DeepEqual(c.Database, db) {
				t.Fatal("unexpected database destination")
			}
			return archive.OpenConfiguredStore(a.Config())
		}
	}
	return s, a, setupInput{Database: db, Username: "admin", Password: testPassword}
}

func setupDatabaseSnapshot(t *testing.T, a *App) map[string]string {
	t.Helper()
	result := map[string]string{}
	for _, table := range []string{"admin_accounts", "system_settings", "storage_profiles", "notification_channels", "x_cookies", "monitors", "posts", "target_posts"} {
		rows, err := a.store.DB.Query("SELECT * FROM " + table + " ORDER BY 1")
		if err != nil {
			t.Fatal(err)
		}
		cols, err := rows.Columns()
		if err != nil {
			t.Fatal(err)
		}
		var data [][]any
		for rows.Next() {
			values, dest := make([]any, len(cols)), make([]any, len(cols))
			for i := range dest {
				dest[i] = &values[i]
			}
			if err = rows.Scan(dest...); err != nil {
				t.Fatal(err)
			}
			data = append(data, values)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			t.Fatal(err)
		}
		b, err := json.Marshal(data)
		if err != nil {
			t.Fatal(err)
		}
		result[table] = string(b)
	}
	return result
}

func TestSetupConnectPreservesExistingDatabase(t *testing.T) {
	s, a, input := existingSetupFixture(t)
	cfg := a.Config()
	cfg.DefaultInterval, cfg.PollingPaused = 1800, true
	cfg.Proxy = "http://127.0.0.1:7890"
	cfg.StorageProfiles = []archive.StorageProfile{{ID: "existing", Name: "Existing storage", Kind: "webdav", Endpoint: "https://storage.invalid/archive", Username: "owner", Password: "storage-secret", Timeout: 120}}
	cfg.MediaStorage = "existing"
	cfg.NotificationChannels = []archive.NotificationChannel{{ID: "notify", Name: "Notify", Kind: "webhook", WebhookURL: "https://notify.invalid", BearerToken: "notification-secret"}}
	if err := a.store.SaveSettings(cfg); err != nil {
		t.Fatal(err)
	}
	seedMonitor(t, a, "alice")
	if err := a.store.SavePosts("alice", []archive.Post{{ID: "1", Username: "alice", AuthorID: "42", Text: "existing post"}}); err != nil {
		t.Fatal(err)
	}
	before := setupDatabaseSnapshot(t, a)
	w := setupRequest(t, s, "database-test", input, "http://localhost")
	ok(t, w, 200)
	var result struct{ Initialized bool }
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil || !result.Initialized {
		t.Fatal("initialized database not detected", err)
	}
	for _, secret := range []string{"storage-secret", "notification-secret", testPassword, "database-secret"} {
		if strings.Contains(w.Body.String(), secret) {
			t.Fatal("detection leaked credentials")
		}
	}
	ok(t, setupRequest(t, s, "connect", input, "http://evil.example"), 403)
	wrong := input
	wrong.Password = "wrong-password"
	ok(t, setupRequest(t, s, "connect", wrong, "http://localhost"), 401)
	wrong = input
	wrong.Username = "different-admin"
	ok(t, setupRequest(t, s, "connect", wrong, "http://localhost"), 401)
	if _, err := os.Stat(s.path); !os.IsNotExist(err) || s.completed || len(s.done) != 0 {
		t.Fatal("failed verification saved bootstrap or completed setup", err)
	}
	// Neither the fresh-setup path nor malicious storage selections may replace
	// any existing records. The connect path does not contact remote storage.
	ok(t, setupRequest(t, s, "complete", input, "http://localhost"), 400)
	input.StorageKind = "s3"
	input.Storage = archive.StorageProfile{Endpoint: "invalid-new-storage"}
	ok(t, setupRequest(t, s, "connect", input, "http://localhost"), 200)
	connected := <-s.done
	if !s.completed || connected.DataDir != s.cfg.DataDir || connected.DataDir == a.Config().DataDir || connected.Database != input.Database || connected.MediaStorage != cfg.MediaStorage || !reflect.DeepEqual(connected.StorageProfiles, cfg.StorageProfiles) || !reflect.DeepEqual(connected.NotificationChannels, cfg.NotificationChannels) || connected.Proxy != cfg.Proxy || connected.DefaultInterval != cfg.DefaultInterval || !connected.PollingPaused {
		t.Fatal("existing configuration was not loaded intact")
	}
	if !reflect.DeepEqual(before, setupDatabaseSnapshot(t, a)) {
		t.Fatal("connection changed existing database records")
	}
	saved, err := archive.LoadBootstrap(s.path)
	if err != nil || saved.Database != input.Database || saved.DataDir != connected.DataDir {
		t.Fatal("incorrect new-host bootstrap", err)
	}
	data, err := os.ReadFile(s.path)
	if err != nil || strings.Contains(string(data), testPassword) || strings.Contains(string(data), "storage-secret") || strings.Contains(string(data), "notification-secret") {
		t.Fatal("bootstrap contains administrator or runtime secrets", err)
	}
	ok(t, setupRequest(t, s, "connect", input, "http://localhost"), 409)
	login(t, a) // The original credentials remain valid.
}

func TestSetupConnectFailureDoesNotChangeConfiguration(t *testing.T) {
	for _, failure := range []string{"bootstrap", "credentials", "settings"} {
		t.Run(failure, func(t *testing.T) {
			s, a, input := existingSetupFixture(t)
			want := 400
			switch failure {
			case "bootstrap":
				s.path = t.TempDir()
				want = 500
			case "credentials":
				if _, err := a.store.DB.Exec("UPDATE admin_accounts SET username='' WHERE id=1"); err != nil {
					t.Fatal(err)
				}
			case "settings":
				if _, err := a.store.DB.Exec("DELETE FROM system_settings"); err != nil {
					t.Fatal(err)
				}
			}
			before := setupDatabaseSnapshot(t, a)
			base := s.cfg
			ok(t, setupRequest(t, s, "connect", input, "http://localhost"), want)
			if s.completed || len(s.done) != 0 || !reflect.DeepEqual(s.cfg, base) || !reflect.DeepEqual(before, setupDatabaseSnapshot(t, a)) {
				t.Fatal("failed connection changed configuration")
			}
		})
	}
}

func TestSetupConnectRequiresExistingAdministrator(t *testing.T) {
	s := setupFixture(t)
	ok(t, setupRequest(t, s, "connect", setupValid(), "http://localhost"), 409)
	if ready, err := installationReady(s.cfg); err != nil || ready {
		t.Fatal("connect initialized a new administrator", err)
	}
	if _, err := os.Stat(s.path); !os.IsNotExist(err) {
		t.Fatal("connect saved bootstrap for uninitialized database", err)
	}
}

func TestSetupConnectRateLimitsVerification(t *testing.T) {
	s, _, input := existingSetupFixture(t)
	wrong := input
	wrong.Password = "incorrect"
	for range 6 {
		ok(t, setupRequest(t, s, "connect", wrong, "http://localhost"), 401)
	}
	w := setupRequest(t, s, "connect", input, "http://localhost")
	ok(t, w, 429)
	if w.Header().Get("Retry-After") != "60" || s.completed {
		t.Fatal("verification rate limit missing")
	}
	s.connectAttempts.Start = time.Now().Add(-2 * time.Minute)
	ok(t, setupRequest(t, s, "connect", input, "http://localhost"), 200)
}

func TestSetupConnectHonorsMySQLWorkerLock(t *testing.T) {
	if os.Getenv("XM_TEST_MYSQL_ADDR") == "" {
		t.Skip("set XM_TEST_MYSQL_ADDR for worker-lock integration")
	}
	s, a, input := existingSetupFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	release, err := a.store.AcquireDatabaseWorker(ctx, cancel, a.Config().Database.Name)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	ok(t, setupRequest(t, s, "database-test", input, "http://localhost"), 200)
	ok(t, setupRequest(t, s, "connect", input, "http://localhost"), 409)
	if _, err := os.Stat(s.path); !os.IsNotExist(err) {
		t.Fatal("busy database was adopted", err)
	}
	release()
	ok(t, setupRequest(t, s, "connect", input, "http://localhost"), 200)
}
