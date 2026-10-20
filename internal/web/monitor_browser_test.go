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
				detail, _ := chromedp.Run(browser, chromedp.Evaluate[string](`JSON.stringify({view:state.view,hash:location.hash,query:location.search,scroll:scrollY,saved:window.savedAccountScroll,accountScroll,restoring:accountRestoring,users:state.users.length,monitors:state.monitors.length,toast:$('toast').textContent})`))
				t.Fatalf("timeout %s: %s", js, detail)
			}
			time.Sleep(30 * time.Millisecond)
		}
	}
	if err := chromedp.Do(browser, chromedp.Navigate(server.URL+"/#monitors")); err != nil {
		t.Fatal(err)
	}
	wait(`!!document.querySelector('#login-form button[type=submit]')`)
	password, _ := json.Marshal(testPassword)
	eval(`(()=>{$('username').value='admin';$('password').value=` + string(password) + `;document.querySelector('#login-form button[type=submit]').click();return true})()`)
	wait(`typeof state!=='undefined' && state.authenticated && !accountRestoring && state.users.length===3 && state.monitors.length===3 && document.querySelectorAll('#user-list .account-card').length===3 && state.users.every(u=>!u.stats_pending)`)
	if !eval(`document.querySelector('[data-view=archive] .nav-text').textContent==='账号' && document.querySelector('#view-archive h1').textContent==='账号' && !document.querySelector('#view-monitors') && document.querySelectorAll('.sidebar nav a').length===5 && location.hash==='#accounts'`) {
		t.Fatal("account pages not consolidated")
	}
	for _, tc := range []struct{ sort, first string }{{"posts-desc", "long_username15"}, {"posts-asc", "alice"}, {"recent", "long_username15"}, {"username", "alice"}} {
		eval(fmt.Sprintf(`(()=>{$('monitor-sort').value=%q;$('monitor-sort').dispatchEvent(new Event('change'));return true})()`, tc.sort))
		if !eval(fmt.Sprintf(`document.querySelector('#user-list [data-op=timeline]').dataset.monitor===%q`, tc.first)) {
			t.Fatal("wrong order", tc.sort)
		}
	}
	// Inline switch saves independently and only removes the account from home.
	eval(`(()=>{document.querySelector('[data-monitor=bob][data-op=timeline]').click();return true})()`)
	wait(`state.monitors.find(m=>m.username==='bob').show_in_timeline===false && document.querySelector('[data-monitor=bob][data-op=timeline]').getAttribute('aria-checked')==='false'`)
	eval(`(()=>{$('monitor-filter').value='hidden';$('monitor-filter').dispatchEvent(new Event('change'));return true})()`)
	if !eval(`document.querySelectorAll('#user-list .account-card').length===1 && $('user-list').textContent.includes('@bob')`) {
		t.Fatal("hidden filter")
	}
	eval(`(()=>{location.hash='home';return true})()`)
	wait(`state.view==='home' && !state.postsLoading && state.postIDs.size===4`)
	eval(`(()=>{location.hash='user/bob';return true})()`)
	wait(`state.view==='user' && !state.postsLoading && state.postIDs.size===2`)
	if err := chromedp.Do(browser, chromedp.Navigate(server.URL+"/?reload=1#monitors")); err != nil {
		t.Fatal(err)
	}
	wait(`document.readyState==='complete' && location.search==='?reload=1' && typeof state!=='undefined' && state.authenticated && state.view==='archive' && state.users.length===3 && state.monitors.length===3 && !!document.querySelector('[data-monitor=bob][data-op=timeline]')`)
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
	eval(`(()=>{$('user-search').value='does-not-exist';$('user-search').dispatchEvent(new Event('input'));return true})()`)
	if !eval(`!$('user-empty').hidden && document.querySelectorAll('#user-list .account-card').length===0`) {
		t.Fatal("empty search")
	}
	eval(`(()=>{$('user-search').value='';$('user-search').dispatchEvent(new Event('input'));$('monitor-sort').value='posts-desc';$('monitor-sort').dispatchEvent(new Event('change'));document.querySelector('[data-account=long_username15] .account-details').open=true;return true})()`)
	// Refresh must preserve ordering and expanded diagnostics.
	eval(`(()=>{refreshMonitors();return true})()`)
	wait(`document.querySelector('#user-list [data-op=timeline]').dataset.monitor==='long_username15' && document.querySelector('[data-account=long_username15] .account-details').open`)
	if !eval(`!$('archive-overview').matches(':popover-open') && !document.querySelector('#view-archive #pause-all') && document.querySelector('#view-archive .heading-actions').firstElementChild===$('archive-overview-toggle')`) {
		t.Fatal("overview must be hidden by default and replace the global pause action")
	}
	eval(`(()=>{$('archive-overview-toggle').click();$('stat-posts').textContent='57,373';$('stat-today').textContent='18,673';$('stat-media').textContent='22,718';$('stat-bytes').textContent='16.9 GB';return true})()`)
	for _, width := range []int64{1440, 1024, 390, 320} {
		if err := chromedp.Do(browser, chromedp.EmulateViewport(width, 1000)); err != nil {
			t.Fatal(err)
		}
		for _, theme := range []string{"light", "dark"} {
			eval(fmt.Sprintf(`(()=>{applyArchiveTheme(%q);return true})()`, theme))
			if !eval(`document.documentElement.scrollWidth<=innerWidth && [...document.querySelectorAll('#user-list .account-card')].every(el=>el.scrollWidth<=el.clientWidth+1)`) {
				t.Fatalf("overflow %d %s", width, theme)
			}
			if width <= 760 && !eval(`(()=>{const cards=[...document.querySelectorAll('#user-list .account-card')],a=cards[0].getBoundingClientRect(),b=cards[1].getBoundingClientRect();return getComputedStyle($('user-list')).gridTemplateColumns.split(' ').length===2 && Math.abs(a.top-b.top)<1 && b.left>=a.right && cards.every(card=>[...card.querySelectorAll('.account-identity,.account-controls,.timeline-switch,.account-actions')].every(el=>el.scrollWidth<=el.clientWidth+1))})()`) {
				t.Fatalf("mobile accounts must fit two per row at %d", width)
			}
			if !eval(`(()=>{const grid=document.querySelector('.account-stats-grid'),box=document.querySelector('.account-overview'),cells=[...grid.children],columns=getComputedStyle(grid).gridTemplateColumns.split(' ').length;return columns===2 && box.matches(':popover-open') && box.getBoundingClientRect().left>=0 && box.getBoundingClientRect().right<=innerWidth && box.getBoundingClientRect().bottom<=innerHeight && $('stat-posts').getBoundingClientRect().left-box.getBoundingClientRect().left>=14 && Math.abs(box.getBoundingClientRect().bottom-grid.getBoundingClientRect().bottom)<=2 && cells.every(cell=>parseFloat(getComputedStyle(cell).paddingLeft)>=14 && cell.scrollWidth<=cell.clientWidth+1)})()`) {
				t.Fatalf("archive overview spacing or columns incorrect %d %s", width, theme)
			}
			if !eval(`(()=>{const card=document.querySelector('[data-account=long_username15]'),name=card.querySelector('.account-name'),handle=card.querySelector('.user-handle'),closed=document.querySelector('[data-account=alice]'),summary=closed.querySelector('.account-details>summary');return [name,handle].every(el=>getComputedStyle(el).whiteSpace==='nowrap' && getComputedStyle(el).textOverflow==='ellipsis' && el.getBoundingClientRect().height<=parseFloat(getComputedStyle(el).lineHeight)+1 && el.title.includes('long_username15')) && closed.getBoundingClientRect().bottom-summary.getBoundingClientRect().bottom<=8 && summary.getBoundingClientRect().height>=40})()`) {
				t.Fatalf("account truncation or bottom spacing incorrect %d %s", width, theme)
			}
			if !eval(`(()=>{const b=document.querySelector('[data-op=timeline]');return b.getBoundingClientRect().height>=44 && b.getAttribute('role')==='switch' && b.getAttribute('aria-label').includes('主时间线')})()`) {
				t.Fatal("switch accessibility")
			}
			if theme == "dark" && !eval(`(()=>{const row=document.querySelector('#user-list .account-card');const c=getComputedStyle(row).backgroundColor.match(/[\d.]+/g);return Number(c[0])<100})()`) {
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
	eval(`(()=>{$('archive-overview-toggle').click();return true})()`)
	if !eval(`!$('archive-overview').matches(':popover-open') && $('archive-overview-toggle').getAttribute('aria-expanded')==='false'`) {
		t.Fatal("overview button did not close the popup")
	}
	eval(`(()=>{$('archive-overview-toggle').click();return true})()`)
	if err := chromedp.Do(browser, chromedp.KeyEvent("\u001b")); err != nil {
		t.Fatal(err)
	}
	wait(`!$('archive-overview').matches(':popover-open')`)
	eval(`(()=>{$('archive-overview-toggle').click();return true})()`)
	if err := chromedp.Do(browser, chromedp.MouseClickXY(2, 2)); err != nil {
		t.Fatal(err)
	}
	wait(`!$('archive-overview').matches(':popover-open')`)
	// Cancellation retains the same archived row and its hidden-home preference.
	eval(`(()=>{document.querySelector('[data-monitor=bob][data-op=timeline]').click();return true})()`)
	wait(`!accountPending.size && state.monitors.find(m=>m.username==='bob').show_in_timeline===false`)
	eval(`(()=>{document.querySelector('[data-account=bob] .account-actions').open=true;document.querySelector('[data-monitor=bob][data-op=delete]').click();return true})()`)
	wait(`$('confirm-dialog').open`)
	eval(`(()=>{$('confirm-delete').click();return true})()`)
	wait(`!$('confirm-dialog').open && !accountPending.size && state.monitors.length===2 && document.querySelectorAll('[data-account=bob]').length===1 && document.querySelector('[data-account=bob]').textContent.includes('仅保留归档')`)
	if !eval(`document.querySelector('[data-monitor=bob][data-op=timeline]').getAttribute('aria-checked')==='false'`) {
		t.Fatal("cancelling monitoring reset the hidden preference")
	}
	eval(`(()=>{$('monitor-filter').value='archived';$('monitor-filter').dispatchEvent(new Event('change'));return true})()`)
	if !eval(`document.querySelectorAll('#user-list .account-card').length===1 && !!document.querySelector('[data-account=bob]')`) {
		t.Fatal("archive-only filter")
	}
	eval(`(()=>{document.querySelector('[data-monitor=bob][data-op=timeline]').click();return true})()`)
	wait(`!accountPending.size && state.monitors.length===2 && document.querySelector('[data-monitor=bob][data-op=timeline]').getAttribute('aria-checked')==='true'`)
	eval(`(()=>{document.querySelector('[data-monitor=bob][data-op=timeline]').click();return true})()`)
	wait(`!accountPending.size && document.querySelector('[data-monitor=bob][data-op=timeline]').getAttribute('aria-checked')==='false'`)
	eval(`(()=>{document.querySelector('[data-monitor=bob][data-op=readd]').click();return true})()`)
	wait(`$('monitor-dialog').open`)
	if !eval(`!$('monitor-timeline').checked && $('monitor-username').disabled && $('monitor-enabled').checked`) {
		t.Fatal("re-add form reset the preference")
	}
	eval(`(()=>{document.querySelector('#monitor-form button[type=submit]').click();return true})()`)
	wait(`!$('monitor-dialog').open && state.monitors.length===3 && $('user-empty').hidden===false`)
	eval(`(()=>{$('monitor-filter').value='all';$('monitor-filter').dispatchEvent(new Event('change'));return true})()`)
	wait(`document.querySelectorAll('#user-list .account-card').length===3 && document.querySelectorAll('[data-account=bob]').length===1 && !state.users.find(u=>u.username==='bob').stats_pending`)
	if !eval(`state.users.find(u=>u.username==='bob').posts===2 && !state.monitors.find(m=>m.username==='bob').show_in_timeline`) {
		t.Fatal("re-add duplicated or lost the archive")
	}
	// Browsing an archive preserves directory search, sorting, details and scroll.
	if err := chromedp.Do(browser, chromedp.EmulateViewport(390, 600)); err != nil {
		t.Fatal(err)
	}
	eval(`(()=>{window.scrollTo({top:150,behavior:"instant"});window.savedAccountScroll=scrollY;document.querySelector('[data-account=bob] .account-identity').click();return true})()`)
	wait(`state.view==='user' && !state.postsLoading && state.postIDs.size===2`)
	eval(`(()=>{document.querySelector('#view-user .back-link').click();return true})()`)
	wait(`state.view==='archive' && document.querySelectorAll('#user-list .account-card').length===3 && Math.abs(scrollY-window.savedAccountScroll)<2`)
	if !eval(`window.savedAccountScroll>0 && $('monitor-sort').value==='posts-desc' && document.querySelector('[data-account=long_username15] .account-details').open`) {
		detail, _ := chromedp.Run(browser, chromedp.Evaluate[string](`JSON.stringify({saved:window.savedAccountScroll,scroll:scrollY,sort:$('monitor-sort').value,expanded:[...accountExpanded],open:document.querySelector('[data-account=long_username15] .account-details').open,height:innerHeight,body:document.body.scrollHeight})`))
		t.Fatal("directory state was lost after browsing", detail)
	}
}
