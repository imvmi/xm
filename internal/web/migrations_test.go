package web

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"xm/internal/archive"
)

func waitMigration(t *testing.T, a *App) *migrationJob {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		a.migrations.mu.Lock()
		running := a.migrations.running
		a.migrations.mu.Unlock()
		if !running {
			return a.migrations.snapshot()
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("migration did not stop")
	return nil
}
func migrationS3Fixture(t *testing.T, a *App) (*atomic.Bool, *atomic.Int64) {
	t.Helper()
	payload := "verified migration photo"
	var corrupt atomic.Bool
	var calls atomic.Int64
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" {
			t.Errorf("migration must only read S3: %s", r.Method)
			w.WriteHeader(405)
			return
		}
		if !strings.HasPrefix(r.Header.Get("Authorization"), "AWS4-HMAC-SHA256 ") {
			t.Error("unsigned S3 request")
		}
		calls.Add(1)
		body := payload
		if corrupt.Load() && strings.HasSuffix(r.URL.Path, "/photo.jpg") {
			body = "corrupt"
		}
		http.ServeContent(w, r, "photo.jpg", time.Time{}, strings.NewReader(body))
	}))
	t.Cleanup(remote.Close)
	cfg := a.Config()
	cfg.StorageProfiles = []archive.StorageProfile{{ID: "s3-test", Name: "Test S3", Kind: "s3", Endpoint: remote.URL, Bucket: "fixture", Region: "us-east-1", AccessKey: "fake-key", SecretKey: "fake-secret", Timeout: 5, PathStyle: true}}
	cfg.MediaStorage = "s3-test"
	if err := a.store.SaveSettings(cfg); err != nil {
		t.Fatal(err)
	}
	a.config = cfg
	if err := a.store.SavePosts("alice", []archive.Post{{ID: "10", Text: "中文 😀", Raw: json.RawMessage(`{}`), Assets: []archive.Asset{{ID: "photo", Kind: "photo"}}}}); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(payload))
	if err := a.store.MediaResult(archive.MediaJob{PostID: "10", ID: "photo", StorageID: "s3-test", ObjectKey: "media/photo.jpg"}, "", int64(len(payload)), hex.EncodeToString(sum[:]), nil); err != nil {
		t.Fatal(err)
	}
	return &corrupt, &calls
}
func TestMigrationMediaValidationRetryAndSafety(t *testing.T) {
	a := testApp(t)
	corrupt, calls := migrationS3Fixture(t, a)
	if err := a.store.SavePosts("alice", []archive.Post{{ID: "11", Assets: []archive.Asset{{ID: "second", Kind: "photo"}}}}); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte("verified migration photo"))
	if err := a.store.MediaResult(archive.MediaJob{PostID: "11", ID: "second", StorageID: "s3-test", ObjectKey: "media/second.jpg"}, "", int64(len("verified migration photo")), hex.EncodeToString(sum[:]), nil); err != nil {
		t.Fatal(err)
	}
	b := login(t, a)
	t.Cleanup(a.migrations.stop)
	corrupt.Store(true)
	unauth := httptest.NewRecorder()
	a.Handler().ServeHTTP(unauth, httptest.NewRequest("GET", "/api/migrations", nil))
	ok(t, unauth, 401)
	ok(t, b.request("GET", "/api/migrations/preview?kind=media&source=s3-test", nil, "", false), 200)
	if calls.Load() != 0 || a.Config().ActiveStorage() != "s3-test" {
		t.Fatal("preview changed storage or contacted S3")
	}
	if _, err := os.Stat(a.migrations.journal()); !os.IsNotExist(err) {
		t.Fatal("preview wrote migration journal")
	}
	input := map[string]any{"kind": "media", "source": "s3-test", "confirm": "开始迁移"}
	ok(t, b.request("POST", "/api/migrations", input, "http://evil.example", true), 403)
	ok(t, b.request("POST", "/api/migrations", input, "http://localhost", false), 403)
	ok(t, b.request("POST", "/api/migrations", map[string]string{"kind": "media", "source": "s3-test"}, "http://localhost", true), 400)
	ok(t, b.request("POST", "/api/migrations", input, "http://localhost", true), 202)
	j := waitMigration(t, a)
	if j.Status != "failed" || j.Failed != 1 || j.Done != 1 || len(j.Failures) != 1 {
		t.Fatalf("expected failed checksum: %+v", j)
	}
	var storage string
	var attempts int
	if err := a.store.DB.QueryRow("SELECT storage_id,attempts FROM media WHERE post_id='10'").Scan(&storage, &attempts); err != nil {
		t.Fatal(err)
	}
	if storage != "s3-test" || attempts != 1 {
		t.Fatal("failure changed source pointer or download attempts")
	}
	if a.Config().ActiveStorage() != "local" {
		t.Fatal("new downloads were not switched to local")
	}
	corrupt.Store(false)
	callsBeforeRetry := calls.Load()
	ok(t, b.request("POST", "/api/migrations", map[string]any{"retry": true, "confirm": "开始迁移"}, "http://localhost", true), 202)
	j = waitMigration(t, a)
	if j.Status != "completed" || j.Done != 1 || j.Failed != 0 {
		t.Fatalf("retry failed: %+v", j)
	}
	if calls.Load() != callsBeforeRetry+1 {
		t.Fatal("retry redownloaded completed media")
	}
	response := b.request("GET", "/media/10/photo", nil, "", false)
	ok(t, response, 200)
	if response.Body.String() != "verified migration photo" {
		t.Fatal("stable URL did not read local copy")
	}
	beforeCalls := calls.Load()
	ok(t, b.request("POST", "/api/migrations", input, "http://localhost", true), 202)
	j = waitMigration(t, a)
	if j.Status != "completed" || j.Total != 0 || calls.Load() != beforeCalls {
		t.Fatal("already migrated media was fetched again")
	}
	status := b.request("GET", "/api/migrations", nil, "", false)
	if strings.Contains(status.Body.String(), "fake-secret") || strings.Contains(status.Body.String(), "fake-key") {
		t.Fatal("credential leaked")
	}
	info, err := os.Stat(a.migrations.journal())
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("unsafe progress file permissions", err)
	}
}

func TestMigrationMaintenancePauseAndRestartRecovery(t *testing.T) {
	a := testApp(t)
	migrationS3Fixture(t, a)
	b := login(t, a)
	t.Cleanup(a.migrations.stop)
	a.writeGate.RLock() // simulate an existing task that must drain first
	ok(t, b.request("POST", "/api/migrations", map[string]string{"kind": "media", "source": "s3-test", "confirm": "开始迁移"}, "http://localhost", true), 202)
	ok(t, b.request("PUT", "/api/settings", map[string]bool{"polling_paused": true}, "http://localhost", true), 503)
	ok(t, b.request("GET", "/api/migrations", nil, "", false), 200)
	ok(t, b.request("POST", "/api/migrations", map[string]string{"kind": "media", "source": "s3-test", "confirm": "开始迁移"}, "http://localhost", true), 409)
	if worked, e := a.scheduler.step(context.Background()); e != nil || worked {
		t.Fatal("scheduler ran during maintenance", e)
	}
	if worked, e := a.notifier.step(context.Background()); e != nil || worked {
		t.Fatal("notifier ran during maintenance", e)
	}
	ok(t, b.request("POST", "/api/migrations/pause", nil, "http://localhost", true), 200)
	j := waitMigration(t, a)
	a.writeGate.RUnlock()
	if j.Status != "paused" || a.maintenance.Load() || a.Config().ActiveStorage() != "s3-test" {
		t.Fatalf("pause during drain changed data: %+v", j)
	}
	ok(t, b.request("PUT", "/api/settings", map[string]bool{"polling_paused": true}, "http://localhost", true), 200)
	if err := a.migrations.update(func(j *migrationJob) { j.Status = "running" }); err != nil {
		t.Fatal(err)
	}
	recovered, err := newMigrationManager(a)
	if err != nil {
		t.Fatal(err)
	}
	if recovered.snapshot().Status != "paused" || recovered.running {
		t.Fatal("interrupted migration resumed without confirmation")
	}
}

func TestMigrationSQLiteCopyValidationAndCancellation(t *testing.T) {
	a := testApp(t)
	if err := a.store.SavePosts("alice", []archive.Post{{ID: "184467440737095516151", Text: "空白\x00与中文 😀", Raw: json.RawMessage(`{"x":null}`)}}); err != nil {
		t.Fatal(err)
	}
	seedMonitor(t, a, "alice")
	putTestBookmark(t, a, "184467440737095516151", 123)
	dst, err := archive.OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer dst.Close()
	if err = migrate(dst); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err = a.store.CopyToSQLite(ctx, dst, nil); err == nil {
		t.Fatal("canceled copy succeeded")
	}
	var n int
	dst.DB.QueryRow("SELECT COUNT(*) FROM storage_profiles").Scan(&n)
	if n != 0 {
		t.Fatal("canceled copy committed data")
	}
	var done, total int64
	if err = a.store.CopyToSQLite(context.Background(), dst, func(_ string, d, t int64) error { done, total = d, t; return nil }); err != nil {
		t.Fatal(err)
	}
	if done != total || total == 0 {
		t.Fatalf("bad progress %d/%d", done, total)
	}
	var bookmarkAt int64
	if err = dst.DB.QueryRow("SELECT created_at FROM post_bookmarks WHERE post_id='184467440737095516151'").Scan(&bookmarkAt); err != nil || bookmarkAt != 123 {
		t.Fatal("bookmark not copied", err)
	}
	if _, ready, e := loadAdmin(dst); e != nil || !ready {
		t.Fatal("admin BLOBs changed", e)
	}
	var text string
	if err = dst.DB.QueryRow("SELECT text FROM posts WHERE id=?", "184467440737095516151").Scan(&text); err != nil || text != "空白\x00与中文 😀" {
		t.Fatal("content changed", err)
	}
	if err = a.store.CopyToSQLite(context.Background(), dst, nil); err == nil {
		t.Fatal("nonempty destination accepted")
	}
}

func TestMigrationMySQLCutover(t *testing.T) {
	if os.Getenv("XM_TEST_MYSQL_ADDR") == "" {
		t.Skip("isolated loopback MySQL required")
	}
	a := testApp(t)
	b := login(t, a)
	t.Cleanup(a.migrations.stop)
	if err := a.store.SavePosts("alice", []archive.Post{{ID: "184467440737095516151", Text: "数据库迁移 😀", Raw: json.RawMessage(`{}`)}}); err != nil {
		t.Fatal(err)
	}
	seedMonitor(t, a, "alice")
	if err := a.store.SaveCookies(archive.Cookies{AuthToken: "fixture-auth", CSRF: "fixture-csrf"}); err != nil {
		t.Fatal(err)
	}
	oldFile := filepath.Join(a.Config().DataDir, "archive.sqlite")
	if err := os.WriteFile(oldFile, []byte("untouched old archive"), 0600); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(a.configPath)
	if err != nil {
		t.Fatal(err)
	}
	ok(t, b.request("GET", "/api/migrations/preview?kind=database", nil, "", false), 200)
	unchanged, _ := os.ReadFile(a.configPath)
	if string(before) != string(unchanged) {
		t.Fatal("preview changed bootstrap")
	}
	ok(t, b.request("POST", "/api/migrations", map[string]string{"kind": "database", "confirm": "开始迁移"}, "http://localhost", true), 202)
	j := waitMigration(t, a)
	if j.Status != "switching" {
		t.Fatalf("copy failed: %+v", j)
	}
	if j.TargetFile != archive.DefaultSQLiteFile {
		t.Fatal("migration did not choose xm.db", j.TargetFile)
	}
	select {
	case <-a.restart:
	default:
		t.Fatal("restart not requested")
	}
	if !a.maintenance.Load() {
		t.Fatal("old database writes resumed after cutover")
	}
	cfg, err := archive.LoadConfig(a.configPath)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Database.DriverName() != "sqlite" || cfg.Database.SQLiteFile != j.TargetFile {
		t.Fatal("bootstrap not switched")
	}
	dst, err := archive.OpenConfiguredStore(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer dst.Close()
	next, err := New(a.configPath, cfg, dst, false)
	if err != nil {
		t.Fatal(err)
	}
	if next.migrations.snapshot().Status != "completed" {
		t.Fatal("cutover recovery not completed")
	}
	login(t, next)
	cookies, err := dst.LoadCookies()
	if err != nil || cookies.AuthToken != "fixture-auth" {
		t.Fatal("cookies not migrated", err)
	}
	var value string
	err = dst.DB.QueryRow("SELECT text FROM posts WHERE id='184467440737095516151'").Scan(&value)
	if err != nil || value != "数据库迁移 😀" {
		t.Fatal("content not preserved", err)
	}
	saved, _ := os.ReadFile(oldFile)
	if string(saved) != "untouched old archive" {
		t.Fatal("existing SQLite overwritten")
	}
	var count int
	a.store.DB.QueryRow("SELECT COUNT(*) FROM posts").Scan(&count)
	if count != 1 {
		t.Fatal("source database modified")
	}
	backups, _ := filepath.Glob(filepath.Join(cfg.DataDir, "migration-db-*", "bootstrap-before.yaml"))
	if len(backups) != 1 {
		t.Fatal("missing bootstrap backup")
	}
	saved, _ = os.ReadFile(backups[0])
	if string(saved) != string(before) {
		t.Fatal("backup differs")
	}
}

func TestMigrationMediaRetryAfterIndexFailure(t *testing.T) {
	a := testApp(t)
	if a.store.DB.Dialect != "sqlite" {
		t.Skip("SQLite fault-injection trigger")
	}
	_, calls := migrationS3Fixture(t, a)
	var item archive.MigrationMedia
	if err := a.store.DB.QueryRow("SELECT post_id,media_id,storage_id,object_key,bytes,sha256 FROM media WHERE post_id='10'").Scan(&item.PostID, &item.MediaID, &item.StorageID, &item.Key, &item.Bytes, &item.SHA256); err != nil {
		t.Fatal(err)
	}
	if _, err := a.store.DB.Exec(`CREATE TRIGGER fail_media_migration BEFORE UPDATE OF storage_id ON media BEGIN SELECT RAISE(ABORT,'fixture index failure'); END`); err != nil {
		t.Fatal(err)
	}
	if err := a.store.LocalizeMedia(context.Background(), a.Config(), item); err == nil {
		t.Fatal("index failure ignored")
	}
	var storage string
	a.store.DB.QueryRow("SELECT storage_id FROM media WHERE post_id='10'").Scan(&storage)
	if storage != "s3-test" {
		t.Fatal("failed commit changed reference")
	}
	if _, err := a.store.DB.Exec("DROP TRIGGER fail_media_migration"); err != nil {
		t.Fatal(err)
	}
	count := calls.Load()
	if err := a.store.LocalizeMedia(context.Background(), a.Config(), item); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != count {
		t.Fatal("verified local file was downloaded again after interrupted commit")
	}
}

func TestMigrationCopyRollsBackMidwayAndRejectsUnknownColumns(t *testing.T) {
	a := testApp(t)
	dst, err := archive.OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer dst.Close()
	if err = migrate(dst); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err = a.store.CopyToSQLite(ctx, dst, func(phase string, done, total int64) error {
		if done > 0 {
			cancel()
			return ctx.Err()
		}
		return nil
	}); err == nil {
		t.Fatal("partial canceled copy committed")
	}
	var count int
	if err = dst.DB.QueryRow("SELECT COUNT(*) FROM storage_profiles").Scan(&count); err != nil || count != 0 {
		t.Fatal("partial transaction not rolled back", err)
	}
	if _, err = a.store.DB.Exec("ALTER TABLE posts ADD COLUMN future_column TEXT"); err != nil {
		t.Fatal(err)
	}
	if err = a.store.CopyToSQLite(context.Background(), dst, nil); err == nil || !strings.Contains(err.Error(), "结构不兼容") {
		t.Fatal("unknown source column silently dropped", err)
	}
}

func TestMigrationSQLiteFilenameNoClobber(t *testing.T) {
	for _, occupied := range []string{"", "xm.db", "xm.db-wal", "xm.db-shm"} {
		dir := t.TempDir()
		if occupied != "" {
			if err := os.WriteFile(filepath.Join(dir, occupied), []byte("preserve"), 0600); err != nil {
				t.Fatal(err)
			}
		}
		name, err := migrationSQLiteFilename(dir, "fixture")
		if err != nil {
			t.Fatal(err)
		}
		want := "xm.db"
		if occupied != "" {
			want = "xm-migrated-fixture.db"
		}
		if name != want {
			t.Fatalf("got %s want %s", name, want)
		}
		if occupied != "" {
			b, err := os.ReadFile(filepath.Join(dir, occupied))
			if err != nil || string(b) != "preserve" {
				t.Fatal("existing file modified")
			}
		}
	}
}
