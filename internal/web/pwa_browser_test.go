package web

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/chromedp/chromedp"
)

func TestPWABrowser(t *testing.T) {
	if os.Getenv("XM_BROWSER_TEST") != "1" {
		t.Skip("offline Chrome UI fixture")
	}
	a := testApp(t)
	handler := a.Handler()
	var unavailable, upgraded atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if unavailable.Load() && (r.URL.Path == "/" || r.URL.Path == "/healthz") {
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
			return
		}
		if upgraded.Load() && r.URL.Path == "/sw.js" {
			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, r)
			for k, values := range recorder.Header() {
				w.Header()[k] = values
			}
			fmt.Fprint(w, strings.ReplaceAll(recorder.Body.String(), pwaRevision, "browser-test-upgrade"))
			return
		}
		handler.ServeHTTP(w, r)
	}))
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	alloc, closeAlloc := chromedp.NewExecAllocator(ctx, chromedp.DefaultExecAllocatorOptions[:]...)
	defer closeAlloc()
	browser, closeBrowser := chromedp.NewContext(alloc)
	defer closeBrowser()
	eval := func(js string) bool {
		t.Helper()
		v, err := chromedp.Run(browser, chromedp.Evaluate[bool](js, chromedp.EvalAwaitPromise))
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	wait := func(js string) {
		t.Helper()
		deadline := time.Now().Add(15 * time.Second)
		for !eval(js) {
			if time.Now().After(deadline) {
				t.Fatal("timed out", js)
			}
			time.Sleep(40 * time.Millisecond)
		}
	}
	screenshot := func(name string) {
		t.Helper()
		if dir := os.Getenv("XM_UI_SCREENSHOT_DIR"); dir != "" {
			b, err := chromedp.Run(browser, chromedp.CaptureScreenshot())
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, name+".png"), b, 0600); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := chromedp.Do(browser, chromedp.EmulateViewport(390, 844), chromedp.Navigate(server.URL)); err != nil {
		t.Fatal(err)
	}
	wait(`!!navigator.serviceWorker.controller`)
	if !eval(`document.querySelector('link[rel=manifest]').href.endsWith('/manifest.webmanifest') && document.documentElement.scrollWidth<=innerWidth`) {
		t.Fatal("manifest or mobile login layout")
	}
	screenshot("pwa-login-mobile")
	eval(`(()=>{document.querySelector('[data-pwa-open]').click();return true})()`)
	wait(`document.getElementById('pwa-dialog').open && !!document.getElementById('pwa-install-help').textContent`)
	screenshot("pwa-install-mobile")
	// Exercise the native install action without opening the OS install dialog.
	eval(`(()=>{const e=new Event('beforeinstallprompt');e.prompt=async()=>{window.installPromptCalled=true};e.userChoice=Promise.resolve({outcome:'dismissed'});dispatchEvent(e);document.getElementById('pwa-install').click();return true})()`)
	wait(`window.installPromptCalled && document.getElementById('pwa-install').hidden`)
	eval(`(()=>{document.getElementById('pwa-close').click();return true})()`)
	password, _ := json.Marshal(testPassword)
	eval(`(()=>{$('username').value='admin';$('password').value=` + string(password) + `;document.querySelector('#login-form button[type=submit]').click();return true})()`)
	wait(`typeof state!=='undefined' && state.authenticated && state.view==='home'`)
	for _, width := range []int64{390, 844, 1280} {
		if err := chromedp.Do(browser, chromedp.EmulateViewport(width, 844)); err != nil {
			t.Fatal(err)
		}
		for _, view := range []string{"home", "settings"} {
			eval(fmt.Sprintf(`(()=>{location.hash=%q;return true})()`, view))
			wait(fmt.Sprintf(`state.view===%q`, view))
			if view == "settings" {
				eval(`(()=>{$('settings-tab-display').click();return true})()`)
				wait(`!$('settings-display').hidden`)
			}
			if !eval(`document.documentElement.scrollWidth<=innerWidth`) {
				t.Fatalf("overflow %s at %d", view, width)
			}
			screenshot(fmt.Sprintf("pwa-%s-%d", view, width))
		}
	}
	// Updates are offered without replacing the page while the user is editing.
	eval(`(()=>{window.pwaBeforeUpdate=true;return true})()`)
	upgraded.Store(true)
	eval(`navigator.serviceWorker.getRegistration().then(async r=>{await r.update();return true})`)
	wait(`!$('pwa-update').hidden`)
	if !eval(`window.pwaBeforeUpdate===true`) {
		t.Fatal("update reloaded without user action")
	}
	eval(`(()=>{$('pwa-update-button').click();return true})()`)
	wait(`typeof state!=='undefined' && state.authenticated && !window.pwaBeforeUpdate && !!navigator.serviceWorker.controller`)
	if !eval(`(async()=>{const names=await caches.keys();if(names.length!==1 || !names[0].endsWith('browser-test-upgrade'))return false;const cache=await caches.open(names[0]);const keys=await cache.keys();return keys.length>0 && keys.every(r=>{const p=new URL(r.url).pathname;return p==='/offline.html'||/^\/assets\/(offline\.(css|js)|favicon.svg|.*\.png)$/.test(p)})})()`) {
		t.Fatal("private data in cache or obsolete worker cache retained")
	}
	unavailable.Store(true)
	if err := chromedp.Do(browser, chromedp.EmulateViewport(390, 844), chromedp.Navigate(server.URL+"/?pwa-test-offline=1#settings")); err != nil {
		t.Fatal(err)
	}
	wait(`!!document.getElementById('offline-retry')`)
	if !eval(`document.documentElement.scrollWidth<=innerWidth && getComputedStyle(document.body).display==='grid'`) {
		t.Fatal("offline page assets or mobile layout")
	}
	screenshot("pwa-offline-mobile")
	eval(`(()=>{document.getElementById('offline-retry').click();return true})()`)
	wait(`document.getElementById('offline-status').textContent.includes('仍无法连接')`)
	unavailable.Store(false)
	eval(`(()=>{document.getElementById('offline-retry').click();return true})()`)
	wait(`typeof state!=='undefined' && state.authenticated && state.view==='settings'`)
}
