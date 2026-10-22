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
	if _, err := a.store.DB.Exec("UPDATE monitors SET replies=1 WHERE username='bob'"); err != nil {
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
	openMenu := func(username string) {
		t.Helper()
		eval(fmt.Sprintf(`(()=>{document.querySelector('[data-account="%s"]').click();return true})()`, username))
		wait(fmt.Sprintf(`$('account-menu-dialog').open && accountMenuTarget===%q`, username))
	}
	if !eval(`!document.querySelector('#user-list details') && [...$('user-list').children].every(card=>card.querySelectorAll('[role=switch]').length===2)`) {
		t.Fatal("cards must contain two switches and no collection details")
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
	// Collection and visibility switches save independently, without opening a menu.
	wait(`!accountPending.size`)
	eval(`(()=>{const original=window.fetch;window.accountToggleBodies=[];window.fetch=(url,...args)=>{if(String(url).endsWith('/monitors/bob')&&args[0]?.method==='PUT')accountToggleBodies.push(JSON.parse(args[0].body));return original(url,...args)};document.querySelector('[data-monitor=bob][data-op=toggle]').click();return true})()`)
	wait(`!accountPending.size && !state.monitors.find(m=>m.username==='bob').enabled`)
	if !eval(`!$('account-menu-dialog').open && !state.monitors.find(m=>m.username==='bob').show_in_timeline && state.monitors.find(m=>m.username==='bob').replies && document.querySelector('[data-monitor=bob][data-op=toggle]').getAttribute('aria-checked')==='false' && accountToggleBodies.length===1 && !('show_in_timeline' in accountToggleBodies[0])`) {
		t.Fatal("collection toggle changed visibility or opened management")
	}
	// Simulate a failed save: the switch must retain its server-confirmed state.
	eval(`(()=>{window.accountTestFetch=window.fetch;window.fetch=(url,...args)=>String(url).endsWith('/monitors/bob')&&args[0]?.method==='PUT'?Promise.resolve(new Response(JSON.stringify({error:'fixture toggle failure'}),{status:500})):accountTestFetch(url,...args);document.querySelector('[data-monitor=bob][data-op=toggle]').click();return true})()`)
	wait(`!accountPending.size && $('toast').textContent==='fixture toggle failure'`)
	if !eval(`!state.monitors.find(m=>m.username==='bob').enabled && document.querySelector('[data-monitor=bob][data-op=toggle]').getAttribute('aria-checked')==='false'`) {
		t.Fatal("failed toggle changed state")
	}
	eval(`(()=>{window.fetch=window.accountTestFetch;document.querySelector('[data-monitor=bob][data-op=toggle]').click();return true})()`)
	wait(`!accountPending.size && state.monitors.find(m=>m.username==='bob').enabled`)
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
	openMenu("bob")
	if !eval(`!document.querySelector('#account-menu-content [data-op=edit]') && document.querySelector('#account-menu-content [data-op=replies]').getAttribute('aria-checked')==='true'`) {
		t.Fatal("management must expose the saved replies switch instead of configuration editing")
	}
	// Hold the save to check duplicate prevention and preservation of other preferences.
	eval(`(()=>{window.repliesFetch=window.fetch;window.replySaves=[];window.fetch=(url,...args)=>{if(String(url).endsWith('/monitors/bob')&&args[0]?.method==='PUT'){replySaves.push(JSON.parse(args[0].body));return new Promise(resolve=>{window.finishReplySave=()=>resolve(repliesFetch(url,...args))})}return repliesFetch(url,...args)};const b=document.querySelector('#account-menu-content [data-op=replies]');b.focus();b.click();document.querySelector('#account-menu-content [data-op=replies]').click();return true})()`)
	if !eval(`$('account-menu-dialog').open && document.querySelector('#account-menu-content [data-op=replies]').disabled && replySaves.length===1 && replySaves[0].replies===false && replySaves[0].enabled===true && !('show_in_timeline' in replySaves[0])`) {
		t.Fatal("reply save must remain in the menu and disable duplicate submissions")
	}
	eval(`(()=>{window.fetch=window.repliesFetch;finishReplySave();return true})()`)
	wait(`!accountPending.size && !state.monitors.find(m=>m.username==='bob').replies`)
	if !eval(`$('account-menu-dialog').open && document.activeElement.dataset.op==='replies' && state.monitors.find(m=>m.username==='bob').enabled && !state.monitors.find(m=>m.username==='bob').show_in_timeline && document.querySelector('#account-menu-content [data-op=replies]').getAttribute('aria-checked')==='false'`) {
		t.Fatal("reply save changed another preference or lost focus")
	}
	// Failed saves retain the previous value and keep the management menu usable.
	eval(`(()=>{window.fetch=(url,...args)=>String(url).endsWith('/monitors/bob')&&args[0]?.method==='PUT'?Promise.resolve(new Response(JSON.stringify({error:'fixture replies failure'}),{status:500})):repliesFetch(url,...args);document.querySelector('#account-menu-content [data-op=replies]').click();return true})()`)
	wait(`!accountPending.size && $('toast').textContent==='fixture replies failure'`)
	if !eval(`$('account-menu-dialog').open && !state.monitors.find(m=>m.username==='bob').replies && document.querySelector('#account-menu-content [data-op=replies]').getAttribute('aria-checked')==='false'`) {
		t.Fatal("failed reply save changed state")
	}
	eval(`(()=>{window.fetch=window.repliesFetch;$('account-menu-dialog').close();document.querySelector('[data-monitor=bob][data-op=toggle]').click();return true})()`)
	wait(`!accountPending.size && !state.monitors.find(m=>m.username==='bob').enabled`)
	openMenu("bob")
	eval(`(()=>{document.querySelector('#account-menu-content [data-op=replies]').click();return true})()`)
	wait(`!accountPending.size && state.monitors.find(m=>m.username==='bob').replies`)
	if !eval(`!state.monitors.find(m=>m.username==='bob').enabled && !state.monitors.find(m=>m.username==='bob').show_in_timeline`) {
		t.Fatal("reply save enabled automatic collection or home visibility")
	}
	var replies, enabled, shown int
	if err := a.store.DB.QueryRow("SELECT replies,enabled,show_in_timeline FROM monitors WHERE username='bob'").Scan(&replies, &enabled, &shown); err != nil || replies != 1 || enabled != 0 || shown != 0 {
		t.Fatalf("reply preference not persisted independently: %d %d %d %v", replies, enabled, shown, err)
	}
	eval(`(()=>{$('account-menu-dialog').close();document.querySelector('[data-monitor=bob][data-op=timeline]').click();return true})()`)
	wait(`!accountPending.size && state.monitors.find(m=>m.username==='bob').show_in_timeline===true`)
	eval(`(()=>{$('user-search').value='does-not-exist';$('user-search').dispatchEvent(new Event('input'));return true})()`)
	if !eval(`!$('user-empty').hidden && document.querySelectorAll('#user-list .account-card').length===0`) {
		t.Fatal("empty search")
	}
	eval(`(()=>{$('user-search').value='';$('user-search').dispatchEvent(new Event('input'));$('monitor-sort').value='posts-desc';$('monitor-sort').dispatchEvent(new Event('change'));document.querySelector('[data-account=long_username15] .account-stats').click();return true})()`)
	wait(`$('account-menu-dialog').open && accountMenuTarget==='long_username15'`)
	if !eval(`$('account-menu-content').textContent.includes('包含回复') && document.querySelector('[data-account=long_username15] .badge').textContent.includes('需关注')`) {
		t.Fatal("card click did not show operations or hid the attention indicator")
	}
	eval(`(()=>{document.querySelector('#account-menu-content .account-details').open=true;return true})()`)
	wait(`accountExpanded.has('long_username15:status')`)
	// Refresh must preserve ordering and expanded diagnostics.
	eval(`(()=>{refreshMonitors();return true})()`)
	wait(`document.querySelector('#user-list [data-op=timeline]').dataset.monitor==='long_username15' && document.querySelector('#account-menu-content .account-details').open && $('account-menu-content').textContent.includes('已有推文与分页断点均已保留')`)
	if err := chromedp.Do(browser, chromedp.KeyEvent("\u001b")); err != nil {
		t.Fatal(err)
	}
	wait(`!$('account-menu-dialog').open`)
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
			if width <= 760 && !eval(`(()=>{const group=document.querySelector('#view-archive .heading-actions'),r=group.getBoundingClientRect(),heading=group.parentElement.getBoundingClientRect(),buttons=[...group.children].map(b=>b.getBoundingClientRect());return Math.abs(r.width-heading.width)<1 && buttons.every(b=>Math.abs(b.top-buttons[0].top)<1 && Math.abs(b.width-buttons[0].width)<1 && b.height>=44) && [...group.children].every(b=>b.scrollWidth<=b.clientWidth+1)})()`) {
				t.Fatalf("mobile header actions must fill one equal-width row at %d", width)
			}
			if !eval(`(()=>{const grid=document.querySelector('.account-stats-grid'),box=document.querySelector('.account-overview'),cells=[...grid.children],columns=getComputedStyle(grid).gridTemplateColumns.split(' ').length;return columns===2 && box.matches(':popover-open') && box.getBoundingClientRect().left>=0 && box.getBoundingClientRect().right<=innerWidth && box.getBoundingClientRect().bottom<=innerHeight && $('stat-posts').getBoundingClientRect().left-box.getBoundingClientRect().left>=14 && Math.abs(box.getBoundingClientRect().bottom-grid.getBoundingClientRect().bottom)<=2 && cells.every(cell=>parseFloat(getComputedStyle(cell).paddingLeft)>=14 && cell.scrollWidth<=cell.clientWidth+1)})()`) {
				t.Fatalf("archive overview spacing or columns incorrect %d %s", width, theme)
			}
			if !eval(`(()=>{const card=document.querySelector('[data-account=long_username15]'),name=card.querySelector('.account-name'),handle=card.querySelector('.user-handle'),closed=document.querySelector('[data-account=alice]'),controls=closed.querySelector('.account-controls');return [name,handle].every(el=>getComputedStyle(el).whiteSpace==='nowrap' && getComputedStyle(el).textOverflow==='ellipsis' && el.getBoundingClientRect().height<=parseFloat(getComputedStyle(el).lineHeight)+1 && el.title.includes('long_username15')) && closed.getBoundingClientRect().bottom-controls.getBoundingClientRect().bottom<=8 && !card.querySelector('.account-details')})()`) {
				t.Fatalf("account truncation or bottom spacing incorrect %d %s", width, theme)
			}
			if !eval(`(()=>{const card=$('user-list').firstElementChild,a=card.querySelector('[data-op=timeline]'),b=card.querySelector('[data-op=toggle]'),ar=a.getBoundingClientRect(),br=b.getBoundingClientRect();return ar.height>=44 && br.height>=44 && br.left>=ar.right && Math.abs(ar.top-br.top)<1 && [a,b].every(el=>el.getAttribute('role')==='switch') && a.getAttribute('aria-label').includes('主时间线') && b.getAttribute('aria-label').includes('自动采集')})()`) {
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
	// Management fits desktop/mobile, preserves expanded details and closes outside.
	for _, width := range []int64{1280, 320} {
		if err := chromedp.Do(browser, chromedp.EmulateViewport(width, 700)); err != nil {
			t.Fatal(err)
		}
		openMenu("long_username15")
		if !eval(`(()=>{const d=$('account-menu-dialog'),r=d.getBoundingClientRect();return r.left>=0 && r.right<=innerWidth && r.top>=0 && r.bottom<=innerHeight && d.scrollWidth<=d.clientWidth+1 && d.querySelector('.account-details').open})()`) {
			t.Fatal("management popup overflow or lost details", width)
		}
		if dir := os.Getenv("XM_UI_SCREENSHOT_DIR"); dir != "" {
			image, err := chromedp.Run(browser, chromedp.CaptureScreenshot())
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("account-menu-%d.png", width)), image, 0600); err != nil {
				t.Fatal(err)
			}
		}
		if err := chromedp.Do(browser, chromedp.MouseClickXY(2, 2)); err != nil {
			t.Fatal(err)
		}
		wait(`!$('account-menu-dialog').open`)
	}
	eval(`(()=>{$('archive-overview-toggle').click();return true})()`)
	if err := chromedp.Do(browser, chromedp.MouseClickXY(2, 2)); err != nil {
		t.Fatal(err)
	}
	wait(`!$('archive-overview').matches(':popover-open')`)
	// Cancellation retains the same archived row and its hidden-home preference.
	eval(`(()=>{document.querySelector('[data-monitor=bob][data-op=timeline]').click();return true})()`)
	wait(`!accountPending.size && state.monitors.find(m=>m.username==='bob').show_in_timeline===false`)
	// Archive-only rows may still arrive after removal through the external API.
	eval(`(()=>{api('/monitors/bob','DELETE').then(()=>refreshAccountsAfterChange());return true})()`)
	wait(`state.monitors.length===2 && document.querySelectorAll('[data-account=bob]').length===1 && document.querySelector('[data-account=bob]').textContent.includes('仅保留归档')`)

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
	if !eval(`document.querySelector('[data-monitor=bob][data-op=toggle]').disabled`) {
		t.Fatal("archive-only account allowed automatic collection without a monitor")
	}
	openMenu("bob")
	eval(`(()=>{document.querySelector('#account-menu-content [data-op=readd]').click();return true})()`)
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
	eval(`(()=>{window.scrollTo({top:150,behavior:"instant"});window.savedAccountScroll=scrollY;document.querySelector('[data-account=bob] .account-name').click();return true})()`)
	wait(`state.view==='user' && !state.postsLoading && state.postIDs.size===2`)
	eval(`(()=>{document.querySelector('#view-user .back-link').click();return true})()`)
	wait(`state.view==='archive' && document.querySelectorAll('#user-list .account-card').length===3 && Math.abs(scrollY-window.savedAccountScroll)<2`)
	if !eval(`window.savedAccountScroll>0 && $('monitor-sort').value==='posts-desc' && accountExpanded.has('long_username15:status')`) {
		detail, _ := chromedp.Run(browser, chromedp.Evaluate[string](`JSON.stringify({saved:window.savedAccountScroll,scroll:scrollY,sort:$('monitor-sort').value,expanded:[...accountExpanded],height:innerHeight,body:document.body.scrollHeight})`))
		t.Fatal("directory state was lost after browsing", detail)
	}
}
