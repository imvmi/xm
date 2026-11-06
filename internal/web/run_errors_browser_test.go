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
	"xm/internal/archive"
)

func TestRunErrorDetailsBrowser(t *testing.T) {
	if os.Getenv("XM_BROWSER_TEST") != "1" {
		t.Skip("offline Chrome fixture")
	}
	a := testApp(t)
	id := seedErrorRun(t, a)
	if _, err := a.store.DB.Exec("UPDATE media SET last_error='媒体下载返回 HTTP 403 <error>' WHERE post_id='200'"); err != nil {
		t.Fatal(err)
	}
	for i := 800; i < 831; i++ {
		p := archive.Post{ID: fmt.Sprint(i), Username: "alice", Assets: []archive.Asset{{ID: "photo", Kind: "photo", URL: "https://pbs.twimg.com/fixture.jpg"}}}
		if err := a.store.SavePosts("alice", []archive.Post{p}); err != nil {
			t.Fatal(err)
		}
	}
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
				t.Fatal("UI timed out", js)
			}
			time.Sleep(30 * time.Millisecond)
		}
	}
	if err := chromedp.Do(browser, chromedp.Navigate(server.URL)); err != nil {
		t.Fatal(err)
	}
	wait(`!$('login-view').hidden`)
	password, _ := json.Marshal(testPassword)
	eval(`(()=>{$('username').value='admin';$('password').value=` + string(password) + `;document.querySelector('#login-form button[type="submit"]').click();return true})()`)
	wait(`typeof state!=='undefined'&&state.authenticated`)
	eval(`(()=>{location.hash='activity';return true})()`)
	wait(fmt.Sprintf(`!!document.querySelector('[data-run-errors="%d"]')`, id))
	if !eval(`!$('runs').textContent.includes('HTTP 403') && $('runs').textContent.includes('错误详情')`) {
		t.Fatal("long diagnostic not folded into details")
	}
	eval(`(()=>{document.querySelector('[data-run-errors]').click();return true})()`)
	wait(`$('run-errors-dialog').open&&document.querySelectorAll('#run-errors-media .media-issue').length===30`)
	if !eval(`$('run-errors-message').textContent.includes('历史记录') && $('run-errors-media').textContent.includes('HTTP 403 <error>') && !$('run-errors-media').querySelector('error') && !$('run-errors-more').hidden && $('run-errors-media').textContent.includes('不能仅凭状态码确定原因')`) {
		t.Fatal("missing history, escaped error, or 403 guidance")
	}
	eval(`(()=>{$('run-errors-more').click();return true})()`)
	wait(`document.querySelectorAll('#run-errors-media .media-issue').length===36&&$('run-errors-more').hidden`)
	for _, theme := range []string{"light", "dark"} {
		eval(`(()=>{applyArchiveTheme('` + theme + `');return true})()`)
		for _, width := range []int64{1280, 390} {
			if err := chromedp.Do(browser, chromedp.EmulateViewport(width, 844)); err != nil {
				t.Fatal(err)
			}
			eval(`(()=>{$('run-errors-dialog').scrollTop=0;return true})()`)
			if !eval(`document.documentElement.scrollWidth<=innerWidth && $('run-errors-dialog').scrollWidth<=$('run-errors-dialog').clientWidth`) {
				t.Fatal("dialog overflow", theme, width)
			}
			if dir := os.Getenv("XM_UI_SCREENSHOT_DIR"); dir != "" {
				shot, err := chromedp.Run(browser, chromedp.CaptureScreenshot())
				if err != nil {
					t.Fatal(err)
				}
				if err = os.WriteFile(filepath.Join(dir, fmt.Sprintf("run-errors-%s-%d.png", theme, width)), shot, 0600); err != nil {
					t.Fatal(err)
				}
			}
		}
	}
	eval(`(()=>{window.originalConfirm=window.confirm;window.confirm=()=>false;document.querySelector('[data-media-post="200"][data-media-operation="ignore"]').click();return true})()`)
	if !eval(`document.querySelector('[data-media-post="200"][data-media-operation="ignore"]')!==null&&!runErrorsState.busy`) {
		t.Fatal("cancelled ignore changed the UI")
	}
	eval(`(()=>{window.confirm=()=>true;document.querySelector('[data-media-post="200"][data-media-operation="ignore"]').click();return true})()`)
	wait(`!runErrorsState.busy&&$('run-errors-status').textContent.includes('已忽略此媒体')`)
	if !eval(`document.querySelector('[data-media-post="200"][data-media-operation="retry"]').textContent==='恢复重试' && $('run-errors-message').textContent.includes('历史记录')`) {
		t.Fatal("ignored media lost restore action or history")
	}
	eval(`(()=>{$('refresh-runs').click();return true})()`)
	if !eval(`$('run-errors-dialog').open`) {
		t.Fatal("history refresh closed details")
	}
	eval(`(()=>{document.querySelector('[data-media-post="200"][data-media-operation="retry"]').click();return true})()`)
	wait(`!runErrorsState.busy&&$('run-errors-status').textContent.includes('已恢复媒体重试资格')`)
	if !eval(`document.querySelector('[data-media-post="200"]').closest('.media-issue').textContent.includes('等待下次采集下载')`) {
		t.Fatal("restore did not update media status")
	}
	// Closing and reopening must not allow an older asynchronous response to win.
	eval(`(()=>{window.savedErrorAPI=api;api=(path,...args)=>path.includes('/errors')?new Promise(resolve=>{window.resolveOldErrors=resolve}):window.savedErrorAPI(path,...args);loadRunErrors();$('run-errors-dialog').close();return true})()`)
	wait(`!$('run-errors-dialog').open`)
	eval(`(()=>{api=window.savedErrorAPI;document.querySelector('[data-run-errors]').click();return true})()`)
	wait(`$('run-errors-dialog').open&&!$('run-errors-history').hidden`)
	eval(`(()=>{window.resolveOldErrors({run:{target:'stale',status:'failed',message:'stale'},items:[],total:0});return true})()`)
	if !eval(`!$('run-errors-message').textContent.includes('stale')`) {
		t.Fatal("stale response overwrote reopened dialog")
	}
	eval(`(()=>{document.querySelector('#run-errors-media [data-error-post-link]').click();return true})()`)
	wait(`!$('run-errors-dialog').open&&state.view==='home'&&location.hash.startsWith('#post/')`)
}
