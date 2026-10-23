package web

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"
	"xm/internal/archive"
)

// Integration runs create/drop only freshly generated schemas on a loopback
// MySQL server. They never accept a production database name or remote address.
func configureMySQLTest(t *testing.T, cfg *archive.Config) {
	t.Helper()
	addr := os.Getenv("XM_TEST_MYSQL_ADDR")
	if addr == "" {
		return
	}
	host, portRaw, e := net.SplitHostPort(addr)
	if e != nil {
		t.Fatal(e)
	}
	ip := net.ParseIP(host)
	if host != "localhost" && (ip == nil || !ip.IsLoopback()) {
		t.Fatal("MySQL tests only allow loopback servers")
	}
	port, e := strconv.Atoi(portRaw)
	if e != nil {
		t.Fatal(e)
	}
	mc := mysql.NewConfig()
	mc.User = os.Getenv("XM_TEST_MYSQL_USER")
	if mc.User == "" {
		mc.User = "root"
	}
	mc.Passwd = os.Getenv("XM_TEST_MYSQL_PASSWORD")
	mc.Net = "tcp"
	mc.Addr = addr
	mc.Timeout = 5 * time.Second
	connector, e := mysql.NewConnector(mc)
	if e != nil {
		t.Fatal(e)
	}
	admin := sql.OpenDB(connector)
	nonce := make([]byte, 10)
	rand.Read(nonce)
	name := "xm_test_" + hex.EncodeToString(nonce)
	if _, e = admin.Exec("CREATE DATABASE `" + name + "` CHARACTER SET utf8mb4 COLLATE utf8mb4_bin"); e != nil {
		admin.Close()
		t.Fatal(e)
	}
	t.Cleanup(func() {
		if _, e := admin.Exec("DROP DATABASE `" + name + "`"); e != nil {
			t.Error(e)
		}
		admin.Close()
	})
	cfg.Database = archive.DatabaseConfig{Driver: "mysql", Host: host, Port: port, Name: name, User: mc.User, Password: mc.Passwd, TLSMode: "disabled"}
}
func TestMySQLMigrationAndWorkerLock(t *testing.T) {
	if os.Getenv("XM_TEST_MYSQL_ADDR") == "" {
		t.Skip("set XM_TEST_MYSQL_ADDR for isolated MySQL integration")
	}
	destCfg := archive.DefaultConfig()
	destCfg.DataDir = t.TempDir()
	configureMySQLTest(t, &destCfg)
	destStore, e := archive.OpenConfiguredStore(destCfg)
	if e != nil {
		t.Fatal(e)
	}
	defer destStore.Close()
	if e = MigrateStore(destStore.DB); e != nil {
		t.Fatal(e)
	}
	dest := &App{store: destStore, config: destCfg}
	cfg := archive.Config{DataDir: t.TempDir(), DefaultInterval: 3600, RequestTimeout: 5, ScrollDelay: 1, MaxScrolls: 1, MaxMediaBytes: 1024}
	muted := false
	cfg.VideoDefaultMuted, cfg.ImageCacheSeconds, cfg.VideoCacheSeconds = &muted, 7200, 120
	cfg.VideoLoop = true
	cfg.VideoAutoNext = true
	cfg.VideoTimelineAutoplay = true
	cfg.SQLiteReadConnections = 7
	source, e := archive.OpenStore(cfg.DataDir)
	if e != nil {
		t.Fatal(e)
	}
	defer source.Close()
	writeTestAdmin(t, source, cfg)
	a, e := New(filepath.Join(t.TempDir(), "config.yaml"), cfg, source, false)
	if e != nil {
		t.Fatal(e)
	}
	seedMonitor(t, a, "alice")
	if e = source.SaveCookies(archive.Cookies{AuthToken: "migration-auth", CSRF: "migration-csrf"}); e != nil {
		t.Fatal(e)
	}
	originalRunID, e := source.StartRun("alice")
	if e != nil {
		t.Fatal(e)
	}
	p := archive.Post{ID: "100", AuthorID: "42", Username: "alice", Text: "中文与 emoji 🌊", Raw: json.RawMessage(`{"test":true}`), QuoteID: "99", Quote: &archive.Post{ID: "99", Username: "bob", Text: "quoted", Assets: []archive.Asset{{ID: "video", Kind: "video"}}}}
	if e = source.SavePosts("alice", []archive.Post{p}); e != nil {
		t.Fatal(e)
	}
	if e = source.MediaResult(archive.MediaJob{PostID: "99", ID: "video", StorageID: "s3_profile", ObjectKey: "archive/media/video.mp4"}, "", 123, "hash", nil); e != nil {
		t.Fatal(e)
	}
	tx, e := source.DB.Begin()
	if e != nil {
		t.Fatal(e)
	}
	if e = enqueueNotification(tx, []archive.NotificationChannel{{ID: "test", Kind: "webhook", WebhookURL: "http://127.0.0.1", Enabled: true}}, NotificationEvent{Type: "test", Title: "queued"}); e != nil {
		tx.Rollback()
		t.Fatal(e)
	}
	if e = tx.Commit(); e != nil {
		t.Fatal(e)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	release, e := dest.store.AcquireDatabaseWorker(ctx, cancel, dest.Config().Database.Name)
	if e != nil {
		t.Fatal(e)
	}
	defer release()
	otherCtx, otherCancel := context.WithCancel(context.Background())
	defer otherCancel()
	if r, e := dest.store.AcquireDatabaseWorker(otherCtx, otherCancel, dest.Config().Database.Name); e == nil {
		r()
		t.Fatal("second writer acquired database lock")
	}
	if _, e = source.DB.Exec("INSERT INTO post_bookmarks(post_id,created_at) VALUES('100',123)"); e != nil {
		t.Fatal(e)
	}
	if _, e = source.DB.Exec("INSERT INTO account_preferences(target,show_in_timeline) VALUES('archived',0)"); e != nil {
		t.Fatal(e)
	}
	counts, e := dest.store.CopySQLiteToMySQL(ctx, filepath.Join(cfg.DataDir, archive.DefaultSQLiteFile))
	if e != nil {
		t.Fatal(e)
	}
	if counts["admin_accounts"] != 1 || counts["system_settings"] != 1 || counts["storage_profiles"] != 1 || counts["x_cookies"] != 1 || counts["posts"] != 2 || counts["notification_deliveries"] != 1 {
		t.Fatal(counts)
	}
	var shown bool
	if e = dest.store.DB.QueryRow("SELECT show_in_timeline FROM account_preferences WHERE target='archived'").Scan(&shown); e != nil || shown || counts["account_preferences"] != 1 {
		t.Fatal("migration lost account preference", e)
	}
	if counts["post_bookmarks"] != 1 {
		t.Fatal("CLI migration lost bookmark", counts)
	}
	migrated, e := dest.store.LoadSettings(dest.Config())
	if e != nil || migrated.Database.DriverName() != "mysql" || migrated.DataDir != destCfg.DataDir {
		t.Fatal("runtime migration overrode bootstrap", e)
	}
	if migrated.SQLiteReadConnections != 7 || migrated.VideoMutedByDefault() || !migrated.VideoLoop || !migrated.VideoAutoNext || !migrated.VideoTimelineAutoplay || migrated.ImageCacheSeconds != 7200 || migrated.VideoCacheSeconds != 120 {
		t.Fatal("SQLite to MySQL migration lost media preferences")
	}
	if _, e = newAuth(dest.store, false); e != nil {
		t.Fatal("administrator was not migrated", e)
	}
	cookies, e := dest.store.LoadCookies()
	if e != nil || cookies.AuthToken != "migration-auth" || cookies.CSRF != "migration-csrf" {
		t.Fatal("Cookie migration failed", e)
	}
	list, e := listPosts(context.Background(), dest.store.DB, "alice", "中文", "video", "", 0, 20)
	if e != nil || len(list.Items) != 1 || list.Items[0].Quote == nil {
		t.Fatal(list, e)
	}
	jobs, e := dest.store.MediaJobs("alice", true)
	if e != nil || len(jobs) != 1 || jobs[0].StorageID != "s3_profile" {
		t.Fatal(jobs, e)
	}
	if _, e = dest.store.CopySQLiteToMySQL(ctx, filepath.Join(cfg.DataDir, archive.DefaultSQLiteFile)); e == nil {
		t.Fatal("nonempty target overwritten")
	}
	var original string
	source.DB.QueryRow("SELECT text FROM posts WHERE id='100'").Scan(&original)
	if original != p.Text {
		t.Fatal("source modified")
	}
	release()
	release2, e := dest.store.AcquireDatabaseWorker(ctx, cancel, dest.Config().Database.Name)
	if e != nil {
		t.Fatal(e)
	}
	release2()
	// Explicit IDs imported into AUTO_INCREMENT tables must not collide later.
	runID, e := dest.store.StartRun("alice")
	if e != nil || runID <= originalRunID {
		t.Fatal(runID, e)
	}
}
func TestMySQLMigrationRollback(t *testing.T) {
	if os.Getenv("XM_TEST_MYSQL_ADDR") == "" {
		t.Skip("requires MySQL")
	}
	cfg := archive.DefaultConfig()
	cfg.DataDir = t.TempDir()
	configureMySQLTest(t, &cfg)
	store, err := archive.OpenConfiguredStore(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err = MigrateStore(store.DB); err != nil {
		t.Fatal(err)
	}
	dest := &App{store: store, config: cfg}
	source, e := archive.OpenStore(t.TempDir())
	if e != nil {
		t.Fatal(e)
	}
	defer source.Close()
	// SQLite accepts this ID; MySQL's strict bounded identifier rejects it.
	if e = source.SavePosts("alice", []archive.Post{{ID: "1", Text: "valid"}, {ID: strings.Repeat("x", 65), Text: "invalid"}}); e != nil {
		t.Fatal(e)
	}
	var path string
	rows, e := source.DB.Query("PRAGMA database_list")
	if e != nil {
		t.Fatal(e)
	}
	for rows.Next() {
		var seq int
		var name string
		rows.Scan(&seq, &name, &path)
	}
	rows.Close()
	if _, e = dest.store.CopySQLiteToMySQL(context.Background(), path); e == nil || !strings.Contains(e.Error(), "迁移表 posts 失败") {
		t.Fatal("expected strict schema failure", e)
	}
	var n int
	dest.store.DB.QueryRow("SELECT COUNT(*) FROM posts").Scan(&n)
	if n != 0 {
		t.Fatal("partial migration committed")
	}
}

func TestMySQLWorkerStopsAfterLockConnectionLoss(t *testing.T) {
	if os.Getenv("XM_TEST_MYSQL_ADDR") == "" {
		t.Skip("requires MySQL")
	}
	a := testApp(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	release, err := a.store.AcquireDatabaseWorker(ctx, cancel, a.Config().Database.Name)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	hash := sha256.Sum256([]byte(a.Config().Database.Name))
	var owner int64
	if err = a.store.DB.QueryRow("SELECT IS_USED_LOCK(?)", "xm:"+hex.EncodeToString(hash[:24])).Scan(&owner); err != nil {
		t.Fatal(err)
	}
	// Kill only the connection holding this test's randomly named database lock.
	if _, err = a.store.DB.Exec("KILL CONNECTION " + strconv.FormatInt(owner, 10)); err != nil {
		t.Fatal(err)
	}
	select {
	case <-ctx.Done():
	case <-time.After(12 * time.Second):
		t.Fatal("worker continued after losing its database lock")
	}
}
