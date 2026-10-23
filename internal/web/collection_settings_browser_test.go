package web

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/chromedp/chromedp"
)

func TestCollectionSettingsBrowser(t *testing.T) {
	if os.Getenv("XM_BROWSER_TEST") != "1" {
		t.Skip("offline Chrome UI fixture")
	}
	a := testApp(t)
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
		result, err := chromedp.Run(browser, chromedp.Evaluate[bool](js))
		if err != nil {
			t.Fatal(err)
		}
		return result
	}
	wait := func(js string) {
		t.Helper()
		ticker := time.NewTicker(50 * time.Millisecond)
		defer ticker.Stop()
		for !eval(js) {
			select {
			case <-ctx.Done():
				t.Fatal("UI timed out", js)
			case <-ticker.C:
			}
		}
	}
	if err := chromedp.Do(browser, chromedp.Navigate(server.URL)); err != nil {
		t.Fatal(err)
	}
	wait(`!document.getElementById('login-view').hidden`)
	password, _ := json.Marshal(testPassword)
	eval(`(()=>{document.getElementById('username').value='admin';document.getElementById('password').value=` + string(password) + `;document.querySelector('#login-form button[type="submit"]').click();return true})()`)
	wait(`typeof state!=='undefined' && state.authenticated`)
	eval(`(()=>{location.hash='settings';return true})()`)
	wait(`state.settings!==null && !document.getElementById('view-settings').hidden && document.getElementById('scroll-delay').value==='3'`)
	if !eval(`[...document.querySelectorAll('.sidebar nav .nav-text')].map(el=>el.textContent).join('/')==='首页/账号/任务/通知/设置' && $('settings-tab-account').textContent==='认证' && $('settings-tab-display').textContent==='时间线' && $('settings-collection').contains($('reset-rate-limit')) && !$('settings-account').contains($('reset-rate-limit')) && document.querySelectorAll('#archive-display-form .display-preference-group').length===4 && $('settings-display').textContent.includes('与「数据 → 媒体存储」中的服务器缓存相互独立')`) {
		t.Fatal("navigation and settings organization are inconsistent")
	}
	// Reader limits have their own form and apply without a restart.
	if a.store.DB.Dialect == "sqlite" {
		eval(`(()=>{$('settings-tab-data').click();return true})()`)
		if !eval(`!$('sqlite-performance-panel').hidden && $('sqlite-read-connections').value==='4'`) {
			t.Fatal("SQLite concurrency control missing")
		}
		eval(`(()=>{$('sqlite-read-connections').value='6';$('sqlite-read-connections').dispatchEvent(new Event('input',{bubbles:true}));document.querySelector('#sqlite-performance-form button[type=submit]').click();return true})()`)
		wait(`state.settings.sqlite_read_connections===6 && !settingsFormDirty('sqlite-performance-form') && $('sqlite-performance-result').textContent.includes('无需重启')`)
		if a.store.DB.Reader.Stats().MaxOpenConnections != 6 {
			t.Fatal("UI did not resize reader pool")
		}
		if !eval(`(()=>{$('sqlite-read-connections').value='17';return !$('sqlite-performance-form').checkValidity()})()`) {
			t.Fatal("invalid pool limit accepted by form")
		}
		eval(`(()=>{$('sqlite-read-connections').value='6';return true})()`)
	} else if !eval(`$('sqlite-performance-panel').hidden`) {
		t.Fatal("SQLite control displayed for MySQL")
	}
	eval(`(()=>{document.getElementById('settings-tab-collection').click();return true})()`)
	// Chrome mode saves independently, preserves drafts, and rolls back on failure.
	wait(`!$('chrome-mode').disabled`)
	if !eval(`$('chrome-mode').getAttribute('role')==='switch' && $('chrome-mode').getAttribute('aria-checked')==='true'`) {
		t.Fatal("Chrome must default to headless")
	}
	eval(`(()=>{$('proxy-url').value='http://127.0.0.1:1234';$('proxy-url').dispatchEvent(new Event('input',{bubbles:true}));$('chrome-mode').click();return true})()`)
	wait(`!chromeModePending && $('chrome-mode').getAttribute('aria-checked')==='false'`)
	if a.Config().Headless || a.Config().Proxy != "" || !eval(`$('proxy-url').value==='http://127.0.0.1:1234' && settingsFormDirty('settings-form')`) {
		t.Fatal("Chrome toggle lost or submitted proxy draft")
	}
	eval(`(()=>{window.chromeSavedAPI=api;api=async(path,method,...args)=>{if(path==='/settings'&&method==='PUT')throw new Error('模式保存失败');return window.chromeSavedAPI(path,method,...args)};$('chrome-mode').click();return true})()`)
	wait(`!chromeModePending && !$('chrome-mode').disabled && $('toast').textContent==='模式保存失败'`)
	if !eval(`$('chrome-mode').getAttribute('aria-checked')==='false' && !$('test-chrome').disabled`) {
		t.Fatal("failed save changed Chrome mode or blocked diagnostics")
	}
	eval(`(()=>{api=window.chromeSavedAPI;$('proxy-url').value='';$('proxy-url').dispatchEvent(new Event('input',{bubbles:true}));return true})()`)
	// The global switch saves immediately without submitting collection drafts.
	wait(`!$('global-auto-collection').disabled && state.overview!==null`)
	if !eval(`$('settings-collection').contains($('global-auto-collection')) && !$('collection-settings-form').contains($('global-auto-collection')) && $('global-auto-collection').getAttribute('role')==='switch' && $('global-auto-collection').getAttribute('aria-checked')===String(!state.settings.polling_paused)`) {
		t.Fatal("global auto collection switch has wrong location or initial state")
	}
	eval(`(()=>{window.initialPollingPaused=state.settings.polling_paused;$('queue-interval').value='17';$('queue-interval').dispatchEvent(new Event('input',{bubbles:true}));$('global-auto-collection').click();return true})()`)
	wait(`!globalCollectionPending && !$('global-auto-collection').disabled && state.overview.paused!==window.initialPollingPaused && $('global-auto-collection').getAttribute('aria-checked')===String(!state.overview.paused)`)
	if !eval(`$('queue-interval').value==='17' && settingsFormDirty('collection-settings-form')`) {
		t.Fatal("switch erased the collection draft")
	}
	if a.Config().QueueInterval != 0 {
		t.Fatal("switch submitted an unrelated draft")
	}
	eval(`(()=>{$('global-auto-collection').click();return true})()`)
	wait(`!globalCollectionPending && !$('global-auto-collection').disabled && state.overview.paused===window.initialPollingPaused`)
	// A failed save must keep the last confirmed switch state and allow retry.
	eval(`(()=>{window.savedSettingsAPI=api;api=async(path,method,...args)=>{if(path==='/settings'&&method==='PUT')throw new Error('模拟保存失败');return window.savedSettingsAPI(path,method,...args)};$('global-auto-collection').click();return true})()`)
	wait(`!globalCollectionPending && !$('global-auto-collection').disabled && $('toast').textContent==='模拟保存失败'`)
	if !eval(`$('global-auto-collection').getAttribute('aria-checked')===String(!window.initialPollingPaused)`) {
		t.Fatal("failed save changed the global switch")
	}
	eval(`(()=>{api=window.savedSettingsAPI;$('queue-interval').value='0';$('queue-interval').dispatchEvent(new Event('input',{bubbles:true}));return true})()`)
	if !eval(`!$('settings-tab-network') && $('settings-collection').contains($('settings-form')) && $('settings-collection').contains($('test-chrome')) && !$('collection-settings-form').contains($('settings-form'))`) {
		t.Fatal("proxy and browser diagnostics must be independent forms within collection settings")
	}
	if !eval(`!document.getElementById('settings-collection').hidden && !document.getElementById('request-interval') && !document.getElementById('max-posts-per-run') && !document.getElementById('page-budget') && !document.getElementById('max-scrolls') && document.getElementById('default-interval').value==='1' && document.getElementById('queue-interval').value==='0' && document.getElementById('scroll-delay').value==='3' && document.getElementById('scroll-jitter').value==='0' && document.getElementById('timeline-idle-seconds').value==='24' && document.getElementById('known-boundary-posts').value==='3' && !document.getElementById('request-budget-mode') && document.getElementById('rate-limit-mode').value==='balanced' && document.getElementById('request-timeout').value==='60' && document.getElementById('max-media-mib').value==='512' && document.getElementById('browser-max-age').value==='6' && document.getElementById('browser-max-tasks').value==='100' && !document.getElementById('monitor-default-form')`) {
		values, _ := chromedp.Run(browser, chromedp.Evaluate[string](`JSON.stringify(Object.fromEntries(['default-interval','queue-interval','scroll-delay','scroll-jitter','timeline-idle-seconds','known-boundary-posts','rate-limit-mode','request-timeout','max-media-mib','browser-max-age','browser-max-tasks','media-retry-initial','media-retry-max','request-interval','request-budget-mode','max-posts-per-run','page-budget','max-scrolls','monitor-default-form'].map(id=>[id,document.getElementById(id)?.value??null])))`))
		t.Fatal("settings defaults/location incorrect", values)
	}
	if !eval(`(()=>{document.getElementById('scroll-delay').value='0';document.getElementById('scroll-jitter').value='5';document.getElementById('default-interval').value='0.5';return !document.getElementById('scroll-delay').checkValidity() && !document.getElementById('scroll-jitter').checkValidity() && !document.getElementById('default-interval').checkValidity()})()`) {
		t.Fatal("minimums not enforced")
	}
	eval(`(()=>{document.getElementById('default-interval').value='2';document.getElementById('queue-interval').value='45';document.getElementById('scroll-delay').value='6';document.getElementById('scroll-delay').dispatchEvent(new Event('input'));document.getElementById('scroll-jitter').value='2';document.getElementById('scroll-jitter').dispatchEvent(new Event('input'));document.getElementById('timeline-idle-seconds').value='40';document.getElementById('known-boundary-posts').value='12';document.getElementById('rate-limit-mode').value='fast';document.getElementById('request-timeout').value='90';document.getElementById('max-media-mib').value='1024';document.getElementById('browser-max-age').value='12';document.getElementById('browser-max-tasks').value='250';document.getElementById('media-retry-initial').value='10';document.getElementById('media-retry-max').value='48';document.querySelector('#collection-settings-form button[type="submit"]').click();return true})()`)
	wait(`state.settings.default_interval_seconds===7200 && state.settings.queue_interval_seconds===45 && state.settings.scroll_delay_seconds===6 && state.settings.scroll_jitter_seconds===2 && state.settings.rate_limit_mode==='fast'`)
	if a.Config().DefaultInterval != 7200 || a.Config().QueueInterval != 45 || a.Config().MaxPostsPerRun != 1000 || a.Config().PageBudget != 15 || a.Config().MaxScrolls != 40 || a.Config().ScrollDelay != 6 || a.Config().ScrollJitter != 2 || a.Config().TimelineIdleSeconds != 40 || a.Config().KnownBoundaryPosts != 12 || a.Config().RequestBudgetMode != "balanced" || a.Config().RateLimitMode != "fast" || a.Config().RequestTimeout != 90 || a.Config().MaxMediaBytes != 1024<<20 || a.Config().BrowserMaxAge != 12*3600 || a.Config().BrowserMaxTasks != 250 || a.Config().MediaRetryInitial != 10*60 || a.Config().MediaRetryMax != 48*3600 {
		t.Fatal("UI did not persist settings")
	}
	if err := chromedp.Do(browser, chromedp.Navigate(server.URL+"/#settings")); err != nil {
		t.Fatal(err)
	}
	wait(`typeof state!=='undefined' && state.authenticated && !document.getElementById('request-interval') && !document.getElementById('max-posts-per-run') && document.getElementById('scroll-delay').value==='6' && document.getElementById('scroll-jitter').value==='2' && document.getElementById('scroll-spacing-help').textContent.includes('4～8 秒') && document.getElementById('default-interval').value==='2' && document.getElementById('queue-interval').value==='45' && document.getElementById('timeline-idle-seconds').value==='40' && !document.getElementById('collection-timeout') && document.getElementById('browser-max-age').value==='12' && !document.getElementById('request-budget-mode') && document.getElementById('browser-max-tasks').value==='250' && document.getElementById('media-retry-max').value==='48'`)
	if !eval(`$('chrome-mode').getAttribute('aria-checked')==='false' && $('chrome-mode-state').textContent==='有界面'`) {
		t.Fatal("Chrome mode did not survive page reload")
	}
	// A draft survives leaving settings and saving another form.
	eval(`(()=>{document.getElementById('settings-tab-collection').click();document.getElementById('browser-max-age').value='8';document.getElementById('browser-max-age').dispatchEvent(new Event('input',{bubbles:true}));location.hash='monitors';return true})()`)
	wait(`state.view==='archive' && location.hash==='#accounts'`)
	eval(`(()=>{location.hash='settings';return true})()`)
	wait(`state.view==='settings' && document.getElementById('browser-max-age').value==='8' && !document.getElementById('settings-draft-notice').hidden`)
	eval(`(()=>{document.getElementById('settings-tab-collection').click();document.querySelector('#settings-form button[type="submit"]').click();return true})()`)
	wait(`document.getElementById('toast').textContent.includes('代理设置已保存')`)
	if !eval(`document.getElementById('browser-max-age').value==='8' && settingsFormDirty('collection-settings-form')`) {
		t.Fatal("saving proxy erased collection draft")
	}
	eval(`(()=>{document.getElementById('settings-tab-collection').click();document.querySelector('[data-settings-group="browser"]').open=true;document.querySelector('[data-default-group="browser"]').click();return true})()`)
	if !eval(`document.getElementById('browser-max-age').value==='6' && document.getElementById('max-media-mib').value==='1024' && document.getElementById('collection-draft-status').textContent==='有未保存修改'`) {
		t.Fatal("group defaults changed unrelated settings")
	}
	eval(`(()=>{document.querySelector('#collection-settings-form button[type="submit"]').click();return true})()`)
	wait(`state.settings.browser_max_age_seconds===21600 && !settingsFormDirty('collection-settings-form')`)
	if !eval(`!document.getElementById('collection-settings-result').textContent.includes('重新安排')`) {
		t.Fatal("save falsely claimed schedule was changed")
	}
	// Invalid fields in collapsed groups must become visible and focusable.
	if !eval(`(()=>{const group=document.querySelector('[data-settings-group="browser"]');group.open=false;document.getElementById('browser-max-age').value='0';document.querySelector('#collection-settings-form button[type="submit"]').click();return group.open})()`) {
		t.Fatal("invalid collapsed field was hidden")
	}
	eval(`(()=>{document.getElementById('browser-max-age').value='6';document.getElementById('browser-max-age').dispatchEvent(new Event('input',{bubbles:true}));document.querySelectorAll('[data-settings-group]').forEach(el=>el.open=true);return true})()`)
	for _, width := range []int64{1280, 390} {
		if err := chromedp.Do(browser, chromedp.EmulateViewport(width, 844)); err != nil {
			t.Fatal(err)
		}
		eval(`(()=>{document.querySelector('[data-settings-group="browser"]').scrollIntoView({behavior:'instant',block:'start'});return true})()`)
		if !eval(`document.documentElement.scrollWidth<=window.innerWidth && (()=>{const r=document.querySelector('.collection-save-bar').getBoundingClientRect();return r.top>=0 && r.bottom<=window.innerHeight && r.width>0})()`) {
			t.Fatal("settings overflow or save action outside viewport", width)
		}
		if dir := os.Getenv("XM_UI_SCREENSHOT_DIR"); dir != "" {
			image, err := chromedp.Run(browser, chromedp.CaptureScreenshot())
			if err != nil {
				t.Fatal(err)
			}
			name := "settings-desktop.png"
			if width == 390 {
				name = "settings-mobile.png"
			}
			if err = os.WriteFile(filepath.Join(dir, name), image, 0600); err != nil {
				t.Fatal(err)
			}
			eval(`(()=>{$('chrome-mode').scrollIntoView({behavior:'instant',block:'center'});return true})()`)
			if !eval(`(()=>{const r=$('chrome-mode').getBoundingClientRect();return r.width>0 && r.left>=0 && r.right<=innerWidth})()`) {
				t.Fatal("Chrome mode switch outside viewport", width)
			}
			image, err = chromedp.Run(browser, chromedp.CaptureScreenshot())
			if err != nil {
				t.Fatal(err)
			}
			if err = os.WriteFile(filepath.Join(dir, "chrome-"+name), image, 0600); err != nil {
				t.Fatal(err)
			}
		}
	}

}
