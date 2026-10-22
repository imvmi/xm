package web

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/chromedp/chromedp"
	"xm/internal/archive"
)

func TestAvatarCacheSettingsBrowser(t *testing.T) {
	if os.Getenv("XM_BROWSER_TEST") != "1" {
		t.Skip("offline Chrome UI fixture")
	}
	a := testApp(t)
	dir := filepath.Join(a.Config().DataDir, "avatars")
	if err := archive.PrivateDir(dir); err != nil {
		t.Fatal(err)
	}
	png, _ := base64.StdEncoding.DecodeString("iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mNk+A8AAQUBAScY42YAAAAASUVORK5CYII=")
	for _, id := range []string{"42", "43"} {
		if err := os.WriteFile(filepath.Join(dir, id), png, 0600); err != nil {
			t.Fatal(err)
		}
	}
	meta := []byte(`{"source_url":"https://pbs.twimg.com/profile_images/42/alice_normal.png"}`)
	if err := os.WriteFile(filepath.Join(dir, "42.json"), meta, 0600); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(a.Handler())
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
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
				t.Fatal("avatar settings timed out", js)
			case <-time.After(25 * time.Millisecond):
			}
		}
	}
	if err := chromedp.Do(browser, chromedp.Navigate(server.URL+"/#settings")); err != nil {
		t.Fatal(err)
	}
	wait(`typeof state!=='undefined' && !!$('username')`)
	password, _ := json.Marshal(testPassword)
	eval(`(()=>{$('username').value='admin';$('password').value=` + string(password) + `;document.querySelector('#login-form button[type="submit"]').click();return true})()`)
	wait(`state.authenticated && $('avatar-cache-status').textContent.includes('待迁移 2 张')`)
	eval(`(()=>{$('settings-tab-storage').click();return true})()`)
	if !eval(`!$('settings-storage').hidden && !$('avatar-migrate').disabled`) {
		t.Fatal("avatar controls unavailable on local storage")
	}
	for _, theme := range []string{"light", "dark"} {
		for _, width := range []int64{1280, 390} {
			if err := chromedp.Do(browser, chromedp.EmulateViewport(width, 844)); err != nil {
				t.Fatal(err)
			}
			eval(fmt.Sprintf(`(()=>{applyArchiveTheme(%q);return true})()`, theme))
			if !eval(`document.documentElement.scrollWidth<=innerWidth`) {
				t.Fatalf("avatar settings overflow %s %d", theme, width)
			}
		}
	}
	eval(`(()=>{$('avatar-migrate').click();return true})()`)
	wait(`$('avatar-cache-result').textContent.includes('迁移 1 张头像') && $('avatar-cache-status').textContent.includes('已缓存 1 张')`)
	eval(`(()=>{$('avatar-cache-clear').click();return true})()`)
	wait(`$('avatar-cache-result').textContent.includes('已清理 1 张') && $('avatar-cache-status').textContent.includes('已缓存 0 张')`)
	if a.store.AvatarSource("42") == "" || !archive.AvatarExists(a.Config().DataDir, "43") {
		t.Fatal("clear lost source or unrecoverable legacy avatar")
	}
}
