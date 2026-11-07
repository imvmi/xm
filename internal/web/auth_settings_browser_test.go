package web

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/chromedp/chromedp"
	"xm/internal/archive"
)

// Exercise the collapsed quick action and independent drafts with a local source.
func TestAuthSettingsBrowser(t *testing.T) {
	if os.Getenv("XM_BROWSER_TEST") != "1" {
		t.Skip("offline Chrome UI fixture")
	}
	a := testApp(t)
	var fetches atomic.Int32
	var failed atomic.Bool
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fetches.Add(1)
		if r.Header.Get("Authorization") != "Bearer fixture-secret" {
			t.Error("saved source authentication lost")
		}
		if failed.Load() {
			w.WriteHeader(503)
			return
		}
		_, _ = w.Write([]byte(`{"auth_token":"fixture-fetched","ct0":"fixture-csrf"}`))
	}))
	defer source.Close()
	if err := a.store.SaveCookies(archive.Cookies{AuthToken: "fixture-initial", CSRF: "fixture-csrf"}); err != nil {
		t.Fatal(err)
	}
	if err := a.store.SaveCookieSource(archive.CookieSource{URL: source.URL, BearerToken: "fixture-secret", AutoRefresh: true}); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(a.Handler())
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	alloc, stopAlloc := chromedp.NewExecAllocator(ctx, chromedp.DefaultExecAllocatorOptions[:]...)
	defer stopAlloc()
	browser, stopBrowser := chromedp.NewContext(alloc)
	defer stopBrowser()
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
		until := time.Now().Add(8 * time.Second)
		for !eval(js) {
			if time.Now().After(until) {
				t.Fatal("UI timed out", js)
			}
			time.Sleep(25 * time.Millisecond)
		}
	}
	if err := chromedp.Do(browser, chromedp.Navigate(server.URL+"/#settings")); err != nil {
		t.Fatal(err)
	}
	wait(`typeof state!=='undefined' && !$('login-view').hidden`)
	password, _ := json.Marshal(testPassword)
	eval(`(()=>{$('username').value='admin';$('password').value=` + string(password) + `;document.querySelector('#login-form button[type=submit]').click();return true})()`)
	wait(`state.authenticated && settingsBaselines.has('cookie-api-form') && !$('cookie-api-fetch-saved').hidden`)
	if !eval(`!$('cookie-manual-method').open && !$('cookie-api-method').open && $('cookie-api-summary').textContent.includes('自动恢复已开启') && $('cookie-api-token').value==='' && !$('settings-account').textContent.includes('fixture-secret')`) {
		t.Fatal("saved credential summary or disclosure state")
	}
	for _, theme := range []string{"dark", "light"} {
		for _, width := range []int64{1280, 390} {
			if err := chromedp.Do(browser, chromedp.EmulateViewport(width, 900)); err != nil {
				t.Fatal(err)
			}
			eval(fmt.Sprintf(`(()=>{applyArchiveTheme(%q);document.querySelector('.auth-panel').scrollIntoView({block:'start',behavior:'instant'});return true})()`, theme))
			if err := chromedp.Do(browser, chromedp.Sleep(250*time.Millisecond)); err != nil {
				t.Fatal(err)
			}
			if !eval(`document.documentElement.scrollWidth<=innerWidth`) {
				t.Fatal("auth layout overflows", theme, width)
			}
			if dir := os.Getenv("XM_UI_SCREENSHOT_DIR"); dir != "" {
				if err := os.MkdirAll(dir, 0700); err != nil {
					t.Fatal(err)
				}
				img, err := chromedp.Run(browser, chromedp.CaptureScreenshot())
				if err != nil {
					t.Fatal(err)
				}
				if err = os.WriteFile(filepath.Join(dir, fmt.Sprintf("auth-summary-%s-%d.png", theme, width)), img, 0600); err != nil {
					t.Fatal(err)
				}
			}
		}
	}
	// Expanded forms must remain readable on phones, including optional credentials.
	for _, method := range []string{"manual", "api"} {
		eval(fmt.Sprintf(`(()=>{applyArchiveTheme('dark');$('cookie-manual-method').open=%t;$('cookie-api-method').open=%t;$('cookie-api-advanced').open=%t;$('cookie-%s-method').scrollIntoView({block:'start',behavior:'instant'});return true})()`, method == "manual", method == "api", method == "api", method))
		if err := chromedp.Do(browser, chromedp.Sleep(250*time.Millisecond)); err != nil {
			t.Fatal(err)
		}
		if !eval(`document.documentElement.scrollWidth<=innerWidth && document.querySelector('.auth-panel').scrollWidth<=document.querySelector('.auth-panel').clientWidth`) {
			t.Fatal("expanded form overflows", method)
		}
		if dir := os.Getenv("XM_UI_SCREENSHOT_DIR"); dir != "" {
			img, err := chromedp.Run(browser, chromedp.CaptureScreenshot())
			if err != nil {
				t.Fatal(err)
			}
			if err = os.WriteFile(filepath.Join(dir, "auth-"+method+"-mobile.png"), img, 0600); err != nil {
				t.Fatal(err)
			}
		}
	}
	eval(`(()=>{$('cookie-manual-method').open=false;$('cookie-api-method').open=false;return true})()`)
	eval(`(()=>{$('cookie-api-fetch-saved').click();return true})()`)
	wait(`!$('cookie-api-fetch-saved').disabled && $('cookie-api-result').textContent.includes('已')`)
	if fetches.Load() != 1 {
		t.Fatalf("quick fetches: %d", fetches.Load())
	}
	// Dirty configuration must be disclosed, never silently saved or ignored.
	eval(`(()=>{$('cookie-api-auto').checked=false;$('cookie-api-auto').dispatchEvent(new Event('change',{bubbles:true}));$('cookie-api-fetch-saved').click();return true})()`)
	if !eval(`$('cookie-api-method').open && settingsFormDirty('cookie-api-form') && $('toast').textContent.includes('尚未保存')`) || fetches.Load() != 1 {
		t.Fatal("quick action bypassed unsaved configuration")
	}
	eval(`(()=>{$('cookie-manual-method').open=true;$('cookie-content').value='auth_token=fixture-manual; ct0=fixture-csrf';$('cookie-content').dispatchEvent(new Event('input',{bubbles:true}));document.querySelector('#cookie-api-form button[value=save]').click();return true})()`)
	wait(`!$('cookie-api-fields').disabled && !settingsFormDirty('cookie-api-form')`)
	if !eval(`settingsFormDirty('cookie-form') && $('cookie-content').value.includes('fixture-manual')`) || fetches.Load() != 1 {
		t.Fatal("source save consumed manual draft or fetched unexpectedly")
	}
	eval(`(()=>{document.querySelector('#cookie-form button[type=submit]').click();return true})()`)
	wait(`!settingsFormDirty('cookie-form') && $('cookie-content').value===''`)
	cookies, err := a.store.LoadCookies()
	if err != nil || cookies.AuthToken != "fixture-manual" {
		t.Fatal("manual import failed", err)
	}
	if !eval(`$('cookie-api-summary').textContent.includes('自动恢复未开启') && $('cookie-api-token').value===''`) {
		t.Fatal("source summary did not update")
	}
	// A failed quick fetch stays visible even with both configuration rows closed.
	failed.Store(true)
	eval(`(()=>{$('cookie-manual-method').open=false;$('cookie-api-method').open=false;$('cookie-api-fetch-saved').click();return true})()`)
	wait(`!$('cookie-api-fetch-saved').disabled && $('cookie-api-result').classList.contains('form-error')`)
	after, err := a.store.LoadCookies()
	if err != nil || after != cookies {
		t.Fatal("failed fetch overwrote working credentials", err)
	}
	if !eval(`$('cookie-api-result').getBoundingClientRect().height>0 && !$('cookie-api-method').open`) {
		t.Fatal("collapsed source hid failure")
	}
}
