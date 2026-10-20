package archive

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLocalCopyCleanupGuards(t *testing.T) {
	for _, scenario := range []string{"success", "missing-local", "remote-corrupt", "remote-missing", "local-changed", "in-use", "reference-changed", "symlink", "parent-symlink", "cancelled"} {
		t.Run(scenario, func(t *testing.T) {
			dir := t.TempDir()
			s, err := OpenStore(dir)
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			const payload = "verified archived image"
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if scenario == "remote-missing" {
					http.NotFound(w, r)
					return
				}
				value := payload
				if scenario == "remote-corrupt" {
					value = "corrupted"
				}
				http.ServeContent(w, r, "photo.jpg", time.Time{}, strings.NewReader(value))
			}))
			defer server.Close()
			cfg := DefaultConfig()
			cfg.DataDir = dir
			cfg.StorageProfiles = []StorageProfile{{ID: "dav", Name: "test", Kind: "webdav", Endpoint: server.URL, Timeout: 5}}
			h := sha256.Sum256([]byte(payload))
			item := LocalMediaCopy{CopyID: "copy", MigrationMedia: MigrationMedia{PostID: "post", MediaID: "photo", StorageID: "dav", Key: "remote.jpg", LocalPath: "media/source.jpg", Bytes: int64(len(payload)), SHA256: hex.EncodeToString(h[:])}}
			if err = s.SavePosts("alice", []Post{{ID: "post", Assets: []Asset{{ID: "photo", Kind: "photo"}}}, {ID: "other", Assets: []Asset{{ID: "photo", Kind: "photo"}}}}); err != nil {
				t.Fatal(err)
			}
			if err = s.MediaResult(MediaJob{PostID: item.PostID, ID: item.MediaID, StorageID: item.StorageID, ObjectKey: item.Key}, "", item.Bytes, item.SHA256, nil); err != nil {
				t.Fatal(err)
			}
			os.MkdirAll(filepath.Join(dir, "media"), 0700)
			localFile := filepath.Join(dir, item.LocalPath)
			os.WriteFile(localFile, []byte(payload), 0600)
			keep := filepath.Join(dir, "keep.jpg")
			os.WriteFile(keep, []byte(payload), 0600)
			switch scenario {
			case "missing-local":
				os.Remove(localFile)
			case "local-changed":
				os.WriteFile(localFile, []byte("new content"), 0600)
			case "in-use":
				err = s.MediaResult(MediaJob{PostID: "other", ID: "photo", StorageID: "local"}, item.LocalPath, item.Bytes, item.SHA256, nil)
			case "reference-changed":
				_, err = s.DB.Exec("UPDATE media SET storage_id='local',local_path=? WHERE post_id='post'", item.LocalPath)
			case "symlink":
				os.Remove(localFile)
				err = os.Symlink(keep, localFile)
			case "parent-symlink":
				err = os.Symlink("..", filepath.Join(dir, "media", "escape"))
				item.LocalPath = "media/escape/keep.jpg"
			}
			if err != nil {
				t.Fatal(err)
			}
			if _, err = s.DB.Exec(`INSERT INTO media_local_copies VALUES(?,?,?,?,?,?,?,?)`, item.CopyID, item.PostID, item.MediaID, item.LocalPath, item.StorageID, item.Key, item.Bytes, item.SHA256); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if scenario == "cancelled" {
				cancel()
			}
			err = s.CleanupLocalMediaCopy(ctx, cfg, item)
			var count int
			s.DB.QueryRow("SELECT COUNT(*) FROM media_local_copies").Scan(&count)
			if scenario == "success" || scenario == "missing-local" {
				if err != nil || count != 0 {
					t.Fatal("cleanup failed", err, count)
				}
				if _, err = os.Stat(localFile); !os.IsNotExist(err) {
					t.Fatal("verified copy remains", err)
				}
			} else {
				if err == nil || count != 1 {
					t.Fatal("unsafe cleanup accepted or record lost", err, count)
				}
				if _, err = os.Lstat(localFile); err != nil {
					t.Fatal("protected source removed", err)
				}
			}
			if got, _ := os.ReadFile(keep); string(got) != payload {
				t.Fatal("unrecorded/outside media file changed")
			}
			if _, err = os.Stat(filepath.Join(dir, "xm.db")); err != nil {
				t.Fatal("database removed", err)
			}
		})
	}
}
