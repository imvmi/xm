package web

import (
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"xm/internal/archive"
	"xm/internal/testutil"
)

func TestStorageCredentialRotation(t *testing.T) {
	for _, kind := range []string{"webdav", "s3"} {
		t.Run(kind, func(t *testing.T) {
			a := testApp(t)
			b := login(t, a)
			var mu sync.Mutex
			objects := map[string]string{}
			var reject, rejectDelete atomic.Bool
			var calls atomic.Int64
			var onDelete atomic.Pointer[func()]
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if reject.Load() {
					w.WriteHeader(403)
					return
				}
				if kind == "webdav" {
					u, p, _ := r.BasicAuth()
					if u != "new-user" || p != "new-password" {
						w.WriteHeader(401)
						return
					}
				} else if !strings.Contains(r.Header.Get("Authorization"), "Credential=new-access/") || r.Header.Get("X-Amz-Security-Token") != "new-token" {
					w.WriteHeader(403)
					return
				}
				if r.Header.Get("Cookie") != "" {
					t.Error("session leaked upstream")
				}
				mu.Lock()
				defer mu.Unlock()
				switch r.Method {
				case "MKCOL":
					w.WriteHeader(201)
				case "PUT":
					data, _ := io.ReadAll(r.Body)
					objects[r.URL.Path] = string(data)
				case "GET", "HEAD":
					value, ok := objects[r.URL.Path]
					if !ok {
						http.NotFound(w, r)
						return
					}
					http.ServeContent(w, r, "probe", time.Time{}, strings.NewReader(value))
				case "DELETE":
					if rejectDelete.Load() {
						w.WriteHeader(403)
						return
					}
					if !strings.Contains(r.URL.Path, "/.xm-check/") {
						t.Error("non-probe deleted")
					}
					if callback := onDelete.Swap(nil); callback != nil {
						(*callback)()
					}
					delete(objects, r.URL.Path)
					w.WriteHeader(204)
				default:
					w.WriteHeader(405)
				}
			}))
			defer server.Close()
			old := archive.StorageProfile{ID: "rotate", Name: "NAS", Kind: kind, Endpoint: server.URL, Prefix: "existing", Username: "old-user", Password: "old-password", Region: "us-east-1", Bucket: "test-bucket", AccessKey: "old-access", SecretKey: "old-secret", SessionToken: "old-token", PathStyle: true, Timeout: 5}
			cfg := a.Config()
			cfg.StorageProfiles = []archive.StorageProfile{old}
			cfg.MediaStorage = old.ID
			if err := a.store.SaveSettings(cfg); err != nil {
				t.Fatal(err)
			}
			a.config = cfg
			path := "/api/storage/profiles/rotate/credentials"
			input := map[string]string{"username": "new-user", "password": "new-password"}
			if kind == "s3" {
				input = map[string]string{"access_key": "new-access", "secret_key": "new-secret", "session_token": "new-token"}
			}
			anon := &browserClient{t: t, a: a}
			ok(t, anon.request("PUT", path, input, "http://localhost", true), 401)
			ok(t, b.request("PUT", path, input, "http://evil.example", true), 403)
			ok(t, b.request("PUT", path, input, "http://localhost", false), 403)
			for _, field := range []string{"endpoint", "prefix", "bucket", "kind", "id", "use_proxy"} {
				ok(t, b.request("PUT", path, map[string]string{field: "changed"}, "http://localhost", true), 400)
			}
			ok(t, b.request("PUT", "/api/storage/profiles/local/credentials", input, "http://localhost", true), 404)
			if calls.Load() != 0 {
				t.Fatal("invalid request contacted storage")
			}
			assertOld := func() {
				t.Helper()
				p, _ := a.Config().Storage(old.ID)
				saved, err := a.store.LoadSettings(cfg)
				if err != nil {
					t.Fatal(err)
				}
				persisted, _ := saved.Storage(old.ID)
				if p != old || persisted != old {
					t.Fatal("failed rotation changed credentials")
				}
			}
			reject.Store(true)
			ok(t, b.request("PUT", path, input, "http://localhost", true), 502)
			assertOld()
			reject.Store(false)
			rejectDelete.Store(true)
			ok(t, b.request("PUT", path, input, "http://localhost", true), 502)
			assertOld()
			rejectDelete.Store(false)
			if a.store.DB.Dialect == "sqlite" {
				if _, err := a.store.DB.Exec("CREATE TRIGGER reject_rotation BEFORE UPDATE ON storage_profiles BEGIN SELECT RAISE(ABORT,'test failure'); END"); err != nil {
					t.Fatal(err)
				}
				ok(t, b.request("PUT", path, input, "http://localhost", true), 500)
				assertOld()
				if _, err := a.store.DB.Exec("DROP TRIGGER reject_rotation"); err != nil {
					t.Fatal(err)
				}
			}
			changeTheme := func() {
				a.changeMu.Lock()
				defer a.changeMu.Unlock()
				latest := a.Config()
				latest.ArchiveTheme = "dark"
				if err := a.saveConfig(latest); err != nil {
					t.Error(err)
				}
			}
			onDelete.Store(&changeTheme)
			w := b.request("PUT", path, input, "http://localhost", true)
			ok(t, w, 200)
			if a.Config().ArchiveTheme != "dark" {
				t.Fatal("rotation overwrote a concurrent settings edit")
			}
			p, _ := a.Config().Storage(old.ID)
			if !p.SameDestination(old) || p.ID != old.ID || p.Name != old.Name || p.Timeout != old.Timeout || p.UseProxy != old.UseProxy || a.Config().ActiveStorage() != old.ID {
				t.Fatal("destination/settings changed")
			}
			if kind == "webdav" && (p.Password != "new-password" || p.Username != "new-user") {
				t.Fatal("credentials not replaced")
			}
			if kind == "s3" && (p.AccessKey != "new-access" || p.SecretKey != "new-secret" || p.SessionToken != "new-token") {
				t.Fatal("S3 credentials not replaced")
			}
			restarted, err := New(a.configPath, cfg, a.store, false)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(restarted.migrations.stop)
			loaded, _ := restarted.Config().Storage(old.ID)
			if loaded != p {
				t.Fatal("rotation lost on restart")
			}
			status := b.request("GET", "/api/storage", nil, "", false)
			for _, secret := range []string{"new-password", "new-secret", "new-token", "new-access"} {
				if strings.Contains(status.Body.String()+w.Body.String(), secret) {
					t.Fatal("credential echoed", secret)
				}
			}
			competing := p
			if kind == "s3" {
				competing.SecretKey = "competing-secret"
			} else {
				competing.Password = "competing-password"
			}
			changeCredentials := func() {
				a.changeMu.Lock()
				defer a.changeMu.Unlock()
				latest := a.Config()
				latest.StorageProfiles[0] = competing
				if err := a.store.SaveSettings(latest); err != nil {
					t.Error(err)
					return
				}
				a.configMu.Lock()
				a.config = latest
				a.configMu.Unlock()
			}
			onDelete.Store(&changeCredentials)
			ok(t, b.request("PUT", path, input, "http://localhost", true), 409)
			current, _ := a.Config().Storage(old.ID)
			if current != competing {
				t.Fatal("stale probe overwrote a newer rotation")
			}
		})
	}
}

func TestStorageCredentialsPartialUpdate(t *testing.T) {
	str := func(s string) *string { return &s }
	p := archive.StorageProfile{ID: "s3", Name: "S3", Kind: "s3", Region: "us-east-1", Bucket: "bucket", AccessKey: "access", SecretKey: "secret", SessionToken: "token", Timeout: 5}
	next, err := (storageCredentialsInput{SessionToken: str("")}).apply(p)
	if err != nil || next.SessionToken != "" || next.AccessKey != p.AccessKey || next.SecretKey != p.SecretKey {
		t.Fatal(next, err)
	}
	if _, err = (storageCredentialsInput{AccessKey: str("partial")}).apply(p); err == nil {
		t.Fatal("half key pair accepted")
	}
	smb := archive.StorageProfile{ID: "smb", Name: "SMB", Kind: "smb", Endpoint: "smb://localhost/archive", Username: "user", Password: "old", Timeout: 5}
	next, err = (storageCredentialsInput{Password: str("new")}).apply(smb)
	expected := smb
	expected.Password = "new"
	if err != nil || !reflect.DeepEqual(next, expected) {
		t.Fatal(next, err)
	}
}

func TestStorageCredentialRotationSMB(t *testing.T) {
	endpoint, _ := testutil.SMBFixture(t)
	a := testApp(t)
	b := login(t, a)
	cfg := a.Config()
	cfg.StorageProfiles = []archive.StorageProfile{{ID: "smb", Name: "SMB", Kind: "smb", Endpoint: endpoint, Prefix: "media", Username: "fixture-user", Password: "expired-password", Timeout: 5}}
	if err := a.store.SaveSettings(cfg); err != nil {
		t.Fatal(err)
	}
	a.config = cfg
	ok(t, b.request("PUT", "/api/storage/profiles/smb/credentials", map[string]string{"password": "fixture-password"}, "http://localhost", true), 200)
	current, _ := a.Config().Storage("smb")
	if current.Password != "fixture-password" || !current.SameDestination(cfg.StorageProfiles[0]) {
		t.Fatal("SMB rotation failed")
	}
}
