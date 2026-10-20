package web

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	"xm/internal/archive"
)

func setupFixture(t *testing.T) *setupWizard {
	t.Helper()
	dir := t.TempDir()
	cfg := archive.Config{DataDir: filepath.Join(dir, "data"), Headless: true, RequestTimeout: 5, ScrollDelay: 1, MaxScrolls: 2, MaxMediaBytes: 1024, DefaultInterval: 3600}
	if err := archive.PrivateDir(cfg.DataDir); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "config.yaml")
	return newSetup(path, cfg)
}
func setupRequest(t *testing.T, s *setupWizard, route string, v any, origin string) *httptest.ResponseRecorder {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest("POST", "http://localhost/api/setup/"+route, bytes.NewReader(b))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Origin", origin)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	return w
}
func setupValid() setupInput {
	return setupInput{Username: "owner", Password: "setup888", Database: archive.DatabaseConfig{Driver: "sqlite"}, StorageKind: "local"}
}

func TestSetupRejectsNonLoopbackListener(t *testing.T) {
	cfg := archive.DefaultConfig()
	cfg.DataDir = t.TempDir()
	_, err := Setup(context.Background(), filepath.Join(t.TempDir(), "config.yaml"), cfg, "0.0.0.0:0", false)
	if err == nil || !strings.Contains(err.Error(), "回环地址") {
		t.Fatalf("non-loopback setup listener was accepted: %v", err)
	}
}

func TestSetupCompletionLoginAndLockedStorage(t *testing.T) {
	s := setupFixture(t)
	v := setupValid()
	health := httptest.NewRecorder()
	s.Handler().ServeHTTP(health, httptest.NewRequest(http.MethodGet, "http://localhost/healthz", nil))
	if health.Code != http.StatusOK || health.Body.String() != "setup\n" {
		t.Fatalf("setup health response = %d %q", health.Code, health.Body.String())
	}
	ok(t, setupRequest(t, s, "defaults", nil, "http://localhost"), 200)
	ok(t, setupRequest(t, s, "complete", v, "http://evil.example"), 403)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, httptest.NewRequest("GET", "http://localhost/", nil))
	if w.Code != 200 || !strings.Contains(w.Body.String(), "首次设置") || strings.Contains(w.Body.String(), "setup-code") {
		t.Fatal("unexpected setup page")
	}
	ok(t, setupRequest(t, s, "database-test", v, "http://localhost"), 200)
	ok(t, setupRequest(t, s, "storage-test", setupInput{Database: v.Database, StorageKind: "local"}, "http://localhost"), 200)
	if _, err := os.Stat(s.path); !os.IsNotExist(err) {
		t.Fatal("connection tests created bootstrap before completion")
	}
	ok(t, setupRequest(t, s, "complete", v, "http://localhost"), 200)
	c := <-s.done
	ok(t, setupRequest(t, s, "complete", v, "http://localhost"), 409)
	adminStore, err := archive.OpenConfiguredStore(c)
	if err != nil {
		t.Fatal(err)
	}
	record, err := adminStore.AdminCredentials()
	adminStore.Close()
	if err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(filepath.Join(c.DataDir, "admin.json")); !os.IsNotExist(err) {
		t.Fatal("administrator file created")
	}
	if bytes.Equal(record.Hash, []byte(v.Password)) || len(record.Hash) != 32 || !record.ConfigurationLocked {
		t.Fatal("credentials not hashed or config not locked")
	}
	saved, err := archive.LoadConfig(s.path)
	if err != nil || saved.ActiveStorage() != "local" {
		t.Fatal(saved.ActiveStorage(), err)
	}
	store, err := archive.OpenConfiguredStore(c)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	var n int
	if err = store.DB.QueryRow("SELECT COUNT(*) FROM web_state").Scan(&n); err != nil || n != 0 {
		t.Fatal("probe wrote persistent rows", n, err)
	}
	a, err := New(s.path, c, store, false)
	if err != nil {
		t.Fatal(err)
	}
	b := &browserClient{t: t, a: a}
	ok(t, b.request("GET", "/api/overview", nil, "", false), 401)
	ok(t, b.request("POST", "/api/login", map[string]string{"username": "other", "password": v.Password}, "http://localhost", false), 401)
	ok(t, b.request("POST", "/api/login", map[string]string{"password": v.Password}, "http://localhost", false), 401)
	r := b.request("POST", "/api/login", map[string]string{"username": v.Username, "password": v.Password}, "http://localhost", false)
	ok(t, r, 200)
	var session map[string]string
	json.Unmarshal(r.Body.Bytes(), &session)
	b.csrf = session["csrf"]
	b.cookie = r.Result().Cookies()[0]
	for _, route := range []string{"/api/storage/profiles", "/api/storage/active", "/api/storage/profiles/unused/test"} {
		method := "POST"
		if strings.HasSuffix(route, "active") {
			method = "PUT"
		}
		ok(t, b.request(method, route, map[string]string{"id": "local"}, "http://localhost", true), 403)
	}
	r = b.request("GET", "/api/storage", nil, "", false)
	ok(t, r, 200)
	if !strings.Contains(r.Body.String(), `"locked":true`) {
		t.Fatal("lock not exposed to UI")
	}
	ok(t, b.request("PUT", "/api/settings", map[string]any{"database": map[string]string{"driver": "mysql"}}, "http://localhost", true), 400)
	ok(t, b.request("PUT", "/api/settings", map[string]any{"default_interval_seconds": 7200}, "http://localhost", true), 200)
	ok(t, b.request("POST", "/api/cookies", map[string]string{"content": "auth_token=fixture; ct0=fixture"}, "http://localhost", true), 200)
	a2, err := New(s.path, c, store, false)
	if err != nil || !a2.auth.record.ConfigurationLocked {
		t.Fatal("lock lost after restart", err)
	}
	if _, err = Setup(context.Background(), s.path, c, "not-a-listen-address", false); err != nil {
		t.Fatal("configured installation opened wizard", err)
	}
}
func TestSetupFailureRetryAndExistingArchive(t *testing.T) {
	s := setupFixture(t)
	v := setupValid()
	v.Password = "1234567"
	ok(t, setupRequest(t, s, "complete", v, "http://localhost"), 400)
	v = setupValid()
	v.Database = archive.DatabaseConfig{Driver: "mysql", Host: "127.0.0.1", Port: 1, Name: "xm", User: "xm", TLSMode: "disabled"}
	ok(t, setupRequest(t, s, "complete", v, "http://localhost"), 400)
	old, _ := os.ReadFile(s.path)
	failure := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(403) }))
	defer failure.Close()
	v = setupValid()
	v.StorageKind = "webdav"
	v.Storage = archive.StorageProfile{Endpoint: failure.URL, Timeout: 5}
	ok(t, setupRequest(t, s, "complete", v, "http://localhost"), 400)
	now, _ := os.ReadFile(s.path)
	if !bytes.Equal(old, now) {
		t.Fatal("failed setup changed configuration")
	}
	if _, err := os.Stat(filepath.Join(s.cfg.DataDir, "admin.json")); !os.IsNotExist(err) {
		t.Fatal("failed setup locked out retry")
	}
	store, err := archive.OpenStore(s.cfg.DataDir)
	if err != nil {
		t.Fatal(err)
	}
	if err = store.SavePosts("alice", []archive.Post{{ID: "1", Text: "existing archive"}}); err != nil {
		t.Fatal(err)
	}
	store.Close()
	v = setupValid()
	v.Database = archive.DatabaseConfig{Driver: "mysql", Host: "127.0.0.1", Port: 1, Name: "xm", User: "xm", TLSMode: "disabled"}
	w := setupRequest(t, s, "database-test", v, "http://localhost")
	ok(t, w, 400)
	if !strings.Contains(w.Body.String(), "migrate-mysql") {
		t.Fatal("existing archive switch not blocked")
	}
	ok(t, setupRequest(t, s, "complete", setupValid(), "http://localhost"), 200)
}
func TestSetupRemoteStorageAndConcurrentCompletion(t *testing.T) {
	for _, kind := range []string{"webdav", "s3"} {
		t.Run(kind, func(t *testing.T) {
			s := setupFixture(t)
			var mu sync.Mutex
			objects := map[string][]byte{}
			remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				defer mu.Unlock()
				if r.Header.Get("Cookie") != "" {
					t.Error("cookie sent to storage")
				}
				switch r.Method {
				case "MKCOL":
					w.WriteHeader(201)
				case "PUT":
					objects[r.URL.Path], _ = io.ReadAll(r.Body)
					w.WriteHeader(200)
				case "HEAD", "GET":
					data, ok := objects[r.URL.Path]
					if !ok {
						w.WriteHeader(404)
						return
					}
					w.Header().Set("Content-Length", strconv.Itoa(len(data)))
					if r.Method == "GET" {
						w.Write(data)
					}
				case "DELETE":
					delete(objects, r.URL.Path)
					w.WriteHeader(204)
				default:
					w.WriteHeader(405)
				}
			}))
			defer remote.Close()
			v := setupValid()
			v.StorageKind = kind
			v.Storage = archive.StorageProfile{Name: "Archive", Endpoint: remote.URL, Prefix: "xm", Bucket: "test-bucket", Region: "us-east-1", AccessKey: "test-key", SecretKey: "test-secret", Password: "test-password", PathStyle: true, Timeout: 5}
			var wg sync.WaitGroup
			results := make(chan int, 2)
			for i := 0; i < 2; i++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					results <- setupRequest(t, s, "complete", v, "http://localhost").Code
				}()
			}
			wg.Wait()
			close(results)
			seen := map[int]int{}
			for code := range results {
				seen[code]++
			}
			if seen[200] != 1 || seen[409] != 1 {
				t.Fatal(seen)
			}
			cfg, err := archive.LoadConfig(s.path)
			if err != nil {
				t.Fatal(err)
			}
			p, err := cfg.Storage(cfg.ActiveStorage())
			if err != nil || p.Kind != kind {
				t.Fatal("remote choice not persisted", err)
			}
			mu.Lock()
			defer mu.Unlock()
			if len(objects) != 0 {
				t.Fatal("test object not removed")
			}
		})
	}
}
func TestSetupMySQL(t *testing.T) {
	if os.Getenv("XM_TEST_MYSQL_ADDR") == "" {
		t.Skip("requires isolated MySQL")
	}
	s := setupFixture(t)
	cfg := s.cfg
	configureMySQLTest(t, &cfg)
	v := setupValid()
	v.Database = cfg.Database
	ok(t, setupRequest(t, s, "database-test", v, "http://localhost"), 200)
	ok(t, setupRequest(t, s, "complete", v, "http://localhost"), 200)
	saved, err := archive.LoadConfig(s.path)
	if err != nil || saved.Database.Driver != "mysql" {
		t.Fatal(err)
	}
	store, err := archive.OpenConfiguredStore(saved)
	if err != nil {
		t.Fatal(err)
	}
	store.Close()
}

func TestSetupPreservesExplicitSQLiteFilename(t *testing.T) {
	s := setupFixture(t)
	s.cfg.Database = archive.DatabaseConfig{Driver: "sqlite", SQLiteFile: "existing.db"}
	cfg, err := s.candidate(setupValid())
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Database.SQLiteFile != "existing.db" {
		t.Fatal("setup discarded explicit SQLite path")
	}
	store, err := archive.OpenConfiguredStore(s.cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err = store.SavePosts("alice", []archive.Post{{ID: "42"}}); err != nil {
		t.Fatal(err)
	}
	store.Close()
	mysqlCfg := s.cfg
	mysqlCfg.Database = archive.DatabaseConfig{Driver: "mysql", Host: "127.0.0.1", Name: "fixture", User: "fixture"}
	if err = s.guardLocalArchive(mysqlCfg); err == nil {
		t.Fatal("custom SQLite archive was not protected")
	}
}

func TestSetupRequiresCompleteInitializationRecord(t *testing.T) {
	s := setupFixture(t)
	ready, err := installationReady(s.cfg)
	if err != nil || ready {
		t.Fatal("fresh database initialized", err)
	}
	record, err := makePasswordRecord("owner", "setup-password-2026")
	if err != nil {
		t.Fatal(err)
	}
	store, err := archive.OpenConfiguredStore(s.cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err = store.Initialize(s.cfg, record); err != nil {
		t.Fatal(err)
	}
	ready, err = installationReady(s.cfg)
	if err != nil || !ready {
		t.Fatal("database initialization not detected", err)
	}
	// SQL constraints reject malformed hashes and incomplete initialization.
	if _, err = store.DB.Exec("UPDATE admin_accounts SET password_hash=? WHERE id=1", []byte("bad")); err == nil {
		t.Fatal("malformed hash accepted by schema")
	}
	if _, err = store.DB.Exec("UPDATE admin_accounts SET username='' WHERE id=1"); err != nil {
		t.Fatal(err)
	}
	if _, err = installationReady(s.cfg); err == nil {
		t.Fatal("corrupt credentials exposed setup wizard")
	}

}

func TestSetupBootstrapFailureDoesNotCreateAdministrator(t *testing.T) {
	s := setupFixture(t)
	s.path = t.TempDir() // A directory cannot be atomically replaced by config YAML.
	ok(t, setupRequest(t, s, "complete", setupValid(), "http://localhost"), 500)
	ready, err := installationReady(s.cfg)
	if err != nil || ready {
		t.Fatal("bootstrap failure committed administrator", err)
	}
}

func TestSetupRejectsAnotherInitializedDatabase(t *testing.T) {
	s := setupFixture(t)
	other := setupFixture(t)
	ok(t, setupRequest(t, other, "complete", setupValid(), "http://localhost"), 200)
	// A competing setup must not overwrite the owner even with a different config path.
	competing := newSetup(s.path, other.cfg)
	ok(t, setupRequest(t, competing, "complete", setupValid(), "http://localhost"), 409)
}
