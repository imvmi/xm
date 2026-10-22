package web

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/chromedp/chromedp"
)

func TestDataCleanupBrowser(t *testing.T) {
	if os.Getenv("XM_BROWSER_TEST") != "1" {
		t.Skip("offline Chrome UI fixture")
	}
	a := testApp(t)
	seedDataCleanup(t, a)
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
				t.Fatal("cleanup UI timed out", js)
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
	wait(`state.authenticated && !!migrationState`)
	eval(`(()=>{$('settings-tab-account').dispatchEvent(new KeyboardEvent('keydown',{key:'End',bubbles:true}));$('settings-tab-credentials').dispatchEvent(new KeyboardEvent('keydown',{key:'ArrowLeft',bubbles:true}));return true})()`)
	if !eval(`!$('settings-cleanup').hidden && $('settings-tab-cleanup').getAttribute('aria-selected')==='true' && document.activeElement===$('settings-tab-cleanup')`) {
		t.Fatal("cleanup tab is not keyboard accessible")
	}
	for _, theme := range []string{"light", "dark"} {
		for _, width := range []int64{1280, 390} {
			if err := chromedp.Do(browser, chromedp.EmulateViewport(width, 844)); err != nil {
				t.Fatal(err)
			}
			eval(fmt.Sprintf(`(()=>{applyArchiveTheme(%q);return true})()`, theme))
			if !eval(`document.documentElement.scrollWidth<=innerWidth`) {
				t.Fatalf("cleanup overflow: %s %d", theme, width)
			}
		}
	}
	eval(`(()=>{$('data-cleanup-notifications').click();$('data-cleanup-tasks').click();return true})()`)
	if !eval(`$('data-cleanup-open').disabled`) {
		t.Fatal("cleanup allowed with no selection")
	}
	eval(`(()=>{$('data-cleanup-notifications').click();$('data-cleanup-open').click();return true})()`)
	if !eval(`$('data-cleanup-dialog').open && $('data-cleanup-description').textContent.includes('通知发送记录') && !$('data-cleanup-description').textContent.includes('任务记录')`) {
		t.Fatal("confirmation did not match selection")
	}
	eval(`(()=>{document.querySelector('[data-close="data-cleanup-dialog"]').click();return true})()`)
	assertCleanupCount(t, a, "notification_deliveries", 39)
	eval(`(()=>{$('data-cleanup-open').click();$('data-cleanup-confirm').click();return true})()`)
	wait(`!$('data-cleanup-dialog').open && $('data-cleanup-result').textContent.includes('38 条通知发送记录')`)
	assertCleanupCount(t, a, "notification_deliveries", 1)
	assertCleanupCount(t, a, "runs", 28)
	eval(`(()=>{$('data-cleanup-notifications').click();$('data-cleanup-tasks').click();$('data-cleanup-open').click();$('data-cleanup-confirm').click();return true})()`)
	wait(`!$('data-cleanup-dialog').open && $('data-cleanup-result').textContent.includes('25 条任务执行记录')`)
	assertCleanupCount(t, a, "runs", 3)
	assertCleanupCount(t, a, "collection_jobs", 3)
	eval(`(()=>{location.hash='notifications';return true})()`)
	wait(`!$('view-notifications').hidden && document.querySelectorAll('.notification-delivery').length===1`)
	if !eval(`$('notification-deliveries').textContent.includes('等待发送 / 重试')`) {
		t.Fatal("pending delivery missing after cleanup")
	}
}
