package web

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"xm/internal/archive"
	"xm/internal/testutil"
)

func TestSambaMigrationAndCleanup(t *testing.T) {
	endpoint, share := testutil.SMBFixture(t)
	a := testApp(t)
	t.Cleanup(a.migrations.stop)
	b := login(t, a)
	cfg := a.Config()
	originalDatabase := cfg.Database
	payload := "archived photo with authenticated SMB storage"
	hash := sha256.Sum256([]byte(payload))
	rel := "media/migrated/source.jpg"
	if err := os.MkdirAll(filepath.Dir(filepath.Join(cfg.DataDir, rel)), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfg.DataDir, rel), []byte(payload), 0600); err != nil {
		t.Fatal(err)
	}
	if err := a.store.SavePosts("alice", []archive.Post{{ID: "smb-photo", Assets: []archive.Asset{{ID: "image", Kind: "photo"}}}}); err != nil {
		t.Fatal(err)
	}
	if err := a.store.MediaResult(archive.MediaJob{PostID: "smb-photo", ID: "image", StorageID: "local"}, rel, int64(len(payload)), hex.EncodeToString(hash[:]), nil); err != nil {
		t.Fatal(err)
	}
	input := migrationInput{Kind: "media", Source: "local", Target: "new", Confirm: "开始迁移",
		Storage: &archive.StorageProfile{Name: "测试 Samba", Kind: "smb", Endpoint: endpoint, Prefix: "xm", Username: "fixture-user", Password: "fixture-password", Timeout: 5}}
	ok(t, b.request("POST", "/api/migrations/preview", input, "http://localhost", true), 200)
	ok(t, b.request("POST", "/api/migrations", input, "http://localhost", true), 202)
	job := waitMigration(t, a)
	if job.Status != "completed" || job.Done != 1 {
		t.Fatalf("outbound migration: %+v", job)
	}
	target := job.Target
	var storage, key, local string
	if err := a.store.DB.QueryRow("SELECT storage_id,object_key,local_path FROM media WHERE post_id='smb-photo'").Scan(&storage, &key, &local); err != nil {
		t.Fatal(err)
	}
	if storage != target || local != "" || a.Config().ActiveStorage() != target || a.Config().Database != originalDatabase {
		t.Fatal("incorrect target or database was changed")
	}
	if got, _ := os.ReadFile(filepath.Join(cfg.DataDir, rel)); string(got) != payload {
		t.Fatal("migration removed local source")
	}
	response := b.request("GET", "/media/smb-photo/image", nil, "", false)
	ok(t, response, 200)
	if response.Body.String() != payload {
		t.Fatal("SMB playback returned wrong content")
	}
	for _, route := range []string{"/api/storage", "/api/migrations"} {
		response := b.request("GET", route, nil, "", false)
		ok(t, response, 200)
		if strings.Contains(response.Body.String(), "fixture-password") {
			t.Fatal("credential leaked", route)
		}
	}
	loaded, err := a.store.LoadSettings(cfg)
	if err != nil || loaded.ActiveStorage() != target || loaded.StorageProfiles[len(loaded.StorageProfiles)-1].Kind != "smb" {
		t.Fatal("SMB profile did not survive settings reload", err)
	}
	// Cleanup requires its own confirmation and never uses stale migration hashes.
	cleanup := migrationInput{Kind: "cleanup", Confirm: "开始迁移"}
	ok(t, b.request("POST", "/api/migrations", cleanup, "http://localhost", true), 400)
	cleanup.Confirm = "清理本地副本"
	remoteFile := filepath.Join(share, filepath.FromSlash(key))
	if err = os.WriteFile(remoteFile, []byte("corrupt"), 0600); err != nil {
		t.Fatal(err)
	}
	ok(t, b.request("POST", "/api/migrations", cleanup, "http://localhost", true), 202)
	job = waitMigration(t, a)
	if job.Status != "failed" || job.Failed != 1 {
		t.Fatalf("cleanup accepted corrupted destination: %+v", job)
	}
	if _, err = os.Stat(filepath.Join(cfg.DataDir, rel)); err != nil {
		t.Fatal("cleanup removed fallback after remote corruption", err)
	}
	if err = os.WriteFile(remoteFile, []byte(payload), 0600); err != nil {
		t.Fatal(err)
	}
	// Restart the manager before retrying; copy records and progress are durable.
	a.migrations.stop()
	a.migrations, err = newMigrationManager(a)
	if err != nil {
		t.Fatal(err)
	}
	ok(t, b.request("POST", "/api/migrations", map[string]any{"retry": true, "confirm": "清理本地副本"}, "http://localhost", true), 202)
	job = waitMigration(t, a)
	if job.Status != "completed" || job.Done != 1 {
		t.Fatalf("cleanup retry: %+v", job)
	}
	if _, err = os.Stat(filepath.Join(cfg.DataDir, rel)); !os.IsNotExist(err) {
		t.Fatal("verified local copy was not removed", err)
	}
	var remaining int
	a.store.DB.QueryRow("SELECT COUNT(*) FROM media_local_copies").Scan(&remaining)
	if remaining != 0 {
		t.Fatal("cleanup manifest was not updated")
	}
	// Bring the remote object back; all content and the database stay intact.
	back := migrationInput{Kind: "media", Source: target, Target: "local", Confirm: "开始迁移"}
	ok(t, b.request("POST", "/api/migrations", back, "http://localhost", true), 202)
	job = waitMigration(t, a)
	if job.Status != "completed" || job.Done != 1 || a.Config().ActiveStorage() != "local" || a.Config().Database != originalDatabase {
		t.Fatalf("inbound migration: %+v", job)
	}
	a.store.DB.QueryRow("SELECT local_path FROM media WHERE post_id='smb-photo'").Scan(&local)
	if got, _ := os.ReadFile(filepath.Join(cfg.DataDir, local)); string(got) != payload {
		t.Fatal("local media does not match after round trip")
	}
	if got, _ := os.ReadFile(remoteFile); string(got) != payload {
		t.Fatal("inbound migration modified remote source")
	}
	status := b.request("GET", "/api/migrations", nil, "", false)
	var data struct {
		LocalCopies struct{ Items int } `json:"local_copies"`
	}
	if err := json.Unmarshal(status.Body.Bytes(), &data); err != nil || data.LocalCopies.Items != 0 {
		t.Fatal("unexpected cleanup candidates", err)
	}
}
