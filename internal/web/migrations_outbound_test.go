package web

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"xm/internal/archive"
)

func TestMigrationLocalToRemote(t *testing.T) {
	for _, kind := range []string{"s3", "webdav"} {
		t.Run(kind, func(t *testing.T) {
			a := testApp(t)
			t.Cleanup(a.migrations.stop)
			b := login(t, a)
			var mu sync.Mutex
			objects := map[string]string{}
			var corrupt, reject atomic.Bool
			var uploads, calls atomic.Int64
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if reject.Load() {
					w.WriteHeader(403)
					return
				}
				if kind == "s3" && !strings.HasPrefix(r.Header.Get("Authorization"), "AWS4-HMAC-SHA256 ") {
					t.Error("missing S3 signature")
				}
				if kind == "webdav" {
					u, p, ok := r.BasicAuth()
					if !ok || u != "fixture-user" || p != "fixture-password" {
						t.Error("missing WebDAV auth")
					}
				}
				mu.Lock()
				defer mu.Unlock()
				switch r.Method {
				case "MKCOL":
					w.WriteHeader(201)
				case "PUT":
					data, err := io.ReadAll(r.Body)
					if err != nil {
						t.Error(err)
					}
					objects[r.URL.Path] = string(data)
					if strings.Contains(r.URL.Path, "/media/") {
						uploads.Add(1)
					}
					w.WriteHeader(200)
				case "GET", "HEAD":
					value, ok := objects[r.URL.Path]
					if !ok {
						http.NotFound(w, r)
						return
					}
					if corrupt.Load() && strings.Contains(r.URL.Path, "/media/") && value == "local photo one" {
						value = "corrupt object"
					}
					http.ServeContent(w, r, "photo.jpg", time.Time{}, strings.NewReader(value))
				case "DELETE":
					if !strings.Contains(r.URL.Path, "/.xm-check/") {
						t.Error("deleted a source object")
					}
					delete(objects, r.URL.Path)
					w.WriteHeader(204)
				default:
					t.Error("unexpected method", r.Method)
					w.WriteHeader(405)
				}
			}))
			defer server.Close()
			profile := archive.StorageProfile{ID: "remote", Name: "目标存储", Kind: kind, Endpoint: server.URL, Timeout: 5, Prefix: "archive", Bucket: "fixture", Region: "us-east-1", AccessKey: "fixture-key", SecretKey: "fixture-secret", PathStyle: true, Username: "fixture-user", Password: "fixture-password"}
			cfg := a.Config()
			// Exercise both selecting an existing connection and creating one.
			input := migrationInput{Kind: "media", Source: "local", Target: "remote", Confirm: "开始迁移"}
			if kind == "s3" {
				cfg.StorageProfiles = append(cfg.StorageProfiles, profile)
				if err := a.store.SaveSettings(cfg); err != nil {
					t.Fatal(err)
				}
				a.config = cfg
			} else {
				input.Target = "new"
				input.Storage = &profile
			}
			localPath := func(id string) string {
				if id == "two" {
					return "media/users/alice_123/existing.jpg"
				}
				return "media/" + id + ".jpg"
			}
			for id, payload := range map[string]string{"one": "local photo one", "two": "local photo two"} {
				if err := a.store.SavePosts("alice", []archive.Post{{ID: id, Assets: []archive.Asset{{ID: "photo", Kind: "photo"}}}}); err != nil {
					t.Fatal(err)
				}
				if err := os.MkdirAll(filepath.Dir(filepath.Join(cfg.DataDir, localPath(id))), 0700); err != nil {
					t.Fatal(err)
				}
				rel := localPath(id)
				if err := os.WriteFile(filepath.Join(cfg.DataDir, rel), []byte(payload), 0600); err != nil {
					t.Fatal(err)
				}
				hash := sha256.Sum256([]byte(payload))
				if err := a.store.MediaResult(archive.MediaJob{PostID: id, ID: "photo", StorageID: "local"}, rel, int64(len(payload)), hex.EncodeToString(hash[:]), nil); err != nil {
					t.Fatal(err)
				}
			}
			ok(t, b.request("POST", "/api/migrations/preview", input, "http://localhost", true), 200)
			if calls.Load() != 0 || a.Config().ActiveStorage() != "local" {
				t.Fatal("preview modified storage")
			}
			// A failed connection check must not switch new downloads away from local.
			reject.Store(true)
			ok(t, b.request("POST", "/api/migrations", input, "http://localhost", true), 202)
			if j := waitMigration(t, a); j.Status != "failed" || a.Config().ActiveStorage() != "local" {
				t.Fatal("bad connection changed destination", j)
			}
			reject.Store(false)
			corrupt.Store(true)
			ok(t, b.request("POST", "/api/migrations", input, "http://localhost", true), 202)
			j := waitMigration(t, a)
			if j.Status != "failed" || j.Done != 1 || j.Failed != 1 {
				t.Fatalf("bad checksum outcome: %+v", j)
			}
			target := j.Target
			if a.Config().ActiveStorage() != target || target == "local" || target == "new" {
				t.Fatal("target not activated")
			}
			var storage string
			var attempts int
			if err := a.store.DB.QueryRow("SELECT storage_id,attempts FROM media WHERE post_id='one'").Scan(&storage, &attempts); err != nil {
				t.Fatal(err)
			}
			if storage != "local" || attempts != 1 {
				t.Fatal("failure changed local pointer or attempts")
			}
			for _, id := range []string{"one", "two"} {
				data, err := os.ReadFile(filepath.Join(cfg.DataDir, localPath(id)))
				if err != nil || string(data) != "local photo "+id {
					t.Fatal("source file changed", err)
				}
			}
			// Reconstruct the manager to exercise durable target selection on resume.
			a.migrations.stop()
			var err error
			a.migrations, err = newMigrationManager(a)
			if err != nil {
				t.Fatal(err)
			}
			corrupt.Store(false)
			before := uploads.Load()
			ok(t, b.request("POST", "/api/migrations", map[string]any{"retry": true, "confirm": "开始迁移"}, "http://localhost", true), 202)
			j = waitMigration(t, a)
			if j.Status != "completed" || j.Done != 1 || j.Target != target || uploads.Load() != before {
				t.Fatalf("resume did not reuse verified object: %+v", j)
			}
			var key string
			if err := a.store.DB.QueryRow("SELECT object_key FROM media WHERE post_id='one'").Scan(&key); err != nil || !strings.HasPrefix(key, "archive/media/users/") {
				t.Fatal("outbound path lost author layout", key, err)
			}
			if err := a.store.DB.QueryRow("SELECT object_key FROM media WHERE post_id='two'").Scan(&key); err != nil || key != "archive/"+localPath("two") {
				t.Fatal("existing users path was renamed", key, err)
			}
			response := b.request("GET", "/media/one/photo", nil, "", false)
			ok(t, response, 200)
			if response.Body.String() != "local photo one" {
				t.Fatal("stable media URL changed")
			}
			status := b.request("GET", "/api/migrations", nil, "", false)
			journal, err := os.ReadFile(a.migrations.journal())
			if err != nil {
				t.Fatal(err)
			}
			for _, secret := range []string{"fixture-secret", "fixture-key", "fixture-password"} {
				if strings.Contains(status.Body.String(), secret) || strings.Contains(string(journal), secret) {
					t.Fatal("credentials leaked")
				}
			}
			// A stable users path must not overwrite a different remote file.
			hash := sha256.Sum256([]byte("local photo two"))
			if err := a.store.MediaResult(archive.MediaJob{PostID: "two", ID: "photo"}, localPath("two"), int64(len("local photo two")), hex.EncodeToString(hash[:]), nil); err != nil {
				t.Fatal(err)
			}
			mu.Lock()
			for objectPath := range objects {
				if strings.HasSuffix(objectPath, key) {
					objects[objectPath] = "keep conflicting content"
				}
			}
			mu.Unlock()
			before = uploads.Load()
			ok(t, b.request("POST", "/api/migrations", migrationInput{Kind: "media", Source: "local", Target: target, Confirm: "开始迁移"}, "http://localhost", true), 202)
			if j = waitMigration(t, a); j.Status != "failed" || j.Failed != 1 || uploads.Load() != before {
				t.Fatal("conflicting remote file was overwritten", j)
			}
		})
	}
}

func TestMigrationSQLiteToMySQLCutover(t *testing.T) {
	testMigrationSQLiteToRemoteCutover(t, "mysql")
}

func TestMigrationSQLiteToMariaDBCutover(t *testing.T) {
	testMigrationSQLiteToRemoteCutover(t, "mariadb")
}

func testMigrationSQLiteToRemoteCutover(t *testing.T, driver string) {
	if os.Getenv("XM_TEST_MYSQL_ADDR") == "" {
		t.Skip("isolated loopback MySQL required")
	}
	destCfg := archive.DefaultConfig()
	destCfg.DataDir = t.TempDir()
	configureMySQLTest(t, &destCfg)
	destCfg.Database.Driver = driver
	// Always start with SQLite even when the test suite's default is MySQL.
	cfg := archive.DefaultConfig()
	cfg.DataDir = t.TempDir()
	source, err := archive.OpenStore(cfg.DataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	writeTestAdmin(t, source, cfg)
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	if err = archive.SaveBootstrap(configPath, cfg); err != nil {
		t.Fatal(err)
	}
	a, err := New(configPath, cfg, source, false)
	if err != nil {
		t.Fatal(err)
	}
	defer a.migrations.stop()
	defer a.scheduler.closeSession()
	b := login(t, a)
	if err = source.SavePosts("alice", []archive.Post{{ID: "184467440737095516151", Text: "迁移\x00中文 😀", Raw: json.RawMessage(`{"x":null}`)}}); err != nil {
		t.Fatal(err)
	}
	seedMonitor(t, a, "alice")
	if err = source.SaveCookies(archive.Cookies{AuthToken: "outbound-auth", CSRF: "outbound-csrf"}); err != nil {
		t.Fatal(err)
	}
	input := migrationInput{Kind: "database", Source: "sqlite", Target: driver, Database: destCfg.Database, Confirm: "开始迁移"}
	putTestBookmark(t, a, "184467440737095516151", 123)
	ok(t, b.request("POST", "/api/migrations/preview", input, "http://localhost", true), 200)
	probe, err := archive.OpenMigrationMySQL(context.Background(), destCfg.Database)
	if err != nil {
		t.Fatal(err)
	}
	defer probe.Close()
	var count int
	if err = probe.DB.QueryRow("SELECT COUNT(*) FROM information_schema.tables WHERE table_schema=DATABASE()").Scan(&count); err != nil || count != 0 {
		t.Fatal("preview created tables", err, count)
	}
	lockCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	release, err := probe.AcquireDatabaseWorker(lockCtx, cancel, destCfg.Database.Name)
	if err != nil {
		t.Fatal(err)
	}
	ok(t, b.request("POST", "/api/migrations", input, "http://localhost", true), 202)
	if j := waitMigration(t, a); j.Status != "failed" || a.maintenance.Load() {
		t.Fatal("worker lock not respected", j)
	}
	release()
	// MySQL rejects oversized indexed fields; earlier table writes must roll back.
	if _, err = source.DB.Exec("UPDATE posts SET author_id=?", strings.Repeat("x", 65)); err != nil {
		t.Fatal(err)
	}
	ok(t, b.request("POST", "/api/migrations", input, "http://localhost", true), 202)
	if failed := waitMigration(t, a); failed.Status != "failed" || a.maintenance.Load() {
		t.Fatal("copy error did not abort cutover", failed)
	}
	if err = probe.CheckMigrationEmpty(context.Background()); err != nil {
		t.Fatal("partial copy not rolled back", err)
	}
	unchanged, err := archive.LoadBootstrap(configPath)
	if err != nil || unchanged.Database.DriverName() != "sqlite" {
		t.Fatal("failed copy changed bootstrap", err)
	}
	if _, err = source.DB.Exec("UPDATE posts SET author_id='' "); err != nil {
		t.Fatal(err)
	}
	ok(t, b.request("POST", "/api/migrations", input, "http://localhost", true), 202)
	j := waitMigration(t, a)
	if j.Status != "switching" || !a.maintenance.Load() || j.Target != driver || j.Done != j.Total {
		t.Fatalf("cutover failed: %+v", j)
	}
	nextCfg, err := archive.LoadConfig(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if nextCfg.Database.DriverName() != driver || nextCfg.Database.Name != destCfg.Database.Name {
		t.Fatal("bootstrap not switched")
	}
	dst, err := archive.OpenConfiguredStore(nextCfg)
	if err != nil {
		t.Fatal(err)
	}
	defer dst.Close()
	next, err := New(configPath, nextCfg, dst, false)
	if err != nil {
		t.Fatal(err)
	}
	defer next.migrations.stop()
	defer next.scheduler.closeSession()
	if next.migrations.snapshot().Status != "completed" {
		t.Fatal("restart did not reconcile")
	}
	nextBrowser := login(t, next)
	var bookmarkAt int64
	if err = dst.DB.QueryRow("SELECT created_at FROM post_bookmarks WHERE post_id='184467440737095516151'").Scan(&bookmarkAt); err != nil || bookmarkAt != 123 {
		t.Fatal("MySQL cutover lost bookmark", err)
	}
	var value string
	if err = dst.DB.QueryRow("SELECT text FROM posts WHERE id=?", "184467440737095516151").Scan(&value); err != nil || value != "迁移\x00中文 😀" {
		t.Fatal("text changed", err)
	}
	cookies, err := dst.LoadCookies()
	if err != nil || cookies.AuthToken != "outbound-auth" {
		t.Fatal("cookies changed", err)
	}
	if err = source.DB.QueryRow("SELECT COUNT(*) FROM posts").Scan(&count); err != nil || count != 1 {
		t.Fatal("source changed", err)
	}
	if err = probe.CheckMigrationEmpty(context.Background()); err == nil {
		t.Fatal("nonempty target accepted")
	}
	if driver == "mariadb" {
		// The configured backend must remain usable after restart, including the
		// existing return-to-SQLite path and its source label.
		back := migrationInput{Kind: "database", Source: driver, Target: "sqlite", Confirm: "开始迁移"}
		ok(t, nextBrowser.request("POST", "/api/migrations/preview", back, "http://localhost", true), 200)
		ok(t, nextBrowser.request("POST", "/api/migrations", back, "http://localhost", true), 202)
		if returned := waitMigration(t, next); returned.Status != "switching" || returned.Source != driver {
			t.Fatalf("MariaDB return migration failed: %+v", returned)
		}
	}
}

func TestMigrationDirectionsRejectInvalidRequests(t *testing.T) {
	a := testApp(t)
	b := login(t, a)
	for _, input := range []migrationInput{
		{Kind: "database", Target: a.Config().Database.DriverName()},
		{Kind: "media", Source: "local", Target: "local"},
		{Kind: "media", Source: "missing", Target: "local"},
		{Kind: "media", Source: "local", Target: "new"},
		{Kind: "media", Source: "local", Target: "missing"},
	} {
		ok(t, b.request("POST", "/api/migrations/preview", input, "http://localhost", true), 400)
	}
}

func TestMariaDBMigrationDirections(t *testing.T) {
	for _, tc := range []struct {
		source, target, claimedSource string
		valid                         bool
	}{
		{"sqlite", "mariadb", "sqlite", true},
		{"sqlite", "mysql", "sqlite", true},
		{"mariadb", "sqlite", "mariadb", true},
		{"mariadb", "", "", true},
		{"mariadb", "mariadb", "", false},
		{"mariadb", "mysql", "", false},
		{"mysql", "mariadb", "", false},
		{"sqlite", "mariadb", "mysql", false},
		{"sqlite", "unknown", "", false},
	} {
		t.Run(tc.source+"_to_"+tc.target+"_from_"+tc.claimedSource, func(t *testing.T) {
			a := &App{configPath: filepath.Join(t.TempDir(), "config.yaml"), config: archive.Config{Database: archive.DatabaseConfig{Driver: tc.source}}}
			input := migrationInput{Kind: "database", Source: tc.claimedSource, Target: tc.target, Database: archive.DatabaseConfig{Driver: "mysql", Host: "localhost", Name: "archive", User: "xm", SQLiteFile: "old.db", CAFile: "ca.pem"}}
			err := a.prepareMigration(&input)
			if (err == nil) != tc.valid {
				t.Fatalf("unexpected validation: %v", err)
			}
			if tc.valid && input.Target == "mariadb" && (input.Database.Driver != "mariadb" || input.Database.SQLiteFile != "" || !filepath.IsAbs(input.Database.CAFile)) {
				t.Fatalf("MariaDB destination not normalized: %+v", input.Database)
			}
			if tc.valid && tc.source == "mariadb" && input.Target != "sqlite" {
				t.Fatal("MariaDB must default to SQLite")
			}
		})
	}
}
