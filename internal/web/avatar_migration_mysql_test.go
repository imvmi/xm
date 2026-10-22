package web

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/chromedp/chromedp"
	"xm/internal/archive"
)

// Run against an isolated, real MySQL server. Both transitions use the same
// HTTP endpoints as Settings, including preview, maintenance, config cutover
// and startup recovery. Only the public image origin is an offline TLS fixture.
func TestAvatarMySQLRoundTrip(t *testing.T) {
	parent := t
	if os.Getenv("XM_TEST_MYSQL_ADDR") == "" {
		t.Skip("isolated loopback MySQL required")
	}
	cfg := archive.DefaultConfig()
	cfg.DataDir = filepath.Join(t.TempDir(), "data")
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	if err := archive.SaveBootstrap(configPath, cfg); err != nil {
		t.Fatal(err)
	}
	source, err := archive.OpenConfiguredStore(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { source.Close() })
	writeTestAdmin(t, source, cfg)
	origins := map[string]string{
		"42":               "https://pbs.twimg.com/profile_images/42/alice_normal.png?name=orig&v=a%2Bb",
		"9007199254740993": "https://pbs.twimg.com/profile_images/9007199254740993/bob_normal.png?name=orig&v=c%2Fd",
	}
	if err = source.SaveTargetProfile("alice", archive.Profile{ID: "42", Username: "alice", AvatarURL: origins["42"]}); err != nil {
		t.Fatal(err)
	}
	if err = source.SaveTargetProfile("bob", archive.Profile{ID: "9007199254740993", Username: "bob"}); err != nil {
		t.Fatal(err)
	}
	img := image.NewRGBA(image.Rect(0, 0, 2, 2))
	img.Set(0, 0, color.RGBA{R: 255, A: 255})
	var encoded bytes.Buffer
	if err = png.Encode(&encoded, img); err != nil {
		t.Fatal(err)
	}
	payload := encoded.Bytes()
	// The second origin comes from an old avatar sidecar during startup.
	legacy := filepath.Join(cfg.DataDir, "avatars")
	if err = archive.PrivateDir(legacy); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(legacy, "9007199254740993"), payload, 0600); err != nil {
		t.Fatal(err)
	}
	meta, _ := json.Marshal(map[string]string{"source_url": origins["9007199254740993"]})
	if err = os.WriteFile(filepath.Join(legacy, "9007199254740993.json"), meta, 0600); err != nil {
		t.Fatal(err)
	}
	var requests atomic.Int32
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Host != "pbs.twimg.com" || r.Header.Get("Cookie") != "" || r.Header.Get("Authorization") != "" {
			t.Error("unexpected avatar request or credentials", r.Host)
			http.Error(w, "unexpected", 400)
			return
		}
		found := false
		for _, raw := range origins {
			u, _ := url.Parse(raw)
			if r.URL.RequestURI() == strings.Replace(u.RequestURI(), "_normal.", "_400x400.", 1) {
				found = true
			}
		}
		if !found {
			t.Error("unexpected image variant/query", r.URL)
			http.NotFound(w, r)
			return
		}
		requests.Add(1)
		w.Header().Set("Content-Type", "image/png")
		w.Header().Set("ETag", `"fixture-avatar"`)
		_, _ = w.Write(payload)
	}))
	defer upstream.Close()
	// HTTPClient clones DefaultTransport. Keep the production downloader and TLS
	// verification, but route the allowed image host to our local fixture only.
	transport := upstream.Client().Transport.(*http.Transport).Clone()
	transport.TLSClientConfig = transport.TLSClientConfig.Clone()
	transport.TLSClientConfig.MinVersion = tls.VersionTLS12
	transport.TLSClientConfig.ServerName = upstream.Certificate().DNSNames[0]
	transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		if address != "pbs.twimg.com:443" {
			return nil, fmt.Errorf("unexpected outgoing connection: %s", address)
		}
		return (&net.Dialer{}).DialContext(ctx, network, upstream.Listener.Addr().String())
	}
	oldTransport := http.DefaultTransport
	http.DefaultTransport = transport
	defer func() { http.DefaultTransport = oldTransport; transport.CloseIdleConnections() }()
	a, err := New(configPath, cfg, source, false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(a.migrations.stop)
	t.Cleanup(a.scheduler.closeSession)
	seedMonitor(t, a, "alice")
	seedMonitor(t, a, "bob")
	for id := range origins {
		if err = archive.DownloadAvatar(context.Background(), cfg, archive.Profile{ID: id, AvatarURL: origins[id]}); err != nil {
			t.Fatal(err)
		}
	}
	// A fresh, empty schema is created by the loopback-only test harness.
	mysqlCfg := cfg
	configureMySQLTest(t, &mysqlCfg)

	assertState := func(t *testing.T, app *App) {
		t.Helper()
		b := login(t, app)
		var count int
		if err := app.store.DB.QueryRow("SELECT COUNT(*) FROM avatar_sources").Scan(&count); err != nil || count != len(origins) {
			t.Fatal("origin count", count, err)
		}
		for id, expected := range origins {
			var raw string
			if err := app.store.DB.QueryRow("SELECT source_url FROM avatar_sources WHERE user_id=?", id).Scan(&raw); err != nil || raw != expected {
				t.Fatal("original URL changed", id, raw, err)
			}
		}
		// Both account metadata and avatar responses remain usable after restart.
		w := b.request("GET", "/api/users", nil, "", false)
		ok(t, w, 200)
		var users []UserView
		if err := json.Unmarshal(w.Body.Bytes(), &users); err != nil {
			t.Fatal(err)
		}
		for id := range origins {
			found := false
			for _, u := range users {
				if u.UserID == id && u.AvatarURL == "/avatars/"+id {
					found = true
				}
			}
			if !found {
				t.Fatal("account lost avatar link", id)
			}
		}
		checkImages := func() {
			for id := range origins {
				w := b.request("GET", "/avatars/"+id, nil, "", false)
				ok(t, w, 200)
				if w.Header().Get("Content-Type") != "image/png" || !bytes.Equal(w.Body.Bytes(), payload) {
					t.Fatal("avatar bytes changed", id)
				}
				decoded, err := png.Decode(bytes.NewReader(w.Body.Bytes()))
				if err != nil || decoded.Bounds().Dx() != 2 {
					t.Fatal("avatar not decodable", id, err)
				}
			}
		}
		before := requests.Load()
		checkImages()
		if requests.Load() != before {
			t.Fatal("warm cache unexpectedly downloaded")
		}
		w = b.request("DELETE", "/api/settings/avatars/cache", nil, "http://localhost", true)
		ok(t, w, 200)
		for id := range origins {
			if archive.AvatarExists(cfg.DataDir, id) {
				t.Fatal("cache did not clear", id)
			}
		}
		if os.Getenv("XM_BROWSER_TEST") == "1" {
			assertMigratedAvatarBrowser(t, app)
		}
		checkImages()
		if requests.Load() != before+int32(len(origins)) {
			t.Fatal("cold cache was not rebuilt exactly once", requests.Load(), before)
		}
		// Public/anonymous access remains denied after both migrations.
		anon := &browserClient{t: t, a: app}
		ok(t, anon.request("GET", "/avatars/42", nil, "", false), 401)
	}
	for _, direction := range []struct {
		name, target string
		database     archive.DatabaseConfig
	}{
		{"sqlite_to_mysql", "mysql", mysqlCfg.Database},
		{"mysql_to_sqlite", "sqlite", archive.DatabaseConfig{}},
	} {
		if !t.Run(direction.name, func(t *testing.T) {
			b := login(t, a)
			input := migrationInput{Kind: "database", Target: direction.target, Database: direction.database, Confirm: "开始迁移"}
			ok(t, b.request("POST", "/api/migrations/preview", input, "http://localhost", true), 200)
			ok(t, b.request("POST", "/api/migrations", input, "http://localhost", true), 202)
			job := waitMigration(t, a)
			if job.Status != "switching" {
				t.Fatalf("migration failed: %+v", job)
			}
			select {
			case <-a.restart:
			default:
				t.Fatal("restart not requested")
			}
			if !a.maintenance.Load() {
				t.Fatal("old database writes resumed")
			}
			nextCfg, err := archive.LoadConfig(configPath)
			if err != nil {
				t.Fatal(err)
			}
			if nextCfg.Database.DriverName() != direction.target {
				t.Fatal("bootstrap not switched")
			}
			dst, err := archive.OpenConfiguredStore(nextCfg)
			if err != nil {
				t.Fatal(err)
			}
			parent.Cleanup(func() { dst.Close() })
			// Register at parent scope: this app is the next direction's source.
			// Close all stores before the random MySQL schema is dropped by its owner.
			next, err := New(configPath, nextCfg, dst, false)
			if err != nil {
				dst.Close()
				t.Fatal(err)
			}
			parent.Cleanup(next.scheduler.closeSession)
			parent.Cleanup(next.migrations.stop)
			if next.migrations.snapshot().Status != "completed" {
				dst.Close()
				t.Fatal("startup recovery incomplete")
			}
			a.migrations.stop()
			a.scheduler.closeSession()
			a = next
			assertState(t, next)
		}) {
			return
		}
	}
	a.migrations.stop()
	a.scheduler.closeSession()
	a.store.Close()
}

func assertMigratedAvatarBrowser(t *testing.T, a *App) {
	t.Helper()
	server := httptest.NewServer(a.Handler())
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()
	alloc, closeAlloc := chromedp.NewExecAllocator(ctx, chromedp.DefaultExecAllocatorOptions[:]...)
	defer closeAlloc()
	browser, closeBrowser := chromedp.NewContext(alloc)
	defer closeBrowser()
	eval := func(js string) bool {
		t.Helper()
		v, err := chromedp.Run(browser, chromedp.Evaluate[bool](js))
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	wait := func(js string) {
		t.Helper()
		for !eval(js) {
			select {
			case <-ctx.Done():
				t.Fatal("avatar display timed out", js)
			case <-time.After(25 * time.Millisecond):
			}
		}
	}
	if err := chromedp.Do(browser, chromedp.Navigate(server.URL+"/#accounts")); err != nil {
		t.Fatal(err)
	}
	wait(`typeof state!=='undefined' && !!$('username')`)
	password, _ := json.Marshal(testPassword)
	eval(`(()=>{$('username').value='admin';$('password').value=` + string(password) + `;document.querySelector('#login-form button[type="submit"]').click();return true})()`)
	wait(`state.authenticated && ['42','9007199254740993'].every(id=>{const img=document.querySelector('img.avatar-image[src="/avatars/'+id+'"]');return img&&img.complete&&img.naturalWidth===2})`)
	t.Log("Chrome decoded both migrated account avatars (naturalWidth=2)")
}
