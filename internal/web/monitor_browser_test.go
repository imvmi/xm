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

func TestAccountMonitoringBrowser(t *testing.T) {
	if os.Getenv("XM_BROWSER_TEST") != "1" {
		t.Skip("offline Chrome UI fixture")
	}
	a := testApp(t)
	for i, name := range []string{"alice", "bob", "long_username15"} {
		if _, err := a.store.DB.Exec("INSERT INTO monitors(username,enabled,next_run,last_finished) VALUES(?,1,2000000000,?)", name, 100+i); err != nil {
			t.Fatal(err)
		}
		posts := []archive.Post{}
		for j := 0; j <= i; j++ {
			posts = append(posts, archive.Post{ID: fmt.Sprint(100 + i*10 + j), Username: name, Text: "账号监控测试推文", Raw: json.RawMessage(`{}`)})
		}
		if err := a.store.SavePosts(name, posts); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := a.store.DB.Exec("UPDATE monitors SET last_error='请求暂时失败，请检查网络连接后重试；已有推文与分页断点均已保留。',baseline_status='blocked' WHERE username='long_username15'"); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(a.Handler())
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 75*time.Second)
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
		deadline := time.Now().Add(8 * time.Second)
		for !eval(js) {
			if time.Now().After(deadline) {
				detail, _ := chromedp.Run(browser, chromedp.Evaluate[string](`document.getElementById('toast')?.textContent||document.body.textContent`))
				t.Fatalf("timeout %s: %s", js, detail)
			}
			time.Sleep(30 * time.Millisecond)
		}
	}
	if err := chromedp.Do(browser, chromedp.Navigate(server.URL+"/#monitors"), chromedp.Reload()); err != nil {
		t.Fatal(err)
	}
	wait(`!!document.querySelector('#login-form button[type=submit]')`)
	password, _ := json.Marshal(testPassword)
	eval(`(()=>{$('username').value='admin';$('password').value=` + string(password) + `;document.querySelector('#login-form button[type=submit]').click();return true})()`)
	wait(`typeof state!=='undefined' && state.authenticated && state.monitors.length===3 && document.querySelectorAll('#monitor-rows tr').length===3`)
	if !eval(`document.querySelector('[data-view=monitors]').textContent==='账号监控' && document.querySelector('#view-monitors h1').textContent==='账号监控'`) {
		t.Fatal("module not renamed")
	}
	for _, tc := range []struct{ sort, first string }{{"posts-desc", "long_username15"}, {"posts-asc", "alice"}, {"recent", "long_username15"}, {"username", "alice"}} {
		eval(fmt.Sprintf(`(()=>{$('monitor-sort').value=%q;$('monitor-sort').dispatchEvent(new Event('change'));return true})()`, tc.sort))
		if !eval(fmt.Sprintf(`document.querySelector('#monitor-rows [data-op=timeline]').dataset.monitor===%q`, tc.first)) {
			t.Fatal("wrong order", tc.sort)
		}
	}
	// Inline switch saves independently and only removes the account from home.
	eval(`(()=>{document.querySelector('[data-monitor=bob][data-op=timeline]').click();return true})()`)
	wait(`state.monitors.find(m=>m.username==='bob').show_in_timeline===false && document.querySelector('[data-monitor=bob][data-op=timeline]').getAttribute('aria-checked')==='false'`)
	eval(`(()=>{$('monitor-filter').value='hidden';$('monitor-filter').dispatchEvent(new Event('change'));return true})()`)
	if !eval(`document.querySelectorAll('#monitor-rows tr').length===1 && $('monitor-rows').textContent.includes('@bob')`) {
		t.Fatal("hidden filter")
	}
	eval(`(()=>{location.hash='home';return true})()`)
	wait(`state.view==='home' && !state.postsLoading && state.postIDs.size===4`)
	eval(`(()=>{location.hash='user/bob';return true})()`)
	wait(`state.view==='user' && !state.postsLoading && state.postIDs.size===2`)
	if err := chromedp.Do(browser, chromedp.Navigate(server.URL+"/#monitors"), chromedp.Reload()); err != nil {
		t.Fatal(err)
	}
	wait(`typeof state!=='undefined' && state.monitors.length===3 && !!document.querySelector('[data-monitor=bob][data-op=edit]')`)
	if !eval(`document.querySelector('[data-monitor=bob][data-op=timeline]').getAttribute('aria-checked')==='false'`) {
		t.Fatal("preference lost on reload")
	}
	eval(`(()=>{document.querySelector('[data-monitor=bob][data-op=edit]').click();return true})()`)
	wait(`$('monitor-dialog').open`)
	if !eval(`!$('monitor-timeline').checked && $('monitor-enabled').checked`) {
		t.Fatal("edit did not load preference")
	}
	eval(`(()=>{$('monitor-timeline').checked=true;document.querySelector('#monitor-form button[type=submit]').click();return true})()`)
	wait(`!$('monitor-dialog').open && state.monitors.find(m=>m.username==='bob').show_in_timeline===true`)
	eval(`(()=>{$('monitor-search').value='does-not-exist';$('monitor-search').dispatchEvent(new Event('input'));return true})()`)
	if !eval(`!$('monitor-no-match').hidden && document.querySelectorAll('#monitor-rows tr').length===0`) {
		t.Fatal("empty search")
	}
	eval(`(()=>{$('monitor-search').value='';$('monitor-search').dispatchEvent(new Event('input'));$('monitor-sort').value='posts-desc';$('monitor-sort').dispatchEvent(new Event('change'));document.querySelector('#monitor-rows details').open=true;return true})()`)
	// Refresh must preserve ordering and expanded diagnostics.
	eval(`(()=>{refreshMonitors();return true})()`)
	wait(`document.querySelector('#monitor-rows [data-op=timeline]').dataset.monitor==='long_username15' && document.querySelector('#monitor-rows details').open`)
	for _, width := range []int64{1440, 1024, 390, 320} {
		if err := chromedp.Do(browser, chromedp.EmulateViewport(width, 1000)); err != nil {
			t.Fatal(err)
		}
		for _, theme := range []string{"light", "dark"} {
			eval(fmt.Sprintf(`(()=>{applyArchiveTheme(%q);return true})()`, theme))
			if !eval(`document.documentElement.scrollWidth<=innerWidth && [...document.querySelectorAll('#monitor-rows td')].every(el=>el.scrollWidth<=el.clientWidth+1)`) {
				t.Fatalf("overflow %d %s", width, theme)
			}
			if !eval(`(()=>{const b=document.querySelector('[data-op=timeline]');return b.getBoundingClientRect().height>=44 && b.getAttribute('role')==='switch' && b.getAttribute('aria-label').includes('主时间线')})()`) {
				t.Fatal("switch accessibility")
			}
			if theme == "dark" && !eval(`(()=>{const row=document.querySelector('#monitor-rows tr');const c=getComputedStyle(row).backgroundColor.match(/[\d.]+/g);return Number(c[0])<100})()`) {
				t.Fatal("light card in dark mode")
			}
			if dir := os.Getenv("XM_UI_SCREENSHOT_DIR"); dir != "" {
				image, err := chromedp.Run(browser, chromedp.CaptureScreenshot())
				if err != nil {
					t.Fatal(err)
				}
				if err = os.WriteFile(filepath.Join(dir, fmt.Sprintf("monitors-%s-%d.png", theme, width)), image, 0600); err != nil {
					t.Fatal(err)
				}
			}
		}
	}
}
