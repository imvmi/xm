package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"xm/internal/archive"
)

func TestStorageProfileDeletion(t *testing.T) {
	for _, scenario := range []string{"unused", "active", "media", "ignored", "copies", "paused-source", "failed-target", "completed", "running", "db-error", "csrf", "origin"} {
		t.Run(scenario, func(t *testing.T) {
			a := testApp(t)
			b := login(t, a)
			var calls atomic.Int64
			remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); w.WriteHeader(500) }))
			defer remote.Close()
			cfg := a.Config()
			cfg.StorageProfiles = []archive.StorageProfile{{ID: "old", Name: "Old NAS", Kind: "webdav", Endpoint: remote.URL, Username: "u", Password: "delete-secret", Timeout: 5}}
			if scenario == "active" {
				cfg.MediaStorage = "old"
			}
			if err := a.store.SaveSettings(cfg); err != nil {
				t.Fatal(err)
			}
			a.config = cfg
			want := 200
			switch scenario {
			case "active":
				want = 409
			case "media", "ignored":
				if err := a.store.SavePosts("alice", []archive.Post{{ID: "delete-post", Assets: []archive.Asset{{ID: "photo", Kind: "photo"}}}}); err != nil {
					t.Fatal(err)
				}
				if _, err := a.store.DB.Exec("UPDATE media SET storage_id='old' WHERE post_id='delete-post'"); err != nil {
					t.Fatal(err)
				}
				if scenario == "ignored" {
					if _, err := a.store.DB.Exec("UPDATE media SET status='ignored' WHERE post_id='delete-post'"); err != nil {
						t.Fatal(err)
					}
				}
				want = 409
			case "copies":
				if _, err := a.store.DB.Exec(`INSERT INTO media_local_copies(copy_id,post_id,media_id,local_path,storage_id,object_key,bytes,sha256) VALUES('copy','p','m','media/a','old','key',1,'hash')`); err != nil {
					t.Fatal(err)
				}
				want = 409
			case "paused-source":
				a.migrations.job = &migrationJob{ID: "job", Kind: "media", Source: "old", Target: "local", Status: "paused"}
				want = 409
			case "failed-target":
				a.migrations.job = &migrationJob{ID: "job", Kind: "media", Source: "local", Target: "old", Status: "failed"}
				want = 409
			case "completed":
				a.migrations.job = &migrationJob{ID: "job", Kind: "media", Source: "old", Target: "local", Status: "completed"}
			case "running":
				a.migrations.running = true
				defer func() { a.migrations.running = false }()
				want = 409
			case "db-error":
				if _, err := a.store.DB.Exec(`CREATE TRIGGER reject_storage_delete BEFORE DELETE ON storage_profiles BEGIN SELECT RAISE(ABORT,'fixture failure'); END`); err != nil {
					t.Fatal(err)
				}
				want = 500
			case "csrf", "origin":
				want = 403
			}
			origin := "http://localhost"
			if scenario == "origin" {
				origin = "http://evil.example"
			}
			if scenario != "running" {
				view := b.request("GET", "/api/storage", nil, "", false)
				ok(t, view, 200)
				var state struct {
					Profiles []storageView `json:"profiles"`
				}
				if err := json.Unmarshal(view.Body.Bytes(), &state); err != nil {
					t.Fatal(err)
				}
				eligible := scenario == "unused" || scenario == "completed" || scenario == "db-error" || scenario == "csrf" || scenario == "origin"
				if len(state.Profiles) != 1 || state.Profiles[0].CanDelete != eligible {
					t.Fatal("incorrect deletion eligibility", view.Body.String())
				}
				if strings.Contains(view.Body.String(), "delete-secret") {
					t.Fatal("credentials exposed")
				}
			}
			result := b.request("DELETE", "/api/storage/profiles/old", nil, origin, scenario != "csrf")
			ok(t, result, want)
			var count int
			if err := a.store.DB.QueryRow("SELECT COUNT(*) FROM storage_profiles WHERE id='old'").Scan(&count); err != nil {
				t.Fatal(err)
			}
			_, memoryErr := a.Config().Storage("old")
			loaded, err := a.store.LoadSettings(cfg)
			if err != nil {
				t.Fatal(err)
			}
			_, reloadErr := loaded.Storage("old")
			if want == 200 {
				if count != 0 || memoryErr == nil || reloadErr == nil {
					t.Fatal("deleted credentials survived in DB, memory, or reload")
				}
			} else if count != 1 || memoryErr != nil || reloadErr != nil {
				t.Fatal("blocked delete changed connection")
			}
			if calls.Load() != 0 {
				t.Fatal("connection deletion contacted remote storage")
			}
		})
	}
}

func TestStorageProfileDeletionUnknownAndLocal(t *testing.T) {
	a := testApp(t)
	b := login(t, a)
	ok(t, b.request("DELETE", "/api/storage/profiles/local", nil, "http://localhost", true), 409)
	ok(t, b.request("DELETE", "/api/storage/profiles/missing", nil, "http://localhost", true), 404)
}
