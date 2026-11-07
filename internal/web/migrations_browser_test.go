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
	eval(`(()=>{$('settings-tab-data').click();return true})()`)
	if !eval(`[...document.querySelectorAll('[data-settings-tab]')].map(b=>b.textContent).join('/')==='认证/爬虫/时间线/数据/凭据' && document.querySelector('.data-overview').contains($('database-label')) && $('data-maintenance').contains($('migration-database-section')) && document.querySelector('.data-overview').contains($('storage-readonly')) && $('data-performance').contains($('avatar-cache-clear')) && !$('data-location').hidden && $('data-performance').hidden && $('data-maintenance').hidden && !$('settings-tab-cleanup') && $('settings-cleanup').hidden && !!$('migration-database-section') && !!$('migration-storage-section') && $('migration-source').options.length===1 && $('migration-source').value==='s3-test' && $('migration-storage-target').value==='local'`) {
		t.Fatal("migration sections or configured direction missing")
	}
	eval(`(()=>{$('data-tab-location').focus();$('data-tab-location').dispatchEvent(new KeyboardEvent('keydown',{key:'ArrowRight',bubbles:true}));return true})()`)
	if !eval(`document.activeElement===$('data-tab-performance') && !$('data-performance').hidden && $('data-tab-location').tabIndex===-1`) {
		t.Fatal("data tabs lack keyboard navigation")
	}
	for _, theme := range []string{"light", "dark"} {
		for _, width := range []int64{1280, 390} {
			if err := chromedp.Do(browser, chromedp.EmulateViewport(width, 844)); err != nil {
				t.Fatal(err)
			}
			eval(fmt.Sprintf(`(()=>{applyArchiveTheme(%q);return true})()`, theme))
			if !eval(`!$('settings-data').hidden && document.documentElement.scrollWidth<=innerWidth`) {
				t.Fatalf("migration tab overflow %s %d", theme, width)
			}
			if dir := os.Getenv("XM_UI_SCREENSHOT_DIR"); dir != "" {
				for _, group := range []string{"location", "performance", "maintenance"} {
					eval(fmt.Sprintf(`(()=>{selectDataTab(%q);$('data-tab-location').scrollIntoView({block:'start',behavior:'instant'});return true})()`, group))
					shot, err := chromedp.Run(browser, chromedp.CaptureScreenshot())
					if err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("%s-%s-%d.png", group, theme, width)), shot, 0600); err != nil {
						t.Fatal(err)
					}
				}
			}
		}
	}
	if calls.Load() != 0 {
		t.Fatal("opening UI started migration")
	}
	eval(`(()=>{selectMigrationOperation('media');$('migration-preview').click();return true})()`)
	wait(`!$('migration-confirm-panel').hidden`)
	if !eval(`$('migration-start').disabled && $('migration-preview-summary').textContent.includes('可用') && $('migration-storage-section').hidden && $('migration-step-confirm').getAttribute('aria-current')==='step'`) {
		t.Fatal("missing preview or confirmation guard")
	}
	// Returning to configuration must invalidate the check and restore its controls.
	eval(`(()=>{$('migration-edit-config').click();return true})()`)
	if !eval(`migrationChecked===null && !$('migration-storage-section').hidden && $('migration-preview-panel').hidden`) {
		t.Fatal("back button retained a stale check or hid the form")
	}
	eval(`(()=>{$('migration-preview').click();return true})()`)
	wait(`!$('migration-confirm-panel').hidden`)
	// Switching tabs preserves the check, but changing operation invalidates it.
	eval(`(()=>{selectDataTab('performance');selectDataTab('maintenance');return true})()`)
	if !eval(`!!migrationChecked && !$('migration-preview-panel').hidden`) {
		t.Fatal("tab change discarded preview")
	}
	eval(`(()=>{$('migration-change-operation').click();return true})()`)
	if !eval(`migrationChecked===null && !$('migration-operation-picker').hidden && $('migration-preview-panel').hidden`) {
		t.Fatal("changing operation retained old confirmation")
	}
	eval(`(()=>{selectMigrationOperation('database');$('migration-mysql-host').value='draft-host';selectDataTab('location');selectDataTab('maintenance');return true})()`)
	if !eval(`$('migration-mysql-host').value==='draft-host' && !$('migration-database-section').hidden`) {
		t.Fatal("tab change lost migration draft")
	}
	eval(`(()=>{selectMigrationOperation('media');$('migration-preview').click();return true})()`)
	wait(`!$('migration-confirm-panel').hidden`)
	// Editing any destination invalidates a previous check.

	eval(`(()=>{$('migration-mysql-host').value='127.0.0.2';$('migration-mysql-host').dispatchEvent(new Event('input',{bubbles:true}));return true})()`)
	if !eval(`$('migration-preview-panel').hidden && migrationChecked===null && $('migration-start').disabled`) {
		t.Fatal("edited destination retained a stale migration check")
	}
	eval(`(()=>{$('migration-preview').click();return true})()`)
	wait(`!$('migration-confirm-panel').hidden`)
	eval(`(()=>{$('migration-confirm').value='开始';$('migration-confirm').dispatchEvent(new Event('input'));return true})()`)
	if !eval(`$('migration-start').disabled`) {
		t.Fatal("partial confirmation allowed")
	}
	eval(`(()=>{$('migration-confirm').value='开始迁移';$('migration-confirm').dispatchEvent(new Event('input'));$('migration-start').click();return true})()`)
	wait(`migrationState?.job?.status==='failed' && !$('migration-retry').hidden`)
	if !eval(`$('migration-source').options.length===1 && $('migration-source').value===migrationState.active_storage && migrationState.active_storage==='local' && $('storage-readonly-name').textContent==='本地文件系统'`) {
		t.Fatal("migration source did not follow the active storage after cutover")
	}
	if !eval(`$('migration-message').textContent.includes('失败') && !$('migration-failures-panel').hidden && $('migration-acknowledge').hidden`) {
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
	eval(`(()=>{selectDataTab('location');return true})()`)
	if !eval(`$('migration-job-panel').getBoundingClientRect().height>0`) {
		t.Fatal("switching tabs hid failed job")
	}
	eval(`(()=>{$('migration-retry').click();return true})()`)
	wait(`!$('migration-confirm-panel').hidden && migrationRetry`)
	if !eval(`!$('data-maintenance').hidden && $('migration-preview-panel').getBoundingClientRect().height>0 && migrationChecked.source==='s3-test' && migrationChecked.target==='local' && $('migration-source').options.length===1 && $('migration-source').value==='local'`) {
		t.Fatal("retry did not preserve the original direction independently of the current source")
	}
	eval(`(()=>{$('migration-confirm').value='开始迁移';$('migration-confirm').dispatchEvent(new Event('input'));$('migration-start').click();return true})()`)
	wait(`migrationState?.job?.status==='completed'`)
	if !eval(`$('migration-progress').value===$('migration-progress').max && $('migration-retry').hidden && !$('migration-acknowledge').hidden && !$('migration-job-panel').hidden`) {
		t.Fatal("completion state incorrect")
	}
	eval(`(()=>{$('migration-acknowledge').click();return true})()`)
	wait(`!!migrationState?.job?.acknowledged && $('migration-job-panel').hidden`)
	if err := chromedp.Do(browser, chromedp.Reload()); err != nil {
		t.Fatal(err)
	}
	wait(`typeof migrationState!=='undefined' && !!migrationState?.job?.acknowledged && $('migration-job-panel').hidden`)
	eval(`(()=>{$('settings-tab-data').click();return true})()`)
	// Local storage can select the configured S3 or add either remote type.
	eval(`(()=>{selectMigrationOperation('media');$('migration-storage-target').value='new';$('migration-storage-target').dispatchEvent(new Event('change'));return true})()`)
	if !eval(`!$('migration-remote-fields').hidden && !$('migration-s3-fields').hidden && [...$('migration-storage-target').options].some(o=>o.value==='s3-test')`) {
		t.Fatal("local outbound choices missing")
	}
	for _, kind := range []string{"s3", "webdav", "smb"} {
		eval(fmt.Sprintf(`(()=>{$('migration-remote-kind').value=%q;$('migration-remote-kind').dispatchEvent(new Event('change'));return true})()`, kind))
		if kind == "smb" && !eval(`!$('migration-webdav-fields').hidden && $('migration-s3-fields').hidden && $('migration-proxy-option').hidden && $('migration-remote-endpoint').placeholder.startsWith('smb://') && $('migration-endpoint-help').textContent.includes('数据库留在本机')`) {
			t.Fatal("SMB configuration fields incorrect")
		}
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
	if a.Config().Database.DriverName() == "sqlite" {
		eval(`(()=>{$('migration-database-engine').value='mariadb';$('migration-database-engine').dispatchEvent(new Event('change'));return true})()`)
		if !eval(`!$('migration-mysql-fields').hidden && $('migration-database-target').textContent==='MariaDB' && migrationInput('database').database.driver==='mariadb' && $('migration-database-help').textContent.includes('MariaDB')`) {
			t.Fatal("SQLite to MariaDB form missing")
		}
	}
	if dir := os.Getenv("XM_UI_SCREENSHOT_DIR"); dir != "" {
		if err := chromedp.Do(browser, chromedp.EmulateViewport(1280, 1000)); err != nil {
			t.Fatal(err)
		}
		eval(`(()=>{applyArchiveTheme('light');selectMigrationOperation('database');$('migration-database-section').scrollIntoView({behavior:'instant',block:'start'});return true})()`)
		shot, err := chromedp.Run(browser, chromedp.CaptureScreenshot())
		if err != nil {
			t.Fatal(err)
		}
		if err = os.WriteFile(filepath.Join(dir, "migration-outbound-desktop.png"), shot, 0600); err != nil {
			t.Fatal(err)
		}
	}
}
