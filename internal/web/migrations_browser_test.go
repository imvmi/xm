package web

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/chromedp/chromedp"
)

func TestMigrationBrowser(t *testing.T) {
	if os.Getenv("XM_BROWSER_TEST") != "1" {
		t.Skip("offline Chrome UI fixture")
	}
	a := testApp(t)
	corrupt, calls := migrationS3Fixture(t, a)
	corrupt.Store(true)
	t.Cleanup(a.migrations.stop)
	server := httptest.NewServer(a.Handler())
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
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
		deadline := time.Now().Add(10 * time.Second)
		for !eval(js) {
			if time.Now().After(deadline) {
				detail, _ := chromedp.Run(browser, chromedp.Evaluate[string](`JSON.stringify({migration:typeof migrationState==='undefined'?null:migrationState,error:document.getElementById('migration-error')?.textContent,toast:document.getElementById('toast')?.textContent})`))
				t.Fatalf("timed out %s: %s", js, detail)
			}
			time.Sleep(25 * time.Millisecond)
		}
	}
	// Open settings directly: the home timeline legitimately reads remote images,
	// which would contaminate this test's no-S3-access-before-migration assertion.
	if err := chromedp.Do(browser, chromedp.Navigate(server.URL+"/#settings")); err != nil {
		t.Fatal(err)
	}
	wait(`typeof state!=='undefined' && !!$('username')`)
	password, _ := json.Marshal(testPassword)
	eval(`(()=>{$('username').value='admin';$('password').value=` + string(password) + `;document.querySelector('#login-form button[type="submit"]').click();return true})()`)
	wait(`state.authenticated`)
	eval(`(()=>{location.hash='settings';return true})()`)
	wait(`typeof migrationState!=='undefined' && !!migrationState`)
	eval(`(()=>{$('settings-tab-migration').click();return true})()`)
	if !eval(`!!$('migration-database-section') && !!$('migration-storage-section') && $('migration-source').value==='s3-test' && $('migration-storage-target').value==='local'`) {
		t.Fatal("migration sections or configured direction missing")
	}
	for _, theme := range []string{"light", "dark"} {
		for _, width := range []int64{1280, 390} {
			if err := chromedp.Do(browser, chromedp.EmulateViewport(width, 844)); err != nil {
				t.Fatal(err)
			}
			eval(fmt.Sprintf(`(()=>{applyArchiveTheme(%q);return true})()`, theme))
			if !eval(`!$('settings-migration').hidden && document.documentElement.scrollWidth<=innerWidth`) {
				t.Fatalf("migration tab overflow %s %d", theme, width)
			}
		}
	}
	if calls.Load() != 0 {
		t.Fatal("opening UI started migration")
	}
	eval(`(()=>{$('migration-preview').click();return true})()`)
	wait(`!$('migration-confirm-panel').hidden`)
	if !eval(`$('migration-start').disabled && $('migration-preview-summary').textContent.includes('可用')`) {
		t.Fatal("missing preview or confirmation guard")
	}
	eval(`(()=>{$('migration-confirm').value='开始';$('migration-confirm').dispatchEvent(new Event('input'));return true})()`)
	if !eval(`$('migration-start').disabled`) {
		t.Fatal("partial confirmation allowed")
	}
	eval(`(()=>{$('migration-confirm').value='开始迁移';$('migration-confirm').dispatchEvent(new Event('input'));$('migration-start').click();return true})()`)
	wait(`migrationState?.job?.status==='failed' && !$('migration-retry').hidden`)
	if !eval(`$('migration-message').textContent.includes('失败') && !$('migration-failures-panel').hidden`) {
		t.Fatal("failure not visible")
	}
	if dir := os.Getenv("XM_UI_SCREENSHOT_DIR"); dir != "" {
		image, err := chromedp.Run(browser, chromedp.CaptureScreenshot())
		if err != nil {
			t.Fatal(err)
		}
		if err = os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
		if err = os.WriteFile(filepath.Join(dir, "migration-dark-mobile.png"), image, 0600); err != nil {
			t.Fatal(err)
		}
	}
	corrupt.Store(false)
	eval(`(()=>{$('migration-retry').click();return true})()`)
	wait(`!$('migration-confirm-panel').hidden && migrationRetry`)
	eval(`(()=>{$('migration-confirm').value='开始迁移';$('migration-confirm').dispatchEvent(new Event('input'));$('migration-start').click();return true})()`)
	wait(`migrationState?.job?.status==='completed'`)
	if !eval(`$('migration-progress').value===$('migration-progress').max && $('migration-retry').hidden`) {
		t.Fatal("completion state incorrect")
	}
	// Local storage can select the configured S3 or add either remote type.
	eval(`(()=>{$('migration-source').value='local';$('migration-source').dispatchEvent(new Event('change'));$('migration-storage-target').value='new';$('migration-storage-target').dispatchEvent(new Event('change'));return true})()`)
	if !eval(`!$('migration-remote-fields').hidden && !$('migration-s3-fields').hidden && [...$('migration-storage-target').options].some(o=>o.value==='s3-test')`) {
		t.Fatal("local outbound choices missing")
	}
	for _, kind := range []string{"s3", "webdav"} {
		eval(fmt.Sprintf(`(()=>{$('migration-remote-kind').value=%q;$('migration-remote-kind').dispatchEvent(new Event('change'));return true})()`, kind))
		for _, width := range []int64{1280, 390} {
			if err := chromedp.Do(browser, chromedp.EmulateViewport(width, 844)); err != nil {
				t.Fatal(err)
			}
			if !eval(`document.documentElement.scrollWidth<=innerWidth`) {
				t.Fatalf("outbound form overflow %s %d", kind, width)
			}
		}
	}
	if a.Config().Database.DriverName() == "sqlite" && !eval(`!$('migration-mysql-fields').hidden && $('migration-database-target').textContent==='MySQL'`) {
		t.Fatal("SQLite to MySQL form missing")
	}
	if dir := os.Getenv("XM_UI_SCREENSHOT_DIR"); dir != "" {
		if err := chromedp.Do(browser, chromedp.EmulateViewport(1280, 1000)); err != nil {
			t.Fatal(err)
		}
		eval(`(()=>{applyArchiveTheme('light');$('migration-database-section').scrollIntoView({behavior:'instant',block:'start'});return true})()`)
		shot, err := chromedp.Run(browser, chromedp.CaptureScreenshot())
		if err != nil {
			t.Fatal(err)
		}
		if err = os.WriteFile(filepath.Join(dir, "migration-outbound-desktop.png"), shot, 0600); err != nil {
			t.Fatal(err)
		}
	}
}
